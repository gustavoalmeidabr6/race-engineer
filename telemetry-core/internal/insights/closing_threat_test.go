package insights

import (
	"strings"
	"testing"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/brain"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
)

// TestEmitOvertaken_NoFireOnFirstTick — first tick captures the baseline,
// no event yet.
func TestEmitOvertaken_NoFireOnFirstTick(t *testing.T) {
	state := &models.RaceState{
		PlayerCarIndex: 19,
		Position:       3,
		CurrentLap:     5,
		SessionType:    10,
	}
	bus := brain.New(func() *models.RaceState { return state }, brain.StaticContext{})
	engine := NewEngine(
		func() *models.RaceState { return state },
		func() int32 { return 10 },
		make(chan models.DrivingInsight, 8),
	)
	engine.SetBrain(bus)

	engine.emitOvertaken(state, 10)

	if got := len(bus.PeekEvents()); got != 0 {
		t.Errorf("expected 0 events on baseline tick, got %d", got)
	}
}

// TestEmitOvertaken_FiresOnPositionDrop — when Position increases (we got
// passed) the engine emits a P5 overtaken event with prev/new positions
// and the compound.
func TestEmitOvertaken_FiresOnPositionDrop(t *testing.T) {
	state := &models.RaceState{
		PlayerCarIndex: 19,
		Position:       3,
		CurrentLap:     5,
		SessionType:    10,
		VisualCompound: 17, // Medium
		TyresAgeLaps:   12,
	}
	bus := brain.New(func() *models.RaceState { return state }, brain.StaticContext{})
	engine := NewEngine(
		func() *models.RaceState { return state },
		func() int32 { return 10 },
		make(chan models.DrivingInsight, 8),
	)
	engine.SetBrain(bus)

	// First tick captures baseline.
	engine.emitOvertaken(state, 10)
	// Now we drop a position.
	state.Position = 4
	engine.emitOvertaken(state, 10)

	events := bus.PeekEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 event after position drop, got %d", len(events))
	}
	ev := events[0]
	if ev.Type != brain.EventOvertaken {
		t.Errorf("expected type overtaken, got %s", ev.Type)
	}
	if ev.Priority != 5 {
		t.Errorf("expected priority 5, got %d", ev.Priority)
	}
	if !strings.Contains(ev.Summary, "P4") {
		t.Errorf("expected new position P4 in summary, got %q", ev.Summary)
	}
	if !strings.Contains(ev.Reasoning, "P3 to P4") {
		t.Errorf("expected position transition in reasoning, got %q", ev.Reasoning)
	}
	if !strings.Contains(ev.Reasoning, "Medium") {
		t.Errorf("expected decoded compound in reasoning, got %q", ev.Reasoning)
	}
}

// TestEmitOvertaken_DoesNotFireOnPositionGain — if Position decreases (we
// passed someone), no event fires from this rule (that's a different
// future event).
func TestEmitOvertaken_DoesNotFireOnPositionGain(t *testing.T) {
	state := &models.RaceState{
		PlayerCarIndex: 19,
		Position:       4,
		CurrentLap:     5,
		SessionType:    10,
	}
	bus := brain.New(func() *models.RaceState { return state }, brain.StaticContext{})
	engine := NewEngine(
		func() *models.RaceState { return state },
		func() int32 { return 10 },
		make(chan models.DrivingInsight, 8),
	)
	engine.SetBrain(bus)

	// Baseline at P4.
	engine.emitOvertaken(state, 10)
	// Now we GAIN a position (P4 → P3).
	state.Position = 3
	engine.emitOvertaken(state, 10)

	if got := len(bus.PeekEvents()); got != 0 {
		t.Errorf("expected no overtaken event on position GAIN, got %d", got)
	}
}

// TestEmitOvertaken_DedupSameLap — getting passed twice in the same lap
// (rare but possible) should fire only once because the bus dedups by
// the per-lap DedupKey.
func TestEmitOvertaken_DedupSameLap(t *testing.T) {
	state := &models.RaceState{
		PlayerCarIndex: 19,
		Position:       3,
		CurrentLap:     5,
		SessionType:    10,
		VisualCompound: 17,
	}
	bus := brain.New(func() *models.RaceState { return state }, brain.StaticContext{})
	engine := NewEngine(
		func() *models.RaceState { return state },
		func() int32 { return 10 },
		make(chan models.DrivingInsight, 8),
	)
	engine.SetBrain(bus)

	engine.emitOvertaken(state, 10) // baseline
	state.Position = 4
	engine.emitOvertaken(state, 10) // first overtake → fires
	state.Position = 5
	engine.emitOvertaken(state, 10) // second overtake same lap → deduped

	if got := len(bus.PeekEvents()); got != 1 {
		t.Errorf("expected 1 event (second deduped by lap key), got %d", got)
	}
}
