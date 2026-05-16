package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/brain"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/intelligence"
	mcpx "github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/mcp"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
)

// --- minimal LLMProvider mock for api tests ---

type stubProvider struct {
	mu        sync.Mutex
	available bool
	textResp  string
	textErr   error
	calls     int
}

func (s *stubProvider) Available() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.available
}

func (s *stubProvider) Complete(_ context.Context, _, _ string, _ intelligence.ProviderConfig) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.textResp, s.textErr
}

func (s *stubProvider) CompleteWithTools(_ context.Context, _, _ string, _ intelligence.ProviderConfig, _ []intelligence.ToolDef) (*intelligence.GenerateResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	// Mirror Complete's textResp/textErr so tests that script answers via the
	// stub's text fields keep working when callers switch from Complete to
	// CompleteWithTools (e.g., AnswerQuestion now exposes interrupt_engineer
	// when the bus is wired).
	return &intelligence.GenerateResult{Text: s.textResp}, s.textErr
}

func (s *stubProvider) Close() {}

func (s *stubProvider) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// --- helpers ---

func makeBrainWithData(t *testing.T) *brain.RaceBrain {
	t.Helper()
	state := &models.RaceState{
		Position:       3,
		CurrentLap:     12,
		TotalLaps:      50,
		ActualCompound: 17,
		TyresAgeLaps:   8,
		TrackID:        7,
		SessionType:    10,
	}
	b := brain.New(
		func() *models.RaceState { return state },
		brain.StaticContext{
			Soul: "Calm under pressure.",
			User: "No filler.",
		},
	)
	b.Write(brain.Observation{
		Topic: brain.TopicTireDegradation, Agent: "test-agent",
		Summary: "RR up 2%/lap", Confidence: 0.7,
	})
	b.RecordEvent(brain.RaceEvent{Code: "DRSE"})
	b.RecordSpeech("DRS available", 3)
	b.RecordDriver("how are tires", "query")
	return b
}

func newAppWithDeps(deps *Deps) *fiber.App {
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Get("/api/brain/snapshot", brainSnapshotHandler(deps))
	app.Post("/api/analyst/query", analystQueryHandler(deps))
	app.Get("/api/analyst/jobs", analystJobsHandler(deps))
	return app
}

func doReq(t *testing.T, app *fiber.App, method, path string, body []byte) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := app.Test(req, 5000)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// waitFor polls until cond is true or timeout. Returns whether it succeeded.
func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

// --- /api/brain/snapshot ---

func TestBrainSnapshot_Markdown(t *testing.T) {
	b := makeBrainWithData(t)
	app := newAppWithDeps(&Deps{Brain: b})

	code, body := doReq(t, app, "GET", "/api/brain/snapshot", nil)
	if code != 200 {
		t.Fatalf("status %d, body %s", code, body)
	}
	md := string(body)
	for _, want := range []string{
		"## Live Telemetry",
		"Silverstone",
		"Active Observations",
		"tires.degradation",
		"RR up 2%/lap",
		"DRSE",
		"Recent Radio",
		"DRS available",
		"Driver State",
		"how are tires",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q\n--- body ---\n%s", want, md)
		}
	}
}

func TestBrainSnapshot_JSON(t *testing.T) {
	b := makeBrainWithData(t)
	app := newAppWithDeps(&Deps{Brain: b})

	code, body := doReq(t, app, "GET", "/api/brain/snapshot?format=json", nil)
	if code != 200 {
		t.Fatalf("status %d, body %s", code, body)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("invalid json: %v\nbody: %s", err, body)
	}
	for _, key := range []string{"hot_facts", "observations", "events", "speech", "driver_events", "static"} {
		if _, ok := payload[key]; !ok {
			t.Errorf("json missing key %q", key)
		}
	}
	static, ok := payload["static"].(map[string]any)
	if !ok {
		t.Fatalf("static block is not an object")
	}
	if static["soul"] != "Calm under pressure." {
		t.Errorf("static.soul mismatch: %v", static["soul"])
	}
}

func TestBrainSnapshot_NoBrain503(t *testing.T) {
	app := newAppWithDeps(&Deps{Brain: nil})
	code, _ := doReq(t, app, "GET", "/api/brain/snapshot", nil)
	if code != 503 {
		t.Errorf("expected 503 when brain missing, got %d", code)
	}
}

// --- /api/analyst/query (now backed by the pi-agent trigger queue) ---

func newTestPiAgent() *mcpx.TriggerQueue {
	return mcpx.NewTriggerQueue(16)
}

func TestAnalystQuery_EnqueuesTriggerAndRegistersJob(t *testing.T) {
	q := newTestPiAgent()
	b := brain.New(func() *models.RaceState { return nil }, brain.StaticContext{})
	app := newAppWithDeps(&Deps{PiAgent: q, Brain: b})

	body := []byte(`{"question":"how are my tires?","context_topic":"tire_strategy"}`)
	code, out := doReq(t, app, "POST", "/api/analyst/query", body)
	if code != 202 {
		t.Fatalf("expected 202 accepted, got %d body=%s", code, out)
	}
	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	jobID, _ := resp["job_id"].(string)
	if !strings.HasPrefix(jobID, "anq_") {
		t.Errorf("job_id should be anq_-prefixed, got %v", resp["job_id"])
	}
	// One trigger should be sitting on the queue with the same job_id.
	trig, ok := q.Pull(context.Background(), 50*time.Millisecond)
	if !ok {
		t.Fatal("expected a query trigger on the queue")
	}
	if trig.Kind != mcpx.TriggerQuery || trig.JobID != jobID {
		t.Errorf("unexpected trigger %+v", trig)
	}
	// Brain should have a pending job entry mirroring the trigger.
	found := false
	for _, j := range b.PendingJobs() {
		if j.JobID == jobID {
			found = true
		}
	}
	if !found {
		t.Errorf("expected brain pending-job entry for %s", jobID)
	}
}

