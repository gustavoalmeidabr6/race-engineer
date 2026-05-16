package insights

import (
	"strings"
	"testing"
	"time"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/brain"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
)

func newFuelEngine(t *testing.T, state *models.RaceState) (*Engine, *brain.RaceBrain) {
	t.Helper()
	bus := brain.New(func() *models.RaceState { return state }, brain.StaticContext{})
	engine := NewEngine(
		func() *models.RaceState { return state },
		func() int32 { return 10 },
		make(chan models.DrivingInsight, 8),
	)
	engine.SetBrain(bus)
	return engine, bus
}

// Above the threshold: no event, no matter the rate.
func TestEmitFuelCritical_NoFireAboveThreshold(t *testing.T) {
	state := &models.RaceState{
		SessionType:       10, // race
		FuelRemainingLaps: 5.0,
		FuelInTank:        25.0,
		CurrentLap:        20,
		TotalLaps:         30,
	}
	engine, bus := newFuelEngine(t, state)

	engine.emitFuelCritical(state, 10)

	if got := len(bus.PeekEvents()); got != 0 {
		t.Errorf("expected 0 events above threshold, got %d", got)
	}
}

// Below the threshold in a race: P4 voiced event with full fuel debug data.
func TestEmitFuelCritical_FiresBelowThreshold(t *testing.T) {
	state := &models.RaceState{
		SessionType:       10, // race
		FuelRemainingLaps: 2.4,
		FuelInTank:        4.0,
		FuelMix:           2, // Rich
		CurrentLap:        45,
		TotalLaps:         50,
	}
	engine, bus := newFuelEngine(t, state)

	engine.emitFuelCritical(state, 10)

	events := bus.PeekEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 critical event, got %d", len(events))
	}
	ev := events[0]
	if ev.Type != brain.EventFuelCritical {
		t.Errorf("expected type fuel_critical, got %s", ev.Type)
	}
	if ev.Priority != 4 {
		t.Errorf("expected P4, got %d", ev.Priority)
	}
	if !strings.Contains(ev.Summary, "2.4") {
		t.Errorf("expected '2.4' in summary, got %q", ev.Summary)
	}
	if got, _ := ev.DebugData["laps_remaining"].(int); got != 5 {
		t.Errorf("expected laps_remaining=5, got %v", ev.DebugData["laps_remaining"])
	}
	if got, _ := ev.DebugData["fuel_mix_name"].(string); got != "Rich" {
		t.Errorf("expected fuel_mix_name=Rich, got %v", ev.DebugData["fuel_mix_name"])
	}
}

// Quali (SessionType=5) never fires — fuel is loaded for the session
// purpose; a "critical fuel" call is meaningless noise.
func TestEmitFuelCritical_NoFireInQuali(t *testing.T) {
	state := &models.RaceState{
		SessionType:       5, // Q1
		FuelRemainingLaps: 1.0,
		FuelInTank:        1.5,
		CurrentLap:        2,
		TotalLaps:         0,
	}
	engine, bus := newFuelEngine(t, state)

	engine.emitFuelCritical(state, 10)

	if got := len(bus.PeekEvents()); got != 0 {
		t.Errorf("expected 0 events in quali, got %d", got)
	}
}

// Same lap, repeated below-threshold ticks → only one event lands (dedup).
func TestEmitFuelCritical_DedupsWithinLap(t *testing.T) {
	state := &models.RaceState{
		SessionType:       10,
		FuelRemainingLaps: 2.0,
		FuelInTank:        3.5,
		CurrentLap:        45,
		TotalLaps:         50,
	}
	engine, bus := newFuelEngine(t, state)

	engine.emitFuelCritical(state, 10)
	state.FuelRemainingLaps = 1.8
	engine.emitFuelCritical(state, 10)
	state.FuelRemainingLaps = 1.5
	engine.emitFuelCritical(state, 10)

	if got := len(bus.PeekEvents()); got != 1 {
		t.Errorf("expected 1 event (rest deduped), got %d", got)
	}
}

// Rate emitter needs a full sample window before it fires — partial fills
// stay silent.
func TestEmitFuelRate_NoFireBeforeWindowFull(t *testing.T) {
	state := &models.RaceState{
		SessionType:       10,
		FuelInTank:        50.0,
		FuelRemainingLaps: 20.0,
		CurrentLap:        3,
	}
	engine, bus := newFuelEngine(t, state)

	// Two samples — below fuelRateSamples=5, should never fire.
	engine.emitFuelRate(state, 10)
	state.FuelInTank = 48.0
	engine.emitFuelRate(state, 10)

	if got := len(bus.PeekEvents()); got != 0 {
		t.Errorf("expected 0 events before full window, got %d", got)
	}
}

