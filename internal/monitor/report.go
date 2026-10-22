package monitor

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// Report is a point-in-time summary of agent activity.
type Report struct {
	Generated time.Time `json:"generated"`
	Filter    Filter    `json:"filter"`
	Stats     Stats     `json:"stats"`
	Alerts    []string  `json:"alerts,omitempty"`
	Recent    []Event   `json:"recent"`
}

// BuildReport summarizes events (oldest first) matching f. recent is the
// number of latest events to include; negative includes all of them.
func BuildReport(events []Event, f Filter, recent int) Report {
	r := Report{Generated: time.Now().UTC(), Filter: f, Stats: computeStats(events, f)}
	for i := len(events) - 1; i >= 0 && (recent < 0 || len(r.Recent) < recent); i-- {
		if f.match(&events[i]) {
			r.Recent = append(r.Recent, events[i])
		}
	}
	for _, a := range r.Stats.Agents {
		var parts []string
		if a.Keys > 1 {
			parts = append(parts, plural(a.Keys, "API key"))
		}
		if a.Users > 1 {
			parts = append(parts, plural(a.Users, "user/account"))
		}
		if a.AgentIDs > 1 {
			parts = append(parts, plural(a.AgentIDs, "agent ID"))
		}
		if len(parts) > 0 {
			r.Alerts = append(r.Alerts, fmt.Sprintf("%s ran under %s", a.Key, strings.Join(parts, ", ")))
		}
	}
	if n := r.Stats.Totals.Errors; n > 0 {
		r.Alerts = append(r.Alerts, plural(n, "failed call")+" or error event(s); see recent activity")
	}
	return r
}

// Report builds a report from the events currently held in memory.
func (s *Store) Report(f Filter, recent int) Report {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return BuildReport(s.all(), f, recent)
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	if strings.HasSuffix(what, "y") && !strings.HasSuffix(what, "key") {
		return fmt.Sprintf("%d %sies", n, what[:len(what)-1])
	}
	return fmt.Sprintf("%d %ss", n, what)
}

// humanCount formats 1234567 as 1.2M.
func humanCount(n int) string {
	switch {
	case n >= 1_000_000:
		return strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64) + "M"
	case n >= 10_000:
		return strconv.Itoa(n/1000) + "k"
	case n >= 1000:
		return strconv.FormatFloat(float64(n)/1e3, 'f', 1, 64) + "k"
	}
	return strconv.Itoa(n)
}

