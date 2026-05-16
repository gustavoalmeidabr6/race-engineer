package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/contrib/websocket"
	"github.com/gofiber/fiber/v2"
	gws "github.com/gorilla/websocket"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/voice"
)

// --- fake LiveAgentRunner ---------------------------------------------------

type fakeLiveAgent struct {
	available      bool
	startErr       error
	frames         atomic.Int32 // increments when audioIn receives a frame
	emitAudio      []byte       // optional: sent to audioOut on start
	emitInputText  string       // optional
	emitOutputText string       // optional
	audioIn        <-chan []byte
	audioOut       chan<- []byte
	inputText      chan<- string
	outputText     chan<- string
}

func (f *fakeLiveAgent) Available() bool { return f.available }

func (f *fakeLiveAgent) Run(ctx context.Context) error {
	if f.startErr != nil {
		return f.startErr
	}
	// Optional one-shot side effects so tests can drive the writer path.
	if len(f.emitAudio) > 0 {
		f.audioOut <- f.emitAudio
	}
	if f.emitInputText != "" {
		f.inputText <- f.emitInputText
	}
	if f.emitOutputText != "" {
		f.outputText <- f.emitOutputText
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-f.audioIn:
			if !ok {
				return nil
			}
			f.frames.Add(1)
		}
	}
}

// --- helpers ----------------------------------------------------------------

func startLiveServer(t *testing.T, deps *LiveDeps) (string, func()) {
	t.Helper()
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use("/api/voice/live", func(c *fiber.Ctx) error {
		if websocket.IsWebSocketUpgrade(c) {
			return c.Next()
		}
		return fiber.ErrUpgradeRequired
	})
	app.Get("/api/voice/live", websocket.New(liveWSHandler(deps)))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go app.Listener(ln) //nolint:errcheck

	cleanup := func() {
		_ = app.Shutdown()
		_ = ln.Close()
	}
	return ln.Addr().String(), cleanup
}

func dialLiveWS(t *testing.T, addr string) *gws.Conn {
	t.Helper()
	u := url.URL{Scheme: "ws", Host: addr, Path: "/api/voice/live"}
	// Brief retry — Fiber occasionally takes a few ms to start accepting.
	var conn *gws.Conn
	var err error
	for i := 0; i < 20; i++ {
		conn, _, err = gws.DefaultDialer.Dial(u.String(), nil)
		if err == nil {
			return conn
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("dial %s: %v", u.String(), err)
	return nil
}

func readJSONFrame(t *testing.T, conn *gws.Conn) map[string]any {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	mt, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if mt != gws.TextMessage {
		t.Fatalf("expected text, got mt=%d", mt)
	}
	var env map[string]any
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, payload)
	}
	return env
}

// --- tests ------------------------------------------------------------------

func TestLiveWS_RejectsWhenAgentFactoryMissing(t *testing.T) {
	addr, cleanup := startLiveServer(t, &LiveDeps{AgentFactory: nil})
	defer cleanup()

	conn := dialLiveWS(t, addr)
	defer conn.Close()
	env := readJSONFrame(t, conn)
	if env["type"] != "error" {
		t.Errorf("expected error envelope, got %v", env)
	}
}

func TestLiveWS_RejectsWhenAgentUnavailable(t *testing.T) {
	addr, cleanup := startLiveServer(t, &LiveDeps{
		AgentFactory: func(ai <-chan []byte, ao chan<- []byte, it, ot chan<- string, _ chan<- voice.ToolEvent) (LiveAgentRunner, error) {
			return &fakeLiveAgent{available: false, audioIn: ai, audioOut: ao, inputText: it, outputText: ot}, nil
		},
	})
	defer cleanup()

	conn := dialLiveWS(t, addr)
	defer conn.Close()
	env := readJSONFrame(t, conn)
	if env["type"] != "error" {
		t.Errorf("expected error, got %v", env)
	}
	data, _ := env["data"].(map[string]any)
	if reason, _ := data["reason"].(string); !strings.Contains(reason, "unavailable") {
		t.Errorf("reason did not mention unavailable: %v", data)
	}
}

