package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog/log"
	"github.com/valyala/fasthttp"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/analyst"
)

// analyst_handlers.go owns every /api/analyst/* route except /query and
// /jobs (which live in handlers.go for historical reasons). The shape
// mirrors the old pi-agent surface so the dashboard rewrite has a 1:1 map
// from the old hook output to the new one.

// analystRecentHandler returns the last N activities for cold-start.
//
//	GET /api/analyst/recent?limit=200
func analystRecentHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.Analyst == nil {
			return c.JSON(fiber.Map{"activities": []any{}, "enabled": false})
		}
		limit, _ := strconv.Atoi(c.Query("limit", "200"))
		return c.JSON(fiber.Map{
			"enabled":      true,
			"ready":        deps.Analyst.Ready(),
			"subscribers":  deps.Analyst.Hub().SubscriberCount(),
			"pending_jobs": deps.Analyst.PendingJobs(),
			"activities":   deps.Analyst.Hub().Recent(limit),
		})
	}
}

// analystStreamHandler streams every new analyst activity over SSE.
//
//	GET /api/analyst/stream
func analystStreamHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.Analyst == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "data analyst not initialised",
			})
		}
		c.Set("Content-Type", "text/event-stream")
		c.Set("Cache-Control", "no-cache")
		c.Set("Connection", "keep-alive")
		c.Set("X-Accel-Buffering", "no")

		hub := deps.Analyst.Hub()
		evCh, cancel := hub.Subscribe(128)

		c.Context().SetBodyStreamWriter(fasthttp.StreamWriter(func(w *bufio.Writer) {
			defer cancel()

			if _, err := w.WriteString(": connected\n\n"); err != nil {
				return
			}
			if err := w.Flush(); err != nil {
				return
			}

			ping := time.NewTicker(15 * time.Second)
			defer ping.Stop()

			for {
				select {
				case e, ok := <-evCh:
					if !ok {
						return
					}
					payload, err := json.Marshal(e)
					if err != nil {
						log.Warn().Err(err).Msg("analyst/stream: marshal failed")
						continue
					}
					if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
						return
					}
					if err := w.Flush(); err != nil {
						return
					}
				case <-ping.C:
					if _, err := w.WriteString(": ping\n\n"); err != nil {
						return
					}
					if err := w.Flush(); err != nil {
						return
					}
				}
			}
		}))
		return nil
	}
}

// analystStatusHandler exposes runtime metadata for the Settings page status
// banner (binary version, child pid, workspace path).
//
//	GET /api/analyst/status
func analystStatusHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.Analyst == nil {
			return c.JSON(fiber.Map{
				"installed": false,
				"ready":     false,
				"reason":    "analyst disabled (DA_ENABLED=false or opencode binary missing)",
			})
		}
		return c.JSON(deps.Analyst.Status())
	}
}

// analystRestartHandler tears down the opencode subprocess (if any) and
// re-runs the boot sequence. Lets the user recover a failed handshake from
// the Settings page without bouncing the whole server.
//
//	POST /api/analyst/restart
func analystRestartHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.Analyst == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "data analyst not initialised",
			})
		}
		if err := deps.Analyst.Restart(); err != nil {
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{
				"error": err.Error(),
			})
		}
		return c.JSON(fiber.Map{"ok": true})
	}
}

// ensureAnalystImported keeps the analyst import "used" even though most of
// this file references it via deps.Analyst's interface type — Go's import
// checker treats interface field references through *Deps as satisfied by
// the type alias, but having one direct reference here keeps maintenance
// edits from breaking the import.
var _ = analyst.ActivityKind("")
