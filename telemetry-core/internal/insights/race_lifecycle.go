package insights

import (
	"fmt"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/brain"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/enums"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
)

// emitRaceLifecycle dispatches the three one-shot race-lifecycle events:
// lights out, final lap, and race finished (podium vs not). Each emitter
// is gated by a latch on the cooldown manager that resets on session
// change, so a fresh race re-arms every fire.
//
// The cumulative effect at the LLM layer is that the engineer now has
// explicit signal for "the race is starting now", "this is the last lap",
// and "the race is over (and here's the result)" — moments the rule
// engine had no producer for before this.
func (e *Engine) emitRaceLifecycle(state *models.RaceState, talkLevel int32) {
	if state == nil {
		return
	}
	// Honour the session-uid latch reset before checking any of the
	// individual flags — otherwise carry-over state from the previous
	// race would block a fresh race-start fire.
	e.cooldown.ResetForNewSession(state.SessionUID)

	if !models.IsRaceSession(state.SessionType) {
		return
	}
	e.emitLightsOut(state, talkLevel)
	e.emitFinalLap(state, talkLevel)
	e.emitRaceFinished(state, talkLevel)
}

// emitLightsOut fires the moment the LGOT event packet arrives. We mirror
// emitFastestLap's pattern: latch + reset-on-different-code so we don't
// re-fire on every 5s tick while LastEventCode stays "LGOT".
//
// Carries grid context (start position, weather, compound, total laps) so
// the engineer can synthesise a pre-race line ("Lights out — P7, mediums,
// 53 laps. Clean launch.") without re-querying state.
func (e *Engine) emitLightsOut(state *models.RaceState, talkLevel int32) {
	if state.LastEventCode != "LGOT" {
		// Reset the latch when the event code rolls over so a restart
		// (replay/flashback) re-arms.
		if e.cooldown.LightsOutNotified() {
			e.cooldown.SetLightsOutNotified(false)
		}
		return
	}
	if e.cooldown.LightsOutNotified() {
		return
	}
	e.cooldown.SetLightsOutNotified(true)

	ev := brain.Event{
		Type:     brain.EventLightsOut,
		Priority: brain.DefaultEventPriority(brain.EventLightsOut),
		Summary:  fmt.Sprintf("Lights out — P%d on the grid, %d laps.", state.GridPosition, state.TotalLaps),
		Reasoning: fmt.Sprintf(
			"LGOT event packet. Starting P%d on %s. Weather %s, track %d°C. %d laps.",
			state.GridPosition, enums.Compound(state.ActualCompound),
			enums.Weather(state.Weather), state.TrackTemp, state.TotalLaps,
		),
		DebugData: map[string]any{
			"grid_position":   int(state.GridPosition),
			"total_laps":      int(state.TotalLaps),
			"compound":        int(state.ActualCompound),
			"compound_name":   enums.Compound(state.ActualCompound),
			"weather":         int(state.Weather),
			"weather_name":    enums.Weather(state.Weather),
			"track_temp_c":    int(state.TrackTemp),
			"air_temp_c":      int(state.AirTemp),
			"rain_percentage": int(state.RainPercentage),
			"fuel_in_tank":    state.FuelInTank,
			"phase":           string(models.PhaseLightsOut),
		},
		Source:   "rule:lights_out",
		DedupKey: fmt.Sprintf("lights_out:%d", state.SessionUID),
	}
	enqueueAndLog(e, ev, talkLevel)
}

// emitFinalLap fires once when CurrentLap rolls into the last lap of the
// race (CurrentLap == TotalLaps). At this point the player is on their
// final lap but hasn't yet crossed the line — distinct from RaceFinished.
// Carries strategic context (position, gap to ahead/behind, fuel, tire
// wear, damage) so the engineer can choose attack/defend framing.
func (e *Engine) emitFinalLap(state *models.RaceState, talkLevel int32) {
	if state.TotalLaps == 0 || state.CurrentLap == 0 {
		return
	}
	if state.CurrentLap < state.TotalLaps {
		return
	}
	if state.RaceFinished {
		// Race is already done; final-lap framing is moot.
		return
	}
	if e.cooldown.FinalLapNotified() {
		return
	}
	e.cooldown.SetFinalLapNotified(true)

	// Pick a one-sentence framing based on field position.
	framing := "Push or protect — make it count."
	switch {
	case state.Position == 1:
		framing = "Last lap. Bring it home clean."
	case state.Position <= 3:
		framing = "Last lap. Podium on the line — hold it."
	case state.DeltaToFrontMs > 0 && state.DeltaToFrontMs < 2000:
		framing = "Last lap. Car ahead is in reach — go for it."
	}

	worstWear := state.TyresWear[0]
	for _, w := range state.TyresWear[1:] {
		if w > worstWear {
			worstWear = w
		}
	}

	ev := brain.Event{
		Type:     brain.EventFinalLap,
		Priority: brain.DefaultEventPriority(brain.EventFinalLap),
		Summary:  fmt.Sprintf("Final lap. P%d. %s", state.Position, framing),
		Reasoning: fmt.Sprintf(
			"CurrentLap %d == TotalLaps %d. Gap ahead %dms, gap behind %dms, fuel %.1f laps, worst tire wear %.1f%%.",
			state.CurrentLap, state.TotalLaps, state.DeltaToFrontMs, -state.DeltaToFrontMs,
			state.FuelRemainingLaps, worstWear,
		),
		DebugData: map[string]any{
			"current_lap":          int(state.CurrentLap),
			"total_laps":           int(state.TotalLaps),
			"position":             int(state.Position),
			"grid_position":        int(state.GridPosition),
			"delta_to_front_ms":    int(state.DeltaToFrontMs),
			"delta_to_leader_ms":   int(state.DeltaToLeaderMs),
			"fuel_remaining_laps":  state.FuelRemainingLaps,
			"worst_tire_wear_pct":  worstWear,
			"phase":                string(models.PhaseFinalLap),
		},
		Source:   "rule:final_lap",
		DedupKey: fmt.Sprintf("final_lap:%d", state.SessionUID),
	}
	enqueueAndLog(e, ev, talkLevel)
}

