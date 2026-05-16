package api

import (
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog/log"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/config"
)

// configKeyKind classifies each persistable key by:
//   - "live": the change is applied immediately via an atomic setter, no
//     restart needed.
//   - "static": the change is persisted to disk but only takes effect on the
//     next process restart. The UI surfaces this as a yellow badge.
type configKeyKind string

const (
	kindLive   configKeyKind = "live"
	kindStatic configKeyKind = "static"
)

// configKey describes one persistable env-style key. The set is the union of
// keys exposed in the Settings page; new sections should add entries here.
//
// Required keys are gated by the dashboard: until they have a non-empty value
// the UI shows a banner and the first-launch flow drops the user on the
// Settings page. The chosen LLM provider's API key is upgraded to Required at
// response build time (see requiredKeyFor) so the user only sees the key they
// actually need to fill in.
type configKey struct {
	Section  string
	Kind     configKeyKind
	Type     string // "string" | "int" | "bool" | "hex"
	Required bool   `json:",omitempty"`
}

// keyRegistry is the closed set of keys the dashboard may persist via
// /api/config. Anything outside this is rejected by POST.
var keyRegistry = map[string]configKey{
	// --- Telemetry ---
	"TELEMETRY_MODE":     {Section: "telemetry", Kind: kindLive, Type: "string"},
	"TELEMETRY_HOST":     {Section: "telemetry", Kind: kindLive, Type: "string"},
	"TELEMETRY_PORT":     {Section: "telemetry", Kind: kindLive, Type: "int"},
	"UDP_MODE":           {Section: "telemetry", Kind: kindLive, Type: "string"},
	"SAMPLE_RATE":        {Section: "telemetry", Kind: kindStatic, Type: "int"},
	"HIFREQ_SAMPLE_RATE": {Section: "telemetry", Kind: kindStatic, Type: "int"},
	"BATCH_SIZE":         {Section: "telemetry", Kind: kindStatic, Type: "int"},
	"WS_PUSH_RATE":       {Section: "telemetry", Kind: kindStatic, Type: "int"},
	"API_PORT":           {Section: "telemetry", Kind: kindStatic, Type: "string"},
	"DB_PATH":            {Section: "telemetry", Kind: kindStatic, Type: "string"},
	"WORKSPACE_DIR":      {Section: "telemetry", Kind: kindStatic, Type: "string"},

	// --- LLM ---
	"LLM_PROVIDER":      {Section: "llm", Kind: kindStatic, Type: "string", Required: true},
	"LLM_MODEL":         {Section: "llm", Kind: kindStatic, Type: "string"},
	"GEMINI_API_KEY":    {Section: "llm", Kind: kindStatic, Type: "string"},
	"ANTHROPIC_API_KEY": {Section: "llm", Kind: kindStatic, Type: "string"},
	"OPENAI_API_KEY":    {Section: "llm", Kind: kindStatic, Type: "string"},

	// --- Voice ---
	// Phase 3.1 collapsed the Python voice service into the Go binary.
	// TTS_VOICE selects a prebuilt Gemini voice (Kore / Puck / Charon /
	// Aoede etc.).
	// VOICE_MODE picks which voice-input path runs at boot:
	//   "live_only" (default) — only Gemini Live; STT skipped, /api/voice 503.
	//   "ptt_only"             — only legacy STT+CommsGate; Live factory off.
	//   "both"                 — both inputs wired, Live owns audio output.
	"TTS_VOICE":  {Section: "voice", Kind: kindStatic, Type: "string"},
	"VOICE_MODE": {Section: "voice", Kind: kindStatic, Type: "string"},

	// --- Push-to-Talk ---
	// All PTT keys are live: the stateWriter goroutine reads them from
	// *config.Config on each BUTN packet, so dashboard edits take effect
	// without a restart.
	"PTT_BUTTON":       {Section: "ptt", Kind: kindLive, Type: "hex"},
	"PTT_MODE":         {Section: "ptt", Kind: kindLive, Type: "string"},
	"PTT_TRIGGER":      {Section: "ptt", Kind: kindLive, Type: "string"},
	"PTT_BUTTON_START": {Section: "ptt", Kind: kindLive, Type: "hex"},
	"PTT_BUTTON_END":   {Section: "ptt", Kind: kindLive, Type: "hex"},
	"LOG_BUTTONS":      {Section: "ptt", Kind: kindLive, Type: "bool"},

	// --- Gemini Live ---
	"GEMINI_LIVE_ENABLED":         {Section: "gemini_live", Kind: kindStatic, Type: "bool"},
	"GEMINI_LIVE_MODEL":           {Section: "gemini_live", Kind: kindStatic, Type: "string"},
	"GEMINI_LIVE_BRAIN_MAX_CHARS": {Section: "gemini_live", Kind: kindStatic, Type: "int"},
	"GEMINI_LIVE_ANALYST_TIMEOUT": {Section: "gemini_live", Kind: kindStatic, Type: "int"},
	"GEMINI_LIVE_PTT_KEY":         {Section: "gemini_live", Kind: kindStatic, Type: "string"},
	"GEMINI_LIVE_PTT_SOURCE":      {Section: "gemini_live", Kind: kindStatic, Type: "string"},

	// --- Coaching ---
	"TALK_LEVEL":     {Section: "coaching", Kind: kindLive, Type: "int"},
	"VERBOSITY":      {Section: "coaching", Kind: kindLive, Type: "int"},
	// Lookahead seconds applied when set_corner_reminder omits the param.
	// Stored as a float string in the JSON file (kind "string" keeps the
	// existing coerceValue path simple — configtool's float kind handles
	// the typed write).
	"CORNER_REMINDER_DEFAULT_LOOKAHEAD_SEC": {Section: "coaching", Kind: kindStatic, Type: "string"},

	// --- Pi Agent ---
	"PI_AGENT_MODE":                {Section: "pi_agent", Kind: kindStatic, Type: "string"},
	"PI_AGENT_PROVIDER":            {Section: "pi_agent", Kind: kindStatic, Type: "string"},
	"PI_AGENT_MODEL":               {Section: "pi_agent", Kind: kindStatic, Type: "string"},
	"PI_AGENT_SPECIALIST_MODEL":    {Section: "pi_agent", Kind: kindStatic, Type: "string"},
	"PI_AGENT_MAX_PRIORITY":        {Section: "pi_agent", Kind: kindStatic, Type: "int"},
	"PI_AGENT_MCP_PATH":            {Section: "pi_agent", Kind: kindStatic, Type: "string"},
	"PI_AGENT_TRIGGER_TIMEOUT_SEC": {Section: "pi_agent", Kind: kindStatic, Type: "int"},

	// --- Transcript / Logging ---
	"LOG_LEVEL":                     {Section: "logging", Kind: kindStatic, Type: "string"},
	"TRANSCRIPT_PROMPT_LINES":       {Section: "logging", Kind: kindStatic, Type: "int"},
	"TRANSCRIPT_TOOL_LIMIT":         {Section: "logging", Kind: kindStatic, Type: "int"},
	"TRANSCRIPT_RETENTION_SESSIONS": {Section: "logging", Kind: kindStatic, Type: "int"},

	// --- Audio ducking ---
	// Static: changing the level/targets means rebuilding the Ducker,
	// so a restart is the simpler path. The dashboard surfaces the
	// yellow "restart required" badge automatically for kindStatic.
	// AUDIO_DUCKING_LEVEL is stored as "string" because keyRegistry has
	// no float Type — same workaround as CORNER_REMINDER_DEFAULT_LOOKAHEAD_SEC.
	"AUDIO_DUCKING_ENABLED": {Section: "audio", Kind: kindStatic, Type: "bool"},
	"AUDIO_DUCKING_MODE":    {Section: "audio", Kind: kindStatic, Type: "string"},
	"AUDIO_DUCKING_LEVEL":   {Section: "audio", Kind: kindStatic, Type: "string"},
	"AUDIO_DUCKING_TARGETS": {Section: "audio", Kind: kindStatic, Type: "string"},
	"AUDIO_DUCKING_TAIL_MS": {Section: "audio", Kind: kindStatic, Type: "int"},
}

