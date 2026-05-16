package brain

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// EventType is the closed set of proactive-radio event categories. Each type
// carries a default priority (see DefaultEventPriority) but producers can
// override per-event when context warrants. Priorities define dispatch
// behaviour, not the type itself.
type EventType string

const (
	EventRedFlag        EventType = "red_flag"        // P5 — derived from session state
	EventBoxNow         EventType = "box_now"         // P5 — analyst-initiated only
	EventOvertaken      EventType = "overtaken"       // P5 — Position number went UP this tick (we got passed)
	EventSafetyCar      EventType = "safety_car"      // P4 — SafetyCarStatus transition
	EventVSC            EventType = "vsc"             // P4
	EventYellowFlag     EventType = "yellow_flag"     // P4 — sector-flag transition
	EventCollisionAhead EventType = "collision_ahead" // P4 — COLL nearby
	EventThreatOvertake EventType = "threat_overtake" // P3-P4 — gap-behind shrinking fast
	EventPitWindowOpen  EventType = "pit_window_open" // P3
	EventTireCliff      EventType = "tire_cliff"      // P3 — analyst-initiated
	EventWeatherChange  EventType = "weather_change"  // P3
	EventLapSummary     EventType = "lap_summary"     // P2
	EventSectorPB       EventType = "sector_pb"       // P2
	EventFastestLap     EventType = "fastest_lap"     // P2 — FTLP packet
	EventCornerCoaching EventType = "corner_coaching" // P3 — agent-set track-position reminder fired
	EventAnalystAnswer  EventType = "analyst_answer"  // P3 (P4 urgent) — pi_agent answered a fire-and-forget ask_data_analyst query
	EventCarApproaching EventType = "car_approaching" // P2 — courtesy heads-up: car behind closing on track distance
	EventPlanReminder   EventType = "plan_reminder"   // P3 — entering the lap of a planned action (e.g., pit stop)
	EventPlanImminent   EventType = "plan_imminent"   // P4 — N metres before the spatial trigger of a planned action
	EventPlanWindowOpen EventType = "plan_window_open"  // P3 — lap_window trigger entered
	EventPlanWindowClose EventType = "plan_window_close" // P3 — lap_window trigger exited
	EventInfo           EventType = "info"            // P1 — generic analyst note
)

// DefaultEventPriority returns the canonical priority for a given event type.
// Producers may override per-event but should rarely need to.
func DefaultEventPriority(t EventType) int {
	switch t {
	case EventRedFlag, EventBoxNow, EventOvertaken:
		return 5
	case EventSafetyCar, EventVSC, EventYellowFlag, EventCollisionAhead, EventPlanImminent:
		return 4
	case EventThreatOvertake, EventPitWindowOpen, EventTireCliff, EventWeatherChange, EventCornerCoaching,
		EventPlanReminder, EventPlanWindowOpen, EventPlanWindowClose, EventAnalystAnswer:
		return 3
	case EventLapSummary, EventSectorPB, EventFastestLap, EventCarApproaching:
		return 2
	case EventInfo:
		return 1
	default:
		return 1
	}
}

// AnalystAllowedEventTypes is the whitelist the analyst LLM may emit through
// its `interrupt_engineer` tool. Game-state-derived types (red_flag,
// safety_car, yellow_flag, collision_ahead, fastest_lap) come from packet
// producers only — the analyst can't fabricate them.
var AnalystAllowedEventTypes = map[EventType]struct{}{
	EventBoxNow:         {},
	EventThreatOvertake: {},
	EventPitWindowOpen:  {},
	EventTireCliff:      {},
	EventWeatherChange:  {},
	EventSectorPB:       {},
	EventLapSummary:     {},
	EventInfo:           {},
	EventAnalystAnswer:  {},
}

// Length caps for producer-supplied fields. Enforced at enqueue time so the
// Live agent's prompt template can't blow up its context window.
const (
	MaxEventSummaryChars   = 80
	MaxEventReasoningChars = 200
	// MaxAnalystReasoningChars relaxes the cap for analyst_answer events:
	// pi_agent targets ≤3 complete sentences (~250–400 chars), which would
	// otherwise blow through the 200-char default. The model still gets the
	// full sentence in Reasoning + a truncated headline in Summary.
	MaxAnalystReasoningChars = 500
	MaxEventDebugBytes       = 1024
)

// maxReasoningCharsFor returns the per-type Reasoning length cap.
// Analyst answers get a relaxed cap because pi_agent's contract is "≤3
// complete sentences" — well over the 200-char default.
func maxReasoningCharsFor(t EventType) int {
	if t == EventAnalystAnswer {
		return MaxAnalystReasoningChars
	}
	return MaxEventReasoningChars
}

// EventStatus tracks the lifecycle of an event in the queue.
//
//	queued     — sitting in the bus, eligible to be leased
//	in_flight  — leased to a dispatcher, awaiting delivery confirmation
//	delivered  — terminal; only ever set on the in-memory copy returned to
//	             callers. Delivered events are removed from the queue, so
//	             this status is rarely observed in PeekEvents.
type EventStatus string

const (
	StatusQueued    EventStatus = "queued"
	StatusInFlight  EventStatus = "in_flight"
	StatusDelivered EventStatus = "delivered"
)

