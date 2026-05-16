package brain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// staticClock returns a clock function whose return value is whatever *cur
// points to right now. Tests advance time by writing to *cur. Unlike
// fixedNow (which auto-ticks 100ms per call), this lets tests jump exactly
// to the moment they care about — necessary for TTL/lease-timeout asserts.
func staticClock(cur *time.Time) func() time.Time {
	return func() time.Time { return *cur }
}

func newLeaseTestBrain() (*RaceBrain, *time.Time) {
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	b := New(nil, StaticContext{})
	b.now = staticClock(&now)
	return b, &now
}

// TestLeaseNext_MarksInFlightAndIsSingleFlight: a successful lease flips
// the event to in_flight; a second lease (any band) returns nil because
// the bus is single-flight by design.
func TestLeaseNext_MarksInFlightAndIsSingleFlight(t *testing.T) {
	b, _ := newLeaseTestBrain()
	if _, err := b.EnqueueEvent(validEvent(EventBoxNow), 10); err != nil {
		t.Fatalf("enqueue box_now: %v", err)
	}
	if _, err := b.EnqueueEvent(validEvent(EventLapSummary), 10); err != nil {
		t.Fatalf("enqueue lap_summary: %v", err)
	}

	leased := b.LeaseNext(4, 5)
	if leased == nil {
		t.Fatal("expected a leased event, got nil")
	}
	if leased.Type != EventBoxNow {
		t.Fatalf("expected box_now first, got %s", leased.Type)
	}
	if leased.Status != StatusInFlight {
		t.Fatalf("expected status in_flight on returned copy, got %s", leased.Status)
	}
	if leased.Attempts != 1 {
		t.Fatalf("expected attempts=1, got %d", leased.Attempts)
	}

	// Second lease blocked by single-flight, even for a different priority band.
	if x := b.LeaseNext(1, 3); x != nil {
		t.Errorf("expected nil while box_now in_flight, got %s", x.Type)
	}
}

// TestAckDelivered_RemovesAndRecordsSpeech: a delivered ack on a non-ack
// event pops it from the queue AND writes a SpeechRecord for
// dedup-against-recent-radio.
func TestAckDelivered_RemovesAndRecordsSpeech(t *testing.T) {
	b, _ := newLeaseTestBrain()
	// LapSummary doesn't require ack (DefaultRequiresAck → false), so
	// AckDelivered removes outright. The awaiting-ack path is covered
	// by TestAckDelivered_RequiresAckHoldsAwaiting below.
	stored, err := b.EnqueueEvent(validEvent(EventLapSummary), 10)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	leased := b.LeaseNext(1, 3)
	if leased == nil || leased.ID != stored.ID {
		t.Fatalf("lease id mismatch")
	}

	if err := b.AckDelivered(stored.ID, "Box, box.", 5); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if got := len(b.PeekEvents()); got != 0 {
		t.Errorf("expected queue empty after ack, got %d", got)
	}

	snap := b.Snapshot(DefaultSnapshotOpts())
	if len(snap.Speech) != 1 {
		t.Fatalf("expected 1 speech record, got %d", len(snap.Speech))
	}
	if !strings.Contains(snap.Speech[0].Text, "Box, box") {
		t.Errorf("speech text mismatch: %q", snap.Speech[0].Text)
	}
}

// TestAckDelivered_FallsBackToSummary: when spoken_text is empty the ack
// records the event's Summary instead so dedup still has something to match.
func TestAckDelivered_FallsBackToSummary(t *testing.T) {
	b, _ := newLeaseTestBrain()
	// LapSummary doesn't auto-require ack so the queue empties on Ack.
	e := validEvent(EventLapSummary)
	e.Summary = "box this lap"
	stored, _ := b.EnqueueEvent(e, 10)
	b.LeaseNext(1, 3)
	if err := b.AckDelivered(stored.ID, "", 0); err != nil {
		t.Fatalf("ack: %v", err)
	}
	snap := b.Snapshot(DefaultSnapshotOpts())
	if len(snap.Speech) != 1 || snap.Speech[0].Text != "box this lap" {
		t.Errorf("expected summary fallback, got %v", snap.Speech)
	}
}

// TestAckDelivered_UnknownIDErrors: acking a stale id returns ErrEventNotFound
// so callers can distinguish "already removed" from "successfully closed".
func TestAckDelivered_UnknownIDErrors(t *testing.T) {
	b, _ := newLeaseTestBrain()
	if err := b.AckDelivered("evt_does_not_exist", "x", 3); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("expected ErrEventNotFound, got %v", err)
	}
}

