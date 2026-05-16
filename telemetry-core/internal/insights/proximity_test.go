package insights

import (
	"strings"
	"testing"
	"time"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/brain"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
)

func newProxWatcherFor(state *models.RaceState) (*ProximityWatcher, *brain.RaceBrain) {
	bus := brain.New(func() *models.RaceState { return state }, brain.StaticContext{})
	w := NewProximityWatcher(
		func() *models.RaceState { return state },
		bus,
		func() int32 { return 5 },
		1*time.Second,
	)
	return w, bus
}

// seedHistory pre-populates per-car samples on the watcher so we can test
// firing logic without waiting real wall-clock seconds.
func seedHistory(w *ProximityWatcher, carIdx uint8, samples []proxSample, armed bool) {
	w.cars[carIdx] = &proxCarHistory{samples: samples, lastSeen: samples[len(samples)-1].at, armed: armed}
}

// TestNearestBehindOnTrack — basic geometry: pick the car whose lap
// distance is just behind ours, ignoring cars in pits / garage.
func TestNearestBehindOnTrack(t *testing.T) {
	st := &models.RaceState{TrackLength: 5891, PlayerCarIndex: 19}
	st.GridPositions[19] = models.GridPosition{CarIndex: 19, LapDistance: 1000, ResultStatus: 2, DriverStatus: 4}
	st.GridPositions[3] = models.GridPosition{CarIndex: 3, LapDistance: 900, ResultStatus: 2, DriverStatus: 4}  // 100m behind
	st.GridPositions[5] = models.GridPosition{CarIndex: 5, LapDistance: 950, ResultStatus: 2, DriverStatus: 4}  // 50m behind — closest
	st.GridPositions[7] = models.GridPosition{CarIndex: 7, LapDistance: 1100, ResultStatus: 2, DriverStatus: 4} // ahead
	st.GridPositions[8] = models.GridPosition{CarIndex: 8, LapDistance: 990, ResultStatus: 2, DriverStatus: 4, PitStatus: 1}
	st.GridPositions[9] = models.GridPosition{CarIndex: 9, LapDistance: 980, ResultStatus: 2, DriverStatus: 0}

	idx, d := nearestBehindOnTrack(st)
	if idx != 5 {
		t.Errorf("expected nearest behind = 5, got %d", idx)
	}
	if d < 49 || d > 51 {
		t.Errorf("expected distance ~50m, got %.1f", d)
	}
}

// TestNearestBehindOnTrack_Wrap — when player is just past start/finish,
// a car near the end of the lap is still counted as behind.
func TestNearestBehindOnTrack_Wrap(t *testing.T) {
	lapLen := float32(5891)
	st := &models.RaceState{TrackLength: uint16(lapLen), PlayerCarIndex: 19}
	st.GridPositions[19] = models.GridPosition{CarIndex: 19, LapDistance: 50, ResultStatus: 2, DriverStatus: 4}
	st.GridPositions[3] = models.GridPosition{CarIndex: 3, LapDistance: lapLen - 30, ResultStatus: 2, DriverStatus: 4}

	idx, d := nearestBehindOnTrack(st)
	if idx != 3 {
		t.Errorf("expected wrap-behind car = 3, got %d", idx)
	}
	if d < 79 || d > 81 {
		t.Errorf("expected wrapped distance ~80m, got %.1f", d)
	}
}

// TestProximityWatcher_FiresOnLowTTI — car closing fast behind, TTI < 8s,
// fires P2.
func TestProximityWatcher_FiresOnLowTTI(t *testing.T) {
	st := &models.RaceState{
		TrackLength:    5891,
		PlayerCarIndex: 19,
		Position:       3,
		CurrentLap:     5,
		Speed:          200,
		SessionType:    1,
		DriverStatus:   4,
		PitStatus:      0,
	}
	st.GridPositions[19] = models.GridPosition{CarIndex: 19, LapDistance: 1000, ResultStatus: 2, DriverStatus: 4}
	// Place car 3 at 200m behind (signed = -200) — at 30 m/s closure → 6.7s TTI.
	st.GridPositions[3] = models.GridPosition{CarIndex: 3, LapDistance: 800, ResultStatus: 2, DriverStatus: 4}

	w, bus := newProxWatcherFor(st)
	now := time.Now()
	// 4 prior samples showing the car closing from 320m → 200m over 4s.
	// closure = (320 - 200) / 4s = 30 m/s. TTI = 200 / 30 = 6.7s < 8s → fire.
	seedHistory(w, 3, []proxSample{
		{at: now.Add(-4 * time.Second), signedM: -320},
		{at: now.Add(-3 * time.Second), signedM: -290},
		{at: now.Add(-2 * time.Second), signedM: -260},
		{at: now.Add(-1 * time.Second), signedM: -230},
	}, true) // pre-armed

	w.tick()

	events := bus.PeekEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.Type != brain.EventCarApproaching {
		t.Errorf("expected EventCarApproaching, got %s", ev.Type)
	}
	if ev.Priority != 2 {
		t.Errorf("expected P2, got %d", ev.Priority)
	}
	if !strings.Contains(ev.Summary, "to contact") {
		t.Errorf("summary should mention contact ETA, got %q", ev.Summary)
	}
}

