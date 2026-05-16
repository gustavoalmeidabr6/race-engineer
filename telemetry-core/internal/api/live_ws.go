package api

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"github.com/gofiber/contrib/websocket"
	"github.com/rs/zerolog/log"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/transcript"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/voice"
)

// LiveAgentRunner is the contract live_ws needs from a Gemini Live agent.
// In production this is *voice.LiveAgent; tests substitute a fake so the
// WebSocket plumbing can be exercised without burning real API quota.
type LiveAgentRunner interface {
	Available() bool
	Run(ctx context.Context) error
}

// liveSessionSlot enforces a process-wide "only one Gemini Live session at
// a time" invariant. Two dashboard tabs (or a hot-reloaded dev page, or
// React StrictMode's double mount) would otherwise each open
// /api/voice/live and end up with parallel Gemini sessions — two mic
// uploads, two model audio streams competing for the speakers, double
// billing. Newest-wins semantics: a new connection cancels the existing
// session and takes over. That matches the realistic recovery path of
// "I reloaded the dashboard mid-race".
type liveSessionSlot struct {
	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	preempt func() // notifies the current owner that it is being superseded
}

var activeLiveSession liveSessionSlot

// claim cancels the previously active session (if any), waits up to
// waitFor for it to fully exit, then registers (cancel, done, preempt)
// as the new active session. release() clears the slot iff this caller
// is still the registered owner — preventing a late-arriving release
// from a preempted session from blowing away the new one.
//
// preempt is invoked on the OLD owner just before cancellation so it
// can send a status frame ("you've been superseded by another tab")
// before its WebSocket dies. Without this the preempted dashboard
// auto-reconnects 3s later and the two tabs oscillate forever, each
// kicking the other out.
func (s *liveSessionSlot) claim(cancel context.CancelFunc, preempt func(), waitFor time.Duration) (done chan struct{}, release func()) {
	done = make(chan struct{})

	s.mu.Lock()
	prevCancel := s.cancel
	prevDone := s.done
	prevPreempt := s.preempt
	s.cancel = cancel
	s.done = done
	s.preempt = preempt
	s.mu.Unlock()

	if prevPreempt != nil {
		// Best-effort: the old session may have already torn down its
		// write goroutine; we don't care about errors here.
		safeInvoke(prevPreempt)
	}
	if prevCancel != nil {
		prevCancel()
	}
	if prevDone != nil {
		select {
		case <-prevDone:
		case <-time.After(waitFor):
			// Previous session is slow to wind down — proceed anyway
			// rather than blocking a legitimate reconnect. The new
			// Gemini SDK session will start; the stale one's Run()
			// will exit when its context cancellation propagates.
			log.Warn().Dur("waited", waitFor).Msg("prior live session slow to release; proceeding")
		}
	}

	release = func() {
		s.mu.Lock()
		if s.done == done {
			s.cancel = nil
			s.done = nil
			s.preempt = nil
		}
		s.mu.Unlock()
		close(done)
	}
	return done, release
}

// safeInvoke runs fn and swallows panics — preempt notifications fire
// while another session is mid-teardown, so a panicking write should
// not bring down the new connection.
func safeInvoke(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Warn().Interface("panic", r).Msg("live preempt notifier panicked")
		}
	}()
	fn()
}

