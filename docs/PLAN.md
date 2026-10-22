# Universal: implementation plan

This plan covers two features, shipped as one Go module (`github.com/meguptasushil/universal`)
and one binary (`universal`):

1. **AI Agent Monitor**: see what AI agents (ChatGPT/OpenAI, Claude, Gemini, local
   models and others) are doing: which model they call, the prompts they send, the
   responses they get, the tools they run, tokens and latency.
2. **One-command hosting**: put anything running on a laptop, home PC or cloud VM on the
   internet with a single command: websites and APIs on a public domain with HTTPS, and
   any TCP service (PostgreSQL, MySQL, Redis, MongoDB, SSH, MQTT, game servers) on a
   public port.

---

## 0. Goals and non-goals

| Goals | Non-goals (for v1) |
|-------|--------------------|
| One static binary for Linux, macOS and Windows; `go install` or `go build` and it runs | Kubernetes operators, multi-region clustering |
| No database server, no cgo, a single dependency (`golang.org/x/crypto` for ACME) | Billing and user accounts |
| Works with the official CLIs and SDKs of OpenAI, Anthropic and Gemini, with no code changes in the agent | Breaking TLS on other people's traffic (no MITM CA) |
| Automatic HTTPS through Let's Encrypt | Wildcard certificates through DNS-01 (planned for v2) |
| Secrets (API keys) are never stored or displayed | |

---

## 1. AI Agent Monitor

### 1.1 How do we see what an agent is doing?

AI agents are programs that call an LLM API in a loop. Every action an agent takes
(reading a file, running a shell command, browsing) shows up in that loop as:

* a **request** to the model, with the conversation, the available tools and the model name, and
* a **response** from the model, with text and **tool calls** ("run `ls -la`", "edit file X").

So **the LLM API traffic is the agent's activity log.** We capture it in two complementary ways:

| Method | How it works | What it captures | Which agents it covers |
|--------|--------------|------------------|------------------------|
| **A. LLM gateway (proxy)** | The agent's API base URL points at `universal monitor`, which forwards every call to the real provider and records it | Model, prompt, response, tool calls, tokens, latency, errors | Any agent that lets you set a base URL: OpenAI SDK/Codex (`OPENAI_BASE_URL`), Claude Code/Anthropic SDK (`ANTHROPIC_BASE_URL`), Gemini CLI (`GOOGLE_GEMINI_BASE_URL`), LangChain, CrewAI, AutoGen, Ollama, OpenRouter, and so on |
| **B. Event ingest (hooks / SDK)** | The agent, or a hook it runs, POSTs JSON events to `/api/ingest` | Actions the agent runs locally: tool executions, sessions starting and stopping, custom events | Claude Code hooks (built-in adapter), your own agents, any framework callback |

Consumer web apps (chatgpt.com, gemini.google.com, claude.ai in a browser) cannot be
proxied without breaking TLS, so they are out of scope for v1. A browser extension that
posts to `/api/ingest` is planned for v2.

### 1.2 Architecture

```
  ┌──────────────┐   OPENAI_BASE_URL=http://monitor:8787/openai/v1     ┌──────────────────────┐
  │ Codex / app  │ ──────────────────────────────────────────────────▶ │                      │ ──▶ api.openai.com
  ├──────────────┤   ANTHROPIC_BASE_URL=http://monitor:8787/anthropic  │   universal monitor  │
  │ Claude Code  │ ──────────────────────────────────────────────────▶ │                      │ ──▶ api.anthropic.com
  ├──────────────┤   GOOGLE_GEMINI_BASE_URL=http://monitor:8787/gemini │  ┌── proxy ────────┐ │
  │ Gemini CLI   │ ──────────────────────────────────────────────────▶ │  │ tee req/resp    │ │ ──▶ generativelanguage.googleapis.com
  ├──────────────┤                                                     │  │ parse provider  │ │
  │ hooks / SDK  │ ──── POST /api/ingest ────────────────────────────▶ │  └──────┬──────────┘ │
  └──────────────┘                                                     │     ┌───▼────┐       │
                                                                       │     │ store  │ ring buffer + JSONL file
  browser ◀──── dashboard (/), REST (/api/events, /api/stats), SSE ─── │     └────────┘       │
                                                                       └──────────────────────┘
```

### 1.3 Components (`internal/monitor`)

