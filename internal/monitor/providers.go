package monitor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// Wire formats understood by the parsers.
const (
	FormatOpenAI    = "openai"
	FormatAnthropic = "anthropic"
	FormatGemini    = "gemini"
	FormatAuto      = "auto"
)

// Upstream is a model provider the gateway forwards to.
type Upstream struct {
	Name   string
	Target *url.URL
	Format string
}

// DefaultUpstreams returns the built-in providers.
func DefaultUpstreams() map[string]*Upstream {
	mk := func(name, raw, format string) *Upstream {
		u, _ := url.Parse(raw)
		return &Upstream{Name: name, Target: u, Format: format}
	}
	return map[string]*Upstream{
		"openai":     mk("openai", "https://api.openai.com", FormatOpenAI),
		"anthropic":  mk("anthropic", "https://api.anthropic.com", FormatAnthropic),
		"gemini":     mk("gemini", "https://generativelanguage.googleapis.com", FormatGemini),
		"openrouter": mk("openrouter", "https://openrouter.ai/api", FormatOpenAI),
		"groq":       mk("groq", "https://api.groq.com/openai", FormatOpenAI),
		"mistral":    mk("mistral", "https://api.mistral.ai", FormatOpenAI),
		"deepseek":   mk("deepseek", "https://api.deepseek.com", FormatOpenAI),
		"xai":        mk("xai", "https://api.x.ai", FormatOpenAI),
		"ollama":     mk("ollama", "http://127.0.0.1:11434", FormatAuto),
	}
}

// ParseUpstream parses "name=https://host[/base][,format]".
func ParseUpstream(spec string) (*Upstream, error) {
	name, rest, ok := strings.Cut(spec, "=")
	if !ok || name == "" || rest == "" {
		return nil, fmt.Errorf("upstream %q: want name=url[,format]", spec)
	}
	raw, format, _ := strings.Cut(rest, ",")
	if format == "" {
		format = FormatAuto
	}
	switch format {
	case FormatOpenAI, FormatAnthropic, FormatGemini, FormatAuto:
	default:
		return nil, fmt.Errorf("upstream %q: unknown format %q", spec, format)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("upstream %q: invalid url", spec)
	}
	return &Upstream{Name: name, Target: u, Format: format}, nil
}

// detectFormat picks a wire format from the request path for "auto" upstreams.
func detectFormat(format, path string) string {
	if format != FormatAuto && format != "" {
		return format
	}
	switch {
	case strings.Contains(path, ":generateContent"), strings.Contains(path, ":streamGenerateContent"):
		return FormatGemini
	case strings.HasSuffix(path, "/messages"), strings.HasSuffix(path, "/messages/count_tokens"):
		return FormatAnthropic
	default:
		return FormatOpenAI
	}
}

// reqInfo is what we extract from a model request.
type reqInfo struct {
	Model        string
	Stream       bool
	Prompt       string
	ToolsOffered int
	User         string
	Session      string
}

// respInfo is what we extract from a model response.
type respInfo struct {
	Model        string
	Text         string
	ToolCalls    []ToolCall
	InputTokens  int
	OutputTokens int
	Error        string
}

type obj = map[string]any

func str(v any) string {
	s, _ := v.(string)
	return s
}

func num(v any) int {
	f, _ := v.(float64)
	return int(f)
}

func m(v any) obj {
	o, _ := v.(obj)
	return o
}

func arr(v any) []any {
	a, _ := v.([]any)
	return a
}

