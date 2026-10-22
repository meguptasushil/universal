package hosting

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// LocalTarget turns "3000", "localhost:3000", "http://host:3000" or "tcp://host:5432"
// into host:port.
func LocalTarget(s string) (string, error) {
	if _, err := strconv.Atoi(s); err == nil {
		return "127.0.0.1:" + s, nil
	}
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil {
			return "", err
		}
		if u.Scheme == "tcp" {
			if u.Port() == "" {
				return "", fmt.Errorf("%s: a port is required", s)
			}
			return u.Host, nil
		}
		if u.Scheme != "http" {
			return "", fmt.Errorf("only http:// targets can be exposed (got %s)", u.Scheme)
		}
		if u.Port() == "" {
			return net.JoinHostPort(u.Hostname(), "80"), nil
		}
		return u.Host, nil
	}
	if _, _, err := net.SplitHostPort(s); err == nil {
		return s, nil
	}
	return "", fmt.Errorf("cannot understand target %q (use a port, host:port, http://url, tcp://host:port or a directory)", s)
}

// CheckRelay verifies that a relay is reachable and accepts token.
func CheckRelay(ctx context.Context, server *url.URL, token string) error {
	conn, err := dialUpgrade(ctx, server, ControlPath, token)
	if err != nil {
		return err
	}
	return conn.Close()
}