// TestProximityWatcher_NoFireWhenTTIHigh — car closing slowly enough
// that TTI stays above the 8s threshold.
func TestProximityWatcher_NoFireWhenTTIHigh(t *testing.T) {
	st := &models.RaceState{
		TrackLength:    5891,
		PlayerCarIndex: 19,
		Speed:          200,
		SessionType:    1,
		DriverStatus:   4,
	}
	st.GridPositions[19] = models.GridPosition{CarIndex: 19, LapDistance: 1000, ResultStatus: 2, DriverStatus: 4}
	// Car 800m behind, closing at 50m/4s = 12.5 m/s → TTI = 800/12.5 = 64s.
	st.GridPositions[3] = models.GridPosition{CarIndex: 3, LapDistance: 200, ResultStatus: 2, DriverStatus: 4}

	w, bus := newProxWatcherFor(st)
	now := time.Now()
	seedHistory(w, 3, []proxSample{
		{at: now.Add(-4 * time.Second), signedM: -850},
		{at: now.Add(-2 * time.Second), signedM: -825},
		{at: now.Add(-1 * time.Second), signedM: -812},
	}, true)
	w.tick()

	if got := len(bus.PeekEvents()); got != 0 {
		t.Errorf("expected no event with TTI ≫ 8s, got %d", got)
	}
}

// TestProximityWatcher_NoFireWhenOpening — car moving away should never fire.
func TestProximityWatcher_NoFireWhenOpening(t *testing.T) {
	st := &models.RaceState{
		TrackLength:    5891,
		PlayerCarIndex: 19,
		Speed:          200,
		SessionType:    1,
		DriverStatus:   4,
	}
	st.GridPositions[19] = models.GridPosition{CarIndex: 19, LapDistance: 1000, ResultStatus: 2, DriverStatus: 4}
	st.GridPositions[3] = models.GridPosition{CarIndex: 3, LapDistance: 850, ResultStatus: 2, DriverStatus: 4}

	w, bus := newProxWatcherFor(st)
	now := time.Now()
	// Distance growing — car opening up.
	seedHistory(w, 3, []proxSample{
		{at: now.Add(-3 * time.Second), signedM: -100},
		{at: now.Add(-2 * time.Second), signedM: -130},
		{at: now.Add(-1 * time.Second), signedM: -150},
	}, true)
	w.tick()

	if got := len(bus.PeekEvents()); got != 0 {
		t.Errorf("expected no event when car is opening, got %d", got)
	}
}

// TestProximityWatcher_FiresOnOutLap — prime use case: slow out-lap,
// hot-lap car behind closing fast.
func TestProximityWatcher_FiresOnOutLap(t *testing.T) {
	st := &models.RaceState{
		TrackLength:    5891,
		PlayerCarIndex: 19,
		Speed:          90,
		SessionType:    5, // Q1
		DriverStatus:   3, // Out Lap — was previously silenced
		PitStatus:      0,
	}
	st.GridPositions[19] = models.GridPosition{CarIndex: 19, LapDistance: 1000, ResultStatus: 2, DriverStatus: 3}
	st.GridPositions[7] = models.GridPosition{CarIndex: 7, LapDistance: 850, ResultStatus: 2, DriverStatus: 1}

	w, bus := newProxWatcherFor(st)
	now := time.Now()
	// Closing at ~40 m/s — TTI = 150 / 40 = 3.75s
	seedHistory(w, 7, []proxSample{
		{at: now.Add(-3 * time.Second), signedM: -270},
		{at: now.Add(-2 * time.Second), signedM: -230},
		{at: now.Add(-1 * time.Second), signedM: -190},
	}, true)
	w.tick()

	if got := len(bus.PeekEvents()); got != 1 {
		t.Fatalf("expected 1 event on out-lap closure, got %d", got)
	}
}