// configResponse is the JSON returned by GET /api/config. The values map is
// secret-masked. file_keys lists which keys currently live in the persisted
// file (vs purely env-driven), so the UI can mark them as "saved here".
type configResponse struct {
	Path     string                  `json:"path"`
	Schema   map[string]configKey    `json:"schema"`
	Values   map[string]any          `json:"values"`
	FileKeys []string                `json:"file_keys"`
}

func getConfigHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		fileCfg, err := config.LoadFile()
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}
		path, _ := config.FilePath()

		// Build the merged value snapshot (file > env > runtime defaults).
		// Use the live Config struct as the runtime default source so the
		// values returned reflect what the server actually applied at boot.
		values := snapshotKnownKeys(deps, fileCfg)

		// Loopback callers (Python services on the same host) can request
		// raw secret values via ?reveal_secrets=1 so race_config.get()
		// returns useful strings.  External callers (dashboard) get the
		// masked view as before.
		reveal := c.Query("reveal_secrets") == "1" && isLoopback(c.IP())

		fileKeys := make([]string, 0, len(fileCfg))
		for k := range fileCfg {
			fileKeys = append(fileKeys, k)
		}

		// Resolve which provider key is "the" required one for the currently
		// selected LLM_PROVIDER. Required flags must reflect the value that
		// will be effective on the next restart, not what booted this run —
		// otherwise a user who just switched provider would still see the
		// old key marked Required. The persisted file is the source of
		// truth post-save; fall back to the runtime config for first launch.
		provider := strFromFile(fileCfg, "LLM_PROVIDER")
		if provider == "" {
			provider, _ = values["LLM_PROVIDER"].(string)
		}
		// Copy the registry so per-request overrides never mutate the global map.
		schema := make(map[string]configKey, len(keyRegistry))
		for k, v := range keyRegistry {
			schema[k] = v
		}
		if reqKey := requiredKeyForProvider(provider); reqKey != "" {
			meta := schema[reqKey]
			meta.Required = true
			schema[reqKey] = meta
		}

		outValues := values
		if !reveal {
			outValues = config.MaskSecrets(values)
		}

		resp := configResponse{
			Path:     path,
			Schema:   schema,
			Values:   outValues,
			FileKeys: fileKeys,
		}
		return c.JSON(resp)
	}
}

