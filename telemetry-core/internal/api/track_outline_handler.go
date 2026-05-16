package api

import (
	"context"
	"fmt"
	"time"

	"github.com/gofiber/fiber/v2"
)

// track_outline_handler.go answers "trace the racing line for this track so
// the dashboard can draw a 2D top-down map." Returns world (x,z) samples
// from a single completed lap, downsampled by track_position bucket so a
// ~5km lap collapses to ~1000 points (every ~0.5%).
//
//	GET /api/track/outline
//	  ?session_uid=…   (optional; falls back to live cache, then most-recent
//	                    completed lap at the current track_id)
//
// Response:
//   {
//     "track_id":     7,
//     "session_uid":  "11097…",
//     "points": [{"lap_distance_m": 0.0, "x": 123.4, "z": -567.8}, …],
//     "note":         "from previous session"  // present when fallback fires
//   }
//
// Empty `points: []` is a normal cold-start result (no lap completed yet).
// The frontend renders a placeholder ring in that case.

type outlinePoint struct {
	LapDistanceM float64 `json:"lap_distance_m"`
	X            float64 `json:"x"`
	Z            float64 `json:"z"`
}

type trackOutlineResponse struct {
	TrackID     int            `json:"track_id"`
	SessionUID  string         `json:"session_uid"`
	TrackLength int            `json:"track_length_m"`
	Points      []outlinePoint `json:"points"`
	Note        string         `json:"note,omitempty"`
}

func trackOutlineHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		ctx := newLapCtx(c, deps)
		state, err := ctx.resolve()
		if err != nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": err.Error()})
		}

		resp := trackOutlineResponse{
			TrackID:     state.trackID,
			SessionUID:  uidString(state.sessionUID),
			TrackLength: state.trackLength,
			Points:      []outlinePoint{},
		}

		dbCtx, cancel := context.WithTimeout(c.Context(), 5*time.Second)
		defer cancel()

		// Try the requested session first.
		pts, err := fetchOutline(dbCtx, deps, state.sessionUID, state.trackLength)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}
		if len(pts) > 0 {
			resp.Points = pts
			return c.JSON(resp)
		}

		// Fallback: most recent completed lap at the same track_id, from any
		// session. The same physical track gives the same racing line, so a
		// previous race's outline is a perfectly good starting render until
		// the current session completes its first lap.
		fbUID, fbPts, err := fetchOutlineFallback(dbCtx, deps, state.trackID, state.sessionUID, state.trackLength)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}
		if len(fbPts) > 0 {
			resp.Points = fbPts
			resp.Note = fmt.Sprintf("from previous session %s", uidString(fbUID))
			return c.JSON(resp)
		}

		// Final fallback: the curated centerline from workspace/tracks/<id>.json.
		// Lets mock mode + brand-new installs render the real track shape
		// before any lap has been driven.
		if deps.TrackMap != nil && state.trackID >= -128 && state.trackID <= 127 {
			info := deps.TrackMap.Lookup(int8(state.trackID))
			if info != nil && len(info.Centerline) > 0 {
				resp.Points = make([]outlinePoint, 0, len(info.Centerline))
				for _, p := range info.Centerline {
					resp.Points = append(resp.Points, outlinePoint{
						LapDistanceM: float64(p.LapDistanceM),
						X:            float64(p.X),
						Z:            float64(p.Z),
					})
				}
				resp.Note = "curated centerline"
			}
		}
		return c.JSON(resp)
	}
}

