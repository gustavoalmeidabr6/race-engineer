package config

// schema.go is the single source of truth for every config key the
// race-engineer stack reads.  Adding a new knob means appending one entry to
// Schema below — Load() picks it up automatically, persist.go's secret mask
// derives from Secret=true, configtool uses it for help/defaults/validation,
// and the dashboard renders its Settings page off /api/config/schema.

// Field kinds. Kept as untyped string constants so JSON encoding round-trips
// without an enum mapping.
const (
	KindString = "string"
	KindInt    = "int"
	KindFloat  = "float"
	KindBool   = "bool"
	KindHex    = "hex"
)

// Group names. Match the dashboard Settings page sections.
const (
	GroupTelemetry  = "telemetry"
	GroupLLM        = "llm"
	GroupVoice      = "voice"
	GroupPTT        = "ptt"
	GroupGeminiLive = "gemini_live"
	GroupCoaching   = "coaching"
	GroupLogging    = "logging"
	GroupServer     = "server"
	GroupDataAnalyst = "data_analyst"
	GroupAudio       = "audio"
)

// Field describes one persistable config key.  Default is the literal value
// applied when neither the JSON file nor an env var supplies the key.
//
//   Kind     — drives type coercion / validation in configtool and the
//              dashboard form rendering.
//   Live     — true when the value can be flipped at runtime via the
//              atomic setters on Config.  Static keys take effect on next
//              process restart (the UI surfaces this as a yellow badge).
//   Secret   — true for API keys and other things we mask in API responses
//              and never log.
//   Required — when true, the key MUST be present (non-empty / non-zero)
//              for the server to boot in strict mode.  Provider API-key
//              gating is also handled here (see SchemaProviderKey).
//   Help     — one-line description.  configtool show prints it; the
//              dashboard renders it under each input.
type Field struct {
	Key      string `json:"key"`
	Kind     string `json:"kind"`
	Default  any    `json:"default,omitempty"`
	Secret   bool   `json:"secret,omitempty"`
	Required bool   `json:"required,omitempty"`
	Live     bool   `json:"live,omitempty"`
	Group    string `json:"group,omitempty"`
	Help     string `json:"help,omitempty"`
}

