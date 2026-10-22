package monitor

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseOpenAIChat(t *testing.T) {
	req := parseRequest(FormatOpenAI, "/v1/chat/completions", []byte(`{"model":"gpt-4o","stream":false,
		"tools":[{"type":"function"}],
		"messages":[{"role":"system","content":"sys"},{"role":"user","content":[{"type":"text","text":"list files"}]}]}`))
	if req.Model != "gpt-4o" || req.Prompt != "list files" || req.ToolsOffered != 1 {
		t.Fatalf("request: %+v", req)
	}
	resp := parseResponse(FormatOpenAI, "application/json", []byte(`{"model":"gpt-4o-2024","choices":[{"message":{"content":"ok",
		"tool_calls":[{"function":{"name":"shell","arguments":"{\"cmd\":\"ls\"}"}}]}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	if resp.Model != "gpt-4o-2024" || resp.Text != "ok" || resp.InputTokens != 10 || resp.OutputTokens != 5 ||
		len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "shell" || resp.ToolCalls[0].Input != `{"cmd":"ls"}` {
		t.Fatalf("response: %+v", resp)
	}
}

func TestParseOpenAIChatStream(t *testing.T) {
	body := "data: {\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"lo\",\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"read_file\",\"arguments\":\"{\\\"p\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\":1}\"}}]}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"
	resp := parseResponse(FormatOpenAI, "text/event-stream", []byte(body))
	if resp.Text != "Hello" || resp.InputTokens != 3 || resp.OutputTokens != 2 ||
		len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "read_file" || resp.ToolCalls[0].Input != `{"p":1}` {
		t.Fatalf("response: %+v", resp)
	}
}

func TestParseOpenAIResponsesAPI(t *testing.T) {
	req := parseRequest(FormatOpenAI, "/v1/responses", []byte(`{"model":"gpt-5","input":[{"role":"user","content":[{"type":"input_text","text":"fix bug"}]}]}`))
	if req.Prompt != "fix bug" {
		t.Fatalf("request: %+v", req)
	}
	body := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-5\",\"output\":[" +
		"{\"type\":\"reasoning\"},{\"type\":\"function_call\",\"name\":\"apply_patch\",\"arguments\":\"{}\"}," +
		"{\"type\":\"local_shell_call\",\"action\":{\"command\":[\"ls\"]}}," +
		"{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\"}]}],\"usage\":{\"input_tokens\":7,\"output_tokens\":9}}}\n\n"
	resp := parseResponse(FormatOpenAI, "text/event-stream", []byte(body))
	if resp.Text != "done" || resp.Model != "gpt-5" || resp.InputTokens != 7 || resp.OutputTokens != 9 || len(resp.ToolCalls) != 2 ||
		resp.ToolCalls[0].Name != "apply_patch" || resp.ToolCalls[1].Name != "local_shell" {
		t.Fatalf("response: %+v", resp)
	}
}

func TestParseAnthropic(t *testing.T) {
	req := parseRequest(FormatAnthropic, "/v1/messages", []byte(`{"model":"claude-x","stream":true,"tools":[{},{}],
		"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":[{"type":"tool_result","content":"file.txt"}]}]}`))
	if req.Model != "claude-x" || !req.Stream || req.ToolsOffered != 2 || req.Prompt != "[tool_result] file.txt" {
		t.Fatalf("request: %+v", req)
	}
	full := parseResponse(FormatAnthropic, "application/json", []byte(`{"type":"message","model":"claude-x","content":[
		{"type":"text","text":"Let me check."},{"type":"tool_use","name":"Bash","input":{"command":"ls"}}],
		"usage":{"input_tokens":4,"cache_read_input_tokens":6,"output_tokens":3}}`))
	if full.Text != "Let me check." || full.InputTokens != 10 || full.OutputTokens != 3 || len(full.ToolCalls) != 1 ||
		full.ToolCalls[0].Name != "Bash" || full.ToolCalls[0].Input != `{"command":"ls"}` {
		t.Fatalf("full: %+v", full)
	}
	stream := `event: message_start
data: {"type":"message_start","message":{"model":"claude-x","usage":{"input_tokens":12,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Running"}}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","name":"Edit","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"file\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"a.go\"}"}}

event: message_delta
data: {"type":"message_delta","usage":{"output_tokens":20}}

event: message_stop
data: {"type":"message_stop"}
`
	s := parseResponse(FormatAnthropic, "text/event-stream", []byte(stream))
	if s.Text != "Running" || s.InputTokens != 12 || s.OutputTokens != 20 || len(s.ToolCalls) != 1 ||
		s.ToolCalls[0].Name != "Edit" || s.ToolCalls[0].Input != `{"file":"a.go"}` {
		t.Fatalf("stream: %+v", s)
	}
	errResp := parseResponse(FormatAnthropic, "application/json", []byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))
	if errResp.Error != "Overloaded" {
		t.Fatalf("error: %+v", errResp)
	}
}

func TestParseGemini(t *testing.T) {
	path := "/v1beta/models/gemini-2.5-pro:streamGenerateContent"
	req := parseRequest(FormatGemini, path, []byte(`{"contents":[{"role":"user","parts":[{"text":"what's up"}]}],
		"tools":[{"functionDeclarations":[{},{},{}]}]}`))
	if req.Model != "gemini-2.5-pro" || !req.Stream || req.Prompt != "what's up" || req.ToolsOffered != 3 {
		t.Fatalf("request: %+v", req)
	}
	body := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hi \"}]}}]}\r\n\r\n" +
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"there\"},{\"functionCall\":{\"name\":\"run_shell_command\",\"args\":{\"command\":\"pwd\"}}}]}}]," +
		"\"usageMetadata\":{\"promptTokenCount\":5,\"candidatesTokenCount\":8},\"modelVersion\":\"gemini-2.5-pro-001\"}\r\n\r\n"
	resp := parseResponse(FormatGemini, "text/event-stream", []byte(body))
	if resp.Text != "Hi there" || resp.Model != "gemini-2.5-pro-001" || resp.InputTokens != 5 || resp.OutputTokens != 8 ||
		len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "run_shell_command" {
		t.Fatalf("response: %+v", resp)
	}
	arrResp := parseResponse(FormatGemini, "application/json", []byte(`[{"candidates":[{"content":{"parts":[{"text":"a"}]}}]},{"candidates":[{"content":{"parts":[{"text":"b"}]}}]}]`))
	if arrResp.Text != "ab" {
		t.Fatalf("array: %+v", arrResp)
	}
}

func TestDetectFormat(t *testing.T) {
	cases := map[string]string{
		"/v1/messages":                     FormatAnthropic,
		"/v1beta/models/x:generateContent": FormatGemini,
		"/v1/chat/completions":             FormatOpenAI,
	}
	for path, want := range cases {
		if got := detectFormat(FormatAuto, path); got != want {
			t.Errorf("%s: got %s want %s", path, got, want)
		}
	}
}

func TestParseUpstream(t *testing.T) {
	u, err := ParseUpstream("azure=https://x.openai.azure.com/openai,openai")
	if err != nil || u.Name != "azure" || u.Target.Host != "x.openai.azure.com" || u.Format != FormatOpenAI {
		t.Fatalf("%+v %v", u, err)
	}
	if _, err := ParseUpstream("bad"); err == nil {
		t.Fatal("expected error")
	}
}

// TestGatewayEndToEnd runs a fake Anthropic upstream and checks that the
// gateway streams the response through unchanged, strips nothing the agent
// needs, records the call and never persists the API key.
func TestGatewayEndToEnd(t *testing.T) {
	const sse = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-test\",\"usage\":{\"input_tokens\":3}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"pong\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":1}}\n\n"
	var gotKey, gotPath, gotAgentHdr string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotPath, gotAgentHdr = r.Header.Get("X-Api-Key"), r.URL.Path, r.Header.Get("X-Agent-Name")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	store, err := OpenStore(dir, 100)
	if err != nil {
		t.Fatal(err)
	}
	ups := DefaultUpstreams()
	ups["anthropic"].Target, _ = url.Parse(upstream.URL)
	srv := httptest.NewServer(&Server{Gateway: &Gateway{Store: store, Upstreams: ups, MaxText: 1000}, Token: "secret"})
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/anthropic/v1/messages", strings.NewReader(
		`{"model":"claude-test","stream":true,"messages":[{"role":"user","content":"ping"}]}`))
	req.Header.Set("X-Api-Key", "sk-ant-SECRET")
	req.Header.Set("X-Agent-Name", "my-bot")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != sse {
		t.Fatalf("stream altered: %q", body)
	}
	if gotKey != "sk-ant-SECRET" || gotPath != "/v1/messages" || gotAgentHdr != "" {
		t.Fatalf("upstream saw key=%q path=%q agentHdr=%q", gotKey, gotPath, gotAgentHdr)
	}

	// API requires the token
	if r, _ := http.Get(srv.URL + "/api/events"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", r.StatusCode)
	}
	var events []Event
	waitFor(t, func() bool {
		r, err := http.Get(srv.URL + "/api/events?token=secret")
		if err != nil {
			return false
		}
		defer r.Body.Close()
		events = nil
		_ = json.NewDecoder(r.Body).Decode(&events)
		return len(events) == 1
	})
	e := events[0]
	if e.Agent != "my-bot" || e.Model != "claude-test" || e.Prompt != "ping" || e.Response != "pong" ||
		e.InputTokens != 3 || e.OutputTokens != 1 || !e.Stream || e.Status != 200 {
		t.Fatalf("event: %+v", e)
	}
	store.Close()
	raw, _ := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if strings.Contains(string(raw), "SECRET") {
		t.Fatal("api key persisted")
	}
	// reload from disk
	store2, _ := OpenStore(dir, 100)
	if got := store2.Query(Filter{}); len(got) != 1 || got[0].ID != e.ID {
		t.Fatalf("reload: %+v", got)
	}
}

func TestAgentPrefixAndUserAgent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"x"}}]}`)
	}))
	defer upstream.Close()
	store, _ := OpenStore("", 10)
	ups := map[string]*Upstream{"openai": {Name: "openai", Format: FormatOpenAI}}
	ups["openai"].Target, _ = url.Parse(upstream.URL)
	srv := httptest.NewServer(&Server{Gateway: &Gateway{Store: store, Upstreams: ups}})
	defer srv.Close()

	r, _ := http.Post(srv.URL+"/a/research-bot/openai/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/openai/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("User-Agent", "codex_cli_rs/0.40")
	r, _ = http.DefaultClient.Do(req)
	io.Copy(io.Discard, r.Body)
	r.Body.Close()

	waitFor(t, func() bool { return len(store.Query(Filter{})) == 2 })
	got := store.Query(Filter{})
	if got[1].Agent != "research-bot" || got[0].Agent != "codex" {
		t.Fatalf("agents: %s, %s", got[1].Agent, got[0].Agent)
	}
	if r, _ := http.Get(srv.URL + "/nope/x"); r.StatusCode != 404 {
		t.Fatalf("unknown provider: %d", r.StatusCode)
	}
}