// TestProximityWatcher_PerCarCooldown — same car shouldn't refire within
// the cooldown window even if still closing.
func TestProximityWatcher_PerCarCooldown(t *testing.T) {
	st := &models.RaceState{
		TrackLength:    5891,
		PlayerCarIndex: 19,
		CurrentLap:     5,
		Speed:          200,
		DriverStatus:   4,
	}
	st.GridPositions[19] = models.GridPosition{CarIndex: 19, LapDistance: 1000, ResultStatus: 2, DriverStatus: 4}
	st.GridPositions[3] = models.GridPosition{CarIndex: 3, LapDistance: 850, ResultStatus: 2, DriverStatus: 4}

	w, bus := newProxWatcherFor(st)
	now := time.Now()
	seedHistory(w, 3, []proxSample{
		{at: now.Add(-3 * time.Second), signedM: -270},
		{at: now.Add(-2 * time.Second), signedM: -230},
		{at: now.Add(-1 * time.Second), signedM: -190},
	}, true)
	w.tick()
	first := len(bus.PeekEvents())
	w.tick() // immediate retick — should be suppressed
	second := len(bus.PeekEvents())

	if first != 1 {
		t.Errorf("first tick: expected 1 fire, got %d", first)
	}
	if second != 1 {
		t.Errorf("second tick (cooldown): expected still 1 event total, got %d", second)
	}
}

// TestProximityWatcher_SnapshotPublished — Snapshot() returns ahead/behind
// lists with closing rates.
func TestProximityWatcher_SnapshotPublished(t *testing.T) {
	st := &models.RaceState{
		TrackLength:    5891,
		PlayerCarIndex: 19,
		Speed:          150,
		SessionType:    10,
		DriverStatus:   4,
	}
	st.GridPositions[19] = models.GridPosition{CarIndex: 19, LapDistance: 1000, ResultStatus: 2, DriverStatus: 4}
	st.GridPositions[3] = models.GridPosition{CarIndex: 3, LapDistance: 850, ResultStatus: 2, DriverStatus: 4}  // 150m behind
	st.GridPositions[5] = models.GridPosition{CarIndex: 5, LapDistance: 1300, ResultStatus: 2, DriverStatus: 4} // 300m ahead
	st.GridPositions[7] = models.GridPosition{CarIndex: 7, LapDistance: 4500, ResultStatus: 2, DriverStatus: 4} // out of range

	w, _ := newProxWatcherFor(st)
	// Two ticks 1s apart so closing rate can compute.
	now := time.Now()
	seedHistory(w, 3, []proxSample{
		{at: now.Add(-1 * time.Second), signedM: -180},
	}, false)
	seedHistory(w, 5, []proxSample{
		{at: now.Add(-1 * time.Second), signedM: 320},
	}, false)
	w.tick()

	snap := w.Snapshot()
	if snap == nil {
		t.Fatal("expected snapshot")
	}
	if len(snap.NearbyBehind) != 1 || snap.NearbyBehind[0].CarIndex != 3 {
		t.Errorf("expected behind=[3], got %+v", snap.NearbyBehind)
	}
	if len(snap.NearbyAhead) != 1 || snap.NearbyAhead[0].CarIndex != 5 {
		t.Errorf("expected ahead=[5], got %+v", snap.NearbyAhead)
	}
	if snap.WatchRadiusM != proximityWatchM {
		t.Errorf("watch radius unset: %v", snap.WatchRadiusM)
	}
}

// TestProximityWatcher_SilentInPitsAndGarage — pit lane / pit area /
// garage all silence the watcher entirely (no events, empty snapshot).
func TestProximityWatcher_SilentInPitsAndGarage(t *testing.T) {
	cases := []struct {
		name      string
		ps        uint8
		ds        uint8
		wantFires bool
	}{
		{"pit lane", 1, 4, false},
		{"in pit area", 2, 4, false},
		{"in garage", 0, 0, false},
		{"in lap", 0, 2, true},
		{"out lap", 0, 3, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := &models.RaceState{
				TrackLength:    5891,
				PlayerCarIndex: 19,
				Speed:          80,
				PitStatus:      c.ps,
				DriverStatus:   c.ds,
			}
			st.GridPositions[19] = models.GridPosition{CarIndex: 19, LapDistance: 1000, ResultStatus: 2, DriverStatus: c.ds}
			st.GridPositions[3] = models.GridPosition{CarIndex: 3, LapDistance: 850, ResultStatus: 2, DriverStatus: 4}

			w, bus := newProxWatcherFor(st)
			now := time.Now()
			seedHistory(w, 3, []proxSample{
				{at: now.Add(-3 * time.Second), signedM: -270},
				{at: now.Add(-2 * time.Second), signedM: -230},
				{at: now.Add(-1 * time.Second), signedM: -190},
			}, true)
			w.tick()

			got := len(bus.PeekEvents())
			if c.wantFires && got == 0 {
				t.Errorf("expected event while %s, got none", c.name)
			}
			if !c.wantFires && got != 0 {
				t.Errorf("expected no event while %s, got %d", c.name, got)
			}
		})
	}
}

