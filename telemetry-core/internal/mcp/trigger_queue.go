// Package mcp hosts the Model Context Protocol server that backs the pi
// agent. The pi agent (a sandboxed Python child process) connects here as
// its only outward channel — it never touches the shell or the filesystem
// directly. All tools the agent calls are registered in tools.go.
package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// TriggerKind enumerates what woke the pi agent.
//
//   - TriggerQuery      — Gemini Live or the dashboard POSTed a question to
//                         /api/analyst/query. Carries Question + ContextTopic.
//   - TriggerLapComplete — the player car just crossed the start/finish line.
//                         Carries the freshly-completed lap number.
//   - TriggerSignificant — the rule engine detected a state change worth
//                         reasoning about (tire cliff, weather, damage, …).
//                         Carries the event Code + Detail.
type TriggerKind string

const (
	TriggerQuery        TriggerKind = "query"
	TriggerLapComplete  TriggerKind = "lap_complete"
	TriggerSignificant  TriggerKind = "significant_event"
)

// Trigger is a single dispatch the pi agent should react to. JobID is set
// only for TriggerQuery so submit_query_answer can route the answer back to
// the right caller.
type Trigger struct {
	Kind         TriggerKind    `json:"kind"`
	JobID        string         `json:"job_id,omitempty"`
	Question     string         `json:"question,omitempty"`
	ContextTopic string         `json:"context_topic,omitempty"`
	Urgent       bool           `json:"urgent,omitempty"`
	Lap          int            `json:"lap,omitempty"`
	EventCode    string         `json:"event_code,omitempty"`
	EventDetail  string         `json:"event_detail,omitempty"`
	Meta         map[string]any `json:"meta,omitempty"`
	EnqueuedAt   time.Time      `json:"enqueued_at"`
}

// pendingQuery tracks an in-flight query for the dedup window and for
// /api/analyst/jobs visibility.
type pendingQuery struct {
	JobID        string
	Question     string
	ContextTopic string
	Urgent       bool
	StartedAt    time.Time
	CompletedAt  time.Time // zero while running
}

const (
	queryDedupWindow = 30 * time.Second
	queryHistoryCap  = 64
)

// TriggerQueue is the producer/consumer queue between the Go server (which
// detects events) and the pi agent (which long-polls pull_next_trigger).
//
// Buffered, lossless for queries (callers get a job_id back), and
// best-effort for lap_complete + significant_event (drops on overflow with a
// log line). Only one consumer is expected; concurrent pulls are safe but
// each Trigger is delivered to exactly one caller.
type TriggerQueue struct {
	ch       chan Trigger
	mu       sync.Mutex
	jobs     []*pendingQuery // ring of recent jobs for dedup + /api/analyst/jobs
	jobIndex map[string]*pendingQuery
}

// NewTriggerQueue returns a queue with the given buffer depth. 256 is a
// reasonable default — long-poll cadence keeps it shallow in practice.
func NewTriggerQueue(buffer int) *TriggerQueue {
	if buffer < 16 {
		buffer = 16
	}
	return &TriggerQueue{
		ch:       make(chan Trigger, buffer),
		jobIndex: map[string]*pendingQuery{},
	}
}

// EnqueueQuery records a synchronous-style question and pushes a TriggerQuery
// onto the queue. Identical (question, context_topic) pairs within the dedup
// window get the existing job_id back without enqueuing a duplicate trigger.
// Returns the job id and whether it was newly created (false = dedup hit).
func (q *TriggerQueue) EnqueueQuery(question, contextTopic string, urgent bool) (string, bool) {
	dedupKey := contextTopic + "|" + question
	q.mu.Lock()
	now := time.Now()
	for _, j := range q.jobs {
		if j.ContextTopic+"|"+j.Question != dedupKey {
			continue
		}
		if j.CompletedAt.IsZero() {
			q.mu.Unlock()
			return j.JobID, false
		}
		if now.Sub(j.CompletedAt) < queryDedupWindow {
			q.mu.Unlock()
			return j.JobID, false
		}
	}
	job := &pendingQuery{
		JobID:        newJobID(),
		Question:     question,
		ContextTopic: contextTopic,
		Urgent:       urgent,
		StartedAt:    now,
	}
	q.jobs = append(q.jobs, job)
	q.jobIndex[job.JobID] = job
	if len(q.jobs) > queryHistoryCap {
		evicted := q.jobs[0]
		q.jobs = q.jobs[1:]
		delete(q.jobIndex, evicted.JobID)
	}
	q.mu.Unlock()

	q.push(Trigger{
		Kind:         TriggerQuery,
		JobID:        job.JobID,
		Question:     question,
		ContextTopic: contextTopic,
		Urgent:       urgent,
		EnqueuedAt:   now,
	})
	return job.JobID, true
}

// EnqueueLapComplete pushes a lap-complete trigger. Best-effort.
func (q *TriggerQueue) EnqueueLapComplete(lap int) {
	q.push(Trigger{
		Kind:       TriggerLapComplete,
		Lap:        lap,
		EnqueuedAt: time.Now(),
	})
}

// EnqueueSignificant pushes a rule-engine event trigger. Best-effort.
func (q *TriggerQueue) EnqueueSignificant(code, detail string, meta map[string]any) {
	q.push(Trigger{
		Kind:        TriggerSignificant,
		EventCode:   code,
		EventDetail: detail,
		Meta:        meta,
		EnqueuedAt:  time.Now(),
	})
}

// push performs the non-blocking send with a debug log on overflow.
func (q *TriggerQueue) push(t Trigger) {
	select {
	case q.ch <- t:
	default:
		// Drop on overflow — the agent fell behind. Queries should never
		// hit this in practice (history cap limits backlog).
	}
}

// Pull blocks for up to timeout, returning the next Trigger or (zero, false)
// if no trigger arrived. ctx cancellation also returns (zero, false).
func (q *TriggerQueue) Pull(ctx context.Context, timeout time.Duration) (Trigger, bool) {
	if timeout <= 0 {
		select {
		case t := <-q.ch:
			return t, true
		case <-ctx.Done():
			return Trigger{}, false
		default:
			return Trigger{}, false
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case t := <-q.ch:
		return t, true
	case <-timer.C:
		return Trigger{}, false
	case <-ctx.Done():
		return Trigger{}, false
	}
}

// CompleteQuery marks a job as completed. Returns the original (question,
// context_topic, urgent) for routing the brain Observation correctly.
func (q *TriggerQueue) CompleteQuery(jobID string) (string, string, bool, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	job, ok := q.jobIndex[jobID]
	if !ok {
		return "", "", false, false
	}
	job.CompletedAt = time.Now()
	return job.Question, job.ContextTopic, job.Urgent, true
}

// PendingJobs returns a copy of currently-running jobs (for /api/analyst/jobs).
func (q *TriggerQueue) PendingJobs() []map[string]any {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]map[string]any, 0, len(q.jobs))
	for _, j := range q.jobs {
		if !j.CompletedAt.IsZero() {
			continue
		}
		out = append(out, map[string]any{
			"job_id":        j.JobID,
			"question":      j.Question,
			"context_topic": j.ContextTopic,
			"urgent":        j.Urgent,
			"started_at":    j.StartedAt,
		})
	}
	return out
}

// Depth returns the number of triggers currently buffered (for diagnostics).
func (q *TriggerQueue) Depth() int { return len(q.ch) }

func newJobID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("anq_%s", hex.EncodeToString(b[:]))
}
