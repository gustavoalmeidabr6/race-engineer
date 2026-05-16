package brain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// fixedNow returns a clock that ticks predictably so tests can assert dedup
// windows without sleeping.
func fixedNow(start time.Time) func() time.Time {
	cur := start
	return func() time.Time {
		t := cur
		cur = cur.Add(100 * time.Millisecond)
		return t
	}
}

func newTestBrain() *RaceBrain {
	b := New(nil, StaticContext{})
	b.now = fixedNow(time.Date(2026, 5, 9, 21, 0, 0, 0, time.UTC))
	return b
}

func validEvent(t EventType) Event {
	return Event{
		Type:      t,
		Priority:  DefaultEventPriority(t),
		Summary:   "test summary, brief.",
		Reasoning: "reasoning with numbers like gap=1.4s.",
		DebugData: map[string]any{"gap_s": 1.4},
		Source:    "test",
	}
}

func TestEnqueueEvent_AssignsIDAndAt(t *testing.T) {
	b := newTestBrain()
	e := validEvent(EventThreatOvertake)
	stored, err := b.EnqueueEvent(e, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(stored.ID, "evt_") {
		t.Errorf("expected evt_ prefix, got %q", stored.ID)
	}
	if stored.At.IsZero() {
		t.Error("expected At to be set")
	}
}

func TestEnqueueEvent_RejectsLengthViolations(t *testing.T) {
	b := newTestBrain()

	// Summary too long.
	e := validEvent(EventInfo)
	e.Summary = strings.Repeat("a", MaxEventSummaryChars+1)
	if _, err := b.EnqueueEvent(e, 10); err == nil {
		t.Error("expected length error for oversized summary")
	}

	// Reasoning too long.
	e = validEvent(EventInfo)
	e.Reasoning = strings.Repeat("b", MaxEventReasoningChars+1)
	if _, err := b.EnqueueEvent(e, 10); err == nil {
		t.Error("expected length error for oversized reasoning")
	}

	// Debug data too big.
	e = validEvent(EventInfo)
	e.DebugData = map[string]any{"blob": strings.Repeat("c", MaxEventDebugBytes+50)}
	if _, err := b.EnqueueEvent(e, 10); err == nil || !errors.Is(err, ErrEventDebugTooBig) {
		t.Errorf("expected debug-size error, got %v", err)
	}
}

// analyst_answer events get a relaxed Reasoning cap (500 chars) because
// pi_agent's contract is ≤3 complete sentences — well over the 200-char
// default. Verify the per-type cap path in Validate().
func TestEnqueueEvent_AnalystAnswerRelaxedReasoningCap(t *testing.T) {
	b := newTestBrain()

	// 500-char reasoning is allowed.
	e := validEvent(EventAnalystAnswer)
	e.Reasoning = strings.Repeat("r", MaxAnalystReasoningChars)
	if _, err := b.EnqueueEvent(e, 10); err != nil {
		t.Errorf("expected analyst_answer to accept %d-char reasoning, got %v", MaxAnalystReasoningChars, err)
	}

	// 501 chars is rejected.
	e2 := validEvent(EventAnalystAnswer)
	e2.Reasoning = strings.Repeat("r", MaxAnalystReasoningChars+1)
	e2.DedupKey = "different-key" // avoid dedup collision with the prior event
	if _, err := b.EnqueueEvent(e2, 10); err == nil {
		t.Errorf("expected analyst_answer to reject %d-char reasoning", MaxAnalystReasoningChars+1)
	}

	// Non-analyst event still capped at MaxEventReasoningChars.
	e3 := validEvent(EventInfo)
	e3.Reasoning = strings.Repeat("r", MaxEventReasoningChars+1)
	if _, err := b.EnqueueEvent(e3, 10); err == nil {
		t.Errorf("expected non-analyst event to reject reasoning > %d chars", MaxEventReasoningChars)
	}
}

func TestEnqueueEvent_BoxNowMustBeP5(t *testing.T) {
	b := newTestBrain()
	e := validEvent(EventBoxNow)
	e.Priority = 4
	if _, err := b.EnqueueEvent(e, 10); err == nil {
		t.Error("expected validation error: box_now must be P5")
	}
}

func TestEnqueueEvent_TalkLevelFloor(t *testing.T) {
	b := newTestBrain()
	// TalkLevel 2 → only P5 events allowed.
	e := validEvent(EventLapSummary) // P2
	if _, err := b.EnqueueEvent(e, 2); !errors.Is(err, ErrEventTalkLevel) {
		t.Errorf("expected ErrEventTalkLevel, got %v", err)
	}
	// P5 still passes at TalkLevel 2.
	e2 := validEvent(EventBoxNow)
	if _, err := b.EnqueueEvent(e2, 2); err != nil {
		t.Errorf("expected P5 to pass TalkLevel 2, got %v", err)
	}
}

func TestEnqueueEvent_DedupAgainstQueue(t *testing.T) {
	b := newTestBrain()
	e := validEvent(EventLapSummary)
	e.DedupKey = "lap_summary:14"
	if _, err := b.EnqueueEvent(e, 10); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if _, err := b.EnqueueEvent(e, 10); !errors.Is(err, ErrEventDeduped) {
		t.Errorf("expected ErrEventDeduped, got %v", err)
	}
}

func TestDequeueNext_PriorityOrder(t *testing.T) {
	b := newTestBrain()

	// Insert in mixed-priority order. Step the clock by more than the
	// highPriorityCoalesceWindow between each insert so the new HP
	// coalesce path doesn't fold box_now + safety_car into one event —
	// this test only cares about per-priority drain ordering.
	cur := time.Date(2026, 5, 9, 21, 0, 0, 0, time.UTC)
	b.now = func() time.Time {
		t := cur
		cur = cur.Add(highPriorityCoalesceWindow + time.Second)
		return t
	}
	for _, et := range []EventType{
		EventLapSummary,     // P2
		EventBoxNow,         // P5
		EventThreatOvertake, // P3
		EventSafetyCar,      // P4
	} {
		e := validEvent(et)
		if _, err := b.EnqueueEvent(e, 10); err != nil {
			t.Fatalf("insert %s: %v", et, err)
		}
	}

	// Fast lane drains P4–P5 first; expect box_now (P5) then safety_car (P4).
	first := b.DequeueNext(4, 5)
	if first == nil || first.Type != EventBoxNow {
		t.Fatalf("expected box_now first, got %+v", first)
	}
	second := b.DequeueNext(4, 5)
	if second == nil || second.Type != EventSafetyCar {
		t.Fatalf("expected safety_car second, got %+v", second)
	}
	// Fast lane is now empty.
	if x := b.DequeueNext(4, 5); x != nil {
		t.Errorf("expected nil, got %+v", x)
	}
	// Idle lane should see threat_overtake then lap_summary.
	third := b.DequeueNext(1, 3)
	if third == nil || third.Type != EventThreatOvertake {
		t.Fatalf("expected threat_overtake, got %+v", third)
	}
	fourth := b.DequeueNext(1, 3)
	if fourth == nil || fourth.Type != EventLapSummary {
		t.Fatalf("expected lap_summary, got %+v", fourth)
	}
}

func TestDequeueNext_EmptyReturnsNil(t *testing.T) {
	b := newTestBrain()
	if x := b.DequeueNext(1, 5); x != nil {
		t.Errorf("expected nil from empty queue, got %+v", x)
	}
}

func TestEnqueueEvent_CapacityBound(t *testing.T) {
	b := newTestBrain()
	// Push 50 events; queue should cap at maxEventQueue (32).
	for i := 0; i < 50; i++ {
		e := validEvent(EventInfo)
		e.Summary = "info event"
		// Distinct DedupKey so each one is accepted.
		e.DedupKey = string(rune('a'+i%26)) + string(rune('a'+i/26))
		if _, err := b.EnqueueEvent(e, 10); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if got := len(b.PeekEvents()); got != maxEventQueue {
		t.Errorf("expected queue capped at %d, got %d", maxEventQueue, got)
	}
}

func TestEnqueueEvent_AnalystWhitelist(t *testing.T) {
	// The whitelist itself is a simple lookup; verify a few key inclusions
	// and exclusions so the analyst handler can rely on it.
	if _, ok := AnalystAllowedEventTypes[EventBoxNow]; !ok {
		t.Error("box_now should be in analyst whitelist")
	}
	if _, ok := AnalystAllowedEventTypes[EventTireCliff]; !ok {
		t.Error("tire_cliff should be in analyst whitelist")
	}
	if _, ok := AnalystAllowedEventTypes[EventRedFlag]; ok {
		t.Error("red_flag must NOT be in analyst whitelist (game-state only)")
	}
	if _, ok := AnalystAllowedEventTypes[EventSafetyCar]; ok {
		t.Error("safety_car must NOT be in analyst whitelist (game-state only)")
	}
}

func TestMinPriorityForTalkLevel(t *testing.T) {
	cases := []struct {
		talk int32
		want int
	}{
		{1, 5}, {2, 5},
		{3, 4}, {4, 4},
		{5, 2}, {6, 2}, {7, 2},
		{8, 1}, {9, 1}, {10, 1},
	}
	for _, c := range cases {
		if got := MinPriorityForTalkLevel(c.talk); got != c.want {
			t.Errorf("TalkLevel %d → expected floor %d, got %d", c.talk, c.want, got)
		}
	}
}

// TestEnqueueEvent_SubjectDedup: a threat event for the same DedupSubject
// is rejected while a recent speech record for that subject sits in the
// buffer, even when the DedupKey is different (cross-rule dedup).
func TestEnqueueEvent_SubjectDedup(t *testing.T) {
	b := newTestBrain()

	// First event: closing_threat for car:5. Lease + ack so it lands in
	// the speech buffer with Subject=car:5.
	first := validEvent(EventThreatOvertake)
	first.Priority = 4 // matches the rule's override at fire time
	first.DedupKey = "closing_threat:14:5"
	first.DedupSubject = "car:5"
	if _, err := b.EnqueueEvent(first, 10); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	leased := b.LeaseNext(4, 5)
	if leased == nil {
		t.Fatalf("lease failed")
	}
	if err := b.AckDelivered(leased.ID, "Leclerc closing", 4); err != nil {
		t.Fatalf("ack failed: %v", err)
	}

	// Second event: car_approaching for the same car:5. Different
	// DedupKey (lap+car_index), but same subject. Should be deduped.
	second := validEvent(EventCarApproaching)
	second.DedupKey = "car_approaching:5:14"
	second.DedupSubject = "car:5"
	if _, err := b.EnqueueEvent(second, 10); !errors.Is(err, ErrEventDeduped) {
		t.Errorf("expected ErrEventDeduped for same-subject follow-up, got %v", err)
	}

	// Third event: same type for a DIFFERENT subject. Should pass.
	third := validEvent(EventCarApproaching)
	third.DedupKey = "car_approaching:7:14"
	third.DedupSubject = "car:7"
	if _, err := b.EnqueueEvent(third, 10); err != nil {
		t.Errorf("expected different-subject event to pass, got %v", err)
	}
}

// TestEnqueueEvent_ThreatCoalesce: two threat-class events for different
// cars arriving within the coalesce window collapse into one queue entry
// whose Summary mentions both subjects and whose Priority is the higher
// of the two.
func TestEnqueueEvent_ThreatCoalesce(t *testing.T) {
	b := newTestBrain()

	first := validEvent(EventThreatOvertake)
	first.Priority = 4
	first.Summary = "Leclerc closing 1.2s."
	first.DedupKey = "closing_threat:14:5"
	first.DedupSubject = "car:5"
	if _, err := b.EnqueueEvent(first, 10); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	second := validEvent(EventCarApproaching)
	second.Priority = 2
	second.Summary = "Verstappen 1.8s."
	second.DedupKey = "car_approaching:7:14"
	second.DedupSubject = "car:7"
	merged, err := b.EnqueueEvent(second, 10)
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}
	if merged == nil {
		t.Fatalf("expected merged event back, got nil")
	}

	q := b.PeekEvents()
	if len(q) != 1 {
		t.Fatalf("expected coalesced queue size 1, got %d", len(q))
	}
	got := q[0]
	if got.Priority != 4 {
		t.Errorf("expected Priority 4 (max of 4,2), got %d", got.Priority)
	}
	if !strings.Contains(got.Summary, "Leclerc") || !strings.Contains(got.Summary, "Verstappen") {
		t.Errorf("expected merged summary to mention both, got %q", got.Summary)
	}
	if !strings.Contains(got.DedupSubject, "car:5") || !strings.Contains(got.DedupSubject, "car:7") {
		t.Errorf("expected merged subject to include both car ids, got %q", got.DedupSubject)
	}
}

// TestEnqueueEvent_ThreatCoalesce_OutsideWindow: same scenario but with
// the second event arriving after the coalesce window — they queue
// separately so the dispatcher can voice them as two distinct calls.
func TestEnqueueEvent_ThreatCoalesce_OutsideWindow(t *testing.T) {
	cur := time.Date(2026, 5, 9, 21, 0, 0, 0, time.UTC)
	b := New(nil, StaticContext{})
	b.now = func() time.Time {
		t := cur
		return t
	}

	first := validEvent(EventThreatOvertake)
	first.Priority = 4
	first.DedupKey = "closing_threat:14:5"
	first.DedupSubject = "car:5"
	if _, err := b.EnqueueEvent(first, 10); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	// Advance the clock past the coalesce window.
	cur = cur.Add(threatCoalesceWindow + time.Second)

	second := validEvent(EventCarApproaching)
	second.Priority = 2
	second.DedupKey = "car_approaching:7:14"
	second.DedupSubject = "car:7"
	if _, err := b.EnqueueEvent(second, 10); err != nil {
		t.Fatalf("second insert: %v", err)
	}

	if got := len(b.PeekEvents()); got != 2 {
		t.Errorf("expected separate events outside coalesce window, got queue size %d", got)
	}
}

// TestEnqueue_HighPriorityCoalesce verifies the generalised P4+ coalesce
// path: two non-threat-class P4+ events arriving within the window merge
// into one bundled event the dispatcher voices as a single radio call.
func TestEnqueue_HighPriorityCoalesce(t *testing.T) {
	cur := time.Date(2026, 5, 9, 21, 0, 0, 0, time.UTC)
	b := New(nil, StaticContext{})
	b.now = func() time.Time { return cur }

	first := validEvent(EventTireCliff)
	first.Priority = 4
	first.Summary = "Tyres past cliff."
	first.DedupKey = "tire_cliff:14"
	if _, err := b.EnqueueEvent(first, 10); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	// Advance 200ms — well inside highPriorityCoalesceWindow.
	cur = cur.Add(200 * time.Millisecond)

	second := validEvent(EventBoxNow)
	second.Priority = 5
	second.Summary = "Box this lap."
	second.DedupKey = "box_now:14"
	merged, err := b.EnqueueEvent(second, 10)
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}
	if merged == nil {
		t.Fatalf("expected merged event back, got nil")
	}

	q := b.PeekEvents()
	if len(q) != 1 {
		t.Fatalf("expected coalesced queue size 1, got %d", len(q))
	}
	got := q[0]
	if got.Priority != 5 {
		t.Errorf("expected merged Priority 5 (max of 4,5), got %d", got.Priority)
	}
	if !strings.Contains(got.Summary, "Tyres past cliff") || !strings.Contains(got.Summary, "Box this lap") {
		t.Errorf("expected merged summary to mention both, got %q", got.Summary)
	}
}

