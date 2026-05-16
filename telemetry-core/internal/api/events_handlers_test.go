package api

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/brain"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/config"
)

// newEventApp wires only the /api/events/* routes — minimum surface so
// we can assert the contract without spinning up the whole server.
func newEventApp(t *testing.T, deps *Deps) *fiber.App {
	t.Helper()
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Post("/api/events/interrupt", eventInterruptHandler(deps))
	app.Get("/api/events/next", eventNextHandler(deps))
	app.Get("/api/events/peek", eventPeekHandler(deps))
	app.Post("/api/events/spoken", eventSpokenHandler(deps))
	app.Post("/api/events/ack", eventAckHandler(deps))
	return app
}

// makeEventDeps builds a Deps with a fresh brain and a config whose TalkLevel
// can be tuned per test.
func makeEventDeps(talkLevel int32) *Deps {
	cfg := &config.Config{}
	// Config has atomic.Int32 fields; the zero value is fine but we want to
	// set TalkLevel explicitly.
	var tl atomic.Int32
	tl.Store(talkLevel)
	cfg.TalkLevel = tl
	b := brain.New(nil, brain.StaticContext{})
	return &Deps{
		Cfg:   cfg,
		Brain: b,
	}
}

func TestEventInterrupt_Accepts(t *testing.T) {
	deps := makeEventDeps(10)
	app := newEventApp(t, deps)
	body, _ := json.Marshal(map[string]any{
		"type":      "threat_overtake",
		"priority":  3,
		"summary":   "Verstappen P+1, gap 1.4s",
		"reasoning": "S2 -0.3 vs us, will close to DRS by T13",
		"debug_data": map[string]any{
			"gap_s": 1.4, "lap_delta_s": -0.3,
		},
		"source": "test",
	})
	code, out := doReq(t, app, "POST", "/api/events/interrupt", body)
	if code != fiber.StatusAccepted {
		t.Fatalf("expected 202, got %d body=%s", code, out)
	}
	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	if id, _ := resp["event_id"].(string); !strings.HasPrefix(id, "evt_") {
		t.Errorf("expected event_id evt_* prefix, got %v", resp["event_id"])
	}
}

func TestEventInterrupt_RejectsLengthViolation(t *testing.T) {
	deps := makeEventDeps(10)
	app := newEventApp(t, deps)
	body, _ := json.Marshal(map[string]any{
		"type":     "info",
		"priority": 1,
		"summary":  strings.Repeat("a", brain.MaxEventSummaryChars+1),
		"source":   "test",
	})
	code, _ := doReq(t, app, "POST", "/api/events/interrupt", body)
	if code != fiber.StatusBadRequest {
		t.Errorf("expected 400 for oversized summary, got %d", code)
	}
}

func TestEventInterrupt_TalkLevelDrops(t *testing.T) {
	deps := makeEventDeps(2) // TalkLevel 2 → only P5 events allowed
	app := newEventApp(t, deps)
	body, _ := json.Marshal(map[string]any{
		"type":     "lap_summary",
		"priority": 2,
		"summary":  "Lap 14 done.",
		"source":   "test",
	})
	code, _ := doReq(t, app, "POST", "/api/events/interrupt", body)
	if code != fiber.StatusTooManyRequests {
		t.Errorf("expected 429 (talk_level_filter), got %d", code)
	}
}

func TestEventInterrupt_DedupReturns429(t *testing.T) {
	deps := makeEventDeps(10)
	app := newEventApp(t, deps)
	body, _ := json.Marshal(map[string]any{
		"type":      "lap_summary",
		"priority":  2,
		"summary":   "Lap 14 done.",
		"dedup_key": "lap_summary:14",
		"source":    "test",
	})
	if code, _ := doReq(t, app, "POST", "/api/events/interrupt", body); code != fiber.StatusAccepted {
		t.Fatalf("first insert: expected 202, got %d", code)
	}
	if code, _ := doReq(t, app, "POST", "/api/events/interrupt", body); code != fiber.StatusTooManyRequests {
		t.Errorf("second insert: expected 429 (deduped), got %d", code)
	}
}

