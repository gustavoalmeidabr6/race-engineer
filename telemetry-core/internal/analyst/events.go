package analyst

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"
)

// ActivityKind enumerates what the dashboard's Data Analyst tab cares about.
// The set is intentionally small and mirrors the old pi-agent activity feed
// closely so the UI rewrite stays focused on the new event shape rather than
// inventing new categories.
type ActivityKind string

const (
	ActivityRuntime       ActivityKind = "runtime"        // subprocess lifecycle: starting, ready, exited, restart
	ActivityTrigger       ActivityKind = "trigger"        // lap_complete | significant_event | query
	ActivitySessionStart  ActivityKind = "session_start"  // ACP session created
	ActivityMessageChunk  ActivityKind = "message_chunk"  // streamed agent text
	ActivityToolCall      ActivityKind = "tool_call"      // agent invoked a tool (MCP or builtin)
	ActivityToolResult    ActivityKind = "tool_result"    // tool returned
	ActivityFileWrite     ActivityKind = "file_write"     // agent wrote to workspace (self-evolve)
	ActivityFileRead      ActivityKind = "file_read"      // agent read a workspace file
	ActivityPermission    ActivityKind = "permission"     // session/request_permission
	ActivityInsightPushed ActivityKind = "insight_pushed" // push_insight reached the driver
	ActivityAnswer        ActivityKind = "answer"         // final answer for a query trigger
	ActivityError         ActivityKind = "error"
)

// Activity is one entry in the analyst live feed.
//
// RunID groups every chunk/tool-call that belongs to one prompt dispatch — UI
// filters by it to render a single "run" panel. SessionID is the ACP session,
// which may host multiple runs across an F1 session.
type Activity struct {
	ID         uint64         `json:"id"`
	At         time.Time      `json:"at"`
	Kind       ActivityKind   `json:"kind"`
	SessionID  string         `json:"session_id,omitempty"`
	RunID      string         `json:"run_id,omitempty"`
	Trigger    string         `json:"trigger,omitempty"` // lap_complete | significant_event | query
	Tool       string         `json:"tool,omitempty"`
	Summary    string         `json:"summary,omitempty"`
	DurationMs int64          `json:"duration_ms,omitempty"`
	Meta       map[string]any `json:"meta,omitempty"`
	Error      string         `json:"error,omitempty"`
}

// ActivityHub fans Activity records to /api/analyst/stream subscribers and
// keeps a bounded ring for cold-start payloads.
type ActivityHub struct {
	mu     sync.Mutex
	ring   []Activity
	cap    int
	nextID atomic.Uint64
	subs   map[chan Activity]struct{}
}

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

func (h *ActivityHub) Publish(a Activity) Activity {
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
			// slow subscriber — drop rather than block the producer.
		}
	}
	return a
}

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

func (h *ActivityHub) SubscriberCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// renderSessionUpdate converts an ACP session/update notification payload
// into one (or more) Activity records. The session/update message envelope
// is `{"sessionId":"...","update":{"sessionUpdate":"<kind>", ...}}` per
// agent-client-protocol v0.13. We accept partial / unknown shapes —
// returning an empty slice for anything we don't recognise is fine because
// the dashboard's recent-feed already discards unknowns.
func renderSessionUpdate(sessionID string, params json.RawMessage) []Activity {
	var env struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
	}
	if err := json.Unmarshal(params, &env); err != nil {
		return nil
	}
	if env.SessionID != "" {
		sessionID = env.SessionID
	}
	var head struct {
		Kind string `json:"sessionUpdate"`
	}
	if err := json.Unmarshal(env.Update, &head); err != nil {
		return nil
	}
	switch head.Kind {
	case "agent_message_chunk", "agent_thought_chunk":
		var msg struct {
			Content struct {
				Text string `json:"text"`
				Type string `json:"type"`
			} `json:"content"`
		}
		if err := json.Unmarshal(env.Update, &msg); err != nil {
			return nil
		}
		text := msg.Content.Text
		if text == "" {
			return nil
		}
		return []Activity{{
			Kind:      ActivityMessageChunk,
			SessionID: sessionID,
			Summary:   trim(text, 800),
			Meta:      map[string]any{"chunk_kind": head.Kind},
		}}
	case "tool_call", "tool_call_update":
		var call struct {
			ToolCallID string          `json:"toolCallId"`
			Title      string          `json:"title"`
			Kind       string          `json:"kind"`
			Status     string          `json:"status"`
			RawInput   json.RawMessage `json:"rawInput"`
		}
		if err := json.Unmarshal(env.Update, &call); err != nil {
			return nil
		}
		kind := ActivityToolCall
		if head.Kind == "tool_call_update" {
			kind = ActivityToolResult
		}
		// Mark filesystem writes with a distinct kind so the UI can highlight
		// self-evolve learnings.
		if call.Kind == "edit" || call.Kind == "write" {
			kind = ActivityFileWrite
		} else if call.Kind == "read" {
			kind = ActivityFileRead
		}
		return []Activity{{
			Kind:      kind,
			SessionID: sessionID,
			Tool:      firstNonEmpty(call.Title, call.Kind),
			Summary:   call.Title,
			Meta: map[string]any{
				"tool_call_id": call.ToolCallID,
				"status":       call.Status,
				"raw_input":    raw(call.RawInput, 400),
			},
		}}
	default:
		return nil
	}
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func raw(b json.RawMessage, n int) any {
	if len(b) == 0 {
		return nil
	}
	if len(b) <= n {
		return json.RawMessage(b)
	}
	return string(b[:n]) + "…"
}
