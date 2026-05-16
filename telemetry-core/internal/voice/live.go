package voice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
	"google.golang.org/genai"
)

// DefaultLiveModel is the Gemini Live API model the agent connects to by
// default. The Python service used "gemini-3.1-flash-live-preview"; we
// match that string so behaviour is identical when the user switches
// between the two implementations during the migration.
const DefaultLiveModel = "gemini-3.1-flash-live-preview"

// LiveAudioInputRate is the sample rate (Hz) Gemini Live expects on the
// inbound mic stream. The dashboard's AudioWorklet downsamples to this
// rate before pushing PCM-16 frames over the WebSocket.
const LiveAudioInputRate = 16000

// LiveAudioOutputRate is the sample rate (Hz) Gemini Live emits on the
// model's audio output. We wrap chunks in a WAV header at this rate
// before forwarding to the browser <audio> element.
const LiveAudioOutputRate = 24000

// ToolHandler is the function shape the Live agent calls back to handle
// a tool invocation. The agent itself stays decoupled from the REST
// surface: cmd/server passes in a handler that knows how to translate
// (name, args) into an in-process call against the real server's
// brain / store / analyst. Returning an error causes the response to
// carry {"error": "..."} so the model can self-correct.
type ToolHandler func(ctx context.Context, name string, args map[string]any) (any, error)

