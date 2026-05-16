package plan

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/brain"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
)

// TrackInfoProvider is the minimal interface the ticker needs to resolve
// per-track spatial triggers (e.g., pit-entry coordinates). Implementations
// live in internal/trackmap — keeping it as an interface here lets the
// plan package stay free of a hard dependency on trackmap and lets tests
// inject a stub.
type TrackInfoProvider interface {
	PitEntry(trackID int) (distM float64, side string, ok bool)
}

// imminentLookaheadM is how far before the spatial trigger we fire
// EventPlanImminent. 250m gives ~6s warning at 150km/h and ~3.5s at 250
// km/h — long enough to brake calmly, short enough that it stays current.
const imminentLookaheadM = 250.0

// Ticker watches the live RaceState and emits brain events for the active
// plan entries whose triggers fire. One Ticker per process; wire it up
// from main once the brain bus and the plan store are both ready.
type Ticker struct {
	store     *Store
	state     func() *models.RaceState
	bus       *brain.RaceBrain
	talkLevel func() int32
	tracks    TrackInfoProvider // optional — without it, track_position triggers are silently skipped

	interval time.Duration
	now      func() time.Time

	// lastLap remembers what lap we evaluated on the previous tick so we
	// can detect lap-entry transitions cleanly. 0 means "no prior tick".
	lastLap int
}

// NewTicker constructs the ticker. tracks may be nil — in that case
// track_position triggers are evaluated as no-ops (they need a per-track
// coordinate to know "where on track" the trigger lives).
func NewTicker(
	store *Store,
	stateLoader func() *models.RaceState,
	bus *brain.RaceBrain,
	talkLevel func() int32,
	tracks TrackInfoProvider,
	interval time.Duration,
) *Ticker {
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	return &Ticker{
		store:     store,
		state:     stateLoader,
		bus:       bus,
		talkLevel: talkLevel,
		tracks:    tracks,
		interval:  interval,
		now:       time.Now,
	}
}

// Run drives the ticker until ctx is cancelled. Call once from main.
func (t *Ticker) Run(ctx context.Context) {
	ticker := time.NewTicker(t.interval)
	defer ticker.Stop()
	log.Info().Dur("interval", t.interval).Msg("plan ticker started")
	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("plan ticker stopping")
			return
		case <-ticker.C:
			t.tick()
		}
	}
}

// tick — one evaluation cycle. Exposed via the type for test access.
func (t *Ticker) tick() {
	if t.store == nil || t.state == nil || t.bus == nil {
		return
	}
	state := t.state()
	if state == nil {
		return
	}
	plan := t.store.Current()
	if plan == nil || len(plan.Entries) == 0 {
		return
	}
	currentLap := int(state.CurrentLap)
	if currentLap <= 0 {
		t.lastLap = 0
		return
	}

	for _, entry := range plan.Entries {
		if entry.Status == StatusDone || entry.Status == StatusCancelled {
			continue
		}
		switch entry.Trigger.Type {
		case TriggerLap:
			t.evaluateLap(state, entry, currentLap)
		case TriggerLapWindow:
			t.evaluateLapWindow(state, entry, currentLap)
		case TriggerTrackPosition:
			t.evaluateTrackPosition(state, entry, currentLap)
		case TriggerTimeLeftSec:
			t.evaluateTimeLeft(state, entry)
		}
	}
	t.lastLap = currentLap
}

// evaluateLap fires EventPlanReminder once when the player enters
// trigger.lap. Idempotent: the FiredEvents map guards against refire.
func (t *Ticker) evaluateLap(state *models.RaceState, entry PlanEntry, currentLap int) {
	if entry.Trigger.Lap == 0 {
		return
	}
	if currentLap < entry.Trigger.Lap {
		return
	}
	if currentLap > entry.Trigger.Lap {
		// Passed without firing — entry status was probably cancelled
		// mid-flight; mark as done so it disappears from the prompt.
		if _, fired := entry.FiredEvents[string(brain.EventPlanReminder)]; !fired {
			_, _ = t.store.SetStatus(entry.ID, StatusDone)
		}
		return
	}
	if t.alreadyFired(entry, brain.EventPlanReminder) {
		return
	}
	ev := buildLapEvent(state, entry, currentLap)
	if _, err := t.bus.EnqueueEvent(ev, t.currentTalkLevel()); err != nil {
		if errors.Is(err, brain.ErrEventDeduped) || errors.Is(err, brain.ErrEventTalkLevel) {
			log.Debug().Err(err).Str("entry_id", entry.ID).Msg("plan: reminder dropped")
			return
		}
		log.Warn().Err(err).Str("entry_id", entry.ID).Msg("plan: reminder rejected")
		return
	}
	_ = t.store.MarkFired(entry.ID, string(brain.EventPlanReminder), t.now())
	_, _ = t.store.SetStatus(entry.ID, StatusActive)
	log.Info().
		Str("entry_id", entry.ID).
		Str("kind", string(entry.Kind)).
		Int("lap", currentLap).
		Msg("plan: reminder fired")
}

