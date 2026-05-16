package config

import (
	"os"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/rs/zerolog/log"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
)

// strictEnvVar names the env knob that disables env fallback entirely.
// Honoured for backwards compatibility — strict mode is the default since
// the file is now auto-seeded with every schema default on boot.
const strictEnvVar = "RACE_ENGINEER_CONFIG_STRICT"

// allowEnvVar is the opt-in escape hatch that re-enables the env-var
// fallback path. Set it truthy when migrating an old deployment whose
// .env still carries values you can't move into the JSON file yet.
const allowEnvVar = "RACE_ENGINEER_ALLOW_ENV"

// envDeprecationOnce ensures we warn at most once per env key per process.
var envDeprecationOnce sync.Map // key string → struct{}

// Config holds all service configuration. Static fields are set once at startup
// from env vars. Dynamic fields use atomic types so the dashboard API can
// change them at runtime without locks.
type Config struct {
	// --- Static (set once at startup) ---
	UDPHost         string // e.g. "0.0.0.0"
	UDPPort         int    // e.g. 20777
	APIPort         string // e.g. "8081"
	DBPath          string // e.g. "workspace/telemetry.duckdb"
	PythonAPI       string // e.g. "http://localhost:8000"
	BatchSize       int    // flush threshold for DuckDB buffers
	SampleRate      int    // 20Hz → 1Hz = sample every 20th packet
	HifreqSampleRate int   // hi-freq player-only sampler stride (UDP_RATE / N).
	//                     // Default 2 → 10Hz from 20Hz car_telemetry packets.
	//                     // Drives writes into telemetry_hifreq for the
	//                     // /api/laps/* lap-trace + brake-point analysis tools.
	GeminiAPIKey    string // Google Gemini API key (empty = LLM disabled)
	LLMProvider     string // "gemini" (default), "anthropic", "openai"
	LLMModel        string // Override model name (empty = provider default)
	AnthropicAPIKey string // Anthropic API key for Claude
	OpenAIAPIKey    string // OpenAI API key
	TTSVoice        string // prebuilt Gemini voice (Kore / Puck / Charon / Aoede / ...), empty = SDK default
	WorkspaceDir    string // path to workspace/ directory with markdown context files
	LogLevel        string // zerolog level: trace, debug, info, warn, error, fatal, panic
	WSPushRate      int    // WebSocket push rate in Hz (e.g. 10 = 100ms interval)

	// --- Pi Agent (sandboxed background data-analyst team) ---
	// Replaces the old Strategy Analyst. The Go server hosts an MCP server at
	// PiAgentMCPPath; pi_agent_service.py is a Python child process whose only
	// outward connection is that MCP endpoint. Event-driven only — no ticker.
	PiAgentMode              string // "off" | "on"
	PiAgentProvider          string // "anthropic" | "gemini" | "openai"
	PiAgentModel             string // empty = provider default
	PiAgentSpecialistModel   string // empty = same as planner
	PiAgentMaxPriority       int    // cap on push_insight priority (1-5)
	PiAgentMCPPath           string // HTTP path the MCP server is mounted on
	PiAgentTriggerTimeoutSec int    // long-poll timeout (s) for pull_next_trigger
	// PTT settings are stored atomically so the dashboard can change them
	// without restarting the state-writer goroutine. Read with the getter
	// methods (PTTButton, PTTMode, …) and write with the setters.
	//   "mfd" watches MFDPanelIndex in car_telemetry packets, which is
	//   sampled at 60Hz so no presses are missed. Each MFD page change is
	//   treated as a single "press".
	//   "dual" uses two distinct BUTN bitmasks: PTTButtonStart opens the
	//   mic, PTTButtonEnd closes it. Press-edge only, no hold required.
	//   Designed for UDP Action 1/2 bound to dedicated wheel buttons
	//   (L2/R2 etc.) — miss-tolerant because each direction is a separate
	//   press event.
	pttButton      atomic.Uint32
	pttMode        atomic.Pointer[string] // "hold" (momentary) or "toggle"
	pttTrigger     atomic.Pointer[string] // "button", "mfd", "dual"
	pttButtonStart atomic.Uint32 // "mic ON" bitmask when trigger="dual"
	pttButtonEnd   atomic.Uint32 // "mic OFF" bitmask when trigger="dual"
	// VoiceMode picks which voice-input path runs at boot. One of:
	//   "live_only" (default) — only Gemini Live owns the voice surface.
	//                            STT is not initialised, /api/voice returns 503.
	//                            Saves an idle SDK client + ~tens of MB.
	//   "ptt_only"             — only the legacy push-to-talk path runs.
	//                            Gemini Live agent factory is not built,
	//                            /api/voice/live refuses new connections.
	//                            STT + CommsGate handle every utterance.
	//   "both"                 — Live owns the voice surface (insights come
	//                            out through Live audio) AND the PTT path
	//                            is available as a fallback input. Both
	//                            handlers respond. Highest cost.
	//
	// Boot-time only — changing it requires a server restart. The dashboard
	// Settings page persists it; .env's VOICE_MODE seeds the initial value.
	// Legacy GEMINI_LIVE_ENABLED is respected as a compat hint: when set to
	// true and VOICE_MODE is empty, the effective mode becomes "both" so
	// existing deployments don't lose their PTT fallback.
	VoiceMode                string // "live_only" | "ptt_only" | "both"
	GeminiLiveEnabled        bool   // derived from VoiceMode: true when VoiceMode != "ptt_only". When true, Gemini Live owns the voice surface — VoiceClient is silenced and the dashboard talks to /api/voice/live.
	GeminiLiveModel          string // optional override for the Live API model name; empty = SDK default
	GeminiLiveAnalystTimeout int    // seconds the ask_data_analyst tool polls the brain for an answer before giving up

	// EventBundleDelayMs is the gather window (ms) the live event pump
	// waits after leasing a P>=4 brain event before asking the brain to
	// fold any concurrently-queued siblings into the same dispatch. A
	// small window (default 250ms) catches correlated alerts firing
	// within the same UDP tick so the driver hears one bundled radio
	// call instead of two back-to-back ones. Set to 0 to disable.
	EventBundleDelayMs int

	// Transcript hub knobs — see internal/transcript for behaviour.
	TranscriptPromptLines int // events injected into LLM prompts per call (default 25)
	TranscriptToolLimit   int // default cap on /api/transcript pulls (default 200)
	TranscriptRetention   int // session files kept on disk (default 30)

	// Audio ducking — lower external music player volumes while the
	// engineer speaks. See internal/audio for the per-OS implementations.
	AudioDuckingEnabled bool
	AudioDuckingMode    string  // "duck" (lower volume) or "pause" (pause/resume the player)
	AudioDuckingLevel   float64 // 0.0-1.0 (ignored in pause mode)
	AudioDuckingTargets string  // comma-separated app names; empty = OS default
	AudioDuckingTailMs  int     // tail-time after last Live audio chunk before unduck

	// CornerReminderDefaultLookaheadSec is the time-before-trigger applied
	// when set_corner_reminder omits the lookahead_seconds parameter. Bumped
	// to 3.0s in the coaching plan — lookahead in metres is then computed
	// from current speed (clamped 30m..400m by insights.ComputeLookaheadM).
	CornerReminderDefaultLookaheadSec float64

	// logButtons, when true, prints every BUTN press/release with its
	// named bitmask through zerolog. Convenience for mapping wheel buttons
	// to UDP Action slots while configuring PTT — saves running buttonwatch
	// in a side terminal. Atomic so it can be toggled from the dashboard.
	logButtons atomic.Bool

	// --- Dynamic (changeable via dashboard at runtime) ---
	MockMode      atomic.Bool          // true = generate fake telemetry instead of listening to UDP
	TalkLevel     atomic.Int32         // 1 (quiet) to 10 (chatty) — controls insight verbosity
	Verbosity     atomic.Int32         // 1 (terse) to 10 (detailed) — controls engineer response length
	MockOverrides models.MockOverrides // manual physics/weather overrides
	RestartGen    atomic.Uint64        // bumped on config change to signal listener restart
	mu            sync.RWMutex         // protects non-atomic dynamic fields below
	udpPort       int                  // runtime-changeable UDP port (requires listener restart)
	udpHost       string               // runtime-changeable UDP host
	udpMode       string               // "broadcast" or "unicast"
}

