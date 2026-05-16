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
// firing logic without waiting real wall-clock seconds. Each sample's
// totalM is derived from signedM relative to a synthetic player baseline
// — only the *delta* matters for closing-rate / speed math.
func seedHistory(w *ProximityWatcher, carIdx uint8, samples []proxSample) {
	w.cars[carIdx] = &proxCarHistory{samples: samples, lastSeen: samples[len(samples)-1].at}
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

// TestProximityWatcher_FiresOnLowTTI — car closing fast behind, TTI under
// the unified 5s threshold, fires a single EventTrafficUpdate.
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
	st.GridPositions[3] = models.GridPosition{CarIndex: 3, LapDistance: 850, ResultStatus: 2, DriverStatus: 4}

	w, bus := newProxWatcherFor(st)
	now := time.Now()
	// Closing 50m/s from 350m → 150m over 4s. TTI = 150 / 50 = 3.0s < 5s.
	seedHistory(w, 3, []proxSample{
		{at: now.Add(-4 * time.Second), signedM: -350, totalM: 0},
		{at: now.Add(-3 * time.Second), signedM: -300, totalM: 50},
		{at: now.Add(-2 * time.Second), signedM: -250, totalM: 100},
		{at: now.Add(-1 * time.Second), signedM: -200, totalM: 150},
	})

	w.tick()

	events := bus.PeekEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.Type != brain.EventTrafficUpdate {
		t.Errorf("expected EventTrafficUpdate, got %s", ev.Type)
	}
	if ev.Priority != 2 {
		t.Errorf("expected P2, got %d", ev.Priority)
	}
	if ev.DedupSubject != "traffic" {
		t.Errorf("expected DedupSubject=traffic, got %q", ev.DedupSubject)
	}
	if got, _ := ev.DebugData["behind_count"].(int); got != 1 {
		t.Errorf("expected behind_count=1, got %d", got)
	}
	cars, _ := ev.DebugData["behind_cars"].([]map[string]any)
	if len(cars) != 1 || cars[0]["car_index"].(int) != 3 {
		t.Errorf("expected behind_cars=[3], got %+v", cars)
	}
}

