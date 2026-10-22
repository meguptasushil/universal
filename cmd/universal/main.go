// Command universal monitors AI agents and hosts local servers on the internet.
//
//	universal monitor   run the AI agent monitor (LLM gateway + dashboard)
//	universal relay     run the public tunnel relay on a server with a public IP
//	universal expose    publish a local port or folder through a relay
//	universal serve     host a folder or local app directly with automatic HTTPS
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/meguptasushil/universal/internal/hosting"
	"github.com/meguptasushil/universal/internal/monitor"
	"github.com/meguptasushil/universal/internal/portal"
)

var version = "dev"

const usage = `universal: monitor AI agents and host anything on the internet

Usage:
  universal monitor [flags]                 AI agent monitor: LLM gateway + live dashboard
  universal report  [--since 24h] [flags]   agent activity report: agents, IDs, sessions, tools
  universal relay   --domain D --token T    public relay for tunnels (run on a cloud VM)
  universal expose  <port|host:port|dir>    publish a local website/API or folder via a relay
  universal expose  5432 --tcp              publish any TCP service (database, SSH, Redis...)
  universal serve   <dir|port|url>          host directly with automatic HTTPS (public IP needed)
  universal portal                          web UI to publish local apps without the command line
  universal version

Run "universal <command> -h" for the flags of a command.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "monitor":
		err = runMonitor(ctx, log, args)
	case "report":
		err = runReport(ctx, args)
	case "relay":
		err = runRelay(ctx, log, args)
	case "expose":
		err = runExpose(ctx, log, args)
	case "serve":
		err = runServe(ctx, log, args)
	case "portal":
		err = runPortal(ctx, log, args)
	case "version", "--version", "-v":
		fmt.Println("universal", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// parse lets a single positional argument appear before or after the flags.
func parse(fs *flag.FlagSet, args []string) (string, error) {
	var pos string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		pos, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if pos == "" {
		pos = fs.Arg(0)
	}
	return pos, nil
}

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ---------------------------------------------------------------- monitor

func runMonitor(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("monitor", flag.ContinueOnError)
	listen := fs.String("listen", env("UNIVERSAL_MONITOR_LISTEN", "127.0.0.1:8787"), "address to listen on (use 0.0.0.0:8787 to accept agents from other machines)")
	dataDir := fs.String("data", env("UNIVERSAL_MONITOR_DATA", "./monitor-data"), "directory for the event log (empty: memory only)")
	token := fs.String("token", env("UNIVERSAL_MONITOR_TOKEN", ""), "token required for the dashboard API and ingest")
	maxText := fs.Int("max-text", 4096, "truncate recorded prompts/responses to this many bytes")
	noContent := fs.Bool("no-content", false, "record metadata only (model, tokens, tool names), never prompt or response text")
	capacity := fs.Int("capacity", 10000, "number of recent events kept in memory")
	var extra listFlag
	fs.Var(&extra, "upstream", "extra provider as name=url[,openai|anthropic|gemini|auto] (repeatable)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	ups := monitor.DefaultUpstreams()
	for _, spec := range extra {
		u, err := monitor.ParseUpstream(spec)
		if err != nil {
			return err
		}
		ups[u.Name] = u
	}
	host, _, _ := net.SplitHostPort(*listen)
	if host != "127.0.0.1" && host != "localhost" && host != "::1" && *token == "" {
		log.Warn("monitor is reachable from the network without --token; anyone who can reach it can read recorded prompts")
	}
	store, err := monitor.OpenStore(*dataDir, *capacity)
	if err != nil {
		return err
	}
	defer store.Close()
	srv := &monitor.Server{
		Gateway: &monitor.Gateway{Store: store, Upstreams: ups, MaxText: *maxText, NoContent: *noContent, Log: log},
		Token:   *token,
	}
	base := "http://" + *listen
	dash := base + "/"
	if *token != "" {
		dash += "?token=" + url.QueryEscape(*token)
	}
	fmt.Fprintf(os.Stderr, `
  AI Agent Monitor
  Dashboard:  %s

  Point your agents at the gateway:
    Claude Code / Anthropic SDK   export ANTHROPIC_BASE_URL=%s/anthropic
    Codex / OpenAI SDK            export OPENAI_BASE_URL=%s/openai/v1
    Gemini CLI / Google GenAI     export GOOGLE_GEMINI_BASE_URL=%s/gemini
    Any provider, named agent     %s/a/<agent-name>/<provider>/...
  Hooks / custom events:          POST %s/api/ingest
  Identify instances (optional):  headers X-Agent-Id, X-Agent-User, X-Agent-Session
  Reports:                        universal report --since 24h

