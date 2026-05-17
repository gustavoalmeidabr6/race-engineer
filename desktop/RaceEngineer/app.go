package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

//go:embed bin/telemetry-core
var coreBinary []byte

// opencodeBinary is the bundled `opencode acp` runtime the Data Analyst
// spawns. Downloaded + checksummed via `make download-opencode`. When the
// file is absent (e.g. local dev builds before the make target runs) the
// embed evaluates to an empty slice and the analyst auto-disables — the
// rest of the app still boots.
//
//go:embed bin/opencode
var opencodeBinary []byte

//go:embed all:workspace-seed
var workspaceSeed embed.FS

//go:embed all:da-workspace-seed
var daWorkspaceSeed embed.FS

// App owns the lifecycle of the embedded telemetry-core child process.
type App struct {
	ctx     context.Context
	cmd     *exec.Cmd
	dataDir string
}

func NewApp() *App { return &App{} }

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	if err := a.boot(); err != nil {
		log.Printf("telemetry-core boot failed: %v", err)
	}
}

func (a *App) shutdown(ctx context.Context) {
	if a.cmd == nil || a.cmd.Process == nil {
		return
	}
	log.Printf("stopping telemetry-core pid=%d", a.cmd.Process.Pid)
	_ = a.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- a.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = a.cmd.Process.Kill()
	}
}

// boot extracts the embedded binary + workspace seed into Application
// Support, then spawns telemetry-core pointing at that workspace. The
// dashboard (already loaded by Wails) will reach the server on :8081.
func (a *App) boot() error {
	support, err := os.UserConfigDir()
	if err != nil {
		return fmt.Errorf("locate Application Support: %w", err)
	}
	a.dataDir = filepath.Join(support, "RaceEngineer")
	workspaceDir := filepath.Join(a.dataDir, "workspace")
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		return fmt.Errorf("mkdir workspace: %w", err)
	}
	if err := seedWorkspace(workspaceDir); err != nil {
		return fmt.Errorf("seed workspace: %w", err)
	}

	binPath := filepath.Join(a.dataDir, "telemetry-core")
	if err := writeBinaryIfChanged(binPath, coreBinary); err != nil {
		return fmt.Errorf("install binary: %w", err)
	}

	// Extract the bundled opencode binary + seed the analyst's workspace.
	// Both paths are best-effort: if opencode isn't bundled (length 0 — the
	// `make download-opencode` step wasn't run) the analyst will degrade
	// gracefully and the rest of the app keeps working.
	opencodePath := ""
	if len(opencodeBinary) > 0 {
		opencodePath = filepath.Join(a.dataDir, "opencode")
		if err := writeBinaryIfChanged(opencodePath, opencodeBinary); err != nil {
			log.Printf("install opencode failed: %v — Data Analyst will be disabled", err)
			opencodePath = ""
		}
	} else {
		log.Printf("opencode binary not embedded (build with `make download-opencode` first) — Data Analyst will be disabled")
	}
	daWorkspaceDir := filepath.Join(a.dataDir, "da-workspace")
	if err := seedDAWorkspace(daWorkspaceDir); err != nil {
		log.Printf("seed da-workspace failed: %v", err)
	}

	apiKey := readAPIKey(a.dataDir)
	if apiKey == "" {
		log.Printf("GEMINI_API_KEY not configured. Create %s/.env with GEMINI_API_KEY=...", a.dataDir)
	}

	// If another telemetry-core is already serving :8081 (typically
	// because the user is running `make start` in a terminal at the same
	// time), don't spawn a duplicate — the second one would race to bind
	// the port, lose, and crash. Just attach to the running server; the
	// dashboard talks to localhost:8081 either way.
	if alreadyRunning() {
		log.Printf("telemetry-core already running on :8081 — using existing instance, NOT spawning a child")
		return nil
	}

	a.cmd = exec.CommandContext(a.ctx, binPath)
	// Pin cwd to the data dir so any relative path in the JSON config
	// (notably the schema default DB_PATH=workspace/telemetry.duckdb)
	// resolves under our own writable workspace, NOT under whatever the
	// launcher's cwd happens to be. Spotlight / Finder / Dock launches
	// give the parent cwd=/ — a relative DuckDB path then tries to create
	// /workspace/… and the server crashes silently before binding :8081,
	// which is what produced the ServerGate retry storm. Terminal launches
	// only worked because they inherited the shell's writable cwd.
	a.cmd.Dir = a.dataDir
	a.cmd.Env = append(os.Environ(),
		// Point the Go config loader at our own isolated config file
		// instead of the user's ~/.race-engineer/config.json. That dev
		// file (if it exists) may carry absolute DB paths or experimental
		// flags that don't apply to the bundled app — isolating it
		// guarantees the .app behaves identically on a fresh user
		// machine and on a dev's machine.
		"RACE_ENGINEER_CONFIG_PATH="+filepath.Join(a.dataDir, "config.json"),
		"DB_PATH="+filepath.Join(workspaceDir, "telemetry.duckdb"),
		"WORKSPACE_DIR="+workspaceDir,
		"GEMINI_API_KEY="+apiKey,
		"API_PORT=8081",
		"TELEMETRY_MODE="+envOr("TELEMETRY_MODE", "mock"),
		"VOICE_MODE="+envOr("VOICE_MODE", "live_only"),
		// Data Analyst: point telemetry-core at the extracted opencode
		// binary and the seeded da-workspace. Empty OPENCODE_BIN tells
		// the analyst package to fall back to PATH lookup (which will
		// likely fail in a sandboxed .app, and the runtime degrades
		// gracefully when it does).
		"OPENCODE_BIN="+opencodePath,
		"DA_WORKSPACE_DIR="+daWorkspaceDir,
	)
	a.cmd.Stdout = os.Stdout
	a.cmd.Stderr = os.Stderr
	if err := a.cmd.Start(); err != nil {
		return fmt.Errorf("spawn telemetry-core: %w", err)
	}
	log.Printf("telemetry-core started pid=%d data_dir=%s", a.cmd.Process.Pid, a.dataDir)

	// Block boot until the server answers /health (≤8s). Wails' OnStartup
	// runs synchronously w.r.t. the rest of the lifecycle, so the webview
	// only fires its first fetch *after* this returns — eliminating the
	// boot race that surfaced as "Failed to load" on /settings.
	a.waitReady()
	return nil
}

