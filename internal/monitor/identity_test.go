package monitor

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestParseAnthropicUserID(t *testing.T) {
	cases := []struct{ in, user, session string }{
		{"user_0123456789abcdef0123_account_5f2c-11aa_session_9e8d-77", "account_5f2c-11aa", "9e8d-77"},
		{"user_0123456789abcdef0123__session_abc", "user_0123456789ab", "abc"},
		{`{"device_id":"dev1","account_uuid":"acc-1","session_id":"s-1"}`, "account_acc-1", "s-1"},
		{"customer-42", "customer-42", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		u, s := parseAnthropicUserID(c.in)
		if u != c.user || s != c.session {
			t.Errorf("%q: got (%q,%q) want (%q,%q)", c.in, u, s, c.user, c.session)
		}
	}
}

func TestKeyFingerprint(t *testing.T) {
	mk := func(h, v string) *http.Request {
		r := httptest.NewRequest("POST", "/x", nil)
		r.Header.Set(h, v)
		return r
	}
	a := keyFingerprint(mk("X-Api-Key", "sk-ant-api03-AAAAAAAAAAAAAAAAWxyz"))
	b := keyFingerprint(mk("Authorization", "Bearer sk-proj-BBBBBBBBBBBBBBBBWxyz"))
	if !strings.HasPrefix(a, "…Wxyz#") || !strings.HasPrefix(b, "…Wxyz#") || a == b {
		t.Fatalf("fingerprints %q %q", a, b)
	}
	if strings.Contains(a, "AAAA") {
		t.Fatal("fingerprint leaks key")
	}
	if got := keyFingerprint(httptest.NewRequest("GET", "/x?key=short", nil)); !strings.HasPrefix(got, "#") {
		t.Fatalf("short key %q", got)
	}
	if keyFingerprint(httptest.NewRequest("GET", "/x", nil)) != "" {
		t.Fatal("no key should give empty fingerprint")
	}
}

// TestGatewayIdentities sends calls as two instances of one agent with
// different keys and checks identities, sessions, alerts and the report.
func TestGatewayIdentities(t *testing.T) {
	var forwarded http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded = r.Header.Clone()
		io.WriteString(w, `{"model":"gpt-x","choices":[{"message":{"content":"ok","tool_calls":[{"function":{"name":"shell","arguments":"{}"}}]}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	}))
	defer upstream.Close()
	store, _ := OpenStore("", 100)
	ups := map[string]*Upstream{"openai": {Name: "openai", Format: FormatOpenAI}}
	ups["openai"].Target, _ = url.Parse(upstream.URL)
	srv := httptest.NewServer(&Server{Gateway: &Gateway{Store: store, Upstreams: ups}})
	defer srv.Close()

	call := func(agentID, key, body string) {
		req, _ := http.NewRequest("POST", srv.URL+"/a/builder/openai/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		if agentID != "" {
			req.Header.Set("X-Agent-Id", agentID)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	call("worker-1", "sk-proj-111111111111111111111111", `{"model":"gpt-x","user":"alice","prompt_cache_key":"conv-A","messages":[]}`)
	call("worker-1", "sk-proj-111111111111111111111111", `{"model":"gpt-x","user":"alice","prompt_cache_key":"conv-A","messages":[]}`)
	call("worker-2", "sk-proj-222222222222222222222222", `{"model":"gpt-x","user":"bob","prompt_cache_key":"conv-B","messages":[]}`)
	waitFor(t, func() bool { return len(store.Query(Filter{})) == 3 })

	if forwarded.Get("X-Agent-Id") != "" {
		t.Fatal("identity header leaked upstream")
	}
	e := store.Query(Filter{AgentID: "worker-2"})
	if len(e) != 1 || e[0].User != "bob" || e[0].Session != "conv-B" || !strings.HasPrefix(e[0].KeyID, "…2222#") {
		t.Fatalf("worker-2 event: %+v", e)
	}

	st := store.Stats(Filter{})
	if len(st.Agents) != 1 || st.Agents[0].AgentIDs != 2 || st.Agents[0].Keys != 2 || st.Agents[0].Users != 2 || st.Agents[0].Sessions != 2 {
		t.Fatalf("agent stats: %+v", st.Agents)
	}
	if len(st.Identities) != 2 || st.Identities[0].Events != 2 || st.Identities[0].AgentID != "worker-1" || st.Identities[0].Sessions != 1 {
		t.Fatalf("identities: %+v", st.Identities)
	}
	if len(st.Sessions) != 2 || st.Sessions[0].TopTools[0] != "shell" {
		t.Fatalf("sessions: %+v", st.Sessions)
	}

	rep := store.Report(Filter{}, 10)
	if len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "builder ran under 2 API keys, 2 user/accounts, 2 agent IDs") {
		t.Fatalf("alerts: %v", rep.Alerts)
	}
	var text bytes.Buffer
	if err := rep.WriteText(&text, 0); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ATTENTION", "AGENTS", "IDENTITIES", "SESSIONS", "worker-1", "conv-A", "alice", "TOOLS", "shell", "RECENT ACTIVITY"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("text report missing %q:\n%s", want, text.String())
		}
	}

	// filtered report over HTTP, as the CLI requests it
	resp, err := http.Get(srv.URL + "/api/report?format=json&agent_id=worker-1&recent=-1")
	if err != nil {
		t.Fatal(err)
	}
	var got Report
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got.Stats.Total != 2 || len(got.Recent) != 2 || len(got.Alerts) != 0 || got.Filter.AgentID != "worker-1" {
		t.Fatalf("filtered report: total=%d recent=%d alerts=%v", got.Stats.Total, len(got.Recent), got.Alerts)
	}
	resp, _ = http.Get(srv.URL + "/api/report?format=csv&recent=-1")
	rows, err := csv.NewReader(resp.Body).ReadAll()
	resp.Body.Close()
	if err != nil || len(rows) != 4 || rows[0][2] != "agent_id" || rows[1][2] != "worker-1" || rows[3][4] == "" {
		t.Fatalf("csv: %v %v", rows, err)
	}
}

func TestReportFromLogAndHooks(t *testing.T) {
	dir := t.TempDir()
	store, _ := OpenStore(dir, 100)
	evs, _ := eventsFromIngest([]byte(`{"hook_event_name":"PreToolUse","session_id":"s1","agent_id":"sub-7","agent_type":"Explore","tool_name":"Grep","tool_input":{}}`), "", 100)
	store.Add(evs[0])
	store.Add(Event{Agent: "old", Time: time.Now().Add(-72 * time.Hour)})
	store.Close()

	events, err := LoadEvents(dir)
	if err != nil || len(events) != 2 {
		t.Fatalf("load: %v %d", err, len(events))
	}
	since, _ := ParseSince("2d")
	rep := BuildReport(events, Filter{Since: since}, 5)
	if rep.Stats.Total != 1 || rep.Stats.Agents[0].Key != "claude-code" || rep.Recent[0].AgentID != "sub-7" || rep.Stats.Sessions[0].Key != "s1" {
		t.Fatalf("report: %+v", rep.Stats)
	}
	empty := BuildReport(events, Filter{Agent: "nobody"}, 5)
	var buf bytes.Buffer
	empty.WriteText(&buf, 0)
	if !strings.Contains(buf.String(), "No activity recorded") {
		t.Fatalf("empty report: %s", buf.String())
	}
}

func TestParseSince(t *testing.T) {
	now := time.Now()
	for in, want := range map[string]time.Duration{"30m": 30 * time.Minute, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour} {
		got, err := ParseSince(in)
		if err != nil || now.Sub(got)-want > time.Minute || now.Sub(got)-want < -time.Minute {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	if got, err := ParseSince("2026-10-01"); err != nil || got.Day() != 1 {
		t.Errorf("date: %v %v", got, err)
	}
	if _, err := ParseSince("yesterday"); err == nil {
		t.Error("expected error")
	}
}
