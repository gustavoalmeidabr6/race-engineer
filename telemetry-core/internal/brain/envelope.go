// Package brain is the shared blackboard that specialist agents write to and
// the conversational engineer (CommsGate) reads from. Multiple producers can
// update the brain concurrently; reads return immutable filtered snapshots.
package brain

import "time"

// SourceRef explains where an observation came from. Useful for explainability
// and for the LLM to weigh confidence ("which lap did we measure this on?").
type SourceRef struct {
	Lap       int       `json:"lap,omitempty"`
	Query     string    `json:"query,omitempty"`
	Packet    string    `json:"packet,omitempty"`
	Timestamp time.Time `json:"timestamp,omitempty"`
}

// Observation is a finding written by an agent. Routed to L2 (per-topic ring)
// when Hypothesis is false, or to L3 (latest-wins per topic) when true.
//
// Summary is the human/LLM-facing one-liner. Body carries optional structured
// data for programmatic consumers — keep it JSON-serializable.
//
// JobID and Urgent are populated by the async analyst pipeline so the
// observation can be correlated with the job that produced it and so urgent
// findings can be marked distinctly when serialized.
type Observation struct {
	Topic      Topic         `json:"topic"`
	Agent      string        `json:"agent"`
	At         time.Time     `json:"at"`
	TTL        time.Duration `json:"ttl"`
	Confidence float64       `json:"confidence"`
	Summary    string        `json:"summary"`
	Body       any           `json:"body,omitempty"`
	Source     SourceRef     `json:"source,omitempty"`
	Hypothesis bool          `json:"hypothesis,omitempty"`
	JobID      string        `json:"job_id,omitempty"`
	Urgent     bool          `json:"urgent,omitempty"`
}

// PendingJob describes an in-flight analyst query that has not yet produced
// an observation. Tracked on the brain so callers (debug endpoints, the
// markdown snapshot) can see what the analyst is currently working on.
type PendingJob struct {
	JobID        string    `json:"job_id"`
	Question     string    `json:"question"`
	ContextTopic string    `json:"context_topic"`
	StartedAt    time.Time `json:"started_at"`
}

// Expired reports whether the observation has lived past its TTL relative to now.
func (o Observation) Expired(now time.Time) bool {
	if o.TTL <= 0 {
		return false
	}
	return now.Sub(o.At) > o.TTL
}

// RaceEvent is a discrete external event (yellow flag, SC, weather change, etc.).
// Sourced from ingestion or rule engine — not from analyst reasoning.
type RaceEvent struct {
	At         time.Time `json:"at"`
	Code       string    `json:"code"`
	Detail     string    `json:"detail,omitempty"`
	VehicleIdx int       `json:"vehicle_idx,omitempty"`
}

// SpeechRecord captures a radio message we already sent to the driver.
// Used at decision time to avoid repeating ourselves.
//
// Subject is an optional opaque tag (e.g. "car:5") propagated from the
// originating Event's DedupSubject. EnqueueEvent uses it to suppress
// follow-up events about the same subject across rule families — so the
// closing_threat call about Charles also gates an EventTrafficUpdate for
// Charles a few seconds later.
type SpeechRecord struct {
	At       time.Time `json:"at"`
	Text     string    `json:"text"`
	Priority int       `json:"priority"`
	Subject  string    `json:"subject,omitempty"`
}

// DriverEvent is something the driver said or asked. Kind is one of
// "query", "complaint", "ack" — agents may add new kinds as needed.
type DriverEvent struct {
	At   time.Time `json:"at"`
	Text string    `json:"text"`
	Kind string    `json:"kind"`
}