`, dash, base, base, base, base, base)
	return hosting.ServePlain(ctx, srv, *listen)
}

// ---------------------------------------------------------------- report

func runReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	dataDir := fs.String("data", env("UNIVERSAL_MONITOR_DATA", "./monitor-data"), "monitor data directory to read")
	server := fs.String("server", env("UNIVERSAL_MONITOR_URL", ""), "read from a running monitor instead, e.g. http://127.0.0.1:8787")
	token := fs.String("token", env("UNIVERSAL_MONITOR_TOKEN", ""), "monitor token (with --server)")
	since := fs.String("since", "", "only activity after this: 30m, 24h, 7d or 2026-10-01")
	format := fs.String("format", "text", "text, json or csv (csv lists every matching event)")
	out := fs.String("out", "", "write the report to this file instead of stdout")
	recent := fs.Int("recent", 20, "number of recent events to list")
	limit := fs.Int("limit", 20, "maximum rows per table")
	watch := fs.Duration("watch", 0, "refresh the report every interval, e.g. 5s (text only)")
	var f monitor.Filter
	fs.StringVar(&f.Agent, "agent", "", "only this agent (e.g. claude-code)")
	fs.StringVar(&f.AgentID, "agent-id", "", "only this agent instance ID")
	fs.StringVar(&f.User, "user", "", "only this user/account ID")
	fs.StringVar(&f.KeyID, "key", "", "only this API key fingerprint (as shown in the report)")
	fs.StringVar(&f.Session, "session", "", "only this session")
	fs.StringVar(&f.Provider, "provider", "", "only this provider")
	fs.StringVar(&f.Model, "model", "", "only this model")
	fs.StringVar(&f.Kind, "kind", "", "only this kind: llm_call, tool_use, prompt, session, custom")
	fs.StringVar(&f.Query, "q", "", "only events containing this text")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	var err error
	if f.Since, err = monitor.ParseSince(*since); err != nil {
		return err
	}
	if *format != "text" && *format != "json" && *format != "csv" {
		return fmt.Errorf("unknown --format %q", *format)
	}
	n := *recent
	if *format == "csv" {
		n = -1
	}

	render := func() error {
		rep, err := loadReport(ctx, *dataDir, *server, *token, f, n)
		if err != nil {
			return err
		}
		w := io.Writer(os.Stdout)
		if *out != "" {
			file, err := os.Create(*out)
			if err != nil {
				return err
			}
			defer file.Close()
			w = file
		}
		switch *format {
		case "json":
			return rep.WriteJSON(w)
		case "csv":
			return rep.WriteCSV(w)
		}
		return rep.WriteText(w, *limit)
	}
	if *watch <= 0 || *format != "text" || *out != "" {
		return render()
	}
	tick := time.NewTicker(*watch)
	defer tick.Stop()
	for {
		fmt.Print("\033[H\033[2J") // clear the terminal
		if err := render(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// loadReport reads the monitor's event log, or asks a running monitor. With
// neither given explicitly it falls back to a monitor on the default address.
func loadReport(ctx context.Context, dataDir, server, token string, f monitor.Filter, recent int) (monitor.Report, error) {
	if server == "" {
		events, err := monitor.LoadEvents(dataDir)
		if err == nil {
			return monitor.BuildReport(events, f, recent), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return monitor.Report{}, err
		}
		rep, srvErr := fetchReport(ctx, "http://127.0.0.1:8787", token, f, recent)
		if srvErr != nil {
			return monitor.Report{}, fmt.Errorf("no event log in %s and no monitor running on 127.0.0.1:8787; "+
				"start one with \"universal monitor\", or pass --data or --server", dataDir)
		}
		return rep, nil
	}
	return fetchReport(ctx, server, token, f, recent)
}

func fetchReport(ctx context.Context, server, token string, f monitor.Filter, recent int) (monitor.Report, error) {
	q := url.Values{"format": {"json"}, "recent": {strconv.Itoa(recent)}}
	for k, v := range map[string]string{"agent": f.Agent, "agent_id": f.AgentID, "user": f.User, "key_id": f.KeyID,
		"session": f.Session, "provider": f.Provider, "model": f.Model, "kind": f.Kind, "q": f.Query} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if !f.Since.IsZero() {
		q.Set("since", f.Since.Format(time.RFC3339))
	}
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimSuffix(server, "/")+"/api/report?"+q.Encode(), nil)
	if err != nil {
		return monitor.Report{}, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	cl := &http.Client{Timeout: 15 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return monitor.Report{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return monitor.Report{}, fmt.Errorf("monitor answered %s (wrong --token?)", resp.Status)
	}
	var rep monitor.Report
	return rep, json.NewDecoder(resp.Body).Decode(&rep)
}

// ---------------------------------------------------------------- relay

func runRelay(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("relay", flag.ContinueOnError)
	domain := fs.String("domain", env("UNIVERSAL_DOMAIN", ""), "apex domain; point example.com and *.example.com at this machine")
	tokens := fs.String("token", env("UNIVERSAL_TOKENS", ""), "comma-separated tokens clients must present")
	noAuth := fs.Bool("no-auth", false, "allow clients without a token (testing only)")
	email := fs.String("email", env("UNIVERSAL_EMAIL", ""), "contact email for Let's Encrypt")
	certs := fs.String("certs", env("UNIVERSAL_CERTS", "./certs"), "certificate cache directory")
	httpsAddr := fs.String("https", ":443", "HTTPS listen address")
	httpAddr := fs.String("http", ":80", "HTTP listen address (ACME challenges and redirect)")
	insecure := fs.String("insecure-http", "", "serve plain HTTP only on this address, without TLS (local testing, or behind another TLS proxy)")
	publicPort := fs.String("public-port", "", "port to show in public URLs when not 443/80")
	custom := fs.Bool("allow-custom-domains", false, "let clients use their own hostnames (they CNAME them to this relay)")
	tcpPorts := fs.String("tcp-ports", env("UNIVERSAL_TCP_PORTS", ""), "public port range for raw TCP tunnels, e.g. 20000-20100 (open it in the firewall)")
	tcpBind := fs.String("tcp-bind", "", "interface to bind TCP tunnel ports on (default all)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	minPort, maxPort, err := parsePortRange(*tcpPorts)
	if err != nil {
		return err
	}
	if *domain == "" {
		return errors.New("--domain is required (e.g. --domain example.com)")
	}
	toks := splitList(*tokens)
	if len(toks) == 0 && !*noAuth {
		return errors.New("--token is required (or --no-auth for testing)")
	}
	r := &hosting.Relay{Domain: *domain, Tokens: toks, AllowCustomDomains: *custom, PublicPort: *publicPort,
		TCPPortMin: minPort, TCPPortMax: maxPort, TCPBind: *tcpBind, Log: log}
	if minPort > 0 {
		log.Info("TCP tunnels enabled", "ports", *tcpPorts)
	}
	h := hosting.AccessLog(log, r)
	if *insecure != "" {
		r.PublicScheme = "http"
		if r.PublicPort == "" {
			if _, p, err := net.SplitHostPort(*insecure); err == nil && p != "80" {
				r.PublicPort = p
			}
		}
		log.Info("relay running without TLS", "addr", *insecure, "domain", *domain)
		return hosting.ServePlain(ctx, h, *insecure)
	}
	return hosting.ServeAutoTLS(ctx, h, hosting.TLSOptions{
		CacheDir: *certs, Email: *email, HTTPAddr: *httpAddr, HTTPSAddr: *httpsAddr, Policy: r.HostPolicy,
	}, log)
}

// parsePortRange parses "20000-20100" or a single port; empty means none.
func parsePortRange(s string) (int, int, error) {
	if s == "" {
		return 0, 0, nil
	}
	lo, hi, found := strings.Cut(s, "-")
	if !found {
		hi = lo
	}
	a, err1 := strconv.Atoi(strings.TrimSpace(lo))
	b, err2 := strconv.Atoi(strings.TrimSpace(hi))
	if err1 != nil || err2 != nil || a < 1 || b > 65535 || a > b {
		return 0, 0, fmt.Errorf("invalid port range %q (want e.g. 20000-20100)", s)
	}
	return a, b, nil
}

// ---------------------------------------------------------------- expose

func runExpose(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("expose", flag.ContinueOnError)
	server := fs.String("server", env("UNIVERSAL_SERVER", ""), "relay URL, e.g. https://example.com")
	token := fs.String("token", env("UNIVERSAL_TOKEN", ""), "relay token")
	sub := fs.String("subdomain", env("UNIVERSAL_SUBDOMAIN", ""), "public name: <subdomain>.<relay domain> (random if empty)")
	hostname := fs.String("hostname", "", "full custom hostname (relay must allow custom domains)")
	spa := fs.Bool("spa", false, "when exposing a folder, serve index.html for unknown paths")
	tcp := fs.Bool("tcp", false, "forward raw TCP (databases, SSH, Redis, MQTT...) on a public port instead of HTTP")
	remotePort := fs.Int("remote-port", 0, "with --tcp: request this public port on the relay")
	allow := fs.String("allow", env("UNIVERSAL_ALLOW", ""), "with --tcp: only accept visitors from these IPs/CIDRs, e.g. 203.0.113.7,10.0.0.0/8")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: universal expose <port|host:port|http://url|tcp://host:port|./folder> [flags]")
		fs.PrintDefaults()
	}
	target, err := parse(fs, args)
	if err != nil {
		return err
	}
	if target == "" {
		fs.Usage()
		return errors.New("missing target")
	}
	if *server == "" {
		return errors.New("--server is required (or set UNIVERSAL_SERVER), e.g. --server https://example.com")
	}
	su, err := url.Parse(*server)
	if err != nil || (su.Scheme != "https" && su.Scheme != "http") || su.Host == "" {
		return fmt.Errorf("invalid --server %q", *server)
	}

	if strings.HasPrefix(target, "tcp://") {
		*tcp = true
	}
	allowNets, err := hosting.ParseAllowList(*allow)
	if err != nil {
		return err
	}
	if len(allowNets) > 0 && !*tcp {
		return errors.New("--allow only applies to --tcp tunnels")
	}

	var local string
	if st, statErr := os.Stat(target); statErr == nil && st.IsDir() {
		if *tcp {
			return errors.New("a folder cannot be exposed as a TCP tunnel")
		}
		h, err := hosting.StaticHandler(target, *spa)
		if err != nil {
			return err
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		go func() { _ = http.Serve(ln, hosting.AccessLog(log, h)) }()
		local = ln.Addr().String()
		abs, _ := filepath.Abs(target)
		log.Info("serving folder", "dir", abs, "local", local)
	} else if local, err = hosting.LocalTarget(target); err != nil {
		return err
	}

	c := &hosting.Client{
		Server: su, Token: *token, Subdomain: *sub, Hostname: *hostname, Target: local,
		TCP: *tcp, RemotePort: *remotePort, Allow: allowNets, Log: log,
		OnReady: func(u string) {
			fmt.Fprintf(os.Stderr, "\n  Online:  %s  →  %s\n", u, local)
			if *tcp {
				hp := strings.TrimPrefix(u, "tcp://")
				h, p, _ := net.SplitHostPort(hp)
				fmt.Fprintf(os.Stderr, "  Connect with host %s and port %s (e.g. psql -h %s -p %s, ssh -p %s user@%s)\n", h, p, h, p, p, h)
				if len(allowNets) == 0 {
					fmt.Fprintln(os.Stderr, "  Warning: anyone on the internet can reach this port. Use --allow to restrict it, and keep the service's own password on.")
				}
			}
			fmt.Fprintln(os.Stderr, "  Press Ctrl+C to stop.")
			fmt.Fprintln(os.Stderr)
		},
	}
	return c.Run(ctx)
}

// ---------------------------------------------------------------- portal

func runPortal(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("portal", flag.ContinueOnError)
	listen := fs.String("listen", env("UNIVERSAL_PORTAL_LISTEN", "127.0.0.1:7070"), "address for the portal UI")
	config := fs.String("config", env("UNIVERSAL_PORTAL_CONFIG", portal.DefaultConfigPath()), "where apps and relay settings are saved")
	token := fs.String("token", env("UNIVERSAL_PORTAL_TOKEN", ""), "require this token in the UI (needed to open the portal from other machines)")
	server := fs.String("server", env("UNIVERSAL_SERVER", ""), "relay URL to use if none is saved yet")
	relayToken := fs.String("relay-token", env("UNIVERSAL_TOKEN", ""), "relay token to use if none is saved yet")
	open := fs.Bool("open", false, "open the portal in the default browser")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		return fmt.Errorf("invalid --listen: %w", err)
	}
	if ip := net.ParseIP(host); (ip == nil || !ip.IsLoopback()) && host != "localhost" && *token == "" {
		return errors.New("the portal can publish ports on this machine; set --token to listen beyond localhost")
	}
	m, err := portal.Open(*config, log)
	if err != nil {
		return err
	}
	if st := m.State(); st.Settings.Server == "" && *server != "" {
		if err := m.UpdateSettings(*server, *relayToken, false); err != nil {
			return err
		}
	}
	m.StartEnabled()
	defer m.Shutdown()

	u := "http://" + *listen + "/"
	if host == "" || host == "0.0.0.0" || host == "::" {
		u = "http://127.0.0.1:" + strings.TrimPrefix(*listen, host+":") + "/"
	}
	if *token != "" {
		u += "?token=" + url.QueryEscape(*token)
	}
	fmt.Fprintf(os.Stderr, "\n  Universal Portal:  %s\n  Apps are saved in %s\n\n", u, *config)
	if *open {
		openBrowser(u)
	}
	return hosting.ServePlain(ctx, (&portal.Server{M: m, Token: *token}).Handler(), *listen)
}

func openBrowser(u string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	_ = cmd.Start()
}

// ---------------------------------------------------------------- serve

func runServe(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	dir := fs.String("dir", "", "folder to serve")
	proxy := fs.String("proxy", "", "local app to reverse-proxy, e.g. http://localhost:3000")
	spa := fs.Bool("spa", false, "serve index.html for unknown paths (single-page apps)")
	domains := fs.String("domain", env("UNIVERSAL_DOMAIN", ""), "comma-separated domains to get HTTPS certificates for (DNS must point here)")
	email := fs.String("email", env("UNIVERSAL_EMAIL", ""), "contact email for Let's Encrypt")
	certs := fs.String("certs", env("UNIVERSAL_CERTS", "./certs"), "certificate cache directory")
	httpsAddr := fs.String("https", ":443", "HTTPS listen address (with --domain)")
	httpAddr := fs.String("http", ":80", "HTTP listen address (with --domain)")
	plain := fs.String("listen", ":8080", "plain HTTP listen address (without --domain)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: universal serve <./folder|port|http://url> [--domain example.com] [flags]")
		fs.PrintDefaults()
	}
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if pos != "" && *dir == "" && *proxy == "" {
		if st, err := os.Stat(pos); err == nil && st.IsDir() {
			*dir = pos
		} else if t, err := hosting.LocalTarget(pos); err == nil {
			*proxy = "http://" + t
		} else {
			return err
		}
	}
	h, err := hosting.SiteHandler(*dir, *proxy, *spa)
	if err != nil {
		return err
	}
	h = hosting.AccessLog(log, h)
	hosts := splitList(*domains)
	if len(hosts) == 0 {
		log.Info("serving over plain HTTP; pass --domain for automatic HTTPS", "addr", *plain)
		return hosting.ServePlain(ctx, h, *plain)
	}
	allowed := map[string]bool{}
	for _, d := range hosts {
		allowed[strings.ToLower(d)] = true
	}
	policy := func(_ context.Context, host string) error {
		if allowed[strings.ToLower(host)] {
			return nil
		}
		return fmt.Errorf("host %q not configured", host)
	}
	for _, d := range hosts {
		fmt.Fprintf(os.Stderr, "  Online:  https://%s\n", d)
	}
	return hosting.ServeAutoTLS(ctx, h, hosting.TLSOptions{
		CacheDir: *certs, Email: *email, HTTPAddr: *httpAddr, HTTPSAddr: *httpsAddr, Policy: policy,
	}, log)
}