// TestProximityWatcher_NoFireWhenTTIHigh — car closing too slowly to hit
// the 5s threshold; nothing fires.
func TestProximityWatcher_NoFireWhenTTIHigh(t *testing.T) {
	st := &models.RaceState{
		TrackLength:    5891,
		PlayerCarIndex: 19,
		Speed:          200,
		SessionType:    1,
		DriverStatus:   4,
	}
	st.GridPositions[19] = models.GridPosition{CarIndex: 19, LapDistance: 1000, ResultStatus: 2, DriverStatus: 4}
	st.GridPositions[3] = models.GridPosition{CarIndex: 3, LapDistance: 200, ResultStatus: 2, DriverStatus: 4}

	w, bus := newProxWatcherFor(st)
	now := time.Now()
	// Distance shrinking ~13m/s, gap ~800m → TTI ~62s.
	seedHistory(w, 3, []proxSample{
		{at: now.Add(-4 * time.Second), signedM: -850, totalM: 0},
		{at: now.Add(-2 * time.Second), signedM: -825, totalM: 25},
		{at: now.Add(-1 * time.Second), signedM: -812, totalM: 38},
	})
	w.tick()

	if got := len(bus.PeekEvents()); got != 0 {
		t.Errorf("expected no event with TTI ≫ 5s, got %d", got)
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
	seedHistory(w, 3, []proxSample{
		{at: now.Add(-3 * time.Second), signedM: -100, totalM: 0},
		{at: now.Add(-2 * time.Second), signedM: -130, totalM: 0},
		{at: now.Add(-1 * time.Second), signedM: -150, totalM: 0},
	})
	w.tick()

	if got := len(bus.PeekEvents()); got != 0 {
		t.Errorf("expected no event when car is opening, got %d", got)
	}
}

// TestProximityWatcher_FiresOnOutLap — slow out-lap, hot-lap car behind
// closing fast.
func TestProximityWatcher_FiresOnOutLap(t *testing.T) {
	st := &models.RaceState{
		TrackLength:    5891,
		PlayerCarIndex: 19,
		Speed:          90,
		SessionType:    5, // Q1
		DriverStatus:   3, // Out Lap
		PitStatus:      0,
	}
	st.GridPositions[19] = models.GridPosition{CarIndex: 19, LapDistance: 1000, ResultStatus: 2, DriverStatus: 3}
	st.GridPositions[7] = models.GridPosition{CarIndex: 7, LapDistance: 850, ResultStatus: 2, DriverStatus: 1}

	w, bus := newProxWatcherFor(st)
	now := time.Now()
	// Closing ~40 m/s, 150m → TTI 3.75s < 5s.
	seedHistory(w, 7, []proxSample{
		{at: now.Add(-3 * time.Second), signedM: -270, totalM: 0},
		{at: now.Add(-2 * time.Second), signedM: -230, totalM: 40},
		{at: now.Add(-1 * time.Second), signedM: -190, totalM: 80},
	})
	w.tick()

	if got := len(bus.PeekEvents()); got != 1 {
		t.Fatalf("expected 1 event on out-lap closure, got %d", got)
	}
}

// TestProximityWatcher_WatcherCooldown — once a traffic_update has fired,
// the watcher must not refire on the very next tick even if the same
// traffic situation is still active.
func TestProximityWatcher_WatcherCooldown(t *testing.T) {
	st := &models.RaceState{
		TrackLength:    5891,
		PlayerCarIndex: 19,
		CurrentLap:     5,
		Speed:          200,
		SessionType:    1,
		DriverStatus:   4,
	}
	st.GridPositions[19] = models.GridPosition{CarIndex: 19, LapDistance: 1000, ResultStatus: 2, DriverStatus: 4}
	st.GridPositions[3] = models.GridPosition{CarIndex: 3, LapDistance: 850, ResultStatus: 2, DriverStatus: 4}

	w, bus := newProxWatcherFor(st)
	now := time.Now()
	seedHistory(w, 3, []proxSample{
		{at: now.Add(-3 * time.Second), signedM: -300, totalM: 0},
		{at: now.Add(-2 * time.Second), signedM: -250, totalM: 50},
		{at: now.Add(-1 * time.Second), signedM: -200, totalM: 100},
	})
	w.tick()
	first := len(bus.PeekEvents())
	w.tick() // same situation — must NOT add another event
	second := len(bus.PeekEvents())

	if first != 1 {
		t.Errorf("first tick: expected 1 fire, got %d", first)
	}
	if second != 1 {
		t.Errorf("second tick (cooldown): expected still 1 event total, got %d", second)
	}
}

// TestProximityWatcher_SnapshotPublished — Snapshot() returns ahead/behind
// lists with closing rates AND per-car speeds.
func TestProximityWatcher_SnapshotPublished(t *testing.T) {
	st := &models.RaceState{
		TrackLength:    5891,
		PlayerCarIndex: 19,
		Speed:          150,
		SessionType:    10,
		DriverStatus:   4,
	}
	st.GridPositions[19] = models.GridPosition{CarIndex: 19, LapDistance: 1000, ResultStatus: 2, DriverStatus: 4}
	st.GridPositions[3] = models.GridPosition{CarIndex: 3, LapDistance: 850, TotalDistance: 120, ResultStatus: 2, DriverStatus: 4}
	st.GridPositions[5] = models.GridPosition{CarIndex: 5, LapDistance: 1300, TotalDistance: 60, ResultStatus: 2, DriverStatus: 4}
	st.GridPositions[7] = models.GridPosition{CarIndex: 7, LapDistance: 4500, ResultStatus: 2, DriverStatus: 4} // out of range

	w, _ := newProxWatcherFor(st)
	now := time.Now()
	// Behind car covering ~60m in 1s plus 60m more on tick append → ~216 km/h.
	seedHistory(w, 3, []proxSample{
		{at: now.Add(-2 * time.Second), signedM: -240, totalM: 0},
		{at: now.Add(-1 * time.Second), signedM: -180, totalM: 60},
	})
	// Ahead car covering ~30m in 1s.
	seedHistory(w, 5, []proxSample{
		{at: now.Add(-2 * time.Second), signedM: 350, totalM: 0},
		{at: now.Add(-1 * time.Second), signedM: 320, totalM: 30},
	})
	w.tick()

	snap := w.Snapshot()
	if snap == nil {
		t.Fatal("expected snapshot")
	}
	if len(snap.NearbyBehind) != 1 || snap.NearbyBehind[0].CarIndex != 3 {
		t.Fatalf("expected behind=[3], got %+v", snap.NearbyBehind)
	}
	if snap.NearbyBehind[0].SpeedKmh < 180 || snap.NearbyBehind[0].SpeedKmh > 240 {
		t.Errorf("expected behind speed_kmh ~216, got %d", snap.NearbyBehind[0].SpeedKmh)
	}
	if len(snap.NearbyAhead) != 1 || snap.NearbyAhead[0].CarIndex != 5 {
		t.Fatalf("expected ahead=[5], got %+v", snap.NearbyAhead)
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
				{at: now.Add(-3 * time.Second), signedM: -300, totalM: 0},
				{at: now.Add(-2 * time.Second), signedM: -250, totalM: 50},
				{at: now.Add(-1 * time.Second), signedM: -200, totalM: 100},
			})
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

// TestProximityWatcher_BothDirectionsBundled — when cars are closing
// from both ahead AND behind in the same tick, ONE traffic_update event
// fires carrying both populations.
func TestProximityWatcher_BothDirectionsBundled(t *testing.T) {
	st := &models.RaceState{
		TrackLength:    5891,
		PlayerCarIndex: 19,
		Position:       4,
		CurrentLap:     5,
		Speed:          200,
		SessionType:    1,
		DriverStatus:   4,
		PitStatus:      0,
	}
	st.GridPositions[19] = models.GridPosition{CarIndex: 19, LapDistance: 1000, ResultStatus: 2, DriverStatus: 4, Position: 4}
	// Two cars behind closing.
	st.GridPositions[3] = models.GridPosition{CarIndex: 3, LapDistance: 850, ResultStatus: 2, DriverStatus: 4, Position: 6}
	st.GridPositions[7] = models.GridPosition{CarIndex: 7, LapDistance: 800, ResultStatus: 2, DriverStatus: 4, Position: 7}
	// One car ahead being caught.
	st.GridPositions[11] = models.GridPosition{CarIndex: 11, LapDistance: 1150, ResultStatus: 2, DriverStatus: 4, Position: 3}

	w, bus := newProxWatcherFor(st)
	now := time.Now()
	// Car 3: 150m behind closing 50m/s → TTI 3.0s.
	seedHistory(w, 3, []proxSample{
		{at: now.Add(-3 * time.Second), signedM: -300, totalM: 0},
		{at: now.Add(-2 * time.Second), signedM: -250, totalM: 50},
		{at: now.Add(-1 * time.Second), signedM: -200, totalM: 100},
	})
	// Car 7: 200m behind closing 40m/s → TTI 5.0s exactly (still inside).
	seedHistory(w, 7, []proxSample{
		{at: now.Add(-3 * time.Second), signedM: -320, totalM: 0},
		{at: now.Add(-2 * time.Second), signedM: -280, totalM: 40},
		{at: now.Add(-1 * time.Second), signedM: -240, totalM: 80},
	})
	// Car 11: 150m ahead, we are catching at 40m/s → TTI 3.75s.
	seedHistory(w, 11, []proxSample{
		{at: now.Add(-3 * time.Second), signedM: 270, totalM: 0},
		{at: now.Add(-2 * time.Second), signedM: 230, totalM: 40},
		{at: now.Add(-1 * time.Second), signedM: 190, totalM: 80},
	})

	w.tick()

	events := bus.PeekEvents()
	if len(events) != 1 {
		t.Fatalf("expected ONE bundled traffic_update, got %d", len(events))
	}
	ev := events[0]
	if ev.Type != brain.EventTrafficUpdate {
		t.Errorf("expected EventTrafficUpdate, got %s", ev.Type)
	}
	bc, _ := ev.DebugData["behind_count"].(int)
	ac, _ := ev.DebugData["ahead_count"].(int)
	if bc < 1 {
		t.Errorf("expected behind_count >= 1 (got %d) — at least car 3 inside 5s TTI", bc)
	}
	if ac != 1 {
		t.Errorf("expected ahead_count=1, got %d", ac)
	}
	behindCars, _ := ev.DebugData["behind_cars"].([]map[string]any)
	aheadCars, _ := ev.DebugData["ahead_cars"].([]map[string]any)
	if len(behindCars) < 2 {
		t.Errorf("expected behind_cars to enumerate top-N (>=2), got %d", len(behindCars))
	}
	if len(aheadCars) != 1 {
		t.Errorf("expected ahead_cars=1, got %d", len(aheadCars))
	}
	// signed_m must be present + correctly signed.
	for _, c := range behindCars {
		sm, ok := c["signed_m"].(float32)
		if !ok || sm >= 0 {
			t.Errorf("behind car signed_m should be negative float32: %+v", c)
		}
	}
	for _, c := range aheadCars {
		sm, ok := c["signed_m"].(float32)
		if !ok || sm <= 0 {
			t.Errorf("ahead car signed_m should be positive float32: %+v", c)
		}
	}
	if len(ev.Summary) > brain.MaxEventSummaryChars {
		t.Errorf("summary exceeds %d-char cap: %d (%q)", brain.MaxEventSummaryChars, len(ev.Summary), ev.Summary)
	}
	if ev.DedupSubject != "traffic" {
		t.Errorf("expected DedupSubject=traffic, got %q", ev.DedupSubject)
	}
	if !strings.Contains(ev.DedupKey, "traffic_update:") {
		t.Errorf("DedupKey should be traffic_update-prefixed, got %q", ev.DedupKey)
	}
}

// TestProximityWatcher_SkipsP_Plus1_Behind — closing_threat.go owns the
// P+1 slot. Even if that car is closing inside the TTI window, the
// traffic_update rule must skip it. (Other cars still trigger if any.)
func TestProximityWatcher_SkipsP_Plus1_Behind(t *testing.T) {
	st := &models.RaceState{
		TrackLength:    5891,
		PlayerCarIndex: 19,
		Position:       3,
		CurrentLap:     5,
		Speed:          200,
		SessionType:    1,
		DriverStatus:   4,
	}
	st.GridPositions[19] = models.GridPosition{CarIndex: 19, LapDistance: 1000, ResultStatus: 2, DriverStatus: 4, Position: 3}
	// Only car behind is at P+1 (Position 4).
	st.GridPositions[3] = models.GridPosition{CarIndex: 3, LapDistance: 850, ResultStatus: 2, DriverStatus: 4, Position: 4}

	w, bus := newProxWatcherFor(st)
	now := time.Now()
	seedHistory(w, 3, []proxSample{
		{at: now.Add(-3 * time.Second), signedM: -300, totalM: 0},
		{at: now.Add(-2 * time.Second), signedM: -250, totalM: 50},
		{at: now.Add(-1 * time.Second), signedM: -200, totalM: 100},
	})
	w.tick()

	if got := len(bus.PeekEvents()); got != 0 {
		t.Errorf("expected no traffic_update when only car is P+1 (owned by closing_threat), got %d", got)
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
