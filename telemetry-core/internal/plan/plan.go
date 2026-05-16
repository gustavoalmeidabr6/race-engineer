// Package plan implements the shared session-plan store. A session plan is
// a mutable list of intent entries (pit stops, compound changes, fuel
// targets, reminders, …) agreed between the driver and the agents. The
// store is the single source of truth that the brain ticker watches for
// lap-keyed reminders — when the car enters the lap of a planned pit stop
// the ticker emits an EventPlanReminder, and N metres before pit entry on
// that same lap an EventPlanImminent. The store is also the canonical
// answer to "what's the plan?" — consumed by CommsGate prompts,
// /api/brain/snapshot, and the Gemini Live agent's plan tools.
//
// One plan file lives per F1 session under <workspace>/sessions/
// sess_<uid>.plan.json. The store auto-rotates when the F1 session_uid
// changes, mirroring the transcript hub's session-aware behaviour.
//
// The schema is intentionally open — Payload is a free-form map so new
// kinds can be added without a code migration. Triggers are discriminated
// by Type so each kind can carry only the fields it needs.
package plan

import (
	"errors"
	"strings"
	"time"
)

// EntryKind is the closed set of plan-entry categories. Kinds are advisory
// labels for UI grouping and analyst reasoning — they do NOT change how
// triggers fire. Add new kinds freely; the ticker only cares about Trigger.
type EntryKind string

const (
	KindPitStop        EntryKind = "pit_stop"
	KindCompoundChange EntryKind = "compound_change"
	KindFuelTarget     EntryKind = "fuel_target"
	KindERSStrategy    EntryKind = "ers_strategy"
	KindDefensiveNote  EntryKind = "defensive_note"
	KindAttackWindow   EntryKind = "attack_window"
	KindReminder       EntryKind = "reminder"
)

// EntryStatus tracks lifecycle. `pending` = waiting for its trigger;
// `active` = trigger fired and the entry is currently in effect (e.g., a
// lap_window between from_lap and to_lap); `done` = trigger expired
// naturally; `cancelled` = explicitly cancelled by an actor.
type EntryStatus string

const (
	StatusPending   EntryStatus = "pending"
	StatusActive    EntryStatus = "active"
	StatusDone      EntryStatus = "done"
	StatusCancelled EntryStatus = "cancelled"
)

// TriggerType discriminates the Trigger union. Each type uses a different
// subset of the Trigger struct's fields; consumers must check Type before
// reading.
type TriggerType string

const (
	// TriggerLap fires once when the player car enters the given lap.
	// Uses: Lap.
	TriggerLap TriggerType = "lap"

	// TriggerLapWindow fires "open" on entering FromLap and "close" on
	// leaving ToLap (i.e., entering ToLap+1). Uses: FromLap, ToLap.
	TriggerLapWindow TriggerType = "lap_window"

	// TriggerTrackPosition fires when the player crosses DistanceM on
	// the given Lap. Used for spatial reminders ("pit entry on the right
	// in 400m"). Uses: Lap, DistanceM.
	TriggerTrackPosition TriggerType = "track_position"

	// TriggerTimeLeftSec fires once when SessionTimeLeft drops below
	// Seconds. Useful for end-of-session reminders. Uses: Seconds.
	TriggerTimeLeftSec TriggerType = "time_left_s"
)

// Trigger is a discriminated-union value describing when an entry should
// fire. The Type field names which other fields are meaningful.
//
// NOTE: only the fields named by Type are used by the ticker; unused
// fields are preserved through round-trip but ignored at evaluation time.
type Trigger struct {
	Type      TriggerType `json:"type"`
	Lap       int         `json:"lap,omitempty"`
	FromLap   int         `json:"from_lap,omitempty"`
	ToLap     int         `json:"to_lap,omitempty"`
	DistanceM float64     `json:"distance_m,omitempty"`
	Seconds   int         `json:"seconds,omitempty"`
}

