package api

import (
	"fmt"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/insights"
)

// proximity_handler.go — /api/state/proximity for the Live agent.
//
// Reads ProximityWatcher's atomic snapshot (1 Hz updates) and returns
// a headline + sorted ahead/behind lists with closing rates and
// time-to-impact. The agent uses this to answer "anything close
// behind", "who's catching me", "who am I about to catch", etc.
//
// The watcher publishes empty snapshots in pit lane / garage so the
// agent reliably gets [] (rather than stale data) when proximity is
// meaningless.

type proximityResponse struct {
	Headline          string                 `json:"headline"`
	GeneratedAt       string                 `json:"generated_at"`
	PlayerCarIndex    uint8                  `json:"player_car_index"`
	PlayerLapDistance float32                `json:"player_lap_distance_m"`
	PlayerSpeedKmh    uint16                 `json:"player_speed_kmh"`
	WatchRadiusM      float32                `json:"watch_radius_m"`
	NearbyAhead       []insights.NearbyCar   `json:"nearby_ahead"`
	NearbyBehind      []insights.NearbyCar   `json:"nearby_behind"`
	ClosestThreat     *insights.NearbyCar    `json:"closest_threat,omitempty"`
}

// proximityHandler powers GET /api/state/proximity.
func proximityHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if deps.Proximity == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "proximity watcher not running",
			})
		}
		snap := deps.Proximity.Snapshot()
		if snap == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "no proximity data yet",
			})
		}

		// Closest threat is the closest BEHIND car that's actively closing.
		// Useful summary for the agent — one field to check first.
		var closestThreat *insights.NearbyCar
		for i := range snap.NearbyBehind {
			cand := &snap.NearbyBehind[i]
			if cand.ClosingMps < 1 {
				continue
			}
			if closestThreat == nil || cand.EtaSec < closestThreat.EtaSec || (closestThreat.EtaSec < 0 && cand.EtaSec >= 0) {
				closestThreat = cand
			}
		}

		return c.JSON(proximityResponse{
			Headline:          buildProximityHeadline(snap, closestThreat),
			GeneratedAt:       snap.GeneratedAt.Format("15:04:05.000"),
			PlayerCarIndex:    snap.PlayerCarIndex,
			PlayerLapDistance: snap.PlayerLapDistance,
			PlayerSpeedKmh:    snap.PlayerSpeedKmh,
			WatchRadiusM:      snap.WatchRadiusM,
			NearbyAhead:       snap.NearbyAhead,
			NearbyBehind:      snap.NearbyBehind,
			ClosestThreat:     closestThreat,
		})
	}
}

// buildProximityHeadline writes the one-line TL;DR the model speaks
// almost verbatim. Highlights the closest threat (if any), else the
// closest car behind / ahead. Uses driver surnames when available so
// the headline reads as natural radio.
func buildProximityHeadline(snap *insights.ProximitySnapshot, threat *insights.NearbyCar) string {
	if threat != nil {
		return fmt.Sprintf("Threat: %s %.0fm behind, closing at %.0fkm/h, ~%.1fs.",
			carLabel(threat), threat.DistanceM, threat.ClosingMps*3.6, threat.EtaSec)
	}
	parts := []string{}
	if len(snap.NearbyBehind) > 0 {
		b := snap.NearbyBehind[0]
		parts = append(parts, fmt.Sprintf("nearest behind: %s %.0fm (%s)",
			carLabel(&b), b.DistanceM, b.Threat))
	}
	if len(snap.NearbyAhead) > 0 {
		a := snap.NearbyAhead[0]
		parts = append(parts, fmt.Sprintf("nearest ahead: %s %.0fm (%s)",
			carLabel(&a), a.DistanceM, a.Threat))
	}
	if len(parts) == 0 {
		return "All clear — no cars within watch range."
	}
	return "No threats — " + strings.Join(parts, ", ") + "."
}

// carLabel returns the driver surname when known, else "P{N}" if a
// position is set, else "#{car_index}". Keeps the radio headline
// natural even when the roster hasn't been populated yet.
func carLabel(c *insights.NearbyCar) string {
	if c.DriverName != "" {
		return c.DriverName
	}
	if c.Position > 0 {
		return fmt.Sprintf("P%d", c.Position)
	}
	return fmt.Sprintf("#%d", c.CarIndex)
}
