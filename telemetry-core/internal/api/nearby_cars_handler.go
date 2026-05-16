package api

import (
	"math"
	"sort"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/insights"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
)

// nearby_cars_handler.go — /api/state/nearby_cars for the Live agent + pi
// agent.
//
// proximity_handler.go answers "anyone close-and-closing within the
// ProximityWatcher's 2 km window?" — the wrong question when the driver
// asks "who's around me?" on a long Spa lap with a spread-out field.
// This handler answers the simpler spatial question instead: top N on
// track ahead + top N behind regardless of gap. Driver only / pit cars
// excluded. No event firing — pure read against the atomic RaceState.
//
// Cars inside the watcher's window are enriched with the watcher's
// pre-computed closing_mps / eta_sec / threat; cars outside fall back to
// closing_mps=0 / eta_sec=-1 / threat="unknown" so the agent can still
// see them on the spatial map even when the watcher hasn't seen them
// closely enough to score a closing rate.

const (
	nearbyCarsDefault = 5
	nearbyCarsMin     = 1
	nearbyCarsMax     = 15
)

type nearbyCarsResponse struct {
	GeneratedAt       string               `json:"generated_at"`
	PlayerCarIndex    uint8                `json:"player_car_index"`
	PlayerLapDistance float32              `json:"player_lap_distance_m"`
	NearbyAhead       []insights.NearbyCar `json:"nearby_ahead"`
	NearbyBehind      []insights.NearbyCar `json:"nearby_behind"`
	AheadCount        int                  `json:"ahead_count"`
	BehindCount       int                  `json:"behind_count"`
}

// nearbyCarsHandler powers GET /api/state/nearby_cars.
func nearbyCarsHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		raceState := deps.Store.Cache().Load()
		if raceState == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "No telemetry data received yet",
			})
		}
		if raceState.TrackLength == 0 {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "track length not yet known",
			})
		}

		nAhead := clampNearbyCount(c.QueryInt("ahead", nearbyCarsDefault))
		nBehind := clampNearbyCount(c.QueryInt("behind", nearbyCarsDefault))

		ahead, behind := selectNearbyCars(raceState, deps.Proximity, deps.Store.Roster())
		ahead = capList(ahead, nAhead)
		behind = capList(behind, nBehind)

		return c.JSON(nearbyCarsResponse{
			GeneratedAt:       time.Now().Format("15:04:05.000"),
			PlayerCarIndex:    raceState.PlayerCarIndex,
			PlayerLapDistance: raceState.LapDistance,
			NearbyAhead:       ahead,
			NearbyBehind:      behind,
			AheadCount:        len(ahead),
			BehindCount:       len(behind),
		})
	}
}

// rosterResolver is the minimal interface this handler needs from
// state.Roster. Keeps the function testable without dragging the full
// roster type into a fake.
type rosterResolver interface {
	ResolveRadioName(idx uint8) string
}

// selectNearbyCars walks every GridPosition slot, filters to on-track
// cars (excludes self / pit lane / garage / inactive slots), splits
// into ahead and behind by signed track delta, and sorts each side by
// absolute distance ascending. Enriches each entry with closing_mps /
// eta_sec / threat from the proximity watcher's snapshot when the car
// is within its tracked window.
func selectNearbyCars(state *models.RaceState, prox *insights.ProximityWatcher, roster rosterResolver) (ahead, behind []insights.NearbyCar) {
	lapLen := float32(state.TrackLength)
	half := lapLen / 2
	me := state.GridPositions[state.PlayerCarIndex]
	ahead = make([]insights.NearbyCar, 0, 22)
	behind = make([]insights.NearbyCar, 0, 22)

	for i := 0; i < 22; i++ {
		idx := uint8(i)
		if idx == state.PlayerCarIndex {
			continue
		}
		gp := state.GridPositions[i]
		if gp.ResultStatus < 2 {
			continue
		}
		if gp.PitStatus != 0 || gp.DriverStatus == 0 {
			continue
		}
		signed := insights.SignedTrackDelta(me.LapDistance, gp.LapDistance, lapLen, half)
		dist := float32(math.Abs(float64(signed)))

		nc := insights.NearbyCar{
			CarIndex:     idx,
			Position:     gp.Position,
			CurrentLap:   gp.CurrentLap,
			DistanceM:    dist,
			SignedM:      signed,
			ClosingMps:   0,
			EtaSec:       -1,
			Threat:       "unknown",
			PitStatus:    insights.PitStatusLabel(gp.PitStatus),
			DriverStatus: insights.DriverStatusLabel(gp.DriverStatus),
			LapDistanceM: gp.LapDistance,
		}
		if roster != nil {
			nc.DriverName = roster.ResolveRadioName(idx)
		}
		if prox != nil {
			if closing, eta, threat, _, ok := prox.LookupHistory(idx); ok {
				nc.ClosingMps = closing
				nc.EtaSec = eta
				nc.Threat = threat
			}
		}
		if signed >= 0 {
			ahead = append(ahead, nc)
		} else {
			behind = append(behind, nc)
		}
	}

	sort.Slice(ahead, func(i, j int) bool { return ahead[i].DistanceM < ahead[j].DistanceM })
	sort.Slice(behind, func(i, j int) bool { return behind[i].DistanceM < behind[j].DistanceM })
	return ahead, behind
}

func capList(list []insights.NearbyCar, n int) []insights.NearbyCar {
	if n <= 0 || len(list) <= n {
		return list
	}
	return list[:n]
}

func clampNearbyCount(v int) int {
	if v < nearbyCarsMin {
		return nearbyCarsMin
	}
	if v > nearbyCarsMax {
		return nearbyCarsMax
	}
	return v
}

