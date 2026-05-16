package brain

import (
	"sync"
	"time"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
)

// Bounded sizes — the brain stays small so LLM context cost stays predictable.
const (
	maxObservationsPerTopic = 5
	maxEvents               = 30
	maxRecentSpeech         = 12
	maxDriverEvents         = 8
	maxPendingJobs          = 16
)

// StaticContext is the slowly-changing prose loaded once at startup
// (driver profile, track setup, soul, user preferences, past learnings).
type StaticContext struct {
	Soul          string
	User          string
	DriverProfile string
	TrackSetup    string
	PastLearnings string
}

// TranscriptHook is the minimal interface the brain needs to record event
// lifecycle into the per-session transcript. Defined here so the brain
// package doesn't import transcript (the hub satisfies it structurally).
type TranscriptHook interface {
	AppendEventLifecycle(kind, actor, text string, meta map[string]any) bool
}

// RaceBrain is the shared blackboard. Multiple agents write; CommsGate reads
// via Snapshot. All public methods are safe for concurrent use.
type RaceBrain struct {
	mu sync.RWMutex

	// L1 — hot facts (live atomic cache pointer, dereferenced at snapshot time)
	stateLoader func() *models.RaceState

	// transcriptHook receives event lifecycle records (dispatched, delivered,
	// dropped). Optional — nil-safe.
	transcriptHook TranscriptHook

	// L2 — observations (per-topic ring of last N)
	observations map[Topic][]Observation

	// L3 — hypotheses (latest-wins per topic)
	hypotheses map[Topic]Observation

	// L4 — decisions (recent radio we issued)
	speech []SpeechRecord

	// L5 — driver state (queries, complaints, acks)
	driverEvents []DriverEvent

	// L6 — static context (loaded once)
	static StaticContext

	// L7 — race events
	events []RaceEvent

	// pendingJobs — in-flight async analyst queries, oldest evicted at cap.
	pendingJobs []PendingJob

	// eventQueue — Interrupts Bus. Priority-sorted queue drained by the Live
	// agent dispatcher. See events.go and event_bus.go.
	eventQueue []Event

	// driverUtterances — ring of recent transcribed driver speech (Gemini
	// Live InputTranscription + PTT path). See driver_utterances.go.
	driverUtterances []DriverUtterance

	// clock — overridable for tests
	now func() time.Time
}

// New creates an empty brain. stateLoader may be nil; if so, hot facts are
// omitted from snapshots.
func New(stateLoader func() *models.RaceState, static StaticContext) *RaceBrain {
	return &RaceBrain{
		stateLoader:  stateLoader,
		observations: make(map[Topic][]Observation),
		hypotheses:   make(map[Topic]Observation),
		static:       static,
		now:          time.Now,
	}
}

// Write records an observation. If Hypothesis is true the observation goes to
// L3 (latest-wins per topic); otherwise it joins the L2 ring buffer for that
// topic. Empty Topic is silently dropped.
func (b *RaceBrain) Write(o Observation) {
	if o.Topic == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	if o.At.IsZero() {
		o.At = b.now()
	}

	if o.Hypothesis {
		b.hypotheses[o.Topic] = o
		return
	}

	bucket := b.observations[o.Topic]
	bucket = append(bucket, o)
	if len(bucket) > maxObservationsPerTopic {
		bucket = bucket[len(bucket)-maxObservationsPerTopic:]
	}
	b.observations[o.Topic] = bucket
}

// RecordSpeech logs an outgoing radio message into L4.
func (b *RaceBrain) RecordSpeech(text string, priority int) {
	if text == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.speech = appendBounded(b.speech, SpeechRecord{
		At:       b.now(),
		Text:     text,
		Priority: priority,
	}, maxRecentSpeech)
}

// RecordDriver logs an incoming driver utterance into L5.
func (b *RaceBrain) RecordDriver(text, kind string) {
	if text == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.driverEvents = appendBounded(b.driverEvents, DriverEvent{
		At:   b.now(),
		Text: text,
		Kind: kind,
	}, maxDriverEvents)
}

// RecordEvent logs a discrete race event into L7.
func (b *RaceBrain) RecordEvent(e RaceEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if e.At.IsZero() {
		e.At = b.now()
	}
	b.events = appendBounded(b.events, e, maxEvents)
}

// RegisterPendingJob records an in-flight analyst query so callers can see
// what the analyst is currently working on. If the slot count exceeds
// maxPendingJobs, the oldest entry is evicted FIFO. Empty jobID is dropped.
func (b *RaceBrain) RegisterPendingJob(jobID, question, contextTopic string) {
	if jobID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	// Replace if same jobID is already tracked (idempotent).
	for i := range b.pendingJobs {
		if b.pendingJobs[i].JobID == jobID {
			b.pendingJobs[i].Question = question
			b.pendingJobs[i].ContextTopic = contextTopic
			b.pendingJobs[i].StartedAt = b.now()
			return
		}
	}

	b.pendingJobs = append(b.pendingJobs, PendingJob{
		JobID:        jobID,
		Question:     question,
		ContextTopic: contextTopic,
		StartedAt:    b.now(),
	})
	if len(b.pendingJobs) > maxPendingJobs {
		b.pendingJobs = b.pendingJobs[len(b.pendingJobs)-maxPendingJobs:]
	}
}

// ClearPendingJob removes a pending analyst job by id. No-op if not present.
func (b *RaceBrain) ClearPendingJob(jobID string) {
	if jobID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.pendingJobs[:0]
	for _, p := range b.pendingJobs {
		if p.JobID == jobID {
			continue
		}
		out = append(out, p)
	}
	b.pendingJobs = out
}

// PendingJobs returns a snapshot copy of the current in-flight analyst jobs.
// Order matches insertion order (oldest first).
func (b *RaceBrain) PendingJobs() []PendingJob {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.pendingJobs) == 0 {
		return nil
	}
	out := make([]PendingJob, len(b.pendingJobs))
	copy(out, b.pendingJobs)
	return out
}

// Static returns a copy of the static context.
func (b *RaceBrain) Static() StaticContext {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.static
}

// SetStatic replaces the static context (e.g., after a workspace reload).
func (b *RaceBrain) SetStatic(s StaticContext) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.static = s
}

// SetTranscriptHook wires the per-session transcript writer. Pass nil to
// disable lifecycle logging. Safe to call after construction.
func (b *RaceBrain) SetTranscriptHook(h TranscriptHook) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.transcriptHook = h
}

// transcriptHookSnapshot returns the current hook under read lock so
// callers can drop the lock before invoking it (the hook may take a
// channel send that we don't want to do under a write lock).
func (b *RaceBrain) transcriptHookSnapshot() TranscriptHook {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.transcriptHook
}

func appendBounded[T any](s []T, v T, max int) []T {
	s = append(s, v)
	if len(s) > max {
		s = s[len(s)-max:]
	}
	return s
}
