# universal

One small Go binary with two jobs:

1. **AI Agent Monitor**: see everything your AI agents (Claude Code, Codex/ChatGPT,
   Gemini CLI, LangChain/CrewAI bots, local models) do: every model call, prompt,
   response, tool call, token count and error, in a live dashboard.
2. **One-command hosting**: put anything running on your computer on the internet:
   websites and APIs at `https://name.yourdomain.com`, and databases, SSH or any TCP
   service at `yourdomain.com:port`.

No database server, no cgo, a single dependency. Runs on Linux, macOS and Windows.
The full design is in [docs/PLAN.md](docs/PLAN.md).

## Quick start (no technical knowledge needed)

**New here? Read [docs/GETTING-STARTED.md](docs/GETTING-STARTED.md)**, a step-by-step guide with no jargon.

1. **Once, on any small cloud server:** run this. It needs no domain or DNS setup, and it
   prints a *connection code*.
   ```sh
   curl -fsSL https://raw.githubusercontent.com/meguptasushil/universal/main/deploy/install.sh | sudo sh -s -- relay
   ```
2. **On your computer**, install Universal:
   - Windows (PowerShell): `irm https://raw.githubusercontent.com/meguptasushil/universal/main/deploy/install.ps1 | iex`
   - Mac/Linux: `curl -fsSL https://raw.githubusercontent.com/meguptasushil/universal/main/deploy/install.sh | sh`
3. Open **Universal** (the Desktop shortcut, or type `universal`). Paste the connection code,
   then click **Publish** next to an app it found, or pick a project folder and Universal
   starts it for you.

The installers download ready-made programs from GitHub Releases. Push a tag such as `v0.1.0`
to publish them (`.github/workflows/release.yml`).

## Build from source

```sh
go install github.com/meguptasushil/universal/cmd/universal@latest   # Go 1.22+
make build            # → ./universal
make release          # cross-compile into dist/ for linux, macOS and windows
```

---

## 1. AI Agent Monitor

```sh
universal monitor                       # dashboard at http://127.0.0.1:8787
```

Agents call LLM APIs in a loop, so their API traffic *is* their activity log. The monitor
is a transparent gateway: point an agent's base URL at it, and every call is forwarded to
the real provider and recorded. Streaming still works token by token, and API keys pass
through but are never stored.

| Agent / SDK | Setting |
|---|---|
| Claude Code, Anthropic SDKs | `export ANTHROPIC_BASE_URL=http://127.0.0.1:8787/anthropic` |
| Codex CLI, OpenAI SDKs, most frameworks | `export OPENAI_BASE_URL=http://127.0.0.1:8787/openai/v1` |
| Gemini CLI, Google GenAI SDK (API key mode) | `export GOOGLE_GEMINI_BASE_URL=http://127.0.0.1:8787/gemini` |
| OpenRouter, Groq, Mistral, DeepSeek, xAI, Ollama | `http://127.0.0.1:8787/<name>/...` (built in) |
| Anything else | `--upstream azure=https://x.openai.azure.com/openai,openai` |

**Naming agents:** send an `X-Agent-Name: research-bot` header, or use the URL form
`http://127.0.0.1:8787/a/research-bot/openai/v1`. Otherwise the agent is identified from
its User-Agent (`claude-code`, `codex`, `gemini-cli`, `cursor`, `aider`, ...).

**What's recorded per call:** agent, provider, model, the latest user prompt, the response
text, the tool calls the model asked for (for example `Bash {"command":"rm -rf build"}`),
input and output tokens, latency, HTTP status and errors. This works for OpenAI Chat
Completions and Responses API, Anthropic Messages and Gemini `generateContent`, in both
JSON and streaming form.

### Tracking agents across IDs

Every event records *who* made it, so you can see when the same agent runs as
several instances, for several users, or with several API keys:

| Field | Where it comes from |
|---|---|
| `agent_id`: agent instance | `X-Agent-Id` header, or the subagent `agent_id` in Claude Code hooks |
| `user`: user or account | `X-Agent-User` header, Claude Code's account id (`metadata.user_id`), OpenAI `user` / `safety_identifier` |
| `key_id`: API key fingerprint | computed from the key the call used, e.g. `…Wxyz#3f9a1c2b` (the last 4 characters plus a hash; the key itself is never stored) |
| `session` | `X-Agent-Session` header, Claude Code session id, Codex conversation id (`prompt_cache_key`), hook `session_id` |

