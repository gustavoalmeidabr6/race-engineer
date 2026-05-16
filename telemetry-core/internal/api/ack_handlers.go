package api

import (
	"errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/brain"
)

// ack_handlers.go — driver-copy lifecycle endpoints.
//
//   GET    /api/events/pending          queued + in_flight + awaiting_ack snapshot
//   POST   /api/events/:id/ack          confirm the driver copied an event
//   POST   /api/events/:id/nag          force a renag on an awaiting-ack event
//   POST   /api/driver/questions/addressed
//                                       mark an open driver utterance as addressed

// eventsPendingHandler returns the current bus state split by lifecycle
// bucket. Used by the dashboard's Comms Queue panel and by the live-agent
// list_awaiting_ack tool.
func eventsPendingHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.Brain == nil {
			return c.JSON(fiber.Map{
				"queued":        []brain.Event{},
				"in_flight":     []brain.Event{},
				"awaiting_ack":  []brain.Event{},
			})
		}
		all := deps.Brain.PeekEvents()
		queued := make([]brain.Event, 0)
		inFlight := make([]brain.Event, 0)
		awaitingAck := make([]brain.Event, 0)
		for _, e := range all {
			switch e.Status {
			case brain.StatusQueued:
				queued = append(queued, e)
			case brain.StatusInFlight:
				inFlight = append(inFlight, e)
			case brain.StatusAwaitingAck:
				awaitingAck = append(awaitingAck, e)
			}
		}
		return c.JSON(fiber.Map{
			"queued":       queued,
			"in_flight":    inFlight,
			"awaiting_ack": awaitingAck,
		})
	}
}

// eventConfirmAckHandler closes an awaiting-ack event with evidence of
// the driver's verbal copy. Used by the live-agent's confirm_event_copy
// tool AND a manual dashboard override.
//
//	POST /api/events/:id/ack
//	{"evidence":"copy that, boxing this lap"}
func eventConfirmAckHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.Brain == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "brain not initialised",
			})
		}
		id := strings.TrimSpace(c.Params("id"))
		if id == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "event id is required"})
		}
		var body struct {
			Evidence string `json:"evidence"`
		}
		// Empty body is allowed (manual ack with no evidence string).
		_ = c.BodyParser(&body)
		err := deps.Brain.ConfirmAck(id, strings.TrimSpace(body.Evidence))
		if errors.Is(err, brain.ErrEventNotFound) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": err.Error()})
		}
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"status": "acked", "event_id": id})
	}
}

// eventManualNagHandler forces a renag on an awaiting-ack event without
// waiting for the AckNagAfter quiet period. Used by the dashboard's
// "Re-send" button — most useful when the engineer realises the driver
// genuinely missed the first call.
//
//	POST /api/events/:id/nag
func eventManualNagHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.Brain == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "brain not initialised",
			})
		}
		id := strings.TrimSpace(c.Params("id"))
		if id == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "event id is required"})
		}
		err := deps.Brain.NagAck(id)
		if errors.Is(err, brain.ErrEventNotFound) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": err.Error()})
		}
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(fiber.Map{"status": "nagged", "event_id": id})
	}
}

// driverQuestionAddressedHandler flips the most-recent unaddressed
// driver utterance to addressed. Optional utterance_at narrows the
// match window; otherwise the newest unaddressed utterance wins.
//
//	POST /api/driver/questions/addressed
//	{"utterance_at":"2026-05-16T15:00:01Z","summary":"answered: gap 1.4s"}
func driverQuestionAddressedHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.Brain == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "brain not initialised",
			})
		}
		var body struct {
			UtteranceAt string `json:"utterance_at"`
			Summary     string `json:"summary"`
		}
		_ = c.BodyParser(&body)

		var at time.Time
		if s := strings.TrimSpace(body.UtteranceAt); s != "" {
			if parsed, err := time.Parse(time.RFC3339, s); err == nil {
				at = parsed
			}
		}
		matched := deps.Brain.MarkUtteranceAddressed(at, strings.TrimSpace(body.Summary))
		if matched.Text == "" {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
				"status":  "no_open_utterance",
				"message": "no unaddressed utterance found within the window",
			})
		}
		return c.JSON(fiber.Map{
			"status":  "addressed",
			"text":    matched.Text,
			"at":      matched.At,
			"reason":  matched.Reason,
		})
	}
}
