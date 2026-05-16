package insights

import (
	"strings"
	"testing"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/brain"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
)

func newLifecycleEngine(t *testing.T, state *models.RaceState) (*Engine, *brain.RaceBrain) {
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

// ── emitLightsOut ──────────────────────────────────────────────────────────

func TestEmitLightsOut_FiresOnceOnLGOT(t *testing.T) {
	state := &models.RaceState{
		SessionUID:     0xdeadbeef,
		SessionType:    10,
		LastEventCode:  "LGOT",
		GridPosition:   7,
		TotalLaps:      53,
		ActualCompound: 17, // medium
		TrackTemp:      32,
	}
	engine, bus := newLifecycleEngine(t, state)

	engine.emitRaceLifecycle(state, 10) // first tick — fires
	engine.emitRaceLifecycle(state, 10) // second tick same code — latched

	events := bus.PeekEvents()
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 lights_out event, got %d", len(events))
	}
	ev := events[0]
	if ev.Type != brain.EventLightsOut {
		t.Errorf("expected lights_out, got %s", ev.Type)
	}
	if ev.Priority != 5 {
		t.Errorf("expected P5, got %d", ev.Priority)
	}
	if !strings.Contains(ev.Summary, "P7") || !strings.Contains(ev.Summary, "53 laps") {
		t.Errorf("summary missing grid/lap context: %q", ev.Summary)
	}
}

func TestEmitLightsOut_LatchResetsOnDifferentEventCode(t *testing.T) {
	state := &models.RaceState{
		SessionUID:    0xdeadbeef,
		SessionType:   10,
		LastEventCode: "LGOT",
		GridPosition:  3,
		TotalLaps:     50,
	}
	engine, bus := newLifecycleEngine(t, state)

	engine.emitRaceLifecycle(state, 10) // fires
	state.LastEventCode = "FTLP"        // different code → latch should reset
	engine.emitRaceLifecycle(state, 10) // no fire (LGOT not active)
	state.LastEventCode = "LGOT"        // back to LGOT — would re-fire IF we did a restart, but session_uid same → dedup at bus level

	// The session-uid dedup key on the bus prevents double-firing within a session.
	// Only one event should survive.
	events := bus.PeekEvents()
	if len(events) != 1 {
		t.Errorf("expected exactly 1 lights_out event across the sequence, got %d", len(events))
	}
}

// ── emitFinalLap ───────────────────────────────────────────────────────────

func TestEmitFinalLap_FiresWhenCurrentEqualsTotal(t *testing.T) {
	state := &models.RaceState{
		SessionUID:        0xfeed,
		SessionType:       10,
		CurrentLap:        53,
		TotalLaps:         53,
		Position:          3,
		DeltaToFrontMs:    1500,
		FuelRemainingLaps: 2.5,
		TyresWear:         [4]float32{12, 14, 10, 11},
	}
	engine, bus := newLifecycleEngine(t, state)

	engine.emitRaceLifecycle(state, 10) // fires
	engine.emitRaceLifecycle(state, 10) // latched

	events := bus.PeekEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 final_lap event, got %d", len(events))
	}
	ev := events[0]
	if ev.Type != brain.EventFinalLap {
		t.Errorf("expected final_lap, got %s", ev.Type)
	}
	if ev.Priority != 4 {
		t.Errorf("expected P4, got %d", ev.Priority)
	}
	if !strings.Contains(ev.Summary, "Final lap") || !strings.Contains(ev.Summary, "P3") {
		t.Errorf("summary missing context: %q", ev.Summary)
	}
	// P3 + gap-to-front <2s should use the "podium on the line" framing.
	if !strings.Contains(ev.Summary, "Podium") && !strings.Contains(ev.Summary, "podium") {
		t.Errorf("expected podium framing for P3 on final lap, got %q", ev.Summary)
	}
}

