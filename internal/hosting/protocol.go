// Package hosting makes local servers reachable from the internet, either
// through a relay ("tunnel" mode) or directly with automatic HTTPS.
package hosting

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	// ControlPath and DataPath are served on the relay's own domain.
	ControlPath = "/_universal/tunnel"
	DataPath    = "/_universal/data"

	upgradeProto     = "universal-tunnel/1"
	pingInterval     = 20 * time.Second
	readTimeout      = 60 * time.Second
	handshakeTimeout = 10 * time.Second
	streamTimeout    = 10 * time.Second
)

// Message is a control-channel message. Messages are sent as JSON, one per line.
type Message struct {
	Type      string `json:"type"` // hello, ok, error, conn, ping, pong
	Version   int    `json:"version,omitempty"`
	Token     string `json:"token,omitempty"`
	Subdomain string `json:"subdomain,omitempty"`
	Hostname  string `json:"hostname,omitempty"`
	// Protocol is "http" (default) or "tcp". TCP tunnels get a public port
	// on the relay; RemotePort requests a specific one.
	Protocol   string `json:"protocol,omitempty"`
	RemotePort int    `json:"remote_port,omitempty"`
	URL        string `json:"url,omitempty"`
	ID         string `json:"id,omitempty"`
	Remote     string `json:"remote,omitempty"` // visitor address, on "conn" for TCP tunnels
	Error      string `json:"error,omitempty"`
}

var labelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidSubdomain reports whether s is a single valid DNS label.
func ValidSubdomain(s string) bool { return labelRE.MatchString(s) }

// ValidHostname reports whether s is a syntactically valid DNS name.
func ValidHostname(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	labels := strings.Split(s, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if !ValidSubdomain(l) {
			return false
		}
	}
	return true
}

func randomID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// randomName returns a readable random subdomain.
func randomName() string {
	const letters = "abcdefghijkmnpqrstuvwxyz23456789"
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = letters[int(b[i])%len(letters)]
	}
	return string(b)
}

// bufferedConn is a net.Conn whose reads go through a bufio.Reader, so bytes
// buffered during the HTTP handshake are not lost.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// CloseWrite half-closes the connection when the underlying conn supports
// it, and fully closes it otherwise.
func (c *bufferedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Conn.Close()
}

// acceptUpgrade completes the server side of the tunnel handshake.
func acceptUpgrade(w http.ResponseWriter, r *http.Request) (net.Conn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), upgradeProto) {
		http.Error(w, "expected Upgrade: "+upgradeProto, http.StatusUpgradeRequired)
		return nil, errors.New("missing upgrade header")
	}
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	if _, err := io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: "+upgradeProto+"\r\nConnection: Upgrade\r\n\r\n"); err != nil {
		conn.Close()
		return nil, err
	}
	return &bufferedConn{Conn: conn, r: brw.Reader}, nil
}

// dialUpgrade opens a connection to the relay and performs the client side of
// the tunnel handshake on path (which may include a query string).
func dialUpgrade(ctx context.Context, server *url.URL, path, token string) (net.Conn, error) {
	host := server.Hostname()
	port := server.Port()
	useTLS := server.Scheme == "https"
	if port == "" {
		port = "80"
		if useTLS {
			port = "443"
		}
	}
	addr := net.JoinHostPort(host, port)
	d := &net.Dialer{Timeout: handshakeTimeout, KeepAlive: 30 * time.Second}
	var conn net.Conn
	var err error
	if useTLS {
		td := &tls.Dialer{NetDialer: d, Config: &tls.Config{ServerName: host, NextProtos: []string{"http/1.1"}}}
		conn, err = td.DialContext(ctx, "tcp", addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: %s\r\nAuthorization: Bearer %s\r\nUser-Agent: universal-expose\r\n\r\n",
		path, server.Host, upgradeProto, token)
	if _, err := io.WriteString(conn, req); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("handshake: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		conn.Close()
		err := fmt.Errorf("relay refused connection: %s: %s", resp.Status, strings.TrimSpace(string(body)))
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, &RejectedError{Reason: err.Error()}
		}
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return &bufferedConn{Conn: conn, r: br}, nil
}

// msgConn reads and writes control messages.
type msgConn struct {
	conn net.Conn
	dec  *json.Decoder
	enc  *json.Encoder
}

func newMsgConn(c net.Conn) *msgConn {
	return &msgConn{conn: c, dec: json.NewDecoder(c), enc: json.NewEncoder(c)}
}

func (m *msgConn) read(timeout time.Duration) (Message, error) {
	var msg Message
	_ = m.conn.SetReadDeadline(time.Now().Add(timeout))
	err := m.dec.Decode(&msg)
	return msg, err
}

// write is not safe for concurrent use; callers serialize.
func (m *msgConn) write(msg Message) error {
	_ = m.conn.SetWriteDeadline(time.Now().Add(handshakeTimeout))
	return m.enc.Encode(msg)
}

// splice copies data both ways until both directions are done.
func splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
	a.Close()
	b.Close()
}