// Event is one item on the Interrupts Bus. Producers fill summary, reasoning,
// and debug_data with FACTS — never radio sentences. The Live agent (Gemini)
// reads all three and synthesises the radio call in its own voice.
//
// Lifecycle fields (Status / InFlightAt / Attempts / LastNackAt) are managed
// by the bus — producers don't set them. StaleAfter, when zero, falls back to
// DefaultStaleAfter(Type) at lease time so producers can opt into the type's
// default TTL by leaving it unset.
type Event struct {
	ID        string         `json:"id"`
	At        time.Time      `json:"at"`
	Type      EventType      `json:"type"`
	Priority  int            `json:"priority"` // 1–5
	Summary   string         `json:"summary"`
	Reasoning string         `json:"reasoning,omitempty"`
	DebugData map[string]any `json:"debug_data,omitempty"`
	Source    string         `json:"source,omitempty"`
	DedupKey  string         `json:"dedup_key,omitempty"`

	// DedupSubject is an opaque tag (e.g. "car:5") that names what the event
	// is *about*. When the driver has just been told something about the
	// same subject — even via a different event type / DedupKey — the bus
	// rejects the new event. Lets closing_threat and car_approaching share
	// a per-driver cooldown without needing matched DedupKeys.
	DedupSubject string `json:"dedup_subject,omitempty"`

	Status     EventStatus   `json:"status,omitempty"`
	InFlightAt time.Time     `json:"in_flight_at,omitempty"`
	Attempts   int           `json:"attempts,omitempty"`
	LastNackAt time.Time     `json:"last_nack_at,omitempty"`
	StaleAfter time.Duration `json:"stale_after,omitempty"`
}

// Validate checks the producer-supplied fields against the contract caps.
// Returns the first violation found, or nil if the event is well-formed.
// Does NOT enforce TalkLevel or dedup — those are queue-level decisions.
func (e Event) Validate() error {
	if e.Type == "" {
		return errors.New("event: type is required")
	}
	if e.Priority < 1 || e.Priority > 5 {
		return fmt.Errorf("event: priority %d out of range [1,5]", e.Priority)
	}
	if strings.TrimSpace(e.Summary) == "" {
		return errors.New("event: summary is required")
	}
	if len(e.Summary) > MaxEventSummaryChars {
		return fmt.Errorf("event: summary length %d > %d", len(e.Summary), MaxEventSummaryChars)
	}
	if cap := maxReasoningCharsFor(e.Type); len(e.Reasoning) > cap {
		return fmt.Errorf("event: reasoning length %d > %d", len(e.Reasoning), cap)
	}
	// box_now must be priority 5 — the type's whole reason for existing is
	// "interrupt the model right now."
	if e.Type == EventBoxNow && e.Priority != 5 {
		return fmt.Errorf("event: box_now must be priority 5, got %d", e.Priority)
	}
	return nil
}

// MinPriorityForTalkLevel maps the dashboard's TalkLevel (1–10) to a priority
// floor on the bus. Events below the floor are dropped at enqueue time so a
// quiet driver only hears what genuinely matters.
//
// Lap summaries, sector PBs and fastest-lap notifications are P2 — they are
// core race-engineer behaviour the user expects on the default setting, so
// the default TalkLevel (5) sits in the P2-floor band, not P3.
//
//	TalkLevel 1–2  → P5 only (critical only)
//	TalkLevel 3–4  → P4+   (critical + urgent flags)
//	TalkLevel 5–7  → P2+   (default — important + debriefs / fastest-lap)
//	TalkLevel 8–10 → P1+   (chatty — every analyst note)
func MinPriorityForTalkLevel(t int32) int {
	switch {
	case t <= 2:
		return 5
	case t <= 4:
		return 4
	case t <= 7:
		return 2
	default:
		return 1
	}
}

// Retry caps. Urgent events get more attempts because they matter more; we
// give up eventually so a wedged dispatcher or persistently-talking driver
// can't trap an event in the queue forever.
const (
	MaxAttemptsUrgent    = 5 // P4–P5
	MaxAttemptsNonUrgent = 3 // P1–P3
)

// MaxAttemptsForPriority returns the retry cap for a given priority. Used by
// the bus when deciding whether a NackInterrupted should re-queue or drop.
func MaxAttemptsForPriority(p int) int {
	if p >= 4 {
		return MaxAttemptsUrgent
	}
	return MaxAttemptsNonUrgent
}

// DefaultStaleAfter returns the per-type TTL applied at lease time when the
// event's StaleAfter is zero. Time-sensitive events (lap_summary, proximity,
// closing threats) expire so a delayed retry doesn't say something that's
// already irrelevant. Critical events (red_flag, box_now, safety_car) never
// expire — they must be delivered.
func DefaultStaleAfter(t EventType) time.Duration {
	switch t {
	case EventBoxNow, EventRedFlag, EventSafetyCar, EventVSC, EventYellowFlag:
		return 0
	case EventCarApproaching:
		return 15 * time.Second
	case EventThreatOvertake, EventCollisionAhead:
		return 30 * time.Second
	case EventLapSummary, EventSectorPB, EventFastestLap:
		return 90 * time.Second
	case EventOvertaken:
		return 30 * time.Second
	case EventPlanImminent:
		// Pit-entry warning is only useful for the few seconds before the
		// driver crosses the entry point. After that the moment has
		// passed and the radio call would be confusing.
		return 20 * time.Second
	case EventPlanReminder, EventPlanWindowOpen, EventPlanWindowClose:
		// Lap-keyed reminders are useful for the full lap they belong
		// to. 60s gives a few flying-lap retries on slow circuits.
		return 60 * time.Second
	default:
		return 60 * time.Second
	}
}

// newEventID returns "evt_<8-char-hex>" using crypto/rand. Falls back to a
// time-based id if rand is unavailable (should never happen in practice).
func newEventID() string {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("evt_%08x", time.Now().UnixNano()&0xffffffff)
	}
	return "evt_" + hex.EncodeToString(buf[:])
}
