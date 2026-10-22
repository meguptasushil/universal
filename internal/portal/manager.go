// Package portal is a local web UI for publishing apps through a relay:
// add an app, press Start, get a public URL.
package portal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/meguptasushil/universal/internal/hosting"
)

// Settings is the relay the portal publishes through.
type Settings struct {
	Server string `json:"server"`
	Token  string `json:"token"`
}

// App is one thing to publish: a local port, URL, folder, project or TCP service.
type App struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Target     string `json:"target"`            // "3000", "localhost:8080", "http://...", "tcp://host:5432" or a folder path
	Command    string `json:"command,omitempty"` // for project folders: how to start it ($PORT = assigned port)
	TCP        bool   `json:"tcp"`
	Subdomain  string `json:"subdomain,omitempty"`
	Hostname   string `json:"hostname,omitempty"`
	RemotePort int    `json:"remote_port,omitempty"`
	Allow      string `json:"allow,omitempty"`
	SPA        bool   `json:"spa,omitempty"`
	Enabled    bool   `json:"enabled"` // keep online; restored when the portal restarts
}

// Config is what the portal saves to disk.
type Config struct {
	Settings Settings `json:"settings"`
	Apps     []App    `json:"apps"`
}

// Status values.
const (
	StatusStopped      = "stopped"
	StatusConnecting   = "connecting"
	StatusOnline       = "online"
	StatusReconnecting = "reconnecting"
	StatusError        = "error"
)

// AppState is an App plus its live status.
type AppState struct {
	App
	Kind        string    `json:"kind"` // web, folder, project, tcp
	Status      string    `json:"status"`
	PublicURL   string    `json:"public_url,omitempty"`
	Error       string    `json:"error,omitempty"`  // plain-language problem
	Hint        string    `json:"hint,omitempty"`   // what to do about it
	Detail      string    `json:"detail,omitempty"` // the technical error`
	OnlineSince time.Time `json:"online_since,omitempty"`
	Connections int64     `json:"connections"`
	LocalAddr   string    `json:"local_addr,omitempty"`
	LocalUp     bool      `json:"local_up"`
}

type runner struct {
	cancel     context.CancelFunc
	done       chan struct{}
	state      AppState
	stopping   bool // Stop was requested: a project exiting now is expected
	procFailed bool // the project process died on its own
}

// setError records err on s in plain language. Caller holds mu.
func setError(s *AppState, status string, err error) {
	s.Status = status
	s.Error, s.Hint = friendly(err)
	s.Detail = err.Error()
}

func clearError(s *AppState) { s.Error, s.Hint, s.Detail = "", "", "" }

// Manager owns the configuration and the running tunnels.
type Manager struct {
	path string
	log  *slog.Logger

	mu   sync.Mutex
	cfg  Config
	runs map[string]*runner
	logs map[string]*logBuffer // project output, kept after a project stops
}

// DefaultConfigPath is ~/.universal/portal.json.
func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "portal.json"
	}
	return filepath.Join(home, ".universal", "portal.json")
}

// Open loads the config at path (a missing file is an empty config).
func Open(path string, log *slog.Logger) (*Manager, error) {
	if log == nil {
		log = slog.Default()
	}
	m := &Manager{path: path, log: log, runs: map[string]*runner{}, logs: map[string]*logBuffer{}}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &m.cfg); err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	return m, nil
}