// Load reads configuration from environment variables with sensible defaults.
// The persisted UI config at ~/.race-engineer/config.json is overlaid on top
// of env so the dashboard's Settings page becomes authoritative for keys
// the user has touched, while .env keeps owning everything else.
func Load() *Config {
	// Seed schema defaults into the JSON file on every boot so the file is
	// always a complete picture. This is the migration path from .env →
	// config.json: a fresh user sees a fully-populated file after first
	// launch; an existing user keeps all their values and only newly-added
	// schema fields are added with their defaults.
	if added, created, err := SeedDefaults(); err != nil {
		log.Warn().Err(err).Msg("config: seeding defaults failed; running with whatever is in file/env")
	} else if created {
		log.Info().Int("keys", len(added)).Msg("config: seeded fresh ~/.race-engineer/config.json with schema defaults")
	} else if len(added) > 0 {
		log.Info().Strs("keys", added).Msg("config: added missing schema defaults to ~/.race-engineer/config.json")
	}

	fileCfg, ferr := LoadFile()
	if ferr != nil {
		log.Warn().Err(ferr).Msg("config file unreadable, falling back to env")
		fileCfg = map[string]any{}
	}
	src := layered(fileCfg)

	c := &Config{
		UDPHost:         src.str("TELEMETRY_HOST", "0.0.0.0"),
		UDPPort:         src.intv("TELEMETRY_PORT", 20777),
		APIPort:         src.str("API_PORT", "8081"),
		DBPath:          src.str("DB_PATH", "workspace/telemetry.duckdb"),
		PythonAPI:       src.str("PYTHON_API_URL", "http://localhost:8000"),
		BatchSize:       src.intv("BATCH_SIZE", 20),
		SampleRate:      src.intv("SAMPLE_RATE", 20),
		HifreqSampleRate: src.intv("HIFREQ_SAMPLE_RATE", 2),
		GeminiAPIKey:    src.str("GEMINI_API_KEY", ""),
		LLMProvider:     src.str("LLM_PROVIDER", "gemini"),
		LLMModel:        src.str("LLM_MODEL", ""),
		AnthropicAPIKey: src.str("ANTHROPIC_API_KEY", ""),
		OpenAIAPIKey:    src.str("OPENAI_API_KEY", ""),
		TTSVoice:        src.str("TTS_VOICE", ""),
		WorkspaceDir:    src.str("WORKSPACE_DIR", "workspace"),
		LogLevel:        src.str("LOG_LEVEL", "info"),
		WSPushRate:      src.intv("WS_PUSH_RATE", 10),

		PiAgentMode:              src.str("PI_AGENT_MODE", "on"),
		PiAgentProvider:          src.str("PI_AGENT_PROVIDER", "gemini"),
		PiAgentModel:             src.str("PI_AGENT_MODEL", "gemini-3.1-flash-lite"),
		PiAgentSpecialistModel:   src.str("PI_AGENT_SPECIALIST_MODEL", "gemini-3.1-flash-lite"),
		PiAgentMaxPriority:       src.intv("PI_AGENT_MAX_PRIORITY", 3),
		PiAgentMCPPath:           src.str("PI_AGENT_MCP_PATH", "/mcp"),
		PiAgentTriggerTimeoutSec: src.intv("PI_AGENT_TRIGGER_TIMEOUT_SEC", 10),

		// VoiceMode is loaded below — it picks the voice path and also
		// drives the legacy GeminiLiveEnabled flag so existing callers
		// keep working without changes.
		GeminiLiveModel:          src.str("GEMINI_LIVE_MODEL", "gemini-3.1-flash-live-preview"),
		GeminiLiveAnalystTimeout: src.intv("GEMINI_LIVE_ANALYST_TIMEOUT", 120),
		EventBundleDelayMs:       src.intv("EVENT_BUNDLE_DELAY_MS", 250),

		TranscriptPromptLines: src.intv("TRANSCRIPT_PROMPT_LINES", 25),
		TranscriptToolLimit:   src.intv("TRANSCRIPT_TOOL_LIMIT", 200),
		TranscriptRetention:   src.intv("TRANSCRIPT_RETENTION_SESSIONS", 30),

		CornerReminderDefaultLookaheadSec: src.floatv("CORNER_REMINDER_DEFAULT_LOOKAHEAD_SEC", 3.0),

		AudioDuckingEnabled: src.boolv("AUDIO_DUCKING_ENABLED", false),
		AudioDuckingMode:    src.str("AUDIO_DUCKING_MODE", "duck"),
		AudioDuckingLevel:   src.floatv("AUDIO_DUCKING_LEVEL", 0.3),
		AudioDuckingTargets: src.str("AUDIO_DUCKING_TARGETS", ""),
		AudioDuckingTailMs:  src.intv("AUDIO_DUCKING_TAIL_MS", 800),
	}

	// Atomic-backed PTT fields — initialised after the struct literal so
	// they pick up persisted values via the same env+file precedence as the
	// static fields above. The dashboard can later flip them live.
	c.pttButton.Store(uint32(src.hex("PTT_BUTTON", 0x00000001))) // observed PS5 Cross/A button
	storeStr(&c.pttMode, src.str("PTT_MODE", "hold"))
	storeStr(&c.pttTrigger, src.str("PTT_TRIGGER", "button"))
	// UDP Action 1 / 2 bits per F1 25 spec — see internal/packets/buttons.go.
	// These are the no-side-effect helper bindings under Controls > UDP;
	// recommended for PTT_TRIGGER=dual because press-edge-only behaviour
	// can't strand the mic on a dropped release event.
	c.pttButtonStart.Store(uint32(src.hex("PTT_BUTTON_START", 0x00020000)))
	c.pttButtonEnd.Store(uint32(src.hex("PTT_BUTTON_END", 0x00040000)))
	c.logButtons.Store(src.boolv("LOG_BUTTONS", false))

	// Resolve voice mode. Precedence:
	//   1. Explicit VOICE_MODE (env or persisted config) wins.
	//   2. Legacy GEMINI_LIVE_ENABLED=true → "both" (preserve PTT fallback
	//      that older deployments expect).
	//   3. Default → "live_only" (new behaviour: Live owns the voice
	//      surface and STT isn't initialised, saving an idle SDK client).
	rawVoiceMode := src.str("VOICE_MODE", "")
	switch rawVoiceMode {
	case "live_only", "ptt_only", "both":
		c.VoiceMode = rawVoiceMode
	case "":
		if src.boolv("GEMINI_LIVE_ENABLED", false) {
			c.VoiceMode = "both"
		} else {
			c.VoiceMode = "live_only"
		}
	default:
		log.Warn().Str("voice_mode", rawVoiceMode).Msg("unknown VOICE_MODE; falling back to live_only")
		c.VoiceMode = "live_only"
	}
	// Derive the legacy flag. live_only and both share the "Live owns the
	// voice surface" semantics; ptt_only is the only mode where the
	// VoiceClient (TTS pipeline) speaks insights.
	c.GeminiLiveEnabled = c.VoiceMode != "ptt_only"

	c.MockMode.Store(src.str("TELEMETRY_MODE", "real") == "mock")
	c.TalkLevel.Store(int32(src.intv("TALK_LEVEL", 5)))
	c.Verbosity.Store(int32(src.intv("VERBOSITY", 5)))
	c.MockOverrides = models.MockOverrides{
		TireWearMultiplier: 1.0,
		FuelBurnMultiplier: 1.0,
		TireTempOffset:     0.0,
	}
	c.udpPort = c.UDPPort
	c.udpHost = c.UDPHost
	c.udpMode = src.str("UDP_MODE", "broadcast")

	log.Info().
		Str("udp_host", c.UDPHost).
		Int("udp_port", c.UDPPort).
		Str("udp_mode", c.udpMode).
		Str("api_port", c.APIPort).
		Str("db_path", c.DBPath).
		Str("python_api", c.PythonAPI).
		Bool("mock_mode", c.MockMode.Load()).
		Int32("talk_level", c.TalkLevel.Load()).
		Str("llm_provider", c.LLMProvider).
		Str("llm_model", c.LLMModel).
		Str("pi_agent_mode", c.PiAgentMode).
		Bool("gemini_live", c.GeminiLiveEnabled).
		Str("voice_mode", c.VoiceMode).
		Msg("Configuration loaded")

	return c
}

