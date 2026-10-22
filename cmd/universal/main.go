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

	"github.com/meguptasushil/universal/internal/autostart"
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
  universal portal                          web UI to publish local apps (also: run universal with no arguments)
  universal login   <connection code>       save the relay connection for portal and expose
  universal version

Run "universal <command> -h" for the flags of a command.
`

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// No arguments (for example, double-clicked): open the portal.
	cmd, args := "portal", []string{"--open"}
	if len(os.Args) >= 2 {
		cmd, args = os.Args[1], os.Args[2:]
	}
	var err error
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
	case "login":
		err = runLogin(ctx, args)
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
	domain := fs.String("domain", env("UNIVERSAL_DOMAIN", ""), "apex domain; point example.com and *.example.com at this machine (default: a free <ip>.sslip.io name, no DNS setup needed)")
	tokens := fs.String("token", env("UNIVERSAL_TOKENS", ""), "comma-separated tokens clients must present (default: generate one and keep it in the state directory)")
	state := fs.String("state", env("UNIVERSAL_STATE", "./universal-relay"), "directory for the relay's token and certificates")
	noAuth := fs.Bool("no-auth", false, "allow clients without a token (testing only)")
	email := fs.String("email", env("UNIVERSAL_EMAIL", ""), "contact email for Let's Encrypt")
	certs := fs.String("certs", env("UNIVERSAL_CERTS", ""), "certificate cache directory (default: <state>/certs)")
	httpsAddr := fs.String("https", ":443", "HTTPS listen address")
	httpAddr := fs.String("http", ":80", "HTTP listen address (ACME challenges and redirect)")
	insecure := fs.String("insecure-http", "", "serve plain HTTP only on this address, without TLS (local testing, or behind another TLS proxy)")
	publicPort := fs.String("public-port", "", "port to show in public URLs when not 443/80")
	custom := fs.Bool("allow-custom-domains", false, "let clients use their own hostnames (they CNAME them to this relay)")
	tcpPorts := fs.String("tcp-ports", env("UNIVERSAL_TCP_PORTS", ""), "public port range for raw TCP tunnels, e.g. 20000-20100 (open it in the firewall)")
	tcpBind := fs.String("tcp-bind", "", "interface to bind TCP tunnel ports on (default all)")
	showCode := fs.Bool("show-code", false, "print the connection code for this relay and exit")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	minPort, maxPort, err := parsePortRange(*tcpPorts)
	if err != nil {
		return err
	}
	if *domain == "" {
		ip, err := hosting.DetectPublicIP(ctx)
		if err != nil {
			return err
		}
		*domain = hosting.IPDomain(ip)
		log.Info("no --domain given; using a free wildcard DNS name for this server's IP", "domain", *domain)
	}
	toks := splitList(*tokens)
	if len(toks) == 0 && !*noAuth {
		tok, created, err := hosting.LoadOrCreateToken(filepath.Join(*state, "relay-token"))
		if err != nil {
			return err
		}
		if created {
			log.Info("generated a relay token", "file", filepath.Join(*state, "relay-token"))
		}
		toks = []string{tok}
	}
	if *certs == "" {
		*certs = filepath.Join(*state, "certs")
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
	} else if r.PublicPort == "" {
		if _, p, err := net.SplitHostPort(*httpsAddr); err == nil && p != "443" {
			r.PublicPort = p
		}
	}
	if *showCode {
		if len(toks) == 0 {
			return errors.New("this relay has no token (--no-auth), so there is no connection code")
		}
		fmt.Println(hosting.EncodeConnectCode(relayURL(r), toks[0]))
		return nil
	}
	printRelayBanner(r, toks, minPort, maxPort)
	if *insecure != "" {
		log.Info("relay running without TLS", "addr", *insecure, "domain", *domain)
		return hosting.ServePlain(ctx, h, *insecure)
	}
	return hosting.ServeAutoTLS(ctx, h, hosting.TLSOptions{
		CacheDir: *certs, Email: *email, HTTPAddr: *httpAddr, HTTPSAddr: *httpsAddr, Policy: r.HostPolicy,
	}, log)
}

// printRelayBanner tells the operator exactly what to hand to users.
func relayURL(r *hosting.Relay) string {
	scheme := r.PublicScheme
	if scheme == "" {
		scheme = "https"
	}
	host := r.Domain
	if r.PublicPort != "" {
		host = net.JoinHostPort(host, r.PublicPort)
	}
	return scheme + "://" + host
}

func printRelayBanner(r *hosting.Relay, tokens []string, minPort, maxPort int) {
	server := relayURL(r)
	fmt.Fprintf(os.Stderr, "\n  Relay is running at %s\n", server)
	if strings.HasSuffix(r.Domain, ".sslip.io") {
		fmt.Fprintln(os.Stderr, "  (free sslip.io address, no DNS setup needed; use --domain with your own domain for production)")
	} else {
		fmt.Fprintf(os.Stderr, "  DNS must point %s and *.%s at this server.\n", r.Domain, r.Domain)
	}
	fmt.Fprintf(os.Stderr, "  Firewall: open ports 80 and 443")
	if minPort > 0 {
		fmt.Fprintf(os.Stderr, ", and %d-%d for databases/TCP", minPort, maxPort)
	}
	fmt.Fprintln(os.Stderr, ".")
	if len(tokens) > 0 {
		fmt.Fprintf(os.Stderr, "\n  Connection code (give it to anyone who should publish apps here):\n\n    %s\n\n", hosting.EncodeConnectCode(server, tokens[0]))
		fmt.Fprintln(os.Stderr, "  They paste it into the Universal portal, or run:  universal login <code>")
	}
	fmt.Fprintln(os.Stderr)
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
	server := fs.String("server", env("UNIVERSAL_SERVER", ""), "relay URL, e.g. https://example.com (default: the one saved by universal login)")
	token := fs.String("token", env("UNIVERSAL_TOKEN", ""), "relay token (default: the one saved by universal login)")
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
		if m, err := portal.Open(portal.DefaultConfigPath(), log); err == nil {
			if saved := m.Settings(); saved.Server != "" {
				*server = saved.Server
				if *token == "" {
					*token = saved.Token
				}
			}
		}
	}
	if *server == "" {
		return errors.New("no relay configured: run \"universal login <connection code>\" first, or pass --server and --token")
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
	u := "http://" + *listen + "/"
	if host == "" || host == "0.0.0.0" || host == "::" {
		u = "http://127.0.0.1:" + strings.TrimPrefix(*listen, host+":") + "/"
	}
	if *token != "" {
		u += "?token=" + url.QueryEscape(*token)
	}
	if portalRunning(u, *token) { // already open (double-clicked twice): just show it
		fmt.Fprintf(os.Stderr, "Universal Portal is already running: %s\n", u)
		if *open {
			openBrowser(u)
		}
		return nil
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

	fmt.Fprintf(os.Stderr, "\n  Universal Portal:  %s\n  Apps are saved in %s\n  Keep this window open while your apps are online.\n\n", u, *config)
	if *open {
		openBrowser(u)
	}
	srv := &portal.Server{M: m, Token: *token}
	if _, p, err := net.SplitHostPort(*listen); err == nil {
		n, _ := strconv.Atoi(p)
		srv.SkipPorts = []int{n}
	}
	if exe, err := os.Executable(); err == nil {
		srv.Autostart = &autostart.Entry{Exe: exe, Args: []string{"portal", "--listen", *listen, "--config", *config}}
	}
	return hosting.ServePlain(ctx, srv.Handler(), *listen)
}

func portalRunning(u, token string) bool {
	req, _ := http.NewRequest("GET", strings.Split(u, "?")[0]+"api/state", nil)
	req.Header.Set("X-Portal", "1")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: time.Second}).Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// ---------------------------------------------------------------- login

func runLogin(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	config := fs.String("config", env("UNIVERSAL_PORTAL_CONFIG", portal.DefaultConfigPath()), "where the connection is saved")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: universal login <connection code>   (the relay prints the code when it starts)")
		fs.PrintDefaults()
	}
	code, err := parse(fs, args)
	if err != nil {
		return err
	}
	if code == "" {
		fs.Usage()
		return errors.New("missing connection code")
	}
	m, err := portal.Open(*config, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		return err
	}
	if err := m.Connect(code); err != nil {
		return err
	}
	fmt.Printf("Saved relay %s\n", m.Settings().Server)
	if err := m.TestRelay(ctx); err != nil {
		return fmt.Errorf("saved, but the relay could not be reached: %w", err)
	}
	fmt.Println("Connected. Publish an app with \"universal expose 3000\" or open the portal with \"universal\".")
	return nil
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