// save writes the config atomically. Caller holds mu.
func (m *Manager) save() error {
	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(m.cfg, "", "  ")
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil { // holds the relay token
		return err
	}
	return os.Rename(tmp, m.path)
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// resolve works out what an app points at: a folder, or a host:port.
func resolve(a *App) (kind, addr string, err error) {
	t := strings.TrimSpace(a.Target)
	if t == "" {
		return "", "", errors.New("enter a port, address or folder to publish")
	}
	if st, statErr := os.Stat(t); statErr == nil && st.IsDir() {
		if a.TCP {
			return "", "", errors.New("a folder can only be published as a website, not as a TCP service")
		}
		if strings.TrimSpace(a.Command) != "" {
			return "project", t, nil
		}
		return "folder", t, nil
	}
	if strings.TrimSpace(a.Command) != "" {
		return "", "", fmt.Errorf("the folder %q doesn't exist on this computer", t)
	}
	addr, err = hosting.LocalTarget(t)
	if err != nil {
		return "", "", err
	}
	if a.TCP || strings.HasPrefix(t, "tcp://") {
		return "tcp", addr, nil
	}
	return "web", addr, nil
}

func validate(a *App) error {
	a.Target = strings.TrimSpace(a.Target)
	a.Command = strings.TrimSpace(a.Command)
	a.Subdomain = strings.ToLower(strings.TrimSpace(a.Subdomain))
	a.Hostname = strings.ToLower(strings.TrimSpace(a.Hostname))
	kind, _, err := resolve(a)
	if err != nil {
		return err
	}
	a.TCP = kind == "tcp"
	if a.Name = strings.TrimSpace(a.Name); a.Name == "" {
		a.Name = a.Target
	}
	if a.Subdomain != "" && !hosting.ValidSubdomain(a.Subdomain) {
		return fmt.Errorf("public name %q may only use a-z, 0-9 and '-'", a.Subdomain)
	}
	if a.Hostname != "" && !hosting.ValidHostname(a.Hostname) {
		return fmt.Errorf("invalid hostname %q", a.Hostname)
	}
	if a.TCP && (a.Subdomain != "" || a.Hostname != "") {
		return errors.New("TCP services get a public port, not a name; leave the public name empty")
	}
	if !a.TCP && (a.RemotePort != 0 || a.Allow != "") {
		return errors.New("public port and IP allow-list only apply to TCP services")
	}
	if a.RemotePort < 0 || a.RemotePort > 65535 {
		return errors.New("invalid public port")
	}
	if _, err := hosting.ParseAllowList(a.Allow); err != nil {
		return err
	}
	return nil
}

func (m *Manager) find(id string) int {
	for i := range m.cfg.Apps {
		if m.cfg.Apps[i].ID == id {
			return i
		}
	}
	return -1
}

// ErrNotFound is returned for unknown app ids.
var ErrNotFound = errors.New("app not found")

// SettingsView is what the UI sees: never the token itself.
type SettingsView struct {
	Server   string `json:"server"`
	TokenSet bool   `json:"token_set"`
	Domain   string `json:"domain"`
}

// State is the full UI state.
type State struct {
	Settings SettingsView `json:"settings"`
	Apps     []AppState   `json:"apps"`
}

// State returns settings and every app with its live status.
func (m *Manager) State() State {
	m.mu.Lock()
	st := State{Settings: SettingsView{Server: m.cfg.Settings.Server, TokenSet: m.cfg.Settings.Token != ""}}
	if u, err := url.Parse(m.cfg.Settings.Server); err == nil {
		st.Settings.Domain = u.Hostname()
	}
	for _, a := range m.cfg.Apps {
		s := AppState{App: a, Status: StatusStopped}
		if r := m.runs[a.ID]; r != nil {
			s = r.state
			s.App = a
		}
		if kind, addr, err := resolve(&a); err == nil {
			s.Kind = kind
			if kind == "web" || kind == "tcp" {
				s.LocalAddr = addr
			}
		}
		st.Apps = append(st.Apps, s)
	}
	m.mu.Unlock()
	// the relay's real domain, once a tunnel has reported its public URL
	// (the relay address may be an IP or a different host)
	for _, a := range st.Apps {
		if u, err := url.Parse(a.PublicURL); err == nil && u.Hostname() != "" && a.Hostname == "" {
			host := u.Hostname()
			if a.Kind != "tcp" {
				_, host, _ = strings.Cut(host, ".")
			}
			if host != "" {
				st.Settings.Domain = host
				break
			}
		}
	}

	// is anything listening locally? (checked outside the lock, in parallel)
	var wg sync.WaitGroup
	for i := range st.Apps {
		a := &st.Apps[i]
		if a.Kind == "folder" {
			a.LocalUp = true
			continue
		}
		if a.LocalAddr == "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c, err := net.DialTimeout("tcp", a.LocalAddr, 300*time.Millisecond); err == nil {
				c.Close()
				a.LocalUp = true
			}
		}()
	}
	wg.Wait()
	return st
}

