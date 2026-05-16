package config

import (
	"testing"
)

// TestLoad_VoiceModeResolution exercises the precedence rules baked into
// Load(): explicit VOICE_MODE wins, legacy GEMINI_LIVE_ENABLED=true falls
// back to "both", everything else defaults to "live_only".
func TestLoad_VoiceModeResolution(t *testing.T) {
	cases := []struct {
		name            string
		voiceMode       string // VOICE_MODE env value ("" = unset for the test)
		geminiLive      string // GEMINI_LIVE_ENABLED env value ("" = unset)
		wantVoiceMode   string
		wantLiveEnabled bool
		wantPTTEnabled  bool
	}{
		{
			name:            "default — no envs → live_only",
			wantVoiceMode:   "live_only",
			wantLiveEnabled: true,
			wantPTTEnabled:  false,
		},
		{
			name:            "explicit live_only",
			voiceMode:       "live_only",
			wantVoiceMode:   "live_only",
			wantLiveEnabled: true,
			wantPTTEnabled:  false,
		},
		{
			name:            "explicit ptt_only",
			voiceMode:       "ptt_only",
			wantVoiceMode:   "ptt_only",
			wantLiveEnabled: false,
			wantPTTEnabled:  true,
		},
		{
			name:            "explicit both",
			voiceMode:       "both",
			wantVoiceMode:   "both",
			wantLiveEnabled: true,
			wantPTTEnabled:  true,
		},
		{
			name:            "legacy GEMINI_LIVE_ENABLED=true → both",
			geminiLive:      "true",
			wantVoiceMode:   "both",
			wantLiveEnabled: true,
			wantPTTEnabled:  true,
		},
		{
			name:            "legacy GEMINI_LIVE_ENABLED=false → live_only (the new default)",
			geminiLive:      "false",
			wantVoiceMode:   "live_only",
			wantLiveEnabled: true,
			wantPTTEnabled:  false,
		},
		{
			name:            "explicit VOICE_MODE wins over legacy flag",
			voiceMode:       "ptt_only",
			geminiLive:      "true",
			wantVoiceMode:   "ptt_only",
			wantLiveEnabled: false,
			wantPTTEnabled:  true,
		},
		{
			name:            "unknown VOICE_MODE → live_only fallback",
			voiceMode:       "garbage",
			wantVoiceMode:   "live_only",
			wantLiveEnabled: true,
			wantPTTEnabled:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Point HOME at an empty dir so LoadFile() doesn't pick up
			// the developer's real ~/.race-engineer/config.json. Strict-
			// by-default means env vars are masked once schema defaults
			// have been seeded into the file; the test writes the values
			// under test directly to the file to match production.
			t.Setenv("HOME", t.TempDir())
			t.Setenv("GEMINI_API_KEY", "stub")

			patch := map[string]any{}
			if tc.voiceMode != "" {
				patch["VOICE_MODE"] = tc.voiceMode
			} else {
				// Empty string represents "key absent from file" — the
				// default VOICE_MODE seeded by Load() is "live_only", but
				// the legacy GEMINI_LIVE_ENABLED branch only triggers when
				// VOICE_MODE is unset. Delete it from the seeded file by
				// writing nil after the seed runs implicitly inside Load.
				patch["VOICE_MODE"] = nil
			}
			if tc.geminiLive != "" {
				patch["GEMINI_LIVE_ENABLED"] = tc.geminiLive == "true"
			} else {
				patch["GEMINI_LIVE_ENABLED"] = nil
			}
			// SaveFile is called *before* Load so the file exists with
			// the test's values; Load's SeedDefaults will fill in the
			// rest of the schema but won't overwrite what's already there.
			if _, _, err := SeedDefaults(); err != nil {
				t.Fatalf("seed defaults: %v", err)
			}
			if err := SaveFile(patch); err != nil {
				t.Fatalf("save test patch: %v", err)
			}

			cfg := Load()
			if cfg.VoiceMode != tc.wantVoiceMode {
				t.Errorf("VoiceMode = %q, want %q", cfg.VoiceMode, tc.wantVoiceMode)
			}
			if cfg.GeminiLiveEnabled != tc.wantLiveEnabled {
				t.Errorf("GeminiLiveEnabled = %v, want %v", cfg.GeminiLiveEnabled, tc.wantLiveEnabled)
			}
			if cfg.LivePathEnabled() != tc.wantLiveEnabled {
				t.Errorf("LivePathEnabled() = %v, want %v", cfg.LivePathEnabled(), tc.wantLiveEnabled)
			}
			if cfg.PTTPathEnabled() != tc.wantPTTEnabled {
				t.Errorf("PTTPathEnabled() = %v, want %v", cfg.PTTPathEnabled(), tc.wantPTTEnabled)
			}
		})
	}
}
