package hosting

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A connection code bundles a relay address and token into one string that
// non-technical users can paste: "uni1.<base64url json>".
const connectPrefix = "uni1."

type connectPayload struct {
	Server string `json:"s"`
	Token  string `json:"t"`
}

// EncodeConnectCode returns the connection code for a relay.
func EncodeConnectCode(server, token string) string {
	b, _ := json.Marshal(connectPayload{Server: server, Token: token})
	return connectPrefix + base64.RawURLEncoding.EncodeToString(b)
}

// DecodeConnectCode parses a connection code. Surrounding spaces, quotes and
// line breaks (from copy and paste) are ignored.
func DecodeConnectCode(code string) (server, token string, err error) {
	code = strings.Trim(strings.Join(strings.Fields(code), ""), `"'`)
	if !strings.HasPrefix(code, connectPrefix) {
		return "", "", errors.New("this doesn't look like a connection code (it should start with uni1.)")
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(strings.TrimPrefix(code, connectPrefix), "="))
	if err != nil {
		return "", "", errors.New("the connection code is incomplete or damaged; copy it again")
	}
	var p connectPayload
	if err := json.Unmarshal(b, &p); err != nil || p.Server == "" {
		return "", "", errors.New("the connection code is incomplete or damaged; copy it again")
	}
	u, err := url.Parse(p.Server)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", "", errors.New("the connection code contains an invalid server address")
	}
	return p.Server, p.Token, nil
}

// RandomToken returns a random token for a relay.
func RandomToken() string {
	b := make([]byte, 18)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// LoadOrCreateToken reads the relay token saved at path, creating one on
// first use, so a relay keeps the same token across restarts.
func LoadOrCreateToken(path string) (string, bool, error) {
	if b, err := os.ReadFile(path); err == nil {
		if tok := strings.TrimSpace(string(b)); tok != "" {
			return tok, false, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	tok := RandomToken()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", false, err
	}
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", false, err
	}
	return tok, true, nil
}

// PublicIPServices answer with the caller's public IP address in plain text.
var PublicIPServices = []string{"https://api.ipify.org", "https://ifconfig.me/ip", "https://icanhazip.com"}

// DetectPublicIP asks well-known services for this machine's public IPv4 address.
func DetectPublicIP(ctx context.Context) (net.IP, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	var lastErr error
	for _, svc := range PublicIPServices {
		req, _ := http.NewRequestWithContext(ctx, "GET", svc, nil)
		req.Header.Set("User-Agent", "curl/8") // some services answer HTML to browsers
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
		resp.Body.Close()
		if ip := net.ParseIP(strings.TrimSpace(string(b))); ip != nil && ip.To4() != nil {
			return ip, nil
		}
		lastErr = fmt.Errorf("%s returned %q", svc, strings.TrimSpace(string(b)))
	}
	return nil, fmt.Errorf("could not detect this server's public IP (%v); pass --domain", lastErr)
}

// IPDomain returns a free wildcard DNS name that resolves to ip, together with
// all of its subdomains: 203.0.113.7 -> 203-0-113-7.sslip.io.
func IPDomain(ip net.IP) string {
	return strings.ReplaceAll(ip.To4().String(), ".", "-") + ".sslip.io"
}
