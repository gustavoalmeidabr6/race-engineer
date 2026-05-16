package insights

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/brain"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
)

// corner_reminders.go — driver-coaching reminders set by the Live agent.
//
// Flow:
//   1. Driver says "I'm struggling at T12".
//   2. Agent calls set_corner_reminder → POST /api/reminders/corner.
//   3. Reminder stored in ReminderStore with a frozen trigger distance.
//   4. CornerWatcher (200 ms ticker) reads RaceState.LapDistance and fires
//      EventCornerCoaching through the bus when the player crosses the
//      effective trigger point on a fresh lap.
//
// The watcher is a separate goroutine from the 5 s rule engine because at
// 300 km/h the player covers ~83 m/s — anything coarser than ~250 ms is
// useless for sub-100 m corner cues.
//
// Reminders are session-scoped (in memory only) — restart drops them.

// MaxReminders caps the in-flight reminders to prevent runaway agent calls.
const MaxReminders = 20

// FireWindowM is the window in metres past the effective trigger inside
// which a reminder fires. 100m at 200ms tick × 80 m/s = ~5 ticks of overlap,
// so missed fires are vanishingly rare even with sharp deceleration.
const FireWindowM = 100

// FallbackSpeedMps is the speed assumed when computing lookahead distance
// from a time-based lookahead if the player is currently slow (e.g., in
// pits, parked). 80 m/s ~= 290 km/h, a reasonable F1 cruising speed.
const FallbackSpeedMps = 80.0

// MinLookaheadM is the floor for any computed lookahead. Below ~30m the
// driver has no time to react.
const MinLookaheadM = 30.0

// MaxLookaheadM is the ceiling. Beyond this the cue arrives so early it
// loses its connection to the corner.
const MaxLookaheadM = 400.0

// Reminder is one in-flight driver-coaching cue. Two trigger kinds are
// supported: position-based (fires when the player crosses
// EffectiveDistM on a fresh lap — works for corners, pit entry, DRS
// zones, sector boundaries, anywhere on the lap with a known distance)
// and lap-based (fires once when the player enters TriggerAtLap — for
// "remind me to pit on lap 15" style cues).
//
// Exactly one of {TriggerDistM > 0, TriggerAtLap > 0} should be set;
// the API handler enforces that.
type Reminder struct {
	ID              string    `json:"id"`
	CornerID        string    `json:"corner_id,omitempty"`     // optional display label ("T12")
	CornerName      string    `json:"corner_name,omitempty"`   // optional display label ("Stowe")
	Label           string    `json:"label,omitempty"`         // freeform display label for non-corner reminders ("pit entry", "DRS zone 2")
	TriggerDistM    float32   `json:"trigger_dist_m"`          // metres into lap where the cue should land (0 when lap-based)
	LookaheadM      float32   `json:"lookahead_m"`             // distance before trigger to fire (position-based only)
	EffectiveDistM  float32   `json:"effective_dist_m"`        // = max(0, TriggerDistM - LookaheadM)
	TriggerAtLap    uint8     `json:"trigger_at_lap,omitempty"`// lap-based trigger: fire when currentLap >= this. 0 = position-based.
	Message         string    `json:"message"`
	Priority        int       `json:"priority"`                // 1-5, default 3
	Recurring       bool      `json:"recurring"`
	// RequiresAck — when true, the fired corner_coaching event is held in
	// the brain Interrupts Bus under StatusAwaitingAck until the driver
	// verbally copies (see brain.RequiresAck). Used for safety-critical
	// cues the engineer should re-nag for. Defaults false (most coaching
	// reminders are advisory).
	RequiresAck     bool      `json:"requires_ack,omitempty"`
	ExpiresAtLap    uint8     `json:"expires_at_lap"`
	LastFiredLap    int16     `json:"last_fired_lap"`          // -1 = never
	CreatedAt       time.Time `json:"created_at"`
	CreatedAtLap    uint8     `json:"created_at_lap"`
}

// ReminderStore is the thread-safe collection used by both the API
// handlers (writers) and the watcher (reader).
type ReminderStore struct {
	mu        sync.Mutex
	reminders []*Reminder
}

// NewReminderStore returns a ready-to-use store.
func NewReminderStore() *ReminderStore {
	return &ReminderStore{reminders: make([]*Reminder, 0, MaxReminders)}
}

// ErrReminderStoreFull is returned by Add when the store would exceed
// MaxReminders.
var ErrReminderStoreFull = errors.New("reminder store full")

// Add inserts a reminder. Returns the inserted entry (with ID populated)
// or ErrReminderStoreFull. It auto-dedupes by (CornerID, Message): if an
// identical pair already exists, the existing entry is returned without
// allocating a new one.
func (s *ReminderStore) Add(r Reminder) (*Reminder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.reminders {
		if existing.CornerID != "" && existing.CornerID == r.CornerID && existing.Message == r.Message {
			return existing, nil
		}
	}
	if len(s.reminders) >= MaxReminders {
		return nil, ErrReminderStoreFull
	}
	if r.ID == "" {
		r.ID = newReminderID()
	}
	if r.LastFiredLap == 0 {
		r.LastFiredLap = -1
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}
	stored := r
	s.reminders = append(s.reminders, &stored)
	return &stored, nil
}

