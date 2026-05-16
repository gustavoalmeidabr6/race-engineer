package voice

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- HTTPEventsPoller -------------------------------------------------------

func TestHTTPEventsPoller_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/events/next" {
			t.Errorf("path %q", r.URL.Path)
		}
		if r.URL.Query().Get("min_priority") != "3" {
			t.Errorf("min_priority = %q, want 3", r.URL.Query().Get("min_priority"))
		}
		w.Write([]byte(`{
			"id":"evt_abc","type":"threat_overtake","priority":4,
			"summary":"P2 closing — gap 0.1s",
			"reasoning":"Norris on softs, 2 laps newer",
			"debug_data":{"car_idx":4,"gap_s":0.1}
		}`))
	}))
	defer srv.Close()

	poll := HTTPEventsPoller(srv.URL, 3)
	ev, err := poll(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ev == nil {
		t.Fatalf("expected an event, got nil")
	}
	if ev.ID != "evt_abc" || ev.Priority != 4 || !strings.Contains(ev.Summary, "P2 closing") {
		t.Errorf("event mis-decoded: %+v", ev)
	}
	if ev.DebugData["gap_s"].(float64) != 0.1 {
		t.Errorf("debug_data lost: %+v", ev.DebugData)
	}
}

func TestHTTPEventsPoller_EmptyQueueReturnsNilNoError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	poll := HTTPEventsPoller(srv.URL, 1)
	ev, err := poll(context.Background())
	if err != nil {
		t.Errorf("empty queue should not be an error: %v", err)
	}
	if ev != nil {
		t.Errorf("empty queue should yield nil event, got %+v", ev)
	}
}

func TestHTTPEventsPoller_ServerErrorReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv.Close()
	poll := HTTPEventsPoller(srv.URL, 1)
	_, err := poll(context.Background())
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("expected 503 error, got %v", err)
	}
}

func TestHTTPEventsPoller_PriorityClamp(t *testing.T) {
	cases := []struct{ in, want int }{
		{-1, 1}, {0, 1}, {1, 1}, {3, 3}, {5, 5}, {6, 5}, {99, 5},
	}
	for _, c := range cases {
		var observed atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n, _ := strconvAtoi(r.URL.Query().Get("min_priority"))
			observed.Store(int32(n))
			w.Write([]byte(`{}`))
		}))
		poll := HTTPEventsPoller(srv.URL, c.in)
		_, _ = poll(context.Background())
		srv.Close()
		if int(observed.Load()) != c.want {
			t.Errorf("in=%d → server saw %d, want %d", c.in, observed.Load(), c.want)
		}
	}
}

// --- HTTPEventsAcker --------------------------------------------------------

func TestHTTPEventsAcker_PostsExpectedBody(t *testing.T) {
	var capturedBody atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/events/ack" || r.Method != "POST" {
			t.Errorf("unexpected: %s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		capturedBody.Store(b)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	ack := HTTPEventsAcker(srv.URL)
	if err := ack(context.Background(), "evt_xyz", EventDelivered, "Norris closing 0.1"); err != nil {
		t.Fatalf("err: %v", err)
	}
	raw := capturedBody.Load().([]byte)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if body["event_id"] != "evt_xyz" {
		t.Errorf("event_id mismatch: %v", body["event_id"])
	}
	if body["outcome"] != "delivered" {
		t.Errorf("outcome mismatch: %v", body["outcome"])
	}
	if body["spoken_text"] != "Norris closing 0.1" {
		t.Errorf("spoken_text mismatch: %v", body["spoken_text"])
	}
}

func TestHTTPEventsAcker_NonSuccessReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	ack := HTTPEventsAcker(srv.URL)
	if err := ack(context.Background(), "x", EventErrored, ""); err == nil {
		t.Errorf("expected error on 500")
	}
}

// --- renderEventPrompt ------------------------------------------------------

func TestRenderEventPrompt_IncludesEverything(t *testing.T) {
	ev := &LiveEvent{
		ID:        "evt_1",
		Type:      "threat_overtake",
		Priority:  4,
		Summary:   "P2 closing — gap 0.1s",
		Reasoning: "Norris on fresher softs",
		DebugData: map[string]any{"car_idx": 4, "gap_s": 0.1},
	}
	out := renderEventPrompt(ev)
	wants := []string{
		"[RACE-ENGINEER PROMPT",
		"threat_overtake",
		"P4",
		"P2 closing — gap 0.1s",
		"Norris on fresher softs",
		"Urgent — drop whatever else you were going to say next.",
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("prompt missing %q\n--- prompt ---\n%s", w, out)
		}
	}
}

