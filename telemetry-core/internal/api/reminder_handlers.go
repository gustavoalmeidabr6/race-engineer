package api

import (
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/insights"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/trackmap"
)

// reminder_handlers.go — REST surface for the corner-reminder store the
// Live agent uses to schedule track-position coaching cues.
//
//   POST   /api/reminders/corner   set_corner_reminder
//   GET    /api/reminders          list_reminders
//   DELETE /api/reminders/:id      cancel_reminder
//
// Reminders are session-scoped and held in memory. Server restart drops
// them — the agent should re-set anything still relevant.

// setReminderRequest is the body shape from the Live agent's tool call.
//
// Exactly one trigger must be supplied:
//   - corner_id ("T12") — resolved against the loaded trackmap for the
//     current session's track_id.
//   - lap_distance_m — raw metres into the lap. Use for pit entry / DRS
//     zones / sector boundaries / any non-corner position cue. Pair with
//     `label` so the radio message identifies what the cue is about.
//   - at_lap — fire once at the START of the named lap. Use for "pit
//     this lap" / "switch fuel mix on lap 8" style cues. One-shot regardless
//     of the recurring flag (repeating a "lap N" cue is meaningless).
type setReminderRequest struct {
	CornerID         string  `json:"corner_id,omitempty"`
	LapDistanceM     float32 `json:"lap_distance_m,omitempty"`
	AtLap            uint8   `json:"at_lap,omitempty"`            // 1-255; fires once on entry to that lap
	Label            string  `json:"label,omitempty"`             // freeform display label for non-corner reminders
	Message          string  `json:"message"`
	LookaheadSeconds float32 `json:"lookahead_seconds,omitempty"` // default 1.5s (position-based only)
	Recurring        *bool   `json:"recurring,omitempty"`         // default true (position-based only)
	ExpiresInLaps    uint8   `json:"expires_in_laps,omitempty"`   // default 5
	Priority         int     `json:"priority,omitempty"`          // default 3
}

type setReminderResponse struct {
	ReminderID         string  `json:"reminder_id"`
	CornerID           string  `json:"corner_id,omitempty"`
	CornerName         string  `json:"corner_name,omitempty"`
	TriggerLapDistance float32 `json:"trigger_lap_distance_m"`
	EffectiveDistance  float32 `json:"effective_lap_distance_m"`
	LookaheadM         float32 `json:"lookahead_m"`
	ExpiresAtLap       uint8   `json:"expires_at_lap"`
	NextLap            uint8   `json:"next_lap"`
	Recurring          bool    `json:"recurring"`
	Priority           int     `json:"priority"`
}