| File | Responsibility |
|------|----------------|
| `event.go` | `Event` model: id, time, source, agent, provider, model, kind, status, latency, input/output tokens, prompt, response, tool calls, error, metadata |
| `providers.go` | Upstream registry (`openai`, `anthropic`, `gemini`, plus custom ones from `--upstream name=url`) and parsers for each wire format, both JSON and SSE streaming: OpenAI Chat Completions + Responses API, Anthropic Messages, Gemini `generateContent` / `streamGenerateContent` |
| `proxy.go` | `httputil.ReverseProxy` per upstream. Tees the request body and wraps the response body so the event is recorded when the stream ends. Streaming is not buffered: `FlushInterval=-1` keeps token-by-token output intact |
| `store.go` | Thread-safe ring buffer (default 10k events), append-only JSONL persistence, reload on start, filtered queries, stats aggregation (agents, identities, sessions, users, keys, models, tools), pub/sub for live streaming |
| `report.go` | Report model, alerts for agents running under multiple IDs, and text/JSON/CSV renderers shared by `universal report` and `/api/report` |
| `server.go` | HTTP routes: the proxy (`/{provider}/...`), `/api/events`, `/api/stats`, `/api/stream` (Server-Sent Events), `/api/ingest`, `/healthz`, and the dashboard (`/`) |
| `hooks.go` | Adapter that turns Claude Code hook payloads (`PreToolUse`, `PostToolUse`, `UserPromptSubmit`, `Stop`, ...) into events |
| `dashboard.html` | Embedded single-page UI with no external CDN: live feed, filters, per-agent, per-model and per-tool stats, an event detail panel |

### 1.4 Agent identification

The monitor picks the agent's name from the first of these that is set:

1. the `X-Agent-Name` header (or `agent` in ingest payloads),
2. the URL prefix form `/a/<agent>/<provider>/...`, so you can name an agent without adding headers,
3. a known `User-Agent` signature (`claude-cli`, `codex`, `GeminiCLI`, `OpenAI/Python`, `Anthropic/JS`, ...),
4. otherwise `unknown@<client-ip>`.

### 1.4.1 Identities: the same agent under different IDs

Beyond the agent *name*, every event carries four identity fields. Together they show
when one agent runs as several instances, for several people, or with several keys:

| Field | Sources (first match wins) |
|-------|----------------------------|
| `agent_id` (instance) | `X-Agent-Id` header; `agent_id` in hook payloads (Claude Code subagents) |
| `user` (user/account) | `X-Agent-User`; Anthropic `metadata.user_id` (Claude Code: `user_<hash>_account_<uuid>_session_<uuid>` or JSON); OpenAI `user` / `safety_identifier` |
| `key_id` (credential) | Fingerprint of the API key or OAuth token: last 4 characters + 8 hex chars of SHA-256. The key is never stored |
| `session` | `X-Agent-Session` / `X-Session-Id` / `session_id` headers; the Claude Code session in `metadata.user_id`; OpenAI `prompt_cache_key` (Codex conversation id); hook `session_id` |

The `X-Agent-*` headers are removed before forwarding. Aggregations add **agents** (with
distinct counts of instance IDs, users, keys and sessions), **identities** (agent ×
agent_id × user × key) and **sessions** (duration, top tools). An agent seen with more than
one key, user or instance ID raises an alert in the dashboard and the report.

### 1.4.2 Reports (`universal report`)

* Reads `events.jsonl` directly, so it is complete and works while the monitor is stopped.
  It can also query a running monitor (`--server`, `GET /api/report`). With no log present
  it falls back to `127.0.0.1:8787`.
* Filters: `--since` (30m / 24h / 7d / date), `--agent`, `--agent-id`, `--user`, `--key`,
  `--session`, `--provider`, `--model`, `--kind`, `--q`.
* Output: aligned terminal tables (alerts, agents, identities, sessions, models, tools,
  recent activity), `--format json` for scripts, and `--format csv` with one row per event
  for spreadsheets. `--watch 5s` refreshes it live, and `--out` writes it to a file.

### 1.5 Security and privacy

* Authorization headers, `x-api-key`, `x-goog-api-key` and `?key=` are forwarded
  upstream but **never stored**.
* `--token` protects the dashboard, the API and ingest (Bearer token or `?token=`).
  The proxy paths stay open so agents keep working, but it binds to `127.0.0.1` by default.
* Prompt and response text is truncated (default 4 KB, set with `--max-text`). `--no-content`
  records only metadata (model, tokens, tools) for privacy-sensitive setups.
* Monitoring other people's agent usage needs their consent. Document this.

