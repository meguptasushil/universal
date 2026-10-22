package monitor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"
)

const (
	maxRequestCapture  = 32 << 20 // requests bigger than this are forwarded but not parsed
	maxResponseCapture = 16 << 20
)

// Gateway forwards agent traffic to model providers and records each call.
type Gateway struct {
	Store     *Store
	Upstreams map[string]*Upstream
	MaxText   int  // truncate prompt/response text to this many bytes
	NoContent bool // record metadata only
	Log       *slog.Logger

	once    sync.Once
	proxies map[string]*httputil.ReverseProxy
}

type callCtxKey struct{}

// callCtx carries what we know about an in-flight call.
type callCtx struct {
	start    time.Time
	upstream *Upstream
	format   string
	agent    string
	path     string
	method   string
	clientIP string
	agentID  string
	user     string
	session  string
	keyID    string
	req      reqInfo
	done     sync.Once
}

func (g *Gateway) init() {
	g.once.Do(func() {
		if g.Log == nil {
			g.Log = slog.Default()
		}
		g.proxies = map[string]*httputil.ReverseProxy{}
		for name, up := range g.Upstreams {
			g.proxies[name] = g.newProxy(up)
		}
	})
}

func (g *Gateway) newProxy(up *Upstream) *httputil.ReverseProxy {
	target := up.Target
	return &httputil.ReverseProxy{
		FlushInterval: -1, // stream tokens to the agent as they arrive
		Rewrite: func(pr *httputil.ProxyRequest) {
			cc := pr.In.Context().Value(callCtxKey{}).(*callCtx)
			out := pr.Out
			out.URL.Scheme = target.Scheme
			out.URL.Host = target.Host
			out.URL.Path = strings.TrimSuffix(target.Path, "/") + cc.path
			out.URL.RawPath = ""
			out.Host = target.Host
			for _, h := range agentHeaders {
				out.Header.Del(h)
			}
			// let the transport negotiate gzip and decompress it for us, so the
			// captured body is readable
			out.Header.Del("Accept-Encoding")
		},
		ModifyResponse: func(resp *http.Response) error {
			cc := resp.Request.Context().Value(callCtxKey{}).(*callCtx)
			resp.Body = &recordingBody{rc: resp.Body, onDone: func(body []byte, truncated bool, readErr error) {
				g.finish(cc, resp.StatusCode, resp.Header.Get("Content-Type"), body, truncated, readErr)
			}}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			cc := r.Context().Value(callCtxKey{}).(*callCtx)
			g.finish(cc, http.StatusBadGateway, "", nil, false, err)
			if !errors.Is(err, context.Canceled) {
				http.Error(w, "monitor: upstream error: "+err.Error(), http.StatusBadGateway)
			}
		},
	}
}

// ServeProxy forwards r (whose path, minus the routing prefix, is path) to
// the named upstream.
func (g *Gateway) ServeProxy(w http.ResponseWriter, r *http.Request, upstream, agent, path string) {
	g.init()
	up, ok := g.Upstreams[upstream]
	if !ok {
		http.Error(w, "monitor: unknown provider "+upstream, http.StatusNotFound)
		return
	}
	if path == "" {
		path = "/"
	}
	cc := &callCtx{
		start:    time.Now(),
		upstream: up,
		format:   detectFormat(up.Format, path),
		agent:    agent,
		path:     path,
		method:   r.Method,
		clientIP: clientIP(r),
	}
	if cc.agent == "" {
		cc.agent = identifyAgent(r, cc.clientIP)
	}
	cc.agentID = r.Header.Get("X-Agent-Id")
	cc.user = r.Header.Get("X-Agent-User")
	cc.session = firstNonEmpty(r.Header.Get("X-Agent-Session"), r.Header.Get("X-Session-Id"),
		r.Header.Get("Session_id"), r.Header.Get("X-Claude-Code-Session-Id"))
	cc.keyID = keyFingerprint(r)
	if r.Body != nil && r.ContentLength <= maxRequestCapture {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestCapture+1))
		r.Body.Close()
		if err != nil {
			http.Error(w, "monitor: reading request: "+err.Error(), http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		if len(body) <= maxRequestCapture {
			cc.req = parseRequest(cc.format, path, body)
		}
	}
	r = r.WithContext(context.WithValue(r.Context(), callCtxKey{}, cc))
	g.proxies[upstream].ServeHTTP(w, r)
}

