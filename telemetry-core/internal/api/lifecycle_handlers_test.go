package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/config"
)

func newAppForLifecycle(t *testing.T, shutdownCalled *atomic.Bool, exitCode *atomic.Int32) (*fiber.App, string, *Deps) {
	t.Helper()
	wsDir := t.TempDir()
	deps := &Deps{Cfg: &config.Config{WorkspaceDir: wsDir}}
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	shutdown := func() error {
		shutdownCalled.Store(true)
		return nil
	}
	exit := func(code int) {
		exitCode.Store(int32(code))
	}
	app.Post("/api/server/restart", postRestartHandler(deps, shutdown, exit))
	return app, wsDir, deps
}

func TestRestartHandler_WritesSentinelAndAccepts(t *testing.T) {
	var shutdownCalled atomic.Bool
	var exitCode atomic.Int32
	exitCode.Store(-1)
	app, wsDir, _ := newAppForLifecycle(t, &shutdownCalled, &exitCode)

	code, body := doReq(t, app, "POST", "/api/server/restart", nil)
	if code != fiber.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", code, body)
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("invalid json: %v\nbody: %s", err, body)
	}
	if payload["status"] != "restarting" {
		t.Errorf("expected status=restarting, got %v", payload["status"])
	}

	sentinelPath := filepath.Join(wsDir, RestartSentinelFile)
	if _, err := os.Stat(sentinelPath); err != nil {
		t.Fatalf("expected sentinel at %s, got err: %v", sentinelPath, err)
	}

	// The shutdown + exit fire on a 200ms timer inside the handler. Poll up
	// to a second so the assertion isn't racy on slow CI runners.
	if !waitFor(time.Second, func() bool {
		return shutdownCalled.Load() && exitCode.Load() == 0
	}) {
		t.Fatalf("expected shutdown+exit to fire; shutdown=%v exit=%d",
			shutdownCalled.Load(), exitCode.Load())
	}
}

func TestRestartHandler_WorkspaceMkdirCreated(t *testing.T) {
	// WorkspaceDir is a path that doesn't exist yet. The handler should
	// create it so the sentinel write succeeds — otherwise an operator who
	// deleted workspace/ for a clean run would lose the restart hook.
	var shutdownCalled atomic.Bool
	var exitCode atomic.Int32
	exitCode.Store(-1)
	parent := t.TempDir()
	wsDir := filepath.Join(parent, "missing")
	deps := &Deps{Cfg: &config.Config{WorkspaceDir: wsDir}}

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Post("/api/server/restart", postRestartHandler(
		deps,
		func() error { shutdownCalled.Store(true); return nil },
		func(code int) { exitCode.Store(int32(code)) },
	))

	code, _ := doReq(t, app, "POST", "/api/server/restart", nil)
	if code != fiber.StatusAccepted {
		t.Fatalf("expected 202, got %d", code)
	}
	if _, err := os.Stat(filepath.Join(wsDir, RestartSentinelFile)); err != nil {
		t.Fatalf("sentinel was not written into freshly-created dir: %v", err)
	}
}

func TestClearRestartSentinel_RemovesStaleFile(t *testing.T) {
	wsDir := t.TempDir()
	sentinel := filepath.Join(wsDir, RestartSentinelFile)
	if err := os.WriteFile(sentinel, []byte("1"), 0o644); err != nil {
		t.Fatalf("seed sentinel: %v", err)
	}
	ClearRestartSentinel(wsDir)
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Errorf("expected sentinel removed, stat err = %v", err)
	}
}

func TestClearRestartSentinel_MissingFileNoError(t *testing.T) {
	// No panic / no log fatal when sentinel never existed.
	ClearRestartSentinel(t.TempDir())
}
