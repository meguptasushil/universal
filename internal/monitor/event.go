// Package monitor records the activity of AI agents (OpenAI, Anthropic, Gemini
// and compatible APIs) by sitting between the agent and the model provider,
// and by accepting events pushed from agent hooks.
package monitor

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// Event kinds.
const (
	KindLLMCall = "llm_call" // one request/response to a model API
	KindToolUse = "tool_use" // an agent executed (or is about to execute) a tool
	KindPrompt  = "prompt"   // a human prompt submitted to an agent
	KindSession = "session"  // session start/stop/notification
	KindCustom  = "custom"   // anything else pushed via ingest
)

// Event sources.
const (
	SourceProxy  = "proxy"
	SourceIngest = "ingest"
)

// ToolCall is a tool invocation requested by a model or executed by an agent.
type ToolCall struct {
	Name  string `json:"name"`
	Input string `json:"input,omitempty"`
}

// Event is one observed agent activity.
type Event struct {
	ID           string         `json:"id"`
	Time         time.Time      `json:"time"`
	Source       string         `json:"source"`
	Kind         string         `json:"kind"`
	Agent        string         `json:"agent"`
	Session      string         `json:"session,omitempty"`
	Provider     string         `json:"provider,omitempty"`
	Model        string         `json:"model,omitempty"`
	Method       string         `json:"method,omitempty"`
	Path         string         `json:"path,omitempty"`
	Status       int            `json:"status,omitempty"`
	LatencyMs    int64          `json:"latency_ms,omitempty"`
	Stream       bool           `json:"stream,omitempty"`
	InputTokens  int            `json:"input_tokens,omitempty"`
	OutputTokens int            `json:"output_tokens,omitempty"`
	Prompt       string         `json:"prompt,omitempty"`
	Response     string         `json:"response,omitempty"`
	ToolsOffered int            `json:"tools_offered,omitempty"`
	ToolCalls    []ToolCall     `json:"tool_calls,omitempty"`
	Error        string         `json:"error,omitempty"`
	ClientIP     string         `json:"client_ip,omitempty"`
	Meta         map[string]any `json:"meta,omitempty"`
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// fill sets defaults for fields that must always be present.
func (e *Event) fill() {
	if e.ID == "" {
		e.ID = newID()
	}
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	if e.Kind == "" {
		e.Kind = KindCustom
	}
	if e.Agent == "" {
		e.Agent = "unknown"
	}
}

func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	// avoid cutting in the middle of a UTF-8 sequence
	cut := n
	for cut > 0 && cut < len(s) && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "…[truncated]"
}