// isLoopback reports whether the request originated from 127.0.0.1 or ::1.
// Used to gate the ?reveal_secrets=1 path for in-process Python services.
func isLoopback(ip string) bool {
	if ip == "" {
		return false
	}
	if ip == "127.0.0.1" || ip == "::1" || strings.HasPrefix(ip, "127.") {
		return true
	}
	return false
}

// getConfigSchemaHandler powers GET /api/config/schema.  Returns the full
// schema as declared in config.Schema so callers (dashboard, Python services,
// future tooling) can render forms and validate input without hard-coding
// field names.
func getConfigSchemaHandler() fiber.Handler {
	return func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"fields":         config.Schema,
			"required_for":   provisionalRequired(),
		})
	}
}

// provisionalRequired emits a tiny map of provider → API-key field so the
// dashboard can mark the right secret as Required when the user flips
// LLM_PROVIDER without a server restart.
func provisionalRequired() map[string]string {
	return map[string]string{
		"gemini":    config.SchemaProviderKey("gemini"),
		"anthropic": config.SchemaProviderKey("anthropic"),
		"openai":    config.SchemaProviderKey("openai"),
	}
}

// requiredKeyForProvider returns the API-key registry name that becomes
// Required when the named provider is selected. Empty / unknown providers
// fall through to Gemini (the default) so the dashboard always has a key to
// nag for on first launch.
func requiredKeyForProvider(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "anthropic", "claude":
		return "ANTHROPIC_API_KEY"
	case "openai":
		return "OPENAI_API_KEY"
	case "gemini", "":
		return "GEMINI_API_KEY"
	}
	return ""
}