// short keeps long ids readable: "account_1f2e…9a8b".
func short(s string, n int) string {
	if s == "" {
		return "-"
	}
	if len(s) <= n {
		return s
	}
	half := (n - 1) / 2
	return s[:half] + "…" + s[len(s)-half:]
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func when(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	t = t.Local()
	if time.Since(t) < 20*time.Hour {
		return t.Format("15:04:05")
	}
	return t.Format("Jan 02 15:04")
}

func span(a, b time.Time) string {
	d := b.Sub(a).Round(time.Second)
	if d <= 0 {
		return "-"
	}
	return d.String()
}

func tokens(c *Counter) string {
	if c.InputTokens == 0 && c.OutputTokens == 0 {
		return "-"
	}
	return humanCount(c.InputTokens) + "/" + humanCount(c.OutputTokens)
}

// WriteText renders the report as aligned tables for a terminal.
func (r Report) WriteText(w io.Writer, limit int) error {
	if limit <= 0 {
		limit = 20
	}
	st := r.Stats
	t := &st.Totals
	fmt.Fprintf(w, "AI AGENT ACTIVITY REPORT   generated %s\n", r.Generated.Local().Format("2006-01-02 15:04:05 MST"))
	if desc := r.Filter.describe(); desc != "" {
		fmt.Fprintf(w, "Filter: %s\n", desc)
	}
	if st.Total == 0 {
		fmt.Fprintln(w, "\nNo activity recorded for this selection.")
		return nil
	}
	period := span(t.FirstSeen, t.LastSeen)
	if period == "-" {
		period = "under 1s"
	}
	fmt.Fprintf(w, "Period: %s → %s (%s)\n", t.FirstSeen.Local().Format("2006-01-02 15:04"), t.LastSeen.Local().Format("2006-01-02 15:04"), period)
	fmt.Fprintf(w, "Events %d · model calls %d · tool calls %d · tokens %s in / %s out · errors %d · agents %d · identities %d · sessions %d\n",
		st.Total, t.LLMCalls, t.ToolCalls, humanCount(t.InputTokens), humanCount(t.OutputTokens), t.Errors,
		len(st.Agents), len(st.Identities), len(st.Sessions))

	if len(r.Alerts) > 0 {
		fmt.Fprintln(w, "\nATTENTION")
		for _, a := range r.Alerts {
			fmt.Fprintf(w, "  ! %s\n", a)
		}
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	section := func(title, header string) {
		tw.Flush()
		fmt.Fprintf(w, "\n%s\n", title)
		fmt.Fprintln(tw, header)
	}

	section("AGENTS", "AGENT\tAGENT IDS\tUSERS\tAPI KEYS\tSESSIONS\tEVENTS\tCALLS\tTOOLS\tTOKENS in/out\tERRORS\tFIRST SEEN\tLAST SEEN")
	for _, a := range st.Agents {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%s\t%d\t%s\t%s\n", a.Key, a.AgentIDs, a.Users, a.Keys, a.Sessions,
			a.Events, a.LLMCalls, a.ToolCalls, tokens(&a.Counter), a.Errors, when(a.FirstSeen), when(a.LastSeen))
	}

	section("IDENTITIES (agent × agent ID × user × API key)", "AGENT\tAGENT ID\tUSER / ACCOUNT\tAPI KEY\tSESSIONS\tEVENTS\tCALLS\tTOOLS\tTOKENS in/out\tERRORS\tLAST SEEN")
	for i, id := range st.Identities {
		if i == limit {
			fmt.Fprintf(tw, "… %d more\n", len(st.Identities)-limit)
			break
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\t%d\t%d\t%s\t%d\t%s\n", id.Agent, short(id.AgentID, 20), short(id.User, 26), dash(id.KeyID),
			id.Sessions, id.Events, id.LLMCalls, id.ToolCalls, tokens(&id.Counter), id.Errors, when(id.LastSeen))
	}

	if len(st.Sessions) > 0 {
		section("SESSIONS (most recent first)", "SESSION\tAGENT\tAGENT ID\tUSER\tSTARTED\tDURATION\tEVENTS\tCALLS\tTOOLS\tTOKENS in/out\tTOP TOOLS")
		for i, s := range st.Sessions {
			if i == limit {
				fmt.Fprintf(tw, "… %d more\n", len(st.Sessions)-limit)
				break
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%s\t%s\n", short(s.Key, 18), s.Agent, short(s.AgentID, 16), short(s.User, 22),
				when(s.FirstSeen), span(s.FirstSeen, s.LastSeen), s.Events, s.LLMCalls, s.ToolCalls, tokens(&s.Counter), dash(strings.Join(s.TopTools, ", ")))
		}
	}

	if len(st.Models) > 0 {
		section("MODELS", "MODEL\tCALLS\tTOKENS in/out\tERRORS\tLAST USED")
		for _, m := range st.Models {
			fmt.Fprintf(tw, "%s\t%d\t%s\t%d\t%s\n", m.Key, m.LLMCalls, tokens(&m), m.Errors, when(m.LastSeen))
		}
	}
	if len(st.Tools) > 0 {
		section("TOOLS", "TOOL\tUSES\tLAST USED")
		for i, c := range st.Tools {
			if i == limit {
				fmt.Fprintf(tw, "… %d more\n", len(st.Tools)-limit)
				break
			}
			fmt.Fprintf(tw, "%s\t%d\t%s\n", c.Key, c.ToolCalls, when(c.LastSeen))
		}
	}
	if len(r.Recent) > 0 {
		section("RECENT ACTIVITY", "TIME\tAGENT\tAGENT ID\tSESSION\tKIND\tMODEL\tSUMMARY")
		for _, e := range r.Recent {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", when(e.Time), e.Agent, short(e.AgentID, 14), short(e.Session, 14), e.Kind, dash(e.Model), summarize(&e, 70))
		}
	}
	return tw.Flush()
}

// summarize describes an event in one line.
func summarize(e *Event, n int) string {
	var s string
	switch {
	case e.Error != "":
		s = "ERROR " + e.Error
	case e.Kind == KindToolUse && len(e.ToolCalls) > 0:
		s = e.ToolCalls[0].Name + " " + e.ToolCalls[0].Input
	default:
		var names []string
		for _, t := range e.ToolCalls {
			names = append(names, t.Name)
		}
		s = firstNonEmpty(e.Response, e.Prompt, e.Path)
		if len(names) > 0 {
			s = "[" + strings.Join(names, ",") + "] " + s
		}
	}
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) > n {
		s = string([]rune(s)[:n-1]) + "…"
	}
	return dash(s)
}

func (f Filter) describe() string {
	var parts []string
	add := func(k, v string) {
		if v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	add("agent", f.Agent)
	add("agent_id", f.AgentID)
	add("user", f.User)
	add("key", f.KeyID)
	add("session", f.Session)
	add("provider", f.Provider)
	add("model", f.Model)
	add("kind", f.Kind)
	add("q", f.Query)
	if !f.Since.IsZero() {
		add("since", f.Since.Local().Format("2006-01-02 15:04"))
	}
	return strings.Join(parts, " ")
}

// WriteJSON renders the report as indented JSON.
func (r Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// WriteCSV writes the report's events, one row each, for spreadsheets.
func (r Report) WriteCSV(w io.Writer) error {
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"time", "agent", "agent_id", "user", "key_id", "session", "kind", "provider", "model",
		"status", "latency_ms", "input_tokens", "output_tokens", "tools", "error", "prompt", "response"})
	for i := len(r.Recent) - 1; i >= 0; i-- { // oldest first
		e := r.Recent[i]
		var names []string
		for _, t := range e.ToolCalls {
			names = append(names, t.Name)
		}
		_ = cw.Write([]string{e.Time.UTC().Format(time.RFC3339), e.Agent, e.AgentID, e.User, e.KeyID, e.Session, e.Kind,
			e.Provider, e.Model, strconv.Itoa(e.Status), strconv.FormatInt(e.LatencyMs, 10), strconv.Itoa(e.InputTokens),
			strconv.Itoa(e.OutputTokens), strings.Join(names, ";"), e.Error, e.Prompt, e.Response})
	}
	cw.Flush()
	return cw.Error()
}