// LivePathEnabled reports whether the Gemini Live voice surface
// (/api/voice/live) should be wired at boot. True for "live_only" and
// "both".
func (c *Config) LivePathEnabled() bool {
	return c.VoiceMode != "ptt_only"
}

// PTTPathEnabled reports whether the legacy push-to-talk surface
// (/api/voice → STT → CommsGate) should be wired at boot. True for
// "ptt_only" and "both".
func (c *Config) PTTPathEnabled() bool {
	return c.VoiceMode != "live_only"
}

// SetMockMode toggles mock mode at runtime (from dashboard API).
func (c *Config) SetMockMode(mock bool) {
	c.MockMode.Store(mock)
	c.RestartGen.Add(1)
	log.Info().Bool("mock_mode", mock).Msg("Mock mode updated")
}

// SetTalkLevel adjusts verbosity at runtime (from dashboard API).
func (c *Config) SetTalkLevel(level int) {
	if level < 1 {
		level = 1
	}
	if level > 10 {
		level = 10
	}
	c.TalkLevel.Store(int32(level))
	log.Info().Int("talk_level", level).Msg("Talk level updated")
}

// SetVerbosity adjusts the engineer's response detail level at runtime.
func (c *Config) SetVerbosity(level int) {
	if level < 1 {
		level = 1
	}
	if level > 10 {
		level = 10
	}
	c.Verbosity.Store(int32(level))
	log.Info().Int("verbosity", level).Msg("Verbosity updated")
}