// LiveDeps bundles the bits live_ws needs from the rest of the server.
// We don't reuse api.Deps because doing so creates an awkward circular
// import once the voice package wants to call back into something
// owned by api — keep it isolated and inject only what's used.
type LiveDeps struct {
	// AgentFactory builds a fresh LiveAgentRunner per WebSocket
	// connection. Sessions are per-connection so a tab close terminates
	// the session cleanly and reopening starts fresh. Only one session
	// runs at a time across the process — see liveSessionSlot for the
	// newest-wins enforcement; opening a second WebSocket cancels the
	// prior session before this factory is called again.
	//
	// audioIn is the channel the agent reads mic audio from.
	// audioOut is the channel the agent writes model audio into.
	// inputText/outputText surface STT transcriptions back to the
	// client; pass nil channels when the feature isn't wired.
	// toolEvents surfaces tool-call lifecycle frames (kind=tool_call /
	// tool_result) so the dashboard can render a real-time tool-use
	// timeline. Pass nil to suppress the broadcast — the Go core still
	// logs every call via zerolog.
	AgentFactory func(audioIn <-chan []byte, audioOut chan<- []byte, inputText, outputText chan<- string, toolEvents chan<- voice.ToolEvent) (LiveAgentRunner, error)

	// Transcript is the per-session interaction hub. When non-nil, the
	// handler also mirrors user/engineer text and tool-call lifecycle
	// frames into the hub — that's what the Live Debug tab reads.
	Transcript *transcript.Hub
}