// Schema is the closed set of keys.  Order is preserved for stable output
// from `configtool show` and the dashboard form.
var Schema = []Field{
	// --- Server / process ---
	{Key: "API_PORT", Kind: KindString, Default: "8081", Group: GroupServer, Help: "Fiber REST/WebSocket listen port"},
	{Key: "DB_PATH", Kind: KindString, Default: "workspace/telemetry.duckdb", Group: GroupServer, Help: "DuckDB file path"},
	{Key: "WORKSPACE_DIR", Kind: KindString, Default: "workspace", Group: GroupServer, Help: "Workspace directory with markdown context files"},
	{Key: "LOG_LEVEL", Kind: KindString, Default: "info", Group: GroupLogging, Help: "zerolog level: trace,debug,info,warn,error,fatal,panic"},
	{Key: "PYTHON_API_URL", Kind: KindString, Default: "http://localhost:8000", Group: GroupServer, Help: "Legacy Python voice service URL (vestigial since Phase 3.1)"},
	{Key: "RACE_API_URL", Kind: KindString, Default: "http://localhost:8081", Group: GroupServer, Help: "Base URL Python services use to call back into Go"},

	// --- Telemetry / ingestion ---
	{Key: "TELEMETRY_MODE", Kind: KindString, Default: "real", Live: true, Group: GroupTelemetry, Help: "real = UDP listener, mock = synthetic generator"},
	{Key: "TELEMETRY_HOST", Kind: KindString, Default: "0.0.0.0", Live: true, Group: GroupTelemetry, Help: "UDP bind host"},
	{Key: "TELEMETRY_PORT", Kind: KindInt, Default: 20777, Live: true, Group: GroupTelemetry, Help: "UDP port (F1 25 default 20777)"},
	{Key: "UDP_MODE", Kind: KindString, Default: "broadcast", Live: true, Group: GroupTelemetry, Help: "broadcast or unicast"},
	{Key: "SAMPLE_RATE", Kind: KindInt, Default: 20, Group: GroupTelemetry, Help: "UDP 20Hz to 1Hz sampling stride"},
	{Key: "HIFREQ_SAMPLE_RATE", Kind: KindInt, Default: 2, Group: GroupTelemetry, Help: "Hi-freq player sampler stride (UDP_RATE/N). Default 2 = 10Hz. 0 disables."},
	{Key: "BATCH_SIZE", Kind: KindInt, Default: 20, Group: GroupTelemetry, Help: "DuckDB buffer flush threshold"},
	{Key: "WS_PUSH_RATE", Kind: KindInt, Default: 10, Group: GroupTelemetry, Help: "WebSocket push rate in Hz"},

	// --- LLM ---
	{Key: "LLM_PROVIDER", Kind: KindString, Default: "gemini", Required: true, Group: GroupLLM, Help: "gemini, anthropic, openai"},
	{Key: "LLM_MODEL", Kind: KindString, Default: "", Group: GroupLLM, Help: "Override model name (empty = provider default)"},
	{Key: "GEMINI_API_KEY", Kind: KindString, Secret: true, Group: GroupLLM, Help: "Google Gemini API key"},
	{Key: "ANTHROPIC_API_KEY", Kind: KindString, Secret: true, Group: GroupLLM, Help: "Anthropic API key"},
	{Key: "OPENAI_API_KEY", Kind: KindString, Secret: true, Group: GroupLLM, Help: "OpenAI API key"},
	{Key: "RESEMBLE_API_KEY", Kind: KindString, Secret: true, Group: GroupLLM, Help: "Resemble.ai voice cloning API key (optional)"},

	// --- Voice ---
	{Key: "TTS_VOICE", Kind: KindString, Default: "Charon", Group: GroupVoice, Help: "Prebuilt Gemini voice (Kore, Puck, Charon, Aoede, ...). Used by both the standalone TTS path and Gemini Live."},
	{Key: "VOICE_MODE", Kind: KindString, Default: "live_only", Group: GroupVoice, Help: "live_only | ptt_only | both — which input path runs at boot"},
	// Legacy Python voice_service.py knobs. The Go core no longer reads
	// them but they're seeded into the JSON file so the Python service
	// keeps working through race_config — and so the dashboard surfaces
	// them as visible knobs.
	{Key: "TTS_ENGINE", Kind: KindString, Default: "edge-tts", Group: GroupVoice, Help: "Python voice_service TTS backend: edge-tts | qwen-tts"},
	{Key: "TTS_VOICE_REF", Kind: KindString, Default: "workspace/voices/reference.wav", Group: GroupVoice, Help: "Voice reference WAV for Qwen TTS cloning"},
	{Key: "TTS_SPEED", Kind: KindFloat, Default: 1.0, Group: GroupVoice, Help: "Qwen TTS speech speed multiplier"},
	{Key: "STT_ENGINE", Kind: KindString, Default: "whisper", Group: GroupVoice, Help: "Python voice_service STT backend: whisper | parakeet | resemble"},
	{Key: "WHISPER_MODEL", Kind: KindString, Default: "medium", Group: GroupVoice, Help: "faster-whisper model size"},
	{Key: "RESEMBLE_BASE_URL", Kind: KindString, Default: "https://app.resemble.ai/api/v2", Group: GroupVoice, Help: "Resemble.ai API base URL"},

	// --- Push-to-Talk ---
	{Key: "PTT_BUTTON", Kind: KindHex, Default: "0x00000001", Group: GroupPTT, Help: "F1 BUTN bitmask for push-to-talk (e.g. PS5 Cross/A)"},
	{Key: "PTT_MODE", Kind: KindString, Default: "hold", Group: GroupPTT, Help: "hold (momentary) or toggle (latch on press)"},
	{Key: "PTT_TRIGGER", Kind: KindString, Default: "button", Group: GroupPTT, Help: "off | button | mfd | dual — which signal drives PTT (off disables hardware PTT entirely)"},
	{Key: "PTT_BUTTON_START", Kind: KindHex, Default: "0x00020000", Group: GroupPTT, Help: "Bitmask that OPENS the mic in dual mode (default = UDP Action 1)"},
	{Key: "PTT_BUTTON_END", Kind: KindHex, Default: "0x00040000", Group: GroupPTT, Help: "Bitmask that CLOSES the mic in dual mode (default = UDP Action 2)"},
	{Key: "LOG_BUTTONS", Kind: KindBool, Default: false, Group: GroupPTT, Help: "Log every BUTN press/release with the named bitmask"},

	// --- Gemini Live ---
	{Key: "GEMINI_LIVE_ENABLED", Kind: KindBool, Default: false, Group: GroupGeminiLive, Help: "Legacy compat: maps to VOICE_MODE=both when VOICE_MODE is empty"},
	{Key: "GEMINI_LIVE_MODEL", Kind: KindString, Default: "gemini-3.1-flash-live-preview", Group: GroupGeminiLive, Help: "Optional override for the Live API model name"},
	{Key: "GEMINI_LIVE_BRAIN_POLL_SEC", Kind: KindInt, Default: 5, Group: GroupGeminiLive, Help: "How often to poll the brain snapshot and push it as context"},
	{Key: "GEMINI_LIVE_BRAIN_MAX_CHARS", Kind: KindInt, Default: 8000, Group: GroupGeminiLive, Help: "Truncation cap for brain context pushes"},
	{Key: "GEMINI_LIVE_ANALYST_TIMEOUT", Kind: KindInt, Default: 120, Group: GroupGeminiLive, Help: "Timeout (s) for the ask_data_analyst tool round-trip"},
	{Key: "GEMINI_LIVE_PTT_KEY", Kind: KindString, Default: "", Group: GroupGeminiLive, Help: "Optional PTT key override for the Live agent"},
	{Key: "GEMINI_LIVE_PTT_SOURCE", Kind: KindString, Default: "", Group: GroupGeminiLive, Help: "Optional PTT source override for the Live agent"},
	{Key: "EVENT_BUNDLE_DELAY_MS", Kind: KindInt, Default: 250, Group: GroupGeminiLive, Help: "Gather window (ms) before dispatching a P>=4 brain event so concurrently-queued siblings merge into one radio call. 0 disables."},

	// --- Coaching / analyst ---
	{Key: "TALK_LEVEL", Kind: KindInt, Default: 5, Live: true, Group: GroupCoaching, Help: "Insight verbosity 1 (quiet) to 10 (chatty)"},
	{Key: "VERBOSITY", Kind: KindInt, Default: 5, Live: true, Group: GroupCoaching, Help: "Engineer response detail 1 (terse) to 10 (detailed)"},
	{Key: "CORNER_REMINDER_DEFAULT_LOOKAHEAD_SEC", Kind: KindFloat, Default: 3.0, Group: GroupCoaching, Help: "Default lookahead seconds when set_corner_reminder omits lookahead_seconds"},

	// --- Data Analyst (opencode-backed pit-wall analyst) ---
	// Replaces the old hand-rolled pi-agent. The Go server launches
	// `opencode acp` once per app lifetime as a long-lived child process
	// and drives it via JSON-RPC 2.0 over stdin/stdout (Agent Client
	// Protocol). The agent reaches back through MCP at the same
	// /mcp endpoint Gemini Live uses, picking up 24 read-only data tools.
	// MCP path is fixed at /mcp (no separate knob — the analyst writes it
	// into opencode.json at session start).
	{Key: "DA_ENABLED", Kind: KindBool, Default: true, Group: GroupDataAnalyst, Help: "Run the Data Analyst on lap/event/query triggers. Auto-disables if the opencode binary cannot be located or fails to start."},
	{Key: "DA_WORKSPACE_DIR", Kind: KindString, Default: "", Group: GroupDataAnalyst, Help: "Path to the analyst workspace (AGENTS.md, skills, learnings). Empty = ~/.race-engineer/da-workspace (or the app-support equivalent in the bundled .app)."},
	{Key: "DA_PROVIDER", Kind: KindString, Default: "gemini", Group: GroupDataAnalyst, Help: "LLM provider for the analyst (gemini | anthropic | openai). Empty = let the first-run wizard / opencode auth state decide."},
	{Key: "DA_MODEL", Kind: KindString, Default: "gemini-3.1-flash-lite", Group: GroupDataAnalyst, Help: "Override model name for the analyst. Empty = provider default."},
	{Key: "DA_HANDSHAKE_TIMEOUT_SEC", Kind: KindInt, Default: 90, Group: GroupDataAnalyst, Help: "Per-call timeout for the opencode ACP session/new step (seconds, 30-300). Higher tolerates a slow /mcp tool-list under boot load. initialize is bounded separately at 15s."},

	// --- Transcript hub ---
	{Key: "TRANSCRIPT_PROMPT_LINES", Kind: KindInt, Default: 25, Group: GroupLogging, Help: "Recent transcript events injected into LLM prompts"},
	{Key: "TRANSCRIPT_TOOL_LIMIT", Kind: KindInt, Default: 200, Group: GroupLogging, Help: "Default cap on /api/transcript pulls"},
	{Key: "TRANSCRIPT_RETENTION_SESSIONS", Kind: KindInt, Default: 30, Group: GroupLogging, Help: "Session files kept on disk before oldest is rotated"},

	// --- Audio ducking ---
	// Lower the volume of external music players (Spotify, Apple Music,
	// browser tabs, VLC, etc.) while the race engineer speaks, then
	// restore. Cross-platform via per-OS native code:
	//   macOS  — AppleScript to Spotify + Music (browser tabs unaffected)
	//   Linux  — pactl set-sink-input-volume (PulseAudio + PipeWire)
	//   Windows — WASAPI IAudioSessionControl per-process session volume
	{Key: "AUDIO_DUCKING_ENABLED", Kind: KindBool, Default: false, Group: GroupAudio, Help: "Duck external music players while the engineer is speaking"},
	{Key: "AUDIO_DUCKING_MODE", Kind: KindString, Default: "duck", Group: GroupAudio, Help: "duck = lower volume to AUDIO_DUCKING_LEVEL; pause = pause the player and resume after (song doesn't progress). pause uses AppleScript on macOS, MPRIS playerctl on Linux, and the system media key on Windows."},
	{Key: "AUDIO_DUCKING_LEVEL", Kind: KindFloat, Default: 0.3, Group: GroupAudio, Help: "Target volume (0.0-1.0) for ducked players during speech (ignored in pause mode)"},
	{Key: "AUDIO_DUCKING_TARGETS", Kind: KindString, Default: "", Group: GroupAudio, Help: "Comma-separated app names to duck. Empty = OS default list. macOS: Spotify,Music. Windows: spotify.exe,chrome.exe,firefox.exe,msedge.exe,vlc.exe. Linux: spotify,firefox,chromium,vlc"},
	{Key: "AUDIO_DUCKING_TAIL_MS", Kind: KindInt, Default: 800, Group: GroupAudio, Help: "Tail-time (ms) after last Live audio chunk before un-ducking. Larger = fewer false toggles, slower restore."},
}