// LiveEvent is the subset of brain.Event the Live agent needs to render
// a synthetic user-role turn for the model. Kept here (not imported from
// internal/brain) so internal/voice stays leaf-package and the agent
// can run against any event-shaped struct, not only the live brain.
type LiveEvent struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`
	Priority  int            `json:"priority"`
	Summary   string         `json:"summary"`
	Reasoning string         `json:"reasoning,omitempty"`
	DebugData map[string]any `json:"debug_data,omitempty"`
}

// EventOutcome mirrors the brain bus' ack vocabulary so callers can
// honour the lease lifecycle. delivered/dropped close the lease;
// interrupted/errored return the event to queued for retry.
type EventOutcome string

const (
	EventDelivered   EventOutcome = "delivered"
	EventDropped     EventOutcome = "dropped"
	EventInterrupted EventOutcome = "interrupted"
	EventErrored     EventOutcome = "errored"
)

// EventsPoller is the callback shape that gives the Live agent its view
// of the brain Interrupts Bus. Implementations typically poll
// /api/events/next; the contract is "block briefly, return one event or
// nil". A nil event means "nothing to dispatch right now, try again".
// Errors are logged at Debug and treated as transient.
type EventsPoller func(ctx context.Context) (*LiveEvent, error)

// EventsAcker closes the lease the bus opened when the event was leased.
// Called once per dispatched event after turn_complete (or after a
// terminal failure). For Phase 3.2 MVP we always send "delivered" — the
// retry/interrupt nuances from the Python service (see todos.md §2c–2e)
// are deferred.
type EventsAcker func(ctx context.Context, eventID string, outcome EventOutcome, spokenText string) error

// EventBundler optionally folds other concurrently-queued same-priority
// events into the in-flight target so the model dispatches one bundled
// turn instead of N back-to-back ones. Returns the mutated target (with
// merged summary/reasoning) and the ids of siblings that were absorbed.
// A nil return means "no siblings worth bundling" — pumpEvents proceeds
// with the original event unchanged.
//
// Wired in cmd/server to brain.MergePendingInto; tests can stub it.
type EventBundler func(ctx context.Context, targetEventID string, floor int) (*LiveEvent, []string)

// LiveAgentConfig collects everything NewLiveAgent needs. Anything
// optional has a documented default so a caller can pass a zero-value
// struct + APIKey and still get a working agent.
type LiveAgentConfig struct {
	APIKey    string      // required; from cfg.GeminiAPIKey
	Model     string      // optional; defaults to DefaultLiveModel
	SystemMD  string      // optional; appended to the system instruction at session start
	InitBrain string      // optional; appended after SystemMD, in a "## Initial Race Brain" section
	OnTool    ToolHandler // required; called for every server-issued tool_call
	Tools     []*genai.Tool

	// AudioIn is the channel the agent reads outgoing mic audio from.
	// Frames must be PCM-16 mono at LiveAudioInputRate. Closing the
	// channel does NOT close the session — the agent treats it as a
	// stream that may resume later (e.g. the user toggled PTT off then
	// on again).
	AudioIn <-chan []byte

	// AudioOut is the channel the agent writes inbound model audio to.
	// Frames are PCM-16 mono at LiveAudioOutputRate, raw (no WAV
	// wrapper). The consumer wraps them per chunk OR concatenates and
	// wraps once per turn — both are fine.
	AudioOut chan<- []byte

	// Outputs the model's spoken text (transcription of its own audio
	// output) so the dashboard can show a live caption. Nil disables.
	OutputText chan<- string

	// Inputs the user's spoken text (server-side STT of their mic
	// audio) so the dashboard can show what was heard. Nil disables.
	InputText chan<- string

	// EventsPoll + EventsAck wire the brain Interrupts Bus into the
	// session. When the brain fires a priority event (e.g. "P2 closing
	// — gap 0.1s"), pumpEvents leases it via EventsPoll, injects an
	// "[RACE-ENGINEER PROMPT]" synthetic user-role turn into the
	// session via SendClientContent, then acks via EventsAck on
	// turn_complete. Without these the engineer never speaks
	// proactively — events sit unread on the bus.
	//
	// Both may be nil; the agent just skips the events pump when so,
	// useful in tests that exercise audio/tool paths only.
	EventsPoll EventsPoller
	EventsAck  EventsAcker

	// EventPollInterval bounds how often pumpEvents asks the bus for
	// the next leasable event. The Python service ran the fast-lane
	// at 250 ms; we match. Zero = use default.
	EventPollInterval time.Duration

	// EventBundler, when non-nil, is invoked after leasing a P>=Floor
	// event so any siblings that landed during the brief gather window
	// get folded into the same dispatch. See EventBundler doc above.
	EventBundler EventBundler

	// EventBundleDelay is the gather window between leasing a
	// bundle-eligible event and calling EventBundler. The rule engine
	// and external pushers tend to emit correlated alerts within
	// ~50–200 ms of each other; a 250 ms default catches them without
	// adding perceptible radio latency. Zero disables the wait (the
	// bundler still gets one shot, just immediately). Capped to 1 s
	// internally to keep the model responsive even on misconfiguration.
	EventBundleDelay time.Duration

	// EventBundlePriorityFloor is the minimum priority at which
	// bundling activates. Default (0 → highPriorityCoalesceFloor=4 from
	// the brain package). Routine P1–P3 calls dispatch one-at-a-time
	// as today.
	EventBundlePriorityFloor int

	// ToolEvents is an optional sink for real-time tool-call lifecycle
	// observations (kind=tool_call before invocation, kind=tool_result
	// after — with elapsed_ms + result_bytes or error). The Go core
	// already logs every call via zerolog; this channel lets the
	// dashboard (or any other consumer) render a tool-use timeline
	// without parsing log lines. Non-blocking — events are dropped if
	// the channel is full.
	ToolEvents chan<- ToolEvent

	// RadioCheckPrompt, when non-nil, is called once right after the
	// Gemini Live session connects. A non-empty return value is sent as
	// a synthetic user-role turn with turn_complete=true, prompting the
	// model to open the conversation — typically with a "radio check"
	// greeting on a fresh F1 session. Returning "" suppresses the
	// kick-off (e.g. for a tab reload mid-session). The agent itself
	// stays agnostic about what counts as "fresh"; the caller decides.
	RadioCheckPrompt func() string
}

// LiveAgent owns a single Gemini Live session for the lifetime of a
// connected dashboard. Run blocks until ctx is cancelled or the session
// terminates fatally; the caller restarts it on disconnect.
//
// The agent is stateless across sessions — every Run() opens a fresh
// connection. Session resumption (the Python service's handle
// management) is deferred to Phase 3.2.1 once the MVP loop is proven.
type LiveAgent struct {
	cfg     LiveAgentConfig
	client  *genai.Client
	running atomic.Bool

	// arbiter, when set, ensures no other in-process audio producer
	// (VoiceClient TTS, future event speakers, etc.) emits while this
	// Live session is connected. Acquired in Run() before any audio
	// flows, released on Run() return.
	arbiter *OutputArbiter

	// inFlightEvent holds the most-recently-dispatched brain event so
	// pumpServer can ack the lease when the model finishes speaking
	// (turn_complete). Single-flight by convention: pumpEvents won't
	// lease a new event while one is in flight, mirroring the Python
	// dispatcher's `_in_flight` invariant.
	inFlightMu sync.Mutex
	inFlight   *LiveEvent
}

// SetArbiter wires the in-process output arbiter so this Live session
// holds the speaking slot for its full lifetime. Call after construction
// and before Run().
func (a *LiveAgent) SetArbiter(arb *OutputArbiter) {
	if a == nil {
		return
	}
	a.arbiter = arb
}

// NewLiveAgent constructs a Live agent. Mirrors NewGeminiTTS' contract:
// returns a non-nil *LiveAgent that reports a Run() error when the API
// key is missing, rather than panicking at boot.
func NewLiveAgent(ctx context.Context, cfg LiveAgentConfig) (*LiveAgent, error) {
	if cfg.Model == "" {
		cfg.Model = DefaultLiveModel
	}
	if cfg.OnTool == nil {
		cfg.OnTool = func(_ context.Context, name string, _ map[string]any) (any, error) {
			return nil, fmt.Errorf("no tool handler wired; refusing call to %q", name)
		}
	}
	a := &LiveAgent{cfg: cfg}
	if cfg.APIKey == "" {
		log.Warn().Msg("GEMINI_API_KEY not set — Gemini Live disabled")
		return a, nil
	}
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  cfg.APIKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		log.Error().Err(err).Msg("Gemini Live client init failed")
		return a, fmt.Errorf("gemini live client: %w", err)
	}
	a.client = client
	log.Info().Str("model", cfg.Model).Msg("Gemini Live ready")
	return a, nil
}

// Available reports whether the agent has a usable SDK client.
func (a *LiveAgent) Available() bool { return a != nil && a.client != nil }

// Run opens a session, ships AudioIn frames in, fans AudioOut/tools out,
// and blocks until ctx is cancelled. Returns the first fatal error so
// the caller can decide whether to reconnect.
func (a *LiveAgent) Run(ctx context.Context) error {
	if !a.Available() {
		return errors.New("gemini live: client unavailable (missing API key?)")
	}
	if a.running.Load() {
		return errors.New("gemini live: already running")
	}
	a.running.Store(true)
	defer a.running.Store(false)

	// Take the audio output slot for the duration of the session. If
	// another producer (VoiceClient TTS, an event speaker) currently
	// holds the slot it will block here until release — which is the
	// correct behaviour: we don't open a Gemini Live session and start
	// burning tokens while another voice is mid-phrase. The session
	// owns the slot until Run() returns; the arbiter has no preempt
	// path for "stop speaking now" because Live's audio is a continuous
	// SDK stream we can't pause mid-frame anyway.
	if a.arbiter != nil {
		sessionCtx, sessionCancel := context.WithCancel(ctx)
		release, aerr := a.arbiter.Acquire(ctx, "live", false, sessionCancel)
		if aerr != nil {
			sessionCancel()
			return fmt.Errorf("audio arbiter: %w", aerr)
		}
		defer release()
		ctx = sessionCtx
	}

	sysInstr := liveSystemInstruction
	if a.cfg.SystemMD != "" {
		sysInstr = a.cfg.SystemMD
	}
	if a.cfg.InitBrain != "" {
		sysInstr += "\n\n## Initial Race Brain (at session start)\n\n" + a.cfg.InitBrain
	}

	cfg := &genai.LiveConnectConfig{
		ResponseModalities: []genai.Modality{genai.ModalityAudio},
		SystemInstruction: &genai.Content{
			Parts: []*genai.Part{{Text: sysInstr}},
		},
		Tools: a.cfg.Tools,
		// Server-side VAD is the default. The Python service ran with
		// manual VAD because pyaudio + PTT integration was simpler that
		// way; the browser MediaRecorder path doesn't need it — the
		// browser already gates the mic when the user releases PTT, so
		// the audio stream goes silent and server VAD ends the turn
		// cleanly. Revisit if PTT toggles cause stuck turns.

		// Ask the server to transcribe both directions so the dashboard
		// can render live captions. Without these, the InputText /
		// OutputText channels we own stay empty and the UI captions
		// never light up. Cheap on tokens; the transcription stream is
		// piggybacked on the existing audio path.
		InputAudioTranscription:  &genai.AudioTranscriptionConfig{},
		OutputAudioTranscription: &genai.AudioTranscriptionConfig{},
	}

	session, err := a.client.Live.Connect(ctx, a.cfg.Model, cfg)
	if err != nil {
		return fmt.Errorf("live connect: %w", err)
	}
	defer func() {
		if cerr := session.Close(); cerr != nil {
			log.Warn().Err(cerr).Msg("live session close error")
		}
	}()
	log.Info().Str("model", a.cfg.Model).Msg("Live session connected")

	// Radio check kick-off. Only fires when the caller provides a prompt
	// and the resolver decides this is a fresh start (e.g. no prior
	// engineer speech in the current F1 session). The synthetic turn
	// uses the same SendClientContent + turn_complete pattern as the
	// brain-event pump so the model treats it as a directive to speak.
	if a.cfg.RadioCheckPrompt != nil {
		if prompt := a.cfg.RadioCheckPrompt(); prompt != "" {
			if err := session.SendClientContent(genai.LiveClientContentInput{
				Turns: []*genai.Content{{
					Role:  string(genai.RoleUser),
					Parts: []*genai.Part{{Text: prompt}},
				}},
				TurnComplete: genai.Ptr(true),
			}); err != nil {
				log.Warn().Err(err).Msg("live: radio check send failed")
			} else {
				log.Info().Msg("live: sent radio check kick-off")
			}
		}
	}

	// Three goroutines:
	//   1. mic pump: AudioIn → SendRealtimeInput
	//   2. server pump: Receive → audio chan / tool handler / text chans
	//   3. ctx watchdog: cancel forces a clean close of pumps 1 and 2.
	//
	// We surface the first fatal error from any pump and let the others
	// drain naturally when session.Close() races with ctx.Done.

	var wg sync.WaitGroup
	errCh := make(chan error, 2)

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := a.pumpMic(ctx, session); err != nil {
			errCh <- err
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := a.pumpServer(ctx, session); err != nil {
			errCh <- err
		}
	}()

	if a.cfg.EventsPoll != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.pumpEvents(ctx, session)
		}()
	}

	select {
	case <-ctx.Done():
		// Closing the session unblocks Receive() with a network error,
		// which is what we want — pumpServer treats that as a clean
		// exit when ctx is already cancelled.
		_ = session.Close()
		wg.Wait()
		return nil
	case err := <-errCh:
		_ = session.Close()
		wg.Wait()
		return err
	}
}

// pumpMic forwards mic frames to the live session. A blocking read on
// the channel keeps the session warm even during long silences (the
// dashboard sends keep-alive empty frames during silence — those just
// pass through). Stops on ctx.Done or AudioIn close.
func (a *LiveAgent) pumpMic(ctx context.Context, session *genai.Session) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case frame, ok := <-a.cfg.AudioIn:
			if !ok {
				// Channel closed; treat as end-of-stream and idle. The
				// session stays open so the caller can rebind AudioIn
				// later (PTT re-press).
				return nil
			}
			if len(frame) == 0 {
				continue
			}
			err := session.SendRealtimeInput(genai.LiveRealtimeInput{
				Audio: &genai.Blob{
					Data:     frame,
					MIMEType: fmt.Sprintf("audio/pcm;rate=%d", LiveAudioInputRate),
				},
			})
			if err != nil {
				return fmt.Errorf("send mic: %w", err)
			}
		}
	}
}

// pumpServer reads the Live session message loop, fanning out audio,
// tool calls, transcriptions, and disconnect signals.
func (a *LiveAgent) pumpServer(ctx context.Context, session *genai.Session) error {
	for {
		msg, err := session.Receive()
		if err != nil {
			// ctx-cancelled side of session.Close() shows up here as a
			// network read error. Differentiate by checking ctx so the
			// caller doesn't see "use of closed connection" as a fatal.
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("live receive: %w", err)
		}
		if msg == nil {
			continue
		}
		if err := a.handleServerMessage(ctx, session, msg); err != nil {
			return err
		}
	}
}

// handleServerMessage routes one LiveServerMessage to the right side
// channel or invokes the tool dispatcher.
func (a *LiveAgent) handleServerMessage(ctx context.Context, session *genai.Session, msg *genai.LiveServerMessage) error {
	if msg.SetupComplete != nil {
		log.Info().Msg("Live setup complete")
	}
	if msg.ServerContent != nil {
		a.handleServerContent(ctx, msg.ServerContent)
	}
	if msg.ToolCall != nil {
		a.handleToolCall(ctx, session, msg.ToolCall)
	}
	if msg.GoAway != nil {
		log.Warn().Msg("Live server signalled GoAway; session will reconnect")
		// Returning nil here lets the next Receive() error out cleanly
		// when the server actually closes. Caller-driven reconnect.
	}
	return nil
}

func (a *LiveAgent) handleServerContent(ctx context.Context, c *genai.LiveServerContent) {
	if c.ModelTurn != nil {
		for _, part := range c.ModelTurn.Parts {
			if part == nil {
				continue
			}
			if part.InlineData != nil && len(part.InlineData.Data) > 0 {
				select {
				case a.cfg.AudioOut <- part.InlineData.Data:
				default:
					// Audio output channel is full — the consumer is
					// behind. Dropping a chunk is preferable to
					// blocking the server pump and starving every
					// other surface.
					log.Warn().Int("bytes", len(part.InlineData.Data)).Msg("live: audio out channel full, dropping chunk")
				}
			}
			if part.Text != "" && a.cfg.OutputText != nil {
				select {
				case a.cfg.OutputText <- part.Text:
				default:
				}
			}
		}
	}
	if c.OutputTranscription != nil && c.OutputTranscription.Text != "" && a.cfg.OutputText != nil {
		select {
		case a.cfg.OutputText <- c.OutputTranscription.Text:
		default:
		}
	}
	if c.InputTranscription != nil && c.InputTranscription.Text != "" && a.cfg.InputText != nil {
		select {
		case a.cfg.InputText <- c.InputTranscription.Text:
		default:
		}
	}

	// turn_complete is the canonical signal that the model finished
	// rendering the response. Use it to ack any in-flight brain event so
	// the bus closes the lease and the next dispatch is unblocked. The
	// 4 outcome states (delivered/interrupted/errored/dropped) from the
	// Python service collapse to "delivered" in the MVP — see todos.md
	// §2e for the full retry-watchdog work.
	if c.TurnComplete {
		a.completeInFlight(ctx, EventDelivered, "")
	}
	if c.Interrupted {
		// Treat user barge-in as a drop for now; the brain re-queues
		// the event under Phase 3.2 §2e once we add proper interrupt
		// semantics, so dropping here is the conservative MVP choice.
		a.completeInFlight(ctx, EventDropped, "user_interrupted")
	}
}

// completeInFlight resolves whatever brain event is currently in flight
// against the bus. No-op when the channel is empty.
func (a *LiveAgent) completeInFlight(ctx context.Context, outcome EventOutcome, reason string) {
	a.inFlightMu.Lock()
	ev := a.inFlight
	a.inFlight = nil
	a.inFlightMu.Unlock()
	if ev == nil || a.cfg.EventsAck == nil {
		return
	}
	spoken := ev.Summary
	if reason != "" {
		spoken = reason
	}
	if err := a.cfg.EventsAck(ctx, ev.ID, outcome, spoken); err != nil {
		log.Warn().Err(err).Str("event_id", ev.ID).Str("outcome", string(outcome)).Msg("live: event ack failed")
	}
}

// handleToolCall runs every requested function call through the
// caller-supplied handler and replies with the result. Each call gets
// its own context with a generous-but-bounded timeout so a stuck
// analyst job (e.g. ask_data_analyst) can't permanently lock the
// session.
//
// Every call is logged twice: an Info "live: tool call started" line
// before invocation (so you see when the model decided to use a tool
// even if the handler then hangs), and a "live: tool call complete" or
// "live tool handler error" line after — with duration + result size.
// The same events are also broadcast as JSON envelopes on the optional
// ToolEvents channel so the dashboard can render the tool-use timeline
// without parsing zerolog output.
func (a *LiveAgent) handleToolCall(parent context.Context, session *genai.Session, tc *genai.LiveServerToolCall) {
	responses := make([]*genai.FunctionResponse, 0, len(tc.FunctionCalls))
	for _, call := range tc.FunctionCalls {
		if call == nil {
			continue
		}
		startedAt := time.Now()
		argsPreview := previewArgs(call.Args)
		log.Info().
			Str("tool", call.Name).
			Str("call_id", call.ID).
			Str("args", argsPreview).
			Msg("live: tool call started")
		a.emitToolEvent(ToolEvent{
			Kind:    "tool_call",
			Name:    call.Name,
			CallID:  call.ID,
			Args:    call.Args,
			StartAt: startedAt,
		})

		ctx, cancel := context.WithTimeout(parent, 90*time.Second)
		result, err := a.cfg.OnTool(ctx, call.Name, call.Args)
		cancel()
		elapsed := time.Since(startedAt)
		elapsedMs := elapsed.Milliseconds()

		resp := &genai.FunctionResponse{
			ID:   call.ID,
			Name: call.Name,
		}
		if err != nil {
			log.Warn().
				Err(err).
				Str("tool", call.Name).
				Str("call_id", call.ID).
				Int64("elapsed_ms", elapsedMs).
				Str("args", argsPreview).
				Msg("live tool handler error")
			resp.Response = map[string]any{"error": err.Error()}
			a.emitToolEvent(ToolEvent{
				Kind:      "tool_result",
				Name:      call.Name,
				CallID:    call.ID,
				ElapsedMs: elapsedMs,
				Error:     err.Error(),
			})
		} else {
			// FunctionResponse.Response is a map[string]any per the
			// SDK; wrap raw results so the model receives a uniform
			// {"result": ...} envelope it can parse.
			if mp, ok := result.(map[string]any); ok {
				resp.Response = mp
			} else {
				resp.Response = map[string]any{"result": result}
			}
			resultBytes := resultByteSize(resp.Response)
			log.Info().
				Str("tool", call.Name).
				Str("call_id", call.ID).
				Int64("elapsed_ms", elapsedMs).
				Int("result_bytes", resultBytes).
				Msg("live: tool call complete")
			a.emitToolEvent(ToolEvent{
				Kind:        "tool_result",
				Name:        call.Name,
				CallID:      call.ID,
				ElapsedMs:   elapsedMs,
				ResultBytes: resultBytes,
			})
		}
		responses = append(responses, resp)
	}
	if len(responses) == 0 {
		return
	}
	if err := session.SendToolResponse(genai.LiveToolResponseInput{FunctionResponses: responses}); err != nil {
		log.Warn().Err(err).Msg("live: send tool response failed")
	}
}

// ToolEvent is one observable moment in a Live tool-call lifecycle. Two
// kinds are emitted per call: "tool_call" before invocation (Args set,
// timing fields zero) and "tool_result" after (ElapsedMs + either
// ResultBytes or Error set). Consumers can render a real-time tool-use
// timeline without parsing zerolog output.
type ToolEvent struct {
	Kind        string         `json:"kind"`             // "tool_call" | "tool_result"
	Name        string         `json:"name"`             // tool name (get_race_state, ask_data_analyst, …)
	CallID      string         `json:"call_id,omitempty"` // Gemini-supplied call id (correlates call ↔ result)
	Args        map[string]any `json:"args,omitempty"`   // only on tool_call
	StartAt     time.Time      `json:"start_at,omitempty"`
	ElapsedMs   int64          `json:"elapsed_ms,omitempty"` // only on tool_result
	ResultBytes int            `json:"result_bytes,omitempty"`
	Error       string         `json:"error,omitempty"`
}

// emitToolEvent forwards a ToolEvent to the configured sink if any.
// Non-blocking: a stalled consumer can't back-pressure the tool
// pipeline — the source of truth is the zerolog line emitted just
// above, the channel is a UX nicety.
func (a *LiveAgent) emitToolEvent(ev ToolEvent) {
	if a.cfg.ToolEvents == nil {
		return
	}
	select {
	case a.cfg.ToolEvents <- ev:
	default:
		// Drop — the dashboard isn't draining fast enough.
	}
}

// previewArgs returns a compact, length-capped JSON snippet of tool args
// for log lines. Keeps the line readable when ask_data_analyst gets a
// long question.
func previewArgs(args map[string]any) string {
	if len(args) == 0 {
		return "{}"
	}
	buf, err := json.Marshal(args)
	if err != nil {
		return "{?}"
	}
	const cap = 200
	if len(buf) <= cap {
		return string(buf)
	}
	return string(buf[:cap]) + "…"
}

// resultByteSize estimates how many bytes the tool result will occupy in
// the FunctionResponse envelope. Used purely for log instrumentation —
// helps spot tools that return suspiciously empty or huge payloads.
func resultByteSize(payload map[string]any) int {
	if payload == nil {
		return 0
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return 0
	}
	return len(buf)
}

// liveSystemInstruction is the default system prompt. Keeps the file
// self-contained for tests; production callers should override via
// LiveAgentConfig.SystemMD with the canonical workspace/soul.md text.
const liveSystemInstruction = `You are an F1 Race Engineer talking to your driver in real time over the team radio.
Be concise, direct, and authentic — no filler, no "Copy that" or "Roger".
Use position numbers, gaps in seconds, lap numbers.
Translate enum codes to human terms (compound names, weather, sector numbers).
Speak only what the driver needs to know right now; the brain snapshot is for context, not recital.