### 1.6 CLI

```
universal monitor [--listen 127.0.0.1:8787] [--data ./data] [--token T]
                  [--upstream name=https://host] [--max-text 4096] [--no-content]
```

---

## 2. One-command hosting

### 2.1 Problem

A home or office machine usually sits behind NAT or CGNAT, has no public IP and no
open ports, and has no TLS certificate. We want:

```
universal expose 3000                 # → https://myapp.example.com
universal expose ./public             # serve a folder as a website
universal expose 5432 --tcp           # PostgreSQL → tcp://example.com:20001
universal expose 22 --tcp --allow 203.0.113.7   # SSH, only from one IP
universal serve --domain site.com --dir ./public   # on a VM with a public IP
```

### 2.2 Two modes

| Mode | When | How |
|------|------|-----|
| **Tunnel** (`relay` + `expose`) | The machine is behind NAT (laptop, home PC, office) | A small **relay** runs on any cheap cloud VM with a public IP and wildcard DNS. The local `expose` client dials *out* to it, so no port forwarding is needed. Visitors hit `https://<name>.<domain>`, the relay forwards to the client, and the client forwards to `localhost:<port>` |
| **Direct** (`serve`) | The machine already has a public IP (cloud VM, VPS) | Serves a folder or reverse-proxies a local app on :80/:443 with automatic Let's Encrypt certificates |

### 2.3 Tunnel architecture

```
 visitor ──https──▶ relay :443 (autocert)  ── route by Host header ──┐
                     │  control conn (HTTP Upgrade on /_universal/tunnel, over TLS)
                     │  ◀──────────────────────────────── expose client (outbound only)
                     │  "conn <id>"  ───────────────────▶ client dials /_universal/data?id=<id>
                     │  ◀═════ raw byte stream ═════════▶ client ◀══▶ localhost:3000
```

* **Transport**: all client to relay traffic is an HTTP `Upgrade` on the relay's normal
  HTTPS port. It needs only one port (443), passes through corporate firewalls, and reuses
  the relay's Let's Encrypt certificate, so the token is always encrypted.
* **Control channel**: newline-delimited JSON messages: `hello` (token, subdomain/hostname),
  `ok` (public URL), `error`, `conn` (open a stream), `ping`/`pong` (every 20 s, 60 s timeout).
* **Data streams**: one TCP connection per proxied connection, matched by a random id and
  spliced with `io.Copy`. This avoids a multiplexer dependency and keeps per-connection
  flow control from the OS.
* **HTTP frontend**: one `httputil.ReverseProxy` per tunnel. Its transport's
  `DialContext` opens a data stream, so keep-alive, WebSockets and streaming all work.
  It sets `X-Forwarded-For`, `X-Forwarded-Proto` and `X-Forwarded-Host`.
* **TLS**: `autocert.Manager` with a `HostPolicy` that allows the apex domain, any
  currently registered tunnel host, and custom domains when `--allow-custom-domains` is set.
  Certificates are cached on disk. `:80` serves ACME HTTP-01 challenges and redirects to HTTPS.
* **Auth**: one or more tokens set with `--token`/`UNIVERSAL_TOKENS`. A subdomain belongs to
  one live tunnel at a time. Without `--subdomain` the relay assigns a random name.
* **Resilience**: the client reconnects with exponential backoff (1 s to 30 s) and
  re-registers the same name.

### 2.3.1 TCP tunnels (databases, SSH, anything that is not HTTP)

Non-HTTP protocols carry no hostname the relay could route on, so a TCP tunnel gets its
own **public port** instead:

```
 psql -h example.com -p 20001 ──tcp──▶ relay :20001 (listener owned by the tunnel)
                                         │ "conn <id> remote=<visitor ip:port>"
                                         ▼
                                   expose client ── allow-list check ──▶ localhost:5432
```

* The relay owner enables a range with `--tcp-ports 20000-20100` and opens it in the firewall.
* The client sends `protocol: "tcp"` in `hello`, optionally with `remote_port` to ask for a
  fixed port (so connection strings stay stable). Otherwise the relay picks the first free port.
* For each accepted connection the relay opens a data stream exactly as for HTTP and
  splices raw bytes. Protocols such as PostgreSQL, MySQL, Redis, MongoDB, SSH, RDP,
  MQTT and Minecraft work unchanged, including their own TLS.
