package analyst

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// triggers.go translates Go-side events (lap complete, significant event,
// dashboard query) into ACP session/prompt calls. Each kind has a markdown
// template under <workspace>/prompts/ that Go fills with substitution
// placeholders and dispatches.
//
// Dispatch is fire-and-forget for lap/significant triggers (the agent may
// push insights via MCP), and synchronous for queries (the caller blocks on
// a job_id until SubmitAnswer fires).

// QueryHandler is the brain-side hook for query triggers. Implemented by
// the server wiring so the analyst can hand off the freshly-derived answer
// + topic to brain.EnqueueEvent without depending on the brain package
// directly. answer="" means no useful answer was produced (timeout, error).
type QueryHandler func(jobID, question, topic, answer string, urgent bool)

// OnLapComplete enqueues a lap-complete prompt. Best effort — if the agent
// is busy or down the trigger is dropped.
func (rt *Runtime) OnLapComplete(lap int) {
	if !rt.Ready() {
		return
	}
	runID := newRunID("lap")
	rt.hub.Publish(Activity{
		Kind:    ActivityTrigger,
		Trigger: "lap_complete",
		RunID:   runID,
		Summary: fmt.Sprintf("lap %d complete", lap),
		Meta:    map[string]any{"lap": lap},
	})
	prompt := rt.renderPrompt("lap_complete.md", map[string]string{
		"LAP": fmt.Sprintf("%d", lap),
	})
	if prompt == "" {
		prompt = fmt.Sprintf("Driver just completed lap %d. Review pace, tire state, fuel margin, and any actionable strategy update. Push insights only if novel and actionable (priority 1-3).", lap)
	}
	go rt.dispatch(context.Background(), runID, prompt)
}

// OnSignificantEvent enqueues a rule-engine-driven prompt.
func (rt *Runtime) OnSignificantEvent(code, detail string, meta map[string]any) {
	if !rt.Ready() {
		return
	}
	runID := newRunID("evt")
	rt.hub.Publish(Activity{
		Kind:    ActivityTrigger,
		Trigger: "significant_event",
		RunID:   runID,
		Summary: code + " — " + detail,
		Meta:    meta,
	})
	prompt := rt.renderPrompt("significant_event.md", map[string]string{
		"EVENT_CODE":   code,
		"EVENT_DETAIL": detail,
	})
	if prompt == "" {
		prompt = fmt.Sprintf("Rule engine fired significant event %q (detail: %s). Investigate via MCP data tools and push an insight (priority 1-3) only if there is something actionable the driver hasn't already been told.", code, detail)
	}
	go rt.dispatch(context.Background(), runID, prompt)
}

// Query enqueues a driver-question trigger and returns a job id. The answer
// is delivered to the registered QueryHandler. Dedup is best-effort by
// (topic, question) within 30s.
func (rt *Runtime) Query(ctx context.Context, question, topic string, urgent bool, onAnswer QueryHandler) (string, bool) {
	if !rt.Ready() {
		return "", false
	}
	if topic == "" {
		topic = "general"
	}
	dedupKey := topic + "|" + strings.TrimSpace(strings.ToLower(question))
	rt.pendingMu.Lock()
	for _, p := range rt.pending {
		if p.Topic+"|"+strings.TrimSpace(strings.ToLower(p.Question)) == dedupKey {
			id := p.JobID
			rt.pendingMu.Unlock()
			return id, false
		}
	}
	jobID := newJobID()
	runID := newRunID("qry")
	rt.pending[jobID] = &pendingQuery{
		JobID:     jobID,
		Question:  question,
		Topic:     topic,
		Urgent:    urgent,
		StartedAt: time.Now(),
		RunID:     runID,
	}
	rt.pendingMu.Unlock()

	rt.hub.Publish(Activity{
		Kind:    ActivityTrigger,
		Trigger: "query",
		RunID:   runID,
		Summary: trim(question, 240),
		Meta: map[string]any{
			"job_id":        jobID,
			"context_topic": topic,
			"urgent":        urgent,
		},
	})
	prompt := rt.renderPrompt("query.md", map[string]string{
		"QUESTION":      question,
		"CONTEXT_TOPIC": topic,
		"URGENT":        boolStr(urgent),
	})
	if prompt == "" {
		prompt = fmt.Sprintf("Driver query (topic=%s, urgent=%v): %s\n\nReturn a conversational answer, ≤3 sentences. Use MCP data tools as needed.", topic, urgent, question)
	}

	go func() {
		answer := rt.dispatchQuery(context.Background(), runID, prompt)
		rt.pendingMu.Lock()
		delete(rt.pending, jobID)
		rt.pendingMu.Unlock()
		if onAnswer != nil {
			onAnswer(jobID, question, topic, answer, urgent)
		}
		rt.hub.Publish(Activity{
			Kind:    ActivityAnswer,
			RunID:   runID,
			Summary: trim(answer, 240),
			Meta: map[string]any{
				"job_id":        jobID,
				"context_topic": topic,
				"urgent":        urgent,
			},
		})
	}()
	return jobID, true
}

