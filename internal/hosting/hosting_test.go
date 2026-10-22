package hosting

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// startRelay runs a relay over plain HTTP on loopback.
func startRelay(t *testing.T) (*Relay, *url.URL) {
	t.Helper()
	r := &Relay{Domain: "tunnel.test", Tokens: []string{"tok"}, PublicScheme: "http", Log: quietLog()}
	// wrapped like the CLI does, so hijacking must work through the wrapper
	srv := httptest.NewServer(AccessLog(quietLog(), r))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return r, u
}

func startClient(t *testing.T, relay *url.URL, c Client) (string, context.CancelFunc, chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ready := make(chan string, 4)
	c.Server, c.Log = relay, quietLog()
	c.OnReady = func(u string) { ready <- u }
	errc := make(chan error, 1)
	go func() { errc <- c.Run(ctx) }()
	select {
	case u := <-ready:
		return u, cancel, errc
	case err := <-errc:
		t.Fatalf("client exited: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("tunnel did not come up")
	}
	return "", cancel, errc
}

// get sends a request to the relay with a given Host header.
func get(t *testing.T, relay *url.URL, host, path string) (int, string, http.Header) {
	t.Helper()
	req, _ := http.NewRequest("GET", relay.String()+path, nil)
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func TestTunnelEndToEnd(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Seen-Host", r.Host)
		w.Header().Set("X-Seen-Proto", r.Header.Get("X-Forwarded-Proto"))
		body, _ := io.ReadAll(r.Body)
		io.WriteString(w, "hello from "+r.URL.Path+" "+string(body))
	}))
	defer backend.Close()
	relay, relayURL := startRelay(t)

	pub, _, _ := startClient(t, relayURL, Client{Token: "tok", Subdomain: "myapp", Target: strings.TrimPrefix(backend.URL, "http://")})
	if pub != "http://myapp.tunnel.test" {
		t.Fatalf("public url %q", pub)
	}
	if !relay.HasHost("myapp.tunnel.test") || relay.HasHost("other.tunnel.test") {
		t.Fatal("host policy")
	}

	// many requests, sequential and concurrent, over pooled streams
	for i := 0; i < 5; i++ {
		code, body, hdr := get(t, relayURL, "myapp.tunnel.test", "/x")
		if code != 200 || body != "hello from /x " || hdr.Get("X-Seen-Host") != "myapp.tunnel.test" || hdr.Get("X-Seen-Proto") != "http" {
			t.Fatalf("got %d %q %v", code, body, hdr)
		}
	}
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		go func() {
			req, _ := http.NewRequest("POST", relayURL.String()+"/post", strings.NewReader("data"))
			req.Host = "myapp.tunnel.test"
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				errs <- err
				return
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(b) != "hello from /post data" {
				err = errors.New("bad body " + string(b))
			}
			errs <- err
		}()
	}
	for i := 0; i < 20; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}

	// unknown host
	if code, _, _ := get(t, relayURL, "nope.tunnel.test", "/"); code != 404 {
		t.Fatalf("unknown host: %d", code)
	}
	// apex landing page
	if code, body, _ := get(t, relayURL, "tunnel.test", "/"); code != 200 || !strings.Contains(body, "1 active tunnel") {
		t.Fatalf("landing: %d %q", code, body)
	}
}

func TestTunnelWebSocketUpgrade(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" {
			http.Error(w, "want upgrade", 400)
			return
		}
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		line, _ := brw.ReadString('\n')
		io.WriteString(conn, "echo:"+line)
	}))
	defer backend.Close()
	_, relayURL := startRelay(t)
	startClient(t, relayURL, Client{Token: "tok", Subdomain: "ws", Target: strings.TrimPrefix(backend.URL, "http://")})

	conn, err := net.Dial("tcp", relayURL.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(conn, "GET /socket HTTP/1.1\r\nHost: ws.tunnel.test\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 101 {
		t.Fatalf("upgrade: %v %v", err, resp)
	}
	io.WriteString(conn, "hi\n")
	got, _ := br.ReadString('\n')
	if got != "echo:hi\n" {
		t.Fatalf("got %q", got)
	}
}