// setReminderHandler powers POST /api/reminders/corner.
func setReminderHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		state := deps.Store.Cache().Load()
		if state == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "no telemetry yet — cannot set a reminder before a session is active",
			})
		}
		var req setReminderRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid JSON body: " + err.Error()})
		}
		req.Message = strings.TrimSpace(req.Message)
		if req.Message == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "message is required"})
		}
		if len(req.Message) > 80 {
			req.Message = req.Message[:80]
		}

		// Position-trigger contract:
		//   - corner_id and lap_distance_m are mutually exclusive (both
		//     resolve to a single trigger distance — picking both is
		//     ambiguous).
		//   - at_lap can stand alone OR combine with one position trigger
		//     to mean "fire at this position ON that lap, one-shot".
		if req.CornerID != "" && req.LapDistanceM > 0 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "corner_id and lap_distance_m are mutually exclusive — supply one (optionally combined with at_lap)",
			})
		}

		// Lap-only trigger short-circuit: no position resolution needed.
		if req.AtLap > 0 && req.CornerID == "" && req.LapDistanceM == 0 {
			return persistLapReminder(c, deps, state, req)
		}

		// Resolve trigger distance: corner_id takes precedence.
		var triggerDist float32
		var cornerID, cornerName string
		var resolvedCorner *trackmap.Corner

		if req.CornerID != "" {
			if deps.TrackMap == nil {
				return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
					"error": "no track geometry loaded; use lap_distance_m instead",
				})
			}
			track := deps.TrackMap.Lookup(state.TrackID)
			if track == nil {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
					"error": "no curated corner data for current track; use lap_distance_m instead",
				})
			}
			resolvedCorner = track.FindCorner(req.CornerID)
			if resolvedCorner == nil {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
					"error": "corner_id '" + req.CornerID + "' not found on " + track.Name,
				})
			}
			triggerDist = resolvedCorner.LapDistanceM
			cornerID = resolvedCorner.ID
			cornerName = resolvedCorner.Name
		} else if req.LapDistanceM > 0 {
			triggerDist = req.LapDistanceM
		} else {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "supply exactly one of corner_id, lap_distance_m, at_lap",
			})
		}

		// Validate against track length when known.
		if state.TrackLength > 0 && triggerDist > float32(state.TrackLength) {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "trigger distance is past end of lap",
			})
		}

		// Resolve lookahead: time→metres at current speed. Default comes
		// from CornerReminderDefaultLookaheadSec (Package C of the coaching
		// plan, bumped from 1.5s to 3.0s).  Falls back to 1.5s when Cfg is
		// somehow unset (tests).
		seconds := req.LookaheadSeconds
		if seconds <= 0 {
			seconds = 1.5
			if deps.Cfg != nil && deps.Cfg.CornerReminderDefaultLookaheadSec > 0 {
				seconds = float32(deps.Cfg.CornerReminderDefaultLookaheadSec)
			}
		}
		lookahead := insights.ComputeLookaheadM(seconds, state.Speed)
		effective := triggerDist - lookahead
		if effective < 0 {
			effective = 0
		}

		// Defaults.
		recurring := true
		if req.Recurring != nil {
			recurring = *req.Recurring
		}
		expiresInLaps := req.ExpiresInLaps
		if expiresInLaps == 0 {
			expiresInLaps = 5
		}
		priority := req.Priority
		if priority < 1 || priority > 5 {
			priority = 3
		}

		// Combined at_lap + position trigger: validate the target lap and
		// clamp the firing semantics. The combined cue is conceptually
		// one-shot ("fire at this position ONLY on lap N"), so we
		// override recurring → false and pin expiry to that lap (plus a
		// one-lap grace window so a missed exact tick still fires).
		if req.AtLap > 0 {
			if req.AtLap < state.CurrentLap {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
					"error": fmt.Sprintf("at_lap=%d is in the past (current lap %d)", req.AtLap, state.CurrentLap),
				})
			}
			recurring = false
		}

		expiresAtLap := state.CurrentLap + expiresInLaps
		// Clamp to uint8 max (overflow on long endurance sessions).
		if state.CurrentLap > 0 && expiresAtLap < state.CurrentLap {
			expiresAtLap = 255
		}
		// Combined trigger: tighten expiry to "target lap + grace" so a
		// stale cue can't haunt the rest of the stint. Operator-set
		// expires_in_laps still wins when it's tighter.
		if req.AtLap > 0 {
			tight := req.AtLap + 1
			if tight > req.AtLap && (expiresAtLap == 0 || tight < expiresAtLap) {
				expiresAtLap = tight
			}
		}

		// Package E soft dedup: if a non-expired reminder already exists for
		// the same corner with a similar message (same first 3 words),
		// return the existing entry rather than spamming the store every
		// analyst cycle. The store's Add() only dedupes on exact-equal
		// message, which doesn't survive LLM rephrasings.
		if cornerID != "" && deps.Reminders != nil {
			intent := messageIntent(req.Message)
			for _, existing := range deps.Reminders.List() {
				if existing.CornerID == cornerID &&
					existing.ExpiresAtLap >= state.CurrentLap &&
					messageIntent(existing.Message) == intent {
					return c.JSON(setReminderResponse{
						ReminderID:         existing.ID,
						CornerID:           existing.CornerID,
						CornerName:         existing.CornerName,
						TriggerLapDistance: existing.TriggerDistM,
						EffectiveDistance:  existing.EffectiveDistM,
						LookaheadM:         existing.LookaheadM,
						ExpiresAtLap:       existing.ExpiresAtLap,
						NextLap:            state.CurrentLap + 1,
						Recurring:          existing.Recurring,
						Priority:           existing.Priority,
					})
				}
			}
		}

		stored, err := deps.Reminders.Add(insights.Reminder{
			CornerID:       cornerID,
			CornerName:     cornerName,
			Label:          strings.TrimSpace(req.Label),
			TriggerDistM:   triggerDist,
			LookaheadM:     lookahead,
			EffectiveDistM: effective,
			TriggerAtLap:   req.AtLap, // 0 when not combined
			Message:        req.Message,
			Priority:       priority,
			Recurring:      recurring,
			ExpiresAtLap:   expiresAtLap,
			LastFiredLap:   -1,
			CreatedAt:      time.Now(),
			CreatedAtLap:   state.CurrentLap,
		})
		if err != nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": err.Error(),
				"hint":  "cancel an existing reminder before adding a new one",
			})
		}

		nextLap := state.CurrentLap + 1
		if state.LapDistance < effective {
			nextLap = state.CurrentLap // we're still before the trigger this lap
		}
		// Combined trigger: the next-fire lap is the requested target.
		if req.AtLap > 0 {
			nextLap = req.AtLap
		}

		return c.JSON(setReminderResponse{
			ReminderID:         stored.ID,
			CornerID:           stored.CornerID,
			CornerName:         stored.CornerName,
			TriggerLapDistance: stored.TriggerDistM,
			EffectiveDistance:  stored.EffectiveDistM,
			LookaheadM:         stored.LookaheadM,
			ExpiresAtLap:       stored.ExpiresAtLap,
			NextLap:            nextLap,
			Recurring:          stored.Recurring,
			Priority:           stored.Priority,
		})
	}
}

