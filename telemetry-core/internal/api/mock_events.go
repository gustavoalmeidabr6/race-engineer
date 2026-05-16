package api

import (
	"errors"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/brain"
)

// mockEventFixtures are canned Event payloads keyed by EventType. The
// dashboard's mock-mode "Event Simulator" buttons fire one of these into the
// Interrupts Bus so the Live agent's dispatchers + voice synthesis can be
// exercised without a real F1-25 session producing the trigger condition.
//
// Each fixture supplies summary / reasoning / debug_data following the same
// brevity contract real producers do — the Live agent voices the radio call
// from these facts. Source is "mock" so spoken-event audits can distinguish
// simulated from real events.
var mockEventFixtures = map[brain.EventType]brain.Event{
	brain.EventRedFlag: {
		Type:      brain.EventRedFlag,
		Priority:  5,
		Summary:   "Red flag — session suspended.",
		Reasoning: "Session paused mid-lap. Drop pace to delta and return to pit lane on the next opportunity.",
		DebugData: map[string]any{"session_paused": true},
		Source:    "mock",
	},
	brain.EventBoxNow: {
		Type:      brain.EventBoxNow,
		Priority:  5,
		Summary:   "Box this lap, box box box.",
		Reasoning: "Tire cliff hit on lap 22, FL wear 96%, pace dropped 1.4s vs lap 19 — pit immediately.",
		DebugData: map[string]any{"wear_fl": 96, "pace_delta_s": 1.4, "lap": 22},
		Source:    "mock",
	},
	brain.EventSafetyCar: {
		Type:      brain.EventSafetyCar,
		Priority:  4,
		Summary:   "Safety car deployed.",
		Reasoning: "Full safety car after incident at T7. Bunch up behind the SC, prepare for restart.",
		DebugData: map[string]any{"safety_car_status": 1},
		Source:    "mock",
	},
	brain.EventVSC: {
		Type:      brain.EventVSC,
		Priority:  4,
		Summary:   "Virtual safety car deployed.",
		Reasoning: "VSC active — hold delta time, no overtaking. Free pit stop window if you want it.",
		DebugData: map[string]any{"safety_car_status": 2},
		Source:    "mock",
	},
	brain.EventYellowFlag: {
		Type:      brain.EventYellowFlag,
		Priority:  4,
		Summary:   "Yellow flag, sector 2.",
		Reasoning: "Local yellow at T9 after off-track. Lift through the affected zone, no overtaking.",
		DebugData: map[string]any{"sector": 2, "turn": 9},
		Source:    "mock",
	},
	brain.EventCollisionAhead: {
		Type:      brain.EventCollisionAhead,
		Priority:  4,
		Summary:   "Collision ahead, debris on track.",
		Reasoning: "Two-car contact at T4 one corner ahead. Stay wide on entry, watch for stationary cars.",
		DebugData: map[string]any{"turn": 4, "vehicles": 2},
		Source:    "mock",
	},
	brain.EventThreatOvertake: {
		Type:      brain.EventThreatOvertake,
		Priority:  3,
		Summary:   "Verstappen P+1, gap 1.4s closing.",
		Reasoning: "Closing 0.3s/lap over last 2 laps. Pace 88.4s vs ours 88.7s — DRS train threat by lap end.",
		DebugData: map[string]any{"gap_s": 1.4, "delta_per_lap_s": -0.3, "his_lap_s": 88.4, "our_lap_s": 88.7},
		Source:    "mock",
	},
	brain.EventPitWindowOpen: {
		Type:      brain.EventPitWindowOpen,
		Priority:  3,
		Summary:   "Pit window open this lap.",
		Reasoning: "Ideal pit lap reached. Gap to traffic exit clear, undercut on Hamilton viable from here.",
		DebugData: map[string]any{"window_lap": 18},
		Source:    "mock",
	},
	brain.EventTireCliff: {
		Type:      brain.EventTireCliff,
		Priority:  3,
		Summary:   "Tire cliff in 2 laps.",
		Reasoning: "FL wear 78% rising 4%/lap. Pace already +0.3s vs lap 14. Window to box closes lap 22.",
		DebugData: map[string]any{"wear_fl": 78, "wear_rate_pct_per_lap": 4, "window_close_lap": 22},
		Source:    "mock",
	},
	brain.EventWeatherChange: {
		Type:      brain.EventWeatherChange,
		Priority:  3,
		Summary:   "Rain in 5 minutes, 70% probability.",
		Reasoning: "Forecast shows light rain band incoming. Track will go green-to-damp; inters viable in 8 mins.",
		DebugData: map[string]any{"rain_pct": 70, "eta_min": 5},
		Source:    "mock",
	},
	brain.EventLapSummary: {
		Type:      brain.EventLapSummary,
		Priority:  2,
		Summary:   "Lap 14 in 1:28.4. S2 +0.3 vs PB.",
		Reasoning: "S1 26.10, S2 33.02 (+0.3 vs PB), S3 29.30. Tire wear FL 42% RR 38%. Fuel on target.",
		DebugData: map[string]any{"lap_ms": 88421, "s1_ms": 26100, "s2_ms": 33020, "s3_ms": 29301},
		Source:    "mock",
	},
	brain.EventSectorPB: {
		Type:      brain.EventSectorPB,
		Priority:  2,
		Summary:   "Sector 2 personal best.",
		Reasoning: "S2 33.02, 0.4s up on previous PB. Tire and brake temps in window.",
		DebugData: map[string]any{"sector": 2, "delta_ms": -400},
		Source:    "mock",
	},
	brain.EventFastestLap: {
		Type:      brain.EventFastestLap,
		Priority:  2,
		Summary:   "Fastest lap of the race.",
		Reasoning: "1:28.4 — 0.2s up on Verstappen's previous best. Tires holding, push to the cliff.",
		DebugData: map[string]any{"lap_ms": 88421},
		Source:    "mock",
	},
	brain.EventInfo: {
		Type:      brain.EventInfo,
		Priority:  1,
		Summary:   "Mock info event for testing.",
		Reasoning: "Generic note from the analyst — lowest priority, only spoken on quiet stretches.",
		DebugData: map[string]any{"mock": true},
		Source:    "mock",
	},
}

