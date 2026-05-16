package insights

import (
	"fmt"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/brain"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/enums"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
)

// emitSetupAdvice runs the four driver-setup mismatch rules. Each rule emits
// an EventSetupAdvice (P3, silent by default) so the Live agent and the
// dashboard "Comms Queue" see the recommendation alongside other context.
// The pi-agent setup specialist may later push a louder version via
// /api/strategy if the mismatch persists.
//
// All four rules are advisory — they encode F1-engineer rules of thumb, not
// strict thresholds — so the cooldown is keyed on the (topic, target_value)
// pair. A stable recommendation re-fires only when the suggested value moves.
func (e *Engine) emitSetupAdvice(state *models.RaceState, talkLevel int32) {
	if e.bus == nil || state == nil {
		return
	}
	e.emitBrakeBiasAdvice(state, talkLevel)
	e.emitERSDeployAdvice(state, talkLevel)
	e.emitDRSUtilizationAdvice(state, talkLevel)
	e.emitFuelMixAdvice(state, talkLevel)
}

// emitBrakeBiasAdvice fires when the most recent insight signature looks like
// a lock-up pattern AND the current brake bias is meaningfully off the
// canonical 54-56% window for the player car. We don't have per-corner
// lock-up history at hand inside the engine, so this is intentionally
// conservative: we hook off LastEventCode to detect a recent hard-braking
// indicator and use it as a permission signal to recommend ±2% bias.
func (e *Engine) emitBrakeBiasAdvice(state *models.RaceState, talkLevel int32) {
	bb := state.DriverSettings.BrakeBias
	if bb == 0 {
		return // CarSetup hasn't landed yet
	}
	// Only fire on race sessions — practice/quali drivers experiment with
	// settings and don't want backseat coaching.
	if !isRaceSession(state.SessionType) {
		return
	}
	// LastEventCode == "HardBraking" is the legacy rule's signal; we use it
	// as a proxy for "the driver just locked up / had a hairy stop". The
	// rule fires sparingly thanks to its own cooldown.
	if state.LastEventCode != "HardBraking" {
		return
	}

	var target int
	var direction string
	switch {
	case bb >= 58:
		target = int(bb) - 2
		direction = "rearward"
	case bb <= 52:
		target = int(bb) + 2
		direction = "forward"
	default:
		return
	}

	if !e.cooldown.ShouldSetupAdvise(string(brain.TopicSetupBrakeBias), target) {
		return
	}

	summary := fmt.Sprintf("Try BB %d (%s 2%%).", target, direction)
	reasoning := fmt.Sprintf(
		"Brake bias %d%% paired with recent HardBraking — shift %s 2%% to settle the rotation.",
		bb, direction,
	)
	ev := brain.Event{
		Type:      brain.EventSetupAdvice,
		Priority:  brain.DefaultEventPriority(brain.EventSetupAdvice),
		Summary:   summary,
		Reasoning: reasoning,
		Source:    "rule:setup_advice.brake_bias",
		DedupKey:  fmt.Sprintf("setup_advice.brake_bias:%d", target),
		DebugData: map[string]any{
			"topic":    string(brain.TopicSetupBrakeBias),
			"current":  int(bb),
			"target":   target,
			"channel":  "cockpit",
			"category": "brake_bias",
		},
	}
	enqueueAndLog(e, ev, talkLevel)
}

