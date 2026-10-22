package hosting

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Relay is the public side of tunnel mode. It accepts tunnel clients on its
// own domain and routes visitors of <name>.<domain> to the matching client.
type Relay struct {
	Domain             string   // apex domain, e.g. example.com
	Tokens             []string // accepted client tokens; empty means no auth (testing only)
	AllowCustomDomains bool     // let clients claim hostnames outside Domain
	PublicScheme       string   // "https" (default) or "http"
	PublicPort         string   // non-standard public port to include in URLs
	// TCPPortMin..TCPPortMax is the range of public ports handed to raw TCP
	// tunnels (databases, SSH, ...). Zero disables TCP tunnels.
	TCPPortMin, TCPPortMax int
	TCPBind                string // address to bind TCP tunnel ports on (default all interfaces)
	Log                    *slog.Logger

	mu      sync.RWMutex
	tunnels map[string]*tunnel // HTTP tunnels by public hostname
	tcp     map[int]*tunnel    // TCP tunnels by public port
	pending map[string]chan net.Conn
}

type tunnel struct {
	host      string // public hostname (HTTP tunnels)
	port      int    // public port (TCP tunnels)
	relay     *Relay
	mc        *msgConn
	wmu       sync.Mutex
	proxy     *httputil.ReverseProxy
	transport *http.Transport
	listener  net.Listener
	closed    chan struct{}
	started   time.Time
}

func (r *Relay) init() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tunnels == nil {
		r.tunnels = map[string]*tunnel{}
		r.tcp = map[int]*tunnel{}
		r.pending = map[string]chan net.Conn{}
	}
	if r.Log == nil {
		r.Log = slog.Default()
	}
}

func normHost(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.TrimSuffix(strings.ToLower(h), ".")
}

