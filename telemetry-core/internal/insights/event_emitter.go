package insights

import (
	"errors"
	"fmt"

	"github.com/rs/zerolog/log"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/brain"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
)

// emitGameStateEvents inspects the current RaceState for transitions worth
// firing typed events on. Called from Engine.evaluate AFTER the legacy rule
// loop so cd has already been updated for the current tick. Each producer
// is a small idempotent function: trigger condition + cooldown guard +
// EnqueueEvent.
//
// Producers deliberately deferred (see plan):
//   - emitYellowFlag     — F1 25 doesn't expose a per-sector flag field cleanly.
//   - emitRedFlag        — RDFL is not a discrete event; needs heuristic.
func (e *Engine) emitGameStateEvents(state *models.RaceState, talkLevel int32) {
	if e.bus == nil || state == nil {
		return
	}
	e.emitSafetyCar(state, talkLevel)
	e.emitVSC(state, talkLevel)
	e.emitFastestLap(state, talkLevel)
	e.emitPitWindowOpen(state, talkLevel)
	e.emitWeatherChange(state, talkLevel)
	// Threats from the car behind. emitOvertaken runs first so a
	// position drop preempts the slower closing-rate trend math.
	// Both are gated to race sessions internally — they use F1 25's
	// nominal Position field which is meaningless in P/Q.
	// EventCarApproaching (proximity heads-up) is emitted by its own
	// 1 s ProximityWatcher goroutine, not from this 5 s tick — at
	// 5 s a fast-closing car covers the whole watch window between
	// ticks and we never accumulate samples.
	e.emitOvertaken(state, talkLevel)
	e.emitClosingThreat(state, talkLevel)
}

// emitSafetyCar fires when SafetyCarStatus transitions to "Full Safety Car".
// Reuses the existing SafetyCarNotified flag so we only fire once per period.
func (e *Engine) emitSafetyCar(state *models.RaceState, talkLevel int32) {
	if state.SafetyCarStatus != 1 {
		// Reset latch when condition clears so the next deployment fires.
		if state.SafetyCarStatus == 0 && e.cooldown.SafetyCarNotified() {
			e.cooldown.SetSafetyCarNotified(false)
		}
		return
	}
	if e.cooldown.SafetyCarNotified() {
		return
	}
	e.cooldown.SetSafetyCarNotified(true)

	ev := brain.Event{
		Type:      brain.EventSafetyCar,
		Priority:  brain.DefaultEventPriority(brain.EventSafetyCar),
		Summary:   "Safety car deployed.",
		Reasoning: fmt.Sprintf("SafetyCarStatus transitioned to 1. Lap %d, position P%d.", state.CurrentLap, state.Position),
		DebugData: map[string]any{
			"lap":      int(state.CurrentLap),
			"position": int(state.Position),
		},
		Source:   "rule:safety_car",
		DedupKey: fmt.Sprintf("safety_car:%d", state.CurrentLap),
	}
	enqueueAndLog(e, ev, talkLevel)
}

// emitVSC fires when SafetyCarStatus transitions to "Virtual Safety Car" (2).
func (e *Engine) emitVSC(state *models.RaceState, talkLevel int32) {
	if state.SafetyCarStatus != 2 {
		if state.SafetyCarStatus == 0 && e.cooldown.VSCNotified() {
			e.cooldown.SetVSCNotified(false)
		}
		return
	}
	if e.cooldown.VSCNotified() {
		return
	}
	e.cooldown.SetVSCNotified(true)

	ev := brain.Event{
		Type:      brain.EventVSC,
		Priority:  brain.DefaultEventPriority(brain.EventVSC),
		Summary:   "VSC deployed — virtual safety car.",
		Reasoning: fmt.Sprintf("SafetyCarStatus=2 at lap %d. Hold delta, do not overtake.", state.CurrentLap),
		DebugData: map[string]any{"lap": int(state.CurrentLap)},
		Source:    "rule:vsc",
		DedupKey:  fmt.Sprintf("vsc:%d", state.CurrentLap),
	}
	enqueueAndLog(e, ev, talkLevel)
}

// emitFastestLap fires on FTLP packets. state.LastEventCode is sticky —
// the writer sets it on an event packet and never clears it, so the rule
// engine sees "FTLP" on every 5 s tick until a different event packet
// overwrites it. Without a latch this producer would re-fire on every
// tick because the bus's DedupKey check only covers queued + recently-
// spoken events, not events that have already been dispatched out of
// the queue. The latch mirrors emitSafetyCar's pattern: fire once when
// the code is FTLP, then reset as soon as a different code arrives.
func (e *Engine) emitFastestLap(state *models.RaceState, talkLevel int32) {
	if state.LastEventCode != "FTLP" {
		if e.cooldown.FastestLapNotified() {
			e.cooldown.SetFastestLapNotified(false)
		}
		return
	}
	if e.cooldown.FastestLapNotified() {
		return
	}
	e.cooldown.SetFastestLapNotified(true)

	ev := brain.Event{
		Type:      brain.EventFastestLap,
		Priority:  brain.DefaultEventPriority(brain.EventFastestLap),
		Summary:   fmt.Sprintf("Fastest lap by Car #%d.", state.LastEventIdx),
		Reasoning: "FTLP event packet — overall fastest lap of the session.",
		DebugData: map[string]any{
			"vehicle_idx": int(state.LastEventIdx),
			"lap":         int(state.CurrentLap),
		},
		Source:   "rule:fastest_lap",
		DedupKey: fmt.Sprintf("fastest_lap:%d:%d", state.LastEventIdx, state.CurrentLap),
	}
	enqueueAndLog(e, ev, talkLevel)
}