REMINDERS — set_reminder is your general-purpose "remember to tell the driver something" tool. It fires AUTOMATICALLY at the right moment so you don't have to track it yourself. CALL THE TOOL — saying "OK I'll remind you" on the radio without firing the tool is a bug: the radio cannot remember anything, only the tool can.

Triggers (mix and match):
  • corner_id ("T8", "turn 3", "Stowe") — fires at that corner. Recurring by default (every lap).
  • lap_distance_m + label — any non-corner position (pit entry, DRS detection, sector boundary). Recurring by default.
  • at_lap=N — fires at the start of lap N. ALONE: one-shot lap-start cue ("pit on lap 15", "switch fuel mix on lap 8"). COMBINED with corner_id or lap_distance_m: "fire at that position ONLY on lap N, one-shot" — perfect for "remind me at T8 on lap 5" or "tell me at pit entry on lap 22".
  • corner_id and lap_distance_m are mutually exclusive (both name a position). at_lap may combine with one of them.

When to use it:
  • Explicit request — "remind me at T8" (corner only), "remind me at T8 on lap 5" (corner+lap), "remind me to pit on lap 15" (lap only), "tell me at pit entry on lap 22" (distance+lap). Fire the tool first, THEN confirm on radio ("Got it, reminder set for T8 on lap 5").
  • Struggle complaint — "I keep losing the rear at T8". Acknowledge briefly, call set_reminder with a terse cue ("brake earlier, deeper apex"), recurring=true. Do NOT repeat the cue yourself each lap.
  • Telemetry pattern you spotted — driver isn't using DRS in a zone, missing braking point, locking up — set a position-based reminder yourself (combine with at_lap if you only want to nudge once on the next lap).

