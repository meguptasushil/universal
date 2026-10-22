package portal

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed portal.html
var portalHTML []byte

// Server serves the portal UI and its JSON API.
//
// The API can expose ports on this machine to the internet, so it is guarded
// against other websites driving it through the browser: the Host header
// must be a loopback name (blocks DNS rebinding) unless a token is set, and
// every API call must carry the X-Portal header (a cross-site page cannot
// add it without a CORS preflight, which is never granted).
type Server struct {
	M     *Manager
	Token string // required on every API call when set (needed for non-loopback access)
	// SkipPorts are left out of discovery (the portal's own port).
	SkipPorts []int
	// Autostart, when set, lets the UI turn "start at login" on and off.
	Autostart Autostarter
}

// Autostarter starts the portal when the user logs in.
type Autostarter interface {
	Enabled() bool
	Enable() error
	Disable() error
}

func loopbackHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; frame-ancestors 'none'")
		w.Header().Set("X-Frame-Options", "DENY")
		_, _ = w.Write(portalHTML)
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.M.State())
	})
	mux.HandleFunc("PUT /api/settings", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Server     string `json:"server"`
			Token      string `json:"token"`
			ClearToken bool   `json:"clear_token"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		reply(w, nil, s.M.UpdateSettings(in.Server, in.Token, in.ClearToken))
	})
	mux.HandleFunc("POST /api/settings/test", func(w http.ResponseWriter, r *http.Request) {
		if err := s.M.TestRelay(r.Context()); err != nil {
			msg, hint := friendly(err)
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": msg, "hint": hint, "detail": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/connect", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Code string `json:"code"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		prev := s.M.Settings()
		if err := s.M.Connect(in.Code); err != nil {
			reply(w, nil, err)
			return
		}
		if err := s.M.TestRelay(r.Context()); err != nil {
			// keep the previous relay, so the user stays where they can fix it
			_ = s.M.UpdateSettings(prev.Server, prev.Token, true)
			msg, hint := friendly(err)
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": msg, "hint": hint, "detail": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /api/discover", func(w http.ResponseWriter, r *http.Request) {
		found := s.M.Discover(r.Context(), s.SkipPorts...)
		if found == nil {
			found = []Found{}
		}
		writeJSON(w, http.StatusOK, found)
	})
	mux.HandleFunc("GET /api/detect", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, Detect(strings.TrimSpace(r.URL.Query().Get("path"))))
	})
	mux.HandleFunc("GET /api/browse", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, browse(r.URL.Query().Get("path")))
	})
	mux.HandleFunc("GET /api/apps/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		lines, err := s.M.Logs(r.PathValue("id"))
		if err != nil {
			reply(w, nil, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
	})
	mux.HandleFunc("GET /api/autostart", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"supported": s.Autostart != nil, "enabled": s.Autostart != nil && s.Autostart.Enabled()})
	})
	mux.HandleFunc("PUT /api/autostart", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Enabled bool `json:"enabled"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		if s.Autostart == nil {
			reply(w, nil, errors.New("starting at login isn't supported here"))
			return
		}
		var err error
		if in.Enabled {
			err = s.Autostart.Enable()
		} else {
			err = s.Autostart.Disable()
		}
		reply(w, nil, err)
	})
	mux.HandleFunc("POST /api/apps", func(w http.ResponseWriter, r *http.Request) {
		var a App
		if !readJSON(w, r, &a) {
			return
		}
		a, err := s.M.Add(a)
		if err == nil && r.URL.Query().Get("start") == "1" {
			err = s.M.Start(a.ID)
		}
		reply(w, a, err)
	})
	mux.HandleFunc("PUT /api/apps/{id}", func(w http.ResponseWriter, r *http.Request) {
		var a App
		if !readJSON(w, r, &a) {
			return
		}
		a, err := s.M.Update(r.PathValue("id"), a)
		reply(w, a, err)
	})
	mux.HandleFunc("DELETE /api/apps/{id}", func(w http.ResponseWriter, r *http.Request) {
		reply(w, nil, s.M.Delete(r.PathValue("id")))
	})
	mux.HandleFunc("POST /api/apps/{id}/start", func(w http.ResponseWriter, r *http.Request) {
		reply(w, nil, s.M.Start(r.PathValue("id")))
	})
	mux.HandleFunc("POST /api/apps/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		s.M.Stop(r.PathValue("id"))
		reply(w, nil, nil)
	})
	return s.guard(mux)
}

func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Token == "" && !loopbackHost(r.Host) {
			http.Error(w, "the portal only answers on localhost; start it with --token to allow other hosts", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if r.Header.Get("X-Portal") != "1" {
				http.Error(w, "missing X-Portal header", http.StatusForbidden)
				return
			}
			if s.Token != "" {
				got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				if subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request: " + err.Error()})
		return false
	}
	return true
}

func reply(w http.ResponseWriter, v any, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case err != nil:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case v == nil:
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		writeJSON(w, http.StatusOK, v)
	}
}

// Listing is one folder in the folder picker.
type Listing struct {
	Path   string   `json:"path"`
	Parent string   `json:"parent,omitempty"`
	Dirs   []string `json:"dirs"`
	Error  string   `json:"error,omitempty"`
}

// browse lists the sub-folders of path (default: the home folder), so people
// can pick a folder instead of typing its path. Hidden folders are skipped.
func browse(path string) Listing {
	if path == "" {
		path, _ = os.UserHomeDir()
	}
	path = filepath.Clean(path)
	l := Listing{Path: path, Dirs: []string{}}
	if parent := filepath.Dir(path); parent != path {
		l.Parent = parent
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		l.Error = "This folder can't be opened."
		return l
	}
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") && e.Name() != "node_modules" {
			l.Dirs = append(l.Dirs, e.Name())
		}
	}
	sort.Strings(l.Dirs)
	return l
}