func compact(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// textOf flattens the common "content" shapes: a string, or a list of parts
// with "text" fields (OpenAI, Anthropic, Responses API, Gemini parts).
func textOf(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, p := range c {
			po := m(p)
			switch {
			case po == nil:
				if s, ok := p.(string); ok {
					parts = append(parts, s)
				}
			case str(po["text"]) != "":
				parts = append(parts, str(po["text"]))
			case po["type"] == "tool_result":
				parts = append(parts, "[tool_result] "+textOf(po["content"]))
			case po["functionResponse"] != nil:
				parts = append(parts, "[function_response] "+compact(po["functionResponse"]))
			case po["type"] == "function_call_output":
				parts = append(parts, "[function_call_output] "+compact(po["output"]))
			case po["type"] == "image" || po["type"] == "image_url" || po["type"] == "input_image" || po["inlineData"] != nil:
				parts = append(parts, "[image]")
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// lastUser returns the text of the last user-authored message in a list.
func lastUser(msgs []any, roleKey, contentKey string) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		mo := m(msgs[i])
		if mo == nil {
			continue
		}
		role := str(mo[roleKey])
		if role == "user" || (role == "" && contentKey == "parts") {
			return textOf(mo[contentKey])
		}
	}
	return ""
}

func parseRequest(format, path string, body []byte) reqInfo {
	var ri reqInfo
	var o obj
	if json.Unmarshal(body, &o) != nil {
		return ri
	}
	switch format {
	case FormatAnthropic:
		ri.Model, ri.Stream = str(o["model"]), o["stream"] == true
		ri.User, ri.Session = parseAnthropicUserID(str(m(o["metadata"])["user_id"]))
		ri.Prompt = lastUser(arr(o["messages"]), "role", "content")
		ri.ToolsOffered = len(arr(o["tools"]))
	case FormatGemini:
		if inner := m(o["request"]); inner != nil { // Code Assist wrapper
			ri.Model = str(o["model"])
			o = inner
		}
		ri.Model = firstNonEmpty(geminiModel(path), ri.Model)
		ri.Stream = strings.Contains(path, ":streamGenerateContent")
		ri.Prompt = lastUser(arr(o["contents"]), "role", "parts")
		for _, t := range arr(o["tools"]) {
			ri.ToolsOffered += len(arr(m(t)["functionDeclarations"]))
			if n := len(arr(m(t)["function_declarations"])); n > 0 {
				ri.ToolsOffered += n
			}
		}
	default: // OpenAI-compatible
		ri.Model, ri.Stream = str(o["model"]), o["stream"] == true
		ri.User = firstNonEmpty(str(o["user"]), str(o["safety_identifier"]))
		ri.Session = str(o["prompt_cache_key"]) // Codex sends its conversation id here
		ri.ToolsOffered = len(arr(o["tools"])) + len(arr(o["functions"]))
		switch {
		case o["messages"] != nil:
			ri.Prompt = lastUser(arr(o["messages"]), "role", "content")
		case o["input"] != nil: // Responses API / embeddings
			if s, ok := o["input"].(string); ok {
				ri.Prompt = s
			} else {
				items := arr(o["input"])
				ri.Prompt = lastUser(items, "role", "content")
				if ri.Prompt == "" && len(items) > 0 {
					ri.Prompt = textOf(items[len(items)-1:])
				}
			}
		case o["prompt"] != nil:
			ri.Prompt = textOf(o["prompt"])
		}
	}
	return ri
}

// parseAnthropicUserID understands Claude Code's metadata.user_id, either
// "user_<hash>_account_<uuid>_session_<uuid>" or a JSON object, and falls back
// to treating the whole value as the user.
func parseAnthropicUserID(id string) (user, session string) {
	if id == "" {
		return "", ""
	}
	if strings.HasPrefix(id, "{") {
		var o obj
		if json.Unmarshal([]byte(id), &o) == nil {
			user = firstNonEmpty(str(o["account_uuid"]), str(o["user_id"]), str(o["device_id"]))
			if user != "" && str(o["account_uuid"]) != "" {
				user = "account_" + user
			}
			return user, str(o["session_id"])
		}
	}
	if i := strings.Index(id, "_session_"); i >= 0 {
		id, session = id[:i], id[i+len("_session_"):]
	}
	if i := strings.Index(id, "_account_"); i >= 0 {
		if acct := id[i+len("_account_"):]; acct != "" {
			return "account_" + acct, session
		}
		id = id[:i]
	}
	if strings.HasPrefix(id, "user_") && len(id) > len("user_")+12 {
		id = id[:len("user_")+12] // a long device hash; a prefix is enough to tell users apart
	}
	return id, session
}

func geminiModel(path string) string {
	i := strings.Index(path, "/models/")
	if i < 0 {
		return ""
	}
	rest := path[i+len("/models/"):]
	if j := strings.IndexAny(rest, ":/"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// accumulator folds one or more JSON payloads (a full response, or the chunks
// of a stream) into a respInfo.
type accumulator struct {
	format string
	ri     respInfo
	text   strings.Builder
	// streaming tool calls keyed by index
	tools map[int]*ToolCall
	order []int
}

func newAccumulator(format string) *accumulator {
	return &accumulator{format: format, tools: map[int]*ToolCall{}}
}

func (a *accumulator) tool(idx int) *ToolCall {
	t := a.tools[idx]
	if t == nil {
		t = &ToolCall{}
		a.tools[idx] = t
		a.order = append(a.order, idx)
	}
	return t
}

func (a *accumulator) addTool(name, input string) {
	t := a.tool(len(a.order) + 1_000_000) // unique, non-streamed
	t.Name, t.Input = name, input
}

func (a *accumulator) errorFrom(o obj) {
	if e := m(o["error"]); e != nil {
		a.ri.Error = firstNonEmpty(str(e["message"]), compact(e))
	} else if s := str(o["error"]); s != "" {
		a.ri.Error = s
	}
}

func (a *accumulator) feed(o obj) {
	if o == nil {
		return
	}
	a.errorFrom(o)
	switch a.format {
	case FormatAnthropic:
		a.feedAnthropic(o)
	case FormatGemini:
		if inner := m(o["response"]); inner != nil { // Code Assist wrapper
			o = inner
		}
		a.feedGemini(o)
	default:
		a.feedOpenAI(o)
	}
}

func (a *accumulator) feedOpenAI(o obj) {
	if s := str(o["model"]); s != "" {
		a.ri.Model = s
	}
	if u := m(o["usage"]); u != nil {
		a.ri.InputTokens = firstInt(num(u["prompt_tokens"]), num(u["input_tokens"]), a.ri.InputTokens)
		a.ri.OutputTokens = firstInt(num(u["completion_tokens"]), num(u["output_tokens"]), a.ri.OutputTokens)
	}
	// Chat Completions (full and streamed)
	for _, c := range arr(o["choices"]) {
		co := m(c)
		if msg := m(co["message"]); msg != nil {
			a.text.WriteString(textOf(msg["content"]))
			for _, tc := range arr(msg["tool_calls"]) {
				f := m(m(tc)["function"])
				a.addTool(str(f["name"]), str(f["arguments"]))
			}
		}
		if d := m(co["delta"]); d != nil {
			a.text.WriteString(str(d["content"]))
			for _, tc := range arr(d["tool_calls"]) {
				tco := m(tc)
				t := a.tool(num(tco["index"]))
				f := m(tco["function"])
				if n := str(f["name"]); n != "" {
					t.Name = n
				}
				t.Input += str(f["arguments"])
			}
		}
		if s := str(co["text"]); s != "" { // legacy completions
			a.text.WriteString(s)
		}
	}
	// Responses API: full object, or the final "response.completed" event
	if r := m(o["response"]); r != nil && r["output"] != nil {
		a.text.Reset()
		a.tools, a.order = map[int]*ToolCall{}, nil
		a.feedOpenAI(r)
		return
	}
	if t := str(o["type"]); t == "response.output_text.delta" {
		a.text.WriteString(str(o["delta"]))
	}
	for _, it := range arr(o["output"]) {
		io := m(it)
		switch t := str(io["type"]); t {
		case "message":
			a.text.WriteString(textOf(io["content"]))
		case "function_call":
			a.addTool(str(io["name"]), str(io["arguments"]))
		case "custom_tool_call":
			a.addTool(str(io["name"]), str(io["input"]))
		case "reasoning", "":
		default: // local_shell_call, web_search_call, computer_call, ...
			a.addTool(strings.TrimSuffix(t, "_call"), compact(firstAny(io["action"], io["arguments"], io["input"])))
		}
	}
}

func (a *accumulator) feedAnthropic(o obj) {
	switch str(o["type"]) {
	case "message": // full response
		a.ri.Model = str(o["model"])
		a.anthropicUsage(m(o["usage"]))
		for _, b := range arr(o["content"]) {
			a.anthropicBlock(-1, m(b))
		}
	case "message_start":
		msg := m(o["message"])
		a.ri.Model = str(msg["model"])
		a.anthropicUsage(m(msg["usage"]))
	case "content_block_start":
		a.anthropicBlock(num(o["index"]), m(o["content_block"]))
	case "content_block_delta":
		d := m(o["delta"])
		switch str(d["type"]) {
		case "text_delta":
			a.text.WriteString(str(d["text"]))
		case "input_json_delta":
			a.tool(num(o["index"])).Input += str(d["partial_json"])
		}
	case "message_delta":
		a.anthropicUsage(m(o["usage"]))
	}
}

func (a *accumulator) anthropicUsage(u obj) {
	if u == nil {
		return
	}
	if in := num(u["input_tokens"]) + num(u["cache_read_input_tokens"]) + num(u["cache_creation_input_tokens"]); in > 0 {
		a.ri.InputTokens = in
	}
	if out := num(u["output_tokens"]); out > 0 {
		a.ri.OutputTokens = out
	}
}

func (a *accumulator) anthropicBlock(idx int, b obj) {
	switch str(b["type"]) {
	case "text":
		a.text.WriteString(str(b["text"]))
	case "tool_use", "server_tool_use", "mcp_tool_use":
		input := ""
		if in := m(b["input"]); len(in) > 0 {
			input = compact(in)
		}
		if idx < 0 {
			a.addTool(str(b["name"]), input)
		} else {
			t := a.tool(idx)
			t.Name, t.Input = str(b["name"]), input
		}
	}
}

func (a *accumulator) feedGemini(o obj) {
	if s := str(o["modelVersion"]); s != "" {
		a.ri.Model = s
	}
	if u := m(o["usageMetadata"]); u != nil {
		a.ri.InputTokens = firstInt(num(u["promptTokenCount"]), a.ri.InputTokens)
		a.ri.OutputTokens = firstInt(num(u["candidatesTokenCount"])+num(u["thoughtsTokenCount"]), a.ri.OutputTokens)
	}
	if cands := arr(o["candidates"]); len(cands) > 0 {
		for _, p := range arr(m(m(cands[0])["content"])["parts"]) {
			po := m(p)
			if po["thought"] == true {
				continue
			}
			a.text.WriteString(str(po["text"]))
			if fc := m(po["functionCall"]); fc != nil {
				a.addTool(str(fc["name"]), compact(fc["args"]))
			}
		}
	}
}

func (a *accumulator) result() respInfo {
	a.ri.Text = a.text.String()
	sort.Ints(a.order)
	for _, i := range a.order {
		if t := a.tools[i]; t.Name != "" {
			a.ri.ToolCalls = append(a.ri.ToolCalls, *t)
		}
	}
	return a.ri
}

func firstInt(vals ...int) int {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}

func firstAny(vals ...any) any {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}

// parseResponse handles JSON bodies, JSON arrays (Gemini streams without
// alt=sse) and Server-Sent Events streams.
func parseResponse(format string, contentType string, body []byte) respInfo {
	a := newAccumulator(format)
	trimmed := bytes.TrimSpace(body)
	switch {
	case strings.Contains(contentType, "text/event-stream") || bytes.HasPrefix(trimmed, []byte("data:")) || bytes.HasPrefix(trimmed, []byte("event:")):
		for _, payload := range ssePayloads(body) {
			var o obj
			if json.Unmarshal([]byte(payload), &o) == nil {
				a.feed(o)
			}
		}
	case bytes.HasPrefix(trimmed, []byte("[")):
		var list []obj
		if json.Unmarshal(trimmed, &list) == nil {
			for _, o := range list {
				a.feed(o)
			}
		}
	case bytes.HasPrefix(trimmed, []byte("{")):
		var o obj
		if json.Unmarshal(trimmed, &o) == nil {
			a.feed(o)
		} else {
			// NDJSON (e.g. Ollama native streaming)
			for _, line := range bytes.Split(trimmed, []byte("\n")) {
				if json.Unmarshal(line, &o) == nil {
					a.feedNDJSON(o)
				}
			}
		}
	}
	return a.result()
}

// feedNDJSON handles Ollama's native /api/chat and /api/generate streams.
func (a *accumulator) feedNDJSON(o obj) {
	if s := str(o["model"]); s != "" {
		a.ri.Model = s
	}
	if msg := m(o["message"]); msg != nil {
		a.text.WriteString(str(msg["content"]))
	}
	a.text.WriteString(str(o["response"]))
	a.ri.InputTokens = firstInt(num(o["prompt_eval_count"]), a.ri.InputTokens)
	a.ri.OutputTokens = firstInt(num(o["eval_count"]), a.ri.OutputTokens)
}

// ssePayloads returns the data payload of each event in an SSE stream.
func ssePayloads(body []byte) []string {
	var out []string
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			p := strings.Join(cur, "\n")
			if p != "[DONE]" {
				out = append(out, p)
			}
			cur = nil
		}
	}
	for _, line := range strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n") {
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "data:"):
			cur = append(cur, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	flush()
	return out
}
