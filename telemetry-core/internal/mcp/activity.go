package mcp

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// ActivityKind enumerates the things the dashboard's "Analyst Team" tab cares
// about. Tool calls dominate; trigger arrivals and the agent's own writes are
// surfaced as their own kinds so the UI can highlight them.
type ActivityKind string

const (
	ActivityToolCall      ActivityKind = "tool_call"
	ActivityToolResult    ActivityKind = "tool_result"
	ActivityTrigger       ActivityKind = "trigger"
	ActivityObservation   ActivityKind = "observation"
	ActivityInsightPushed ActivityKind = "insight_pushed"
	ActivityQueryAnswer   ActivityKind = "query_answer"
	ActivityError         ActivityKind = "error"
)

// Activity is one entry in the pi-agent's live feed.
//
// RunID groups every tool call that belongs to a single specialist
// dispatch — when the dashboard wants to see "what is THIS run doing
// right now", filter activities by run_id. Activities outside any run
// (pull_next_trigger polls, server-initiated writes) leave it empty.
type Activity struct {
	ID         uint64         `json:"id"`
	At         time.Time      `json:"at"`
	Kind       ActivityKind   `json:"kind"`
	Tool       string         `json:"tool,omitempty"`
	Persona    string         `json:"persona,omitempty"` // populated when known (responder, tires, …)
	RunID      string         `json:"run_id,omitempty"`
	Args       any            `json:"args,omitempty"`
	Summary    string         `json:"summary,omitempty"`
	DurationMs int64          `json:"duration_ms,omitempty"`
	Meta       map[string]any `json:"meta,omitempty"`
	Error      string         `json:"error,omitempty"`
}

// ActivityHub fans pi-agent events out to /api/pi_agent/stream subscribers
// and keeps a small ring for cold-start so /api/pi_agent/recent never returns
// an empty list right after a page load.
type ActivityHub struct {
	mu      sync.Mutex
	ring    []Activity
	cap     int
	nextID  atomic.Uint64
	subs    map[chan Activity]struct{}
}

// NewActivityHub returns a hub that keeps the last `ringCap` activities (256
// is a sensible default — covers ~10 minutes of pi-agent work at typical
// cadence).
func NewActivityHub(ringCap int) *ActivityHub {
	if ringCap < 16 {
		ringCap = 16
	}
	return &ActivityHub{
		ring: make([]Activity, 0, ringCap),
		cap:  ringCap,
		subs: map[chan Activity]struct{}{},
	}
}

// Publish appends an activity to the ring and fans it out to live subscribers.
// Non-blocking: subscribers that fall behind silently miss events (they should
// reconnect or refresh from the ring).
func (h *ActivityHub) Publish(a Activity) {
	a.ID = h.nextID.Add(1)
	if a.At.IsZero() {
		a.At = time.Now()
	}
	h.mu.Lock()
	if len(h.ring) >= h.cap {
		h.ring = h.ring[1:]
	}
	h.ring = append(h.ring, a)
	subs := make([]chan Activity, 0, len(h.subs))
	for c := range h.subs {
		subs = append(subs, c)
	}
	h.mu.Unlock()
	for _, c := range subs {
		select {
		case c <- a:
		default:
			// subscriber too slow — drop rather than block the producer.
		}
	}
}

// Recent returns up to limit activities (default 100), newest last so the UI
// can append directly. limit<=0 returns the entire ring.
func (h *ActivityHub) Recent(limit int) []Activity {
	h.mu.Lock()
	defer h.mu.Unlock()
	if limit <= 0 || limit > len(h.ring) {
		out := make([]Activity, len(h.ring))
		copy(out, h.ring)
		return out
	}
	start := len(h.ring) - limit
	out := make([]Activity, limit)
	copy(out, h.ring[start:])
	return out
}

// Subscribe returns a channel receiving every Publish() call. The cancel
// function is mandatory — callers must invoke it when their request ends so
// the hub doesn't accumulate dead subscribers.
func (h *ActivityHub) Subscribe(buf int) (<-chan Activity, func()) {
	if buf <= 0 {
		buf = 64
	}
	c := make(chan Activity, buf)
	h.mu.Lock()
	h.subs[c] = struct{}{}
	h.mu.Unlock()
	cancel := func() {
		h.mu.Lock()
		if _, ok := h.subs[c]; ok {
			delete(h.subs, c)
			close(c)
		}
		h.mu.Unlock()
	}
	return c, cancel
}