// evaluateLapWindow fires open on entry to from_lap and close on entry to
// to_lap+1. Status transitions reflect the lifecycle: pending → active →
// done.
func (t *Ticker) evaluateLapWindow(state *models.RaceState, entry PlanEntry, currentLap int) {
	from, to := entry.Trigger.FromLap, entry.Trigger.ToLap
	if from == 0 || to == 0 {
		return
	}
	openKey := string(brain.EventPlanWindowOpen)
	closeKey := string(brain.EventPlanWindowClose)

	if currentLap >= from && currentLap <= to {
		// Inside the window — fire open exactly once.
		if t.alreadyFired(entry, brain.EventPlanWindowOpen) {
			return
		}
		ev := buildWindowEvent(state, entry, currentLap, true)
		if _, err := t.bus.EnqueueEvent(ev, t.currentTalkLevel()); err != nil {
			if !errors.Is(err, brain.ErrEventDeduped) && !errors.Is(err, brain.ErrEventTalkLevel) {
				log.Warn().Err(err).Str("entry_id", entry.ID).Msg("plan: window_open rejected")
				return
			}
		}
		_ = t.store.MarkFired(entry.ID, openKey, t.now())
		_, _ = t.store.SetStatus(entry.ID, StatusActive)
		log.Info().Str("entry_id", entry.ID).Int("lap", currentLap).Msg("plan: window opened")
		return
	}
	if currentLap > to {
		if !t.alreadyFired(entry, brain.EventPlanWindowClose) && t.alreadyFired(entry, brain.EventPlanWindowOpen) {
			ev := buildWindowEvent(state, entry, currentLap, false)
			if _, err := t.bus.EnqueueEvent(ev, t.currentTalkLevel()); err != nil {
				if !errors.Is(err, brain.ErrEventDeduped) && !errors.Is(err, brain.ErrEventTalkLevel) {
					log.Warn().Err(err).Str("entry_id", entry.ID).Msg("plan: window_close rejected")
				}
			}
			_ = t.store.MarkFired(entry.ID, closeKey, t.now())
		}
		_, _ = t.store.SetStatus(entry.ID, StatusDone)
	}
}

// evaluateTrackPosition fires EventPlanImminent N metres before the spatial
// trigger on the matching lap. Uses an optional TrackInfoProvider when the
// trigger doesn't carry an explicit DistanceM (e.g., user said "pit stop on
// lap 14" without specifying the exact metres; we infer pit-entry).
func (t *Ticker) evaluateTrackPosition(state *models.RaceState, entry PlanEntry, currentLap int) {
	if currentLap != entry.Trigger.Lap {
		if currentLap > entry.Trigger.Lap && !t.alreadyFired(entry, brain.EventPlanImminent) {
			_, _ = t.store.SetStatus(entry.ID, StatusDone)
		}
		return
	}
	target := entry.Trigger.DistanceM
	if target <= 0 && t.tracks != nil {
		if d, _, ok := t.tracks.PitEntry(int(state.TrackID)); ok {
			target = d
		}
	}
	if target <= 0 {
		return
	}
	if state.TrackLength == 0 {
		return
	}
	lapDist := float64(state.LapDistance)
	gap := target - lapDist
	if gap > imminentLookaheadM || gap < 0 {
		return
	}
	if t.alreadyFired(entry, brain.EventPlanImminent) {
		return
	}
	ev := buildTrackPositionEvent(state, entry, currentLap, target, gap)
	if _, err := t.bus.EnqueueEvent(ev, t.currentTalkLevel()); err != nil {
		if !errors.Is(err, brain.ErrEventDeduped) && !errors.Is(err, brain.ErrEventTalkLevel) {
			log.Warn().Err(err).Str("entry_id", entry.ID).Msg("plan: imminent rejected")
			return
		}
	}
	_ = t.store.MarkFired(entry.ID, string(brain.EventPlanImminent), t.now())
	_, _ = t.store.SetStatus(entry.ID, StatusActive)
	log.Info().
		Str("entry_id", entry.ID).
		Int("lap", currentLap).
		Float64("target_m", target).
		Float64("gap_m", gap).
		Msg("plan: imminent fired")
}

