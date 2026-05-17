package main

import (
	"context"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/analyst"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/api"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/audio"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/brain"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/config"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/ingestion"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/insights"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/intelligence"
	mcpx "github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/mcp"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/plan"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/storage"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/trackmap"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/transcript"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/voice"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/workspace"

	"path/filepath"
)

func main() {
	// Pretty console logging for development.
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: "15:04:05"})
	log.Info().Msg("=== Telemetry Core Service ===")

	// ── 1. Configuration ─────────────────────────────────────────────────
	cfg := config.Load()

	// Remove any leftover restart sentinel from a previous run before we
	// start serving requests. The supervisor only treats the sentinel as
	// meaningful when it's present at the moment of child exit — but if a
	// process was sigkilled mid-restart we don't want a stale file to
	// trigger another bounce on the very next clean shutdown.
	api.ClearRestartSentinel(cfg.WorkspaceDir)

	// Apply log level from config (default: info).
	level, err := zerolog.ParseLevel(cfg.LogLevel)
	if err != nil {
		level = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(level)
	log.Info().Str("log_level", level.String()).Msg("Log level set")

	// ── 2. Root context + WaitGroup ──────────────────────────────────────
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup

	// ── 3. DuckDB Storage ────────────────────────────────────────────────
	store, err := storage.NewStorage(cfg.DBPath, cfg.BatchSize)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to initialise DuckDB storage")
	}
	defer func() {
		log.Info().Msg("Closing DuckDB storage…")
		if err := store.Close(); err != nil {
			log.Error().Err(err).Msg("Error closing storage")
		}
	}()

	// ── 4. Channel Topology ──────────────────────────────────────────────
	//
	//   insightEngine (5s) ──→ rawInsightChan ──→ CommsGate (15s batch / immediate urgent) ──→ translatedChan ──→ FANOUT
	//   analyst (15s, dual-mode) ──→ rawInsightChan ──↗                                        ├→ dashboardChan
	//   POST /api/strategy ──→ analyst.HandleStrategyWebhook → rawInsightChan ──↗               ├→ voiceChan → synthesize → WS audio
	//   POST /api/driver_query → gate.HandleQuery() → translatedChan                            └→ workspace
	//   POST /api/voice → /transcribe → gate.HandleQuery() → translatedChan
	//
	insightLog := insights.NewLog(500)
	rawInsightChan := make(chan models.DrivingInsight, 100)
	translatedChan := make(chan models.DrivingInsight, 100)
	dashboardChan := make(chan models.DrivingInsight, 100)
	voiceChan := make(chan models.DrivingInsight, 100)
	wsChan := make(chan models.DrivingInsight, 100)

	// ── 5. LLM Provider ─────────────────────────────────────────────────
	// Resolve API key: provider-specific key takes priority, fallback to GEMINI_API_KEY.
	llmKey := cfg.GeminiAPIKey
	switch cfg.LLMProvider {
	case "anthropic", "claude":
		if cfg.AnthropicAPIKey != "" {
			llmKey = cfg.AnthropicAPIKey
		}
	case "openai":
		if cfg.OpenAIAPIKey != "" {
			llmKey = cfg.OpenAIAPIKey
		}
	}

	provider, err := intelligence.NewProvider(cfg.LLMProvider, llmKey, cfg.LLMModel)
	if err != nil {
		log.Fatal().Err(err).Str("provider", cfg.LLMProvider).Msg("Failed to create LLM provider")
	}
	defer provider.Close()

	// ── 5b. RaceBrain (shared blackboard) ────────────────────────────────
	// Specialist agents and real-time streams write observations/events here.
	// CommsGate (the conversational engineer) will eventually read snapshots
	// from this brain at decision time. For now, this is a passive recorder
	// layer — readers are unchanged so behavior stays identical.
	raceBrain := brain.New(
		func() *models.RaceState { return store.Cache().Load() },
		brain.StaticContext{
			Soul:          intelligence.LoadFileWithFallback(cfg.WorkspaceDir, "soul.md", "SOUL.md", ""),
			User:          intelligence.LoadFileWithFallback(cfg.WorkspaceDir, "user.md", "USER.md", ""),
			DriverProfile: intelligence.LoadFileWithFallback(cfg.WorkspaceDir, "driver_profile.md", "driver_profile.md", ""),
			TrackSetup:    intelligence.LoadFileWithFallback(cfg.WorkspaceDir, "track_setup.md", "track_setup.md", ""),
			PastLearnings: intelligence.LoadFileWithFallback(cfg.WorkspaceDir, "past_learnings.md", "past_learnings.md", ""),
		},
	)

	// Lease reaper — nacks events that sit in_flight past LeaseTimeout so a
	// crashed dispatcher (or a stuck Live session) can't strand events in
	// the queue. Single goroutine; ticks every second.
	wg.Add(1)
	go func() {
		defer wg.Done()
		raceBrain.RunReaper(ctx, time.Second)
	}()

	// ── 5d. Transcript Hub ───────────────────────────────────────────────
	// Per-session interaction log shared by every LLM-touching surface
	// (CommsGate, analyst, voice handler, Gemini Live). Writes JSONL to
	// workspace/sessions/<session_id>.jsonl, rotates on F1 session_uid
	// change. See internal/transcript for the full lifecycle.
	transcriptCfg := transcript.DefaultConfig()
	transcriptCfg.Dir = filepath.Join(cfg.WorkspaceDir, "sessions")
	transcriptCfg.PromptLines = cfg.TranscriptPromptLines
	transcriptCfg.ToolLimit = cfg.TranscriptToolLimit
	transcriptCfg.Retention = cfg.TranscriptRetention
	transcriptHub, err := transcript.New(transcriptCfg, func() uint64 {
		st := store.Cache().Load()
		if st == nil {
			return 0
		}
		return st.SessionUID
	})
	if err != nil {
		log.Fatal().Err(err).Msg("transcript hub init failed")
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		transcriptHub.Run(ctx)
	}()

	// Mirror every zerolog line into the transcript hub so the Live Debug
	// tab sees what the terminal sees. The console writer keeps its
	// existing stderr destination via MultiLevelWriter — log lines are
	// fanned out to both, not redirected.
	log.Logger = log.Output(zerolog.MultiLevelWriter(
		zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: "15:04:05"},
		transcript.NewLogWriter(transcriptHub),
	))

	// Wire the transcript hub to the brain so EnqueueEvent/AckDelivered/
	// NackInterrupted-drop produce event_dispatched/delivered/dropped
	// records on the per-session log.
	raceBrain.SetTranscriptHook(brainTranscriptAdapter{hub: transcriptHub})

	// ── 6. Workspace Writer ──────────────────────────────────────────────
	ws := workspace.NewWriter(
		cfg.WorkspaceDir,
		func() *models.RaceState { return store.Cache().Load() },
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		ws.Run(ctx)
	}()

	// ── 7. Insight Engine (reads from atomic RaceState cache) ────────────
	// Engine now sends to rawInsightChan (instead of directly to dashboard).
	engine := insights.NewEngine(
		func() *models.RaceState { return store.Cache().Load() },
		func() int32 { return cfg.TalkLevel.Load() },
		rawInsightChan,
	)
	engine.SetOnInsight(func(i models.DrivingInsight) {
		ws.RecordInsight(i)
		insightLog.Record("engine", i)
		if transcriptHub != nil {
			transcriptHub.Append(transcript.Event{
				Kind:  transcript.KindInsightPushed,
				Actor: "rule_engine",
				Text:  i.Message,
				Meta: map[string]any{
					"priority": i.Priority,
					"type":     i.Type,
				},
			})
		}
	})
	// Wire the Interrupts Bus + DB reader so the engine can fire typed Events
	// (lap_summary etc.) on top of the legacy DrivingInsight stream.
	engine.SetBrain(raceBrain)
	engine.SetReader(store.Reader())
	engine.SetRoster(store.Roster())

	// Data Analyst triggers are constructed below in section 13. We wire
	// the engine hooks deferred via a closure so the order of construction
	// doesn't force section reshuffling. The analyst spawns opencode
	// asynchronously — until it reports Ready, OnLapComplete /
	// OnSignificantEvent are no-ops.
	var dataAnalyst *analyst.Runtime
	engine.SetLapCompleteHook(func(lap int) {
		if dataAnalyst != nil {
			dataAnalyst.OnLapComplete(lap)
		}
	})
	engine.SetSignificantEventHook(func(code, detail string, meta map[string]any) {
		if dataAnalyst != nil {
			dataAnalyst.OnSignificantEvent(code, detail, meta)
		}
	})

	wg.Add(1)
	go func() {
		defer wg.Done()
		engine.Run(ctx)
	}()

	// ── 7b. Corner-reminder store + 200ms watcher ──────────────────────
	// Driver-set track-position cues. The watcher polls the atomic cache
	// at 200ms (vs the engine's 5s) because corner reminders need ~16m
	// precision at 290 km/h. Store is in-memory and session-scoped.
	reminderStore := insights.NewReminderStore()
	cornerWatcher := insights.NewCornerWatcher(
		reminderStore,
		func() *models.RaceState { return store.Cache().Load() },
		raceBrain,
		func() int32 { return cfg.TalkLevel.Load() },
		0, // 200ms default
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		cornerWatcher.Run(ctx)
	}()

	// ── 7c. Proximity watcher — "car closing from behind" heads-up ──────
	// 1 s tick (vs the engine's 5 s) because at 5 s a fast-closing car
	// covers the whole 250 m watch window in a single tick — the trend
	// math never accumulates samples. Uses on-track lap_distance via
	// GridPositions, so it works in any session type (practice / quali /
	// race / TT). Stays silent only in pit lane / pit area / garage.
	proximityWatcher := insights.NewProximityWatcher(
		func() *models.RaceState { return store.Cache().Load() },
		raceBrain,
		func() int32 { return cfg.TalkLevel.Load() },
		0, // 1s default
	)
	proximityWatcher.SetRoster(store.Roster())
	wg.Add(1)
	go func() {
		defer wg.Done()
		proximityWatcher.Run(ctx)
	}()

	// ── 7d. Track geometry registry ────────────────────────────────────
	// Hand-curated corner + pit-entry data from <workspace>/tracks/
	// <track_id>.json. Loaded here (rather than in section 14 below) so
	// the plan ticker can consult pit-entry coordinates for spatial
	// triggers. Unmapped tracks fall back to lap-distance reminders.
	trackReg := trackmap.NewRegistry()
	if n, err := trackReg.Load(cfg.WorkspaceDir + "/tracks"); err != nil {
		log.Warn().Err(err).Msg("track geometry: load failed, continuing without corner data")
	} else {
		log.Info().Int("tracks_loaded", n).Msg("track geometry: ready")
	}

	// ── 7e. Session plan store + ticker ────────────────────────────────
	// The plan store holds the collaborative session plan (pit stops,
	// compound changes, reminders). It auto-rotates on F1 session_uid
	// change, mirroring the transcript hub. The ticker watches live
	// telemetry and emits brain events for active plan entries whose
	// triggers fire (lap reached, lap-window opened, spatial pit entry
	// approaching).
	planCfg := plan.DefaultConfig()
	planCfg.Dir = filepath.Join(cfg.WorkspaceDir, "sessions")
	planStore, err := plan.NewStore(planCfg, func() uint64 {
		st := store.Cache().Load()
		if st == nil {
			return 0
		}
		return st.SessionUID
	})
	if err != nil {
		log.Fatal().Err(err).Msg("plan store init failed")
	}
	planTicker := plan.NewTicker(
		planStore,
		func() *models.RaceState { return store.Cache().Load() },
		raceBrain,
		func() int32 { return cfg.TalkLevel.Load() },
		trackReg,
		0, // 500ms default — spatial triggers need sub-second precision
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		planTicker.Run(ctx)
	}()

	// ── 8. CommsGate (rawInsightChan → translatedChan) ──────────────────
	// Reads telemetry, observations, recent radio, and static workspace context
	// from the shared RaceBrain. The gate carries no state of its own.
	// CommsGate: proactive rule-engine path is silenced when Gemini Live owns
	// the voice surface — the Interrupts Bus + bus producers handle that now.
	// Driver queries (POST /api/driver_query, POST /api/voice) still flow
	// through CommsGate.HandleQuery in either mode.
	gate := intelligence.NewCommsGate(
		provider,
		raceBrain,
		func() int32 { return cfg.Verbosity.Load() },
		rawInsightChan,
		translatedChan,
	)
	gate.SetSilentRulePath(cfg.GeminiLiveEnabled)
	gate.SetTranscript(transcriptHub)
	wg.Add(1)
	go func() {
		defer wg.Done()
		gate.Run(ctx)
	}()

	// ── 9. Fanout (translatedChan → dashboard + voice + WS + workspace) ──
	wg.Add(1)
	go func() {
		defer wg.Done()
		fanout(ctx, translatedChan, dashboardChan, voiceChan, wsChan, ws, insightLog, raceBrain)
	}()

	// ── 10. Ingestion Pipeline (3 self-healing goroutines) ───────────────
	ingester := ingestion.NewIngester(cfg, store)
	ingester.SetTrackMap(trackReg)
	ingester.SetEventHook(func(code, detail string, vehicleIdx int) {
		raceBrain.RecordEvent(brain.RaceEvent{
			Code:       code,
			Detail:     detail,
			VehicleIdx: vehicleIdx,
		})
	})
	ingester.Start(ctx, &wg) // adds 3 to wg internally

	// ── 11. WebSocket Hub ───────────────────────────────────────────────
	// Hub must be created before VoiceClient since VoiceClient broadcasts audio through it.
	startedAt := time.Now()
	wsHub := api.NewHub(
		cfg.WSPushRate,
		func() *models.RaceState { return store.Cache().Load() },
		func() interface{} {
			dbOK := true
			if _, err := store.Reader().Exec("SELECT 1"); err != nil {
				dbOK = false
			}
			return map[string]interface{}{
				"status":     "ok",
				"uptime":     time.Since(startedAt).Truncate(time.Second).String(),
				"packets_rx": ingester.PacketsReceived(),
				"duckdb_ok":  dbOK,
				"mock_mode":  cfg.MockMode.Load(),
				"talk_level": cfg.TalkLevel.Load(),
				"udp_host":   cfg.RuntimeUDPHost(),
				"udp_port":   cfg.RuntimeUDPPort(),
				"udp_mode":   cfg.RuntimeUDPMode(),
			}
		},
		// Per-tick track_position dynamic payload for dashboard LiveMap.
		// Geometry (corners/length/sector_starts) still comes one-shot via
		// REST /api/state/track_position; this push covers only the player
		// + 22-car grid + headline that changes every UDP packet.
		func(s *models.RaceState) any {
			var track *trackmap.TrackInfo
			if trackReg != nil {
				track = trackReg.Lookup(s.TrackID)
			}
			return api.BuildTrackPositionDynamic(s, track, store.Roster())
		},
	)
	wsHub.SetTranscript(transcriptHub)

	// Start Hub broadcast loops.
	wg.Add(1)
	go func() {
		defer wg.Done()
		wsHub.Run(ctx)
	}()

	// Drain wsChan → broadcast to WebSocket clients.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case insight, ok := <-wsChan:
				if !ok {
					return
				}
				wsHub.BroadcastInsight(insight)
			}
		}
	}()

	// Drain PTTChan → broadcast push-to-talk state to WebSocket clients.
	// Only spawned when the legacy PTT path is enabled: in live_only mode
	// the ingestion writer never pushes to PTTChan and the dashboard
	// ignores `ptt` WebSocket frames anyway, so the drain goroutine has
	// nothing to do.
	if cfg.PTTPathEnabled() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case active, ok := <-ingester.PTTChan:
					if !ok {
						return
					}
					wsHub.BroadcastPTT(active)
				}
			}
		}()
	}

	// ── 12. Voice — in-process Gemini TTS + STT (Phase 3.1) ─────────────
	// Boot a single SDK client for each direction. Errors are non-fatal:
	// when GEMINI_API_KEY is missing the providers come up disabled and
	// the dashboard's voice features quietly no-op rather than crashing
	// the whole server (TTS/STT is one feature among many).
	geminiTTS, err := voice.NewGeminiTTS(ctx, voice.GeminiTTSConfig{
		APIKey: cfg.GeminiAPIKey,
		Voice:  cfg.TTSVoice,
	})
	if err != nil {
		log.Warn().Err(err).Msg("voice: TTS init failed; insights will have no audio")
	}
	// STT only needed when the legacy push-to-talk path is wired
	// (voice_mode != "live_only"). Skipping the init in live_only mode
	// drops an idle SDK client + ~tens of MB of resident memory.
	var geminiSTT voice.Transcriber
	if cfg.PTTPathEnabled() {
		stt, err := voice.NewGeminiSTT(ctx, voice.GeminiSTTConfig{APIKey: cfg.GeminiAPIKey})
		if err != nil {
			log.Warn().Err(err).Msg("voice: STT init failed; /api/voice will 503")
		}
		geminiSTT = stt
	} else {
		log.Info().Str("voice_mode", cfg.VoiceMode).Msg("voice: STT skipped (live_only mode)")
	}
	ackCache := voice.NewAckCache(geminiTTS)

	// audioArbiter is the in-process voice output gate. Every audio
	// producer (Live agent, VoiceClient TTS, ack phrases) acquires
	// before speaking so the user never hears two voices at once.
	// Today the only active producer is the Live agent; the arbiter is
	// the runtime invariant that prevents future regressions.
	audioArbiter := voice.NewOutputArbiter()

	// audioDucker (optional) lowers external music players (Spotify,
	// Apple Music, browsers, VLC, ...) while the engineer is speaking.
	// Per-OS native code lives in internal/audio. Disabled by default
	// — opt in via AUDIO_DUCKING_ENABLED.
	var audioDucker *audio.Coordinator
	if cfg.AudioDuckingEnabled {
		targets := parseAudioTargets(cfg.AudioDuckingTargets)
		mode := audio.ParseMode(cfg.AudioDuckingMode)
		d, derr := audio.NewOSDucker(targets, cfg.AudioDuckingLevel, mode)
		if derr != nil {
			log.Warn().Err(derr).Msg("audio ducker init failed; ducking disabled")
		} else {
			audioDucker = audio.NewCoordinator(d)
			log.Info().
				Str("mode", cfg.AudioDuckingMode).
				Float64("level", cfg.AudioDuckingLevel).
				Int("tail_ms", cfg.AudioDuckingTailMs).
				Msg("audio ducking enabled")
		}
	}

	voiceClient := intelligence.NewVoiceClient(geminiTTS, ackCache, voiceChan, wsHub)
	voiceClient.SetArbiter(audioArbiter)
	voiceClient.SetDucker(audioDucker)
	if cfg.GeminiLiveEnabled {
		// Gemini Live owns the voice surface; standard TTS is intentionally
		// disabled in that mode. Drain the channel so producers don't block.
		// Dashboard transcript still works — radio messages reach it via
		// the WebSocket "insight" message type.
		log.Info().Msg("Voice client disabled — Gemini Live owns the voice surface")
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case _, ok := <-voiceChan:
					if !ok {
						return
					}
					// drop on the floor
				}
			}
		}()
	} else {
		wg.Add(1)
		go func() {
			defer wg.Done()
			voiceClient.Run(ctx)
		}()
	}

	// ── 12b. Gemini Live agent factory (Phase 3.2) ──────────────────────
	// Build the per-WebSocket factory that wires Gemini Live into a
	// bidirectional audio bridge. The factory takes the channels the
	// /api/voice/live handler owns (audio in/out, transcription text)
	// and returns a runner the handler invokes for the duration of the
	// WebSocket connection. One Gemini Live session per browser
	// connection — matches the Python service's session-per-process
	// model without that process.
	//
	// In voice_mode=ptt_only the factory is left nil so /api/voice/live
	// returns "live agent factory not wired" — the dashboard's Live tab
	// becomes inert in that mode, the PTT path handles everything.
	//
	// GEMINI_LIVE_PYTHON_BACKEND=true forces the legacy Python service
	// (gemini_live_service.py) and disables this in-process path so the
	// two Live agents are never alive at once — they'd both connect to
	// Gemini and the user would hear two simultaneous voices.
	pythonLiveBackend := strings.EqualFold(os.Getenv("GEMINI_LIVE_PYTHON_BACKEND"), "true")
	var liveDeps *api.LiveDeps
	if cfg.LivePathEnabled() && !pythonLiveBackend {
		apiBase := "http://localhost:" + cfg.APIPort
		// radioCheckOnFreshSession returns the kick-off prompt only when no
		// engineer_speech has been logged in the current F1 session yet.
		// On a tab reload mid-session the hub still holds prior speech,
		// so this returns "" and the agent stays quiet until the driver
		// talks. On a fresh F1 session_uid the hub rotates to an empty
		// file and we return the prompt so the engineer opens with a
		// radio check.
		radioCheckOnFreshSession := func() string {
			if transcriptHub == nil {
				return voice.RadioCheckPromptText
			}
			prior, err := transcriptHub.Load("current", 1, []transcript.Kind{transcript.KindEngineerSpeech}, 0)
			if err != nil {
				log.Debug().Err(err).Msg("live: radio-check hub lookup failed; defaulting to greet")
				return voice.RadioCheckPromptText
			}
			if len(prior) > 0 {
				return ""
			}
			return voice.RadioCheckPromptText
		}
		// In-process bundler: after the live pump leases a P>=floor event,
		// it waits EventBundleDelay and asks the brain to fold any
		// concurrently-queued P>=floor siblings into the in-flight target.
		// The model then dispatches ONE merged turn instead of N
		// back-to-back ones. See live.go pumpEvents for the call site and
		// brain.MergePendingInto for the merge semantics.
		brainRef := raceBrain
		eventBundler := func(_ context.Context, targetID string, floor int) (*voice.LiveEvent, []string) {
			if brainRef == nil {
				return nil, nil
			}
			merged, siblings := brainRef.MergePendingInto(targetID, floor)
			if merged == nil || len(siblings) == 0 {
				return nil, nil
			}
			return &voice.LiveEvent{
				ID:        merged.ID,
				Type:      string(merged.Type),
				Priority:  merged.Priority,
				Summary:   merged.Summary,
				Reasoning: merged.Reasoning,
				DebugData: merged.DebugData,
			}, siblings
		}
		liveAgentFactory := func(audioIn <-chan []byte, audioOut chan<- []byte, inputText, outputText chan<- string, toolEvents chan<- voice.ToolEvent) (api.LiveAgentRunner, error) {
			agent, aerr := voice.NewLiveAgent(ctx, voice.LiveAgentConfig{
				APIKey:   cfg.GeminiAPIKey,
				Model:    cfg.GeminiLiveModel,
				Voice:    cfg.TTSVoice,
				SystemMD: intelligence.LoadFileWithFallback(cfg.WorkspaceDir, "soul.md", "SOUL.md", ""),
				Tools:    voice.DefaultLiveTools(),
				OnTool:   voice.HTTPToolHandler(apiBase, time.Duration(cfg.GeminiLiveAnalystTimeout)*time.Second),
				// Wire the brain Interrupts Bus into the Live session so
				// proactive events (lap_summary, threat_overtake, …) reach
				// the engineer's voice. Without these the bus fires but
				// nothing speaks.
				EventsPoll:       voice.HTTPEventsPoller(apiBase, 1),
				EventsAck:        voice.HTTPEventsAcker(apiBase),
				EventBundler:     eventBundler,
				EventBundleDelay: time.Duration(cfg.EventBundleDelayMs) * time.Millisecond,
				AudioIn:          audioIn,
				AudioOut:         audioOut,
				InputText:        inputText,
				OutputText:       outputText,
				ToolEvents:       toolEvents,
				RadioCheckPrompt: radioCheckOnFreshSession,
				DuckChunk: func(chunkDur, tail time.Duration) {
					if audioDucker != nil {
						audioDucker.SpeakingChunk(chunkDur, tail)
					}
				},
				DuckTail: time.Duration(cfg.AudioDuckingTailMs) * time.Millisecond,
				OnDriverUtterance: func(text string) {
					if raceBrain != nil {
						raceBrain.AppendDriverUtterance(text)
					}
				},
			})
			if agent != nil {
				agent.SetArbiter(audioArbiter)
			}
			return agent, aerr
		}
		liveDeps = &api.LiveDeps{AgentFactory: liveAgentFactory, Transcript: transcriptHub}
	} else if pythonLiveBackend {
		log.Info().Msg("voice: Go in-process Live disabled — GEMINI_LIVE_PYTHON_BACKEND=true, legacy Python service owns the channel")
	} else {
		log.Info().Str("voice_mode", cfg.VoiceMode).Msg("voice: Gemini Live factory skipped (ptt_only mode)")
	}

	// ── 13. MCP server (shared by Gemini Live + Data Analyst) ──────────
	// The MCP surface stays the same — 24 read-only data tools plus the
	// gated push_insight writer. Both the in-Go Gemini Live agent and the
	// bundled opencode Data Analyst connect to /mcp.
	mcpActivity := mcpx.NewActivityHub(256)
	mcpServer := mcpx.NewServer(&mcpx.Deps{
		Brain:       raceBrain,
		Reader:      store.Reader(),
		InsightLog:  insightLog,
		Cache:       func() *models.RaceState { return store.Cache().Load() },
		Activity:    mcpActivity,
		APIBase:     "http://localhost:" + cfg.APIPort,
		MaxPriority: 3,
	})
	log.Info().Msg("MCP server ready")

	// ── 13b. Data Analyst — opencode (ACP) ──────────────────────────────
	// Long-lived `opencode acp` subprocess. Drives prompts on lap-complete
	// / significant-event / dashboard query triggers, reaches back through
	// MCP for telemetry data. Failure to start disables the analyst but
	// does not block the server (rule-engine path still feeds the radio).
	switch {
	case !cfg.DAEnabled:
		log.Info().Msg("Data Analyst disabled (DA_ENABLED=false)")
	case cfg.DAProvider == "":
		log.Warn().Msg("Data Analyst skipped — DA_PROVIDER is empty. Set it (gemini|anthropic|openai) in Settings and ensure opencode is authenticated for that provider (`opencode auth login`).")
	default:
		workspaceDir, werr := analyst.ResolveDefaultWorkspace(cfg.DAWorkspaceDir)
		if werr != nil {
			log.Warn().Err(werr).Msg("data analyst: workspace path resolve failed; analyst disabled")
			break
		}
		da, derr := analyst.Start(ctx, analyst.Options{
			Binary:              os.Getenv("OPENCODE_BIN"),
			Workspace:           workspaceDir,
			MCPURL:              "http://localhost:" + cfg.APIPort + "/mcp",
			Provider:            cfg.DAProvider,
			Model:               cfg.DAModel,
			HandshakeTimeoutSec: cfg.DAHandshakeTimeoutSec,
			Logger:              log.With().Str("comp", "analyst").Logger(),
		})
		if derr != nil {
			log.Warn().Err(derr).Msg("data analyst: pre-flight failed; analyst disabled")
		} else {
			dataAnalyst = da
			defer dataAnalyst.Stop()
			log.Info().Str("workspace", workspaceDir).Str("provider", cfg.DAProvider).Msg("Data Analyst boot queued (handshake runs after API is up)")
		}
	}

	// ── 14. Track geometry registry ─────────────────────────────────────
	// (Now loaded earlier, before the plan ticker which depends on the
	// registry for spatial pit-entry triggers. Kept here as a reference
	// marker for the original boot ordering.)
	_ = trackReg

	// ── 15. Fiber REST API ───────────────────────────────────────────────
	deps := &api.Deps{
		Store:          store,
		Cfg:            cfg,
		InsightChan:    dashboardChan,
		StartedAt:      startedAt,
		PacketsRx:      ingester.PacketsReceived,
		Workspace:      ws,
		Gate:           gate,
		Analyst:        dataAnalyst,
		RawInsightChan: rawInsightChan,
		Hub:            wsHub,
		VoiceClient:    voiceClient,
		Transcriber:    geminiSTT,
		LiveDeps:       liveDeps,
		TranslatedChan: translatedChan,
		InsightLog:     insightLog,
		Brain:          raceBrain,
		TrackMap:       trackReg,
		Reminders:      reminderStore,
		Proximity:      proximityWatcher,
		Transcript:     transcriptHub,
		Plan:           planStore,
	}
	server := api.NewServer(cfg.APIPort, deps)
	mcpServer.Mount(server.App(), "/mcp")
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Info().Str("port", cfg.APIPort).Msg("API server starting")
		if err := server.Start(); err != nil {
			// Fatal exits the process. A go-core with no HTTP/WS/MCP surface
			// cannot serve anything useful — keeping it alive only hides the
			// real failure behind downstream cascades (pi-agent 404 loop
			// against a stale older instance, vite ws proxy EPIPEs, dead
			// dashboard). Exit non-zero so `concurrently --kill-others`
			// tears the rest of the stack down with one clear error.
			log.Fatal().Err(err).Msg("API server failed to start — exiting")
		}
	}()

	log.Info().
		Str("api", ":"+cfg.APIPort).
		Int("udp_port", cfg.UDPPort).
		Int("ws_push_rate", cfg.WSPushRate).
		Bool("mock", cfg.MockMode.Load()).
		Bool("llm", provider.Available()).
		Str("llm_provider", cfg.LLMProvider).
		Msg("All systems go — awaiting telemetry")

	// ── 16. Graceful Shutdown ────────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info().Msg("Shutdown signal received — tearing down…")

	// Cancel context to stop all goroutines.
	cancel()

	// Shutdown HTTP server so no new requests are accepted.
	if err := server.Shutdown(); err != nil {
		log.Error().Err(err).Msg("HTTP shutdown error")
	}

	// Restore music volume before goroutines drain — once the speaker
	// goroutines are gone we lose the only thing that would re-issue a
	// duck/unduck, so doing it now beats leaking a ducked volume on
	// shutdown.
	if audioDucker != nil {
		audioDucker.Stop()
	}

	// Wait for all goroutines to finish (they flush buffers on exit).
	wg.Wait()

	log.Info().Msg("Graceful shutdown complete.")
}

