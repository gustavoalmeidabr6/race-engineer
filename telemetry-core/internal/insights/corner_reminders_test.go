package insights

import (
	"strings"
	"testing"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/brain"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
)

func mustAdd(t *testing.T, s *ReminderStore, r Reminder) *Reminder {
	t.Helper()
	out, err := s.Add(r)
	if err != nil {
		t.Fatalf("Add(%v): %v", r, err)
	}
	return out
}

// Standard fire path: player approaches the trigger, fire window straddles
// it, dedupes within a single lap.
func TestReminderStore_FiresInsideWindow_DedupSameLap(t *testing.T) {
	s := NewReminderStore()
	mustAdd(t, s, Reminder{
		CornerID:       "T9",
		TriggerDistM:   2480,
		LookaheadM:     90,
		EffectiveDistM: 2390,
		Message:        "brake later, deeper apex",
		Priority:       3,
		Recurring:      true,
		ExpiresAtLap:   30,
		LastFiredLap:   -1,
	})

	// Before the window — no fire.
	if got := s.poll(5, 2300); len(got) != 0 {
		t.Errorf("expected no fire before trigger, got %d", len(got))
	}
	// Inside the window — fires.
	if got := s.poll(5, 2400); len(got) != 1 {
		t.Errorf("expected 1 fire inside window, got %d", len(got))
	}
	// Same lap, still inside window — deduped.
	if got := s.poll(5, 2450); len(got) != 0 {
		t.Errorf("expected dedup on same lap, got %d", len(got))
	}
	// New lap — fires again (recurring).
	if got := s.poll(6, 2400); len(got) != 1 {
		t.Errorf("expected re-fire on new lap, got %d", len(got))
	}
}

// Past the window — no fire (we don't want to coach AFTER the corner).
func TestReminderStore_PastWindow_NoFire(t *testing.T) {
	s := NewReminderStore()
	mustAdd(t, s, Reminder{
		TriggerDistM:   2480,
		LookaheadM:     90,
		EffectiveDistM: 2390,
		Message:        "x",
		Priority:       3,
		Recurring:      true,
		LastFiredLap:   -1,
	})
	// 200m past effective trigger → outside FireWindowM (100m).
	if got := s.poll(5, 2600); len(got) != 0 {
		t.Errorf("expected no fire 200m past trigger, got %d", len(got))
	}
}

// Non-recurring reminders are removed after firing once.
func TestReminderStore_NonRecurring_DropsAfterFire(t *testing.T) {
	s := NewReminderStore()
	mustAdd(t, s, Reminder{
		TriggerDistM:   2480,
		LookaheadM:     90,
		EffectiveDistM: 2390,
		Message:        "one shot",
		Priority:       3,
		Recurring:      false,
		LastFiredLap:   -1,
	})
	if got := s.poll(5, 2400); len(got) != 1 {
		t.Fatalf("expected 1 fire, got %d", len(got))
	}
	if got := s.List(); len(got) != 0 {
		t.Errorf("expected store empty after non-recurring fire, got %d", len(got))
	}
}

// Expired reminders are removed without firing.
func TestReminderStore_Expiry(t *testing.T) {
	s := NewReminderStore()
	mustAdd(t, s, Reminder{
		TriggerDistM:   2480,
		LookaheadM:     90,
		EffectiveDistM: 2390,
		Message:        "x",
		Priority:       3,
		Recurring:      true,
		ExpiresAtLap:   3,
		LastFiredLap:   -1,
	})
	// Lap 4 is past expiry.
	if got := s.poll(4, 2400); len(got) != 0 {
		t.Errorf("expected no fire after expiry, got %d", len(got))
	}
	if got := s.List(); len(got) != 0 {
		t.Errorf("expected expired reminder dropped, got %d", len(got))
	}
}

// Lap-based reminders fire once when the player enters the target lap and
// are then dropped from the store regardless of the Recurring flag.
func TestReminderStore_LapBased_OneShot(t *testing.T) {
	s := NewReminderStore()
	mustAdd(t, s, Reminder{
		TriggerAtLap: 15,
		Message:      "pit this lap",
		Priority:     4,
		Recurring:    true, // intentionally true — should be ignored for at_lap
		ExpiresAtLap: 16,
		LastFiredLap: -1,
	})
	// Before the target lap — no fire regardless of lap distance.
	if got := s.poll(14, 2500); len(got) != 0 {
		t.Errorf("expected no fire before target lap, got %d", len(got))
	}
	// Entering the target lap — fires once.
	got := s.poll(15, 50)
	if len(got) != 1 {
		t.Fatalf("expected 1 fire on entering target lap, got %d", len(got))
	}
	if got[0].TriggerAtLap != 15 || got[0].Message != "pit this lap" {
		t.Errorf("fired reminder mismatch: %+v", got[0])
	}
	// One-shot: store should be empty even though Recurring=true was set.
	if remaining := s.List(); len(remaining) != 0 {
		t.Errorf("expected lap-based reminder dropped after firing, got %d", len(remaining))
	}
	// Re-polling does not re-fire.
	if got := s.poll(15, 200); len(got) != 0 {
		t.Errorf("expected no re-fire after one-shot drop, got %d", len(got))
	}
}