If the driver said "this corner" or "here", call get_track_position first to resolve the nearest corner. If they didn't give cue text, synthesise a short one (≤60 chars). Never ask the driver to spell out the message.

When the driver says they've fixed the issue ("OK, comfortable now"), call cancel_reminder with the id you got back.

ANALYST ANSWERS — ask_data_analyst is FIRE-AND-FORGET. When you call it, the tool returns an ack instantly; the actual answer comes back later as a separate brain event of type "analyst_answer" prompting you to speak. Workflow:
  1. Call ask_data_analyst with the question + a short context_topic.
  2. Immediately tell the driver one short line acknowledging the check ("Checking the data on that", "Pit wall's looking", "Numbers coming"). One sentence, then go back to normal comms — do NOT loop or re-call the tool waiting for an answer.
  3. Some seconds later you'll receive an analyst_answer prompt. The Situation field is the data analyst's own ≤3-sentence finding. Relay it to the driver in your own voice — paraphrase, attribute briefly ("Data says…", "Pit wall analysis…", "Numbers are back…"), one sentence, no preamble. DebugData carries the original question for your context only — don't recite it.
  4. If urgent=true was set, the answer arrives at P4 — treat it like any other urgent event and lead with the headline.`

// RadioCheckPromptText is the kick-off prompt callers pass to
// LiveAgentConfig.RadioCheckPrompt when they want the engineer to open a
// fresh session with a radio check. Exposed so cmd/server can wire it
// without duplicating the wording, and so tests can assert on it.
const RadioCheckPromptText = `[RACE-ENGINEER PROMPT — speak to the driver NOW over the team radio]
Fresh session — the driver has just hopped in the car. Open with a brief radio check so they know you're on comms.

