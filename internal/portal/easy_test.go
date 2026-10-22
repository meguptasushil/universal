package portal

import (
	"context"
	"errors"
	"fmt"
	"io"
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

// TestHelperProcess is not a real test: project tests run the test binary
// itself as a "user project" that serves HTTP on $PORT.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("UNIVERSAL_HELPER") != "1" {
		return
	}
	fmt.Println("helper project listening on", os.Getenv("PORT"))
	_ = http.ListenAndServe("127.0.0.1:"+os.Getenv("PORT"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "project ok")
	}))
	os.Exit(0)
}

func TestFriendly(t *testing.T) {
	cases := map[string]string{
		"relay refused connection: 401 Unauthorized: invalid token":    "access code",
		"dial tcp: lookup nope.example: no such host":                  "doesn't exist",
		"dial tcp 1.2.3.4:443: connect: connection refused":            "isn't answering",
		"relay rejected tunnel: myapp.example.com is already in use":   "already taken",
		"TCP tunnels are disabled on this relay (start it with ...)":   "doesn't accept databases",
		"tls: failed to verify certificate: x509: certificate expired": "certificate",
	}
	for in, want := range cases {
		msg, hint := friendly(errors.New(in))
		if !strings.Contains(msg, want) || hint == "" {
			t.Errorf("%q -> %q / %q", in, msg, hint)
		}
	}
	if msg, hint := friendly(errors.New("something odd")); msg != "something odd" || hint != "" {
		t.Errorf("fallback: %q %q", msg, hint)
	}
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDetect(t *testing.T) {
	mk := func(files map[string]string) string {
		d := t.TempDir()
		for n, b := range files {
			write(t, d, n, b)
		}
		return d
	}
	cases := []struct {
		files   map[string]string
		typ     string
		command string
	}{
		{map[string]string{"package.json": `{"scripts":{"dev":"vite"},"devDependencies":{"vite":"5"}}`}, "node", "npm install && npm run dev -- --port $PORT --strictPort"},
		{map[string]string{"package.json": `{"scripts":{"dev":"next dev"},"dependencies":{"next":"14"}}`}, "node", "npm install && npm run dev -- -p $PORT"},
		{map[string]string{"package.json": `{"scripts":{"start":"node server.js"}}`}, "node", "npm install && npm start"},
		{map[string]string{"package.json": `{}`, "server.js": ""}, "node", "npm install && node server.js"},
		{map[string]string{"manage.py": ""}, "django", "manage.py runserver 127.0.0.1:$PORT"},
		{map[string]string{"main.py": "app = FastAPI()"}, "python", "-m uvicorn main:app --host 127.0.0.1 --port $PORT"},
		{map[string]string{"app.py": "app = Flask(__name__)"}, "python", "-m flask --app app run --port $PORT"},
		{map[string]string{"Dockerfile": "FROM nginx\nEXPOSE 8080\n"}, "docker", "-p 127.0.0.1:$PORT:8080"},
		{map[string]string{"go.mod": "module x"}, "go", "go run ."},
		{map[string]string{"index.html": "<h1>hi</h1>"}, "static", ""},
		{map[string]string{"notes.txt": ""}, "unknown", ""},
	}
	for _, c := range cases {
		d := Detect(mk(c.files))
		if d.Type != c.typ || !strings.Contains(d.Command, c.command) || d.Note == "" {
			t.Errorf("%v: got %+v", c.files, d)
		}
	}
	if d := Detect(filepath.Join(t.TempDir(), "missing")); d.Type != "unknown" {
		t.Errorf("missing folder: %+v", d)
	}
	withModules := mk(map[string]string{"package.json": `{"scripts":{"start":"x"}}`})
	os.Mkdir(filepath.Join(withModules, "node_modules"), 0o755)
	if d := Detect(withModules); d.Command != "npm start" {
		t.Errorf("installed project should not reinstall: %q", d.Command)
	}
}

func TestDiscover(t *testing.T) {
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "<html><head><title>  My Shop  </title></head></html>")
	}))
	defer web.Close()
	plain, _ := net.Listen("tcp", "127.0.0.1:0")
	defer plain.Close()
	go func() {
		for {
			c, err := plain.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	port := func(addr string) int { _, p, _ := net.SplitHostPort(addr); n, _ := strconv.Atoi(p); return n }
	webPort, plainPort, closed := port(strings.TrimPrefix(web.URL, "http://")), port(plain.Addr().String()), freeTestPort(t)
	old := ScanPorts
	ScanPorts = []int{webPort, plainPort, closed}
	defer func() { ScanPorts = old }()

	m, _ := Open(filepath.Join(t.TempDir(), "p.json"), quiet())
	found := m.Discover(context.Background())
	if len(found) != 2 || found[0].Port != webPort || found[0].Kind != "web" || found[0].Name != "My Shop" ||
		found[1].Port != plainPort || found[1].Kind != "tcp" {
		t.Fatalf("found: %+v", found)
	}
	m.Add(App{Target: strconv.Itoa(webPort)})
	if f := m.Discover(context.Background(), plainPort); len(f) != 1 || !f[0].Published {
		t.Fatalf("published / skip: %+v", f)
	}
}

func TestProjectLifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	t.Setenv("UNIVERSAL_HELPER", "1")
	relay := startRelay(t, 0)
	m, _ := Open(filepath.Join(t.TempDir(), "p.json"), quiet())
	defer m.Shutdown()
	m.UpdateSettings(relay, "tok", false)

	dir := t.TempDir()
	cmd := fmt.Sprintf("%q -test.run=TestHelperProcess", os.Args[0])
	a, err := m.Add(App{Name: "shop", Target: dir, Command: cmd, Subdomain: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(a.ID); err != nil {
		t.Fatal(err)
	}
	st := waitStatus(t, m, a.ID, StatusOnline)
	if st.Kind != "project" || st.LocalAddr == "" {
		t.Fatalf("state: %+v", st)
	}
	deadline := time.Now().Add(10 * time.Second)
	for viaRelay(t, relay, "shop.tunnel.test") != "project ok" {
		if time.Now().After(deadline) {
			t.Fatalf("project never served: %q", viaRelay(t, relay, "shop.tunnel.test"))
		}
		time.Sleep(50 * time.Millisecond)
	}
	logs, _ := m.Logs(a.ID)
	if !strings.Contains(strings.Join(logs, "\n"), "helper project listening on") {
		t.Fatalf("logs: %v", logs)
	}
	addr := st.LocalAddr
	m.Stop(a.ID)
	if c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("project still running after Stop")
	}
	if s := m.State().Apps[0]; s.Status != StatusStopped {
		t.Fatalf("after stop: %+v", s)
	}

	// a project that crashes takes its address offline and explains why
	crash, _ := m.Add(App{Target: dir, Command: "echo starting; exit 3"})
	m.Start(crash.ID)
	s := waitStatus(t, m, crash.ID, StatusError)
	if s.Error != "Your project stopped running." || !strings.Contains(s.Hint, "Logs") {
		t.Fatalf("crash: %+v", s)
	}
	logs, _ = m.Logs(crash.ID)
	if !strings.Contains(strings.Join(logs, "\n"), "starting") {
		t.Fatalf("crash logs: %v", logs)
	}
	missing, _ := m.Add(App{Target: dir, Command: "definitely-not-installed-xyz"})
	m.Start(missing.ID)
	s = waitStatus(t, m, missing.ID, StatusError)
	if !strings.Contains(s.Error, "isn't installed") {
		t.Fatalf("missing program: %+v", s)
	}
	if _, err := m.Add(App{Target: filepath.Join(dir, "nope"), Command: "npm start"}); err == nil {
		t.Fatal("expected error for missing project folder")
	}
}

func TestConnectAndBrowseAPI(t *testing.T) {
	relay := startRelay(t, 0)
	m, _ := Open(filepath.Join(t.TempDir(), "p.json"), quiet())
	srv := httptest.NewServer((&Server{M: m}).Handler())
	defer srv.Close()
	post := func(path, body string) (int, string) {
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
		req.Header.Set("X-Portal", "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}
	if code, body := post("/api/connect", `{"code":"not a code"}`); code != 400 || !strings.Contains(body, "connection code") {
		t.Fatalf("bad code: %d %s", code, body)
	}
	if _, body := post("/api/connect", `{"code":"`+hosting.EncodeConnectCode(relay, "wrong")+`"}`); !strings.Contains(body, "access code") {
		t.Fatalf("wrong token: %s", body)
	}
	if s := m.Settings(); s.Server != "" {
		t.Fatalf("a failed connect must not be saved: %+v", s)
	}
	if _, body := post("/api/connect", `{"code":"`+hosting.EncodeConnectCode(relay, "tok")+`"}`); !strings.Contains(body, `"ok":true`) {
		t.Fatalf("connect: %s", body)
	}
	if s := m.Settings(); s.Server != relay || s.Token != "tok" {
		t.Fatalf("settings: %+v", s)
	}

	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "site"), 0o755)
	os.Mkdir(filepath.Join(dir, ".git"), 0o755)
	l := browse(dir)
	if len(l.Dirs) != 1 || l.Dirs[0] != "site" || l.Parent == "" {
		t.Fatalf("browse: %+v", l)
	}
	if browse(filepath.Join(dir, "missing")).Error == "" {
		t.Fatal("expected browse error")
	}
}
