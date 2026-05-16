package api

import (
	"os"
	"path/filepath"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog/log"
)

// RestartSentinelFile is the marker the Go server drops inside WorkspaceDir
// before exiting in response to POST /api/server/restart. The Node
// supervisor (scripts/start.mjs in Phase 2) watches for this file and, when
// it sees the file together with a clean child exit, restarts every process
// in the launch set instead of treating the exit as a shutdown.
//
// In Phase 1 the supervisor side does not exist yet — the bash start.sh
// uses `concurrently --kill-others`, so writing the sentinel before exit is
// harmless but the operator has to re-run `make start` manually. The Phase 2
// launcher consumes this file end-to-end.
const RestartSentinelFile = ".restart"

// shutdownFn is the abstraction lifecycle_handlers depends on so unit tests
// can drive postRestartHandler without booting a real Fiber app + exiting
// the process. The production wiring in NewServer plugs in app.Shutdown +
// os.Exit; tests inject a fake that records the call.
type shutdownFn func() error

// exitFn lets tests intercept the process termination side-effect that the
// production handler triggers via os.Exit(0).
type exitFn func(code int)

// postRestartHandler returns a Fiber handler that:
//  1. Writes a .restart sentinel into the workspace directory.
//  2. Returns a 202 JSON ack so the dashboard can show "Restarting…".
//  3. Asynchronously (after a short flush delay) shuts the HTTP server down
//     and exits the process with code 0.
//
// The supervisor's restart-on-clean-exit-when-sentinel-present rule then
// brings everything back up. If the supervisor isn't watching (Phase 1 bash
// path), the sentinel is left on disk and removed on next clean start so it
// doesn't accidentally re-trigger on the *next* shutdown.
func postRestartHandler(deps *Deps, shutdown shutdownFn, exit exitFn) fiber.Handler {
	if exit == nil {
		exit = os.Exit
	}
	return func(c *fiber.Ctx) error {
		workspaceDir := deps.Cfg.WorkspaceDir
		if workspaceDir == "" {
			workspaceDir = "workspace"
		}
		sentinel := filepath.Join(workspaceDir, RestartSentinelFile)

		// Best-effort: the supervisor only cares about sentinel presence,
		// not its content. If the workspace dir is read-only we still want
		// the dashboard to learn the restart is happening, so don't fail
		// the request on a write error.
		if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
			log.Warn().Err(err).Str("dir", workspaceDir).Msg("restart: workspace mkdir failed")
		}
		if err := os.WriteFile(sentinel, []byte("1"), 0o644); err != nil {
			log.Warn().Err(err).Str("file", sentinel).Msg("restart: sentinel write failed")
		} else {
			log.Info().Str("file", sentinel).Msg("restart: sentinel written")
		}

		go func() {
			// Let Fiber flush the response before we tear the server down.
			// 200ms is conservative; the response is a few dozen bytes.
			time.Sleep(200 * time.Millisecond)
			if shutdown != nil {
				if err := shutdown(); err != nil {
					log.Warn().Err(err).Msg("restart: shutdown error")
				}
			}
			log.Info().Msg("restart: exiting for supervisor restart")
			exit(0)
		}()

		return c.Status(fiber.StatusAccepted).JSON(fiber.Map{
			"status":   "restarting",
			"sentinel": sentinel,
		})
	}
}

// ClearRestartSentinel removes a stale .restart file if one exists, so the
// supervisor's "did the child exit with the sentinel present?" check reflects
// only the current run. Called once at server startup. Errors other than
// "not exist" are logged but non-fatal — the supervisor is robust to a
// noisy sentinel.
func ClearRestartSentinel(workspaceDir string) {
	if workspaceDir == "" {
		workspaceDir = "workspace"
	}
	sentinel := filepath.Join(workspaceDir, RestartSentinelFile)
	if err := os.Remove(sentinel); err != nil && !os.IsNotExist(err) {
		log.Warn().Err(err).Str("file", sentinel).Msg("restart: failed to clear stale sentinel")
	}
}