// TestEnqueue_HighPriorityCoalesce_BelowFloor verifies a P3 event arriving
// near a P4 does NOT merge — only the high-priority lane bundles.
func TestEnqueue_HighPriorityCoalesce_BelowFloor(t *testing.T) {
	cur := time.Date(2026, 5, 9, 21, 0, 0, 0, time.UTC)
	b := New(nil, StaticContext{})
	b.now = func() time.Time { return cur }

	first := validEvent(EventSafetyCar)
	first.Priority = 4
	first.DedupKey = "safety_car:1"
	if _, err := b.EnqueueEvent(first, 10); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	cur = cur.Add(100 * time.Millisecond)

	second := validEvent(EventLapSummary)
	second.Priority = 3
	second.DedupKey = "lap_summary:14"
	if _, err := b.EnqueueEvent(second, 10); err != nil {
		t.Fatalf("second insert: %v", err)
	}

	if got := len(b.PeekEvents()); got != 2 {
		t.Errorf("expected separate events when one is below HP floor, got queue size %d", got)
	}
}

// TestEnqueue_HighPriorityCoalesce_OutsideWindow verifies the HP window
// behaves as a cooldown — events spaced past it dispatch separately.
func TestEnqueue_HighPriorityCoalesce_OutsideWindow(t *testing.T) {
	cur := time.Date(2026, 5, 9, 21, 0, 0, 0, time.UTC)
	b := New(nil, StaticContext{})
	b.now = func() time.Time { return cur }

	first := validEvent(EventTireCliff)
	first.Priority = 4
	first.DedupKey = "tire_cliff:14"
	if _, err := b.EnqueueEvent(first, 10); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	cur = cur.Add(highPriorityCoalesceWindow + time.Second)

	second := validEvent(EventSafetyCar)
	second.Priority = 4
	second.DedupKey = "safety_car:1"
	if _, err := b.EnqueueEvent(second, 10); err != nil {
		t.Fatalf("second insert: %v", err)
	}

	if got := len(b.PeekEvents()); got != 2 {
		t.Errorf("expected separate events outside HP coalesce window, got queue size %d", got)
	}
}