// TestNackInterrupted_RequeuesAtSamePriority: nack flips status back to
// queued, leaves it in the queue, and a subsequent LeaseNext returns the
// SAME event with Attempts incremented.
func TestNackInterrupted_RequeuesAtSamePriority(t *testing.T) {
	b, _ := newLeaseTestBrain()
	stored, _ := b.EnqueueEvent(validEvent(EventBoxNow), 10)

	leased := b.LeaseNext(4, 5)
	if leased == nil || leased.Attempts != 1 {
		t.Fatalf("first lease state wrong: %+v", leased)
	}

	if err := b.NackInterrupted(stored.ID, "user_barge_in"); err != nil {
		t.Fatalf("nack: %v", err)
	}

	// Should still be on the queue, but back to queued status.
	q := b.PeekEvents()
	if len(q) != 1 {
		t.Fatalf("expected 1 event still queued, got %d", len(q))
	}
	if q[0].Status != StatusQueued {
		t.Errorf("expected status queued after nack, got %s", q[0].Status)
	}

	// Second lease returns the same event, with Attempts=2.
	again := b.LeaseNext(4, 5)
	if again == nil || again.ID != stored.ID {
		t.Fatalf("re-lease should return same event, got %+v", again)
	}
	if again.Attempts != 2 {
		t.Errorf("expected attempts=2, got %d", again.Attempts)
	}
}

// TestNackInterrupted_DropsAfterMaxAttempts: after MaxAttemptsForPriority
// nacks, the event is dropped and an Observation is recorded under
// "delivery" topic so the analyst can see what happened.
func TestNackInterrupted_DropsAfterMaxAttempts(t *testing.T) {
	b, _ := newLeaseTestBrain()
	// Use a non-urgent event (cap=3).
	stored, _ := b.EnqueueEvent(validEvent(EventLapSummary), 10)

	for i := 0; i < MaxAttemptsNonUrgent; i++ {
		leased := b.LeaseNext(1, 3)
		if leased == nil {
			t.Fatalf("attempt %d: lease returned nil", i+1)
		}
		if err := b.NackInterrupted(stored.ID, "user_barge_in"); err != nil {
			t.Fatalf("attempt %d: nack: %v", i+1, err)
		}
	}

	if got := len(b.PeekEvents()); got != 0 {
		t.Errorf("expected queue empty after %d nacks, got %d", MaxAttemptsNonUrgent, got)
	}

	// Observation should be in the brain under topic "delivery".
	snap := b.Snapshot(DefaultSnapshotOpts())
	found := false
	for _, obs := range snap.Observations["delivery"] {
		if strings.Contains(obs.Summary, "lap_summary") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected delivery observation, got: %v", snap.Observations["delivery"])
	}
}

// TestNackInterrupted_DeadConsumerDropsImmediately: when the reason is a
// known dead-consumer signal (Live WS half-closed mid-dispatch), the bus
// short-circuits the retry budget and drops on the first nack instead of
// burning 3 attempts against a socket that won't recover this tick.
func TestNackInterrupted_DeadConsumerDropsImmediately(t *testing.T) {
	for _, reason := range []string{
		NackReasonSendClientContentFailed,
		NackReasonNoConsumer,
		NackReasonSessionDead,
	} {
		t.Run(reason, func(t *testing.T) {
			b, _ := newLeaseTestBrain()
			stored, _ := b.EnqueueEvent(validEvent(EventLapSummary), 10)

			leased := b.LeaseNext(1, 3)
			if leased == nil || leased.ID != stored.ID {
				t.Fatalf("lease failed: %+v", leased)
			}
			if err := b.NackInterrupted(stored.ID, reason); err != nil {
				t.Fatalf("nack: %v", err)
			}
			if got := len(b.PeekEvents()); got != 0 {
				t.Errorf("expected immediate drop, queue len = %d", got)
			}

			snap := b.Snapshot(DefaultSnapshotOpts())
			found := false
			for _, obs := range snap.Observations["delivery"] {
				if strings.Contains(obs.Summary, reason) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected delivery observation mentioning %q, got: %v",
					reason, snap.Observations["delivery"])
			}
		})
	}
}