func TestEventNext_PriorityFiltering(t *testing.T) {
	deps := makeEventDeps(10)
	app := newEventApp(t, deps)

	// Push a P2 and a P5 event.
	for _, payload := range []map[string]any{
		{"type": "lap_summary", "priority": 2, "summary": "lap 14"},
		{"type": "box_now", "priority": 5, "summary": "box this lap"},
	} {
		body, _ := json.Marshal(payload)
		if code, _ := doReq(t, app, "POST", "/api/events/interrupt", body); code != fiber.StatusAccepted {
			t.Fatalf("setup insert: %d", code)
		}
	}

	// Fast lane (min=4) should lease box_now first.
	code, out := doReq(t, app, "GET", "/api/events/next?min_priority=4", nil)
	if code != fiber.StatusOK {
		t.Fatalf("fast-lane next: %d", code)
	}
	var fast map[string]any
	_ = json.Unmarshal(out, &fast)
	if fast["type"] != "box_now" {
		t.Errorf("expected box_now from fast lane, got %v", fast["type"])
	}
	boxID, _ := fast["id"].(string)
	if boxID == "" {
		t.Fatalf("missing id in lease response")
	}

	// While box_now is in_flight the bus is single-flight: idle lane gets {}.
	code, out = doReq(t, app, "GET", "/api/events/next?max_priority=3", nil)
	if code != fiber.StatusOK {
		t.Fatalf("idle-lane next while in_flight: %d", code)
	}
	var blocked map[string]any
	_ = json.Unmarshal(out, &blocked)
	if len(blocked) != 0 {
		t.Errorf("expected {} idle while urgent in_flight, got %v", blocked)
	}

	// Ack box_now as delivered → lease closes.
	ack, _ := json.Marshal(map[string]any{
		"event_id":    boxID,
		"outcome":     "delivered",
		"spoken_text": "Box, box.",
	})
	if code, _ := doReq(t, app, "POST", "/api/events/ack", ack); code != fiber.StatusOK {
		t.Fatalf("ack: %d", code)
	}

	// Fast lane should now be empty → returns {} (200).
	code, out = doReq(t, app, "GET", "/api/events/next?min_priority=4", nil)
	if code != fiber.StatusOK {
		t.Fatalf("expected 200 with empty body, got %d", code)
	}
	var empty map[string]any
	_ = json.Unmarshal(out, &empty)
	if len(empty) != 0 {
		t.Errorf("expected {} body, got %v", empty)
	}

	// Idle lane (max=3) should now lease lap_summary.
	code, out = doReq(t, app, "GET", "/api/events/next?max_priority=3", nil)
	var idle map[string]any
	_ = json.Unmarshal(out, &idle)
	if idle["type"] != "lap_summary" {
		t.Errorf("expected lap_summary from idle lane, got %v", idle["type"])
	}
}

func TestEventSpoken_RecordsToBrain(t *testing.T) {
	deps := makeEventDeps(10)
	app := newEventApp(t, deps)

	body, _ := json.Marshal(map[string]any{
		"event_id":    "evt_abcd1234",
		"spoken_text": "Verstappen one back, give it up.",
		"priority":    3,
	})
	code, _ := doReq(t, app, "POST", "/api/events/spoken", body)
	if code != fiber.StatusOK {
		t.Fatalf("spoken: expected 200, got %d", code)
	}

	// The brain's snapshot speech buffer should contain it.
	snap := deps.Brain.Snapshot(brain.DefaultSnapshotOpts())
	if len(snap.Speech) != 1 {
		t.Fatalf("expected 1 speech entry, got %d", len(snap.Speech))
	}
	if !strings.Contains(snap.Speech[0].Text, "Verstappen one back") {
		t.Errorf("speech text mismatch: %q", snap.Speech[0].Text)
	}
}

