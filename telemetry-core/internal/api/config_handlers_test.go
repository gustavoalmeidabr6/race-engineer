package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/config"
)

// configResponseTest mirrors the wire shape of configResponse for assertions.
// The handler returns Schema as a map[string]configKey but JSON tags on
// configKey itself control field names; use a permissive shape here so a
// future tag rename doesn't silently break the test.
type configResponseTest struct {
	Path     string                    `json:"path"`
	Schema   map[string]map[string]any `json:"schema"`
	Values   map[string]any            `json:"values"`
	FileKeys []string                  `json:"file_keys"`
}

func newConfigApp(t *testing.T, deps *Deps) *fiber.App {
	t.Helper()
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Get("/api/config", getConfigHandler(deps))
	return app
}

// withTempHome forces config.FilePath/LoadFile/SaveFile to write into a
// temp HOME so the test never touches the developer's real
// ~/.race-engineer/config.json.
func withTempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func TestRequiredKeyForProvider(t *testing.T) {
	cases := []struct {
		provider string
		want     string
	}{
		{"gemini", "GEMINI_API_KEY"},
		{"GEMINI", "GEMINI_API_KEY"},
		{" gemini ", "GEMINI_API_KEY"},
		{"", "GEMINI_API_KEY"},
		{"anthropic", "ANTHROPIC_API_KEY"},
		{"claude", "ANTHROPIC_API_KEY"},
		{"openai", "OPENAI_API_KEY"},
		{"OpenAI", "OPENAI_API_KEY"},
		{"groq", ""},
	}
	for _, tc := range cases {
		got := requiredKeyForProvider(tc.provider)
		if got != tc.want {
			t.Errorf("requiredKeyForProvider(%q) = %q, want %q", tc.provider, got, tc.want)
		}
	}
}

func TestGetConfig_FilePromotionWinsOverCfg(t *testing.T) {
	// The user just saved LLM_PROVIDER=openai but the running process still
	// has cfg.LLMProvider=gemini (boot value). The Required flag must follow
	// the file so the dashboard's "what to fill in" reflects the next-restart
	// state, not the previous-run state.
	home := withTempHome(t)
	must(t, os.MkdirAll(filepath.Join(home, ".race-engineer"), 0o755))
	must(t, os.WriteFile(
		filepath.Join(home, ".race-engineer", "config.json"),
		[]byte(`{"LLM_PROVIDER":"openai"}`),
		0o600,
	))

	cfg := &config.Config{LLMProvider: "gemini"} // boot value lags
	app := newConfigApp(t, &Deps{Cfg: cfg})
	code, body := doReq(t, app, "GET", "/api/config", nil)
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	var resp configResponseTest
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if !asBool(resp.Schema["OPENAI_API_KEY"]["Required"]) {
		t.Errorf("OPENAI_API_KEY should be Required after file override")
	}
	if asBool(resp.Schema["GEMINI_API_KEY"]["Required"]) {
		t.Errorf("GEMINI_API_KEY should NOT be Required when file says openai")
	}
}

func TestGetConfig_PromotesGeminiKeyToRequired(t *testing.T) {
	withTempHome(t)
	cfg := &config.Config{LLMProvider: "gemini"}
	app := newConfigApp(t, &Deps{Cfg: cfg})

	code, body := doReq(t, app, "GET", "/api/config", nil)
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}

	var resp configResponseTest
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("invalid json: %v", err)
	}

	if !asBool(resp.Schema["LLM_PROVIDER"]["Required"]) {
		t.Errorf("LLM_PROVIDER should always be Required")
	}
	if !asBool(resp.Schema["GEMINI_API_KEY"]["Required"]) {
		t.Errorf("GEMINI_API_KEY should be Required when provider=gemini")
	}
	if asBool(resp.Schema["ANTHROPIC_API_KEY"]["Required"]) {
		t.Errorf("ANTHROPIC_API_KEY should NOT be Required when provider=gemini")
	}
	if asBool(resp.Schema["OPENAI_API_KEY"]["Required"]) {
		t.Errorf("OPENAI_API_KEY should NOT be Required when provider=gemini")
	}
}

func TestGetConfig_PromotesAnthropicKeyWhenSelected(t *testing.T) {
	home := withTempHome(t)
	// Persist the provider override in the file so the merged values map
	// reflects what the dashboard would see for an Anthropic user.
	must(t, os.MkdirAll(filepath.Join(home, ".race-engineer"), 0o755))
	must(t, os.WriteFile(
		filepath.Join(home, ".race-engineer", "config.json"),
		[]byte(`{"LLM_PROVIDER":"anthropic"}`),
		0o600,
	))

	cfg := &config.Config{LLMProvider: "anthropic"}
	app := newConfigApp(t, &Deps{Cfg: cfg})
	code, body := doReq(t, app, "GET", "/api/config", nil)
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	var resp configResponseTest
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if !asBool(resp.Schema["ANTHROPIC_API_KEY"]["Required"]) {
		t.Errorf("ANTHROPIC_API_KEY should be Required when provider=anthropic")
	}
	if asBool(resp.Schema["GEMINI_API_KEY"]["Required"]) {
		t.Errorf("GEMINI_API_KEY should not be Required when provider=anthropic")
	}
}

func TestGetConfig_DoesNotMutateGlobalRegistry(t *testing.T) {
	// Promote OpenAI Required via a request, then assert the global
	// keyRegistry still has Required=false for OPENAI_API_KEY. Without
	// the per-request schema copy this would leak across requests.
	withTempHome(t)
	cfg := &config.Config{LLMProvider: "openai"}
	app := newConfigApp(t, &Deps{Cfg: cfg})
	if code, body := doReq(t, app, "GET", "/api/config", nil); code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	if keyRegistry["OPENAI_API_KEY"].Required {
		t.Fatalf("global keyRegistry was mutated — OPENAI_API_KEY.Required leaked")
	}
}

// helpers

func asBool(v any) bool {
	b, _ := v.(bool)
	return b
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