// PTTButton returns the current single-button push-to-talk bitmask.
func (c *Config) PTTButton() uint32 { return c.pttButton.Load() }

// PTTMode returns "hold" or "toggle".
func (c *Config) PTTMode() string { return loadStr(&c.pttMode) }

// PTTTrigger returns "button", "mfd", or "dual".
func (c *Config) PTTTrigger() string { return loadStr(&c.pttTrigger) }

// PTTButtonStart returns the dual-trigger "mic ON" bitmask.
func (c *Config) PTTButtonStart() uint32 { return c.pttButtonStart.Load() }

// PTTButtonEnd returns the dual-trigger "mic OFF" bitmask.
func (c *Config) PTTButtonEnd() uint32 { return c.pttButtonEnd.Load() }

// LogButtons reports whether every BUTN press/release should be logged.
func (c *Config) LogButtons() bool { return c.logButtons.Load() }

// SetPTTButton updates the single-button PTT bitmask live. The stateWriter
// goroutine re-reads it on the next BUTN packet, so no restart needed.
func (c *Config) SetPTTButton(v uint32) {
	c.pttButton.Store(v)
	log.Info().Str("ptt_button", "0x"+strconvFormatHex(uint64(v))).Msg("PTT button updated")
}

// SetPTTMode updates the press behaviour ("hold" or "toggle") live.
func (c *Config) SetPTTMode(v string) {
	if v != "hold" && v != "toggle" {
		log.Warn().Str("ptt_mode", v).Msg("ignoring invalid PTT mode")
		return
	}
	storeStr(&c.pttMode, v)
	log.Info().Str("ptt_mode", v).Msg("PTT mode updated")
}