// Synthesise a full window of high-rate samples and confirm the P2 event
// fires with the right magnitude. Hand-built samples bypass the
// wall-clock-spacing problem that comes from calling emitFuelRate in a
// tight loop.
func TestEmitFuelRate_FiresWhenRateExceedsThreshold(t *testing.T) {
	state := &models.RaceState{
		SessionType:       10,
		FuelInTank:        50.0,
		FuelRemainingLaps: 20.0,
		FuelMix:           2, // Rich
		CurrentLap:        3,
	}
	engine, bus := newFuelEngine(t, state)

	// Burn 1.5kg over 25s → 0.060 kg/s, comfortably above the 0.040
	// threshold. Spread across 5 evenly-spaced samples.
	now := time.Now()
	engine.fuelSamples = []fuelSample{
		{at: now.Add(-25 * time.Second), fuelInTank: 50.0},
		{at: now.Add(-20 * time.Second), fuelInTank: 49.7},
		{at: now.Add(-15 * time.Second), fuelInTank: 49.4},
		{at: now.Add(-10 * time.Second), fuelInTank: 49.1},
		{at: now.Add(-5 * time.Second), fuelInTank: 48.8},
	}
	// Final tick that pushes the newest sample past the threshold —
	// total burned = 50.0 - 48.5 = 1.5kg over ~25s.
	state.FuelInTank = 48.5
	engine.emitFuelRate(state, 10)

	events := bus.PeekEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 rate event, got %d", len(events))
	}
	ev := events[0]
	if ev.Type != brain.EventFuelRate {
		t.Errorf("expected type fuel_rate, got %s", ev.Type)
	}
	if ev.Priority != 2 {
		t.Errorf("expected P2 (silent), got %d", ev.Priority)
	}
	rate, _ := ev.DebugData["rate_kg_per_sec"].(float64)
	if rate < fuelRateHighKgPerSec {
		t.Errorf("expected rate_kg_per_sec ≥ %v, got %v", fuelRateHighKgPerSec, rate)
	}
}

// Critical takes precedence — when FuelRemainingLaps is already in the
// critical band, the rate emitter stays silent to avoid double-broadcasting.
func TestEmitFuelRate_SuppressedWhenCritical(t *testing.T) {
	state := &models.RaceState{
		SessionType:       10,
		FuelInTank:        2.0,
		FuelRemainingLaps: 2.5, // < fuelCriticalLapsThreshold (3.0)
		CurrentLap:        45,
	}
	engine, bus := newFuelEngine(t, state)

	now := time.Now()
	engine.fuelSamples = []fuelSample{
		{at: now.Add(-25 * time.Second), fuelInTank: 4.0},
		{at: now.Add(-20 * time.Second), fuelInTank: 3.6},
		{at: now.Add(-15 * time.Second), fuelInTank: 3.2},
		{at: now.Add(-10 * time.Second), fuelInTank: 2.8},
		{at: now.Add(-5 * time.Second), fuelInTank: 2.4},
	}
	engine.emitFuelRate(state, 10)

	if got := len(bus.PeekEvents()); got != 0 {
		t.Errorf("expected 0 rate events while critical, got %d", got)
	}
}

// In a pit-lap window the rate samples are reset and no event is produced.
func TestEmitFuelRate_PitLapResetsWindow(t *testing.T) {
	state := &models.RaceState{
		SessionType:       10,
		FuelInTank:        30.0,
		FuelRemainingLaps: 15.0,
		PitStatus:         1, // pitting
		CurrentLap:        20,
	}
	engine, bus := newFuelEngine(t, state)

	// Pre-fill the window — pitlap call should wipe it.
	engine.fuelSamples = []fuelSample{
		{at: time.Now().Add(-25 * time.Second), fuelInTank: 35.0},
		{at: time.Now().Add(-20 * time.Second), fuelInTank: 34.0},
	}
	engine.emitFuelRate(state, 10)

	if engine.fuelSamples != nil {
		t.Errorf("expected pit-lap to clear fuel sample window, got %d samples", len(engine.fuelSamples))
	}
	if got := len(bus.PeekEvents()); got != 0 {
		t.Errorf("expected 0 events on pit lap, got %d", got)
	}
}