// parseAudioTargets splits the AUDIO_DUCKING_TARGETS config string into
// a trimmed list. Empty/whitespace-only input yields nil so the
// platform ducker picks its own defaults (e.g. Spotify+Music on macOS).
func parseAudioTargets(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// fanout distributes translated insights to the dashboard channel, voice
// channel, WebSocket channel, and workspace writer. Non-blocking sends — if a
// channel is full, that consumer's insight is dropped rather than blocking others.
func fanout(ctx context.Context, in <-chan models.DrivingInsight, dashboard, voice, wsCh chan<- models.DrivingInsight, ws *workspace.Writer, ilog *insights.Log, b *brain.RaceBrain) {
	log.Info().Msg("Insight fanout started")
	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("Insight fanout stopping")
			return
		case insight, ok := <-in:
			if !ok {
				return
			}

			// Record to workspace and insight log (all translated insights).
			ws.RecordInsight(insight)
			ilog.Record("comms_gate", insight)
			if b != nil {
				b.RecordSpeech(insight.Message, insight.Priority)
			}

			// Dashboard (non-blocking).
			select {
			case dashboard <- insight:
			default:
				log.Warn().Msg("Dashboard insight channel full, dropping")
			}

			// Voice (non-blocking).
			select {
			case voice <- insight:
			default:
				log.Warn().Msg("Voice insight channel full, dropping")
			}

			// WebSocket (non-blocking).
			select {
			case wsCh <- insight:
			default:
				log.Warn().Msg("WebSocket insight channel full, dropping")
			}
		}
	}
}

// brainTranscriptAdapter satisfies brain.TranscriptHook by forwarding into
// the transcript hub. Defined here (not in transcript or brain) so neither
// package depends on the other.
type brainTranscriptAdapter struct {
	hub *transcript.Hub
}

func (a brainTranscriptAdapter) AppendEventLifecycle(kind, actor, text string, meta map[string]any) bool {
	if a.hub == nil {
		return false
	}
	return a.hub.Append(transcript.Event{
		Kind:  transcript.Kind(kind),
		Actor: actor,
		Text:  text,
		Meta:  meta,
	})
}