func TestLiveWS_FactoryErrorReturnedToClient(t *testing.T) {
	addr, cleanup := startLiveServer(t, &LiveDeps{
		AgentFactory: func(_ <-chan []byte, _ chan<- []byte, _, _ chan<- string, _ chan<- voice.ToolEvent) (LiveAgentRunner, error) {
			return nil, errors.New("boom: factory blew up")
		},
	})
	defer cleanup()

	conn := dialLiveWS(t, addr)
	defer conn.Close()
	env := readJSONFrame(t, conn)
	if env["type"] != "error" {
		t.Fatalf("expected error envelope, got %v", env)
	}
	data, _ := env["data"].(map[string]any)
	if reason, _ := data["reason"].(string); !strings.Contains(reason, "boom") {
		t.Errorf("reason missing factory error: %v", data)
	}
}

func TestLiveWS_HappyPath_AudioRoundTrip(t *testing.T) {
	var capturedAgent *fakeLiveAgent
	var mu sync.Mutex
	addr, cleanup := startLiveServer(t, &LiveDeps{
		AgentFactory: func(ai <-chan []byte, ao chan<- []byte, it, ot chan<- string, _ chan<- voice.ToolEvent) (LiveAgentRunner, error) {
			mu.Lock()
			defer mu.Unlock()
			capturedAgent = &fakeLiveAgent{
				available:      true,
				audioIn:        ai,
				audioOut:       ao,
				inputText:      it,
				outputText:     ot,
				emitAudio:      []byte{0x01, 0x02, 0x03, 0x04},
				emitInputText:  "box this lap",
				emitOutputText: "Copy, boxing",
			}
			return capturedAgent, nil
		},
	})
	defer cleanup()

	conn := dialLiveWS(t, addr)
	defer conn.Close()

	// We don't know in which order the JSON frames will land relative to
	// the binary frame, so collect frames for ~500ms and assert by type.
	gotStatus := false
	gotInputText := false
	gotOutputText := false
	gotBinaryHex := ""
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !(gotStatus && gotInputText && gotOutputText && gotBinaryHex != "") {
		conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		mt, payload, err := conn.ReadMessage()
		if err != nil {
			break
		}
		switch mt {
		case gws.TextMessage:
			var env map[string]any
			if json.Unmarshal(payload, &env) != nil {
				continue
			}
			switch env["type"] {
			case "status":
				gotStatus = true
			case "input_text":
				gotInputText = true
			case "output_text":
				gotOutputText = true
			}
		case gws.BinaryMessage:
			gotBinaryHex = fmt.Sprintf("%x", payload)
		}
	}
	if !gotStatus {
		t.Errorf("missing status frame")
	}
	if !gotInputText {
		t.Errorf("missing input_text frame")
	}
	if !gotOutputText {
		t.Errorf("missing output_text frame")
	}
	if gotBinaryHex != "01020304" {
		t.Errorf("binary frame mismatch: %q", gotBinaryHex)
	}

	// Send a binary mic frame; the fake agent counts it.
	if err := conn.WriteMessage(gws.BinaryMessage, []byte{0xAA, 0xBB}); err != nil {
		t.Fatalf("send mic: %v", err)
	}
	// Let the agent goroutine process it. Poll the counter rather than sleeping for a fixed duration.
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		count := capturedAgent.frames.Load()
		mu.Unlock()
		if count >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	got := capturedAgent.frames.Load()
	mu.Unlock()
	if got < 1 {
		t.Errorf("expected agent to receive mic frame, got %d", got)
	}

	// Sending a bye control message should cleanly terminate the session.
	bye, _ := json.Marshal(map[string]string{"type": "bye"})
	_ = conn.WriteMessage(gws.TextMessage, bye)
}