// TestNackInterrupted_RegularReasonStillRetries: a "user_barge_in" nack
// keeps using the per-priority retry budget — dead-consumer fast-drop must
// not bleed into the existing behaviour for transient failures.
func TestNackInterrupted_RegularReasonStillRetries(t *testing.T) {
	b, _ := newLeaseTestBrain()
	stored, _ := b.EnqueueEvent(validEvent(EventLapSummary), 10)

	leased := b.LeaseNext(1, 3)
	if leased == nil || leased.ID != stored.ID {
		t.Fatalf("lease failed")
	}
	if err := b.NackInterrupted(stored.ID, "user_barge_in"); err != nil {
		t.Fatalf("nack: %v", err)
	}
	q := b.PeekEvents()
	if len(q) != 1 || q[0].Status != StatusQueued {
		t.Errorf("expected event re-queued after transient nack, got %+v", q)
	}
}

// TestReapStaleLeases_NacksTimedOutInFlight: an event leased but not acked
// within LeaseTimeout is nacked with reason "lease_timeout" by the reaper.
func TestReapStaleLeases_NacksTimedOutInFlight(t *testing.T) {
	b, now := newLeaseTestBrain()
	stored, _ := b.EnqueueEvent(validEvent(EventBoxNow), 10)
	leased := b.LeaseNext(4, 5)
	if leased == nil || leased.ID != stored.ID {
		t.Fatalf("lease failed")
	}

	// No reap yet: we're inside the lease window.
	if got := b.ReapStaleLeases(); got != 0 {
		t.Errorf("expected 0 reaps inside window, got %d", got)
	}

	// Advance past the timeout. Use the per-event budget so this test
	// stays valid for any LeaseTimeoutFor schedule (box_now is in the
	// tool-using bucket with a longer budget than the default).
	*now = now.Add(LeaseTimeoutFor(*leased) + time.Second)
	if got := b.ReapStaleLeases(); got != 1 {
		t.Errorf("expected 1 reap, got %d", got)
	}

	// Event back to queued, attempts=1 (only one lease so far).
	q := b.PeekEvents()
	if len(q) != 1 {
		t.Fatalf("expected event still in queue, got %d", len(q))
	}
	if q[0].Status != StatusQueued {
		t.Errorf("expected queued after reap, got %s", q[0].Status)
	}
}

// TestLeaseNext_DropsStaleByTTL: an event that has lived past its
// StaleAfter window without being leased is silently dropped on the next
// lease attempt. Critical events (StaleAfter==0) never expire.
func TestLeaseNext_DropsStaleByTTL(t *testing.T) {
	b, now := newLeaseTestBrain()

	// traffic_update has StaleAfter=15s (proximity goes stale fast).
	if _, err := b.EnqueueEvent(validEvent(EventTrafficUpdate), 10); err != nil {
		t.Fatalf("enqueue traffic_update: %v", err)
	}
	// box_now has StaleAfter=0 — must always be delivered.
	if _, err := b.EnqueueEvent(validEvent(EventBoxNow), 10); err != nil {
		t.Fatalf("enqueue box_now: %v", err)
	}

	*now = now.Add(20 * time.Second)

	// Idle lane lease: the proximity event should be filtered out as stale.
	idle := b.LeaseNext(1, 3)
	if idle != nil {
		t.Errorf("expected nil (proximity expired), got %s", idle.Type)
	}

	// Fast lane: box_now still leasable.
	fast := b.LeaseNext(4, 5)
	if fast == nil || fast.Type != EventBoxNow {
		t.Fatalf("expected box_now from fast lane, got %+v", fast)
	}

	// Queue should now hold only the in_flight box_now.
	q := b.PeekEvents()
	if len(q) != 1 || q[0].Type != EventBoxNow {
		t.Errorf("queue contents wrong: %+v", q)
	}
}

// TestLeaseNext_ReturnsNilWhenInFlight: an in_flight event blocks both
// priority bands until acked or nacked (single-flight invariant).
func TestLeaseNext_ReturnsNilWhenInFlight(t *testing.T) {
	b, _ := newLeaseTestBrain()
	if _, err := b.EnqueueEvent(validEvent(EventLapSummary), 10); err != nil {
		t.Fatalf("enqueue lap_summary: %v", err)
	}
	if _, err := b.EnqueueEvent(validEvent(EventThreatOvertake), 10); err != nil {
		t.Fatalf("enqueue threat: %v", err)
	}

	// Lease the high-priority one.
	first := b.LeaseNext(1, 5)
	if first == nil || first.Type != EventThreatOvertake {
		t.Fatalf("first lease wrong: %+v", first)
	}

	// Both bands now blocked.
	if x := b.LeaseNext(1, 3); x != nil {
		t.Errorf("expected nil on idle band, got %s", x.Type)
	}
	if x := b.LeaseNext(4, 5); x != nil {
		t.Errorf("expected nil on fast band, got %s", x.Type)
	}
}