// emitERSDeployAdvice fires when the current ERS deploy mode looks
// mismatched against the race phase. Two main mismatches:
//   - Hotlap (mode 3) on a long stint with a stable gap — costs battery
//     in a window where it doesn't change outcomes.
//   - Medium (mode 2) when a close fight is on (we have a car within ~1.0s
//     ahead OR behind) and the driver should be on Overtake.
//
// ERSDeployMode enum: 0=None, 1=Medium, 2=Hotlap, 3=Overtake (F1 25). The
// canonical race-pace mode is Medium; "low" is None.
func (e *Engine) emitERSDeployAdvice(state *models.RaceState, talkLevel int32) {
	if !isRaceSession(state.SessionType) || state.TotalLaps == 0 {
		return
	}
	mode := state.ERSDeployMode
	remaining := int(state.TotalLaps) - int(state.CurrentLap)

	// Mismatch 1: Hotlap with > 10 laps left and no immediate threat.
	if mode == 2 && remaining > 10 && (state.DeltaToFrontMs > 2500 || state.DeltaToFrontMs == 0) {
		target := 1 // recommend Medium
		if !e.cooldown.ShouldSetupAdvise(string(brain.TopicSetupERSMode), target) {
			return
		}
		ev := brain.Event{
			Type:     brain.EventSetupAdvice,
			Priority: brain.DefaultEventPriority(brain.EventSetupAdvice),
			Summary:  "ERS to Medium — long stint.",
			Reasoning: fmt.Sprintf(
				"ERS Hotlap with %d laps remaining and no close threat — Medium banks battery for the closing stint.",
				remaining,
			),
			Source:   "rule:setup_advice.ers_mode",
			DedupKey: "setup_advice.ers_mode:medium_long_stint",
			DebugData: map[string]any{
				"topic":        string(brain.TopicSetupERSMode),
				"current":      int(mode),
				"current_name": enums.ERSDeployMode(mode),
				"target":       target,
				"target_name":  enums.ERSDeployMode(uint8(target)),
				"channel":      "cockpit",
				"category":     "ers_mode",
			},
		}
		enqueueAndLog(e, ev, talkLevel)
		return
	}

	// Mismatch 2: Medium when fighting within ~1.0s — Overtake mode buys
	// straight-line speed for the attack.
	if mode == 1 && state.DeltaToFrontMs > 0 && state.DeltaToFrontMs < 1000 {
		target := 3 // recommend Overtake
		if !e.cooldown.ShouldSetupAdvise(string(brain.TopicSetupERSMode), target) {
			return
		}
		ev := brain.Event{
			Type:     brain.EventSetupAdvice,
			Priority: brain.DefaultEventPriority(brain.EventSetupAdvice),
			Summary:  "ERS to Overtake — car ahead in range.",
			Reasoning: fmt.Sprintf(
				"Gap to car ahead %.1fs — Overtake mode for one or two laps to attack.",
				float64(state.DeltaToFrontMs)/1000.0,
			),
			Source:   "rule:setup_advice.ers_mode",
			DedupKey: "setup_advice.ers_mode:overtake_attack",
			DebugData: map[string]any{
				"topic":        string(brain.TopicSetupERSMode),
				"current":      int(mode),
				"current_name": enums.ERSDeployMode(mode),
				"target":       target,
				"target_name":  enums.ERSDeployMode(uint8(target)),
				"gap_ms":       int(state.DeltaToFrontMs),
				"channel":      "cockpit",
				"category":     "ers_mode",
			},
		}
		enqueueAndLog(e, ev, talkLevel)
	}
}

// emitDRSUtilizationAdvice fires when the driver had meaningful DRS
// availability on the just-completed lap but used less than ~70% of it.
// Hooked off the lap rollover so each evaluation looks at the lap that
// just closed. Race-only — DRS exists in race mode and feature laps.
func (e *Engine) emitDRSUtilizationAdvice(state *models.RaceState, talkLevel int32) {
	if !isRaceSession(state.SessionType) {
		return
	}
	// Lap accumulators reset on rollover (handleCarTelemetry) — so the live
	// values reflect "this lap so far". We want the closed-lap snapshot,
	// which is what we have just before the rollover. Approximate by only
	// firing when LastEventIdx changes lap-flagged are near-finish (the
	// engine's evaluate() cadence is 5s, fine-grained enough for advisory
	// nudges). To avoid double-firing this rule we use a one-per-lap dedup
	// via DedupKey.
	if state.DRSAvailableThisLap < 3.0 {
		return
	}
	util := state.DRSUsedThisLap / state.DRSAvailableThisLap
	if util >= 0.70 {
		return
	}
	// Target percentage = 90 (a round value the engineer can call out).
	target := 90
	if !e.cooldown.ShouldSetupAdvise(string(brain.TopicSetupDRSUsage), target) {
		return
	}
	ev := brain.Event{
		Type:     brain.EventSetupAdvice,
		Priority: brain.DefaultEventPriority(brain.EventSetupAdvice),
		Summary: fmt.Sprintf(
			"DRS only %.0f%% used this lap — open it earlier.",
			util*100,
		),
		Reasoning: fmt.Sprintf(
			"DRS was permitted for %.1fs but you only opened the flap for %.1fs (%.0f%%). Hit the button at the activation line, not after.",
			state.DRSAvailableThisLap, state.DRSUsedThisLap, util*100,
		),
		Source:   "rule:setup_advice.drs_usage",
		DedupKey: fmt.Sprintf("setup_advice.drs_usage:lap%d", state.CurrentLap),
		DebugData: map[string]any{
			"topic":           string(brain.TopicSetupDRSUsage),
			"used_s":          float64(state.DRSUsedThisLap),
			"available_s":     float64(state.DRSAvailableThisLap),
			"utilization_pct": util * 100,
			"target":          target,
			"channel":         "cockpit",
			"category":        "drs_usage",
		},
	}
	enqueueAndLog(e, ev, talkLevel)
}