// Cancel removes a reminder by id. Returns true if it existed.
func (s *ReminderStore) Cancel(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, r := range s.reminders {
		if r.ID == id {
			s.reminders = append(s.reminders[:i], s.reminders[i+1:]...)
			return true
		}
	}
	return false
}

// List returns a snapshot of all live reminders.
func (s *ReminderStore) List() []*Reminder {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Reminder, len(s.reminders))
	for i, r := range s.reminders {
		cp := *r
		out[i] = &cp
	}
	return out
}

// Clear empties the store. Used on session boundary or for tests.
func (s *ReminderStore) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reminders = s.reminders[:0]
}

// poll returns reminders that should fire NOW given the player's current
// lap_distance and current lap. It mutates each fired reminder's
// LastFiredLap. Reminders past their expiry are removed in-place.
//
// Three firing modes (driven by which fields are set):
//   - Position-only (TriggerDistM > 0, TriggerAtLap == 0):
//       lapDist ∈ [EffectiveDistM, EffectiveDistM + FireWindowM)
//       and LastFiredLap != currentLap. Repeats every lap when Recurring.
//   - Lap-only (TriggerDistM == 0, TriggerAtLap > 0):
//       fires once when currentLap >= TriggerAtLap and LastFiredLap == -1.
//       Always one-shot (repeating "lap N" makes no sense).
//   - Lap+position combined (both > 0):
//       fires once when currentLap == TriggerAtLap and lapDist is inside
//       the fire window. Use for "remind me at T8 ON lap 5" style cues.
//       Always one-shot.
func (s *ReminderStore) poll(currentLap uint8, lapDist float32) []*Reminder {
	s.mu.Lock()
	defer s.mu.Unlock()
	var firing []*Reminder
	keep := s.reminders[:0]
	for _, r := range s.reminders {
		// Expire — drop without firing.
		if r.ExpiresAtLap > 0 && currentLap > r.ExpiresAtLap {
			continue
		}
		keep = append(keep, r)

		// Combined trigger: position-on-specific-lap. Fires once.
		if r.TriggerAtLap > 0 && r.TriggerDistM > 0 {
			if r.LastFiredLap != -1 {
				continue
			}
			// Wait for the target lap. If we somehow slip past it (e.g.
			// player skipped a tick across S/F), the next-lap fallback
			// below still fires within the position window so the cue
			// isn't lost; otherwise expiry drops it.
			if currentLap < r.TriggerAtLap {
				continue
			}
			dist := lapDist - r.EffectiveDistM
			if dist < 0 || dist > FireWindowM {
				continue
			}
			r.LastFiredLap = int16(currentLap)
			keep = keep[:len(keep)-1] // one-shot
			copied := *r
			firing = append(firing, &copied)
			continue
		}

		if r.TriggerAtLap > 0 {
			// Lap-only, one-shot. Wait until the target lap (or beyond,
			// in case we missed the exact crossing tick) and fire once.
			if r.LastFiredLap != -1 || currentLap < r.TriggerAtLap {
				continue
			}
			r.LastFiredLap = int16(currentLap)
			keep = keep[:len(keep)-1] // one-shot
			copied := *r
			firing = append(firing, &copied)
			continue
		}

		// Position-only.
		dist := lapDist - r.EffectiveDistM
		if dist < 0 || dist > FireWindowM {
			continue
		}
		if int16(currentLap) == r.LastFiredLap {
			continue
		}
		r.LastFiredLap = int16(currentLap)
		// Non-recurring → also drop after firing.
		if !r.Recurring {
			keep = keep[:len(keep)-1]
		}
		// Copy for return so callers don't race against the mutex.
		copied := *r
		firing = append(firing, &copied)
	}
	s.reminders = keep
	return firing
}

// ComputeLookaheadM resolves a time-based lookahead (seconds) to metres
// using the player's current speed. Falls back to FallbackSpeedMps when
// the player is slow, and clamps to [MinLookaheadM, MaxLookaheadM].
func ComputeLookaheadM(seconds float32, speedKmh uint16) float32 {
	speedMps := float32(speedKmh) / 3.6
	if speedMps < 30 {
		speedMps = FallbackSpeedMps
	}
	m := seconds * speedMps
	if m < MinLookaheadM {
		m = MinLookaheadM
	}
	if m > MaxLookaheadM {
		m = MaxLookaheadM
	}
	return m
}

func newReminderID() string {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("rmd_%08x", time.Now().UnixNano()&0xffffffff)
	}
	return "rmd_" + hex.EncodeToString(buf[:])
}

// CornerWatcher polls a ReminderStore at high frequency against the
// player's RaceState and emits EventCornerCoaching events through the bus.
type CornerWatcher struct {
	store     *ReminderStore
	state     func() *models.RaceState
	bus       *brain.RaceBrain
	talkLevel func() int32
	interval  time.Duration
}