// SetPTTTrigger updates the trigger source ("off", "button", "mfd", "dual") live.
// "off" tells the BUTN-packet branch to skip PTT entirely — useful when the
// driver only wants Gemini Live's keyboard PTT and no hardware-button events.
func (c *Config) SetPTTTrigger(v string) {
	switch v {
	case "off", "button", "mfd", "dual":
	default:
		log.Warn().Str("ptt_trigger", v).Msg("ignoring invalid PTT trigger")
		return
	}
	storeStr(&c.pttTrigger, v)
	log.Info().Str("ptt_trigger", v).Msg("PTT trigger updated")
}

// SetPTTButtonStart updates the dual-trigger "mic ON" bitmask live.
func (c *Config) SetPTTButtonStart(v uint32) {
	c.pttButtonStart.Store(v)
	log.Info().Str("ptt_button_start", "0x"+strconvFormatHex(uint64(v))).Msg("PTT start button updated")
}

// SetPTTButtonEnd updates the dual-trigger "mic OFF" bitmask live.
func (c *Config) SetPTTButtonEnd(v uint32) {
	c.pttButtonEnd.Store(v)
	log.Info().Str("ptt_button_end", "0x"+strconvFormatHex(uint64(v))).Msg("PTT end button updated")
}

// SetLogButtons toggles BUTN press/release logging live.
func (c *Config) SetLogButtons(v bool) {
	c.logButtons.Store(v)
	log.Info().Bool("log_buttons", v).Msg("LOG_BUTTONS updated")
}