func TestTunnelRejections(t *testing.T) {
	_, relayURL := startRelay(t)
	target := "127.0.0.1:1"

	// bad token
	c := Client{Server: relayURL, Token: "wrong", Subdomain: "a", Target: target, Log: quietLog()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.runOnce(ctx); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("bad token: %v", err)
	}
	// invalid name
	c = Client{Server: relayURL, Token: "tok", Subdomain: "Bad_Name!", Target: target, Log: quietLog()}
	var rej *RejectedError
	if err := c.Run(ctx); !errors.As(err, &rej) {
		t.Fatalf("invalid name: %v", err)
	}
	// custom domain disabled
	c = Client{Server: relayURL, Token: "tok", Hostname: "www.customer.com", Target: target, Log: quietLog()}
	if err := c.Run(ctx); !errors.As(err, &rej) {
		t.Fatalf("custom domain: %v", err)
	}
	// duplicate subdomain
	startClient(t, relayURL, Client{Token: "tok", Subdomain: "dup", Target: target})
	c = Client{Server: relayURL, Token: "tok", Subdomain: "dup", Target: target, Log: quietLog()}
	if err := c.Run(ctx); !errors.As(err, &rej) || !strings.Contains(rej.Reason, "already in use") {
		t.Fatalf("duplicate: %v", err)
	}
	// random name
	pub, _, _ := startClient(t, relayURL, Client{Token: "tok", Target: target})
	if !strings.HasSuffix(pub, ".tunnel.test") || len(pub) != len("http://")+8+len(".tunnel.test") {
		t.Fatalf("random url %q", pub)
	}
}

func TestTunnelLocalServiceDown(t *testing.T) {
	_, relayURL := startRelay(t)
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close() // nothing listens here now
	startClient(t, relayURL, Client{Token: "tok", Subdomain: "down", Target: addr})
	code, body, _ := get(t, relayURL, "down.tunnel.test", "/")
	if code != 502 || !strings.Contains(body, "nothing answered") {
		t.Fatalf("got %d %q", code, body)
	}
}

func TestTunnelDisconnectFreesName(t *testing.T) {
	relay, relayURL := startRelay(t)
	_, cancel, errc := startClient(t, relayURL, Client{Token: "tok", Subdomain: "gone", Target: "127.0.0.1:1"})
	cancel()
	<-errc
	deadline := time.Now().Add(3 * time.Second)
	for relay.HasHost("gone.tunnel.test") {
		if time.Now().After(deadline) {
			t.Fatal("tunnel not removed after disconnect")
		}
		time.Sleep(20 * time.Millisecond)
	}
	startClient(t, relayURL, Client{Token: "tok", Subdomain: "gone", Target: "127.0.0.1:1"})
}

func TestStaticHandler(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>home</h1>"), 0o644)
	os.WriteFile(filepath.Join(dir, "app.js"), []byte("js"), 0o644)
	os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET"), 0o644)
	h, err := StaticHandler(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	check := func(path string, code int, want string) {
		t.Helper()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != code || !strings.Contains(string(b), want) {
			t.Fatalf("%s: %d %q", path, resp.StatusCode, b)
		}
	}
	check("/", 200, "home")
	check("/app.js", 200, "js")
	check("/some/client/route", 200, "home") // SPA fallback
	check("/.env", 404, "")
	if _, err := SiteHandler("", "", false); err == nil {
		t.Fatal("expected error")
	}
}

func TestValidNames(t *testing.T) {
	for _, s := range []string{"a", "my-app", "x1"} {
		if !ValidSubdomain(s) {
			t.Errorf("%q should be valid", s)
		}
	}
	for _, s := range []string{"", "-a", "a-", "A", "a.b", strings.Repeat("a", 64)} {
		if ValidSubdomain(s) {
			t.Errorf("%q should be invalid", s)
		}
	}
	if !ValidHostname("www.example.com") || ValidHostname("localhost") {
		t.Error("hostname validation")
	}
}
