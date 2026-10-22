package monitor

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed dashboard.html
var dashboardHTML []byte

// Server exposes the gateway, the REST/SSE API and the dashboard.
type Server struct {
	Gateway *Gateway
	Token   string // protects /api/*; empty disables auth
}

func (s *Server) authorized(r *http.Request) bool {
	if s.Token == "" {
		return true
	}
	got := r.URL.Query().Get("token")
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		got = strings.TrimPrefix(h, "Bearer ")
	}
	if h := r.Header.Get("X-Monitor-Token"); h != "" {
		got = h
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) == 1
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case p == "/" || p == "/index.html":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'")
		_, _ = w.Write(dashboardHTML)
	case p == "/healthz":
		_, _ = io.WriteString(w, "ok\n")
	case strings.HasPrefix(p, "/api/"):
		if !s.authorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		s.api(w, r, strings.TrimPrefix(p, "/api"))
	case strings.HasPrefix(p, "/a/"):
		// /a/<agent>/<provider>/<rest...>
		parts := strings.SplitN(strings.TrimPrefix(p, "/a/"), "/", 3)
		if len(parts) < 2 {
			http.NotFound(w, r)
			return
		}
		rest := ""
		if len(parts) == 3 {
			rest = "/" + parts[2]
		}
		s.Gateway.ServeProxy(w, r, parts[1], parts[0], rest)
	default:
		name, rest, _ := strings.Cut(strings.TrimPrefix(p, "/"), "/")
		if _, ok := s.Gateway.Upstreams[name]; !ok {
			http.NotFound(w, r)
			return
		}
		s.Gateway.ServeProxy(w, r, name, "", "/"+rest)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func filterFrom(r *http.Request) Filter {
	q := r.URL.Query()
	f := Filter{
		Agent:    q.Get("agent"),
		Provider: q.Get("provider"),
		Kind:     q.Get("kind"),
		Model:    q.Get("model"),
		Query:    q.Get("q"),
	}
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	if s := q.Get("since"); s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			f.Since = time.Now().Add(-d)
		} else if t, err := time.Parse(time.RFC3339, s); err == nil {
			f.Since = t
		}
	}
	return f
}

func (s *Server) api(w http.ResponseWriter, r *http.Request, p string) {
	store := s.Gateway.Store
	switch {
	case p == "/events" && r.Method == http.MethodGet:
		writeJSON(w, store.Query(filterFrom(r)))
	case strings.HasPrefix(p, "/events/") && r.Method == http.MethodGet:
		e, ok := store.Get(strings.TrimPrefix(p, "/events/"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, e)
	case p == "/stats":
		writeJSON(w, store.Stats(filterFrom(r)))
	case p == "/upstreams":
		type up struct {
			Name   string `json:"name"`
			Target string `json:"target"`
			Format string `json:"format"`
		}
		var out []up
		for _, u := range s.Gateway.Upstreams {
			out = append(out, up{u.Name, u.Target.String(), u.Format})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		writeJSON(w, out)
	case p == "/ingest" && r.Method == http.MethodPost:
		body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		events, err := eventsFromIngest(body, r.URL.Query().Get("agent"), s.Gateway.MaxText)
		if err != nil {
			http.Error(w, "invalid event: "+err.Error(), http.StatusBadRequest)
			return
		}
		ids := make([]string, 0, len(events))
		for _, e := range events {
			if s.Gateway.NoContent {
				e.Prompt, e.Response = "", ""
				for i := range e.ToolCalls {
					e.ToolCalls[i].Input = ""
				}
			}
			ids = append(ids, store.Add(e).ID)
		}
		writeJSON(w, map[string]any{"ids": ids})
	case p == "/stream":
		s.stream(w, r)
	default:
		http.NotFound(w, r)
	}
}

// stream pushes new events to the browser as Server-Sent Events.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	f := filterFrom(r)
	ch, cancel := s.Gateway.Store.Subscribe()
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	_, _ = io.WriteString(w, ": connected\n\n")
	fl.Flush()
	tick := time.NewTicker(25 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			_, _ = io.WriteString(w, ": ping\n\n")
			fl.Flush()
		case e := <-ch:
			if !f.match(&e) {
				continue
			}
			b, _ := json.Marshal(e)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
	}
}