// loadStr / storeStr wrap atomic.Pointer[string] with a nil-safe getter
// and a stable allocation site so the value never aliases the caller's
// stack/local string.
func loadStr(p *atomic.Pointer[string]) string {
	s := p.Load()
	if s == nil {
		return ""
	}
	return *s
}

func storeStr(p *atomic.Pointer[string], v string) {
	s := v
	p.Store(&s)
}

func strconvFormatHex(n uint64) string { return strconv.FormatUint(n, 16) }

// SetMockOverrides updates the manual overrides for physics/weather simulation.
func (c *Config) SetMockOverrides(overrides models.MockOverrides) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.MockOverrides = overrides
}

// GetMockOverrides returns a snapshot of the current mock simulation overrides.
func (c *Config) GetMockOverrides() models.MockOverrides {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.MockOverrides
}

// RuntimeUDPHost returns the current runtime UDP host.
func (c *Config) RuntimeUDPHost() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.udpHost
}

// RuntimeUDPPort returns the current runtime UDP port (may differ from startup).
func (c *Config) RuntimeUDPPort() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.udpPort
}

// RuntimeUDPMode returns the current UDP stream mode ("broadcast" or "unicast").
func (c *Config) RuntimeUDPMode() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.udpMode
}

// SetRuntimeUDP changes the UDP host, port, and/or mode at runtime and signals
// the ingestion layer to restart the listener.
func (c *Config) SetRuntimeUDP(host string, port int, udpMode string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if host != "" {
		c.udpHost = host
	}
	if port > 0 {
		c.udpPort = port
	}
	if udpMode == "broadcast" || udpMode == "unicast" {
		c.udpMode = udpMode
	}
	c.RestartGen.Add(1)
	log.Info().Str("udp_host", c.udpHost).Int("udp_port", c.udpPort).Str("udp_mode", c.udpMode).Msg("Runtime UDP config updated")
}

// SetRuntimeUDPPort changes the UDP port at runtime (backwards compat).
func (c *Config) SetRuntimeUDPPort(port int) {
	c.SetRuntimeUDP("", port, "")
}

// --- helpers ---

func envStr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	switch v {
	case "1", "true", "TRUE", "True", "yes", "YES":
		return true
	case "0", "false", "FALSE", "False", "no", "NO":
		return false
	}
	return fallback
}

func envHex(key string, fallback uint64) uint64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseUint(v, 0, 32); err == nil {
			return n
		}
	}
	return fallback
}

