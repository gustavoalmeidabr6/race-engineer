package insights

import (
	"strings"
	"testing"
	"time"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/brain"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
)

// newPositionChangeEngine wires a brain + engine with an injectable clock
// so tests can advance past the debounce window without sleeping. The
// returned advance() closure pushes the engine's "now" forward.
func newPositionChangeEngine(t *testing.T, state *models.RaceState) (*Engine, *brain.RaceBrain, func(time.Duration)) {
	t.Helper()
	bus := brain.New(func() *models.RaceState { return state }, brain.StaticContext{})
	engine := NewEngine(
		func() *models.RaceState { return state },
		func() int32 { return 10 },
		make(chan models.DrivingInsight, 8),
	)
	engine.SetBrain(bus)
	cur := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	engine.now = func() time.Time { return cur }
	advance := func(d time.Duration) { cur = cur.Add(d) }
	return engine, bus, advance
}

// First tick banks the baseline — no event yet, even though Position is set.
func TestEmitPositionChange_NoFireOnFirstTick(t *testing.T) {
	state := &models.RaceState{Position: 5, CurrentLap: 3, SessionType: 10}
	engine, bus, _ := newPositionChangeEngine(t, state)

	engine.emitPositionChange(state, 10)

	if got := len(bus.PeekEvents()); got != 0 {
		t.Errorf("expected 0 events on baseline tick, got %d", got)
	}
}

// Position gain fires after the settle window expires with a stable
// position. The "from" position is the value before the burst started.
func TestEmitPositionChange_FiresOnGainAfterSettle(t *testing.T) {
	state := &models.RaceState{Position: 5, CurrentLap: 3, SessionType: 10}
	engine, bus, advance := newPositionChangeEngine(t, state)

	engine.emitPositionChange(state, 10) // baseline
	state.Position = 4
	engine.emitPositionChange(state, 10) // captures pending burst, no fire yet
	if got := len(bus.PeekEvents()); got != 0 {
		t.Fatalf("expected 0 events during settle window, got %d", got)
	}

	advance(positionChangeSettleWindow + time.Second)
	engine.emitPositionChange(state, 10) // settle window elapsed → fire

	events := bus.PeekEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 event after settle, got %d", len(events))
	}
	ev := events[0]
	if ev.Type != brain.EventPositionChange {
		t.Errorf("expected type position_change, got %s", ev.Type)
	}
	if ev.Priority != 3 {
		t.Errorf("expected priority 3 (silent), got %d", ev.Priority)
	}
	if !strings.HasPrefix(ev.Summary, "Up to P4") {
		t.Errorf("expected 'Up to P4' prefix, got %q", ev.Summary)
	}
	if ev.DebugData["gained"] != true {
		t.Errorf("expected gained=true in debug, got %#v", ev.DebugData["gained"])
	}
	if ev.DebugData["session_type"] != 10 {
		t.Errorf("expected session_type=10, got %#v", ev.DebugData["session_type"])
	}
}

// Position loss fires with "Down to" framing — separate from emitOvertaken.
func TestEmitPositionChange_FiresOnLoss(t *testing.T) {
	state := &models.RaceState{Position: 5, CurrentLap: 3, SessionType: 10}
	engine, bus, advance := newPositionChangeEngine(t, state)

	engine.emitPositionChange(state, 10)
	state.Position = 6
	engine.emitPositionChange(state, 10)
	advance(positionChangeSettleWindow + time.Second)
	engine.emitPositionChange(state, 10)

	events := bus.PeekEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 event after loss, got %d", len(events))
	}
	if !strings.HasPrefix(events[0].Summary, "Down to P6") {
		t.Errorf("expected 'Down to P6' prefix, got %q", events[0].Summary)
	}
}

// Quali (SessionType=5) still fires — unlike emitOvertaken which is
// race-only. Session metadata flows through DebugData so Gemini Live
// can pick the appropriate phrasing.
func TestEmitPositionChange_FiresInQuali(t *testing.T) {
	state := &models.RaceState{Position: 8, CurrentLap: 2, SessionType: 5}
	engine, bus, advance := newPositionChangeEngine(t, state)

	engine.emitPositionChange(state, 10)
	state.Position = 6
	engine.emitPositionChange(state, 10)
	advance(positionChangeSettleWindow + time.Second)
	engine.emitPositionChange(state, 10)

	events := bus.PeekEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 event in quali, got %d", len(events))
	}
	if events[0].DebugData["is_race"] != false {
		t.Errorf("expected is_race=false for quali, got %#v", events[0].DebugData["is_race"])
	}
}

