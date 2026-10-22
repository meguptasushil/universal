package hosting

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// freePort returns a port that was free a moment ago.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// echoServer stands in for a database or any line-based TCP service.
func echoServer(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				sc := bufio.NewScanner(c)
				for sc.Scan() {
					io.WriteString(c, "db:"+sc.Text()+"\n")
				}
			}()
		}
	}()
	return l.Addr().String()
}

func startTCPRelay(t *testing.T, port int) *url.URL {
	t.Helper()
	r := &Relay{Domain: "tunnel.test", Tokens: []string{"tok"}, PublicScheme: "http",
		TCPPortMin: port, TCPPortMax: port, TCPBind: "127.0.0.1", Log: quietLog()}
	srv := httptest.NewServer(AccessLog(quietLog(), r))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return u
}

func roundTrip(t *testing.T, port int, line string) (string, error) {
	t.Helper()
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 2*time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	io.WriteString(c, line+"\n")
	return bufio.NewReader(c).ReadString('\n')
}

func TestTCPTunnel(t *testing.T) {
	port := freePort(t)
	relayURL := startTCPRelay(t, port)
	pub, _, _ := startClient(t, relayURL, Client{Token: "tok", TCP: true, RemotePort: port, Target: echoServer(t)})
	if pub != "tcp://tunnel.test:"+strconv.Itoa(port) {
		t.Fatalf("public address %q", pub)
	}
	// several concurrent "database sessions"
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		go func(i int) {
			want := "db:SELECT " + strconv.Itoa(i) + "\n"
			got, err := roundTrip(t, port, "SELECT "+strconv.Itoa(i))
			if err == nil && got != want {
				err = errors.New("got " + got)
			}
			errs <- err
		}(i)
	}
	for i := 0; i < 10; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

func TestTCPTunnelAllowList(t *testing.T) {
	port := freePort(t)
	relayURL := startTCPRelay(t, port)
	allow, err := ParseAllowList("203.0.113.7, 10.0.0.0/8")
	if err != nil || len(allow) != 2 {
		t.Fatalf("allow list: %v %v", allow, err)
	}
	startClient(t, relayURL, Client{Token: "tok", TCP: true, Target: echoServer(t), Allow: allow})
	// 127.0.0.1 is not on the list, so the connection must be dropped
	if got, err := roundTrip(t, port, "SELECT 1"); err == nil {
		t.Fatalf("blocked visitor got %q", got)
	}
	if _, err := ParseAllowList("not-an-ip"); err == nil {
		t.Fatal("expected error")
	}
}

func TestTCPTunnelRejections(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var rej *RejectedError

	_, plain := startRelay(t) // no TCP range configured
	c := Client{Server: plain, Token: "tok", TCP: true, Target: "127.0.0.1:1", Log: quietLog()}
	if err := c.Run(ctx); !errors.As(err, &rej) || !strings.Contains(rej.Reason, "disabled") {
		t.Fatalf("disabled: %v", err)
	}

	port := freePort(t)
	relayURL := startTCPRelay(t, port)
	c = Client{Server: relayURL, Token: "tok", TCP: true, RemotePort: port + 1, Target: "127.0.0.1:1", Log: quietLog()}
	if err := c.Run(ctx); !errors.As(err, &rej) || !strings.Contains(rej.Reason, "outside") {
		t.Fatalf("out of range: %v", err)
	}
	startClient(t, relayURL, Client{Token: "tok", TCP: true, Target: "127.0.0.1:1"})
	c = Client{Server: relayURL, Token: "tok", TCP: true, Target: "127.0.0.1:1", Log: quietLog()}
	if err := c.Run(ctx); !errors.As(err, &rej) || !strings.Contains(rej.Reason, "no free TCP port") {
		t.Fatalf("exhausted: %v", err)
	}
}