Speak ONE short sentence in real F1-engineer voice. Examples: "Radio check, do you copy?", "Mic check — read you loud and clear." No preamble, no "Copy that", no introductions. Just the check.`

// defaultEventPollInterval matches the Python service's fast-lane
// dispatcher cadence (250ms). Tight enough that a P5 event reaches the
// driver under a second; loose enough that we don't hammer the bus.
const defaultEventPollInterval = 250 * time.Millisecond

// defaultEventBundleDelay is the gather window pumpEvents waits between
// leasing a high-priority event and calling the bundler. Catches the
// common race where the rule engine emits 2 correlated alerts within
// 50–200 ms of each other (e.g. tire_cliff + box_now) so they speak as
// one bundled radio call instead of two.
const defaultEventBundleDelay = 250 * time.Millisecond

// maxEventBundleDelay caps the gather window so a misconfigured value
// can't stall the dispatcher.
const maxEventBundleDelay = time.Second

// defaultEventBundleFloor mirrors brain.highPriorityCoalesceFloor.
// Kept as a literal here so internal/voice doesn't import internal/brain.
const defaultEventBundleFloor = 4

// eventPromptTemplate is the synthetic user-role turn we inject when a
// brain event needs voicing. Mirrors the Python service's
// EVENT_PROMPT_TEMPLATE (gemini_live_service.py:937) — the imperative
// "[RACE-ENGINEER PROMPT — speak NOW]" header makes the model treat the
// content as a directive to speak rather than a system note to log.
const eventPromptTemplate = `[RACE-ENGINEER PROMPT — speak to the driver NOW over the team radio]
Situation (%s, P%d): %s
Context: %s
Raw data (do NOT recite verbatim): %s