// fetchOutline pulls a downsampled world-position trace for the last
// completed lap of the given session. "Last completed" means MAX(lap) - 1
// so we don't grab the in-progress lap that has no samples past the
// current track_position. Bucketing by FLOOR(track_position * 2) gives one
// point every ~0.5m (then ORDER BY track_position keeps them around the
// lap). LIMIT inside the CTE keeps the result under a few thousand points
// even for outliers.
//
// trackLength is the curated lap length in metres; we require the chosen
// lap to cover at least 90% of it so a partial lap can't out-rank the
// curated centerline in the fallback chain. Pass 0 to disable the gate
// (e.g. when track_length is unknown).
func fetchOutline(ctx context.Context, deps *Deps, uid uint64, trackLength int) ([]outlinePoint, error) {
	if uid == 0 {
		return nil, nil
	}
	uidStr := uidString(uid)
	// Coverage gate: require MAX(track_position) - MIN(track_position) >=
	// 0.9 * track_length on the SAME ON-TRACK SUBSET that the outline is
	// actually drawn from (pit_status = 0). Without the pit_status filter,
	// a qualifying lap that sat in the pit lane for half its duration
	// looks like a full lap by raw track_position span, even though the
	// returned outline is just the brief out-lap sliver — partial garbage.
	coverageClause := ""
	if trackLength > 0 {
		coverageClause = fmt.Sprintf(
			" AND (MAX(track_position) - MIN(track_position)) >= %f",
			0.9*float64(trackLength),
		)
	}
	sql := fmt.Sprintf(`
WITH lap_counts AS (
  SELECT lap, COUNT(*) AS n
  FROM telemetry_hifreq
  WHERE session_uid = %s AND lap IS NOT NULL AND lap > 0
    AND pit_status = 0
    AND track_position IS NOT NULL
    AND world_pos_x IS NOT NULL
    AND world_pos_z IS NOT NULL
  GROUP BY lap
  HAVING COUNT(*) >= 100%s
),
pick AS (
  SELECT MAX(lap) AS chosen FROM lap_counts
),
src AS (
  SELECT track_position, world_pos_x, world_pos_z,
         ROW_NUMBER() OVER (
           PARTITION BY CAST(track_position * 2 AS INTEGER)
           ORDER BY timestamp DESC
         ) AS rn
  FROM telemetry_hifreq, pick
  WHERE session_uid = %s
    AND lap = pick.chosen
    AND track_position IS NOT NULL
    AND world_pos_x IS NOT NULL
    AND world_pos_z IS NOT NULL
    AND pit_status = 0
)
SELECT track_position, world_pos_x, world_pos_z
FROM src WHERE rn = 1
ORDER BY track_position`, uidStr, coverageClause, uidStr)

	rows, err := deps.Store.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	return rowsToOutline(rows), nil
}

// fetchOutlineFallback finds the most recent session at the same track_id
// that has at least one completed lap, and pulls that lap's outline. Used
// when the active session hasn't finished a lap yet so the map still
// renders the racing line for that physical track.
func fetchOutlineFallback(ctx context.Context, deps *Deps, trackID int, excludeUID uint64, trackLength int) (uint64, []outlinePoint, error) {
	pickSQL := fmt.Sprintf(`
SELECT h.session_uid, MAX(h.timestamp) AS ts
FROM telemetry_hifreq h
JOIN session_data s ON s.timestamp BETWEEN
        (SELECT MIN(timestamp) FROM telemetry_hifreq WHERE session_uid = h.session_uid)
    AND (SELECT MAX(timestamp) FROM telemetry_hifreq WHERE session_uid = h.session_uid)
WHERE s.track_id = %d
  AND h.session_uid != %s
  AND h.lap > 0
GROUP BY h.session_uid
HAVING COUNT(*) >= 200
ORDER BY ts DESC
LIMIT 1`, trackID, uidString(excludeUID))

	pickRows, err := deps.Store.Query(ctx, pickSQL)
	if err != nil {
		return 0, nil, err
	}
	if len(pickRows) == 0 {
		return 0, nil, nil
	}
	uid := toUint64(pickRows[0]["session_uid"])
	if uid == 0 {
		return 0, nil, nil
	}
	pts, err := fetchOutline(ctx, deps, uid, trackLength)
	if err != nil {
		return 0, nil, err
	}
	return uid, pts, nil
}

func rowsToOutline(rows []map[string]interface{}) []outlinePoint {
	out := make([]outlinePoint, 0, len(rows))
	for _, r := range rows {
		out = append(out, outlinePoint{
			LapDistanceM: toFloat(r["track_position"]),
			X:            toFloat(r["world_pos_x"]),
			Z:            toFloat(r["world_pos_z"]),
		})
	}
	return out
}