// fileEnv resolves a config key from the persisted JSON file first, then
// from os.Getenv. Returns "" if neither holds it. Numeric values stored as
// JSON numbers are stringified so the existing parsers (envInt, envHex)
// keep their semantics.
//
// Precedence policy (Package G migration): the file is authoritative; env
// is a fallback we warn about so users can spot keys still tied to .env.
// When strict==true (RACE_ENGINEER_CONFIG_STRICT=1) the env fallback is
// disabled entirely and only the file is consulted.
type fileEnv struct {
	file   map[string]any
	strict bool
}

func layered(file map[string]any) fileEnv {
	// Strict by default: the file is the only source. The legacy
	// RACE_ENGINEER_CONFIG_STRICT toggle is still honoured for callers that
	// hardcoded it; RACE_ENGINEER_ALLOW_ENV is the new escape hatch that
	// re-enables os.Getenv fallback for partial-migration scenarios.
	strict := true
	if isTruthy(os.Getenv(allowEnvVar)) {
		strict = false
	}
	// Backwards-compat: explicit STRICT=true/1 still forces strict on. An
	// explicit STRICT=false used to disable strict mode — preserve that.
	if v := os.Getenv(strictEnvVar); v != "" {
		switch v {
		case "0", "false", "FALSE", "False", "no", "NO":
			strict = false
		case "1", "true", "TRUE", "True", "yes", "YES":
			strict = true
		}
	}
	return fileEnv{file: file, strict: strict}
}

func isTruthy(v string) bool {
	switch v {
	case "1", "true", "TRUE", "True", "yes", "YES":
		return true
	}
	return false
}

func (f fileEnv) raw(key string) (string, bool) {
	if v, ok := f.file[key]; ok && v != nil {
		switch x := v.(type) {
		case string:
			return x, x != ""
		case bool:
			if x {
				return "true", true
			}
			return "false", true
		case float64:
			// JSON numbers always decode to float64. Render whole numbers without
			// the ".0" suffix so envInt/envHex parse cleanly.
			if x == float64(int64(x)) {
				return strconv.FormatInt(int64(x), 10), true
			}
			return strconv.FormatFloat(x, 'f', -1, 64), true
		case int:
			return strconv.Itoa(x), true
		case int64:
			return strconv.FormatInt(x, 10), true
		}
	}
	if f.strict {
		return "", false
	}
	if v := os.Getenv(key); v != "" {
		warnEnvDeprecated(key)
		return v, true
	}
	return "", false
}

// warnEnvDeprecated logs the one-time deprecation notice for a key that
// resolved from the environment instead of the JSON config file. Quiet on
// every subsequent hit so we don't spam boot logs.
func warnEnvDeprecated(key string) {
	if _, loaded := envDeprecationOnce.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	// Skip the env-only toggles themselves — they are *only* env vars by design.
	if key == strictEnvVar || key == allowEnvVar {
		return
	}
	log.Warn().
		Str("key", key).
		Msg("env var resolved from os.Environ; persist it in ~/.race-engineer/config.json (configtool set)")
}

func (f fileEnv) str(key, fallback string) string {
	if v, ok := f.raw(key); ok {
		return v
	}
	return fallback
}

func (f fileEnv) intv(key string, fallback int) int {
	if v, ok := f.raw(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func (f fileEnv) boolv(key string, fallback bool) bool {
	v, ok := f.raw(key)
	if !ok {
		return fallback
	}
	switch v {
	case "1", "true", "TRUE", "True", "yes", "YES":
		return true
	case "0", "false", "FALSE", "False", "no", "NO":
		return false
	}
	return fallback
}

func (f fileEnv) hex(key string, fallback uint64) uint64 {
	if v, ok := f.raw(key); ok {
		if n, err := strconv.ParseUint(v, 0, 32); err == nil {
			return n
		}
	}
	return fallback
}

func (f fileEnv) floatv(key string, fallback float64) float64 {
	if v, ok := f.raw(key); ok {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			return n
		}
	}
	return fallback
}