// waitReady polls /health and blocks until the server responds 200 or
// the deadline elapses. Logs success/failure either way.
func (a *App) waitReady() {
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		client := http.Client{Timeout: 500 * time.Millisecond}
		resp, err := client.Get("http://localhost:8081/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				log.Printf("telemetry-core ready on :8081 after %s", time.Since(deadline.Add(-8*time.Second)))
				return
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	log.Printf("telemetry-core did not become ready within 8s — dashboard may show errors briefly")
}

// DataDir is exposed to JS so the UI can show the user where their files live.
func (a *App) DataDir() string { return a.dataDir }

func writeBinaryIfChanged(dst string, data []byte) error {
	if existing, err := os.ReadFile(dst); err == nil && len(existing) == len(data) {
		return os.Chmod(dst, 0o755)
	}
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		return err
	}
	return os.Chmod(dst, 0o755)
}

// seedDAWorkspace mirrors seedWorkspace for the Data Analyst's own
// workspace (AGENTS.md, .agents/skills/, driver context, prompts). Same
// copy-don't-overwrite rule so user edits persist across upgrades.
func seedDAWorkspace(dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	return fs.WalkDir(daWorkspaceSeed, "da-workspace-seed", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(path, "da-workspace-seed")
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if _, err := os.Stat(target); err == nil {
			return nil
		}
		b, err := daWorkspaceSeed.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
}

func seedWorkspace(dst string) error {
	return fs.WalkDir(workspaceSeed, "workspace-seed", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(path, "workspace-seed")
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if _, err := os.Stat(target); err == nil {
			return nil // never overwrite — user may have edited
		}
		b, err := workspaceSeed.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
}

func readAPIKey(dir string) string {
	if v := os.Getenv("GEMINI_API_KEY"); v != "" {
		return v
	}
	b, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "GEMINI_API_KEY") {
			continue
		}
		_, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		return strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return ""
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// alreadyRunning probes :8081 and returns true if /health answers 200
// within a short timeout. Used to avoid spawning a duplicate
// telemetry-core when the user is also running `make start` in a
// terminal — both would race to bind the port and one would crash,
// triggering the ServerGate retry storm.
func alreadyRunning() bool {
	client := http.Client{Timeout: 300 * time.Millisecond}
	resp, err := client.Get("http://localhost:8081/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}