// NewCornerWatcher wires the watcher. interval defaults to 200ms when zero.
func NewCornerWatcher(
	store *ReminderStore,
	state func() *models.RaceState,
	bus *brain.RaceBrain,
	talkLevel func() int32,
	interval time.Duration,
) *CornerWatcher {
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}
	return &CornerWatcher{
		store:     store,
		state:     state,
		bus:       bus,
		talkLevel: talkLevel,
		interval:  interval,
	}
}

// Run blocks until ctx is cancelled, polling the store on every tick.
func (w *CornerWatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	log.Info().Dur("interval", w.interval).Msg("Corner reminder watcher started")
	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("Corner reminder watcher stopping")
			return
		case <-ticker.C:
			w.tick()
		}
	}
}

// tick — one watcher cycle. Exported via the type for test access.
func (w *CornerWatcher) tick() {
	if w.store == nil || w.state == nil || w.bus == nil {
		return
	}
	state := w.state()
	if state == nil {
		return
	}
	if state.TrackLength == 0 || state.CurrentLap == 0 {
		return
	}
	// Skip pits / in-out laps — gap data is noise and the driver has bigger
	// problems than corner coaching.
	if state.PitStatus != 0 {
		return
	}
	if state.DriverStatus == 0 || state.DriverStatus == 2 || state.DriverStatus == 3 {
		return
	}
	firing := w.store.poll(state.CurrentLap, state.LapDistance)
	talkLevel := int32(5)
	if w.talkLevel != nil {
		talkLevel = w.talkLevel()
	}
	for _, r := range firing {
		ev := buildCornerCoachingEvent(r, state)
		if _, err := w.bus.EnqueueEvent(ev, talkLevel); err != nil {
			if errors.Is(err, brain.ErrEventDeduped) || errors.Is(err, brain.ErrEventTalkLevel) {
				log.Debug().Str("reminder", r.ID).Err(err).Msg("Corner coaching dropped")
				continue
			}
			log.Warn().Err(err).Str("reminder", r.ID).Msg("Corner coaching enqueue failed")
		}
	}
}

func buildCornerCoachingEvent(r *Reminder, state *models.RaceState) brain.Event {
	// Build a human label that adapts to whichever trigger kind the
	// reminder uses. Falls back through corner_id → freeform label →
	// raw lap distance → "lap N". When a position trigger combines with
	// at_lap, the lap number is appended so logs/observations clearly
	// show "T8 on lap 5".
	var label string
	switch {
	case r.CornerID != "":
		label = r.CornerID
		if r.CornerName != "" && r.CornerName != r.CornerID {
			label = label + " " + r.CornerName
		}
	case r.Label != "":
		label = r.Label
	case r.TriggerAtLap > 0 && r.TriggerDistM == 0:
		label = fmt.Sprintf("lap %d", r.TriggerAtLap)
	case r.TriggerDistM > 0:
		label = fmt.Sprintf("%.0fm", r.TriggerDistM)
	default:
		label = "reminder"
	}
	if r.TriggerAtLap > 0 && r.TriggerDistM > 0 {
		label = label + fmt.Sprintf(" on lap %d", r.TriggerAtLap)
	}
	priority := r.Priority
	if priority < 1 || priority > 5 {
		priority = brain.DefaultEventPriority(brain.EventCornerCoaching)
	}
	summary := r.Message
	if len(summary) > brain.MaxEventSummaryChars {
		summary = summary[:brain.MaxEventSummaryChars]
	}
	var reasoning string
	if r.TriggerAtLap > 0 {
		reasoning = fmt.Sprintf("Driver-set reminder for %s. Fired on entering lap %d.",
			label, state.CurrentLap)
	} else {
		reasoning = fmt.Sprintf("Driver-set reminder for %s. Triggered at %.0fm (target %.0fm − %.0fm lookahead). Lap %d.",
			label, state.LapDistance, r.TriggerDistM, r.LookaheadM, state.CurrentLap)
	}
	if len(reasoning) > brain.MaxEventReasoningChars {
		reasoning = reasoning[:brain.MaxEventReasoningChars]
	}
	return brain.Event{
		Type:        brain.EventCornerCoaching,
		Priority:    priority,
		Summary:     summary,
		Reasoning:   reasoning,
		RequiresAck: r.RequiresAck,
		DebugData: map[string]any{
			"reminder_id":    r.ID,
			"corner_id":      r.CornerID,
			"corner_name":    r.CornerName,
			"label":          r.Label,
			"trigger_m":      r.TriggerDistM,
			"trigger_at_lap": int(r.TriggerAtLap),
			"lookahead_m":    r.LookaheadM,
			"effective_m":    r.EffectiveDistM,
			"speed_kmh":      int(state.Speed),
			"lap":            int(state.CurrentLap),
		},
		Source:   "rule:reminder",
		DedupKey: fmt.Sprintf("reminder:%s:%d", r.ID, state.CurrentLap),
	}
}