// dispatch is fire-and-forget — the agent's notifications already land in
// the hub, so no answer aggregation is needed. Serialises through promptMu
// so concurrent triggers don't interleave in the same ACP session, and
// stamps the active run_id onto every session/update notification opencode
// emits during the prompt.
func (rt *Runtime) dispatch(ctx context.Context, runID, prompt string) {
	sessionID := rt.client.SessionID()
	if sessionID == "" {
		return
	}
	rt.promptMu.Lock()
	rid := runID
	rt.activeRunID.Store(&rid)
	defer func() {
		rt.activeRunID.Store(nil)
		rt.promptMu.Unlock()
	}()
	if _, err := rt.client.SessionPrompt(ctx, sessionID, prompt); err != nil {
		rt.log.Warn().Err(err).Str("run_id", runID).Msg("analyst: session/prompt failed")
		rt.hub.Publish(Activity{
			Kind:    ActivityError,
			RunID:   runID,
			Summary: err.Error(),
		})
	}
}

// dispatchQuery wraps dispatch with an answer aggregator. We capture the
// streamed agent_message_chunk text via a temporary subscription and
// concatenate it as the final answer.
func (rt *Runtime) dispatchQuery(ctx context.Context, runID, prompt string) string {
	sessionID := rt.client.SessionID()
	if sessionID == "" {
		return ""
	}
	rt.promptMu.Lock()
	rid := runID
	rt.activeRunID.Store(&rid)
	defer func() {
		rt.activeRunID.Store(nil)
		rt.promptMu.Unlock()
	}()
	collector := &chunkCollector{runID: runID}
	sub, cancel := rt.hub.Subscribe(64)
	defer cancel()
	go collector.run(sub)
	if _, err := rt.client.SessionPrompt(ctx, sessionID, prompt); err != nil {
		rt.log.Warn().Err(err).Str("run_id", runID).Msg("analyst: query prompt failed")
		rt.hub.Publish(Activity{
			Kind:    ActivityError,
			RunID:   runID,
			Summary: err.Error(),
		})
		return ""
	}
	// session/prompt has returned — the agent has finished the turn, so
	// all chunks for this run have already been observed. Stop the
	// collector and return what it accumulated.
	collector.stop()
	return collector.text()
}

type chunkCollector struct {
	runID string

	mu      sync.Mutex
	chunks  []string
	stopped bool
}

func (c *chunkCollector) run(sub <-chan Activity) {
	for a := range sub {
		c.mu.Lock()
		if c.stopped {
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()
		// We can't perfectly filter by runID because session/update doesn't
		// carry it — the run grouping is our convention, not opencode's.
		// In practice triggers are serialized through one session so the
		// chunks between Trigger publish and session/prompt return are all
		// ours. The chunk collector simply takes everything until stop().
		if a.Kind == ActivityMessageChunk && a.Summary != "" {
			c.mu.Lock()
			c.chunks = append(c.chunks, a.Summary)
			c.mu.Unlock()
		}
	}
}

func (c *chunkCollector) stop() {
	c.mu.Lock()
	c.stopped = true
	c.mu.Unlock()
}

func (c *chunkCollector) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.chunks, "")
}

// renderPrompt loads a template from <workspace>/prompts/<name> and
// performs a literal {{KEY}} substitution. Missing file returns "".
func (rt *Runtime) renderPrompt(name string, vars map[string]string) string {
	path := filepath.Join(rt.opts.Workspace, "prompts", name)
	body, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	out := string(body)
	for k, v := range vars {
		out = strings.ReplaceAll(out, "{{"+k+"}}", v)
	}
	return out
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func newRunID(prefix string) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}

func newJobID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "anq_" + hex.EncodeToString(b[:])
}
