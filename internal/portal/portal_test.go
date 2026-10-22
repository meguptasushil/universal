package portal

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/meguptasushil/universal/internal/hosting"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func freeTestPort(t *testing.T) int {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func startRelay(t *testing.T, tcpPort int) string {
	r := &hosting.Relay{Domain: "tunnel.test", Tokens: []string{"tok"}, PublicScheme: "http",
		TCPPortMin: tcpPort, TCPPortMax: tcpPort, TCPBind: "127.0.0.1", Log: quiet()}
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv.URL
}

func waitStatus(t *testing.T, m *Manager, id, want string) AppState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, a := range m.State().Apps {
			if a.ID == id && a.Status == want {
				return a
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("app %s never became %s: %+v", id, want, m.State().Apps)
	return AppState{}
}

// viaRelay fetches path from the relay as a visitor of host.
func viaRelay(t *testing.T, relay, host string) string {
	req, _ := http.NewRequest("GET", relay+"/", nil)
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestPublishWebFolderAndTCP(t *testing.T) {
	tcpPort := freeTestPort(t)
	relay := startRelay(t, tcpPort)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "backend ok") }))
	defer backend.Close()
	site := t.TempDir()
	os.WriteFile(filepath.Join(site, "index.html"), []byte("folder ok"), 0o644)
	db, _ := net.Listen("tcp", "127.0.0.1:0")
	defer db.Close()
	go func() {
		for {
			c, err := db.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); l, _ := bufio.NewReader(c).ReadString('\n'); io.WriteString(c, "db:"+l) }()
		}
	}()

	cfg := filepath.Join(t.TempDir(), "portal.json")
	m, err := Open(cfg, quiet())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateSettings(relay, "tok", false); err != nil {
		t.Fatal(err)
	}
	if err := m.TestRelay(context.Background()); err != nil {
		t.Fatalf("relay test: %v", err)
	}

	web, err := m.Add(App{Name: "api", Target: strings.TrimPrefix(backend.URL, "http://"), Subdomain: "api"})
	if err != nil {
		t.Fatal(err)
	}
	folder, _ := m.Add(App{Target: site, Subdomain: "www"})
	dbApp, err := m.Add(App{Name: "postgres", Target: db.Addr().String(), TCP: true, RemotePort: tcpPort})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{web.ID, folder.ID, dbApp.ID} {
		if err := m.Start(id); err != nil {
			t.Fatal(err)
		}
	}
	w := waitStatus(t, m, web.ID, StatusOnline)
	if w.PublicURL != "http://api.tunnel.test" || !w.LocalUp || w.Kind != "web" {
		t.Fatalf("web state: %+v", w)
	}
	waitStatus(t, m, folder.ID, StatusOnline)
	d := waitStatus(t, m, dbApp.ID, StatusOnline)
	if d.PublicURL != "tcp://tunnel.test:"+strconv.Itoa(tcpPort) || d.Kind != "tcp" {
		t.Fatalf("tcp state: %+v", d)
	}

	if got := viaRelay(t, relay, "api.tunnel.test"); got != "backend ok" {
		t.Fatalf("web: %q", got)
	}
	if got := viaRelay(t, relay, "www.tunnel.test"); got != "folder ok" {
		t.Fatalf("folder: %q", got)
	}
	c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(tcpPort))
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(c, "SELECT 1\n")
	line, _ := bufio.NewReader(c).ReadString('\n')
	c.Close()
	if line != "db:SELECT 1\n" {
		t.Fatalf("tcp: %q", line)
	}
	for _, a := range m.State().Apps {
		if a.ID == web.ID && a.Connections < 1 {
			t.Fatalf("connections not counted: %+v", a)
		}
	}

	// stop one, then "restart the portal": enabled apps come back by themselves
	m.Stop(folder.ID)
	if got := viaRelay(t, relay, "www.tunnel.test"); !strings.Contains(got, "Nothing is hosted") {
		t.Fatalf("stopped folder still served: %q", got)
	}
	m.Shutdown()
	m2, err := Open(cfg, quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Shutdown()
	m2.StartEnabled()
	waitStatus(t, m2, web.ID, StatusOnline)
	waitStatus(t, m2, dbApp.ID, StatusOnline)
	for _, a := range m2.State().Apps {
		if a.ID == folder.ID && a.Status != StatusStopped {
			t.Fatalf("stopped app restarted: %+v", a)
		}
	}
	raw, _ := os.ReadFile(cfg)
	if st, _ := os.Stat(cfg); (runtime.GOOS != "windows" && st.Mode().Perm() != 0o600) || !strings.Contains(string(raw), `"token": "tok"`) {
		t.Fatalf("config not saved privately: %v", st.Mode())
	}
	if err := m2.Delete(web.ID); err != nil || len(m2.State().Apps) != 2 {
		t.Fatalf("delete: %v", err)
	}
}

