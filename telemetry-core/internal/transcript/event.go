// Package transcript records a per-session interaction log of every
// LLM-touching surface in the race engineer: driver utterances, engineer
// radio replies, analyst LLM round-trips, tool calls, insight pushes, and
// the lifecycle of every Interrupts-Bus event.
//
// The log is append-only JSONL on disk under workspace/sessions/<id>.jsonl
// and an in-memory ring of recent events for low-latency prompt injection.
// See Hub for the lifecycle (rotation on F1 session_uid change, retention,
// HTTP surface).
package transcript

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Kind is the closed enum of transcript-event categories. Closed so
// consumers can filter by kind without string-matching mistakes.
type Kind string

const (
	// User said something — driver query via PTT, transcribed by STT.
	KindUserUtterance Kind = "user_utterance"

	// Engineer-side text intended for radio output. Source can be the
	// CommsGate translator, a strategy webhook, or a Gemini Live event
	// dispatch — actor in the meta clarifies which.
	KindEngineerSpeech Kind = "engineer_speech"

	// LLM tool call observed during a session (e.g. Gemini Live calls
	// get_race_state or ask_data_analyst). text = tool name; meta carries
	// arguments and latency.
	KindToolCall Kind = "tool_call"
	// Tool's response back to the LLM. meta carries the structured result
	// (truncated if oversized).
	KindToolResult Kind = "tool_result"

	// Strategy analyst question (input prompt) and answer (LLM output).
	KindAnalystQuery  Kind = "analyst_query"
	KindAnalystAnswer Kind = "analyst_answer"

	// New insight pushed via /api/strategy or rule-engine emit.
	KindInsightPushed Kind = "insight_pushed"

	// Interrupts-bus event lifecycle. Dispatched when EnqueueEvent
	// accepts; delivered on a successful AckDelivered; dropped on a final
	// NackInterrupted past the retry cap.
	KindEventDispatched   Kind = "event_dispatched"
	KindEventDelivered    Kind = "event_delivered"
	KindEventDropped      Kind = "event_dropped"
	// Awaiting-ack lifecycle: an event was spoken but RequiresAck=true is
	// keeping it in the queue. Acked closes the lifecycle once the driver
	// copies; nagged is emitted each time the bus re-renders the event
	// because no copy arrived inside AckNagAfter.
	KindEventAwaitingAck  Kind = "event_awaiting_ack"
	KindEventAcked        Kind = "event_acked"
	KindEventNagged       Kind = "event_nagged"

	// Zerolog stdout mirror. The transcript writer fans every log line
	// through Append so the Live Debug tab sees the same stream as the
	// terminal. KindLogDebug is gated by the server's LOG_LEVEL.
	KindLogDebug Kind = "log_debug"
	KindLogInfo  Kind = "log_info"
	KindLogWarn  Kind = "log_warn"
	KindLogError Kind = "log_error"

	// WebSocket lifecycle. Emitted by the dashboard /ws hub on each
	// client register/unregister. Meta carries remote_addr + path.
	KindWSConnected    Kind = "ws_connected"
	KindWSDisconnected Kind = "ws_disconnected"
)

// AllKinds enumerates every closed Kind constant. Useful for HTTP filter
// validation and tool docstrings.
var AllKinds = []Kind{
	KindUserUtterance,
	KindEngineerSpeech,
	KindToolCall,
	KindToolResult,
	KindAnalystQuery,
	KindAnalystAnswer,
	KindInsightPushed,
	KindEventDispatched,
	KindEventDelivered,
	KindEventDropped,
	KindEventAwaitingAck,
	KindEventAcked,
	KindEventNagged,
	KindLogDebug,
	KindLogInfo,
	KindLogWarn,
	KindLogError,
	KindWSConnected,
	KindWSDisconnected,
}

// IsKnown reports whether k is one of the closed Kind constants.
func IsKnown(k Kind) bool {
	for _, v := range AllKinds {
		if v == k {
			return true
		}
	}
	return false
}

// Event is one transcript line. Wire-compatible JSON: at, session_id,
// kind, actor, text, meta. Meta is free-form structured payload — keep
// JSON-serialisable and reasonably small (validated by the hub).
type Event struct {
	At        time.Time      `json:"at"`
	SessionID string         `json:"session_id"`
	Kind      Kind           `json:"kind"`
	Actor     string         `json:"actor"`
	Text      string         `json:"text"`
	Meta      map[string]any `json:"meta,omitempty"`
}

// MaxTextChars is the hard limit on Event.Text — prevents a runaway tool
// result from blowing up a session file or a prompt context window.
// Producers SHOULD truncate themselves; the hub also enforces.
const MaxTextChars = 4096

// MaxMetaBytes is the post-serialise cap on Event.Meta. Same reasoning as
// MaxTextChars; cheap to compute since meta is small in the common case.
const MaxMetaBytes = 4096

// Validate checks the producer-supplied fields. Returns the first
// violation. Does not enforce truncation — the hub does that on append
// so producers can shovel raw payloads in without pre-checking.
func (e Event) Validate() error {
	if e.Kind == "" {
		return fmt.Errorf("transcript: kind is required")
	}
	if !IsKnown(e.Kind) {
		return fmt.Errorf("transcript: unknown kind %q", e.Kind)
	}
	if strings.TrimSpace(e.Actor) == "" {
		return fmt.Errorf("transcript: actor is required")
	}
	return nil
}

// Marshal returns the JSONL line (without trailing newline) for e.
func (e Event) Marshal() ([]byte, error) {
	return json.Marshal(e)
}
