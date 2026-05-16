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
	EventTrafficUpdate  EventType = "traffic_update"  // P2 — unified spatial awareness: top 5 ahead + top 5 behind with closing rates. Fires when any car closes within ~5s TTI in either direction.
	EventPositionChange EventType = "position_change" // P3 — silent ground-truth: player Position number changed (any direction, any session). Voiced calls remain on EventOvertaken (P5, race only); this one is context for the Live agent.
	EventFuelCritical   EventType = "fuel_critical"   // P4 — voiced. FuelRemainingLaps below the critical threshold; driver may not finish the race on current fuel.
	EventFuelRate       EventType = "fuel_rate"       // P2 — silent context. Sustained high fuel burn rate over the trend window; LLM can mention if relevant.
	EventPlanReminder   EventType = "plan_reminder"   // P3 — entering the lap of a planned action (e.g., pit stop)
	EventPlanImminent   EventType = "plan_imminent"   // P4 — N metres before the spatial trigger of a planned action
	EventPlanWindowOpen EventType = "plan_window_open"  // P3 — lap_window trigger entered
	EventPlanWindowClose EventType = "plan_window_close" // P3 — lap_window trigger exited
	EventInfo           EventType = "info"            // P1 — generic analyst note
	// Setup-advisory events: silent (P2–P3) context the Live agent may voice
	// at its discretion. Sourced from the rule engine's setup_advice
	// producers and the pi-agent setup specialist. Each carries the current
	// setting value + suggested target in DebugData so the engineer can
	// phrase a concrete radio call ("BB 56% — try 54").
	EventSetupAdvice EventType = "setup_advice" // P3

	// Race-lifecycle events. Fired once per race by the insight engine off
	// CHQF / LGOT packet codes + lap math. Phase-transition events are
	// exempt from the "Recent Radio" dedup guard — the engineer MUST voice
	// "you won" even if a position_change for P1 was already spoken earlier.
	EventLightsOut    EventType = "lights_out"    // P5 — race start moment, voiced as urgent
	EventFinalLap     EventType = "final_lap"     // P4 — CurrentLap == TotalLaps rollover
	EventRaceFinished EventType = "race_finished" // P5 — CHQF observed, non-podium framing
	EventPodium       EventType = "podium"        // P5 — CHQF observed AND finished P1/P2/P3
)

// DefaultEventPriority returns the canonical priority for a given event type.
// Producers may override per-event but should rarely need to.
func DefaultEventPriority(t EventType) int {
	switch t {
	case EventRedFlag, EventBoxNow, EventOvertaken, EventLightsOut, EventRaceFinished, EventPodium:
		return 5
	case EventSafetyCar, EventVSC, EventYellowFlag, EventCollisionAhead, EventPlanImminent, EventFuelCritical, EventFinalLap:
		return 4
	case EventThreatOvertake, EventPitWindowOpen, EventTireCliff, EventWeatherChange, EventCornerCoaching,
		EventPlanReminder, EventPlanWindowOpen, EventPlanWindowClose, EventAnalystAnswer, EventPositionChange,
		EventSetupAdvice:
		return 3
	case EventLapSummary, EventSectorPB, EventFastestLap, EventTrafficUpdate, EventFuelRate:
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
//	queued        — sitting in the bus, eligible to be leased
//	in_flight     — leased to a dispatcher, awaiting delivery confirmation
//	awaiting_ack  — spoken once but RequiresAck=true; held in the queue
//	                until the driver verbally copies, ReapAwaitingAcks
//	                flips it back to queued (a re-nag), or MaxAckNags is
//	                exceeded.
//	delivered     — terminal; only ever set on the in-memory copy returned
//	                to callers. Delivered events are removed from the queue,
//	                so this status is rarely observed in PeekEvents.
type EventStatus string

const (
	StatusQueued       EventStatus = "queued"
	StatusInFlight     EventStatus = "in_flight"
	StatusAwaitingAck  EventStatus = "awaiting_ack"
	StatusDelivered    EventStatus = "delivered"
)

// AckNagAfter is the quiet-period before an awaiting-ack event auto-renags.
// MaxAckNags caps total redeliveries (the original speak counts as #1, so
// MaxAckNags=2 means: speak → silence → renag → silence → drop).
const (
	AckNagAfter = 20 * time.Second
	MaxAckNags  = 2
)

// DefaultRequiresAck returns the producer-default for whether the event
// type should be held in the queue until the driver acknowledges it.
// Producers may override per-event but should rarely need to.
//
// Require ack: tactical calls the driver MUST hear and act on
// (pit calls, planned-action reminders, tire-cliff alerts).
//
// No ack needed: cheap, frequent, or already-known facts (lap summaries,
// traffic_update, fastest_lap, generic info).
func DefaultRequiresAck(t EventType) bool {
	switch t {
	case EventBoxNow,
		EventPlanReminder,
		EventPlanImminent,
		EventPlanWindowOpen,
		EventTireCliff:
		return true
	default:
		return false
	}
}

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

	// RequiresAck — producer-set. When true, AckDelivered does NOT remove
	// the event; it flips to StatusAwaitingAck and waits for the driver to
	// verbally copy (via ConfirmAck) before discharge. ReapAwaitingAcks
	// renags after AckNagAfter; MaxAckNags caps total redeliveries.
	RequiresAck bool `json:"requires_ack,omitempty"`

	// AwaitingAckSince is the time the event entered StatusAwaitingAck.
	// Used by ReapAwaitingAcks to decide when to renag.
	AwaitingAckSince time.Time `json:"awaiting_ack_since,omitempty"`

	// NagCount is the number of re-emits while awaiting copy. Capped by
	// MaxAckNags. Reset is implicit — once acked/dropped the event leaves
	// the queue entirely.
	NagCount int `json:"nag_count,omitempty"`
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

// Well-known nack reasons. The bus uses these to identify failures where
// retrying immediately into the same dead consumer is pointless — those
// short-circuit the per-priority retry budget so we don't burn 3 attempts
// in a single second while the consumer reconnects.
const (
	NackReasonSendClientContentFailed = "send_client_content_failed"
	NackReasonNoConsumer              = "no_consumer"
	NackReasonSessionDead             = "session_dead"
)

// isDeadConsumerReason returns true when the nack reason indicates the
// downstream is gone, not a transient delivery failure. The bus collapses
// these to an immediate drop so the queue doesn't churn during reconnect.
func isDeadConsumerReason(reason string) bool {
	switch reason {
	case NackReasonSendClientContentFailed,
		NackReasonNoConsumer,
		NackReasonSessionDead:
		return true
	default:
		return false
	}
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
	case EventTrafficUpdate:
		return 15 * time.Second
	case EventThreatOvertake, EventCollisionAhead:
		return 30 * time.Second
	case EventLapSummary, EventSectorPB, EventFastestLap:
		return 90 * time.Second
	case EventOvertaken:
		return 30 * time.Second
	case EventPositionChange:
		// Silent context event — the position number is only useful while
		// it's current. After 30s a stale "you moved to P4" call would
		// confuse the model if positions have shifted again.
		return 30 * time.Second
	case EventFuelCritical:
		// Fuel state changes slowly but a stale "critical fuel" call after
		// 60s is misleading if the driver already started saving and
		// recovered the margin.
		return 60 * time.Second
	case EventFuelRate:
		// Burn-rate trend is a 25s-window observation; double that as the
		// useful shelf life.
		return 60 * time.Second
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