func TestEventAck_DeliveredRemovesAndRecords(t *testing.T) {
	deps := makeEventDeps(10)
	app := newEventApp(t, deps)

	body, _ := json.Marshal(map[string]any{
		"type":     "box_now",
		"priority": 5,
		"summary":  "Box this lap",
	})
	doReq(t, app, "POST", "/api/events/interrupt", body)

	_, leaseOut := doReq(t, app, "GET", "/api/events/next?min_priority=4", nil)
	var leased map[string]any
	_ = json.Unmarshal(leaseOut, &leased)
	id, _ := leased["id"].(string)
	if id == "" {
		t.Fatalf("lease missing id: %s", leaseOut)
	}

	ack, _ := json.Marshal(map[string]any{
		"event_id":    id,
		"outcome":     "delivered",
		"spoken_text": "Box, box.",
	})
	code, _ := doReq(t, app, "POST", "/api/events/ack", ack)
	if code != fiber.StatusOK {
		t.Fatalf("ack: %d", code)
	}

	if got := len(deps.Brain.PeekEvents()); got != 0 {
		t.Errorf("expected queue empty after delivered ack, got %d", got)
	}
	snap := deps.Brain.Snapshot(brain.DefaultSnapshotOpts())
	if len(snap.Speech) == 0 || !strings.Contains(snap.Speech[0].Text, "Box, box") {
		t.Errorf("delivered ack should record speech, got: %v", snap.Speech)
	}
}

func TestEventAck_InterruptedRequeues(t *testing.T) {
	deps := makeEventDeps(10)
	app := newEventApp(t, deps)

	body, _ := json.Marshal(map[string]any{
		"type":     "box_now",
		"priority": 5,
		"summary":  "Box this lap",
	})
	doReq(t, app, "POST", "/api/events/interrupt", body)

	_, leaseOut := doReq(t, app, "GET", "/api/events/next?min_priority=4", nil)
	var leased map[string]any
	_ = json.Unmarshal(leaseOut, &leased)
	id, _ := leased["id"].(string)
	if id == "" {
		t.Fatalf("lease missing id")
	}

	nack, _ := json.Marshal(map[string]any{
		"event_id": id,
		"outcome":  "interrupted",
		"reason":   "user_barge_in",
	})
	code, _ := doReq(t, app, "POST", "/api/events/ack", nack)
	if code != fiber.StatusOK {
		t.Fatalf("nack: %d", code)
	}

	// Event should be back to queued — a fresh lease returns it again.
	_, again := doReq(t, app, "GET", "/api/events/next?min_priority=4", nil)
	var second map[string]any
	_ = json.Unmarshal(again, &second)
	if second["id"] != id {
		t.Errorf("expected re-lease of same id, got %v", second["id"])
	}
}

func TestEventAck_UnknownEventReturns404(t *testing.T) {
	deps := makeEventDeps(10)
	app := newEventApp(t, deps)
	body, _ := json.Marshal(map[string]any{
		"event_id":    "evt_does_not_exist",
		"outcome":     "delivered",
		"spoken_text": "noop",
	})
	code, _ := doReq(t, app, "POST", "/api/events/ack", body)
	if code != fiber.StatusNotFound {
		t.Errorf("expected 404 for unknown event, got %d", code)
	}
}

func TestEventAck_BadOutcomeReturns400(t *testing.T) {
	deps := makeEventDeps(10)
	app := newEventApp(t, deps)
	body, _ := json.Marshal(map[string]any{
		"event_id": "evt_x",
		"outcome":  "weirdvalue",
	})
	code, _ := doReq(t, app, "POST", "/api/events/ack", body)
	if code != fiber.StatusBadRequest {
		t.Errorf("expected 400 for bad outcome, got %d", code)
	}
}

func TestEventPeek_ReturnsQueueContents(t *testing.T) {
	deps := makeEventDeps(10)
	app := newEventApp(t, deps)
	body, _ := json.Marshal(map[string]any{
		"type":     "info",
		"priority": 1,
		"summary":  "first",
	})
	doReq(t, app, "POST", "/api/events/interrupt", body)

	code, out := doReq(t, app, "GET", "/api/events/peek", nil)
	if code != fiber.StatusOK {
		t.Fatalf("peek: %d", code)
	}
	var events []map[string]any
	if err := json.Unmarshal(out, &events); err != nil {
		t.Fatalf("peek body: %v", err)
	}
	if len(events) != 1 || events[0]["summary"] != "first" {
		t.Errorf("unexpected peek body: %v", events)
	}
}