// evaluateTimeLeft fires EventPlanReminder once when SessionTimeLeft drops
// below the configured threshold. Useful for end-of-session reminders
// ("two laps left, switch to push") that don't have a lap-keyed trigger.
func (t *Ticker) evaluateTimeLeft(state *models.RaceState, entry PlanEntry) {
	if entry.Trigger.Seconds <= 0 || state.SessionTimeLeft == 0 {
		return
	}
	if int(state.SessionTimeLeft) > entry.Trigger.Seconds {
		return
	}
	if t.alreadyFired(entry, brain.EventPlanReminder) {
		return
	}
	ev := buildTimeLeftEvent(state, entry)
	if _, err := t.bus.EnqueueEvent(ev, t.currentTalkLevel()); err != nil {
		if !errors.Is(err, brain.ErrEventDeduped) && !errors.Is(err, brain.ErrEventTalkLevel) {
			log.Warn().Err(err).Str("entry_id", entry.ID).Msg("plan: time-left rejected")
			return
		}
	}
	_ = t.store.MarkFired(entry.ID, string(brain.EventPlanReminder), t.now())
	_, _ = t.store.SetStatus(entry.ID, StatusActive)
}

func (t *Ticker) alreadyFired(entry PlanEntry, kind brain.EventType) bool {
	_, ok := entry.FiredEvents[string(kind)]
	return ok
}

func (t *Ticker) currentTalkLevel() int32 {
	if t.talkLevel == nil {
		return 5
	}
	return t.talkLevel()
}

// buildLapEvent builds a P3 EventPlanReminder for a `lap` trigger. The
// summary is a fact-only sentence; the LLM rephrases it into a radio call.
func buildLapEvent(state *models.RaceState, entry PlanEntry, currentLap int) brain.Event {
	headline := entryHeadline(entry)
	summary := truncateTo(fmt.Sprintf("Plan: %s — lap %d.", headline, currentLap), brain.MaxEventSummaryChars)
	reasoning := truncateTo(fmt.Sprintf(
		"Plan entry %s (kind=%s) fires this lap. Notes: %s",
		entry.ID, entry.Kind, fallback(entry.Notes, "—"),
	), brain.MaxEventReasoningChars)
	return brain.Event{
		Type:      brain.EventPlanReminder,
		Priority:  brain.DefaultEventPriority(brain.EventPlanReminder),
		Summary:   summary,
		Reasoning: reasoning,
		DebugData: map[string]any{
			"plan_entry_id":   entry.ID,
			"plan_entry_kind": string(entry.Kind),
			"trigger_lap":     entry.Trigger.Lap,
			"current_lap":     currentLap,
			"speed_kmh":       int(state.Speed),
			"payload":         entry.Payload,
		},
		Source:       "plan:lap",
		DedupKey:     fmt.Sprintf("plan_reminder:%s:%d", entry.ID, currentLap),
		DedupSubject: "plan:" + entry.ID,
	}
}

// buildWindowEvent builds the open/close edge of a `lap_window` trigger.
// open=true uses EventPlanWindowOpen, open=false uses EventPlanWindowClose.
func buildWindowEvent(state *models.RaceState, entry PlanEntry, currentLap int, open bool) brain.Event {
	kind := brain.EventPlanWindowClose
	verb := "closed"
	if open {
		kind = brain.EventPlanWindowOpen
		verb = "open"
	}
	headline := entryHeadline(entry)
	summary := truncateTo(fmt.Sprintf("Plan window %s: %s — lap %d.", verb, headline, currentLap), brain.MaxEventSummaryChars)
	reasoning := truncateTo(fmt.Sprintf(
		"Plan entry %s window %d-%d %s at lap %d. Notes: %s",
		entry.ID, entry.Trigger.FromLap, entry.Trigger.ToLap, verb, currentLap, fallback(entry.Notes, "—"),
	), brain.MaxEventReasoningChars)
	return brain.Event{
		Type:      kind,
		Priority:  brain.DefaultEventPriority(kind),
		Summary:   summary,
		Reasoning: reasoning,
		DebugData: map[string]any{
			"plan_entry_id":   entry.ID,
			"plan_entry_kind": string(entry.Kind),
			"from_lap":        entry.Trigger.FromLap,
			"to_lap":          entry.Trigger.ToLap,
			"current_lap":     currentLap,
			"payload":         entry.Payload,
		},
		Source:       "plan:lap_window",
		DedupKey:     fmt.Sprintf("plan_window:%s:%s:%d", entry.ID, verb, currentLap),
		DedupSubject: "plan:" + entry.ID,
	}
}

