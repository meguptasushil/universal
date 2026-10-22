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

## Install

```sh
go install github.com/meguptasushil/universal/cmd/universal@latest
# or, from a checkout
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

1. DNS: `A  example.com → VM_IP` and `A  *.example.com → VM_IP`
2. Open ports 80 and 443, plus a port range for TCP tunnels if you want them.
3. Start the relay:

```sh
sudo sh deploy/install.sh relay example.com MY_SECRET you@example.com 20000-20100
# or by hand / docker:
universal relay --domain example.com --token MY_SECRET --email you@example.com --tcp-ports 20000-20100
```

HTTPS certificates come from Let's Encrypt automatically, one per subdomain on its first visit.

**Then, from any machine, one command:**

```sh
export UNIVERSAL_SERVER=https://example.com UNIVERSAL_TOKEN=MY_SECRET

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
deploy/              Dockerfile assets, systemd units, install script, Claude Code hooks
docs/PLAN.md         architecture and roadmap
```

## Long-term vision

The original goal for this project: a lightweight base layer that runs on Windows, Linux
or macOS, on top of which other operating systems and workloads can run, with
container-like processing and the ability to serve as a fully hosted operating system.
The monitor and hosting features are the first building blocks: observing what runs on
the machine, and publishing it to the internet.
