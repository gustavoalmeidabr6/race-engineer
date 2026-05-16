package brain

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
)

func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func TestWriteSetsTimestampAndSnapshotReturnsObservation(t *testing.T) {
	now := time.Unix(1000, 0)
	b := New(func() *models.RaceState { return nil }, StaticContext{})
	b.now = fixedClock(now)

	b.Write(Observation{
		Topic:      TopicTireDegradation,
		Agent:      "test",
		Summary:    "RR 0.4%/lap",
		Confidence: 0.8,
	})

	snap := b.Snapshot(SnapshotOpts{Topics: []Topic{TopicTireDegradation}})
	got := snap.Observations[TopicTireDegradation]
	if len(got) != 1 {
		t.Fatalf("want 1 observation, got %d", len(got))
	}
	if !got[0].At.Equal(now) {
		t.Fatalf("Write should stamp At; got %v want %v", got[0].At, now)
	}
}

func TestSnapshotFiltersExpiredObservations(t *testing.T) {
	base := time.Unix(1000, 0)
	b := New(nil, StaticContext{})
	b.now = fixedClock(base)

	b.Write(Observation{Topic: TopicTireDegradation, Agent: "old", Summary: "stale"})

	b.now = fixedClock(base.Add(5 * time.Minute))
	snap := b.Snapshot(SnapshotOpts{
		Topics:            []Topic{TopicTireDegradation},
		MaxObservationAge: time.Minute,
	})
	if len(snap.Observations[TopicTireDegradation]) != 0 {
		t.Fatalf("stale observation should be filtered out")
	}
}

func TestObservationRingBufferIsBounded(t *testing.T) {
	b := New(nil, StaticContext{})
	for i := 0; i < maxObservationsPerTopic+5; i++ {
		b.Write(Observation{Topic: TopicTireDegradation, Agent: "x", Summary: "y"})
	}
	snap := b.Snapshot(SnapshotOpts{Topics: []Topic{TopicTireDegradation}})
	if got := len(snap.Observations[TopicTireDegradation]); got != maxObservationsPerTopic {
		t.Fatalf("ring buffer should cap at %d, got %d", maxObservationsPerTopic, got)
	}
}

func TestHypothesisLatestWinsAndDoesNotPolluteObservations(t *testing.T) {
	base := time.Unix(1000, 0)
	b := New(nil, StaticContext{})
	b.now = fixedClock(base)

	b.Write(Observation{Topic: TopicPitWindow, Agent: "a", Summary: "first", Hypothesis: true})
	b.now = fixedClock(base.Add(10 * time.Second))
	b.Write(Observation{Topic: TopicPitWindow, Agent: "b", Summary: "second", Hypothesis: true})

	snap := b.Snapshot(SnapshotOpts{Topics: []Topic{TopicPitWindow}})
	h, ok := snap.Hypotheses[TopicPitWindow]
	if !ok {
		t.Fatalf("hypothesis missing")
	}
	if h.Summary != "second" || h.Agent != "b" {
		t.Fatalf("expected latest-wins, got %+v", h)
	}
	if len(snap.Observations[TopicPitWindow]) != 0 {
		t.Fatalf("hypothesis should not populate L2 observations")
	}
}

func TestRecordSpeechAndDriverAreBounded(t *testing.T) {
	b := New(nil, StaticContext{})

	for i := 0; i < maxRecentSpeech+5; i++ {
		b.RecordSpeech("hello", 3)
	}
	for i := 0; i < maxDriverEvents+5; i++ {
		b.RecordDriver("ack", "ack")
	}

	snap := b.Snapshot(SnapshotOpts{
		IncludeRecentSpeech: true,
		IncludeDriver:       true,
	})
	if got := len(snap.Speech); got != maxRecentSpeech {
		t.Fatalf("speech bound %d, got %d", maxRecentSpeech, got)
	}
	if got := len(snap.DriverEvents); got != maxDriverEvents {
		t.Fatalf("driver events bound %d, got %d", maxDriverEvents, got)
	}
}

func TestRecordSpeechIgnoresEmptyText(t *testing.T) {
	b := New(nil, StaticContext{})
	b.RecordSpeech("", 5)
	snap := b.Snapshot(SnapshotOpts{IncludeRecentSpeech: true})
	if len(snap.Speech) != 0 {
		t.Fatalf("empty speech should be ignored")
	}
}