// UpdateSettings changes the relay and restarts running apps to apply it.
// An empty token keeps the current one unless clearToken is set.
func (m *Manager) UpdateSettings(server, token string, clearToken bool) error {
	server = strings.TrimRight(strings.TrimSpace(server), "/")
	if server != "" {
		if _, err := parseServer(server); err != nil {
			return err
		}
	}
	m.mu.Lock()
	m.cfg.Settings.Server = server
	if token != "" || clearToken {
		m.cfg.Settings.Token = strings.TrimSpace(token)
	}
	err := m.save()
	var running []string
	for id := range m.runs {
		running = append(running, id)
	}
	m.mu.Unlock()
	for _, id := range running {
		m.Stop(id)
		_ = m.Start(id)
	}
	return err
}

func parseServer(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("relay address must look like https://example.com (got %q)", s)
	}
	return u, nil
}

// Settings returns the saved relay settings.
func (m *Manager) Settings() Settings {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg.Settings
}

// Connect saves the relay from a connection code.
func (m *Manager) Connect(code string) error {
	server, token, err := hosting.DecodeConnectCode(code)
	if err != nil {
		return err
	}
	return m.UpdateSettings(server, token, true)
}

// Logs returns the latest output of a project app.
func (m *Manager) Logs(id string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.find(id) < 0 {
		return nil, ErrNotFound
	}
	if l := m.logs[id]; l != nil {
		return l.Lines(), nil
	}
	return []string{}, nil
}

// TestRelay checks that the configured relay is reachable and accepts the token.
func (m *Manager) TestRelay(ctx context.Context) error {
	m.mu.Lock()
	s := m.cfg.Settings
	m.mu.Unlock()
	if s.Server == "" {
		return errors.New("enter the relay address first")
	}
	u, err := parseServer(s.Server)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return hosting.CheckRelay(ctx, u, s.Token)
}

// Add saves a new app. It is not started.
func (m *Manager) Add(a App) (App, error) {
	if err := validate(&a); err != nil {
		return a, err
	}
	a.ID, a.Enabled = newID(), false
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg.Apps = append(m.cfg.Apps, a)
	return a, m.save()
}

// Update replaces an app's settings, restarting it if it was running.
func (m *Manager) Update(id string, a App) (App, error) {
	if err := validate(&a); err != nil {
		return a, err
	}
	m.mu.Lock()
	i := m.find(id)
	if i < 0 {
		m.mu.Unlock()
		return a, ErrNotFound
	}
	a.ID, a.Enabled = id, m.cfg.Apps[i].Enabled
	m.cfg.Apps[i] = a
	err := m.save()
	_, running := m.runs[id]
	m.mu.Unlock()
	if running {
		m.Stop(id)
		err = errors.Join(err, m.Start(id))
	}
	return a, err
}

// Delete stops and removes an app.
func (m *Manager) Delete(id string) error {
	m.Stop(id)
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.find(id)
	if i < 0 {
		return ErrNotFound
	}
	m.cfg.Apps = append(m.cfg.Apps[:i], m.cfg.Apps[i+1:]...)
	return m.save()
}

