package hosting

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"time"
)

// Client connects a local service to a relay.
type Client struct {
	Server    *url.URL // relay base URL, e.g. https://example.com
	Token     string
	Subdomain string // requested name; random when empty
	Hostname  string // full custom hostname instead of a subdomain
	Target    string // local host:port to forward to
	// TCP forwards raw TCP (databases, SSH, MQTT, game servers...) through a
	// public port on the relay instead of HTTP by hostname.
	TCP        bool
	RemotePort int // requested public port for TCP tunnels (0: any free one)
	// Allow restricts TCP tunnel visitors to these networks; empty allows all.
	Allow []*net.IPNet
	Log   *slog.Logger
	// OnReady is called with the public URL every time the tunnel (re)connects.
	OnReady func(publicURL string)
}

// ParseAllowList parses comma-separated IPs and CIDRs ("203.0.113.7,10.0.0.0/8").
func ParseAllowList(s string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "/") {
			ip := net.ParseIP(part)
			if ip == nil {
				return nil, fmt.Errorf("invalid IP %q", part)
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			part = fmt.Sprintf("%s/%d", ip, bits)
		}
		_, n, err := net.ParseCIDR(part)
		if err != nil {
			return nil, fmt.Errorf("invalid network %q", part)
		}
		out = append(out, n)
	}
	return out, nil
}

// RejectedError is returned when the relay refuses the tunnel (bad token,
// name taken, ...). Retrying will not help.
type RejectedError struct{ Reason string }

func (e *RejectedError) Error() string { return "relay rejected tunnel: " + e.Reason }

// Run keeps the tunnel up until ctx is cancelled, reconnecting with backoff.
func (c *Client) Run(ctx context.Context) error {
	if c.Log == nil {
		c.Log = slog.Default()
	}
	connected := false
	backoff := time.Second
	for {
		up, err := c.runOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		var rej *RejectedError
		if errors.As(err, &rej) && !connected {
			return err
		}
		if up {
			connected = true
			backoff = time.Second
		}
		c.Log.Warn("tunnel down, reconnecting", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

// runOnce runs one control session. up reports whether registration succeeded.
func (c *Client) runOnce(ctx context.Context) (up bool, err error) {
	conn, err := dialUpgrade(ctx, c.Server, ControlPath, c.Token)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	mc := newMsgConn(conn)
	hello := Message{Type: "hello", Version: 1, Token: c.Token, Subdomain: c.Subdomain, Hostname: c.Hostname}
	if c.TCP {
		hello.Protocol, hello.RemotePort = "tcp", c.RemotePort
	}
	if err := mc.write(hello); err != nil {
		return false, err
	}
	reply, err := mc.read(handshakeTimeout)
	if err != nil {
		return false, fmt.Errorf("waiting for relay: %w", err)
	}
	if reply.Type == "error" {
		return false, &RejectedError{Reason: reply.Error}
	}
	if reply.Type != "ok" {
		return false, fmt.Errorf("unexpected reply %q", reply.Type)
	}
	if c.OnReady != nil {
		c.OnReady(reply.URL)
	}
	for {
		msg, err := mc.read(readTimeout)
		if err != nil {
			return true, err
		}
		switch msg.Type {
		case "ping":
			if err := mc.write(Message{Type: "pong"}); err != nil {
				return true, err
			}
		case "conn":
			go c.handleStream(ctx, msg.ID, msg.Remote)
		}
	}
}

// allowed reports whether a TCP visitor may connect.
func (c *Client) allowed(remote string) bool {
	if len(c.Allow) == 0 {
		return true
	}
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(host)
	for _, n := range c.Allow {
		if ip != nil && n.Contains(ip) {
			return true
		}
	}
	return false
}

func (c *Client) handleStream(ctx context.Context, id, visitor string) {
	remote, err := dialUpgrade(ctx, c.Server, DataPath+"?id="+url.QueryEscape(id), c.Token)
	if err != nil {
		c.Log.Warn("opening stream", "err", err)
		return
	}
	if c.TCP {
		if !c.allowed(visitor) {
			c.Log.Warn("blocked connection", "from", visitor)
			remote.Close()
			return
		}
		c.Log.Info("connection", "from", visitor)
	}
	local, err := net.DialTimeout("tcp", c.Target, 5*time.Second)
	if err != nil {
		c.Log.Warn("local service unreachable", "target", c.Target, "err", err)
		if c.TCP {
			remote.Close()
			return
		}
		msg := fmt.Sprintf("universal: the tunnel is up, but nothing answered on %s on the host machine.\nStart your server there and reload.\n", c.Target)
		_, _ = io.WriteString(remote, fmt.Sprintf("HTTP/1.1 502 Bad Gateway\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(msg), msg))
		remote.Close()
		return
	}
	splice(remote, local)
}
