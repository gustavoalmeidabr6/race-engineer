package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// configFileDir / configFileName form the path ~/.race-engineer/config.json
// when joined with the user's home directory. Documented in the project plan;
// chosen over os.UserConfigDir() so the path is identical across macOS/Linux
// and matches the dotfile convention of the rest of the workspace tooling.
const (
	configFileDir  = ".race-engineer"
	configFileName = "config.json"
)

// secretKeys is derived from Schema (Field.Secret == true). Maintained as a
// package-level var so existing callers (MaskSecrets, IsSecretKey) keep
// working without paying the schema-walk on every call.
var secretKeys = buildSecretSet()

func buildSecretSet() map[string]struct{} {
	out := make(map[string]struct{}, len(Schema))
	for _, f := range Schema {
		if f.Secret {
			out[f.Key] = struct{}{}
		}
	}
	return out
}

var fileMu sync.Mutex

// FilePath returns the absolute path of the config file the persistence
// layer reads and writes. Honours the RACE_ENGINEER_CONFIG_PATH env var
// when set — used by the bundled desktop app to isolate its config from
// any dev `~/.race-engineer/config.json` that already exists on the
// user's machine. Returns ("", err) when the env var is unset AND the
// user home dir lookup fails.
func FilePath() (string, error) {
	if override := strings.TrimSpace(os.Getenv("RACE_ENGINEER_CONFIG_PATH")); override != "" {
		return override, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, configFileDir, configFileName), nil
}

// LoadFile reads the persisted config, returning an empty map (not an error)
// when the file does not yet exist. Other I/O or JSON errors propagate.
func LoadFile() (map[string]any, error) {
	path, err := FilePath()
	if err != nil {
		return map[string]any{}, nil
	}

	fileMu.Lock()
	defer fileMu.Unlock()

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) == 0 {
		return map[string]any{}, nil
	}

	out := map[string]any{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return out, nil
}

// SeedDefaults ensures every Schema field with a Default has an entry in
// the persisted config file. Existing values are NEVER overwritten — only
// missing keys are added. Returns the list of keys that were newly seeded
// so callers can log them, and a flag indicating whether the file was
// created from scratch.
//
// Called from Load() on every boot so the JSON file converges to a complete
// snapshot of the schema. Once seeded, the file is the single source of
// truth and env vars become an opt-in escape hatch.
func SeedDefaults() (added []string, created bool, err error) {
	current, err := LoadFile()
	if err != nil {
		return nil, false, err
	}
	path, perr := FilePath()
	if perr != nil {
		return nil, false, perr
	}
	_, statErr := os.Stat(path)
	created = errors.Is(statErr, fs.ErrNotExist)

	patch := map[string]any{}
	for _, f := range Schema {
		if f.Default == nil {
			continue
		}
		if _, ok := current[f.Key]; ok {
			continue
		}
		// Legacy compat: when seeding VOICE_MODE and the user has
		// GEMINI_LIVE_ENABLED=true (either persisted or via env), preserve
		// the previous "both inputs" behaviour instead of writing the new
		// "live_only" default. Without this, an existing user who ran with
		// GEMINI_LIVE_ENABLED=true silently loses the PTT fallback on
		// first boot after migration.
		if f.Key == "VOICE_MODE" {
			if legacyLiveEnabled(current) {
				patch[f.Key] = "both"
				added = append(added, f.Key)
				continue
			}
		}
		patch[f.Key] = f.Default
		added = append(added, f.Key)
	}
	if len(patch) == 0 {
		return nil, created, nil
	}
	if err := SaveFile(patch); err != nil {
		return nil, created, err
	}
	return added, created, nil
}

// legacyLiveEnabled reports whether GEMINI_LIVE_ENABLED resolves truthy
// in the file or (when env fallback is allowed) the environment. Used by
// SeedDefaults to preserve the old "both inputs" semantic when seeding
// VOICE_MODE for the first time.
func legacyLiveEnabled(current map[string]any) bool {
	if v, ok := current["GEMINI_LIVE_ENABLED"]; ok {
		switch x := v.(type) {
		case bool:
			return x
		case string:
			s := strings.ToLower(strings.TrimSpace(x))
			return s == "true" || s == "1" || s == "yes"
		}
	}
	if isTruthy(os.Getenv("GEMINI_LIVE_ENABLED")) {
		return true
	}
	return false
}

// SaveFile patches the persisted config: existing keys are kept, keys in
// `patch` are overwritten, and keys whose value is nil are deleted. Writes
// are atomic (temp file + rename) and the file is forced to mode 0600 since
// it stores API keys.
func SaveFile(patch map[string]any) error {
	path, err := FilePath()
	if err != nil {
		return err
	}

	fileMu.Lock()
	defer fileMu.Unlock()

	current := map[string]any{}
	if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
		if uerr := json.Unmarshal(data, &current); uerr != nil {
			return fmt.Errorf("existing config corrupted: %w", uerr)
		}
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read existing: %w", err)
	}

	for k, v := range patch {
		if v == nil {
			delete(current, k)
			continue
		}
		current[k] = v
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	out, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".config.json.tmp-*")
	if err != nil {
		return fmt.Errorf("tempfile: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// MaskSecrets returns a copy of cfg with secret values replaced by
// "•••• + last4". Empty secrets are returned as "" (not masked).
func MaskSecrets(cfg map[string]any) map[string]any {
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		if _, isSecret := secretKeys[k]; isSecret {
			s, _ := v.(string)
			out[k] = maskString(s)
			continue
		}
		out[k] = v
	}
	return out
}

func maskString(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 4 {
		return "••••"
	}
	return "••••" + s[len(s)-4:]
}

// IsMaskedSecret reports whether a string looks like a returned mask, in
// which case POST /api/config should not overwrite the existing secret with
// it. Defensive: only matches our exact prefix.
func IsMaskedSecret(s string) bool {
	if len(s) < 4 {
		return false
	}
	return s[:len("••••")] == "••••"
}

// IsSecretKey reports whether the named env-style key holds a secret value.
func IsSecretKey(key string) bool {
	_, ok := secretKeys[key]
	return ok
}