// Start publishes an app and marks it to come back online after a restart.
func (m *Manager) Start(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.find(id)
	if i < 0 {
		return ErrNotFound
	}
	if r, ok := m.runs[id]; ok {
		select {
		case <-r.done: // failed earlier: start again
			delete(m.runs, id)
		default:
			return nil // already running
		}
	}
	s := m.cfg.Settings
	if s.Server == "" {
		return errors.New("set the relay address in Settings first")
	}
	server, err := parseServer(s.Server)
	if err != nil {
		return err
	}
	a := m.cfg.Apps[i]
	kind, addr, err := resolve(&a)
	if err != nil {
		return err
	}
	allow, err := hosting.ParseAllowList(a.Allow)
	if err != nil {
		return err
	}

	var ln net.Listener
	var proc *exec.Cmd
	var procExited chan struct{}
	logs := m.logs[id]
	switch kind {
	case "folder":
		h, err := hosting.StaticHandler(addr, a.SPA)
		if err != nil {
			return err
		}
		if ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			return err
		}
		go func() { _ = http.Serve(ln, h) }()
		addr = ln.Addr().String()
	case "project":
		dir, err := filepath.Abs(addr)
		if err != nil {
			return err
		}
		port, err := freePort()
		if err != nil {
			return err
		}
		command := strings.ReplaceAll(a.Command, "$PORT", port)
		if logs == nil {
			logs = &logBuffer{}
			m.logs[id] = logs
		}
		logs.Note("starting in " + dir + ": " + command)
		proc = shellCommand(command)
		proc.Dir = dir
		proc.Env = append(os.Environ(), "PORT="+port)
		proc.Stdout, proc.Stderr = logs, logs
		if err := proc.Start(); err != nil {
			logs.Note("could not start: " + err.Error())
			return err
		}
		procExited = make(chan struct{})
		addr = "127.0.0.1:" + port
	}

	ctx, cancel := context.WithCancel(context.Background())
	r := &runner{cancel: cancel, done: make(chan struct{}), state: AppState{App: a, Kind: kind, Status: StatusConnecting}}
	if kind == "project" {
		r.state.LocalAddr = addr
	}
	m.runs[id] = r
	m.cfg.Apps[i].Enabled = true
	saveErr := m.save()

	update := func(f func(s *AppState)) {
		m.mu.Lock()
		f(&r.state)
		m.mu.Unlock()
	}
	c := &hosting.Client{
		Server: server, Token: s.Token, Subdomain: a.Subdomain, Hostname: a.Hostname, Target: addr,
		TCP: a.TCP, RemotePort: a.RemotePort, Allow: allow, Log: m.log.With("app", a.Name),
		OnReady: func(u string) {
			update(func(s *AppState) {
				s.Status, s.PublicURL, s.OnlineSince = StatusOnline, u, time.Now().UTC()
				clearError(s)
			})
		},
		OnDown: func(err error) {
			update(func(s *AppState) { setError(s, StatusReconnecting, err) })
		},
		OnConn: func(string) { update(func(s *AppState) { s.Connections++ }) },
	}
	if proc != nil {
		go func() {
			err := proc.Wait()
			close(procExited)
			m.mu.Lock()
			expected := r.stopping
			if !expected {
				r.procFailed = true
				r.state.Status = StatusError
				r.state.Error = "Your project stopped running."
				r.state.Hint = "Open Logs to see what went wrong, fix it, then press Start."
				r.state.Detail = fmt.Sprint(err)
				if code := exitCode(err); code == 127 || code == 9009 {
					r.state.Error, r.state.Hint = friendly(errors.New("command not found"))
				}
			}
			m.mu.Unlock()
			logs.Note(fmt.Sprintf("project exited (%v)", err))
			if !expected {
				r.cancel() // take the public address offline too
			}
		}()
	}
	go func() {
		defer close(r.done)
		err := c.Run(ctx)
		if ln != nil {
			ln.Close()
		}
		if proc != nil {
			m.mu.Lock()
			r.stopping = true
			m.mu.Unlock()
			killTree(proc, procExited)
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		switch {
		case r.procFailed: // keep the project's error
		case err != nil:
			setError(&r.state, StatusError, err)
			// keep the failed runner visible until Stop or the next Start
		default:
			r.state.Status = StatusStopped
		}
	}()
	m.log.Info("publishing", "app", a.Name, "target", a.Target)
	return saveErr
}

// Stop takes an app offline.
func (m *Manager) Stop(id string) {
	m.mu.Lock()
	r := m.runs[id]
	delete(m.runs, id)
	if r != nil {
		r.stopping = true
	}
	if i := m.find(id); i >= 0 && r != nil {
		m.cfg.Apps[i].Enabled = false
		_ = m.save()
	}
	m.mu.Unlock()
	if r != nil {
		r.cancel()
		<-r.done
	}
}

// StartEnabled brings back every app that was online when the portal last ran.
func (m *Manager) StartEnabled() {
	m.mu.Lock()
	var ids []string
	for _, a := range m.cfg.Apps {
		if a.Enabled {
			ids = append(ids, a.ID)
		}
	}
	m.mu.Unlock()
	for _, id := range ids {
		if err := m.Start(id); err != nil {
			m.log.Warn("could not restore app", "id", id, "err", err)
		}
	}
}

// Shutdown stops all tunnels but remembers which were online.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	runs := m.runs
	m.runs = map[string]*runner{}
	for _, r := range runs {
		r.stopping = true
	}
	m.mu.Unlock()
	ids := make([]string, 0, len(runs))
	for id := range runs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		runs[id].cancel()
		<-runs[id].done
	}
}

func freePort() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return port, nil
}

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return 0
}