// TestMergePendingInto verifies the dispatcher-side bundle helper folds
// any queued P>=floor siblings into the named in-flight target.
func TestMergePendingInto(t *testing.T) {
	cur := time.Date(2026, 5, 9, 21, 0, 0, 0, time.UTC)
	b := New(nil, StaticContext{})
	b.now = func() time.Time { return cur }

	// Enqueue the target outside the HP coalesce window from the
	// sibling, so they don't merge at enqueue time — we want to
	// exercise MergePendingInto directly.
	target := validEvent(EventBoxNow)
	target.Priority = 5
	target.Summary = "Box this lap."
	target.DedupKey = "box_now:14"
	stored, err := b.EnqueueEvent(target, 10)
	if err != nil {
		t.Fatalf("target insert: %v", err)
	}

	// Lease the target so it's StatusInFlight (preconditions
	// MergePendingInto checks).
	leased := b.LeaseNext(4, 5)
	if leased == nil || leased.ID != stored.ID {
		t.Fatalf("expected to lease target, got %+v", leased)
	}

	// Now enqueue a sibling. We're still inside the threat/HP windows
	// but the target is in_flight so neither coalesce pass fires —
	// the sibling lands as its own queue entry.
	cur = cur.Add(100 * time.Millisecond)
	sibling := validEvent(EventSafetyCar)
	sibling.Priority = 4
	sibling.Summary = "Safety car deployed."
	sibling.DedupKey = "safety_car:1"
	if _, err := b.EnqueueEvent(sibling, 10); err != nil {
		t.Fatalf("sibling insert: %v", err)
	}

	if got := len(b.PeekEvents()); got != 2 {
		t.Fatalf("expected queue size 2 before merge, got %d", got)
	}

	merged, ids := b.MergePendingInto(stored.ID, highPriorityCoalesceFloor)
	if merged == nil {
		t.Fatalf("expected merged target, got nil")
	}
	if len(ids) != 1 {
		t.Errorf("expected exactly 1 merged sibling id, got %v", ids)
	}
	if !strings.Contains(merged.Summary, "Box this lap") || !strings.Contains(merged.Summary, "Safety car") {
		t.Errorf("expected merged summary to mention both, got %q", merged.Summary)
	}
	if got := len(b.PeekEvents()); got != 1 {
		t.Errorf("expected queue size 1 after merge, got %d", got)
	}
}