// strFromFile reads a string-coerced value from the persisted file map.
// Returns "" when the key is absent, nil, or holds a non-string type that
// can't be sensibly displayed (the snapshot path handles non-string types
// via the live Config struct anyway).
func strFromFile(fileCfg map[string]any, key string) string {
	v, ok := fileCfg[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// snapshotKnownKeys builds a values map from the live Config struct overlaid
// with the persisted file. Used by GET /api/config so the UI sees the same
// effective values the running server is using.
func snapshotKnownKeys(deps *Deps, fileCfg map[string]any) map[string]any {
	cfg := deps.Cfg
	values := map[string]any{
		"TELEMETRY_HOST":                cfg.RuntimeUDPHost(),
		"TELEMETRY_PORT":                cfg.RuntimeUDPPort(),
		"UDP_MODE":                      cfg.RuntimeUDPMode(),
		"SAMPLE_RATE":                   cfg.SampleRate,
		"HIFREQ_SAMPLE_RATE":            cfg.HifreqSampleRate,
		"BATCH_SIZE":                    cfg.BatchSize,
		"WS_PUSH_RATE":                  cfg.WSPushRate,
		"API_PORT":                      cfg.APIPort,
		"DB_PATH":                       cfg.DBPath,
		"WORKSPACE_DIR":                 cfg.WorkspaceDir,
		"LLM_PROVIDER":                  cfg.LLMProvider,
		"LLM_MODEL":                     cfg.LLMModel,
		"GEMINI_API_KEY":                cfg.GeminiAPIKey,
		"ANTHROPIC_API_KEY":             cfg.AnthropicAPIKey,
		"OPENAI_API_KEY":                cfg.OpenAIAPIKey,
		"TTS_VOICE":                     cfg.TTSVoice,
		"VOICE_MODE":                    cfg.VoiceMode,
		"PTT_BUTTON":                    formatHex(uint64(cfg.PTTButton())),
		"PTT_MODE":                      cfg.PTTMode(),
		"PTT_TRIGGER":                   cfg.PTTTrigger(),
		"PTT_BUTTON_START":              formatHex(uint64(cfg.PTTButtonStart())),
		"PTT_BUTTON_END":                formatHex(uint64(cfg.PTTButtonEnd())),
		"LOG_BUTTONS":                   cfg.LogButtons(),
		"GEMINI_LIVE_ENABLED":           cfg.GeminiLiveEnabled,
		"TALK_LEVEL":                    int(cfg.TalkLevel.Load()),
		"VERBOSITY":                     int(cfg.Verbosity.Load()),
		"TELEMETRY_MODE":                ifMock(cfg.MockMode.Load()),
		"PI_AGENT_MODE":                 cfg.PiAgentMode,
		"PI_AGENT_PROVIDER":             cfg.PiAgentProvider,
		"PI_AGENT_MODEL":                cfg.PiAgentModel,
		"PI_AGENT_SPECIALIST_MODEL":     cfg.PiAgentSpecialistModel,
		"PI_AGENT_MAX_PRIORITY":         cfg.PiAgentMaxPriority,
		"PI_AGENT_MCP_PATH":             cfg.PiAgentMCPPath,
		"PI_AGENT_TRIGGER_TIMEOUT_SEC":  cfg.PiAgentTriggerTimeoutSec,
		"LOG_LEVEL":                     cfg.LogLevel,
		"TRANSCRIPT_PROMPT_LINES":       cfg.TranscriptPromptLines,
		"TRANSCRIPT_TOOL_LIMIT":         cfg.TranscriptToolLimit,
		"TRANSCRIPT_RETENTION_SESSIONS": cfg.TranscriptRetention,
		"CORNER_REMINDER_DEFAULT_LOOKAHEAD_SEC": cfg.CornerReminderDefaultLookaheadSec,
	}

	// Voice / Gemini Live fields not on the Config struct fall through to
	// the file (or env) directly so their UI rows still populate.
	for k := range keyRegistry {
		if _, ok := values[k]; ok {
			continue
		}
		if v, ok := fileCfg[k]; ok {
			values[k] = v
		}
	}
	return values
}

func ifMock(b bool) string {
	if b {
		return "mock"
	}
	return "real"
}

func formatHex(n uint64) string {
	return "0x" + strings.ToUpper(strconv.FormatUint(n, 16))
}

// postConfigPayload is the body of POST /api/config. Patch keys map directly
// to env-style names; values may be string, number, or bool. nil values
// delete the key from the persisted file (revert to env / default).
type postConfigPayload struct {
	Patch map[string]any `json:"patch"`
}

func postConfigHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		var payload postConfigPayload
		if err := c.BodyParser(&payload); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON"})
		}
		if len(payload.Patch) == 0 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "patch is empty"})
		}

		// Validate every key against the registry; reject unknown keys so a
		// stray UI bug can't poison the file with arbitrary content.
		validated := map[string]any{}
		liveApply := map[string]any{}
		for rawKey, v := range payload.Patch {
			key := strings.TrimSpace(rawKey)
			meta, ok := keyRegistry[key]
			if !ok {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
					"error": "unknown config key: " + key,
				})
			}

			// Refuse to overwrite a secret with its returned mask.
			if config.IsSecretKey(key) {
				if s, ok := v.(string); ok && config.IsMaskedSecret(s) {
					continue
				}
			}

			// Type-coerce and reject obviously wrong shapes.
			coerced, err := coerceValue(meta.Type, v)
			if err != nil {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
					"error": key + ": " + err.Error(),
				})
			}
			validated[key] = coerced
			if meta.Kind == kindLive {
				liveApply[key] = coerced
			}
		}

		if err := config.SaveFile(validated); err != nil {
			log.Warn().Err(err).Msg("config save failed")
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}

		// Apply live keys immediately via atomic setters.
		applyLive(deps, liveApply)

		return c.JSON(fiber.Map{
			"status":      "saved",
			"saved_keys":  keysOf(validated),
			"applied_live": keysOf(liveApply),
		})
	}
}