func TestLiveWS_DiscardsOversizedAudioBacklog(t *testing.T) {
	// Block the agent's read by sending more frames than the channel
	// capacity (16). The handler should drop oldest entries rather than
	// blocking the reader.
	addr, cleanup := startLiveServer(t, &LiveDeps{
		AgentFactory: func(_ <-chan []byte, ao chan<- []byte, it, ot chan<- string, _ chan<- voice.ToolEvent) (LiveAgentRunner, error) {
			// agent that never reads audioIn — forces back-pressure.
			return &slowAgent{audioOut: ao, inputText: it, outputText: ot}, nil
		},
	})
	defer cleanup()

	conn := dialLiveWS(t, addr)
	defer conn.Close()
	// Drain the initial JSON frames quickly.
	_ = readJSONFrame(t, conn) // status

	// Send 50 binary frames; the handler should never block.
	deadline := time.Now().Add(2 * time.Second)
	for i := 0; i < 50; i++ {
		conn.SetWriteDeadline(time.Now().Add(200 * time.Millisecond))
		if err := conn.WriteMessage(gws.BinaryMessage, []byte{byte(i)}); err != nil {
			t.Fatalf("write %d: %v (frames likely blocking)", i, err)
		}
		if time.Now().After(deadline) {
			break
		}
	}
}

// slowAgent ignores audio input — used to verify back-pressure handling.
type slowAgent struct {
	audioOut   chan<- []byte
	inputText  chan<- string
	outputText chan<- string
}

func (s *slowAgent) Available() bool { return true }
func (s *slowAgent) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// --- singleton enforcement --------------------------------------------------

func TestLiveSessionSlot_NewestWins(t *testing.T) {
	var slot liveSessionSlot

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	_, release1 := slot.claim(cancel1, nil, 500*time.Millisecond)

	// Second claimant must cancel the first one before its claim returns.
	_, cancel2 := context.WithCancel(context.Background())
	defer cancel2()

	claimedCh := make(chan struct{})
	go func() {
		_, release2 := slot.claim(cancel2, nil, 500*time.Millisecond)
		close(claimedCh)
		release2()
	}()

	// First context should observe cancel from the second claim. Give the
	// goroutine a moment; we expect ctx1.Done before claim2 returns
	// (because cancel1 fires synchronously inside slot.claim).
	select {
	case <-ctx1.Done():
		// expected
	case <-time.After(time.Second):
		t.Fatal("first session was not cancelled by the new claimant")
	}

	// First session must release before second claim returns (because
	// claim waits on prevDone).
	select {
	case <-claimedCh:
		t.Fatal("second claim returned before first released")
	case <-time.After(50 * time.Millisecond):
		// expected: second claim blocked on prevDone
	}
	release1()

	select {
	case <-claimedCh:
		// expected
	case <-time.After(time.Second):
		t.Fatal("second claim never completed after first release")
	}
}

func TestLiveSessionSlot_LateReleaseDoesNotClobberSuccessor(t *testing.T) {
	var slot liveSessionSlot

	_, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	_, release1 := slot.claim(cancel1, nil, 100*time.Millisecond)

	_, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	_, release2 := slot.claim(cancel2, nil, 100*time.Millisecond)

	// Late release from the preempted session must NOT clear the slot.
	release1()
	slot.mu.Lock()
	if slot.cancel == nil {
		slot.mu.Unlock()
		t.Fatal("late release from preempted session cleared the active slot")
	}
	slot.mu.Unlock()

	release2()
	slot.mu.Lock()
	if slot.cancel != nil {
		slot.mu.Unlock()
		t.Fatal("active release did not clear the slot")
	}
	slot.mu.Unlock()
}