The `X-Agent-*` headers are stripped before the call reaches the provider. When an agent
shows up under more than one key, account or instance ID, the dashboard and the report
flag it.

### Reports

```sh
universal report                         # everything in ./monitor-data
universal report --since 24h             # last day (also 30m, 7d, 2026-10-01)
universal report --agent claude-code --session <id>
universal report --key '…Wxyz#3f9a1c2b'  # everything one API key did
universal report --watch 5s              # live, refreshing in the terminal
universal report --format csv --out activity.csv   # every event, for spreadsheets
universal report --format json           # for scripts
universal report --server https://monitor.example.com --token T   # a remote monitor
```

The report shows:
- totals and alerts (for example "codex ran under 2 API keys"),
- **agents**, with how many instance IDs, users, keys and sessions each one used,
- **identities**, one row per agent × instance × user × key combination,
- **sessions**, with duration and top tools,
- models, tools, and recent activity.

It reads the event log directly, so it also works when the monitor is stopped. If there
is no log, it asks a monitor running on `127.0.0.1:8787`. The same report is available
over HTTP at `/api/report?format=text|json|csv`.

### Hooks: see what the agent actually executes

The gateway shows what the model *asked for*. Hooks report what the agent *ran*. To use
them with Claude Code, merge [deploy/claude-code-hooks.json](deploy/claude-code-hooks.json)
into `~/.claude/settings.json`. Prompts and every tool use (`PreToolUse`/`PostToolUse`)
then show up in the dashboard with session id and working directory.

Any program can push events:

```sh
curl -X POST http://127.0.0.1:8787/api/ingest -H "Authorization: Bearer $TOKEN" \
  -d '{"agent":"my-bot","kind":"tool_use","tool_calls":[{"name":"send_email","input":"to: bob"}]}'
```

### API

| Endpoint | Purpose |
|---|---|
| `GET /api/events?agent=&agent_id=&user=&key_id=&session=&provider=&kind=&model=&q=&since=1h&limit=` | Search events, newest first |
| `GET /api/events/{id}` | One event |
| `GET /api/stats` | Totals per agent, identity, session, user, API key, provider, model and tool |
| `GET /api/report?format=text\|json\|csv&recent=N` | The `universal report` output (same filters) |
| `GET /api/stream` | Live Server-Sent Events feed (same filters) |
| `POST /api/ingest` | Push an event, a list of events, or a hook payload |

### Options and privacy

`--token T` protects the dashboard API and ingest. `--listen 0.0.0.0:8787` accepts agents from
other machines (use it with `--token`). `--no-content` records metadata only, never
prompt text. `--max-text` sets truncation. Events persist to `./monitor-data/events.jsonl`.
Only monitor agents and accounts you're responsible for, and tell people when their
usage is being recorded.

---

## 2. One-command hosting

### a) Your machine is behind a router (laptop, home or office PC): tunnel mode

**One-time setup on any cheap cloud VM with a public IP:**

```sh
universal relay                  # that's all: no domain, DNS or token needed
```

With no `--domain`, the relay detects its public IP and uses the free wildcard name
`<ip>.sslip.io`. With no `--token`, it generates one and keeps it in `--state`. It then
prints a **connection code** (`uni1.…`) that holds the address and token in one string.
Users paste the code into the portal, or run `universal login <code>`. `universal relay
--show-code` prints the code again. To run the relay as a service:
`sudo sh deploy/install.sh relay [domain] [20000-20100]`.

For production, use your own domain: point `A example.com` and `A *.example.com` at the VM,
then run `universal relay --domain example.com --email you@example.com --tcp-ports
20000-20100`. Open ports 80 and 443, plus the TCP range if you use one. HTTPS certificates
come from Let's Encrypt automatically, one per subdomain on its first visit. Free sslip.io
names share Let's Encrypt's rate limits with other users of that service, so prefer your
own domain for anything important.

**Then, from any machine, one command:**