// mockEventHandler accepts {"type":"<event_type>","priority":<optional>} and
// fires a canned Event onto the Interrupts Bus. Gated on cfg.MockMode so it
// can't be hit in real-session deployments. TalkLevel filter is bypassed
// (we pass 10) so testing isn't dependent on the dashboard's verbosity slider.
//
//	POST /api/mock/event
//	{"type":"red_flag"}                 → uses default priority for the type
//	{"type":"threat_overtake","priority":4}  → override priority (1–5)
//
// 202 → {"event_id":"evt_...","type":"...","priority":N}
// 400 → {"error":"..."} on unknown type or invalid priority
// 403 → {"error":"mock mode disabled"} when cfg.MockMode is false
func mockEventHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.Cfg == nil || !deps.Cfg.MockMode.Load() {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
				"error": "mock mode disabled",
			})
		}
		if deps.Brain == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "brain not initialised",
			})
		}

		var payload struct {
			Type     string `json:"type"`
			Priority int    `json:"priority"`
		}
		if err := c.BodyParser(&payload); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "invalid JSON: " + err.Error(),
			})
		}

		evType := brain.EventType(strings.TrimSpace(payload.Type))
		fixture, ok := mockEventFixtures[evType]
		if !ok {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "unknown event type: " + payload.Type,
			})
		}

		// Copy the fixture so concurrent requests can't observe a partially
		// mutated entry (priority override path).
		ev := fixture
		if payload.Priority != 0 {
			if payload.Priority < 1 || payload.Priority > 5 {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
					"error": "priority must be in [1,5]",
				})
			}
			ev.Priority = payload.Priority
		}

		// Bypass TalkLevel — testing should always reach the dispatchers.
		stored, err := deps.Brain.EnqueueEvent(ev, 10)
		switch {
		case err == nil:
			return c.Status(fiber.StatusAccepted).JSON(fiber.Map{
				"event_id": stored.ID,
				"type":     stored.Type,
				"priority": stored.Priority,
			})
		case errors.Is(err, brain.ErrEventDeduped):
			return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
				"error":  "deduped",
				"detail": err.Error(),
			})
		default:
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": err.Error(),
			})
		}
	}
}
