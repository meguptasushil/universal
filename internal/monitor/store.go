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
	Agent    string    `json:"agent,omitempty"`
	Provider string    `json:"provider,omitempty"`
	Kind     string    `json:"kind,omitempty"`
	Model    string    `json:"model,omitempty"`
	AgentID  string    `json:"agent_id,omitempty"`
	User     string    `json:"user,omitempty"`
	Session  string    `json:"session,omitempty"`
	KeyID    string    `json:"key_id,omitempty"`
	Query    string    `json:"q,omitempty"` // case-insensitive substring over prompt/response/tools/error/ids
	Since    time.Time `json:"since,omitempty"`
	Limit    int       `json:"-"`
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
	if (f.AgentID != "" && e.AgentID != f.AgentID) || (f.User != "" && e.User != f.User) ||
		(f.Session != "" && e.Session != f.Session) || (f.KeyID != "" && e.KeyID != f.KeyID) {
		return false
	}
	if !f.Since.IsZero() && e.Time.Before(f.Since) {
		return false
	}
	if f.Query != "" {
		q := strings.ToLower(f.Query)
		hay := strings.ToLower(e.Prompt + "\n" + e.Response + "\n" + e.Error + "\n" + e.Path + "\n" +
			e.AgentID + "\n" + e.User + "\n" + e.Session + "\n" + e.KeyID)
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
	FirstSeen    time.Time `json:"first_seen"`
	LastSeen     time.Time `json:"last_seen"`
}

func (c *Counter) add(e *Event) {
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
	if c.FirstSeen.IsZero() || e.Time.Before(c.FirstSeen) {
		c.FirstSeen = e.Time
	}
	if e.Time.After(c.LastSeen) {
		c.LastSeen = e.Time
	}
}

// AgentStat summarizes one agent and how many distinct identities it ran under.
type AgentStat struct {
	Counter
	AgentIDs int `json:"agent_ids"`
	Users    int `json:"users"`
	Keys     int `json:"keys"`
	Sessions int `json:"sessions"`
}

// IdentityStat is activity for one agent + instance + user + API key combination.
type IdentityStat struct {
	Counter
	Agent    string `json:"agent"`
	AgentID  string `json:"agent_id,omitempty"`
	User     string `json:"user,omitempty"`
	KeyID    string `json:"key_id,omitempty"`
	Sessions int    `json:"sessions"`
}

// SessionStat is activity within one agent session.
type SessionStat struct {
	Counter
	Agent    string   `json:"agent"`
	AgentID  string   `json:"agent_id,omitempty"`
	User     string   `json:"user,omitempty"`
	TopTools []string `json:"top_tools,omitempty"`
}

// Stats is a summary of the stored events.
type Stats struct {
	Total      int            `json:"total"`
	Totals     Counter        `json:"totals"`
	Agents     []AgentStat    `json:"agents"`
	Identities []IdentityStat `json:"identities"`
	Sessions   []SessionStat  `json:"sessions"`
	Users      []Counter      `json:"users"`
	Keys       []Counter      `json:"keys"`
	Providers  []Counter      `json:"providers"`
	Models     []Counter      `json:"models"`
	Tools      []Counter      `json:"tools"`
}

type distinct map[string]map[string]struct{}

func (d distinct) add(group, v string) {
	if v == "" {
		return
	}
	if d[group] == nil {
		d[group] = map[string]struct{}{}
	}
	d[group][v] = struct{}{}
}

// Stats aggregates the events matching f (Limit is ignored).
func (s *Store) Stats(f Filter) Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return computeStats(s.all(), f)
}