func coerceValue(typ string, v any) (any, error) {
	switch typ {
	case "string":
		switch x := v.(type) {
		case string:
			return x, nil
		case nil:
			return nil, nil
		case float64:
			if x == float64(int64(x)) {
				return strconv.FormatInt(int64(x), 10), nil
			}
			return strconv.FormatFloat(x, 'f', -1, 64), nil
		case bool:
			if x {
				return "true", nil
			}
			return "false", nil
		}
		return nil, fiberErr("expected string")
	case "int":
		switch x := v.(type) {
		case float64:
			return int(x), nil
		case int:
			return x, nil
		case string:
			n, err := strconv.Atoi(x)
			if err != nil {
				return nil, err
			}
			return n, nil
		case nil:
			return nil, nil
		}
		return nil, fiberErr("expected number")
	case "bool":
		switch x := v.(type) {
		case bool:
			return x, nil
		case string:
			switch strings.ToLower(x) {
			case "true", "yes", "1":
				return true, nil
			case "false", "no", "0":
				return false, nil
			}
		case nil:
			return nil, nil
		}
		return nil, fiberErr("expected bool")
	case "hex":
		switch x := v.(type) {
		case string:
			n, err := strconv.ParseUint(x, 0, 32)
			if err != nil {
				return nil, err
			}
			// Round-trip back to a normalised "0x..." form so the file is
			// stable regardless of how the UI wrote it.
			return "0x" + strings.ToUpper(strconv.FormatUint(n, 16)), nil
		case float64:
			n := uint64(x)
			return "0x" + strings.ToUpper(strconv.FormatUint(n, 16)), nil
		case nil:
			return nil, nil
		}
		return nil, fiberErr("expected hex string")
	}
	return v, nil
}

type configErr struct{ msg string }

func (e *configErr) Error() string { return e.msg }
func fiberErr(msg string) error    { return &configErr{msg} }

// parseHexLive extracts a uint32 from the coerced patch value used by the
// live-apply path. coerceValue normalises hex inputs to "0x…" strings, but
// JSON numbers can also slip through as float64 when the dashboard ever
// sends a decimal literal — handle both for safety.
func parseHexLive(v any) (uint32, bool) {
	switch x := v.(type) {
	case string:
		n, err := strconv.ParseUint(x, 0, 32)
		if err != nil {
			return 0, false
		}
		return uint32(n), true
	case float64:
		return uint32(uint64(x)), true
	case int:
		return uint32(x), true
	}
	return 0, false
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// applyLive walks the patch and runs the matching atomic setter. Static keys
// are persisted by SaveFile and take effect on next process boot; nothing
// else to do here for them.
func applyLive(deps *Deps, patch map[string]any) {
	for k, v := range patch {
		switch k {
		case "TELEMETRY_MODE":
			if s, ok := v.(string); ok {
				deps.Cfg.SetMockMode(strings.EqualFold(s, "mock"))
			}
		case "TALK_LEVEL":
			if n, ok := v.(int); ok {
				deps.Cfg.SetTalkLevel(n)
			}
		case "VERBOSITY":
			if n, ok := v.(int); ok {
				deps.Cfg.SetVerbosity(n)
			}
		case "PTT_BUTTON":
			if n, ok := parseHexLive(v); ok {
				deps.Cfg.SetPTTButton(n)
			}
		case "PTT_BUTTON_START":
			if n, ok := parseHexLive(v); ok {
				deps.Cfg.SetPTTButtonStart(n)
			}
		case "PTT_BUTTON_END":
			if n, ok := parseHexLive(v); ok {
				deps.Cfg.SetPTTButtonEnd(n)
			}
		case "PTT_MODE":
			if s, ok := v.(string); ok {
				deps.Cfg.SetPTTMode(s)
			}
		case "PTT_TRIGGER":
			if s, ok := v.(string); ok {
				deps.Cfg.SetPTTTrigger(s)
			}
		case "LOG_BUTTONS":
			if b, ok := v.(bool); ok {
				deps.Cfg.SetLogButtons(b)
			}
		case "TELEMETRY_HOST", "TELEMETRY_PORT", "UDP_MODE":
			// UDP runtime change — collected as a single SetRuntimeUDP call.
			host := deps.Cfg.RuntimeUDPHost()
			port := deps.Cfg.RuntimeUDPPort()
			mode := deps.Cfg.RuntimeUDPMode()
			if v2, ok := patch["TELEMETRY_HOST"].(string); ok {
				host = v2
			}
			if v2, ok := patch["TELEMETRY_PORT"].(int); ok {
				port = v2
			}
			if v2, ok := patch["UDP_MODE"].(string); ok {
				mode = v2
			}
			deps.Cfg.SetRuntimeUDP(host, port, mode)
			// Avoid re-applying for the other UDP keys in this loop.
			delete(patch, "TELEMETRY_HOST")
			delete(patch, "TELEMETRY_PORT")
			delete(patch, "UDP_MODE")
		}
	}
}
