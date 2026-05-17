package analyst

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// LocateBinary returns the absolute path of an opencode binary the runtime
// should spawn. Precedence:
//
//  1. Explicit `binary` argument (e.g. extracted from the Wails .app bundle).
//  2. OPENCODE_BIN env var (escape hatch for power users / CI).
//  3. `opencode` on $PATH (the dev/Homebrew flow).
//
// The returned path is verified to exist + be executable, but the version
// probe is left to ProbeVersion so callers can decide whether a missing
// binary should disable the analyst or hard-fail.
func LocateBinary(binary string) (string, error) {
	candidates := []string{}
	if binary != "" {
		candidates = append(candidates, binary)
	}
	if v := strings.TrimSpace(os.Getenv("OPENCODE_BIN")); v != "" {
		candidates = append(candidates, v)
	}
	for _, c := range candidates {
		abs, err := filepath.Abs(c)
		if err != nil {
			continue
		}
		st, err := os.Stat(abs)
		if err != nil || st.IsDir() {
			continue
		}
		if st.Mode()&0o111 == 0 {
			// Not executable; chmod once on the user's behalf so the
			// Wails extraction path doesn't have to remember.
			_ = os.Chmod(abs, 0o755)
		}
		return abs, nil
	}
	// Fall back to PATH lookup.
	if p, err := exec.LookPath("opencode"); err == nil {
		return p, nil
	}
	return "", errors.New("opencode binary not found (set OPENCODE_BIN or install via brew install sst/opencode/opencode)")
}

// ProbeVersion runs `opencode --version` with a 5s timeout. The returned
// string is whatever opencode prints — typically a single semver line. An
// error means the binary is unusable (segfault, permission denied, missing
// dynamic linker, ...).
func ProbeVersion(ctx context.Context, binary string) (string, error) {
	if binary == "" {
		return "", errors.New("binary path is empty")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, binary, "--version")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
