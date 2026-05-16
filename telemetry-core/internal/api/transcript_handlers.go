package api

import (
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/transcript"
)

// transcript_handlers.go owns the per-session interaction-history HTTP
// surface. The Live agent (Python) posts tool_call / tool_result via
// POST /api/transcript; analysts and dashboards GET /api/transcript to
// read recent dialogue. /api/transcript/sessions lists session files.
//
// Producers in Go call deps.Transcript.Append() directly — the HTTP
// route exists for cross-process producers (Python Live agent, future
// CLI tools).

// transcriptIngestHandler accepts a transcript event from an external
// producer and forwards it to the in-process hub. Same shape as the
// JSONL line on disk.
//
//	POST /api/transcript
//	{"kind":"tool_call","actor":"live_agent","text":"get_race_state",
//	 "meta":{"latency_ms":12,"args":{"...":"..."}}}
func transcriptIngestHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.Transcript == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "transcript hub not initialised",
			})
		}
		var payload struct {
			At    time.Time      `json:"at"`
			Kind  string         `json:"kind"`
			Actor string         `json:"actor"`
			Text  string         `json:"text"`
			Meta  map[string]any `json:"meta"`
		}
		if err := c.BodyParser(&payload); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "invalid JSON",
			})
		}
		ev := transcript.Event{
			At:    payload.At,
			Kind:  transcript.Kind(strings.TrimSpace(payload.Kind)),
			Actor: strings.TrimSpace(payload.Actor),
			Text:  payload.Text,
			Meta:  payload.Meta,
		}
		if err := ev.Validate(); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": err.Error(),
			})
		}
		if !deps.Transcript.Append(ev) {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "hub busy",
			})
		}
		return c.Status(fiber.StatusAccepted).JSON(fiber.Map{
			"status":     "queued",
			"session_id": deps.Transcript.CurrentSession(),
		})
	}
}

// transcriptListHandler returns transcript events for the requested scope.
//
//	GET /api/transcript?scope=current&limit=50&kinds=engineer_speech,tool_call&since_minutes=10
//
// scope:           current | previous | all | <session_id>     (default current)
// limit:           int, capped by the hub's ToolLimit            (default ToolLimit)
// kinds:           comma-separated Kind filter                   (optional)
// since_minutes:   trim events older than now - N minutes        (optional)
//
// Body shape: {"session_id": "...", "events": [Event, ...]}
func transcriptListHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.Transcript == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "transcript hub not initialised",
			})
		}
		scope := c.Query("scope", "current")
		limit, _ := strconv.Atoi(c.Query("limit", "0"))

		var kinds []transcript.Kind
		if raw := strings.TrimSpace(c.Query("kinds")); raw != "" {
			for _, p := range strings.Split(raw, ",") {
				k := transcript.Kind(strings.TrimSpace(p))
				if !transcript.IsKnown(k) {
					return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
						"error": "unknown kind: " + string(k),
					})
				}
				kinds = append(kinds, k)
			}
		}

		var since time.Duration
		if raw := strings.TrimSpace(c.Query("since_minutes")); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
					"error": "since_minutes must be a non-negative integer",
				})
			}
			since = time.Duration(n) * time.Minute
		}

		events, err := deps.Transcript.Load(scope, limit, kinds, since)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": err.Error(),
			})
		}
		if events == nil {
			events = []transcript.Event{}
		}
		// Resolve the scope so the response advertises which session file
		// was actually read, not just the live session. Failures here are
		// non-fatal — Load already succeeded.
		resolved, _ := deps.Transcript.ResolveScope(scope)
		if resolved == "" {
			resolved = deps.Transcript.CurrentSession()
		}
		return c.JSON(fiber.Map{
			"session_id":      resolved,
			"current_session": deps.Transcript.CurrentSession(),
			"scope":           scope,
			"events":          events,
		})
	}
}

// transcriptSessionsHandler lists session metadata.
//
//	GET /api/transcript/sessions
func transcriptSessionsHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.Transcript == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "transcript hub not initialised",
			})
		}
		sessions, err := deps.Transcript.Sessions()
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": err.Error(),
			})
		}
		if sessions == nil {
			sessions = []transcript.SessionMeta{}
		}
		return c.JSON(fiber.Map{
			"current": deps.Transcript.CurrentSession(),
			"sessions": sessions,
		})
	}
}
