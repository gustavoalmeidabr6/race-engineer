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
)

// piAgentRecentHandler returns the last N activities from the in-memory ring
// (cold-start payload for the Analyst Team tab so it never opens empty).
//
//	GET /api/pi_agent/recent?limit=200
func piAgentRecentHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.PiActivity == nil {
			return c.JSON(fiber.Map{"activities": []any{}, "enabled": false})
		}
		limit, _ := strconv.Atoi(c.Query("limit", "200"))
		acts := deps.PiActivity.Recent(limit)
		pending := []map[string]any{}
		if deps.PiAgent != nil {
			pending = deps.PiAgent.PendingJobs()
		}
		return c.JSON(fiber.Map{
			"enabled":      true,
			"mode":         deps.Cfg.PiAgentMode,
			"provider":     deps.Cfg.PiAgentProvider,
			"trigger_depth": triggerDepth(deps),
			"subscribers":  deps.PiActivity.SubscriberCount(),
			"pending_jobs": pending,
			"activities":   acts,
		})
	}
}

// piAgentStreamHandler streams every new pi-agent activity over Server-Sent
// Events. Mirrors the /api/debug/stream pattern.
//
//	GET /api/pi_agent/stream
func piAgentStreamHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.PiActivity == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "pi-agent activity hub not initialised",
			})
		}
		c.Set("Content-Type", "text/event-stream")
		c.Set("Cache-Control", "no-cache")
		c.Set("Connection", "keep-alive")
		c.Set("X-Accel-Buffering", "no")

		evCh, cancel := deps.PiActivity.Subscribe(128)

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
						log.Warn().Err(err).Msg("pi_agent/stream: marshal failed")
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

func triggerDepth(deps *Deps) int {
	if deps.PiAgent == nil {
		return 0
	}
	return deps.PiAgent.Depth()
}