// TestMergePendingInto_NoTarget verifies the no-op path: when the named
// id isn't in_flight (e.g. already acked), nothing is merged.
func TestMergePendingInto_NoTarget(t *testing.T) {
	b := newTestBrain()
	if merged, ids := b.MergePendingInto("evt_doesnotexist", 4); merged != nil || ids != nil {
		t.Errorf("expected (nil, nil) for unknown id, got (%+v, %v)", merged, ids)
	}
}

// TestLeaseTimeoutFor: tool-using event types get the longer (15s) budget;
// everything else falls back to LeaseTimeout (8s).
func TestLeaseTimeoutFor(t *testing.T) {
	cases := []struct {
		typ  EventType
		want time.Duration
	}{
		{EventThreatOvertake, 15 * time.Second},
		{EventBoxNow, 15 * time.Second},
		{EventTireCliff, 15 * time.Second},
		{EventPitWindowOpen, 15 * time.Second},
		{EventCarApproaching, LeaseTimeout},
		{EventLapSummary, LeaseTimeout},
		{EventOvertaken, LeaseTimeout},
	}
	for _, c := range cases {
		got := LeaseTimeoutFor(Event{Type: c.typ})
		if got != c.want {
			t.Errorf("LeaseTimeoutFor(%s) = %v, want %v", c.typ, got, c.want)
		}
	}
}