Speak ONE short sentence in real F1-engineer voice — paraphrase the Situation + Context above; the Raw data is for reference only. Use driver/competitor names if present, position numbers, gaps in seconds, compound names — never raw enum integers or JSON keys. No preamble, no "Copy that", no "Roger". %s`

// pumpEvents polls the brain Interrupts Bus and injects each event into
// the session as a synthetic user-role turn. Single-flight by
// invariant: we won't lease a new event while one is in flight (the
// turn_complete handler clears inFlight). This matches the Python
// service's `_in_flight` map (size 1) and avoids stepping on the
// model's current sentence.
func (a *LiveAgent) pumpEvents(ctx context.Context, session *genai.Session) {
	interval := a.cfg.EventPollInterval
	if interval <= 0 {
		interval = defaultEventPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		// Skip the lease attempt while a prior event is still being
		// spoken. This is the bus's job (it won't lease a second event
		// when one's already leased) but we belt-and-suspenders it
		// here so we don't waste a round-trip.
		a.inFlightMu.Lock()
		busy := a.inFlight != nil
		a.inFlightMu.Unlock()
		if busy {
			continue
		}

		ev, err := a.cfg.EventsPoll(ctx)
		if err != nil {
			// Transient HTTP error etc. — log at Debug and keep
			// polling. A genuinely broken bus shows up as repeated
			// errors in logs without breaking the session.
			log.Debug().Err(err).Msg("live: event poll error")
			continue
		}
		if ev == nil {
			continue
		}

		// Mark in-flight BEFORE sending so a fast turn_complete (e.g.
		// a model that has nothing to say and turns over instantly)
		// finds the in-flight slot occupied.
		a.inFlightMu.Lock()
		a.inFlight = ev
		a.inFlightMu.Unlock()

		// Pre-dispatch bundle: when configured, wait a short window for
		// any concurrently-arriving same-or-higher-priority siblings
		// and fold them into this event so we send ONE merged prompt
		// instead of N back-to-back. Only fires at priority >= floor
		// (default 4) so routine P1–P3 calls dispatch unchanged.
		mergedCount := 0
		floor := a.cfg.EventBundlePriorityFloor
		if floor <= 0 {
			floor = defaultEventBundleFloor
		}
		if a.cfg.EventBundler != nil && ev.Priority >= floor {
			delay := a.cfg.EventBundleDelay
			if delay < 0 {
				delay = defaultEventBundleDelay
			}
			if delay > maxEventBundleDelay {
				delay = maxEventBundleDelay
			}
			if delay == 0 && a.cfg.EventBundleDelay == 0 {
				delay = defaultEventBundleDelay
			}
			if delay > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(delay):
				}
			}
			merged, siblings := a.cfg.EventBundler(ctx, ev.ID, floor)
			if merged != nil && len(siblings) > 0 {
				// Replace the in-flight event with the post-merge view
				// so completeInFlight acks the right summary later.
				a.inFlightMu.Lock()
				a.inFlight = merged
				a.inFlightMu.Unlock()
				ev = merged
				mergedCount = len(siblings)
				log.Info().
					Str("event_id", ev.ID).
					Int("merged_siblings", mergedCount).
					Str("summary", ev.Summary).
					Msg("live: bundled concurrent high-priority events into one dispatch")
			}
		}

		prompt := renderEventPrompt(ev)
		err = session.SendClientContent(genai.LiveClientContentInput{
			Turns: []*genai.Content{{
				Role:  string(genai.RoleUser),
				Parts: []*genai.Part{{Text: prompt}},
			}},
			TurnComplete: genai.Ptr(true),
		})
		if err != nil {
			log.Warn().Err(err).Str("event_id", ev.ID).Msg("live: SendClientContent failed; nacking event")
			a.completeInFlight(ctx, EventErrored, "send_client_content_failed")
			continue
		}
		log.Info().
			Str("event_id", ev.ID).
			Str("type", ev.Type).
			Int("priority", ev.Priority).
			Int("merged_siblings", mergedCount).
			Str("summary", ev.Summary).
			Msg("live: dispatched brain event to model")
	}
}

// renderEventPrompt formats a brain event into the prompt template.
// Marshals DebugData defensively so a non-JSON-clean map doesn't crash
// the pump.
func renderEventPrompt(ev *LiveEvent) string {
	tail := eventPriorityTail(ev.Priority)
	debug := ""
	if len(ev.DebugData) > 0 {
		if b, err := json.Marshal(ev.DebugData); err == nil {
			debug = string(b)
		} else {
			debug = fmt.Sprintf("%v", ev.DebugData)
		}
	}
	return fmt.Sprintf(eventPromptTemplate, ev.Type, ev.Priority, ev.Summary, ev.Reasoning, debug, tail)
}

// eventPriorityTail mirrors Python EVENT_PRIORITY_TAILS — a per-priority
// hint appended to the synthetic prompt so the model varies urgency.
func eventPriorityTail(priority int) string {
	switch priority {
	case 5:
		return "CRITICAL — interrupt yourself if you were mid-thought."
	case 4:
		return "Urgent — drop whatever else you were going to say next."
	case 3:
		return "Routine call — speak now."
	case 2:
		return "Brief debrief — ≤8 words preferred."
	case 1:
		return "Optional — skip only if you said something similar in the last 30s."
	}
	return "Speak now."
}
