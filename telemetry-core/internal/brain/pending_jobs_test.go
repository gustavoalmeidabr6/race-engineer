package brain

import (
	"strings"
	"testing"
	"time"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
)

func TestRegisterPendingJob_AppendsAndReturnsCopy(t *testing.T) {
	b := New(func() *models.RaceState { return nil }, StaticContext{})
	base := time.Unix(2000, 0)
	b.now = func() time.Time { return base }

	b.RegisterPendingJob("anq_a", "Q1", "tires")
	b.RegisterPendingJob("anq_b", "Q2", "fuel")

	jobs := b.PendingJobs()
	if len(jobs) != 2 {
		t.Fatalf("want 2 jobs, got %d", len(jobs))
	}
	if jobs[0].JobID != "anq_a" || jobs[0].ContextTopic != "tires" {
		t.Errorf("first job mismatch: %+v", jobs[0])
	}
	if !jobs[0].StartedAt.Equal(base) {
		t.Errorf("StartedAt should equal injected clock, got %v", jobs[0].StartedAt)
	}

	// Returned slice should be a copy — mutating it must not affect the brain.
	jobs[0].JobID = "mutated"
	if got := b.PendingJobs()[0].JobID; got != "anq_a" {
		t.Errorf("PendingJobs() should return a copy, got %q", got)
	}
}

func TestRegisterPendingJob_DropsEmptyID(t *testing.T) {
	b := New(nil, StaticContext{})
	b.RegisterPendingJob("", "Q", "topic")
	if got := len(b.PendingJobs()); got != 0 {
		t.Errorf("empty id should be dropped, got %d", got)
	}
}

func TestRegisterPendingJob_IdempotentForSameID(t *testing.T) {
	b := New(nil, StaticContext{})
	b.RegisterPendingJob("anq_x", "old", "old-topic")
	b.RegisterPendingJob("anq_x", "new", "new-topic")
	jobs := b.PendingJobs()
	if len(jobs) != 1 {
		t.Fatalf("want 1 job after re-register, got %d", len(jobs))
	}
	if jobs[0].Question != "new" || jobs[0].ContextTopic != "new-topic" {
		t.Errorf("re-register should overwrite fields, got %+v", jobs[0])
	}
}

func TestRegisterPendingJob_FIFOEvictionAtCap(t *testing.T) {
	b := New(nil, StaticContext{})
	for i := 0; i < maxPendingJobs+5; i++ {
		b.RegisterPendingJob(fmtJobID(i), "q", "t")
	}
	jobs := b.PendingJobs()
	if len(jobs) != maxPendingJobs {
		t.Fatalf("want %d (cap), got %d", maxPendingJobs, len(jobs))
	}
	// Oldest IDs should have been evicted; newest should be at the tail.
	if jobs[len(jobs)-1].JobID != fmtJobID(maxPendingJobs+4) {
		t.Errorf("newest entry not at tail: %q", jobs[len(jobs)-1].JobID)
	}
}

func TestClearPendingJob_RemovesByID(t *testing.T) {
	b := New(nil, StaticContext{})
	b.RegisterPendingJob("anq_keep", "k", "x")
	b.RegisterPendingJob("anq_drop", "d", "x")
	b.ClearPendingJob("anq_drop")
	jobs := b.PendingJobs()
	if len(jobs) != 1 || jobs[0].JobID != "anq_keep" {
		t.Errorf("clear failed: %+v", jobs)
	}
}

func TestClearPendingJob_UnknownIDIsNoOp(t *testing.T) {
	b := New(nil, StaticContext{})
	b.RegisterPendingJob("anq_a", "q", "t")
	b.ClearPendingJob("does-not-exist")
	if got := len(b.PendingJobs()); got != 1 {
		t.Errorf("unknown id should be no-op, got %d", got)
	}
}

func TestSnapshotIncludesPendingJobs(t *testing.T) {
	b := New(func() *models.RaceState { return nil }, StaticContext{})
	b.RegisterPendingJob("anq_snap", "what's going on", "race")

	snap := b.Snapshot(DefaultSnapshotOpts())
	if len(snap.PendingJobs) != 1 {
		t.Fatalf("snapshot should include pending jobs, got %d", len(snap.PendingJobs))
	}
	if snap.PendingJobs[0].JobID != "anq_snap" {
		t.Errorf("pending job id mismatch in snapshot")
	}
}

func TestObservationCarriesJobIDAndUrgent(t *testing.T) {
	b := New(nil, StaticContext{})
	dynamicTopic := Topic(AnalystTopicPrefix + "weather")
	b.Write(Observation{
		Topic:   dynamicTopic,
		Agent:   "analyst",
		Summary: "rain in 5m",
		JobID:   "anq_w",
		Urgent:  true,
	})
	snap := b.Snapshot(DefaultSnapshotOpts())
	items := snap.Observations[dynamicTopic]
	if len(items) != 1 {
		t.Fatalf("dynamic analyst topic should appear in default snapshot, got %d", len(items))
	}
	if items[0].JobID != "anq_w" || !items[0].Urgent {
		t.Errorf("JobID/Urgent not preserved: %+v", items[0])
	}
}

func TestSnapshotMarkdownIncludesAnalystTopics(t *testing.T) {
	b := New(nil, StaticContext{})
	b.Write(Observation{
		Topic:   Topic(AnalystTopicPrefix + "tires"),
		Agent:   "analyst",
		Summary: "tires holding up",
	})
	md := b.Snapshot(DefaultSnapshotOpts()).Markdown()
	if !strings.Contains(md, "analyst.tires") {
		t.Errorf("markdown should surface dynamic analyst topic, got:\n%s", md)
	}
	if !strings.Contains(md, "tires holding up") {
		t.Errorf("markdown should include analyst summary, got:\n%s", md)
	}
}

func fmtJobID(i int) string {
	return "anq_" + string(rune('a'+i%26)) + string(rune('0'+i/26%10))
}