// emitRaceFinished fires once after CHQF has been observed (state.RaceFinished
// is the latch the writer flips). Routes by final position:
//
//   - P1            → EventPodium with a real win celebration.
//   - P2 / P3       → EventPodium podium framing.
//   - P4..          → EventRaceFinished with constructive framing (grid-to-
//                     finish delta, fastest-lap if any, clean run summary).
//
// All three carry a rich DebugData payload (pit stops, total laps, fuel left,
// damage summary, gap to next car) so the LLM has the raw material for a
// proper end-of-race radio call without re-querying.
func (e *Engine) emitRaceFinished(state *models.RaceState, talkLevel int32) {
	if !state.RaceFinished {
		return
	}
	if e.cooldown.RaceFinishNotified() {
		return
	}
	e.cooldown.SetRaceFinishNotified(true)

	finalPos := state.FinalClassification
	if finalPos == 0 {
		finalPos = state.Position
	}

	gridDelta := int(state.GridPosition) - int(finalPos) // positive = gained spots
	damageSummary := summariseDamage(state)

	debug := map[string]any{
		"final_position":    int(finalPos),
		"grid_position":     int(state.GridPosition),
		"positions_gained":  gridDelta,
		"total_laps":        int(state.TotalLaps),
		"pit_stops":         int(state.NumPitStops),
		"fuel_remaining_kg": state.FuelInTank,
		"compound":          int(state.ActualCompound),
		"compound_name":     enums.Compound(state.ActualCompound),
		"damage":            damageSummary,
		"phase":             string(models.PhaseFinished),
	}

	var ev brain.Event
	switch {
	case finalPos == 1:
		ev = brain.Event{
			Type:     brain.EventPodium,
			Priority: brain.DefaultEventPriority(brain.EventPodium),
			Summary:  "P1 — race win. Celebrate on the radio.",
			Reasoning: fmt.Sprintf(
				"Race finished P1 (started P%d, %+d positions). %d pit stops, %d laps. This is the headline moment — lead the radio call with the win, mean it.",
				state.GridPosition, gridDelta, state.NumPitStops, state.TotalLaps,
			),
			DebugData: debug,
			Source:    "rule:race_finished",
			DedupKey:  fmt.Sprintf("race_finished:%d:p1", state.SessionUID),
		}
	case finalPos == 2 || finalPos == 3:
		ev = brain.Event{
			Type:     brain.EventPodium,
			Priority: brain.DefaultEventPriority(brain.EventPodium),
			Summary:  fmt.Sprintf("P%d — podium finish.", finalPos),
			Reasoning: fmt.Sprintf(
				"Race finished P%d (started P%d, %+d positions). %d pit stops, %d laps. Acknowledge the podium properly before any debrief.",
				finalPos, state.GridPosition, gridDelta, state.NumPitStops, state.TotalLaps,
			),
			DebugData: debug,
			Source:    "rule:race_finished",
			DedupKey:  fmt.Sprintf("race_finished:%d:podium", state.SessionUID),
		}
	default:
		// Reframe non-podium constructively. If we gained positions from
		// the grid, lead with that. If we lost, keep it grounded — no
		// fake positivity.
		var summary string
		switch {
		case gridDelta > 0:
			summary = fmt.Sprintf("Race over — P%d, gained %d. Solid drive.", finalPos, gridDelta)
		case gridDelta == 0:
			summary = fmt.Sprintf("Race over — P%d, held station.", finalPos)
		default:
			summary = fmt.Sprintf("Race over — P%d. Debrief in the garage.", finalPos)
		}
		ev = brain.Event{
			Type:     brain.EventRaceFinished,
			Priority: brain.DefaultEventPriority(brain.EventRaceFinished),
			Summary:  summary,
			Reasoning: fmt.Sprintf(
				"Race finished P%d (started P%d, %+d positions). %d pit stops, %d laps. Lead with the result honestly; one constructive takeaway, no coaching after.",
				finalPos, state.GridPosition, gridDelta, state.NumPitStops, state.TotalLaps,
			),
			DebugData: debug,
			Source:    "rule:race_finished",
			DedupKey:  fmt.Sprintf("race_finished:%d", state.SessionUID),
		}
	}
	enqueueAndLog(e, ev, talkLevel)
}

// summariseDamage returns a compact map of component -> percent for the
// non-zero damage channels. Lets the end-of-race event carry "what
// survived" without the LLM having to inspect every field.
func summariseDamage(state *models.RaceState) map[string]int {
	out := map[string]int{}
	add := func(name string, v uint8) {
		if v > 0 {
			out[name] = int(v)
		}
	}
	add("front_left_wing", state.FrontLeftWingDmg)
	add("front_right_wing", state.FrontRightWingDmg)
	add("rear_wing", state.RearWingDmg)
	add("floor", state.FloorDmg)
	add("diffuser", state.DiffuserDmg)
	add("sidepod", state.SidepodDmg)
	add("gearbox", state.GearBoxDmg)
	add("engine", state.EngineDmg)
	return out
}