```sh
universal login uni1.eyJz...                # once: saves the relay for expose and the portal

universal expose 3000 --subdomain myapp     # web app/API → https://myapp.example.com
universal expose ./public --subdomain www   # static site → https://www.example.com
universal expose 5432 --tcp                 # PostgreSQL  → tcp://example.com:20000
universal expose 3306 --tcp --remote-port 20010 --allow 203.0.113.7   # MySQL, fixed port, one IP
universal expose 22 --tcp --allow 10.0.0.0/8                          # SSH
```

The client connects *out* to the relay, so no port forwarding, static IP or router
configuration is needed. It reconnects automatically. HTTP tunnels support
keep-alive, streaming and WebSockets. TCP tunnels carry any protocol byte for byte
(PostgreSQL, MySQL, Redis, MongoDB, SSH, RDP, MQTT, game servers), including the
protocol's own TLS.

> **Exposing databases:** an open database port gets scanned within minutes. Always keep
> the database's own password or TLS on, and restrict visitors with `--allow`. The client
> enforces the allow-list on *your* machine, so the relay operator can't loosen it.

### Prefer a UI? The portal

```sh
universal                        # or: universal portal --open  (http://127.0.0.1:7070)
```

The portal is a local web page for the same job, made for people who don't use a terminal:
1. **Connect**: paste the relay's connection code once. If it fails, the error is explained
   in plain words, and nothing is saved.
2. **Running on this computer**: the portal finds web apps and databases that are already
   running, and you publish one with one click.
3. **Publish something**: choose *an app that's running* (a port), *a project folder*,
   *a website folder* or *a database or other service*. For a project folder, the portal
   detects how to start it and runs it for you, with logs:
   - Node.js (Vite, Next.js, npm start),
   - Python (Django, Flask, FastAPI, Streamlit),
   - Go,
   - Docker.

   **Browse…** picks folders without typing paths, and the page previews the public address
   before you publish.
4. **Settings**: switch relays, and choose whether to *start at login* (systemd or XDG
   autostart on Linux, a LaunchAgent on macOS, the Startup folder on Windows).

Each app shows whether it's online, its public URL (with a copy button), whether
anything is listening locally, how many connections it has had, and any error. You can
start, stop, edit or delete an app at any time. Apps and settings are saved in
`~/.universal/portal.json`, and apps that were online come back online when the portal
restarts.

The portal can open ports on your machine to the internet, so it only answers on
`localhost`, and it rejects requests that other websites send through your browser. To
open it from another machine, start it with `--listen 0.0.0.0:7070 --token SECRET`.

Custom domains: start the relay with `--allow-custom-domains`, CNAME `www.customer.com` to
the relay, then run `universal expose 3000 --hostname www.customer.com`.

### b) Your machine has a public IP (VPS, cloud VM): direct mode

```sh
universal serve ./public --domain mysite.com --email you@mysite.com      # static site, HTTPS
universal serve 3000 --domain api.mysite.com                             # reverse-proxy an app
universal serve ./public --spa                                           # plain HTTP on :8080
```

### Deploying with Docker or systemd

* `Dockerfile`: `docker build -t universal .`. Run commands are in the file header.
* `deploy/systemd/*.service` together with `deploy/install.sh relay|monitor`, which installs and enables them.

---

## Development

```sh
make test     # go vet + go test -race ./...
```

```
cmd/universal/       CLI (monitor, relay, expose, serve)
internal/monitor/    LLM gateway, provider parsers, event store, API, dashboard, hook adapter
internal/hosting/    tunnel protocol, relay, client, TCP tunnels, static/proxy serving, autocert
internal/portal/     web UI + API: connect wizard, app discovery, project runner, friendly errors
internal/autostart/  start the portal at login on Linux, macOS and Windows
deploy/              Dockerfile assets, systemd units, install script, Claude Code hooks
docs/PLAN.md         architecture and roadmap
```

## Long-term vision

The original goal for this project: a lightweight base layer that runs on Windows, Linux
or macOS, on top of which other operating systems and workloads can run, with
container-like processing and the ability to serve as a fully hosted operating system.
The monitor and hosting features are the first building blocks: observing what runs on
the machine, and publishing it to the internet.