func (g *Gateway) finish(cc *callCtx, status int, contentType string, body []byte, truncated bool, callErr error) {
	cc.done.Do(func() {
		ri := respInfo{}
		if len(body) > 0 {
			ri = parseResponse(cc.format, contentType, body)
		}
		e := Event{
			Source:       SourceProxy,
			Kind:         KindLLMCall,
			Agent:        cc.agent,
			AgentID:      cc.agentID,
			User:         firstNonEmpty(cc.user, cc.req.User),
			KeyID:        cc.keyID,
			Session:      firstNonEmpty(cc.session, cc.req.Session),
			Provider:     cc.upstream.Name,
			Model:        firstNonEmpty(ri.Model, cc.req.Model),
			Method:       cc.method,
			Path:         cc.path,
			Status:       status,
			LatencyMs:    time.Since(cc.start).Milliseconds(),
			Stream:       cc.req.Stream,
			InputTokens:  ri.InputTokens,
			OutputTokens: ri.OutputTokens,
			Prompt:       truncate(cc.req.Prompt, g.MaxText),
			Response:     truncate(ri.Text, g.MaxText),
			ToolsOffered: cc.req.ToolsOffered,
			ToolCalls:    ri.ToolCalls,
			Error:        ri.Error,
			ClientIP:     cc.clientIP,
		}
		for i := range e.ToolCalls {
			e.ToolCalls[i].Input = truncate(e.ToolCalls[i].Input, g.MaxText)
		}
		if callErr != nil && !errors.Is(callErr, io.EOF) {
			e.Error = firstNonEmpty(e.Error, callErr.Error())
		}
		if e.Error == "" && status >= 400 {
			e.Error = http.StatusText(status)
		}
		if truncated {
			e.Meta = map[string]any{"response_capture_truncated": true}
		}
		if g.NoContent {
			e.Prompt, e.Response = "", ""
			for i := range e.ToolCalls {
				e.ToolCalls[i].Input = ""
			}
		}
		g.Store.Add(e)
		g.Log.Info("llm call", "agent", e.Agent, "provider", e.Provider, "model", e.Model,
			"status", e.Status, "ms", e.LatencyMs, "in", e.InputTokens, "out", e.OutputTokens, "tools", len(e.ToolCalls))
	})
}

// recordingBody copies what the agent reads into a buffer and reports it
// once the stream ends.
type recordingBody struct {
	rc        io.ReadCloser
	buf       bytes.Buffer
	truncated bool
	once      sync.Once
	onDone    func(body []byte, truncated bool, err error)
}

func (b *recordingBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		if b.buf.Len()+n <= maxResponseCapture {
			b.buf.Write(p[:n])
		} else {
			b.truncated = true
		}
	}
	if err != nil {
		b.report(err)
	}
	return n, err
}

func (b *recordingBody) Close() error {
	b.report(nil)
	return b.rc.Close()
}

func (b *recordingBody) report(err error) {
	b.once.Do(func() { b.onDone(b.buf.Bytes(), b.truncated, err) })
}

// agentHeaders are identity hints for the monitor; they are not forwarded.
var agentHeaders = []string{"X-Agent-Name", "X-Agent-Id", "X-Agent-User", "X-Agent-Session"}

// keyFingerprint identifies which API key (or OAuth token) a call used
// without storing it: the last 4 characters, as providers display them, plus
// a short hash so keys sharing those characters stay distinct.
func keyFingerprint(r *http.Request) string {
	k := ""
	if h := r.Header.Get("Authorization"); h != "" {
		if _, tok, ok := strings.Cut(h, " "); ok {
			k = strings.TrimSpace(tok)
		} else {
			k = strings.TrimSpace(h)
		}
	}
	for _, h := range []string{"X-Api-Key", "X-Goog-Api-Key", "Api-Key"} {
		if v := r.Header.Get(h); v != "" {
			k = v
		}
	}
	if k == "" {
		k = r.URL.Query().Get("key")
	}
	if k == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(k))
	tag := hex.EncodeToString(sum[:4])
	if len(k) >= 16 {
		return "…" + k[len(k)-4:] + "#" + tag
	}
	return "#" + tag
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// agentSignatures maps User-Agent substrings (lowercase) to agent names.
var agentSignatures = []struct{ needle, name string }{
	{"claude-cli", "claude-code"},
	{"claude-code", "claude-code"},
	{"codex", "codex"},
	{"geminicli", "gemini-cli"},
	{"gemini-cli", "gemini-cli"},
	{"cursor", "cursor"},
	{"aider", "aider"},
	{"cline", "cline"},
	{"continue", "continue"},
	{"langchain", "langchain"},
	{"litellm", "litellm"},
	{"crewai", "crewai"},
	{"autogen", "autogen"},
	{"openai/python", "openai-python-sdk"},
	{"openai/js", "openai-node-sdk"},
	{"anthropic/python", "anthropic-python-sdk"},
	{"anthropic/js", "anthropic-node-sdk"},
	{"google-genai", "google-genai-sdk"},
	{"goog-api-client", "google-sdk"},
	{"curl/", "curl"},
}

func identifyAgent(r *http.Request, ip string) string {
	if a := strings.TrimSpace(r.Header.Get("X-Agent-Name")); a != "" {
		return a
	}
	ua := strings.ToLower(r.Header.Get("User-Agent") + " " + r.Header.Get("X-Goog-Api-Client"))
	for _, s := range agentSignatures {
		if strings.Contains(ua, s.needle) {
			return s.name
		}
	}
	return "unknown@" + ip
}