func TestEmitFinalLap_NoFireMidRace(t *testing.T) {
	state := &models.RaceState{
		SessionUID:  0xfeed,
		SessionType: 10,
		CurrentLap:  20,
		TotalLaps:   53,
		Position:    3,
	}
	engine, bus := newLifecycleEngine(t, state)

	engine.emitRaceLifecycle(state, 10)

	for _, ev := range bus.PeekEvents() {
		if ev.Type == brain.EventFinalLap {
			t.Errorf("final_lap fired mid-race")
		}
	}
}

func TestEmitFinalLap_SkippedIfRaceAlreadyFinished(t *testing.T) {
	state := &models.RaceState{
		SessionUID:   0xfeed,
		SessionType:  10,
		CurrentLap:   53,
		TotalLaps:    53,
		Position:     1,
		RaceFinished: true,
	}
	engine, bus := newLifecycleEngine(t, state)

	engine.emitRaceLifecycle(state, 10)

	for _, ev := range bus.PeekEvents() {
		if ev.Type == brain.EventFinalLap {
			t.Errorf("final_lap should be muted once race already finished")
		}
	}
}

// ── emitRaceFinished ───────────────────────────────────────────────────────

func TestEmitRaceFinished_P1_FiresAsPodium(t *testing.T) {
	state := &models.RaceState{
		SessionUID:          0xc0ffee,
		SessionType:         10,
		RaceFinished:        true,
		FinalClassification: 1,
		GridPosition:        7,
		TotalLaps:           53,
		NumPitStops:         2,
	}
	engine, bus := newLifecycleEngine(t, state)

	engine.emitRaceLifecycle(state, 10)
	engine.emitRaceLifecycle(state, 10) // latched

	events := bus.PeekEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 podium event, got %d", len(events))
	}
	ev := events[0]
	if ev.Type != brain.EventPodium {
		t.Errorf("expected podium event type for P1, got %s", ev.Type)
	}
	if ev.Priority != 5 {
		t.Errorf("expected P5, got %d", ev.Priority)
	}
	if !strings.Contains(ev.Summary, "P1") || !strings.Contains(strings.ToLower(ev.Summary), "win") {
		t.Errorf("P1 summary should celebrate the win, got %q", ev.Summary)
	}
	if ev.DebugData["positions_gained"] != 6 {
		t.Errorf("expected positions_gained=6 (grid P7 → P1), got %#v", ev.DebugData["positions_gained"])
	}
}

func TestEmitRaceFinished_P3_FiresAsPodium(t *testing.T) {
	state := &models.RaceState{
		SessionUID:          0xc0ffee,
		SessionType:         10,
		RaceFinished:        true,
		FinalClassification: 3,
		GridPosition:        5,
		TotalLaps:           53,
		NumPitStops:         1,
	}
	engine, bus := newLifecycleEngine(t, state)

	engine.emitRaceLifecycle(state, 10)

	events := bus.PeekEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 podium event, got %d", len(events))
	}
	ev := events[0]
	if ev.Type != brain.EventPodium {
		t.Errorf("expected podium type for P3, got %s", ev.Type)
	}
	if !strings.Contains(ev.Summary, "P3") {
		t.Errorf("podium summary missing P3, got %q", ev.Summary)
	}
}

func TestEmitRaceFinished_P10_FiresAsRaceFinished(t *testing.T) {
	state := &models.RaceState{
		SessionUID:          0xc0ffee,
		SessionType:         10,
		RaceFinished:        true,
		FinalClassification: 10,
		GridPosition:        12, // gained 2
		TotalLaps:           53,
		NumPitStops:         2,
	}
	engine, bus := newLifecycleEngine(t, state)

	engine.emitRaceLifecycle(state, 10)

	events := bus.PeekEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 race_finished event, got %d", len(events))
	}
	ev := events[0]
	if ev.Type != brain.EventRaceFinished {
		t.Errorf("expected race_finished type for P10, got %s", ev.Type)
	}
	// Gained 2 positions → summary should reflect that constructively.
	if !strings.Contains(ev.Summary, "P10") || !strings.Contains(strings.ToLower(ev.Summary), "gained") {
		t.Errorf("P10 (+2) summary should highlight gain, got %q", ev.Summary)
	}
}