// Past the target lap, a lap-based reminder still fires once if the watcher
// somehow missed the exact entry tick (e.g., session paused). Expiry still
// removes it before that fallback kicks in.
func TestReminderStore_LapBased_FiresIfMissedExactLap(t *testing.T) {
	s := NewReminderStore()
	mustAdd(t, s, Reminder{
		TriggerAtLap: 5,
		Message:      "fuel mix 2",
		Recurring:    false,
		ExpiresAtLap: 8,
		LastFiredLap: -1,
	})
	if got := s.poll(7, 1000); len(got) != 1 {
		t.Errorf("expected lap-based reminder to fire after target lap if not expired, got %d", len(got))
	}
}

// Combined trigger (at_lap + position): fires once when the player crosses
// the target distance ON the named lap, and only on that lap.
func TestReminderStore_Combined_LapPlusPosition(t *testing.T) {
	s := NewReminderStore()
	mustAdd(t, s, Reminder{
		CornerID:       "T8",
		TriggerDistM:   2480,
		LookaheadM:     90,
		EffectiveDistM: 2390,
		TriggerAtLap:   5,
		Message:        "watch turn-in",
		Priority:       3,
		Recurring:      true, // intentionally true — should be ignored
		ExpiresAtLap:   6,
		LastFiredLap:   -1,
	})
	// Wrong lap — position inside window but lap doesn't match yet.
	if got := s.poll(4, 2400); len(got) != 0 {
		t.Errorf("expected no fire on lap 4 (target 5), got %d", len(got))
	}
	// Target lap, but before the position — still wait.
	if got := s.poll(5, 2300); len(got) != 0 {
		t.Errorf("expected no fire before trigger on target lap, got %d", len(got))
	}
	// Target lap + inside window — fires.
	got := s.poll(5, 2400)
	if len(got) != 1 {
		t.Fatalf("expected 1 fire on combined trigger, got %d", len(got))
	}
	if got[0].CornerID != "T8" || got[0].TriggerAtLap != 5 {
		t.Errorf("fired reminder mismatch: %+v", got[0])
	}
	// One-shot: store should be empty.
	if remaining := s.List(); len(remaining) != 0 {
		t.Errorf("expected combined reminder dropped after firing, got %d", len(remaining))
	}
}

// Combined trigger that misses the target lap by a tick still fires within
// the position window on the next lap (until expiry catches up).
func TestReminderStore_Combined_FiresOnLaterLapWithinExpiry(t *testing.T) {
	s := NewReminderStore()
	mustAdd(t, s, Reminder{
		CornerID:       "T8",
		TriggerDistM:   2480,
		LookaheadM:     90,
		EffectiveDistM: 2390,
		TriggerAtLap:   5,
		Message:        "watch turn-in",
		Recurring:      false,
		ExpiresAtLap:   6,
		LastFiredLap:   -1,
	})
	// Skip past lap 5 entirely — the watcher tick was missed. Lap 6 is
	// still within expiry, so the cue fires once when the player crosses
	// the window there.
	got := s.poll(6, 2400)
	if len(got) != 1 {
		t.Fatalf("expected 1 fire on lap 6 (target 5, expiry 6), got %d", len(got))
	}
}

// Add dedupes on (CornerID, Message).
func TestReminderStore_Add_DedupSameCornerAndMessage(t *testing.T) {
	s := NewReminderStore()
	first := mustAdd(t, s, Reminder{CornerID: "T12", Message: "brake earlier"})
	second := mustAdd(t, s, Reminder{CornerID: "T12", Message: "brake earlier"})
	if first.ID != second.ID {
		t.Errorf("expected dedup, got distinct ids %s vs %s", first.ID, second.ID)
	}
	if got := s.List(); len(got) != 1 {
		t.Errorf("expected 1 stored, got %d", len(got))
	}
}

// Capacity overflow.
func TestReminderStore_Capacity(t *testing.T) {
	s := NewReminderStore()
	for i := 0; i < MaxReminders; i++ {
		_, err := s.Add(Reminder{Message: "m", CornerID: "" /* no dedup */})
		if err != nil {
			t.Fatalf("unexpected err on add %d: %v", i, err)
		}
	}
	_, err := s.Add(Reminder{Message: "overflow"})
	if err != ErrReminderStoreFull {
		t.Errorf("expected ErrReminderStoreFull, got %v", err)
	}
}