func computeStats(events []Event, f Filter) Stats {
	agents := map[string]*AgentStat{}
	idents := map[[4]string]*IdentityStat{}
	sessions := map[string]*SessionStat{}
	sessionTools := map[string]map[string]int{}
	users, keys, providers, models, tools := map[string]*Counter{}, map[string]*Counter{}, map[string]*Counter{}, map[string]*Counter{}, map[string]*Counter{}
	agentIDs, agentUsers, agentKeys, agentSessions, identSessions := distinct{}, distinct{}, distinct{}, distinct{}, distinct{}

	bump := func(m map[string]*Counter, key string, e *Event) {
		if key == "" {
			return
		}
		c := m[key]
		if c == nil {
			c = &Counter{Key: key}
			m[key] = c
		}
		c.add(e)
	}
	var st Stats
	for i := range events {
		e := &events[i]
		if !f.match(e) {
			continue
		}
		st.Total++
		st.Totals.add(e)

		a := agents[e.Agent]
		if a == nil {
			a = &AgentStat{Counter: Counter{Key: e.Agent}}
			agents[e.Agent] = a
		}
		a.add(e)
		agentIDs.add(e.Agent, e.AgentID)
		agentUsers.add(e.Agent, e.User)
		agentKeys.add(e.Agent, e.KeyID)
		agentSessions.add(e.Agent, e.Session)

		ik := [4]string{e.Agent, e.AgentID, e.User, e.KeyID}
		id := idents[ik]
		if id == nil {
			id = &IdentityStat{Counter: Counter{Key: strings.Join(ik[:], "|")}, Agent: e.Agent, AgentID: e.AgentID, User: e.User, KeyID: e.KeyID}
			idents[ik] = id
		}
		id.add(e)
		identSessions.add(id.Key, e.Session)

		if e.Session != "" {
			ss := sessions[e.Session]
			if ss == nil {
				ss = &SessionStat{Counter: Counter{Key: e.Session}, Agent: e.Agent}
				sessions[e.Session] = ss
				sessionTools[e.Session] = map[string]int{}
			}
			ss.add(e)
			ss.AgentID = firstNonEmpty(ss.AgentID, e.AgentID)
			ss.User = firstNonEmpty(ss.User, e.User)
			for _, t := range e.ToolCalls {
				sessionTools[e.Session][t.Name]++
			}
		}

		bump(users, e.User, e)
		bump(keys, e.KeyID, e)
		bump(providers, e.Provider, e)
		bump(models, e.Model, e)
		for _, t := range e.ToolCalls {
			c := tools[t.Name]
			if c == nil {
				c = &Counter{Key: t.Name}
				tools[t.Name] = c
			}
			c.Events++
			c.ToolCalls++
			if c.FirstSeen.IsZero() || e.Time.Before(c.FirstSeen) {
				c.FirstSeen = e.Time
			}
			if e.Time.After(c.LastSeen) {
				c.LastSeen = e.Time
			}
		}
	}

	for name, a := range agents {
		a.AgentIDs, a.Users, a.Keys, a.Sessions = len(agentIDs[name]), len(agentUsers[name]), len(agentKeys[name]), len(agentSessions[name])
		st.Agents = append(st.Agents, *a)
	}
	sort.Slice(st.Agents, func(i, j int) bool { return byActivity(&st.Agents[i].Counter, &st.Agents[j].Counter) })
	for _, id := range idents {
		id.Sessions = len(identSessions[id.Key])
		st.Identities = append(st.Identities, *id)
	}
	sort.Slice(st.Identities, func(i, j int) bool {
		if st.Identities[i].Agent != st.Identities[j].Agent {
			return st.Identities[i].Agent < st.Identities[j].Agent
		}
		return byActivity(&st.Identities[i].Counter, &st.Identities[j].Counter)
	})
	for key, ss := range sessions {
		ss.TopTools = topN(sessionTools[key], 3)
		st.Sessions = append(st.Sessions, *ss)
	}
	sort.Slice(st.Sessions, func(i, j int) bool { return st.Sessions[i].LastSeen.After(st.Sessions[j].LastSeen) })

	flat := func(m map[string]*Counter) []Counter {
		out := make([]Counter, 0, len(m))
		for _, c := range m {
			out = append(out, *c)
		}
		sort.Slice(out, func(i, j int) bool { return byActivity(&out[i], &out[j]) })
		return out
	}
	st.Users, st.Keys, st.Providers, st.Models, st.Tools = flat(users), flat(keys), flat(providers), flat(models), flat(tools)
	return st
}

func byActivity(a, b *Counter) bool {
	if a.Events != b.Events {
		return a.Events > b.Events
	}
	return a.Key < b.Key
}

func topN(counts map[string]int, n int) []string {
	names := make([]string, 0, len(counts))
	for k := range counts {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool {
		if counts[names[i]] != counts[names[j]] {
			return counts[names[i]] > counts[names[j]]
		}
		return names[i] < names[j]
	})
	if len(names) > n {
		names = names[:n]
	}
	return names
}

// LoadEvents reads every event from dir/events.jsonl without opening it for
// writing, so reports can run while the monitor is running.
func LoadEvents(dir string) ([]Event, error) {
	f, err := os.Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}