func TestIngestHookAndStream(t *testing.T) {
	store, _ := OpenStore("", 10)
	srv := httptest.NewServer(&Server{Gateway: &Gateway{Store: store, Upstreams: DefaultUpstreams()}})
	defer srv.Close()

	streamResp, err := http.Get(srv.URL + "/api/stream?kind=tool_use")
	if err != nil {
		t.Fatal(err)
	}
	defer streamResp.Body.Close()
	rd := bufio.NewReader(streamResp.Body)
	if line, _ := rd.ReadString('\n'); !strings.HasPrefix(line, ": connected") {
		t.Fatalf("stream start: %q", line)
	}

	hook := `{"hook_event_name":"PreToolUse","session_id":"s1","cwd":"/repo","tool_name":"Bash","tool_input":{"command":"rm -rf build"}}`
	r, err := http.Post(srv.URL+"/api/ingest", "application/json", strings.NewReader(hook))
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("ingest: %v %v", err, r.StatusCode)
	}
	r, _ = http.Post(srv.URL+"/api/ingest?agent=mine", "application/json", strings.NewReader(`[{"kind":"custom","response":"hello"}]`))
	if r.StatusCode != 200 {
		t.Fatalf("ingest list: %d", r.StatusCode)
	}

	done := make(chan string, 1)
	go func() {
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			if strings.HasPrefix(line, "data: ") {
				done <- line
				return
			}
		}
	}()
	select {
	case line := <-done:
		var e Event
		_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e)
		if e.Agent != "claude-code" || e.Kind != KindToolUse || e.Session != "s1" || e.ToolCalls[0].Name != "Bash" ||
			e.ToolCalls[0].Input != `{"command":"rm -rf build"}` {
			t.Fatalf("event: %+v", e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no streamed event")
	}

	st := store.Stats(Filter{})
	if st.Total != 2 || len(st.Tools) != 1 || st.Tools[0].Key != "Bash" {
		t.Fatalf("stats: %+v", st)
	}
	if got := store.Query(Filter{Query: "RM -RF"}); len(got) != 1 {
		t.Fatalf("search: %d", len(got))
	}
}

func TestStoreRing(t *testing.T) {
	s, _ := OpenStore("", 3)
	for i := 0; i < 5; i++ {
		s.Add(Event{Agent: string(rune('a' + i))})
	}
	got := s.Query(Filter{})
	if len(got) != 3 || got[0].Agent != "e" || got[2].Agent != "c" {
		t.Fatalf("ring: %+v", got)
	}
}

func TestTruncateUTF8(t *testing.T) {
	if got := truncate("héllo", 2); got != "h…[truncated]" {
		t.Fatalf("%q", got)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met")
}