// Cancel + List basics.
func TestReminderStore_Cancel(t *testing.T) {
	s := NewReminderStore()
	r := mustAdd(t, s, Reminder{CornerID: "T1", Message: "x"})
	if !s.Cancel(r.ID) {
		t.Error("expected Cancel to return true")
	}
	if s.Cancel(r.ID) {
		t.Error("expected second Cancel to return false")
	}
	if len(s.List()) != 0 {
		t.Error("expected empty store after cancel")
	}
}

// ComputeLookaheadM converts time→distance and clamps.
func TestComputeLookaheadM(t *testing.T) {
	cases := []struct {
		seconds  float32
		speedKmh uint16
		min, max float32
	}{
		// Standard case: 1.5s @ 300 km/h ≈ 125m
		{1.5, 300, 100, 150},
		// Slow speed → falls back to ~80 m/s base
		{1.5, 50, 100, 150},
		// Tiny lookahead → floors at MinLookaheadM
		{0.1, 200, MinLookaheadM, MinLookaheadM + 0.1},
		// Huge lookahead → ceils at MaxLookaheadM
		{10, 320, MaxLookaheadM, MaxLookaheadM + 0.1},
	}
	for _, c := range cases {
		got := ComputeLookaheadM(c.seconds, c.speedKmh)
		if got < c.min || got > c.max {
			t.Errorf("ComputeLookaheadM(%v, %v) = %v, want in [%v,%v]",
				c.seconds, c.speedKmh, got, c.min, c.max)
		}
	}
}

// CornerWatcher tick — full integration: store + brain + state.
// Verifies a reminder fires through the watcher.tick() path.
func TestCornerWatcher_Tick_FiresThroughBus(t *testing.T) {
	store := NewReminderStore()
	mustAdd(t, store, Reminder{
		CornerID:       "T9",
		CornerName:     "Copse",
		TriggerDistM:   2480,
		LookaheadM:     90,
		EffectiveDistM: 2390,
		Message:        "brake later",
		Priority:       3,
		Recurring:      true,
		LastFiredLap:   -1,
	})

	state := &models.RaceState{
		CurrentLap:   5,
		LapDistance:  2400, // inside window
		Speed:        290,
		TrackLength:  5891,
		DriverStatus: 4, // on track
		PitStatus:    0,
	}
	bus := brain.New(func() *models.RaceState { return state }, brain.StaticContext{})

	w := NewCornerWatcher(
		store,
		func() *models.RaceState { return state },
		bus,
		func() int32 { return 5 },
		0,
	)
	w.tick()

	events := bus.PeekEvents()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != brain.EventCornerCoaching {
		t.Errorf("expected EventCornerCoaching, got %s", events[0].Type)
	}
	if events[0].Priority != 3 {
		t.Errorf("expected priority 3, got %d", events[0].Priority)
	}
	if !strings.Contains(events[0].Reasoning, "T9 Copse") {
		t.Errorf("expected corner label in reasoning, got %q", events[0].Reasoning)
	}
}

// Watcher skips when player is in pits / out lap / garage.
func TestCornerWatcher_SkipsInPitsAndOutLap(t *testing.T) {
	store := NewReminderStore()
	mustAdd(t, store, Reminder{
		TriggerDistM:   2480,
		LookaheadM:     90,
		EffectiveDistM: 2390,
		Message:        "x",
		Priority:       3,
		Recurring:      true,
		LastFiredLap:   -1,
	})

	cases := []struct {
		name  string
		state *models.RaceState
	}{
		{"in pit lane", &models.RaceState{CurrentLap: 5, LapDistance: 2400, TrackLength: 5891, PitStatus: 1, DriverStatus: 4}},
		{"in pit area", &models.RaceState{CurrentLap: 5, LapDistance: 2400, TrackLength: 5891, PitStatus: 2, DriverStatus: 4}},
		{"in garage",   &models.RaceState{CurrentLap: 5, LapDistance: 2400, TrackLength: 5891, PitStatus: 0, DriverStatus: 0}},
		{"in lap",      &models.RaceState{CurrentLap: 5, LapDistance: 2400, TrackLength: 5891, PitStatus: 0, DriverStatus: 2}},
		{"out lap",     &models.RaceState{CurrentLap: 5, LapDistance: 2400, TrackLength: 5891, PitStatus: 0, DriverStatus: 3}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bus := brain.New(func() *models.RaceState { return c.state }, brain.StaticContext{})
			w := NewCornerWatcher(store, func() *models.RaceState { return c.state }, bus, nil, 0)
			w.tick()
			if got := len(bus.PeekEvents()); got != 0 {
				t.Errorf("expected no events while %s, got %d", c.name, got)
			}
		})
	}
}
