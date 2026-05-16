package insights

import (
	"fmt"
	"time"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/brain"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/enums"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
)

// fuel.go fires two complementary brain events about the player's fuel:
//
//   - emitFuelCritical (P4): voiced. FuelRemainingLaps below the critical
//     threshold — on current consumption the driver may not finish the
//     race. Per-lap deduped so we don't nag every 5s tick.
//
//   - emitFuelRate (P2): silent context. The 25-second burn-rate trend is
//     above the "rich running" threshold. Gemini Live reads this in the
//     brain snapshot and decides whether to mention it (e.g. "mix lean" or
//     "0.04 kg/s — knock it back"); it never triggers TTS itself.
//
// Both gate to race sessions: in practice/quali, fuel is loaded for the
// session purpose (low-fuel quali runs are intentional) so a "critical
// fuel" call there is meaningless noise. The legacy ruleFuelLow at
// rules.go:129 still fires its P5 DrivingInsight via the CommsGate path —
// these brain events run in parallel for the Live agent's typed context.

const (
	// fuelCriticalLapsThreshold matches the legacy ruleFuelLow critical
	// boundary. Kept in sync so both paths trigger together.
	fuelCriticalLapsThreshold float32 = 3.0

	// fuelRateSamples is the number of 5 s engine ticks we hold for the
	// kg/sec rate calculation. 5 → ~25 s window, long enough to smooth a
	// single corner's full-throttle burst but short enough to catch a
	// driver switching to rich mix.
	fuelRateSamples = 5

	// fuelRateHighKgPerSec is the steady-state threshold. F1 25 burn is
	// typically 0.010–0.025 kg/s on standard/lean mix; rich and qualifying
	// modes push toward 0.035–0.045. Above 0.040 sustained over 25 s the
	// driver is using more than a normal lap budget.
	fuelRateHighKgPerSec = 0.040
)

// fuelSample is one reading of (when, FuelInTank in kg). emitFuelRate
// keeps a ring of these and reads the rate as ΔFuelInTank / Δt across the
// oldest and newest entries in the ring.
type fuelSample struct {
	at         time.Time
	fuelInTank float32
}

// emitFuelCritical fires a P4 brain event the moment FuelRemainingLaps
// drops below the critical floor. Race sessions only — fuel is irrelevant
// to a practice/quali driver who fuelled for the session. Per-lap dedup
// prevents 5 s-tick spam while the driver is in the danger zone.
func (e *Engine) emitFuelCritical(state *models.RaceState, talkLevel int32) {
	if state == nil {
		return
	}
	if !isRaceSession(state.SessionType) {
		return
	}
	// FuelRemainingLaps == 0 happens during session warm-up before the
	// car_status packet has been seen. Skip until we have a real reading.
	if state.FuelRemainingLaps <= 0 {
		return
	}
	if state.FuelRemainingLaps >= fuelCriticalLapsThreshold {
		return
	}

	lapsRemaining := int(state.TotalLaps) - int(state.CurrentLap)
	mixName := enums.FuelMix(state.FuelMix)

	summary := fmt.Sprintf("Fuel critical — %.1f laps left in tank.", state.FuelRemainingLaps)
	reasoning := fmt.Sprintf(
		"FuelRemainingLaps=%.2f on lap %d/%d (%d laps to go), tank=%.1fkg, mix=%s.",
		state.FuelRemainingLaps, state.CurrentLap, state.TotalLaps,
		lapsRemaining, state.FuelInTank, mixName,
	)

	debug := map[string]any{
		"fuel_remaining_laps": float64(state.FuelRemainingLaps),
		"fuel_in_tank_kg":     float64(state.FuelInTank),
		"fuel_mix":            int(state.FuelMix),
		"fuel_mix_name":       mixName,
		"current_lap":         int(state.CurrentLap),
		"total_laps":          int(state.TotalLaps),
		"laps_remaining":      lapsRemaining,
		"threshold_laps":      float64(fuelCriticalLapsThreshold),
	}

	ev := brain.Event{
		Type:      brain.EventFuelCritical,
		Priority:  brain.DefaultEventPriority(brain.EventFuelCritical), // 4 — voiced
		Summary:   summary,
		Reasoning: reasoning,
		DebugData: debug,
		Source:    "rule:fuel_critical",
		DedupKey:  fmt.Sprintf("fuel_critical:%d", state.CurrentLap),
	}
	enqueueAndLog(e, ev, talkLevel)
}