// buildTrackPositionEvent builds the P4 EventPlanImminent for a
// `track_position` trigger. The summary uses the metres remaining so the
// driver hears "...in 200m..." rather than "...soon..."
func buildTrackPositionEvent(state *models.RaceState, entry PlanEntry, currentLap int, targetM, gapM float64) brain.Event {
	headline := entryHeadline(entry)
	summary := truncateTo(fmt.Sprintf("Plan imminent: %s — %.0fm.", headline, gapM), brain.MaxEventSummaryChars)
	reasoning := truncateTo(fmt.Sprintf(
		"Plan entry %s spatial trigger at %.0fm into lap %d. Currently %.0fm; %.0fm to go. Notes: %s",
		entry.ID, targetM, currentLap, state.LapDistance, gapM, fallback(entry.Notes, "—"),
	), brain.MaxEventReasoningChars)
	return brain.Event{
		Type:      brain.EventPlanImminent,
		Priority:  brain.DefaultEventPriority(brain.EventPlanImminent),
		Summary:   summary,
		Reasoning: reasoning,
		DebugData: map[string]any{
			"plan_entry_id":   entry.ID,
			"plan_entry_kind": string(entry.Kind),
			"trigger_lap":     entry.Trigger.Lap,
			"current_lap":     currentLap,
			"target_m":        targetM,
			"current_m":       state.LapDistance,
			"gap_m":           gapM,
			"speed_kmh":       int(state.Speed),
			"payload":         entry.Payload,
		},
		Source:       "plan:track_position",
		DedupKey:     fmt.Sprintf("plan_imminent:%s:%d", entry.ID, currentLap),
		DedupSubject: "plan:" + entry.ID,
	}
}

// buildTimeLeftEvent builds the P3 EventPlanReminder for a `time_left_s`
// trigger.
func buildTimeLeftEvent(state *models.RaceState, entry PlanEntry) brain.Event {
	headline := entryHeadline(entry)
	summary := truncateTo(fmt.Sprintf("Plan: %s — %ds left.", headline, state.SessionTimeLeft), brain.MaxEventSummaryChars)
	reasoning := truncateTo(fmt.Sprintf(
		"Plan entry %s time trigger (threshold %ds). Session time left %ds. Notes: %s",
		entry.ID, entry.Trigger.Seconds, state.SessionTimeLeft, fallback(entry.Notes, "—"),
	), brain.MaxEventReasoningChars)
	return brain.Event{
		Type:      brain.EventPlanReminder,
		Priority:  brain.DefaultEventPriority(brain.EventPlanReminder),
		Summary:   summary,
		Reasoning: reasoning,
		DebugData: map[string]any{
			"plan_entry_id":     entry.ID,
			"plan_entry_kind":   string(entry.Kind),
			"threshold_s":       entry.Trigger.Seconds,
			"session_time_left": int(state.SessionTimeLeft),
			"payload":           entry.Payload,
		},
		Source:       "plan:time_left",
		DedupKey:     fmt.Sprintf("plan_time_reminder:%s", entry.ID),
		DedupSubject: "plan:" + entry.ID,
	}
}

// entryHeadline picks the most useful one-line description of an entry,
// preferring an explicit Payload["headline"] when set, then Notes, then
// the Kind name.
func entryHeadline(e PlanEntry) string {
	if v, ok := e.Payload["headline"].(string); ok {
		v = strings.TrimSpace(v)
		if v != "" {
			return v
		}
	}
	if v := strings.TrimSpace(e.Notes); v != "" {
		return v
	}
	return string(e.Kind)
}

func fallback(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func truncateTo(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 1 {
		return s[:max]
	}
	return s[:max-1] + "…"
}