// TestAckDelivered_RequiresAckHoldsAwaiting: ack on a RequiresAck event
// holds it in the queue under StatusAwaitingAck instead of removing it.
// The L4 speech buffer is still populated so dedup-against-recent-radio
// keeps working.
func TestAckDelivered_RequiresAckHoldsAwaiting(t *testing.T) {
	b, _ := newLeaseTestBrain()
	// EventBoxNow has RequiresAck=true by default.
	stored, err := b.EnqueueEvent(validEvent(EventBoxNow), 10)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if !stored.RequiresAck {
		t.Fatalf("expected box_now to default to RequiresAck=true")
	}

	leased := b.LeaseNext(4, 5)
	if leased == nil {
		t.Fatal("lease nil")
	}
	if err := b.AckDelivered(stored.ID, "Box, box.", 5); err != nil {
		t.Fatalf("ack: %v", err)
	}

	q := b.PeekEvents()
	if len(q) != 1 {
		t.Fatalf("expected 1 event still in queue (awaiting_ack), got %d", len(q))
	}
	if q[0].Status != StatusAwaitingAck {
		t.Errorf("expected awaiting_ack status, got %s", q[0].Status)
	}
	if q[0].AwaitingAckSince.IsZero() {
		t.Errorf("expected AwaitingAckSince to be set")
	}

	// Speech still recorded.
	snap := b.Snapshot(DefaultSnapshotOpts())
	if len(snap.Speech) != 1 {
		t.Fatalf("expected 1 speech record, got %d", len(snap.Speech))
	}
	if len(snap.AwaitingAck) != 1 {
		t.Errorf("expected awaiting_ack to surface in snapshot, got %d", len(snap.AwaitingAck))
	}
}

// TestConfirmAck_RemovesAwaitingEvent: ConfirmAck on an awaiting-ack
// event removes it from the queue. Subsequent ConfirmAck on the same id
// is idempotent (returns ErrEventNotFound).
func TestConfirmAck_RemovesAwaitingEvent(t *testing.T) {
	b, _ := newLeaseTestBrain()
	stored, _ := b.EnqueueEvent(validEvent(EventBoxNow), 10)
	b.LeaseNext(4, 5)
	_ = b.AckDelivered(stored.ID, "Box, box.", 5)

	if err := b.ConfirmAck(stored.ID, "copy that"); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if got := len(b.PeekEvents()); got != 0 {
		t.Errorf("expected queue empty after confirm, got %d", got)
	}
	// Idempotent.
	if err := b.ConfirmAck(stored.ID, "copy"); !errors.Is(err, ErrEventNotFound) {
		t.Errorf("expected ErrEventNotFound on double-confirm, got %v", err)
	}
}

// TestConfirmAck_RejectsNonAwaitingEvent: ConfirmAck refuses to operate
// on queued or in-flight events — only awaiting-ack is eligible.
func TestConfirmAck_RejectsNonAwaitingEvent(t *testing.T) {
	b, _ := newLeaseTestBrain()
	stored, _ := b.EnqueueEvent(validEvent(EventLapSummary), 10)
	if err := b.ConfirmAck(stored.ID, "copy"); !errors.Is(err, ErrEventNotFound) {
		t.Errorf("expected ErrEventNotFound on queued event, got %v", err)
	}
}

// TestNagAck_FlipsBackToQueuedAndIncrementsCount: NagAck on an awaiting
// event resets it to queued, increments NagCount, and zeroes Attempts so
// the renag gets a fresh retry budget.
func TestNagAck_FlipsBackToQueuedAndIncrementsCount(t *testing.T) {
	b, _ := newLeaseTestBrain()
	stored, _ := b.EnqueueEvent(validEvent(EventBoxNow), 10)
	b.LeaseNext(4, 5)
	_ = b.AckDelivered(stored.ID, "Box.", 5)

	if err := b.NagAck(stored.ID); err != nil {
		t.Fatalf("nag: %v", err)
	}
	q := b.PeekEvents()
	if len(q) != 1 || q[0].Status != StatusQueued {
		t.Fatalf("expected queued after nag, got %+v", q)
	}
	if q[0].NagCount != 1 {
		t.Errorf("expected NagCount=1, got %d", q[0].NagCount)
	}
	if q[0].Attempts != 0 {
		t.Errorf("expected Attempts reset to 0 on nag, got %d", q[0].Attempts)
	}
}

