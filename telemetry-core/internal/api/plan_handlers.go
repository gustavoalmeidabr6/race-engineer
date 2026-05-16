package api

import (
	"errors"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/plan"
)

// Plan handlers expose the per-session plan store over REST. The shape
// mirrors the in-memory store: the dashboard issues mutations and the
// LLM-side tools (Gemini Live, analyst) read the current plan to inform
// their reasoning. All writes flow through the store so the ticker and
// the persistence file stay consistent.

// planCurrentHandler returns the full active plan as JSON. Empty when no
// session yet or no entries.
//
//	GET /api/plan/current
//	→ {"session_uid": "...", "entries": [...], "created_at": "...", "updated_at": "..."}
func planCurrentHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.Plan == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "plan store not initialised",
			})
		}
		p := deps.Plan.Current()
		if p == nil {
			return c.JSON(fiber.Map{"session_uid": 0, "entries": []any{}})
		}
		return c.JSON(p)
	}
}

// planAddHandler adds a new entry. Returns 201 with the stored entry on
// success.
//
//	POST /api/plan/entries
//	body: {"kind":"pit_stop","trigger":{"type":"lap","lap":14},"notes":"hard tyres","payload":{"compound":"hard"}}
//	→ 201 {stored PlanEntry}
func planAddHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.Plan == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "plan store not initialised",
			})
		}
		var body struct {
			Kind      plan.EntryKind   `json:"kind"`
			Status    plan.EntryStatus `json:"status,omitempty"`
			Trigger   plan.Trigger     `json:"trigger"`
			Payload   map[string]any   `json:"payload,omitempty"`
			Notes     string           `json:"notes,omitempty"`
			CreatedBy string           `json:"created_by,omitempty"`
		}
		if err := c.BodyParser(&body); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "invalid JSON: " + err.Error(),
			})
		}
		actor := strings.TrimSpace(body.CreatedBy)
		if actor == "" {
			actor = "api"
		}
		entry := plan.PlanEntry{
			Kind:    body.Kind,
			Status:  body.Status,
			Trigger: body.Trigger,
			Payload: body.Payload,
			Notes:   strings.TrimSpace(body.Notes),
		}
		stored, err := deps.Plan.Add(entry, actor)
		if err != nil {
			if errors.Is(err, plan.ErrPlanInvalid) {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
			}
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}
		return c.Status(fiber.StatusCreated).JSON(stored)
	}
}

// planUpdateHandler patches an entry by id. Only fields present in the
// body are touched. Returns the updated entry on success.
//
//	PATCH /api/plan/entries/:id
//	body: {"status":"done","notes":"...","trigger":{...},"payload":{...}}
//	→ 200 {updated PlanEntry}
func planUpdateHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.Plan == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "plan store not initialised",
			})
		}
		id := c.Params("id")
		if id == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "id is required"})
		}
		var body struct {
			Kind    *plan.EntryKind   `json:"kind"`
			Status  *plan.EntryStatus `json:"status"`
			Trigger *plan.Trigger     `json:"trigger"`
			Payload map[string]any    `json:"payload"`
			Notes   *string           `json:"notes"`
		}
		// BodyParser tolerates an empty body, but we still need to
		// distinguish "field absent" from "field set to empty".
		if len(c.Body()) > 0 {
			if err := c.BodyParser(&body); err != nil {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON: " + err.Error()})
			}
		}
		stored, err := deps.Plan.Update(id, func(e *plan.PlanEntry) error {
			if body.Kind != nil {
				e.Kind = *body.Kind
			}
			if body.Status != nil {
				e.Status = *body.Status
			}
			if body.Trigger != nil {
				e.Trigger = *body.Trigger
			}
			if body.Payload != nil {
				e.Payload = body.Payload
			}
			if body.Notes != nil {
				e.Notes = strings.TrimSpace(*body.Notes)
			}
			return nil
		})
		if err != nil {
			if errors.Is(err, plan.ErrPlanEntryNotFound) {
				return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": err.Error()})
			}
			if errors.Is(err, plan.ErrPlanInvalid) {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
			}
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(stored)
	}
}

// planCancelHandler marks an entry as cancelled with an optional reason.
//
//	DELETE /api/plan/entries/:id?reason=...
//	→ 200 {cancelled PlanEntry}
func planCancelHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.Plan == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "plan store not initialised",
			})
		}
		id := c.Params("id")
		if id == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "id is required"})
		}
		reason := strings.TrimSpace(c.Query("reason"))
		stored, err := deps.Plan.Cancel(id, reason)
		if err != nil {
			if errors.Is(err, plan.ErrPlanEntryNotFound) {
				return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": err.Error()})
			}
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}
		return c.JSON(stored)
	}
}
