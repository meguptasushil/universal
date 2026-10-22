package monitor

import (
	"encoding/json"
	"strings"
)

// hookPayload is the JSON that Claude Code (and compatible CLIs such as
// Gemini CLI) pipe to hook commands on stdin.
type hookPayload struct {
	HookEventName  string          `json:"hook_event_name"`
	SessionID      string          `json:"session_id"`
	Cwd            string          `json:"cwd"`
	TranscriptPath string          `json:"transcript_path"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	ToolResponse   json.RawMessage `json:"tool_response"`
	Prompt         string          `json:"prompt"`
	Message        string          `json:"message"`
	Source         string          `json:"source"`
	Reason         string          `json:"reason"`
}

// eventsFromIngest decodes an ingest body: one Event, a list of Events, or a
// hook payload.
func eventsFromIngest(body []byte, agent string, maxText int) ([]Event, error) {
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "[") {
		var list []Event
		if err := json.Unmarshal(body, &list); err != nil {
			return nil, err
		}
		for i := range list {
			normalizeIngested(&list[i], agent, maxText)
		}
		return list, nil
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, err
	}
	if _, ok := probe["hook_event_name"]; ok {
		var h hookPayload
		if err := json.Unmarshal(body, &h); err != nil {
			return nil, err
		}
		e := eventFromHook(h, agent)
		normalizeIngested(&e, "", maxText)
		return []Event{e}, nil
	}
	var e Event
	if err := json.Unmarshal(body, &e); err != nil {
		return nil, err
	}
	normalizeIngested(&e, agent, maxText)
	return []Event{e}, nil
}

func normalizeIngested(e *Event, agent string, maxText int) {
	e.Source = SourceIngest
	if e.Agent == "" {
		e.Agent = agent
	}
	e.Prompt = truncate(e.Prompt, maxText)
	e.Response = truncate(e.Response, maxText)
	for i := range e.ToolCalls {
		e.ToolCalls[i].Input = truncate(e.ToolCalls[i].Input, maxText)
	}
}

func rawString(r json.RawMessage) string {
	if len(r) == 0 || string(r) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(r, &s) == nil {
		return s
	}
	return string(r)
}

func eventFromHook(h hookPayload, agent string) Event {
	if agent == "" {
		agent = "claude-code"
	}
	e := Event{
		Agent:   agent,
		Session: h.SessionID,
		Meta:    map[string]any{"hook": h.HookEventName},
	}
	if h.Cwd != "" {
		e.Meta["cwd"] = h.Cwd
	}
	switch h.HookEventName {
	case "PreToolUse", "PostToolUse", "PostToolUseFailure", "BeforeTool", "AfterTool":
		e.Kind = KindToolUse
		e.ToolCalls = []ToolCall{{Name: h.ToolName, Input: rawString(h.ToolInput)}}
		e.Response = rawString(h.ToolResponse)
	case "UserPromptSubmit", "BeforeAgent":
		e.Kind = KindPrompt
		e.Prompt = h.Prompt
	default: // SessionStart, SessionEnd, Stop, SubagentStop, Notification, PreCompact...
		e.Kind = KindSession
		e.Response = firstNonEmpty(h.Message, h.Reason, h.Source)
		if e.Response == "" {
			e.Response = h.HookEventName
		}
	}
	return e
}