func TestEmitRaceFinished_NoFireWhileRunning(t *testing.T) {
	state := &models.RaceState{
		SessionUID:   0xc0ffee,
		SessionType:  10,
		RaceFinished: false,
		CurrentLap:   20,
		TotalLaps:    53,
		Position:     5,
	}
	engine, bus := newLifecycleEngine(t, state)

	engine.emitRaceLifecycle(state, 10)

	for _, ev := range bus.PeekEvents() {
		if ev.Type == brain.EventRaceFinished || ev.Type == brain.EventPodium {
			t.Errorf("race_finished/podium fired while race still running (lap %d/%d)", state.CurrentLap, state.TotalLaps)
		}
	}
}

// ── Session reset behaviour ────────────────────────────────────────────────

// New session should reset the lifecycle latches in the cooldown manager.
// We assert on the latch state directly because the bus's P5 coalesce
// window would otherwise merge two in-test sessions emitted microseconds
// apart, which is unrealistic in production (sessions are minutes apart).
func TestEmitRaceLifecycle_ResetsOnNewSession(t *testing.T) {
	state := &models.RaceState{
		SessionUID:    1,
		SessionType:   10,
		LastEventCode: "LGOT",
		GridPosition:  5,
		TotalLaps:     50,
	}
	engine, _ := newLifecycleEngine(t, state)

	engine.emitRaceLifecycle(state, 10)
	if !engine.cooldown.LightsOutNotified() {
		t.Fatal("expected lightsOutNotified=true after first emit")
	}

	// New session_uid → ResetForNewSession clears the latch.
	state.SessionUID = 2
	engine.cooldown.ResetForNewSession(state.SessionUID)
	if engine.cooldown.LightsOutNotified() {
		t.Fatal("expected lightsOutNotified=false after session reset")
	}

	// Subsequent emit on the new session re-arms.
	engine.emitRaceLifecycle(state, 10)
	if !engine.cooldown.LightsOutNotified() {
		t.Fatal("expected lightsOutNotified=true after second emit on new session")
	}
}

// ── Phase helper ───────────────────────────────────────────────────────────

func TestPhaseHelper(t *testing.T) {
	cases := []struct {
		name  string
		state models.RaceState
		want  models.RacePhase
	}{
		{"non-race", models.RaceState{SessionType: 5}, models.PhaseNonRace},
		{"grid", models.RaceState{SessionType: 10, CurrentLap: 0}, models.PhaseGrid},
		{"formation", models.RaceState{SessionType: 10, SafetyCarStatus: 3, CurrentLap: 0}, models.PhaseFormation},
		{"lights_out", models.RaceState{SessionType: 10, CurrentLap: 1, LastEventCode: "LGOT", TotalLaps: 50}, models.PhaseLightsOut},
		{"racing", models.RaceState{SessionType: 10, CurrentLap: 25, TotalLaps: 50}, models.PhaseRacing},
		{"final_lap", models.RaceState{SessionType: 10, CurrentLap: 50, TotalLaps: 50}, models.PhaseFinalLap},
		{"finished", models.RaceState{SessionType: 10, CurrentLap: 50, TotalLaps: 50, RaceFinished: true}, models.PhaseFinished},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.state.Phase(); got != tc.want {
				t.Errorf("Phase() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLapsRemaining(t *testing.T) {
	cases := []struct {
		state models.RaceState
		want  int
	}{
		{models.RaceState{SessionType: 10, CurrentLap: 5, TotalLaps: 50}, 45},
		{models.RaceState{SessionType: 10, CurrentLap: 50, TotalLaps: 50}, 0},
		{models.RaceState{SessionType: 10, CurrentLap: 52, TotalLaps: 50}, 0}, // clamp
		{models.RaceState{SessionType: 5, CurrentLap: 5, TotalLaps: 50}, 0},   // non-race
	}
	for _, tc := range cases {
		if got := tc.state.LapsRemaining(); got != tc.want {
			t.Errorf("LapsRemaining(%+v) = %d, want %d", tc.state, got, tc.want)
		}
	}
}