func TestLiveWS_SecondConnectionPreemptsFirst(t *testing.T) {
	var runStarted, runStopped atomic.Int32
	agentRun := func(ai <-chan []byte, ao chan<- []byte, it, ot chan<- string, _ chan<- voice.ToolEvent) (LiveAgentRunner, error) {
		return &lifecycleAgent{
			audioIn:    ai,
			audioOut:   ao,
			inputText:  it,
			outputText: ot,
			started:    &runStarted,
			stopped:    &runStopped,
		}, nil
	}

	addr, cleanup := startLiveServer(t, &LiveDeps{AgentFactory: agentRun})
	defer cleanup()

	c1 := dialLiveWS(t, addr)
	defer c1.Close()
	_ = readJSONFrame(t, c1) // status: ready

	// Wait until the first agent's Run() is actually executing.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && runStarted.Load() < 1 {
		time.Sleep(10 * time.Millisecond)
	}
	if runStarted.Load() < 1 {
		t.Fatal("first agent never started")
	}

	// Open a second connection. The handler should cancel the first
	// session and stand up a fresh one.
	c2 := dialLiveWS(t, addr)
	defer c2.Close()
	_ = readJSONFrame(t, c2) // status: ready from the second session

	// The first agent's Run() must have returned by now (the singleton
	// cancelled its context).
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && runStopped.Load() < 1 {
		time.Sleep(10 * time.Millisecond)
	}
	if runStopped.Load() < 1 {
		t.Fatalf("first agent was not preempted: started=%d stopped=%d",
			runStarted.Load(), runStopped.Load())
	}
	if runStarted.Load() < 2 {
		t.Fatalf("second agent never started: started=%d", runStarted.Load())
	}
}

// TestLiveWS_SecondConnectionSendsSupersededToFirst verifies that the
// older session receives a {"type":"status","data":{"state":"superseded"}}
// frame before its WS is torn down. The dashboard relies on this hint
// to suppress its 3-second auto-reconnect — otherwise two tabs would
// oscillate, each kicking the other out forever.
func TestLiveWS_SecondConnectionSendsSupersededToFirst(t *testing.T) {
	agentRun := func(_ <-chan []byte, ao chan<- []byte, it, ot chan<- string, _ chan<- voice.ToolEvent) (LiveAgentRunner, error) {
		return &slowAgent{audioOut: ao, inputText: it, outputText: ot}, nil
	}

	addr, cleanup := startLiveServer(t, &LiveDeps{AgentFactory: agentRun})
	defer cleanup()

	c1 := dialLiveWS(t, addr)
	defer c1.Close()
	if env := readJSONFrame(t, c1); env["type"] != "status" {
		t.Fatalf("c1: expected ready status, got %v", env)
	}

	// Open a second connection. The handler should fire the preempt
	// notifier on c1 BEFORE cancelling the session — c1 must observe a
	// superseded status frame.
	c2 := dialLiveWS(t, addr)
	defer c2.Close()

	// Read frames from c1 until we see the superseded status or the
	// socket closes / times out. The frame is emitted before
	// ReadMessage on c1 wakes up via the read deadline.
	gotSuperseded := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c1.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		mt, payload, err := c1.ReadMessage()
		if err != nil {
			break
		}
		if mt != gws.TextMessage {
			continue
		}
		var env map[string]any
		if json.Unmarshal(payload, &env) != nil {
			continue
		}
		if env["type"] == "status" {
			data, _ := env["data"].(map[string]any)
			if state, _ := data["state"].(string); state == "superseded" {
				gotSuperseded = true
				break
			}
		}
	}
	if !gotSuperseded {
		t.Fatal("preempted session never received superseded status frame")
	}
}

// lifecycleAgent records start/stop counts so tests can assert that one
// session was preempted and another took its place.
type lifecycleAgent struct {
	audioIn    <-chan []byte
	audioOut   chan<- []byte
	inputText  chan<- string
	outputText chan<- string
	started    *atomic.Int32
	stopped    *atomic.Int32
}

func (l *lifecycleAgent) Available() bool { return true }
func (l *lifecycleAgent) Run(ctx context.Context) error {
	l.started.Add(1)
	defer l.stopped.Add(1)
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-l.audioIn:
			if !ok {
				return nil
			}
		}
	}
}