func TestBadTokenShowsError(t *testing.T) {
	relay := startRelay(t, 0)
	m, _ := Open(filepath.Join(t.TempDir(), "p.json"), quiet())
	defer m.Shutdown()
	m.UpdateSettings(relay, "wrong", false)
	if err := m.TestRelay(context.Background()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("relay test with bad token: %v", err)
	}
	a, _ := m.Add(App{Target: "3000"})
	m.Start(a.ID)
	s := waitStatus(t, m, a.ID, StatusError)
	if !strings.Contains(s.Detail, "invalid token") || s.Error != "The relay didn't accept your access code." || s.Hint == "" {
		t.Fatalf("error: %q / %q / %q", s.Error, s.Hint, s.Detail)
	}
	// starting again after fixing the token works
	m.UpdateSettings(relay, "tok", false)
	if err := m.Start(a.ID); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, m, a.ID, StatusOnline)
}

func TestValidation(t *testing.T) {
	m, _ := Open(filepath.Join(t.TempDir(), "p.json"), quiet())
	dir := t.TempDir()
	bad := []App{
		{Target: ""},
		{Target: "not a target"},
		{Target: "3000", Subdomain: "Bad Name"},
		{Target: "5432", TCP: true, Subdomain: "db"},
		{Target: "3000", Allow: "10.0.0.0/8"},
		{Target: dir, TCP: true},
		{Target: "5432", TCP: true, Allow: "nope"},
	}
	for _, a := range bad {
		if _, err := m.Add(a); err == nil {
			t.Errorf("expected error for %+v", a)
		}
	}
	a, err := m.Add(App{Target: "tcp://localhost:6379"})
	if err != nil || !a.TCP || a.Name != "tcp://localhost:6379" {
		t.Fatalf("tcp url: %+v %v", a, err)
	}
	if err := m.Start(a.ID); err == nil || !strings.Contains(err.Error(), "relay") {
		t.Fatalf("start without relay: %v", err)
	}
	if err := m.UpdateSettings("ftp://x", "", false); err == nil {
		t.Fatal("expected bad relay url error")
	}
}

func TestServerGuard(t *testing.T) {
	m, _ := Open(filepath.Join(t.TempDir(), "p.json"), quiet())
	srv := httptest.NewServer((&Server{M: m}).Handler())
	defer srv.Close()
	do := func(method, path, host, body string, hdr map[string]string) int {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if host != "" {
			req.Host = host
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	portalHdr := map[string]string{"X-Portal": "1"}
	if c := do("GET", "/", "", "", nil); c != 200 {
		t.Fatalf("ui: %d", c)
	}
	if c := do("POST", "/api/apps", "", `{"target":"3000"}`, nil); c != 403 {
		t.Fatalf("missing X-Portal (CSRF): %d", c)
	}
	if c := do("GET", "/api/state", "evil.example", "", portalHdr); c != 403 {
		t.Fatalf("DNS rebinding host: %d", c)
	}
	if c := do("POST", "/api/apps", "", `{"target":"3000","name":"x"}`, portalHdr); c != 200 {
		t.Fatalf("add: %d", c)
	}
	if c := do("POST", "/api/apps/nope/start", "", "", portalHdr); c != 404 {
		t.Fatalf("unknown app: %d", c)
	}

	// with a token, other hosts are fine but the token is required
	tsrv := httptest.NewServer((&Server{M: m, Token: "s3"}).Handler())
	defer tsrv.Close()
	req, _ := http.NewRequest("GET", tsrv.URL+"/api/state", nil)
	req.Host = "portal.lan"
	req.Header.Set("X-Portal", "1")
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 401 {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	req.Header.Set("Authorization", "Bearer s3")
	resp, _ := http.DefaultClient.Do(req)
	var st State
	json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(st.Apps) != 1 {
		t.Fatalf("with token: %d %+v", resp.StatusCode, st)
	}
	b, _ := json.Marshal(st)
	if strings.Contains(string(b), `"token"`) {
		t.Fatal("state must not expose the relay token")
	}
}
