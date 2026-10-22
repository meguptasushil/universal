package monitor

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store keeps recent events in memory and appends every event to a JSONL file.
type Store struct {
	mu   sync.RWMutex
	ring []Event
	next int
	full bool
	file *os.File
	subs map[chan Event]struct{}
}

// OpenStore creates a store with the given capacity. If dir is non-empty,
// events are persisted to dir/events.jsonl and the most recent ones are
// reloaded on start.
func OpenStore(dir string, capacity int) (*Store, error) {
	if capacity <= 0 {
		capacity = 10000
	}
	s := &Store{ring: make([]Event, capacity), subs: map[chan Event]struct{}{}}
	if dir == "" {
		return s, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "events.jsonl")
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
		for sc.Scan() {
			var e Event
			if json.Unmarshal(sc.Bytes(), &e) == nil {
				s.put(e)
			}
		}
		f.Close()
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	s.file = f
	return s, nil
}

func (s *Store) put(e Event) {
	s.ring[s.next] = e
	s.next = (s.next + 1) % len(s.ring)
	if s.next == 0 {
		s.full = true
	}
}

// Add records an event, persists it and notifies subscribers.
func (s *Store) Add(e Event) Event {
	e.fill()
	s.mu.Lock()
	s.put(e)
	if s.file != nil {
		if b, err := json.Marshal(e); err == nil {
			_, _ = s.file.Write(append(b, '\n'))
		}
	}
	for ch := range s.subs {
		select {
		case ch <- e:
		default: // slow subscriber, drop
		}
	}
	s.mu.Unlock()
	return e
}

// Subscribe returns a channel receiving every new event, and a cancel func.
func (s *Store) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 256)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}
}

// Close flushes and closes the backing file.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file != nil {
		return s.file.Close()
	}
	return nil
}

// all returns events oldest first. Caller must hold the read lock.
func (s *Store) all() []Event {
	var out []Event
	if s.full {
		out = append(out, s.ring[s.next:]...)
	}
	return append(out, s.ring[:s.next]...)
}

// Filter selects events in queries.
type Filter struct {
	Agent    string
	Provider string
	Kind     string
	Model    string
	Query    string // case-insensitive substring over prompt/response/tools/error
	Since    time.Time
	Limit    int
}

func (f Filter) match(e *Event) bool {
	if f.Agent != "" && e.Agent != f.Agent {
		return false
	}
	if f.Provider != "" && e.Provider != f.Provider {
		return false
	}
	if f.Kind != "" && e.Kind != f.Kind {
		return false
	}
	if f.Model != "" && e.Model != f.Model {
		return false
	}
	if !f.Since.IsZero() && e.Time.Before(f.Since) {
		return false
	}
	if f.Query != "" {
		q := strings.ToLower(f.Query)
		hay := strings.ToLower(e.Prompt + "\n" + e.Response + "\n" + e.Error + "\n" + e.Path)
		for _, t := range e.ToolCalls {
			hay += "\n" + strings.ToLower(t.Name+" "+t.Input)
		}
		if !strings.Contains(hay, q) {
			return false
		}
	}
	return true
}

// Query returns matching events, newest first.
func (s *Store) Query(f Filter) []Event {
	if f.Limit <= 0 {
		f.Limit = 200
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	all := s.all()
	out := make([]Event, 0, f.Limit)
	for i := len(all) - 1; i >= 0 && len(out) < f.Limit; i-- {
		if f.match(&all[i]) {
			out = append(out, all[i])
		}
	}
	return out
}

// Get returns a single event by id.
func (s *Store) Get(id string) (Event, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.all() {
		if e.ID == id {
			return e, true
		}
	}
	return Event{}, false
}

// Counter aggregates activity for one key (agent, model, tool...).
type Counter struct {
	Key          string    `json:"key"`
	Events       int       `json:"events"`
	LLMCalls     int       `json:"llm_calls"`
	ToolCalls    int       `json:"tool_calls"`
	Errors       int       `json:"errors"`
	InputTokens  int       `json:"input_tokens"`
	OutputTokens int       `json:"output_tokens"`
	LastSeen     time.Time `json:"last_seen"`
}

// Stats is a summary of the stored events.
type Stats struct {
	Total     int       `json:"total"`
	Agents    []Counter `json:"agents"`
	Providers []Counter `json:"providers"`
	Models    []Counter `json:"models"`
	Tools     []Counter `json:"tools"`
}

// Stats aggregates the events matching f (Limit is ignored).
func (s *Store) Stats(f Filter) Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	agents, providers, models, tools := map[string]*Counter{}, map[string]*Counter{}, map[string]*Counter{}, map[string]*Counter{}
	bump := func(m map[string]*Counter, key string, e *Event) *Counter {
		c := m[key]
		if c == nil {
			c = &Counter{Key: key}
			m[key] = c
		}
		c.Events++
		if e.Kind == KindLLMCall {
			c.LLMCalls++
		}
		c.ToolCalls += len(e.ToolCalls)
		if e.Error != "" || e.Status >= 400 {
			c.Errors++
		}
		c.InputTokens += e.InputTokens
		c.OutputTokens += e.OutputTokens
		if e.Time.After(c.LastSeen) {
			c.LastSeen = e.Time
		}
		return c
	}
	var st Stats
	for _, e := range s.all() {
		e := e
		if !f.match(&e) {
			continue
		}
		st.Total++
		bump(agents, e.Agent, &e)
		if e.Provider != "" {
			bump(providers, e.Provider, &e)
		}
		if e.Model != "" {
			bump(models, e.Model, &e)
		}
		for _, t := range e.ToolCalls {
			c := tools[t.Name]
			if c == nil {
				c = &Counter{Key: t.Name}
				tools[t.Name] = c
			}
			c.Events++
			c.ToolCalls++
			if e.Time.After(c.LastSeen) {
				c.LastSeen = e.Time
			}
		}
	}
	flat := func(m map[string]*Counter) []Counter {
		out := make([]Counter, 0, len(m))
		for _, c := range m {
			out = append(out, *c)
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Events != out[j].Events {
				return out[i].Events > out[j].Events
			}
			return out[i].Key < out[j].Key
		})
		return out
	}
	st.Agents, st.Providers, st.Models, st.Tools = flat(agents), flat(providers), flat(models), flat(tools)
	return st
}
