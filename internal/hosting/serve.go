package hosting

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// StaticHandler serves files from dir. With spa set, unknown paths fall back
// to index.html so client-side routers work. Dotfiles are never served.
func StaticHandler(dir string, spa bool) (http.Handler, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	root := http.Dir(abs)
	fs := http.FileServer(root)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := path.Clean("/" + r.URL.Path)
		for _, seg := range strings.Split(clean, "/") {
			if strings.HasPrefix(seg, ".") && seg != "." {
				http.NotFound(w, r)
				return
			}
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		if spa {
			if f, err := root.Open(clean); err != nil {
				r2 := r.Clone(r.Context())
				r2.URL.Path = "/"
				fs.ServeHTTP(w, r2)
				return
			} else {
				f.Close()
			}
		}
		fs.ServeHTTP(w, r)
	}), nil
}

// ProxyHandler forwards every request to target (e.g. http://localhost:3000).
func ProxyHandler(target string) (http.Handler, error) {
	u, err := url.Parse(target)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid proxy target %q (want e.g. http://localhost:3000)", target)
	}
	return &httputil.ReverseProxy{
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u)
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
		},
	}, nil
}

// SiteHandler builds the handler for "serve" and "expose ./dir".
func SiteHandler(dir, proxy string, spa bool) (http.Handler, error) {
	switch {
	case dir != "" && proxy != "":
		return nil, errors.New("use either a directory or a proxy target, not both")
	case dir != "":
		return StaticHandler(dir, spa)
	case proxy != "":
		return ProxyHandler(proxy)
	}
	return nil, errors.New("nothing to serve: pass a directory or a proxy target")
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// AccessLog logs one line per request.
func AccessLog(log *slog.Logger, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rec, r)
		log.Info("request", "host", r.Host, "method", r.Method, "path", r.URL.Path, "status", rec.status, "ms", time.Since(start).Milliseconds())
	})
}
