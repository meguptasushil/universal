package portal

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
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
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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
