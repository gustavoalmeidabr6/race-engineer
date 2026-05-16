package insights

import (
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/brain"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
)

// TestBuildLapSummaryText_Practice covers the practice-shape string.
func TestBuildLapSummaryText_Practice(t *testing.T) {
	state := &models.RaceState{
		SessionType:    1, // P1
		PlayerCarIndex: 19,
		TotalLaps:      0,
		TyresWear:      [4]float32{38, 42, 41, 39},
		FuelInTank:     65.5,
	}
	// completed lap 14, 1:28.421 (88421 ms), s2 +0.300 vs PB.
	summary, reasoning := buildLapSummaryText(
		state, 14,
		88421, 26000, 33000, 29421,
		88121, 26000, 32700, 29421,
		"", // no competitor blurb in this unit test
	)
	if !strings.Contains(summary, "Lap 14") {
		t.Errorf("expected lap number in summary, got %q", summary)
	}
	if !strings.Contains(summary, "1:28") {
		t.Errorf("expected 1:28 lap time fragment, got %q", summary)
	}
	if !strings.Contains(reasoning, "S2 +0.300") {
		t.Errorf("expected S2 +0.300 sector delta, got %q", reasoning)
	}
}

// TestBuildLapSummaryText_RaceShape covers the race-shape string.
func TestBuildLapSummaryText_RaceShape(t *testing.T) {
	state := &models.RaceState{
		SessionType:     10, // Race
		Position:        4,
		TotalLaps:       50,
		DeltaToFrontMs:  1234,
		DeltaToLeaderMs: 8765,
		ActualCompound:  17,
		VisualCompound:  17, // Medium — drives the headline compound name
		TyresAgeLaps:    8,
		FuelInTank:      52.3,
	}
	summary, reasoning := buildLapSummaryText(state, 14, 88421, 0, 0, 0, 0, 0, 0, 0, "")
	if !strings.Contains(summary, "Lap 14 of 50") {
		t.Errorf("expected 'Lap 14 of 50' in race summary, got %q", summary)
	}
	if !strings.Contains(summary, "P4") {
		t.Errorf("expected position P4 in summary, got %q", summary)
	}
	if !strings.Contains(reasoning, "+1.2s") {
		t.Errorf("expected gap-ahead in reasoning, got %q", reasoning)
	}
}

// TestEmitLapSummary_EnqueuesEvent exercises the engine path with a nil
// reader (state-only fallback) and asserts a lap_summary event lands on the
// bus with the right shape.
func TestEmitLapSummary_EnqueuesEvent(t *testing.T) {
	state := &models.RaceState{
		PlayerCarIndex: 19,
		Position:       4,
		CurrentLap:     15,
		TotalLaps:      50,
		SessionType:    10,
		LastLapTimeMs:  88421,
		FuelInTank:     50.0,
	}
	bus := brain.New(func() *models.RaceState { return state }, brain.StaticContext{})

	engine := NewEngine(
		func() *models.RaceState { return state },
		func() int32 { return 10 },
		make(chan models.DrivingInsight, 8),
	)
	engine.SetBrain(bus)
	// reader is intentionally nil — exercise the fallback path.

	engine.emitLapSummary(state, 14, 10)

	events := bus.PeekEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 event on bus, got %d", len(events))
	}
	ev := events[0]
	if ev.Type != brain.EventLapSummary {
		t.Errorf("expected type lap_summary, got %s", ev.Type)
	}
	if ev.Priority != 2 {
		t.Errorf("expected priority 2, got %d", ev.Priority)
	}
	if !strings.Contains(ev.Summary, "Lap 14") {
		t.Errorf("expected lap 14 in summary, got %q", ev.Summary)
	}
	if ev.DedupKey != "lap_summary:14" {
		t.Errorf("expected dedup key lap_summary:14, got %q", ev.DedupKey)
	}
	if _, ok := ev.DebugData["lap_ms"]; !ok {
		t.Errorf("expected lap_ms in debug_data, got %v", ev.DebugData)
	}
}

// TestEmitLapSummary_RespectsTalkLevel verifies the bus drops lap-summary
// events when TalkLevel is below the P2 floor (≤6 lets P3+ only).
func TestEmitLapSummary_RespectsTalkLevel(t *testing.T) {
	state := &models.RaceState{PlayerCarIndex: 19, CurrentLap: 15, LastLapTimeMs: 88000, SessionType: 10}
	bus := brain.New(func() *models.RaceState { return state }, brain.StaticContext{})

	engine := NewEngine(
		func() *models.RaceState { return state },
		func() int32 { return 4 }, // talkLevel 4 → P4+ only, drops P2 lap_summary
		make(chan models.DrivingInsight, 8),
	)
	engine.SetBrain(bus)

	engine.emitLapSummary(state, 14, 4)

	if got := len(bus.PeekEvents()); got != 0 {
		t.Errorf("expected 0 events (TalkLevel filter), got %d", got)
	}
}

// TestTruncate sanity check.
func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("no-truncate path: %q", got)
	}
	if got := truncate(strings.Repeat("a", 100), 10); len(got) != 10 {
		t.Errorf("expected 10-char truncation, got len %d", len(got))
	}
}

// silence unused-variable warning in this file
var _ atomic.Int32