// TestProximityWatcher_CoalescesMultipleCars — when 2+ cars behind cross
// the TTI threshold in the same tick, the watcher emits ONE bundled event
// listing every closer in DebugData (sorted by ETA ascending), not N
// separate per-car events.
func TestProximityWatcher_CoalescesMultipleCars(t *testing.T) {
	st := &models.RaceState{
		TrackLength:    5891,
		PlayerCarIndex: 19,
		Position:       1, // P+1 = car at position 2 would be skipped — none of our test cars sit there.
		CurrentLap:     5,
		Speed:          200,
		SessionType:    1, // P1 — non-race so closing_threat.go stays out of it
		DriverStatus:   4,
		PitStatus:      0,
	}
	st.GridPositions[19] = models.GridPosition{CarIndex: 19, LapDistance: 1000, ResultStatus: 2, DriverStatus: 4, Position: 1}
	st.GridPositions[3] = models.GridPosition{CarIndex: 3, LapDistance: 850, ResultStatus: 2, DriverStatus: 4, Position: 3}
	st.GridPositions[7] = models.GridPosition{CarIndex: 7, LapDistance: 800, ResultStatus: 2, DriverStatus: 4, Position: 4}
	st.GridPositions[11] = models.GridPosition{CarIndex: 11, LapDistance: 700, ResultStatus: 2, DriverStatus: 4, Position: 5}

	w, bus := newProxWatcherFor(st)
	now := time.Now()
	// car 3: 150m behind closing 40m/s → TTI 3.75s
	seedHistory(w, 3, []proxSample{
		{at: now.Add(-3 * time.Second), signedM: -270},
		{at: now.Add(-2 * time.Second), signedM: -230},
		{at: now.Add(-1 * time.Second), signedM: -190},
	}, true)
	// car 7: 200m behind closing 30m/s → TTI 6.7s
	seedHistory(w, 7, []proxSample{
		{at: now.Add(-3 * time.Second), signedM: -290},
		{at: now.Add(-2 * time.Second), signedM: -260},
		{at: now.Add(-1 * time.Second), signedM: -230},
	}, true)
	// car 11: 300m behind closing ~50m/s → TTI ~6.0s
	seedHistory(w, 11, []proxSample{
		{at: now.Add(-3 * time.Second), signedM: -450},
		{at: now.Add(-2 * time.Second), signedM: -400},
		{at: now.Add(-1 * time.Second), signedM: -350},
	}, true)

	w.tick()

	events := bus.PeekEvents()
	if len(events) != 1 {
		t.Fatalf("expected ONE bundled event, got %d", len(events))
	}
	ev := events[0]
	if ev.Type != brain.EventCarApproaching {
		t.Errorf("expected EventCarApproaching, got %s", ev.Type)
	}
	count, _ := ev.DebugData["behind_count"].(int)
	if count != 3 {
		t.Errorf("expected behind_count=3 in DebugData, got %d", count)
	}
	cars, ok := ev.DebugData["behind_cars"].([]map[string]any)
	if !ok || len(cars) != 3 {
		t.Fatalf("expected behind_cars list of 3, got %T len=%d", ev.DebugData["behind_cars"], len(cars))
	}
	// Sorted by ETA ascending: 3 (3.75s) < 11 (~6.0s) < 7 (~6.7s).
	if got := cars[0]["car_index"].(int); got != 3 {
		t.Errorf("expected lead car_index=3, got %d", got)
	}
	if got := cars[2]["car_index"].(int); got != 7 {
		t.Errorf("expected tail car_index=7, got %d", got)
	}
	for _, c := range cars {
		if _, ok := c["eta_sec"].(float32); !ok {
			t.Errorf("entry missing eta_sec: %+v", c)
		}
		if _, ok := c["distance_m"].(float32); !ok {
			t.Errorf("entry missing distance_m: %+v", c)
		}
	}
	if !strings.Contains(ev.Summary, "cars closing") {
		t.Errorf("multi-car summary should mention 'cars closing', got %q", ev.Summary)
	}
	if len(ev.Summary) > brain.MaxEventSummaryChars {
		t.Errorf("summary exceeds %d-char cap: %d (%q)", brain.MaxEventSummaryChars, len(ev.Summary), ev.Summary)
	}
	// DebugData must still serialise under the 1KB cap (enforced at enqueue).
	// If we got here, EnqueueEvent already validated — log for visibility.
	if ev.DedupSubject == "" {
		t.Errorf("group event must keep a DedupSubject for cross-rule cooldown")
	}
}