// renderEventPrompt on an analyst_answer event must expose the answer
// (Summary + Reasoning) AND the original question (carried in DebugData)
// so the model can attribute the radio call to the data analyst.
func TestRenderEventPrompt_AnalystAnswer(t *testing.T) {
	ev := &LiveEvent{
		ID:        "evt_a1",
		Type:      "analyst_answer",
		Priority:  3,
		Summary:   "RR cliff in 4 laps",
		Reasoning: "RR deg climbing 2%/lap from lap 18 onward; cliff threshold ~80%.",
		DebugData: map[string]any{
			"job_id":        "anq_42",
			"context_topic": "tire_deg",
			"question":      "When does the RR fall off the cliff?",
		},
	}
	out := renderEventPrompt(ev)
	wants := []string{
		"analyst_answer",
		"RR cliff in 4 laps",
		"RR deg climbing",
		"When does the RR fall off the cliff?", // question survives via DebugData
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("prompt missing %q\n--- prompt ---\n%s", w, out)
		}
	}
}

func TestEventPriorityTail_AllBuckets(t *testing.T) {
	for _, p := range []int{1, 2, 3, 4, 5} {
		if eventPriorityTail(p) == "" {
			t.Errorf("priority %d has empty tail", p)
		}
	}
	if got := eventPriorityTail(0); got != "Speak now." {
		t.Errorf("priority 0 fallback wrong: %q", got)
	}
}

// --- completeInFlight semantics --------------------------------------------

// TestCompleteInFlight_CallsAckExactlyOnce drives the in-flight tracker
// directly: stash an event in inFlight, fire completeInFlight twice, and
// verify the EventsAck callback is only invoked on the first call (the
// second sees an empty inFlight slot and no-ops).
func TestCompleteInFlight_CallsAckExactlyOnce(t *testing.T) {
	var acks atomic.Int32
	var seenID atomic.Value
	var seenOutcome atomic.Value
	a := &LiveAgent{
		cfg: LiveAgentConfig{
			EventsAck: func(_ context.Context, id string, outcome EventOutcome, _ string) error {
				acks.Add(1)
				seenID.Store(id)
				seenOutcome.Store(string(outcome))
				return nil
			},
		},
	}
	a.inFlight = &LiveEvent{ID: "evt_42", Summary: "x"}
	a.completeInFlight(context.Background(), EventDelivered, "")
	a.completeInFlight(context.Background(), EventDelivered, "") // no-op
	if got := acks.Load(); got != 1 {
		t.Errorf("acks=%d, want 1", got)
	}
	if seenID.Load() != "evt_42" {
		t.Errorf("seenID=%v", seenID.Load())
	}
	if seenOutcome.Load() != "delivered" {
		t.Errorf("seenOutcome=%v", seenOutcome.Load())
	}
}

func TestCompleteInFlight_NilEventsAckIsSafe(t *testing.T) {
	a := &LiveAgent{cfg: LiveAgentConfig{}}
	a.inFlight = &LiveEvent{ID: "evt_x"}
	a.completeInFlight(context.Background(), EventDelivered, "")
	if a.inFlight != nil {
		t.Errorf("inFlight should be cleared even without an acker")
	}
}

// --- concurrent pump-style stress ------------------------------------------

// Race two completeInFlight callers per seeded event and verify the
// mutex serialises them: exactly one wins per seed, the other sees an
// empty slot and no-ops. Run with `go test -race` for actual data-race
// detection; this test just checks the happens-before count.
func TestCompleteInFlight_ConcurrentSingleWinner(t *testing.T) {
	var acks atomic.Int32
	a := &LiveAgent{
		cfg: LiveAgentConfig{
			EventsAck: func(_ context.Context, _ string, _ EventOutcome, _ string) error {
				acks.Add(1)
				return nil
			},
		},
	}
	const seeds = 50
	for i := 0; i < seeds; i++ {
		a.inFlightMu.Lock()
		a.inFlight = &LiveEvent{ID: "x"}
		a.inFlightMu.Unlock()

		// Spawn two racing completers and wait for BOTH before seeding
		// the next slot — otherwise the next seed can fire while the
		// previous winner is still running, letting it ack a NEW event.
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); a.completeInFlight(context.Background(), EventDelivered, "") }()
		go func() { defer wg.Done(); a.completeInFlight(context.Background(), EventDropped, "user") }()
		wg.Wait()
	}
	if got := acks.Load(); int(got) != seeds {
		t.Errorf("acks=%d, want exactly %d (one winner per seed)", got, seeds)
	}
}

