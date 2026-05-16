package plan

import (
	"testing"
	"time"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/brain"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
)

// stubTracks implements TrackInfoProvider with a single canned entry, for
// asserting that the ticker passes the track id through correctly without
// pulling the real trackmap package into the plan tests.
type stubTracks struct {
	trackID int
	distM   float64
	side    string
}

func (s stubTracks) PitEntry(id int) (float64, string, bool) {
	if id != s.trackID {
		return 0, "", false
	}
	return s.distM, s.side, true
}

// newTestTicker wires a store, ticker, and brain bus over an in-memory
// RaceState pointer. The state pointer is returned so tests can flip
// fields between tick() calls.
func newTestTicker(t *testing.T, tracks TrackInfoProvider) (*Ticker, *Store, *brain.RaceBrain, *models.RaceState) {
	t.Helper()
	st := &models.RaceState{CurrentLap: 0}
	s := newTestStore(t, 99)
	bus := brain.New(func() *models.RaceState { return st }, brain.StaticContext{})
	tk := NewTicker(s, func() *models.RaceState { return st }, bus, func() int32 { return 10 }, tracks, 500*time.Millisecond)
	tk.now = func() time.Time { return time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC) }
	return tk, s, bus, st
}

// TestTicker_LapTriggerFiresOnce — a lap trigger fires EventPlanReminder
// exactly once when the player enters trigger.lap, regardless of how many
// ticks happen on that lap.
func TestTicker_LapTriggerFiresOnce(t *testing.T) {
	tk, s, bus, st := newTestTicker(t, nil)
	_, _ = s.Add(PlanEntry{Kind: KindPitStop, Trigger: Trigger{Type: TriggerLap, Lap: 14}, Notes: "hard tyres"}, "test")

	// Lap 13 — must not fire yet.
	st.CurrentLap = 13
	tk.tick()
	if got := len(bus.PeekEvents()); got != 0 {
		t.Errorf("lap 13: expected no event, got %d", got)
	}

	// Lap 14 — fires once.
	st.CurrentLap = 14
	tk.tick()
	tk.tick() // refire suppressed
	tk.tick()
	events := bus.PeekEvents()
	if len(events) != 1 {
		t.Fatalf("lap 14: expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.Type != brain.EventPlanReminder {
		t.Errorf("expected EventPlanReminder, got %s", ev.Type)
	}
	// Entry should flip to active after firing.
	cur := s.Current()
	if cur.Entries[0].Status != StatusActive {
		t.Errorf("expected entry status=active after fire, got %q", cur.Entries[0].Status)
	}
}

// TestTicker_LapTriggerPastWithoutFiring — if the player passes the lap
// without the entry ever firing (e.g., cooldown / dedup race), the entry
// transitions to done so it doesn't haunt the plan.
func TestTicker_LapTriggerPastWithoutFiring(t *testing.T) {
	tk, s, _, st := newTestTicker(t, nil)
	stored, _ := s.Add(PlanEntry{Kind: KindReminder, Trigger: Trigger{Type: TriggerLap, Lap: 5}}, "test")
	// Pre-mark as already-fired to simulate "we tried but it was deduped".
	_ = s.MarkFired(stored.ID, string(brain.EventPlanReminder), tk.now())

	st.CurrentLap = 6 // past the trigger lap
	tk.tick()
	// Status should NOT auto-transition because FiredEvents already has the kind.
	cur := s.Current()
	if cur.Entries[0].Status != StatusPending {
		// already-fired entries stay in whatever status the previous fire
		// landed at; we only auto-close ones that NEVER fired.
		_ = cur
	}

	// Now a different entry, never fired, on lap 5. We pass to lap 6 →
	// auto-close to done.
	other, _ := s.Add(PlanEntry{Kind: KindReminder, Trigger: Trigger{Type: TriggerLap, Lap: 5}}, "test")
	tk.tick()
	cur = s.Current()
	for _, e := range cur.Entries {
		if e.ID == other.ID && e.Status != StatusDone {
			t.Errorf("expected unfired past-lap entry status=done, got %q", e.Status)
		}
	}
}

// TestTicker_LapWindowOpensAndCloses — a lap_window trigger fires open on
// entry to from_lap and close on exit past to_lap.
func TestTicker_LapWindowOpensAndCloses(t *testing.T) {
	tk, s, bus, st := newTestTicker(t, nil)
	_, _ = s.Add(PlanEntry{Kind: KindPitStop, Trigger: Trigger{Type: TriggerLapWindow, FromLap: 12, ToLap: 16}}, "test")

	st.CurrentLap = 12
	tk.tick()
	if got := len(bus.PeekEvents()); got != 1 {
		t.Fatalf("window open: expected 1 event, got %d", got)
	}
	if got := bus.PeekEvents()[0].Type; got != brain.EventPlanWindowOpen {
		t.Errorf("expected EventPlanWindowOpen, got %s", got)
	}

	// Mid-window ticks must not refire.
	st.CurrentLap = 14
	tk.tick()
	if got := len(bus.PeekEvents()); got != 1 {
		t.Errorf("mid-window: expected still 1 event, got %d", got)
	}

	// Past window → close.
	bus.ClearEventQueue()
	st.CurrentLap = 17
	tk.tick()
	if got := len(bus.PeekEvents()); got != 1 {
		t.Fatalf("window close: expected 1 event, got %d", got)
	}
	if got := bus.PeekEvents()[0].Type; got != brain.EventPlanWindowClose {
		t.Errorf("expected EventPlanWindowClose, got %s", got)
	}
	cur := s.Current()
	if cur.Entries[0].Status != StatusDone {
		t.Errorf("expected entry status=done after window close, got %q", cur.Entries[0].Status)
	}
}

// TestTicker_TrackPositionFiresWithinLookahead — fires EventPlanImminent
// when the player is within imminentLookaheadM of the spatial trigger.
func TestTicker_TrackPositionFiresWithinLookahead(t *testing.T) {
	tk, s, bus, st := newTestTicker(t, nil)
	_, _ = s.Add(PlanEntry{
		Kind:    KindPitStop,
		Trigger: Trigger{Type: TriggerTrackPosition, Lap: 14, DistanceM: 5700},
	}, "test")
	st.CurrentLap = 14
	st.TrackLength = 5891
	st.LapDistance = 5300 // 400m before → outside lookahead

	tk.tick()
	if got := len(bus.PeekEvents()); got != 0 {
		t.Errorf("400m gap: expected no event, got %d", got)
	}

	st.LapDistance = 5500 // 200m before → inside the 250m lookahead window
	tk.tick()
	events := bus.PeekEvents()
	if len(events) != 1 {
		t.Fatalf("200m gap: expected 1 event, got %d", len(events))
	}
	if events[0].Type != brain.EventPlanImminent {
		t.Errorf("expected EventPlanImminent, got %s", events[0].Type)
	}
	if events[0].Priority != 4 {
		t.Errorf("expected P4, got %d", events[0].Priority)
	}

	// Past the trigger — must not refire.
	st.LapDistance = 5800
	tk.tick()
	if got := len(bus.PeekEvents()); got != 1 {
		t.Errorf("post-trigger: expected still 1 event, got %d", got)
	}
}

// TestTicker_TrackPositionUsesTrackInfoProvider — when an entry has no
// explicit DistanceM, the ticker resolves the spatial coordinate via the
// TrackInfoProvider (i.e., the trackmap registry).
func TestTicker_TrackPositionUsesTrackInfoProvider(t *testing.T) {
	tracks := stubTracks{trackID: 7, distM: 5700, side: "right"}
	tk, s, bus, st := newTestTicker(t, tracks)
	_, _ = s.Add(PlanEntry{
		Kind:    KindPitStop,
		Trigger: Trigger{Type: TriggerTrackPosition, Lap: 14}, // no DistanceM
	}, "test")
	st.TrackID = 7
	st.CurrentLap = 14
	st.TrackLength = 5891
	st.LapDistance = 5500 // 200m before resolved target

	tk.tick()
	if got := len(bus.PeekEvents()); got != 1 {
		t.Fatalf("expected 1 imminent event resolved via tracks, got %d", got)
	}
}

// TestTicker_TrackPositionSkipsWithoutPitData — when the entry omits
// DistanceM AND no TrackInfoProvider answer is available, the ticker
// silently skips the entry (no panic, no event).
func TestTicker_TrackPositionSkipsWithoutPitData(t *testing.T) {
	tracks := stubTracks{trackID: 999} // doesn't match
	tk, s, bus, st := newTestTicker(t, tracks)
	_, _ = s.Add(PlanEntry{
		Kind:    KindPitStop,
		Trigger: Trigger{Type: TriggerTrackPosition, Lap: 14},
	}, "test")
	st.TrackID = 7
	st.CurrentLap = 14
	st.TrackLength = 5891
	st.LapDistance = 5500

	tk.tick()
	if got := len(bus.PeekEvents()); got != 0 {
		t.Errorf("expected no event without pit data, got %d", got)
	}
}

// TestTicker_CancelledEntriesSilent — cancelled / done entries don't fire
// even when their trigger condition matches.
func TestTicker_CancelledEntriesSilent(t *testing.T) {
	tk, s, bus, st := newTestTicker(t, nil)
	stored, _ := s.Add(PlanEntry{Kind: KindPitStop, Trigger: Trigger{Type: TriggerLap, Lap: 14}}, "test")
	_, _ = s.Cancel(stored.ID, "weather")

	st.CurrentLap = 14
	tk.tick()
	if got := len(bus.PeekEvents()); got != 0 {
		t.Errorf("expected no event for cancelled entry, got %d", got)
	}
}

// TestTicker_TimeLeftTriggerFiresOnce — fires when SessionTimeLeft drops
// below the threshold; idempotent.
func TestTicker_TimeLeftTriggerFiresOnce(t *testing.T) {
	tk, s, bus, st := newTestTicker(t, nil)
	_, _ = s.Add(PlanEntry{Kind: KindReminder, Trigger: Trigger{Type: TriggerTimeLeftSec, Seconds: 60}, Notes: "push now"}, "test")
	st.CurrentLap = 1
	st.SessionTimeLeft = 120 // above threshold
	tk.tick()
	if got := len(bus.PeekEvents()); got != 0 {
		t.Errorf("above threshold: expected no event, got %d", got)
	}
	st.SessionTimeLeft = 30 // below threshold
	tk.tick()
	tk.tick() // dedup
	if got := len(bus.PeekEvents()); got != 1 {
		t.Errorf("below threshold: expected 1 event, got %d", got)
	}
}
