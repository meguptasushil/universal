package hosting

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"golang.org/x/crypto/acme/autocert"
)

// TLSOptions configures automatic HTTPS through Let's Encrypt.
type TLSOptions struct {
	CacheDir  string // where certificates are stored
	Email     string // ACME account contact (recommended)
	HTTPAddr  string // ":80", serves ACME challenges and redirects to HTTPS
	HTTPSAddr string // ":443"
	Policy    autocert.HostPolicy
}

func newServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// ServeAutoTLS serves h over HTTPS with certificates issued on demand, plus
// an HTTP listener for ACME challenges. It blocks until ctx is cancelled.
func ServeAutoTLS(ctx context.Context, h http.Handler, o TLSOptions, log *slog.Logger) error {
	if o.CacheDir == "" {
		o.CacheDir = "certs"
	}
	if o.HTTPAddr == "" {
		o.HTTPAddr = ":80"
	}
	if o.HTTPSAddr == "" {
		o.HTTPSAddr = ":443"
	}
	m := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		Cache:      autocert.DirCache(o.CacheDir),
		HostPolicy: o.Policy,
		Email:      o.Email,
	}
	httpsSrv := newServer(o.HTTPSAddr, h)
	httpsSrv.TLSConfig = m.TLSConfig()
	httpSrv := newServer(o.HTTPAddr, m.HTTPHandler(nil)) // nil: redirect everything else to https

	errc := make(chan error, 2)
	go func() { errc <- httpSrv.ListenAndServe() }()
	go func() { errc <- httpsSrv.ListenAndServeTLS("", "") }()
	log.Info("serving with automatic HTTPS", "https", o.HTTPSAddr, "http", o.HTTPAddr, "certs", o.CacheDir)
	return waitAndShutdown(ctx, errc, httpSrv, httpsSrv)
}

// ServePlain serves h over plain HTTP until ctx is cancelled.
func ServePlain(ctx context.Context, h http.Handler, addr string) error {
	srv := newServer(addr, h)
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	return waitAndShutdown(ctx, errc, srv)
}

func waitAndShutdown(ctx context.Context, errc chan error, srvs ...*http.Server) error {
	var err error
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range srvs {
		_ = s.Shutdown(sctx)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