func TestEventWindowFilter(t *testing.T) {
	base := time.Unix(1000, 0)
	b := New(nil, StaticContext{})
	b.now = fixedClock(base)

	b.RecordEvent(RaceEvent{Code: "OLD"})

	b.now = fixedClock(base.Add(10 * time.Minute))
	b.RecordEvent(RaceEvent{Code: "NEW"})

	snap := b.Snapshot(SnapshotOpts{
		IncludeEvents: true,
		EventWindow:   5 * time.Minute,
	})
	if len(snap.Events) != 1 || snap.Events[0].Code != "NEW" {
		t.Fatalf("expected only NEW within window, got %+v", snap.Events)
	}
}

func TestUnknownTopicIsAcceptedButNotSerialized(t *testing.T) {
	b := New(nil, StaticContext{})
	b.Write(Observation{Topic: Topic("rogue.topic"), Agent: "x", Summary: "y"})

	snap := b.Snapshot(DefaultSnapshotOpts())
	for _, t := range AllTopics {
		if len(snap.Observations[t]) > 0 {
			break
		}
	}
	md := snap.Markdown()
	if strings.Contains(md, "rogue.topic") {
		t.Fatalf("unknown topic leaked into markdown: %s", md)
	}
}

func TestMarkdownContainsExpectedSections(t *testing.T) {
	base := time.Unix(1000, 0)
	b := New(nil, StaticContext{
		User:          "Driver hates filler",
		DriverProfile: "Aggressive style",
	})
	b.now = fixedClock(base)

	b.Write(Observation{
		Topic: TopicTireDegradation, Agent: "analyst",
		Summary: "RR 0.4%/lap", Confidence: 0.8,
	})
	b.Write(Observation{
		Topic: TopicPitWindow, Agent: "strategist",
		Summary: "Box L23-L25", Confidence: 0.7, Hypothesis: true,
	})
	b.RecordSpeech("Push now", 4)
	b.RecordDriver("how are tires", "query")
	b.RecordEvent(RaceEvent{Code: "SCDP"})

	snap := b.Snapshot(DefaultSnapshotOpts())
	md := snap.Markdown()

	for _, want := range []string{
		"## Active Observations",
		"tires.degradation",
		"HYPOTHESIS",
		"pit.window",
		"## Recent Race Events",
		"SCDP",
		"## Recent Radio",
		"Push now",
		"## Driver State",
		"how are tires",
		"## Driver Preferences",
		"Driver hates filler",
		"## Driver Profile",
		"Aggressive style",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q\n--- got ---\n%s", want, md)
		}
	}
}

func TestSnapshotIsImmutableCopyOfSpeech(t *testing.T) {
	b := New(nil, StaticContext{})
	b.RecordSpeech("first", 3)

	snap := b.Snapshot(SnapshotOpts{IncludeRecentSpeech: true})
	if len(snap.Speech) != 1 {
		t.Fatalf("setup wrong, got %d", len(snap.Speech))
	}

	b.RecordSpeech("second", 3)
	if len(snap.Speech) != 1 {
		t.Fatalf("snapshot should not change after later writes, got %d", len(snap.Speech))
	}
}

func TestConcurrentWritesAndReads(t *testing.T) {
	b := New(nil, StaticContext{})
	const writers = 8
	const reads = 200
	const writes = 200

	var wg sync.WaitGroup
	wg.Add(writers + 1)

	for w := 0; w < writers; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < writes; i++ {
				b.Write(Observation{Topic: TopicTireDegradation, Agent: "w", Summary: "x"})
				b.RecordEvent(RaceEvent{Code: "FTLP"})
				b.RecordSpeech("hi", 2)
				b.RecordDriver("q", "query")
			}
		}()
	}

	go func() {
		defer wg.Done()
		for i := 0; i < reads; i++ {
			_ = b.Snapshot(DefaultSnapshotOpts())
		}
	}()

	wg.Wait()
}

func TestKnownTopicLookup(t *testing.T) {
	if !Known(TopicTireDegradation) {
		t.Fatalf("registered topic should be known")
	}
	if Known(Topic("not.real")) {
		t.Fatalf("unknown topic should not be reported as known")
	}
}
