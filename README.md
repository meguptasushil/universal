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
| `GET /api/events?agent=&provider=&kind=&model=&q=&since=1h&limit=` | Search events, newest first |
| `GET /api/events/{id}` | One event |
| `GET /api/stats` | Totals per agent, provider, model and tool |
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