// emitPitWindowOpen fires once when the player's CurrentLap reaches the
// game's PitWindowIdeal lap. Latched so subsequent ticks within the window
// don't re-fire.
func (e *Engine) emitPitWindowOpen(state *models.RaceState, talkLevel int32) {
	if state.PitWindowIdeal == 0 || state.CurrentLap < state.PitWindowIdeal {
		return
	}
	if e.cooldown.PitWindowOpenNotified() {
		return
	}
	e.cooldown.SetPitWindowOpenNotified(true)

	ev := brain.Event{
		Type:     brain.EventPitWindowOpen,
		Priority: brain.DefaultEventPriority(brain.EventPitWindowOpen),
		Summary:  fmt.Sprintf("Pit window open. Ideal lap %d, latest %d.", state.PitWindowIdeal, state.PitWindowLatest),
		Reasoning: fmt.Sprintf("Currently lap %d at P%d on compound %d (%d laps old).",
			state.CurrentLap, state.Position, state.ActualCompound, state.TyresAgeLaps),
		DebugData: map[string]any{
			"current_lap":       int(state.CurrentLap),
			"pit_window_ideal":  int(state.PitWindowIdeal),
			"pit_window_latest": int(state.PitWindowLatest),
			"compound":          int(state.ActualCompound),
			"tyre_age_laps":     int(state.TyresAgeLaps),
		},
		Source:   "rule:pit_window_open",
		DedupKey: fmt.Sprintf("pit_window:%d", state.PitWindowIdeal),
	}
	enqueueAndLog(e, ev, talkLevel)
}

// emitWeatherChange fires once per session when the first WeatherForecasts
// sample shows rain >50% in the next 5 minutes. Reuses RainWarned cooldown
// so we don't double up with the existing legacy ruleRainForecast rule.
func (e *Engine) emitWeatherChange(state *models.RaceState, talkLevel int32) {
	if e.cooldown.WeatherChangeNotified() {
		return
	}
	// Walk the first few samples; trigger on the soonest with rain probability ≥ 60.
	for _, fc := range state.WeatherForecasts {
		if fc.TimeOffset == 0 || fc.TimeOffset > 5 {
			continue
		}
		if fc.RainPercentage < 60 {
			continue
		}
		e.cooldown.SetWeatherChangeNotified(true)
		ev := brain.Event{
			Type:     brain.EventWeatherChange,
			Priority: brain.DefaultEventPriority(brain.EventWeatherChange),
			Summary:  fmt.Sprintf("Rain in %d min, %d%% chance.", fc.TimeOffset, fc.RainPercentage),
			Reasoning: fmt.Sprintf("Forecast sample T+%dmin: weather=%d, rain=%d%%. Currently at lap %d.",
				fc.TimeOffset, fc.Weather, fc.RainPercentage, state.CurrentLap),
			DebugData: map[string]any{
				"time_offset_min": int(fc.TimeOffset),
				"rain_percentage": int(fc.RainPercentage),
				"weather":         int(fc.Weather),
			},
			Source:   "rule:weather_change",
			DedupKey: fmt.Sprintf("weather_change:%d", state.CurrentLap),
		}
		enqueueAndLog(e, ev, talkLevel)
		return
	}
}

// enqueueAndLog is the standard producer postlude: enqueue, log on success,
// log + suppress on filter, warn on validation error.
func enqueueAndLog(e *Engine, ev brain.Event, talkLevel int32) {
	stored, err := e.bus.EnqueueEvent(ev, talkLevel)
	switch {
	case err == nil:
		log.Info().
			Str("event_id", stored.ID).
			Str("type", string(stored.Type)).
			Int("priority", stored.Priority).
			Str("summary", stored.Summary).
			Msg("Producer → Interrupts Bus")
	case errors.Is(err, brain.ErrEventDeduped):
		log.Debug().Str("type", string(ev.Type)).Msg("Event deduped")
	case errors.Is(err, brain.ErrEventTalkLevel):
		log.Debug().Str("type", string(ev.Type)).Int32("talk_level", talkLevel).Msg("Event dropped by TalkLevel")
	default:
		log.Warn().Err(err).Str("type", string(ev.Type)).Msg("Event rejected")
	}
}