// reminderTriggerKind classifies a stored reminder for callers that want
// to render trigger-type-aware UI without re-deriving the rules. Lap-based
// always wins because it never has a position.
func reminderTriggerKind(r *insights.Reminder) string {
	if r.TriggerAtLap > 0 {
		return "lap"
	}
	if r.CornerID != "" {
		return "corner"
	}
	return "distance"
}

// persistLapReminder is the at_lap branch of setReminderHandler. Keeps the
// main handler readable by splitting out the lap-based trigger path —
// no track-position math needed, just stash the target lap and let the
// watcher fire it on entry to that lap.
func persistLapReminder(c *fiber.Ctx, deps *Deps, state *models.RaceState, req setReminderRequest) error {
	if req.AtLap <= state.CurrentLap {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error": fmt.Sprintf("at_lap=%d is not in the future (current lap %d)", req.AtLap, state.CurrentLap),
		})
	}
	expiresInLaps := req.ExpiresInLaps
	if expiresInLaps == 0 {
		expiresInLaps = 5
	}
	priority := req.Priority
	if priority < 1 || priority > 5 {
		priority = 3
	}
	// Expire one lap after target so the watcher has a window to fire
	// even if the player skips a tick (rare but possible at low rates).
	expiresAtLap := req.AtLap + 1
	if expiresInLaps > 1 {
		expiresAtLap = req.AtLap + uint8(expiresInLaps)
	}
	if expiresAtLap < req.AtLap {
		expiresAtLap = 255 // overflow on long stints
	}
	stored, err := deps.Reminders.Add(insights.Reminder{
		Label:        strings.TrimSpace(req.Label),
		TriggerAtLap: req.AtLap,
		Message:      req.Message,
		Priority:     priority,
		Recurring:    false, // lap-based is always one-shot
		ExpiresAtLap: expiresAtLap,
		LastFiredLap: -1,
		CreatedAt:    time.Now(),
		CreatedAtLap: state.CurrentLap,
	})
	if err != nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
			"error": err.Error(),
			"hint":  "cancel an existing reminder before adding a new one",
		})
	}
	return c.JSON(fiber.Map{
		"reminder_id":    stored.ID,
		"label":          stored.Label,
		"trigger_at_lap": stored.TriggerAtLap,
		"next_lap":       stored.TriggerAtLap,
		"expires_at_lap": stored.ExpiresAtLap,
		"recurring":      stored.Recurring,
		"priority":       stored.Priority,
	})
}

// listRemindersHandler powers GET /api/reminders.
func listRemindersHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.Reminders == nil {
			return c.JSON([]any{})
		}
		state := deps.Store.Cache().Load()
		var currentLap uint8
		if state != nil {
			currentLap = state.CurrentLap
		}
		out := make([]fiber.Map, 0)
		for _, r := range deps.Reminders.List() {
			firesIn := int(r.ExpiresAtLap) - int(currentLap)
			out = append(out, fiber.Map{
				"reminder_id":              r.ID,
				"corner_id":                r.CornerID,
				"corner_name":              r.CornerName,
				"label":                    r.Label,
				"trigger_kind":             reminderTriggerKind(r),
				"trigger_lap_distance_m":   r.TriggerDistM,
				"effective_lap_distance_m": r.EffectiveDistM,
				"lookahead_m":              r.LookaheadM,
				"trigger_at_lap":           r.TriggerAtLap,
				"message":                  r.Message,
				"priority":                 r.Priority,
				"recurring":                r.Recurring,
				"expires_at_lap":           r.ExpiresAtLap,
				"laps_until_expiry":        firesIn,
				"last_fired_lap":           r.LastFiredLap,
				"created_at_lap":           r.CreatedAtLap,
			})
		}
		return c.JSON(out)
	}
}

// messageIntent returns the lowercased, punctuation-stripped first three
// words of a coaching message. Used by the Package E soft-dedup check: two
// messages that share the same intent ("brake earlier at T3" vs "brake
// earlier T3 chicane") collapse to one stored reminder so the analyst's
// per-cycle re-fires don't bloat the store.
func messageIntent(msg string) string {
	msg = strings.ToLower(strings.TrimSpace(msg))
	// Strip simple punctuation so "brake earlier!" matches "brake earlier".
	repl := strings.NewReplacer(",", " ", ".", " ", "!", " ", "?", " ", ":", " ", ";", " ")
	msg = repl.Replace(msg)
	fields := strings.Fields(msg)
	if len(fields) > 3 {
		fields = fields[:3]
	}
	return strings.Join(fields, " ")
}

// cancelReminderHandler powers DELETE /api/reminders/:id.
func cancelReminderHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		id := c.Params("id")
		if id == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "reminder id required"})
		}
		if deps.Reminders == nil || !deps.Reminders.Cancel(id) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "reminder not found"})
		}
		return c.JSON(fiber.Map{"reminder_id": id, "cancelled": true})
	}
}