* **Security**: a database on the open internet gets scanned within minutes. The visitor
  address goes to the client in the `conn` message, and `--allow <ip,cidr,...>` drops
  everyone else *on the host machine*, so the relay operator cannot loosen it. The CLI
  warns when a TCP tunnel has no allow-list. The database's own authentication stays on.

### 2.4 Components (`internal/hosting`)

| File | Responsibility |
|------|----------------|
| `protocol.go` | Message types, JSON line codec, the HTTP-Upgrade handshake for client and server, name validation |
| `relay.go` | Relay server: tunnel registry, control and data handlers, Host routing, landing/status page, autocert wiring |
| `client.go` | `expose` client: connect, register, heartbeat, open a data stream per request, splice it to the local target, reconnect |
| `serve.go` | Direct mode: static file server (SPA fallback, security headers) or reverse proxy, with autocert or plain HTTP |
| `tls.go` | Shared `autocert` setup: cache dir, ACME email, HTTP-01 handler, redirect |

### 2.5 CLI

```
universal relay  --domain example.com --token SECRET [--email you@x.com]
                 [--https :443 --http :80] [--insecure-http :8080] [--allow-custom-domains]
                 [--tcp-ports 20000-20100]
universal expose <port|host:port|url|tcp://host:port|./dir> [--server https://example.com] [--token SECRET]
                 [--subdomain myapp] [--hostname www.custom.com]
                 [--tcp] [--remote-port 20001] [--allow 203.0.113.7,10.0.0.0/8]
universal serve  (--dir ./public | --proxy http://localhost:3000)
                 [--domain site.com --email you@x.com] [--http :8080]
```

`UNIVERSAL_SERVER` and `UNIVERSAL_TOKEN` env vars shorten `expose` to `universal expose 3000`.

### 2.6 Customer setup (one time, about 5 minutes)

1. Get a VM with a public IP (any cloud provider, for example a $5 VPS).
2. DNS: `A example.com → VM_IP` and `A *.example.com → VM_IP`. Open ports 80 and 443
   (plus the TCP range if you want database/SSH tunnels).
3. On the VM: `curl -fsSL .../deploy/install.sh | sh -s -- relay example.com` (installs the
   binary and a systemd unit), or `docker run` with the provided Dockerfile.
4. On any machine: `universal expose 3000 --server https://example.com --token ...`.

---

## 3. Cross-cutting

* **Layout**: `cmd/universal` (CLI), `internal/monitor`, `internal/hosting`, `deploy/`
  (Dockerfile, systemd units, install script), `docs/`.
* **Config**: flags first, with environment variables (`UNIVERSAL_*`) as fallbacks.
* **Logging**: `log/slog` text logs.
* **Graceful shutdown**: SIGINT/SIGTERM triggers `http.Server.Shutdown`.
* **Testing**:
  * monitor: parser unit tests for every provider format (JSON and SSE); an end-to-end
    test with a fake upstream that checks streaming passthrough, that events are
    recorded, and that keys are redacted; a Claude hook adapter test.
  * hosting: an end-to-end test with relay, client and a local backend on loopback (plain
    HTTP mode), covering host routing, unknown host returning 404, a bad token being
    rejected, duplicate subdomains being rejected, WebSocket upgrades, and a static `serve` test.
  * TCP tunnels: concurrent sessions through a public port, a requested port, allow-list
    blocking, TCP disabled on the relay, port out of range, and port pool exhausted.
* **Build/release**: `go build ./cmd/universal`; cross-compile with `GOOS`/`GOARCH`
  (`make release`).

## 4. Milestones

| # | Deliverable | Status |
|---|-------------|--------|
| M1 | Module skeleton, CLI dispatcher | in this change |
| M2 | Monitor: proxy, parsers, store, API, dashboard, hook ingest, tests | in this change |
| M2b | Identity tracking (agent ID, user, API key fingerprint, session), multi-ID alerts, `universal report` CLI | in this change |
| M3 | Hosting: relay, expose client, direct serve, autocert, tests | in this change |
| M3b | TCP tunnels for databases/SSH/any TCP service, with an IP allow-list | in this change |
| M4 | Deploy assets (Dockerfile, systemd, install script), README | in this change |
| M5 (v2) | Browser extension for web chat apps, process/OS-level agent watcher (exec and file events through eBPF/ETW), alert rules (for example "agent ran `rm -rf`"), SQLite backend, multi-user auth | future |
| M6 (v2) | Wildcard certs through DNS-01, UDP tunnels, per-tunnel basic auth and IP allow-lists for HTTP, usage dashboard for the relay | future |