// liveWSHandler returns a Fiber WebSocket handler for /api/voice/live.
//
// Protocol:
//
//	client → server: binary frames of PCM-16 mono mic audio at 16kHz
//	                 (per voice.LiveAudioInputRate).
//	server → client: binary frames of PCM-16 mono model audio at 24kHz
//	                 (per voice.LiveAudioOutputRate). The browser
//	                 reconstructs an AudioBuffer at that rate.
//	server → client: text/JSON frames with {"type":"input_text"|"output_text"|
//	                 "status"|"error", ...}. Used for live captions, state
//	                 transitions, and surfacing errors back to the UI.
//
// One Gemini Live session per WebSocket connection. The session ends as
// soon as the client closes or the agent's Run() returns.
func liveWSHandler(deps *LiveDeps) func(*websocket.Conn) {
	if deps == nil || deps.AgentFactory == nil {
		// Defensive: returning a closed-conn handler is preferable to
		// nil-panicking inside the websocket framework when Live is
		// misconfigured at boot.
		return func(conn *websocket.Conn) {
			_ = writeLiveJSON(conn, "error", map[string]any{"reason": "live agent factory not wired"})
			conn.Close()
		}
	}
	return func(conn *websocket.Conn) {
		defer conn.Close()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// writeMu serialises every write to the WebSocket. Declared
		// before claim() so the preempt notifier (invoked on the OLD
		// owner from claim() of a NEW connection) holds the same lock
		// as the audio + text goroutines below.
		var writeMu sync.Mutex

		// Tell a preempted client "you've been superseded" so its
		// dashboard auto-reconnect can stay idle instead of immediately
		// kicking us back out. Best-effort — if the WS is already dead
		// this is a no-op.
		preempt := func() {
			_ = writeLiveJSONLocked(&writeMu, conn, "status", map[string]string{"state": "superseded"})
		}

		// Enforce one Gemini Live session per process. If another tab
		// already has one open, notify it, cancel it, and wait briefly
		// for it to release before we boot a fresh session — otherwise
		// two SDK sessions briefly overlap on the wire and the user
		// hears two voices. See liveSessionSlot.
		_, release := activeLiveSession.claim(cancel, preempt, 2*time.Second)
		defer release()

		// gorilla/fiber WebSocket ReadMessage doesn't observe context
		// cancellation. When the singleton cancels this session from
		// another goroutine the reader below would otherwise stay blocked
		// until the client TCP-closes. Force ReadMessage to unblock by
		// expiring its read deadline; the resulting error wakes the
		// reader loop which then runs the defer-driven teardown.
		go func() {
			<-ctx.Done()
			_ = conn.SetReadDeadline(time.Now())
		}()

		// Channels sized to absorb ~1s of audio at 16kHz mono PCM-16
		// (32 KB/s). We pump at MediaRecorder cadence (~100ms frames)
		// so 16 slots leaves comfortable headroom before back-pressure.
		audioIn := make(chan []byte, 16)
		audioOut := make(chan []byte, 32)
		inputText := make(chan string, 8)
		outputText := make(chan string, 8)
		toolEvents := make(chan voice.ToolEvent, 16)

		agent, err := deps.AgentFactory(audioIn, audioOut, inputText, outputText, toolEvents)
		if err != nil {
			_ = writeLiveJSON(conn, "error", map[string]any{"reason": err.Error()})
			return
		}
		if !agent.Available() {
			_ = writeLiveJSON(conn, "error", map[string]any{"reason": "live agent unavailable (missing GEMINI_API_KEY?)"})
			return
		}

		// agentDone signals when the session terminates so the writer
		// loop can shut down cleanly. Buffer 1 so the agent goroutine
		// never blocks on send.
		agentDone := make(chan error, 1)
		go func() {
			agentDone <- agent.Run(ctx)
			close(audioOut)
			close(inputText)
			close(outputText)
			close(toolEvents)
		}()

		// (writeMu declared above so the preempt notifier can use it.)
		var wg sync.WaitGroup

		// Audio out: drain model audio, send as binary frames.
		wg.Add(1)
		go func() {
			defer wg.Done()
			for chunk := range audioOut {
				if len(chunk) == 0 {
					continue
				}
				if err := writeLiveBinary(&writeMu, conn, chunk); err != nil {
					cancel()
					return
				}
			}
		}()

		// Captions + tool events: drain text + tool-event channels and
		// forward each as its own JSON envelope. tool_event frames carry
		// the full ToolEvent shape so the dashboard can correlate
		// tool_call ↔ tool_result by call_id.
		//
		// turnID groups user_utterance → tool_call(s) → tool_result(s) →
		// engineer_speech under one card in the Live Debug tab. A fresh
		// id is minted on every inputText; outputText/tool events that
		// follow inherit it until the next inputText. Reset to "" on
		// session start so a stale id doesn't leak in from a prior conn.
		var turnID string
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case t, ok := <-inputText:
					if !ok {
						return
					}
					_ = writeLiveJSONLocked(&writeMu, conn, "input_text", map[string]string{"text": t})
					if deps.Transcript != nil && t != "" {
						turnID = newTurnID()
						deps.Transcript.Append(transcript.Event{
							Kind:  transcript.KindUserUtterance,
							Actor: "gemini_live",
							Text:  t,
							Meta:  map[string]any{"turn_id": turnID},
						})
					}
				case t, ok := <-outputText:
					if !ok {
						return
					}
					_ = writeLiveJSONLocked(&writeMu, conn, "output_text", map[string]string{"text": t})
					if deps.Transcript != nil && t != "" {
						deps.Transcript.Append(transcript.Event{
							Kind:  transcript.KindEngineerSpeech,
							Actor: "gemini_live",
							Text:  t,
							Meta:  turnMeta(turnID, nil),
						})
					}
				case ev, ok := <-toolEvents:
					if !ok {
						return
					}
					_ = writeLiveJSONLocked(&writeMu, conn, "tool_event", ev)
					if deps.Transcript != nil {
						mirrorToolEventToTranscript(deps.Transcript, ev, turnID)
					}
				case <-ctx.Done():
					return
				}
			}
		}()

		_ = writeLiveJSONLocked(&writeMu, conn, "status", map[string]string{"state": "ready"})

		// Reader loop runs on this goroutine so a disconnect surfaces
		// here and triggers cancel(), which winds the agent and the
		// other goroutines down. We accept both binary (audio) and
		// text (control) frames.
		for {
			mt, payload, err := conn.ReadMessage()
			if err != nil {
				log.Debug().Err(err).Msg("live ws read closed")
				cancel()
				break
			}
			switch mt {
			case websocket.BinaryMessage:
				if len(payload) == 0 {
					continue
				}
				select {
				case audioIn <- payload:
				default:
					// Mic input chan full: drop the oldest queued frame
					// rather than blocking the reader (which would also
					// block ReadMessage acks).
					select {
					case <-audioIn:
					default:
					}
					select {
					case audioIn <- payload:
					default:
					}
				}
			case websocket.TextMessage:
				handleLiveControlMessage(payload, cancel)
			}
		}
		close(audioIn)

		<-agentDone
		wg.Wait()
	}
}