// SubscriberCount is for diagnostics / /api/agent/status.
func (h *ActivityHub) SubscriberCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// installHooks wires the mcp-go BeforeCallTool / AfterCallTool callbacks to
// the activity hub so every tool the pi agent calls shows up in the live
// feed. Some tools get special handling so the UI can render them more
// cleanly (e.g. pull_next_trigger surfaces the Trigger payload as a
// distinct ActivityTrigger entry).
func installActivityHooks(hooks *mcpserver.Hooks, hub *ActivityHub) {
	type startKey struct{ id any }
	starts := sync.Map{} // startKey → time.Time

	hooks.AddBeforeCallTool(func(_ context.Context, id any, req *mcp.CallToolRequest) {
		starts.Store(startKey{id: id}, time.Now())
		args := req.GetArguments()
		runID, persona := extractRunMeta(args)
		hub.Publish(Activity{
			Kind:    ActivityToolCall,
			Tool:    req.Params.Name,
			RunID:   runID,
			Persona: persona,
			Args:    redactArgs(req.Params.Name, args),
			Summary: summariseCall(req.Params.Name, args),
		})
	})

	hooks.AddAfterCallTool(func(_ context.Context, id any, req *mcp.CallToolRequest, raw any) {
		startedAny, _ := starts.LoadAndDelete(startKey{id: id})
		started, _ := startedAny.(time.Time)
		var dur int64
		if !started.IsZero() {
			dur = time.Since(started).Milliseconds()
		}
		text, isErr := summariseResult(raw)
		kind := ActivityToolResult
		if isErr {
			kind = ActivityError
		}
		args := req.GetArguments()
		runID, persona := extractRunMeta(args)
		// Special-case some tools so the timeline is more readable than a
		// raw JSON blob.
		switch req.Params.Name {
		case "pull_next_trigger":
			if t, ok := decodeTrigger(text); ok {
				hub.Publish(Activity{
					Kind:       ActivityTrigger,
					Summary:    triggerSummary(t),
					DurationMs: dur,
					Meta: map[string]any{
						"kind":          t.Kind,
						"job_id":        t.JobID,
						"event_code":    t.EventCode,
						"lap":           t.Lap,
						"context_topic": t.ContextTopic,
						"urgent":        t.Urgent,
					},
				})
				return
			}
		case "submit_query_answer":
			hub.Publish(Activity{
				Kind:    ActivityQueryAnswer,
				RunID:   runID,
				Persona: persona,
				Summary: trimText(stringField(args, "answer"), 200),
				Meta: map[string]any{
					"job_id": stringField(args, "job_id"),
					"urgent": args["urgent"],
				},
				DurationMs: dur,
			})
			return
		case "write_observation":
			hub.Publish(Activity{
				Kind:    ActivityObservation,
				RunID:   runID,
				Persona: persona,
				Summary: trimText(stringField(args, "summary"), 200),
				Meta: map[string]any{
					"topic":      "pi_agent." + stringField(args, "topic"),
					"hypothesis": args["hypothesis"],
				},
				DurationMs: dur,
			})
			return
		case "push_insight":
			hub.Publish(Activity{
				Kind:    ActivityInsightPushed,
				RunID:   runID,
				Persona: persona,
				Summary: trimText(stringField(args, "insight"), 200),
				Meta: map[string]any{
					"priority": args["priority"],
					"source":   stringField(args, "source"),
				},
				DurationMs: dur,
			})
			return
		}
		hub.Publish(Activity{
			Kind:       kind,
			Tool:       req.Params.Name,
			RunID:      runID,
			Persona:    persona,
			Summary:    trimText(text, 240),
			DurationMs: dur,
		})
	})
}

// extractRunMeta pulls the pi-agent's injected `_run_id` / `_persona`
// tracking arguments out of a tool-call payload. These keys are added
// client-side by MCPClient.call so every activity can be grouped by run
// without a per-tool schema change. They are NOT forwarded to the tool's
// actual handler — mcp-go ignores unknown args and the registered tool
// schemas don't declare them, so the underlying handler never sees them.
func extractRunMeta(args map[string]any) (runID, persona string) {
	if args == nil {
		return "", ""
	}
	runID = stringField(args, "_run_id")
	persona = stringField(args, "_persona")
	return
}

// redactArgs trims arg payloads that aren't useful in the live feed (long SQL
// strings still pass through but bounded).
func redactArgs(tool string, args map[string]any) map[string]any {
	if args == nil {
		return nil
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		if s, ok := v.(string); ok && len(s) > 400 {
			out[k] = s[:400] + "…"
		} else {
			out[k] = v
		}
	}
	return out
}

func summariseCall(tool string, args map[string]any) string {
	switch tool {
	case "query_sql":
		return trimText(stringField(args, "sql"), 140)
	case "get_skill":
		return "skill=" + stringField(args, "name")
	case "set_corner_reminder":
		msg := stringField(args, "message")
		corner := stringField(args, "corner_id")
		if corner == "" {
			return msg
		}
		return corner + ": " + msg
	}
	return ""
}

func summariseResult(raw any) (string, bool) {
	r, ok := raw.(*mcp.CallToolResult)
	if !ok || r == nil {
		return "", false
	}
	var b []byte
	for _, part := range r.Content {
		if tc, ok := mcp.AsTextContent(part); ok {
			b = append(b, []byte(tc.Text)...)
		}
	}
	return string(b), r.IsError
}

func trimText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func stringField(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func decodeTrigger(text string) (Trigger, bool) {
	var t Trigger
	if err := json.Unmarshal([]byte(text), &t); err != nil {
		return Trigger{}, false
	}
	if t.Kind == "" || t.Kind == "none" {
		return Trigger{}, false
	}
	return t, true
}

func triggerSummary(t Trigger) string {
	switch t.Kind {
	case TriggerQuery:
		return "Q: " + trimText(t.Question, 160)
	case TriggerLapComplete:
		return "lap " + itoa(t.Lap) + " complete"
	case TriggerSignificant:
		s := t.EventCode
		if t.EventDetail != "" {
			s += " — " + t.EventDetail
		}
		return trimText(s, 200)
	}
	return string(t.Kind)
}

func itoa(n int) string {
	// avoid strconv import for one helper
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