// emitFuelRate samples FuelInTank over a 25 s window and fires a P2
// silent context event when the average burn rate exceeds the "rich
// running" threshold. Suppressed when fuel is already critical — at that
// point the P4 critical event is the right channel and a rate note is
// just noise.
func (e *Engine) emitFuelRate(state *models.RaceState, talkLevel int32) {
	if state == nil {
		return
	}
	if !isRaceSession(state.SessionType) {
		e.fuelSamples = nil
		return
	}
	if state.FuelInTank <= 0 {
		return
	}
	// Pit-lap fuel readings are meaningless for rate calculation — the
	// car is not at race pace. Discard the window and skip.
	if state.PitStatus != 0 {
		e.fuelSamples = nil
		return
	}
	// Don't double-broadcast — critical owns the radio while we're below
	// the floor.
	if state.FuelRemainingLaps > 0 && state.FuelRemainingLaps < fuelCriticalLapsThreshold {
		return
	}

	now := time.Now()
	e.fuelSamples = append(e.fuelSamples, fuelSample{at: now, fuelInTank: state.FuelInTank})
	if len(e.fuelSamples) > fuelRateSamples {
		e.fuelSamples = e.fuelSamples[len(e.fuelSamples)-fuelRateSamples:]
	}

	// Need the full window for a stable rate.
	if len(e.fuelSamples) < fuelRateSamples {
		return
	}

	oldest := e.fuelSamples[0]
	newest := e.fuelSamples[len(e.fuelSamples)-1]
	dt := newest.at.Sub(oldest.at).Seconds()
	if dt <= 0 {
		return
	}
	burnedKg := float64(oldest.fuelInTank - newest.fuelInTank)
	if burnedKg <= 0 {
		// Refuel or sensor jitter — reset the window so we don't fire on
		// noise straddling the event.
		e.fuelSamples = e.fuelSamples[len(e.fuelSamples)-1:]
		return
	}
	ratePerSec := burnedKg / dt
	if ratePerSec < fuelRateHighKgPerSec {
		return
	}

	mixName := enums.FuelMix(state.FuelMix)
	summary := fmt.Sprintf("Burn rate high — %.3f kg/s (%s mix).", ratePerSec, mixName)
	reasoning := fmt.Sprintf(
		"Burned %.2fkg over the last %.0fs (%.3f kg/s). On %s mix, lap %d. FuelRemainingLaps=%.1f.",
		burnedKg, dt, ratePerSec, mixName, state.CurrentLap, state.FuelRemainingLaps,
	)

	debug := map[string]any{
		"rate_kg_per_sec":     ratePerSec,
		"burned_kg":           burnedKg,
		"window_sec":          dt,
		"samples":             len(e.fuelSamples),
		"fuel_in_tank_kg":     float64(state.FuelInTank),
		"fuel_remaining_laps": float64(state.FuelRemainingLaps),
		"fuel_mix":            int(state.FuelMix),
		"fuel_mix_name":       mixName,
		"threshold_kg_per_sec": float64(fuelRateHighKgPerSec),
	}

	ev := brain.Event{
		Type:      brain.EventFuelRate,
		Priority:  brain.DefaultEventPriority(brain.EventFuelRate), // 2 — silent
		Summary:   summary,
		Reasoning: reasoning,
		DebugData: debug,
		Source:    "rule:fuel_rate",
		DedupKey:  fmt.Sprintf("fuel_rate:%d", state.CurrentLap),
	}
	enqueueAndLog(e, ev, talkLevel)
}