// TestProximityWatcher_PartialGroup_CooldownIsolation — cars in cooldown
// must be EXCLUDED from the bundled event so a stale fire doesn't keep
// re-mentioning a car the driver was just told about, while a newly-armed
// car in the same tick still fires on its own.
func TestProximityWatcher_PartialGroup_CooldownIsolation(t *testing.T) {
	st := &models.RaceState{
		TrackLength:    5891,
		PlayerCarIndex: 19,
		Position:       1,
		CurrentLap:     5,
		Speed:          200,
		SessionType:    1,
		DriverStatus:   4,
		PitStatus:      0,
	}
	st.GridPositions[19] = models.GridPosition{CarIndex: 19, LapDistance: 1000, ResultStatus: 2, DriverStatus: 4, Position: 1}
	st.GridPositions[3] = models.GridPosition{CarIndex: 3, LapDistance: 850, ResultStatus: 2, DriverStatus: 4, Position: 3}
	st.GridPositions[7] = models.GridPosition{CarIndex: 7, LapDistance: 800, ResultStatus: 2, DriverStatus: 4, Position: 4}

	w, bus := newProxWatcherFor(st)
	now := time.Now()
	// Car 3 has just-fired cooldown (10s ago, refire window is 25s).
	seedHistory(w, 3, []proxSample{
		{at: now.Add(-3 * time.Second), signedM: -270},
		{at: now.Add(-2 * time.Second), signedM: -230},
		{at: now.Add(-1 * time.Second), signedM: -190},
	}, false)
	w.cars[3].lastFireAt = now.Add(-10 * time.Second)
	// Car 7 is fresh.
	seedHistory(w, 7, []proxSample{
		{at: now.Add(-3 * time.Second), signedM: -290},
		{at: now.Add(-2 * time.Second), signedM: -260},
		{at: now.Add(-1 * time.Second), signedM: -230},
	}, true)

	w.tick()

	events := bus.PeekEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	count, _ := ev.DebugData["behind_count"].(int)
	if count != 1 {
		t.Errorf("expected behind_count=1 (car 3 in cooldown), got %d", count)
	}
	if got := ev.DebugData["behind_car_index"].(int); got != 7 {
		t.Errorf("expected sole car_index=7, got %d", got)
	}
}

// TestEmitOvertaken_SkipsInPractice — race-only gate.
func TestEmitOvertaken_SkipsInPractice(t *testing.T) {
	for _, sessionType := range []uint8{1, 2, 3, 4, 5, 6, 7, 8, 9, 13} {
		st := &models.RaceState{
			PlayerCarIndex: 19,
			Position:       3,
			CurrentLap:     5,
			SessionType:    sessionType,
			VisualCompound: 17,
		}
		bus := brain.New(func() *models.RaceState { return st }, brain.StaticContext{})
		engine := NewEngine(
			func() *models.RaceState { return st },
			func() int32 { return 10 },
			make(chan models.DrivingInsight, 8),
		)
		engine.SetBrain(bus)

		engine.emitOvertaken(st, 10)
		st.Position = 4
		engine.emitOvertaken(st, 10)

		if got := len(bus.PeekEvents()); got != 0 {
			t.Errorf("session_type=%d: expected no event in non-race session, got %d", sessionType, got)
		}
	}
}

// TestIsRaceSession — boundary conditions.
func TestIsRaceSession(t *testing.T) {
	for _, race := range []uint8{10, 11, 12} {
		if !isRaceSession(race) {
			t.Errorf("expected isRaceSession(%d)=true", race)
		}
	}
	for _, nonRace := range []uint8{0, 1, 5, 9, 13} {
		if isRaceSession(nonRace) {
			t.Errorf("expected isRaceSession(%d)=false", nonRace)
		}
	}
}
