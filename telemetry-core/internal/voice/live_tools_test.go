package voice

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHTTPToolHandler_GetRaceState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/state/race" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"headline":"P3 lap 12","position":3,"lap":12}`))
	}))
	defer srv.Close()

	h := HTTPToolHandler(srv.URL, time.Second)
	out, err := h(context.Background(), "get_race_state", nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", out)
	}
	if m["position"].(float64) != 3 {
		t.Errorf("position=%v want 3", m["position"])
	}
}

func TestHTTPToolHandler_QueryBrain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/brain/snapshot" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Write([]byte("## Brain\nRR up 2%"))
	}))
	defer srv.Close()

	h := HTTPToolHandler(srv.URL, time.Second)
	out, err := h(context.Background(), "query_brain", nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	m := out.(map[string]any)
	if !strings.Contains(m["text"].(string), "RR up 2%") {
		t.Errorf("text=%q", m["text"])
	}
}

func TestHTTPToolHandler_PushStrategyInsight(t *testing.T) {
	var seen struct {
		mu     sync.Mutex
		body   map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/strategy" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if r.Method != "POST" {
			t.Errorf("method=%s", r.Method)
		}
		b, _ := io.ReadAll(r.Body)
		seen.mu.Lock()
		_ = json.Unmarshal(b, &seen.body)
		seen.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"queued"}`))
	}))
	defer srv.Close()

	h := HTTPToolHandler(srv.URL, time.Second)
	_, err := h(context.Background(), "push_strategy_insight", map[string]any{
		"summary":        "Norris pitted",
		"recommendation": "Push now to cover the undercut",
		"priority":       float64(4), // simulate JSON number
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	seen.mu.Lock()
	body := seen.body
	seen.mu.Unlock()
	if body["source"] != "gemini_live" {
		t.Errorf("source=%v", body["source"])
	}
	if !strings.Contains(body["insight"].(string), "Norris pitted") {
		t.Errorf("insight=%v", body["insight"])
	}
	if int(body["priority"].(float64)) != 4 {
		t.Errorf("priority=%v", body["priority"])
	}
}

func TestHTTPToolHandler_PushStrategyValidation(t *testing.T) {
	h := HTTPToolHandler("http://unreachable", time.Second)
	_, err := h(context.Background(), "push_strategy_insight", map[string]any{
		"recommendation": "x",
		"priority":       3,
	})
	if err == nil || !strings.Contains(err.Error(), "summary") {
		t.Errorf("expected summary-required error, got %v", err)
	}
}

// ask_data_analyst is fire-and-forget: it POSTs /api/analyst/query and
// returns the submitted job id immediately. The actual answer arrives
// asynchronously as a brain EventAnalystAnswer pushed by pi_agent —
// the tool itself MUST NOT poll. This test verifies the submit-and-go
// behaviour and that /api/brain/snapshot is NEVER hit by the tool.
func TestHTTPToolHandler_AskDataAnalyst_FireAndForget(t *testing.T) {
	var seenBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/analyst/query" && r.Method == "POST":
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &seenBody)
			w.WriteHeader(http.StatusAccepted)
			w.Write([]byte(`{"job_id":"job1","status":"queued","eta_seconds":12}`))
		case r.URL.Path == "/api/brain/snapshot":
			t.Errorf("fire-and-forget tool must not poll /api/brain/snapshot")
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	h := HTTPToolHandler(srv.URL, 10*time.Second)
	out, err := h(context.Background(), "ask_data_analyst", map[string]any{
		"question":      "When does the RR fall off the cliff?",
		"context_topic": "tire_deg",
		"urgent":        true,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	m := out.(map[string]any)
	if m["status"] != "submitted" {
		t.Errorf("status=%v, want submitted", m["status"])
	}
	if m["job_id"] != "job1" {
		t.Errorf("job_id=%v", m["job_id"])
	}
	if m["context_topic"] != "tire_deg" {
		t.Errorf("context_topic=%v", m["context_topic"])
	}
	if m["urgent"] != true {
		t.Errorf("urgent=%v", m["urgent"])
	}
	if eta, _ := m["eta_seconds"].(int); eta != 12 {
		t.Errorf("eta_seconds=%v (type %T)", m["eta_seconds"], m["eta_seconds"])
	}
	if !strings.Contains(m["message"].(string), "analyst") {
		t.Errorf("expected ack mentioning analyst, got %q", m["message"])
	}
	if seenBody["question"] != "When does the RR fall off the cliff?" {
		t.Errorf("submit body question=%v", seenBody["question"])
	}
	if seenBody["context_topic"] != "tire_deg" {
		t.Errorf("submit body topic=%v", seenBody["context_topic"])
	}
}

func TestHTTPToolHandler_AskDataAnalyst_ValidationErrors(t *testing.T) {
	h := HTTPToolHandler("http://unreachable", time.Second)
	if _, err := h(context.Background(), "ask_data_analyst", map[string]any{
		"context_topic": "x",
	}); err == nil || !strings.Contains(err.Error(), "question") {
		t.Errorf("expected question-required error, got %v", err)
	}
	if _, err := h(context.Background(), "ask_data_analyst", map[string]any{
		"question": "x",
	}); err == nil || !strings.Contains(err.Error(), "context_topic") {
		t.Errorf("expected context_topic-required error, got %v", err)
	}
}

func TestHTTPToolHandler_UnknownToolRejected(t *testing.T) {
	h := HTTPToolHandler("http://unreachable", time.Second)
	_, err := h(context.Background(), "definitely_not_a_tool", nil)
	if err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("expected unknown tool error, got %v", err)
	}
}

func TestIntArg(t *testing.T) {
	cases := []struct {
		in       map[string]any
		key      string
		fallback int
		want     int
	}{
		{map[string]any{"p": 3}, "p", 0, 3},
		{map[string]any{"p": float64(5)}, "p", 0, 5},
		{map[string]any{"p": "4"}, "p", 0, 4},
		{map[string]any{"p": int64(7)}, "p", 0, 7},
		{map[string]any{}, "p", 9, 9},
		{map[string]any{"p": "bogus"}, "p", 2, 2},
	}
	for _, c := range cases {
		got := intArg(c.in, c.key, c.fallback)
		if got != c.want {
			t.Errorf("intArg(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestDefaultLiveTools_ToolCount(t *testing.T) {
	tools := DefaultLiveTools()
	if len(tools) != 1 {
		t.Fatalf("expected exactly 1 Tool wrapper, got %d", len(tools))
	}
	// 4 original tools + 5 coaching tools (recent rolling + 4 lap-trace) +
	// 4 track-position / reminder tools (get_track_position,
	// set_corner_reminder, list_reminders, cancel_reminder). The reminder
	// surface was added so "remind me at T8" requests hit the right tool
	// instead of being silently swallowed as conversational acks.
	if got := len(tools[0].FunctionDeclarations); got != 13 {
		t.Errorf("expected 13 function declarations, got %d", got)
	}
	names := map[string]bool{}
	for _, fn := range tools[0].FunctionDeclarations {
		names[fn.Name] = true
	}
	for _, want := range []string{
		"get_race_state", "query_brain", "ask_data_analyst", "push_strategy_insight",
		"get_recent_telemetry",
		"list_laps", "get_lap_traces", "get_lap_delta", "compare_lap_corners",
		"get_track_position", "set_reminder", "list_reminders", "cancel_reminder",
	} {
		if !names[want] {
			t.Errorf("tool %q missing", want)
		}
	}
}