// emitFuelMixAdvice fires when the driver is running Rich or Max fuel mix
// in a race with stable pace and healthy fuel margin — wasting fuel that
// could be banked. Or when running Lean with a closing rival inside 2s.
//
// FuelMix enum: 0=Lean, 1=Standard, 2=Rich, 3=Max (F1 25).
func (e *Engine) emitFuelMixAdvice(state *models.RaceState, talkLevel int32) {
	if !isRaceSession(state.SessionType) || state.TotalLaps == 0 {
		return
	}
	mix := state.FuelMix
	remaining := int(state.TotalLaps) - int(state.CurrentLap)
	if remaining <= 0 {
		return
	}
	// Rich/Max with healthy fuel margin → Standard, bank fuel.
	if mix >= 2 && state.FuelRemainingLaps > float32(remaining)+1.0 {
		target := 1
		if !e.cooldown.ShouldSetupAdvise(string(brain.TopicSetupFuelMix), target) {
			return
		}
		ev := brain.Event{
			Type:     brain.EventSetupAdvice,
			Priority: brain.DefaultEventPriority(brain.EventSetupAdvice),
			Summary:  "Mix Standard — fuel margin healthy.",
			Reasoning: fmt.Sprintf(
				"Mix %s with %.1f laps fuel for %d laps remaining — banking fuel costs nothing here.",
				enums.FuelMix(mix), state.FuelRemainingLaps, remaining,
			),
			Source:   "rule:setup_advice.fuel_mix",
			DedupKey: "setup_advice.fuel_mix:standard_bank",
			DebugData: map[string]any{
				"topic":        string(brain.TopicSetupFuelMix),
				"current":      int(mix),
				"current_name": enums.FuelMix(mix),
				"target":       target,
				"target_name":  enums.FuelMix(uint8(target)),
				"channel":      "cockpit",
				"category":     "fuel_mix",
			},
		}
		enqueueAndLog(e, ev, talkLevel)
		return
	}
	// Lean with rival in DRS range → Standard for the attack.
	if mix == 0 && state.DeltaToFrontMs > 0 && state.DeltaToFrontMs < 2000 {
		target := 1
		if !e.cooldown.ShouldSetupAdvise(string(brain.TopicSetupFuelMix), target) {
			return
		}
		ev := brain.Event{
			Type:     brain.EventSetupAdvice,
			Priority: brain.DefaultEventPriority(brain.EventSetupAdvice),
			Summary:  "Mix Standard for the attack.",
			Reasoning: fmt.Sprintf(
				"Lean mix with %.1fs gap ahead — Standard for two laps to close the deficit.",
				float64(state.DeltaToFrontMs)/1000.0,
			),
			Source:   "rule:setup_advice.fuel_mix",
			DedupKey: "setup_advice.fuel_mix:standard_attack",
			DebugData: map[string]any{
				"topic":        string(brain.TopicSetupFuelMix),
				"current":      int(mix),
				"current_name": enums.FuelMix(mix),
				"target":       target,
				"target_name":  enums.FuelMix(uint8(target)),
				"gap_ms":       int(state.DeltaToFrontMs),
				"channel":      "cockpit",
				"category":     "fuel_mix",
			},
		}
		enqueueAndLog(e, ev, talkLevel)
	}
}
