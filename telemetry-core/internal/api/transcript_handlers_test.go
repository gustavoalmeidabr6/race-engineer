package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/transcript"
)

func newTranscriptApp(t *testing.T) (*fiber.App, *transcript.Hub, func()) {
	t.Helper()
	dir := t.TempDir()
	cfg := transcript.DefaultConfig()
	cfg.Dir = dir
	cfg.RingSize = 16
	cfg.PromptLines = 5
	cfg.ToolLimit = 25
	cfg.ChannelSize = 16
	uid := uint64(0xfeed)
	hub, err := transcript.New(cfg, func() uint64 { return uid })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		hub.Run(ctx)
		close(done)
	}()

	deps := &Deps{Transcript: hub}
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Post("/api/transcript", transcriptIngestHandler(deps))
	app.Get("/api/transcript", transcriptListHandler(deps))
	app.Get("/api/transcript/sessions", transcriptSessionsHandler(deps))

	teardown := func() {
		cancel()
		<-done
	}
	return app, hub, teardown
}

func TestTranscript_IngestPersistsEvent(t *testing.T) {
	app, hub, teardown := newTranscriptApp(t)
	defer teardown()

	body, _ := json.Marshal(map[string]any{
		"kind":  "engineer_speech",
		"actor": "engineer",
		"text":  "Box, box.",
		"meta":  map[string]any{"priority": 5},
	})
	code, out := doReq(t, app, "POST", "/api/transcript", body)
	if code != fiber.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", code, out)
	}

	if err := hub.Sync(timeoutCtx(t, time.Second)); err != nil {
		t.Fatal(err)
	}

	got, err := hub.Load("current", 0, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Text != "Box, box." {
		t.Errorf("hub didn't persist event: %+v", got)
	}
}

func TestTranscript_IngestRejectsUnknownKind(t *testing.T) {
	app, _, teardown := newTranscriptApp(t)
	defer teardown()

	body, _ := json.Marshal(map[string]any{
		"kind":  "totally_made_up",
		"actor": "x",
		"text":  "y",
	})
	code, _ := doReq(t, app, "POST", "/api/transcript", body)
	if code != fiber.StatusBadRequest {
		t.Errorf("expected 400 for unknown kind, got %d", code)
	}
}

func TestTranscript_ListReturnsOnlyRequestedKinds(t *testing.T) {
	app, hub, teardown := newTranscriptApp(t)
	defer teardown()

	hub.Append(transcript.Event{Kind: transcript.KindEngineerSpeech, Actor: "engineer", Text: "speak1"})
	hub.Append(transcript.Event{Kind: transcript.KindToolCall, Actor: "live_agent", Text: "get_race_state"})
	hub.Append(transcript.Event{Kind: transcript.KindEngineerSpeech, Actor: "engineer", Text: "speak2"})
	if err := hub.Sync(timeoutCtx(t, time.Second)); err != nil {
		t.Fatal(err)
	}

	code, out := doReq(t, app, "GET", "/api/transcript?kinds=engineer_speech&limit=10", nil)
	if code != fiber.StatusOK {
		t.Fatalf("list: %d", code)
	}
	var resp struct {
		SessionID string              `json:"session_id"`
		Events    []transcript.Event `json:"events"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Events) != 2 {
		t.Fatalf("expected 2 engineer_speech events, got %d", len(resp.Events))
	}
	for _, e := range resp.Events {
		if e.Kind != transcript.KindEngineerSpeech {
			t.Errorf("filter leak: %s", e.Kind)
		}
	}
	if !strings.HasPrefix(resp.SessionID, "sess_") {
		t.Errorf("session id wrong: %s", resp.SessionID)
	}
}

func TestTranscript_ListRejectsBadKind(t *testing.T) {
	app, _, teardown := newTranscriptApp(t)
	defer teardown()
	code, _ := doReq(t, app, "GET", "/api/transcript?kinds=fakey_kind", nil)
	if code != fiber.StatusBadRequest {
		t.Errorf("expected 400, got %d", code)
	}
}

func TestTranscript_SessionsListShowsCurrent(t *testing.T) {
	app, hub, teardown := newTranscriptApp(t)
	defer teardown()

	hub.Append(transcript.Event{Kind: transcript.KindEngineerSpeech, Actor: "engineer", Text: "x"})
	if err := hub.Sync(timeoutCtx(t, time.Second)); err != nil {
		t.Fatal(err)
	}

	code, out := doReq(t, app, "GET", "/api/transcript/sessions", nil)
	if code != fiber.StatusOK {
		t.Fatalf("sessions: %d", code)
	}
	var resp struct {
		Current  string                      `json:"current"`
		Sessions []transcript.SessionMeta    `json:"sessions"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Current == "" {
		t.Errorf("expected current session id, got empty")
	}
	if len(resp.Sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(resp.Sessions))
	}
	if resp.Sessions[0].Lines != 1 {
		t.Errorf("expected 1 line in session, got %d", resp.Sessions[0].Lines)
	}
}

func TestTranscript_NoHubReturns503(t *testing.T) {
	deps := &Deps{Transcript: nil}
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Post("/api/transcript", transcriptIngestHandler(deps))
	app.Get("/api/transcript", transcriptListHandler(deps))
	app.Get("/api/transcript/sessions", transcriptSessionsHandler(deps))

	body, _ := json.Marshal(map[string]any{"kind": "engineer_speech", "actor": "e"})
	if code, _ := doReq(t, app, "POST", "/api/transcript", body); code != fiber.StatusServiceUnavailable {
		t.Errorf("ingest expected 503, got %d", code)
	}
	if code, _ := doReq(t, app, "GET", "/api/transcript", nil); code != fiber.StatusServiceUnavailable {
		t.Errorf("list expected 503, got %d", code)
	}
	if code, _ := doReq(t, app, "GET", "/api/transcript/sessions", nil); code != fiber.StatusServiceUnavailable {
		t.Errorf("sessions expected 503, got %d", code)
	}
}

// timeoutCtx returns a context with the given timeout. Helper to keep
// test bodies readable.
func timeoutCtx(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}