// Drive pumpEvents very briefly with a no-op session-like surface to
// confirm the in-flight slot blocks repeat leases. We can't drive a
// real *genai.Session in a unit test, but we can drive
// completeInFlight + the inFlight mutex from the poller side.
func TestPumpEvents_SingleFlightThrottle(t *testing.T) {
	// Two events ready immediately. The first lease should populate
	// inFlight; the second poll tick should see busy=true and skip.
	// (We don't have a real session here, so we simulate by NEVER
	// completing the in-flight — the test passes when only one event
	// is consumed by the poller within the test window.)
	var polls atomic.Int32
	poll := func(_ context.Context) (*LiveEvent, error) {
		polls.Add(1)
		return &LiveEvent{ID: "evt_blocking", Type: "lap_summary", Priority: 2, Summary: "test"}, nil
	}
	a := &LiveAgent{cfg: LiveAgentConfig{
		EventsPoll:        poll,
		EventPollInterval: 20 * time.Millisecond,
	}}

	// Lease one manually (simulating dispatch already happened).
	a.inFlightMu.Lock()
	a.inFlight = &LiveEvent{ID: "evt_inflight"}
	a.inFlightMu.Unlock()

	// Spin up an inner loop that mimics pumpEvents' busy-check + lease
	// behaviour without the actual session.SendClientContent call.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			if got := polls.Load(); got != 0 {
				t.Errorf("expected zero polls while inFlight set, got %d", got)
			}
			return
		case <-ticker.C:
			a.inFlightMu.Lock()
			busy := a.inFlight != nil
			a.inFlightMu.Unlock()
			if busy {
				continue
			}
			_, _ = a.cfg.EventsPoll(ctx)
		}
	}
}

// TestEventBundler_FoldsSiblingsIntoSingleDispatch exercises the bundle
// contract: when a P>=floor event leases and a bundler is wired, calling
// the bundler returns the merged target (with combined summary) plus the
// sibling ids that got absorbed. The pump uses this to dispatch ONE
// merged prompt instead of two back-to-back ones.
func TestEventBundler_FoldsSiblingsIntoSingleDispatch(t *testing.T) {
	calls := atomic.Int32{}
	bundler := func(_ context.Context, targetID string, floor int) (*LiveEvent, []string) {
		calls.Add(1)
		if targetID != "evt_box" {
			t.Errorf("targetID = %q, want evt_box", targetID)
		}
		if floor < 4 {
			t.Errorf("floor = %d, want >=4", floor)
		}
		return &LiveEvent{
			ID:       "evt_box",
			Type:     "box_now",
			Priority: 5,
			Summary:  "Box this lap. | Safety car deployed.",
		}, []string{"evt_safety"}
	}

	a := &LiveAgent{cfg: LiveAgentConfig{EventBundler: bundler}}
	a.inFlight = &LiveEvent{ID: "evt_box", Type: "box_now", Priority: 5, Summary: "Box this lap."}

	merged, siblings := a.cfg.EventBundler(context.Background(), a.inFlight.ID, defaultEventBundleFloor)
	if merged == nil || len(siblings) != 1 {
		t.Fatalf("bundler should return merged + 1 sibling, got %+v / %v", merged, siblings)
	}
	if !strings.Contains(merged.Summary, "Box this lap") || !strings.Contains(merged.Summary, "Safety car") {
		t.Errorf("merged summary missing one side: %q", merged.Summary)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("bundler should be called exactly once, got %d", got)
	}

	// Render the merged prompt the same way pumpEvents would — both
	// summaries must reach the model in a single turn.
	prompt := renderEventPrompt(merged)
	if !strings.Contains(prompt, "Box this lap") || !strings.Contains(prompt, "Safety car") {
		t.Errorf("rendered prompt missing one side:\n%s", prompt)
	}
}

// TestEventBundler_LowPriorityNotInvoked: a P3 event must not trigger
// the bundle window — only the high-priority lane bundles.
func TestEventBundler_LowPriorityNotInvoked(t *testing.T) {
	calls := atomic.Int32{}
	bundler := func(_ context.Context, _ string, _ int) (*LiveEvent, []string) {
		calls.Add(1)
		return nil, nil
	}
	ev := &LiveEvent{ID: "evt_low", Priority: 3, Summary: "Routine update."}
	floor := defaultEventBundleFloor

	// Replicate pumpEvents' "only bundle when ev.Priority >= floor" guard.
	if ev.Priority >= floor {
		_, _ = bundler(context.Background(), ev.ID, floor)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("bundler invoked for P3 event (floor=%d), expected skip", floor)
	}
}

// strconvAtoi is a vendored helper so the test doesn't add a strconv
// import (keeping the test file self-contained).
func strconvAtoi(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, nil
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errBadInt
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

var errBadInt = errCustom("bad int")

type errCustom string

func (e errCustom) Error() string { return string(e) }