func TestAnalystQuery_DedupReturnsSameJobID(t *testing.T) {
	q := newTestPiAgent()
	app := newAppWithDeps(&Deps{PiAgent: q})
	body := []byte(`{"question":"identical","context_topic":"dedup"}`)
	code1, out1 := doReq(t, app, "POST", "/api/analyst/query", body)
	code2, out2 := doReq(t, app, "POST", "/api/analyst/query", body)
	if code1 != 202 || code2 != 202 {
		t.Fatalf("both calls should be 202, got %d / %d", code1, code2)
	}
	var r1, r2 map[string]any
	json.Unmarshal(out1, &r1)
	json.Unmarshal(out2, &r2)
	if r1["job_id"] != r2["job_id"] {
		t.Errorf("dedup expected same job_id; got %v vs %v", r1["job_id"], r2["job_id"])
	}
}

func TestAnalystQuery_DefaultsContextTopicToGeneral(t *testing.T) {
	q := newTestPiAgent()
	app := newAppWithDeps(&Deps{PiAgent: q})
	code, _ := doReq(t, app, "POST", "/api/analyst/query", []byte(`{"question":"x"}`))
	if code != 202 {
		t.Fatalf("expected 202, got %d", code)
	}
	trig, ok := q.Pull(context.Background(), 50*time.Millisecond)
	if !ok {
		t.Fatal("expected trigger on queue")
	}
	if trig.ContextTopic != "general" {
		t.Errorf("expected context_topic=general, got %q", trig.ContextTopic)
	}
}

func TestAnalystQuery_EmptyQuestion(t *testing.T) {
	app := newAppWithDeps(&Deps{PiAgent: newTestPiAgent()})
	code, _ := doReq(t, app, "POST", "/api/analyst/query", []byte(`{"question":""}`))
	if code != 400 {
		t.Errorf("empty question should be 400, got %d", code)
	}
}

func TestAnalystQuery_InvalidJSON(t *testing.T) {
	app := newAppWithDeps(&Deps{PiAgent: newTestPiAgent()})
	code, _ := doReq(t, app, "POST", "/api/analyst/query", []byte(`not json`))
	if code != 400 {
		t.Errorf("invalid JSON should be 400, got %d", code)
	}
}

func TestAnalystQuery_NoQueue503(t *testing.T) {
	app := newAppWithDeps(&Deps{PiAgent: nil})
	code, _ := doReq(t, app, "POST", "/api/analyst/query", []byte(`{"question":"x"}`))
	if code != 503 {
		t.Errorf("missing queue should be 503, got %d", code)
	}
}

// --- /api/analyst/jobs ---

func TestAnalystJobs_EmptyWhenNoBrain(t *testing.T) {
	app := newAppWithDeps(&Deps{Brain: nil})
	code, body := doReq(t, app, "GET", "/api/analyst/jobs", nil)
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}
	var jobs []any
	if err := json.Unmarshal(body, &jobs); err != nil {
		t.Fatalf("expected JSON array, got %s", body)
	}
	if len(jobs) != 0 {
		t.Errorf("expected empty array, got %d entries", len(jobs))
	}
}

func TestAnalystJobs_ListsPendingJob(t *testing.T) {
	b := brain.New(func() *models.RaceState { return nil }, brain.StaticContext{})
	b.RegisterPendingJob("anq_test123", "what is going on?", "general")
	app := newAppWithDeps(&Deps{Brain: b})

	code, body := doReq(t, app, "GET", "/api/analyst/jobs", nil)
	if code != 200 {
		t.Fatalf("status %d body %s", code, body)
	}
	var jobs []map[string]any
	if err := json.Unmarshal(body, &jobs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	if jobs[0]["job_id"] != "anq_test123" {
		t.Errorf("job_id mismatch: %v", jobs[0]["job_id"])
	}
	if jobs[0]["question"] != "what is going on?" {
		t.Errorf("question mismatch: %v", jobs[0]["question"])
	}
	if jobs[0]["context_topic"] != "general" {
		t.Errorf("context_topic mismatch: %v", jobs[0]["context_topic"])
	}
}

func TestAnalystJobs_ClearedAfterCompletion(t *testing.T) {
	b := brain.New(func() *models.RaceState { return nil }, brain.StaticContext{})
	b.RegisterPendingJob("anq_clear", "q", "topic")
	b.ClearPendingJob("anq_clear")
	app := newAppWithDeps(&Deps{Brain: b})

	code, body := doReq(t, app, "GET", "/api/analyst/jobs", nil)
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	var jobs []any
	json.Unmarshal(body, &jobs)
	if len(jobs) != 0 {
		t.Errorf("expected empty after clear, got %d", len(jobs))
	}
}