// HasHost reports whether host is the relay's domain or an active tunnel.
// It is used as the autocert host policy.
func (r *Relay) HasHost(host string) bool {
	host = normHost(host)
	if host == normHost(r.Domain) {
		return true
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.tunnels[host]
	return ok
}

// HostPolicy adapts HasHost to autocert.HostPolicy.
func (r *Relay) HostPolicy(_ context.Context, host string) error {
	r.init()
	if r.HasHost(host) {
		return nil
	}
	return fmt.Errorf("relay: no tunnel for %q", host)
}

func (r *Relay) checkToken(tok string) bool {
	if len(r.Tokens) == 0 {
		return true
	}
	ok := false
	for _, t := range r.Tokens {
		if subtle.ConstantTimeCompare([]byte(t), []byte(tok)) == 1 {
			ok = true
		}
	}
	return ok
}

func (r *Relay) publicURL(host string) string {
	scheme := r.PublicScheme
	if scheme == "" {
		scheme = "https"
	}
	if r.PublicPort != "" {
		host = net.JoinHostPort(host, r.PublicPort)
	}
	return scheme + "://" + host
}

// ServeHTTP routes control traffic and visitor traffic.
func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.init()
	host := normHost(req.Host)
	r.mu.RLock()
	t := r.tunnels[host]
	r.mu.RUnlock()
	if t != nil {
		t.proxy.ServeHTTP(w, req)
		return
	}
	switch req.URL.Path {
	case ControlPath:
		r.handleControl(w, req)
		return
	case DataPath:
		r.handleData(w, req)
		return
	}
	if host == normHost(r.Domain) || r.Domain == "" {
		if req.URL.Path == "/healthz" {
			_, _ = w.Write([]byte("ok\n"))
			return
		}
		if req.URL.Path == "/" {
			r.landing(w)
			return
		}
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	fmt.Fprintf(w, `<!doctype html><title>Not found</title><body style="font-family:system-ui;max-width:560px;margin:15vh auto;padding:0 16px">
<h1>Nothing is hosted at %s</h1><p>No tunnel is connected for this address right now. If this is your site, start it with
<code>universal expose &lt;port&gt;</code>.</p></body>`, html.EscapeString(host))
}

func (r *Relay) landing(w http.ResponseWriter) {
	r.mu.RLock()
	n := len(r.tunnels) + len(r.tcp)
	r.mu.RUnlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><title>Universal relay</title><body style="font-family:system-ui;max-width:560px;margin:15vh auto;padding:0 16px">
<h1>Universal relay</h1><p>%d active tunnel(s).</p>
<pre style="background:#f3f3f3;padding:12px;border-radius:6px">universal expose 3000 --server %s --token &lt;token&gt;</pre></body>`,
		n, html.EscapeString(r.publicURL(normHost(r.Domain))))
}

func bearer(req *http.Request) string {
	return strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
}

func (r *Relay) handleControl(w http.ResponseWriter, req *http.Request) {
	if !r.checkToken(bearer(req)) {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	conn, err := acceptUpgrade(w, req)
	if err != nil {
		return
	}
	mc := newMsgConn(conn)
	hello, err := mc.read(handshakeTimeout)
	if err != nil || hello.Type != "hello" {
		conn.Close()
		return
	}
	fail := func(msg string) {
		_ = mc.write(Message{Type: "error", Error: msg})
		conn.Close()
		r.Log.Warn("tunnel rejected", "remote", req.RemoteAddr, "reason", msg)
	}
	if !r.checkToken(hello.Token) {
		fail("invalid token")
		return
	}
	if hello.Protocol == "tcp" {
		r.registerTCP(req, mc, hello, fail)
		return
	}
	if hello.Protocol != "" && hello.Protocol != "http" {
		fail("unsupported protocol " + hello.Protocol)
		return
	}
	domain := normHost(r.Domain)
	var host string
	switch {
	case hello.Hostname != "":
		host = normHost(hello.Hostname)
		if !ValidHostname(host) {
			fail("invalid hostname " + hello.Hostname)
			return
		}
		if host == domain || (!strings.HasSuffix(host, "."+domain) && !r.AllowCustomDomains) {
			fail("hostname must be a subdomain of " + domain + " (custom domains are disabled on this relay)")
			return
		}
	case hello.Subdomain != "":
		sub := strings.ToLower(hello.Subdomain)
		if !ValidSubdomain(sub) {
			fail("invalid subdomain " + hello.Subdomain + " (use a-z, 0-9 and '-')")
			return
		}
		host = sub + "." + domain
	default:
		host = randomName() + "." + domain
	}

	t := &tunnel{host: host, relay: r, mc: mc, closed: make(chan struct{}), started: time.Now()}
	t.transport = &http.Transport{
		DialContext:           t.dial,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	target := &url.URL{Scheme: "http", Host: host}
	t.proxy = &httputil.ReverseProxy{
		Transport:     t.transport,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host // keep the public host for the app
			pr.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			r.Log.Warn("tunnel proxy error", "host", host, "err", err)
			http.Error(w, "universal: the tunnel client did not answer ("+err.Error()+")", http.StatusBadGateway)
		},
	}

	r.mu.Lock()
	if _, taken := r.tunnels[host]; taken {
		r.mu.Unlock()
		fail(host + " is already in use")
		return
	}
	r.tunnels[host] = t
	r.mu.Unlock()

	publicURL := r.publicURL(host)
	if err := t.send(Message{Type: "ok", URL: publicURL}); err != nil {
		t.close()
		return
	}
	r.Log.Info("tunnel connected", "url", publicURL, "remote", req.RemoteAddr)
	t.serveControl()
	r.Log.Info("tunnel disconnected", "url", publicURL, "uptime", time.Since(t.started).Round(time.Second))
}

// serveControl keeps the control channel alive until the client goes away.
func (t *tunnel) serveControl() {
	go t.pinger()
	for {
		if _, err := t.mc.read(readTimeout); err != nil {
			break // any message (pong) proves liveness
		}
	}
	t.close()
}

// registerTCP gives the client a public port and forwards every connection
// accepted on it through the tunnel.
func (r *Relay) registerTCP(req *http.Request, mc *msgConn, hello Message, fail func(string)) {
	if r.TCPPortMin <= 0 || r.TCPPortMax < r.TCPPortMin {
		fail("TCP tunnels are disabled on this relay (start it with --tcp-ports)")
		return
	}
	if hello.RemotePort != 0 && (hello.RemotePort < r.TCPPortMin || hello.RemotePort > r.TCPPortMax) {
		fail(fmt.Sprintf("port %d is outside this relay's TCP range %d-%d", hello.RemotePort, r.TCPPortMin, r.TCPPortMax))
		return
	}
	t := &tunnel{relay: r, mc: mc, closed: make(chan struct{}), started: time.Now()}
	candidates := []int{hello.RemotePort}
	if hello.RemotePort == 0 {
		candidates = candidates[:0]
		for p := r.TCPPortMin; p <= r.TCPPortMax; p++ {
			candidates = append(candidates, p)
		}
	}
	r.mu.Lock()
	for _, p := range candidates {
		if r.tcp[p] != nil {
			continue
		}
		ln, err := net.Listen("tcp", net.JoinHostPort(r.TCPBind, strconv.Itoa(p)))
		if err != nil {
			continue // in use by something else
		}
		t.port, t.listener = p, ln
		r.tcp[p] = t
		break
	}
	r.mu.Unlock()
	if t.listener == nil {
		if hello.RemotePort != 0 {
			fail(fmt.Sprintf("port %d is already in use", hello.RemotePort))
		} else {
			fail("no free TCP port left on this relay")
		}
		return
	}
	addr := "tcp://" + net.JoinHostPort(normHost(r.Domain), strconv.Itoa(t.port))
	if err := t.send(Message{Type: "ok", URL: addr}); err != nil {
		t.close()
		return
	}
	r.Log.Info("tcp tunnel connected", "addr", addr, "remote", req.RemoteAddr)
	go t.acceptTCP()
	t.serveControl()
	r.Log.Info("tcp tunnel disconnected", "addr", addr, "uptime", time.Since(t.started).Round(time.Second))
}

func (t *tunnel) acceptTCP() {
	for {
		c, err := t.listener.Accept()
		if err != nil {
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), streamTimeout)
			stream, err := t.open(ctx, c.RemoteAddr().String())
			cancel()
			if err != nil {
				t.relay.Log.Warn("tcp tunnel stream", "port", t.port, "err", err)
				c.Close()
				return
			}
			splice(c, stream)
		}()
	}
}

func (t *tunnel) send(m Message) error {
	t.wmu.Lock()
	defer t.wmu.Unlock()
	return t.mc.write(m)
}

func (t *tunnel) pinger() {
	tk := time.NewTicker(pingInterval)
	defer tk.Stop()
	for {
		select {
		case <-t.closed:
			return
		case <-tk.C:
			if t.send(Message{Type: "ping"}) != nil {
				t.close()
				return
			}
		}
	}
}

func (t *tunnel) close() {
	r := t.relay
	r.mu.Lock()
	if t.host != "" && r.tunnels[t.host] == t {
		delete(r.tunnels, t.host)
	}
	if t.listener != nil && r.tcp[t.port] == t {
		delete(r.tcp, t.port)
	}
	select {
	case <-t.closed:
	default:
		close(t.closed)
	}
	r.mu.Unlock()
	t.mc.conn.Close()
	if t.transport != nil {
		t.transport.CloseIdleConnections()
	}
	if t.listener != nil {
		t.listener.Close()
	}
}

// dial is the HTTP transport's DialContext.
func (t *tunnel) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	return t.open(ctx, "")
}

// open asks the client to open a data stream and waits for it. remote is the
// visitor's address, passed on for TCP tunnels so clients can filter by IP.
func (t *tunnel) open(ctx context.Context, remote string) (net.Conn, error) {
	r := t.relay
	id := randomID(16)
	ch := make(chan net.Conn, 1)
	r.mu.Lock()
	r.pending[id] = ch
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.pending, id)
		r.mu.Unlock()
		select { // a stream that arrived after we gave up
		case c := <-ch:
			c.Close()
		default:
		}
	}()
	if err := t.send(Message{Type: "conn", ID: id, Remote: remote}); err != nil {
		return nil, err
	}
	timer := time.NewTimer(streamTimeout)
	defer timer.Stop()
	select {
	case c := <-ch:
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.closed:
		return nil, errors.New("tunnel closed")
	case <-timer.C:
		return nil, errors.New("timed out waiting for tunnel client")
	}
}

func (r *Relay) handleData(w http.ResponseWriter, req *http.Request) {
	if !r.checkToken(bearer(req)) {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	id := req.URL.Query().Get("id")
	r.mu.Lock()
	ch, ok := r.pending[id]
	if ok {
		delete(r.pending, id)
	}
	r.mu.Unlock()
	if !ok {
		http.Error(w, "unknown stream", http.StatusNotFound)
		return
	}
	conn, err := acceptUpgrade(w, req)
	if err != nil {
		return
	}
	select {
	case ch <- conn:
	default:
		conn.Close()
	}
}

// Tunnels returns the public hostnames and TCP ports currently connected.
func (r *Relay) Tunnels() []string {
	r.init()
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.tunnels)+len(r.tcp))
	for h := range r.tunnels {
		out = append(out, h)
	}
	for p := range r.tcp {
		out = append(out, "tcp:"+strconv.Itoa(p))
	}
	return out
}