// A burst of consecutive changes (passed by 3 cars in quick succession)
// collapses to ONE event citing the burst's ORIGINAL "from" and final "to".
// This is the spam suppression the user asked for: before debouncing this
// produced three back-to-back P3 events flooding the brain snapshot.
func TestEmitPositionChange_BurstCollapsesToOne(t *testing.T) {
	state := &models.RaceState{Position: 5, CurrentLap: 3, SessionType: 10}
	engine, bus, advance := newPositionChangeEngine(t, state)

	engine.emitPositionChange(state, 10) // baseline P5

	// Three rapid passes within the settle window: P5 → P6 → P7 → P8.
	state.Position = 6
	engine.emitPositionChange(state, 10)
	advance(1 * time.Second)
	state.Position = 7
	engine.emitPositionChange(state, 10)
	advance(1 * time.Second)
	state.Position = 8
	engine.emitPositionChange(state, 10)
	// Nothing fired yet — burst still in flight.
	if got := len(bus.PeekEvents()); got != 0 {
		t.Fatalf("expected no event mid-burst, got %d", got)
	}

	// Stabilise on P8 past the settle window.
	advance(positionChangeSettleWindow + time.Second)
	engine.emitPositionChange(state, 10)

	events := bus.PeekEvents()
	if len(events) != 1 {
		t.Fatalf("expected ONE coalesced event, got %d", len(events))
	}
	ev := events[0]
	if !strings.HasPrefix(ev.Summary, "Down to P8") {
		t.Errorf("expected coalesced 'Down to P8 (was P5)', got %q", ev.Summary)
	}
	if !strings.Contains(ev.Summary, "was P5") {
		t.Errorf("expected summary to cite original P5, got %q", ev.Summary)
	}
	if ev.DebugData["prev_position"] != 5 {
		t.Errorf("expected prev_position=5 (burst origin), got %#v", ev.DebugData["prev_position"])
	}
	if ev.DebugData["new_position"] != 8 {
		t.Errorf("expected new_position=8, got %#v", ev.DebugData["new_position"])
	}
}

// Oscillating back to the original position within the settle window
// suppresses the event entirely — no net change happened.
func TestEmitPositionChange_OscillatingCancelsEvent(t *testing.T) {
	state := &models.RaceState{Position: 5, CurrentLap: 3, SessionType: 10}
	engine, bus, advance := newPositionChangeEngine(t, state)

	engine.emitPositionChange(state, 10) // baseline P5
	state.Position = 4
	engine.emitPositionChange(state, 10) // start burst
	advance(2 * time.Second)
	state.Position = 5
	engine.emitPositionChange(state, 10) // back to baseline — burst cancelled

	advance(positionChangeSettleWindow + time.Second)
	engine.emitPositionChange(state, 10)

	if got := len(bus.PeekEvents()); got != 0 {
		t.Errorf("expected 0 events when burst oscillated back, got %d", got)
	}
}

// Re-firing the same SETTLED position on the same lap is suppressed by
// the dedup key (session_type:lap:position) — handles repeat bursts to
// the same destination on the same lap.
func TestEmitPositionChange_DedupSameDestinationSameLap(t *testing.T) {
	state := &models.RaceState{Position: 5, CurrentLap: 3, SessionType: 10}
	engine, bus, advance := newPositionChangeEngine(t, state)

	engine.emitPositionChange(state, 10)
	state.Position = 4
	engine.emitPositionChange(state, 10)
	advance(positionChangeSettleWindow + time.Second)
	engine.emitPositionChange(state, 10) // P5 → P4 fires
	state.Position = 5
	engine.emitPositionChange(state, 10)
	advance(positionChangeSettleWindow + time.Second)
	engine.emitPositionChange(state, 10) // P4 → P5 fires (different destination)
	state.Position = 4
	engine.emitPositionChange(state, 10)
	advance(positionChangeSettleWindow + time.Second)
	engine.emitPositionChange(state, 10) // back to P4 same lap — deduped by brain bus

	got := len(bus.PeekEvents())
	if got != 2 {
		t.Errorf("expected 2 events after bounce (P4 deduped second time), got %d", got)
	}
}