// schemaIndex is the by-key lookup map.  Built lazily; tests that mutate
// Schema after process start should call ResetSchemaIndex() between runs.
var schemaIndex map[string]*Field

func buildSchemaIndex() {
	schemaIndex = make(map[string]*Field, len(Schema))
	for i := range Schema {
		schemaIndex[Schema[i].Key] = &Schema[i]
	}
}

// FieldFor returns the schema entry for a key, or nil if not declared.
func FieldFor(key string) *Field {
	if schemaIndex == nil {
		buildSchemaIndex()
	}
	return schemaIndex[key]
}

// SchemaKeys returns the declared key names in Schema order.
func SchemaKeys() []string {
	out := make([]string, 0, len(Schema))
	for _, f := range Schema {
		out = append(out, f.Key)
	}
	return out
}

// ResetSchemaIndex is exposed for tests that swap Schema contents at runtime.
func ResetSchemaIndex() { schemaIndex = nil }

// SchemaProviderKey returns the API-key field that should be treated as
// required for the named LLM provider.  Returns "" when the provider is
// unknown or has no secret.
func SchemaProviderKey(provider string) string {
	switch provider {
	case "anthropic", "claude":
		return "ANTHROPIC_API_KEY"
	case "openai":
		return "OPENAI_API_KEY"
	case "gemini", "":
		return "GEMINI_API_KEY"
	}
	return ""
}