// handleLiveControlMessage parses a client-sent JSON control envelope.
// Today the only honoured type is "bye" — explicit client-driven
// shutdown — but the surface is here so future PTT semantics, language
// hints, etc. can be added without renegotiating the protocol.
func handleLiveControlMessage(payload []byte, cancel context.CancelFunc) {
	var env struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return
	}
	switch env.Type {
	case "bye":
		cancel()
	}
}

// writeLiveBinary writes a binary frame under the connection's write
// mutex. Returns the underlying error so the audio-out goroutine can
// trip cancel() on disconnect.
func writeLiveBinary(mu *sync.Mutex, conn *websocket.Conn, payload []byte) error {
	mu.Lock()
	defer mu.Unlock()
	if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	return conn.WriteMessage(websocket.BinaryMessage, payload)
}

// writeLiveJSON / writeLiveJSONLocked write a JSON message envelope.
// The variant without a mutex is used during connection setup before
// any background writers have started.
func writeLiveJSON(conn *websocket.Conn, kind string, data any) error {
	body, err := json.Marshal(map[string]any{"type": kind, "data": data})
	if err != nil {
		return err
	}
	if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, body)
}

func writeLiveJSONLocked(mu *sync.Mutex, conn *websocket.Conn, kind string, data any) error {
	body, err := json.Marshal(map[string]any{"type": kind, "data": data})
	if err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()
	if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, body)
}

// Static assertion: *voice.LiveAgent implements LiveAgentRunner. Keeps
// the interface and implementation in sync.
var _ LiveAgentRunner = (*voice.LiveAgent)(nil)

// mirrorToolEventToTranscript writes a tool_call or tool_result row into
// the transcript so the Live Debug tab can correlate Gemini's tool use
// alongside user/engineer speech. The original WS frame still goes out
// to the dashboard's live session for the existing tool timeline. turnID
// (when non-empty) groups the row under the current user→engineer turn
// card in the Live Debug UI.
func mirrorToolEventToTranscript(hub *transcript.Hub, ev voice.ToolEvent, turnID string) {
	if hub == nil {
		return
	}
	meta := map[string]any{
		"tool":    ev.Name,
		"call_id": ev.CallID,
	}
	if turnID != "" {
		meta["turn_id"] = turnID
	}
	var kind transcript.Kind
	switch ev.Kind {
	case "tool_call":
		kind = transcript.KindToolCall
		if len(ev.Args) > 0 {
			meta["args"] = ev.Args
		}
	case "tool_result":
		kind = transcript.KindToolResult
		meta["elapsed_ms"] = ev.ElapsedMs
		if ev.ResultBytes > 0 {
			meta["result_bytes"] = ev.ResultBytes
		}
		if ev.Error != "" {
			meta["error"] = ev.Error
		}
	default:
		return
	}
	hub.Append(transcript.Event{
		Kind:  kind,
		Actor: "gemini_live",
		Text:  ev.Name,
		Meta:  meta,
	})
}

// turnMeta merges turnID into base if non-empty. Returns nil when both
// inputs are empty so the transcript row stays minimal.
func turnMeta(turnID string, base map[string]any) map[string]any {
	if turnID == "" && len(base) == 0 {
		return nil
	}
	out := make(map[string]any, len(base)+1)
	for k, v := range base {
		out[k] = v
	}
	if turnID != "" {
		out["turn_id"] = turnID
	}
	return out
}

// newTurnID returns a short random hex id used to group rows in the
// Live Debug tab. crypto/rand keeps collisions astronomically unlikely
// across concurrent Live sessions.
func newTurnID() string {
	var b [6]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		// Fall back to time-based id — collision-prone but only on the
		// rarely-hit "rand failed" path. Better than crashing the live
		// session for a debug feature.
		return time.Now().Format("150405.000")
	}
	return hex.EncodeToString(b[:])
}
