package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog/log"
	"github.com/valyala/fasthttp"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/transcript"
)

// debugRecentHandler returns the most recent transcript events from the
// hub's in-memory ring. Used as the cold-start snapshot before the
// dashboard opens its SSE stream — guarantees the Live Debug tab is
// never empty.
//
//	GET /api/debug/recent?limit=200&kinds=engineer_speech,tool_call
//
// limit:  int, capped by the ring size                     (default 200)
// kinds:  comma-separated Kind filter                      (optional)
//
// Body shape: {"events": [Event, ...]}
func debugRecentHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.Transcript == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "transcript hub not initialised",
			})
		}
		limit, _ := strconv.Atoi(c.Query("limit", "0"))

		kinds, err := parseKinds(c.Query("kinds"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
		}

		events := deps.Transcript.Recent(limit, kinds)
		if events == nil {
			events = []transcript.Event{}
		}
		return c.JSON(fiber.Map{
			"session_id": deps.Transcript.CurrentSession(),
			"events":     events,
		})
	}
}

// debugStreamHandler streams every new transcript event over Server-Sent
// Events. The browser's EventSource handles reconnect for free. Each
// frame is `data: <json>\n\n`. A `: ping` comment fires every 15s to
// keep proxies from closing idle connections.
//
//	GET /api/debug/stream?kinds=tool_call,event_dispatched
//
// kinds: comma-separated Kind filter (optional) — applied server-side.
func debugStreamHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.Transcript == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "transcript hub not initialised",
			})
		}
		kinds, err := parseKinds(c.Query("kinds"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
		}
		kindSet := make(map[transcript.Kind]struct{}, len(kinds))
		for _, k := range kinds {
			kindSet[k] = struct{}{}
		}

		c.Set("Content-Type", "text/event-stream")
		c.Set("Cache-Control", "no-cache")
		c.Set("Connection", "keep-alive")
		c.Set("X-Accel-Buffering", "no") // disable nginx buffering if proxied

		// Subscribe to the hub *before* writing the first byte so we never
		// miss events between cold-start render and the stream opening.
		evCh, cancel := deps.Transcript.Subscribe(256)

		c.Context().SetBodyStreamWriter(fasthttp.StreamWriter(func(w *bufio.Writer) {
			defer cancel()

			// Initial comment so EventSource fires onopen immediately rather
			// than waiting for the first real event.
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
					if len(kindSet) > 0 {
						if _, accept := kindSet[e.Kind]; !accept {
							continue
						}
					}
					payload, err := json.Marshal(e)
					if err != nil {
						log.Warn().Err(err).Msg("debug/stream: marshal failed")
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

// parseKinds parses a comma-separated kinds query param and validates
// each entry against the closed enum. Returns nil + nil on empty input
// (meaning "accept all").
func parseKinds(raw string) ([]transcript.Kind, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var out []transcript.Kind
	for _, p := range strings.Split(raw, ",") {
		k := transcript.Kind(strings.TrimSpace(p))
		if k == "" {
			continue
		}
		if !transcript.IsKnown(k) {
			return nil, fmt.Errorf("unknown kind: %s", k)
		}
		out = append(out, k)
	}
	return out, nil
}