// Validate returns an error if the trigger's fields are inconsistent with
// its Type. Called on every Add/Update so the store can't accumulate
// undeliverable entries.
func (t Trigger) Validate() error {
	switch t.Type {
	case TriggerLap:
		if t.Lap <= 0 {
			return errors.New("plan: trigger.lap must be > 0")
		}
	case TriggerLapWindow:
		if t.FromLap <= 0 || t.ToLap <= 0 {
			return errors.New("plan: trigger.from_lap and to_lap must be > 0")
		}
		if t.ToLap < t.FromLap {
			return errors.New("plan: trigger.to_lap must be >= from_lap")
		}
	case TriggerTrackPosition:
		if t.Lap <= 0 {
			return errors.New("plan: trigger.lap must be > 0")
		}
		// DistanceM == 0 is allowed: it signals "use the track-info
		// provider's pit-entry coordinate at fire time". Producers that
		// know the exact metres can set DistanceM > 0 and skip the
		// lookup. Validate stays permissive; the ticker silently skips
		// when neither a DistanceM nor a track resolver is available.
		if t.DistanceM < 0 {
			return errors.New("plan: trigger.distance_m must be >= 0")
		}
	case TriggerTimeLeftSec:
		if t.Seconds <= 0 {
			return errors.New("plan: trigger.seconds must be > 0")
		}
	case "":
		return errors.New("plan: trigger.type is required")
	default:
		return errors.New("plan: unknown trigger.type " + string(t.Type))
	}
	return nil
}

// PlanEntry is one item in the session plan. ID is assigned by the store
// on Add; callers should treat it as opaque. Payload is intentionally a
// loose map so new entry shapes (e.g., specific compound choice + lap
// count) can land without a schema migration.
type PlanEntry struct {
	ID        string         `json:"id"`
	Kind      EntryKind      `json:"kind"`
	Status    EntryStatus    `json:"status"`
	Trigger   Trigger        `json:"trigger"`
	Payload   map[string]any `json:"payload,omitempty"`
	Notes     string         `json:"notes,omitempty"`
	CreatedBy string         `json:"created_by"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`

	// FiredEvents records which downstream event kinds have already been
	// emitted for this entry, keyed by event-type-string. The ticker uses
	// this to avoid re-firing a reminder on every tick of the matching
	// lap. Empty map and nil map are equivalent.
	FiredEvents map[string]time.Time `json:"fired_events,omitempty"`
}

// Validate runs surface-level checks (kind, status, trigger, lengths).
// Returns the first violation found.
func (e PlanEntry) Validate() error {
	switch e.Kind {
	case KindPitStop, KindCompoundChange, KindFuelTarget, KindERSStrategy,
		KindDefensiveNote, KindAttackWindow, KindReminder:
	case "":
		return errors.New("plan: kind is required")
	default:
		// Unknown kinds are allowed — the schema is open by design — but
		// they must still be non-empty trimmed strings.
		if strings.TrimSpace(string(e.Kind)) == "" {
			return errors.New("plan: kind cannot be whitespace")
		}
	}
	switch e.Status {
	case StatusPending, StatusActive, StatusDone, StatusCancelled, "":
	default:
		return errors.New("plan: unknown status " + string(e.Status))
	}
	if err := e.Trigger.Validate(); err != nil {
		return err
	}
	if len(e.Notes) > 500 {
		return errors.New("plan: notes too long (>500 chars)")
	}
	return nil
}

// Plan is the full per-session document persisted to disk. Entries are
// ordered by CreatedAt ascending so the JSON file reads chronologically.
type Plan struct {
	SessionUID uint64      `json:"session_uid"`
	Entries    []PlanEntry `json:"entries"`
	CreatedAt  time.Time   `json:"created_at"`
	UpdatedAt  time.Time   `json:"updated_at"`
}

// FilterActive returns the subset of entries that aren't done or
// cancelled. Used by the snapshot markdown injector and the
// get_session_plan tool — the LLM doesn't need to wade through a long
// history of completed entries to decide what's next.
func (p *Plan) FilterActive() []PlanEntry {
	if p == nil {
		return nil
	}
	out := make([]PlanEntry, 0, len(p.Entries))
	for _, e := range p.Entries {
		if e.Status == StatusDone || e.Status == StatusCancelled {
			continue
		}
		out = append(out, e)
	}
	return out
}