// TestNagAck_DropsAfterMaxNags: the second NagAck past MaxAckNags drops
// the event and records an Observation under TopicDelivery.
func TestNagAck_DropsAfterMaxNags(t *testing.T) {
	b, _ := newLeaseTestBrain()
	stored, _ := b.EnqueueEvent(validEvent(EventBoxNow), 10)
	b.LeaseNext(4, 5)
	_ = b.AckDelivered(stored.ID, "Box.", 5)

	// First nag: back to queued.
	if err := b.NagAck(stored.ID); err != nil {
		t.Fatalf("first nag: %v", err)
	}
	// Cycle back to awaiting_ack.
	b.LeaseNext(4, 5)
	_ = b.AckDelivered(stored.ID, "Box, box.", 5)

	// Second nag: should drop (MaxAckNags=2 means total redeliveries cap is 2).
	if err := b.NagAck(stored.ID); err != nil {
		t.Fatalf("second nag: %v", err)
	}
	if got := len(b.PeekEvents()); got != 0 {
		t.Errorf("expected queue empty after MaxAckNags drop, got %d", got)
	}
	// Observation should land under topic "delivery".
	snap := b.Snapshot(DefaultSnapshotOpts())
	found := false
	for _, obs := range snap.Observations[TopicDelivery] {
		if strings.Contains(obs.Summary, "unacknowledged nags") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected delivery observation, got: %v", snap.Observations[TopicDelivery])
	}
}

// TestReapAwaitingAcks_NagsAfterQuietPeriod: an awaiting-ack event whose
// AwaitingAckSince predates now-AckNagAfter gets renagged.
func TestReapAwaitingAcks_NagsAfterQuietPeriod(t *testing.T) {
	b, now := newLeaseTestBrain()
	stored, _ := b.EnqueueEvent(validEvent(EventBoxNow), 10)
	b.LeaseNext(4, 5)
	_ = b.AckDelivered(stored.ID, "Box.", 5)

	// Inside the window: no reap.
	if got := b.ReapAwaitingAcks(); got != 0 {
		t.Errorf("expected 0 inside window, got %d", got)
	}
	// Advance past AckNagAfter.
	*now = now.Add(AckNagAfter + time.Second)
	if got := b.ReapAwaitingAcks(); got != 1 {
		t.Errorf("expected 1 nag, got %d", got)
	}
	q := b.PeekEvents()
	if len(q) != 1 || q[0].Status != StatusQueued || q[0].NagCount != 1 {
		t.Errorf("expected requeued with nag_count=1, got %+v", q)
	}
}

// TestFilterStaleLocked_ExemptsAwaitingAck: awaiting-ack events are NOT
// silently dropped by the StaleAfter TTL — MaxAckNags is their ceiling.
func TestFilterStaleLocked_ExemptsAwaitingAck(t *testing.T) {
	b, now := newLeaseTestBrain()
	// Use plan_imminent which has both StaleAfter=20s AND RequiresAck=true.
	ev := validEvent(EventPlanImminent)
	stored, err := b.EnqueueEvent(ev, 10)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	b.LeaseNext(4, 5)
	_ = b.AckDelivered(stored.ID, "Pit entry coming.", 4)

	// Advance way past plan_imminent's 20s StaleAfter window.
	*now = now.Add(45 * time.Second)
	// LeaseNext triggers filterStaleLocked; the awaiting-ack event must
	// survive.
	_ = b.LeaseNext(1, 5)
	if got := len(b.PeekEvents()); got != 1 {
		t.Errorf("expected awaiting-ack to survive TTL filter, got %d", got)
	}
}

// TestDequeueNext_SkipsInFlight: legacy DequeueNext does not pop in_flight
// events — keeps the lease invariant intact even for callers that haven't
// migrated to the lease/ack contract.
func TestDequeueNext_SkipsInFlight(t *testing.T) {
	b, _ := newLeaseTestBrain()
	if _, err := b.EnqueueEvent(validEvent(EventBoxNow), 10); err != nil {
		t.Fatalf("enqueue box_now: %v", err)
	}
	if _, err := b.EnqueueEvent(validEvent(EventLapSummary), 10); err != nil {
		t.Fatalf("enqueue lap_summary: %v", err)
	}

	// Lease the urgent one.
	if leased := b.LeaseNext(4, 5); leased == nil {
		t.Fatal("lease box_now")
	}

	// DequeueNext on the idle band should still return lap_summary —
	// in_flight skipping is per-event, not whole-queue.
	got := b.DequeueNext(1, 3)
	if got == nil || got.Type != EventLapSummary {
		t.Fatalf("expected lap_summary from DequeueNext, got %+v", got)
	}
}
