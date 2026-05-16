package api

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"
)

// lap_delta_handler.go answers "where am I gaining or losing time across
// the lap, vs my best (or any chosen) reference lap, in milliseconds at
// every distance bucket". This is the single most actionable comparison
// view — drivers and AI tools both consume it to localise where laps
// diverge.
//
//	GET /api/laps/delta
//	  ?session_uid=…          (optional; live cache if absent)
//	  &lap=N|last             (default: most recent completed lap)
//	  &reference=best|N       (default: best valid lap of this session)
//	  &buckets=80             (range 20..200)
//
// Returns aligned arrays {distance_m[], delta_ms[]} where
// delta_ms[i] = your_elapsed_time_at_bucket_i - reference_elapsed_time_at_bucket_i.
// Negative = you're ahead of reference at that distance.

type lapDeltaResponse struct {
	SessionUID    string  `json:"session_uid"`
	TrackID       int     `json:"track_id"`
	TrackLengthM  float64 `json:"track_length_m"`
	Buckets       int     `json:"buckets"`
	BucketSizeM   float64 `json:"bucket_size_m"`
	Lap           int     `json:"lap"`
	LapTimeMs     int     `json:"lap_time_ms,omitempty"`
	// LapSessionUID is set when ?lap_session_uid= overrides the lap's
	// home session. Empty when the lap belongs to the resolving session.
	LapSessionUID       string    `json:"lap_session_uid,omitempty"`
	ReferenceLap        int       `json:"reference_lap"`
	ReferenceMs         int       `json:"reference_lap_time_ms,omitempty"`
	ReferenceSessionUID string    `json:"reference_session_uid,omitempty"`
	DistanceM           []float64 `json:"distance_m"`
	// DeltaMs is one entry per bucket. Pointer-slice so empty buckets
	// (no hi-freq sample fell in either lap) marshal as JSON null.
	DeltaMs []*int `json:"delta_ms"`
}

func lapDeltaHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		ctx := newLapCtx(c, deps)
		state, err := ctx.resolve()
		if err != nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": err.Error()})
		}
		trackLen := float64(state.trackLength)
		uid := state.sessionUID
		carIdx := state.playerCarIndex

		buckets := parseIntDefault(c.Query("buckets"), 80)
		if buckets < 20 {
			buckets = 20
		}
		if buckets > 200 {
			buckets = 200
		}
		bucketSize := trackLen / float64(buckets)

		// Cross-session overrides. When supplied, lap/reference are looked
		// up in the named session; track_length must match (otherwise the
		// bucket math is meaningless and the resulting delta would be a
		// lie). Empty / missing → fall back to the resolving session.
		yourUID, yourCar, err := resolveCrossSession(c, deps, "lap_session_uid", uid, carIdx, trackLen)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
		}
		refUID, refCar, err := resolveCrossSession(c, deps, "reference_session_uid", uid, carIdx, trackLen)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
		}

		// Resolve "your" lap (default = most recent completed).
		your, yourMs, err := resolveSingleLap(c, deps, yourUID, yourCar, c.Query("lap"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
		}
		if your == 0 {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "no completed laps in this session yet"})
		}

		// Resolve reference lap (default = best valid lap).
		ref, refMs, err := resolveReferenceLap(c, deps, refUID, refCar, c.Query("reference", "best"))
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
		}
		if ref == 0 {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "no reference lap available (need at least one valid completed lap)"})
		}

		// Pull AVG(current_lap_time_ms) per (session, lap, bucket) for both
		// laps. The two laps may live in different sessions when the
		// cross-session params are supplied, so we OR by (uid, lap).
		sql := fmt.Sprintf(`
SELECT session_uid,
       lap,
       FLOOR(track_position / %f)::INT AS bucket,
       AVG(current_lap_time_ms) AS t_ms
FROM telemetry_hifreq
WHERE ((session_uid = %s AND lap = %d) OR (session_uid = %s AND lap = %d))
  AND pit_status = 0
  AND track_position IS NOT NULL
  AND track_position >= 0
  AND current_lap_time_ms > 0
GROUP BY session_uid, lap, bucket
ORDER BY session_uid, lap, bucket`, bucketSize, uidString(yourUID), your, uidString(refUID), ref)

		rows, err := deps.Store.Query(c.Context(), sql)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}

		yourBuckets := make([]*float64, buckets)
		refBuckets := make([]*float64, buckets)
		for _, row := range rows {
			lap := toInt(row["lap"])
			rowUID := toUint64(row["session_uid"])
			b := toInt(row["bucket"])
			if b < 0 || b >= buckets {
				continue
			}
			if row["t_ms"] == nil {
				continue
			}
			v := toFloat(row["t_ms"])
			if rowUID == yourUID && lap == your {
				yourBuckets[b] = &v
			}
			if rowUID == refUID && lap == ref {
				refBuckets[b] = &v
			}
		}

		distance := make([]float64, buckets)
		delta := make([]*int, buckets)
		for i := 0; i < buckets; i++ {
			distance[i] = (float64(i) + 0.5) * bucketSize
			if yourBuckets[i] == nil || refBuckets[i] == nil {
				continue
			}
			d := int(*yourBuckets[i] - *refBuckets[i])
			delta[i] = &d
		}

		resp := lapDeltaResponse{
			SessionUID:   uidString(uid),
			TrackID:      state.trackID,
			TrackLengthM: trackLen,
			Buckets:      buckets,
			BucketSizeM:  bucketSize,
			Lap:          your,
			LapTimeMs:    yourMs,
			ReferenceLap: ref,
			ReferenceMs:  refMs,
			DistanceM:    distance,
			DeltaMs:      delta,
		}
		if yourUID != uid {
			resp.LapSessionUID = uidString(yourUID)
		}
		if refUID != uid {
			resp.ReferenceSessionUID = uidString(refUID)
		}
		return c.JSON(resp)
	}
}

// resolveCrossSession parses an optional ?<param>=<uid> override. Returns
// (defaultUID, defaultCarIdx) when the param is empty. When set, looks up
// the session's player car_index and rejects mismatched track lengths so
// the cumulative-time math stays apples-to-apples.
func resolveCrossSession(c *fiber.Ctx, deps *Deps, param string, defaultUID uint64, defaultCarIdx int, defaultTrackLen float64) (uint64, int, error) {
	raw := strings.TrimSpace(c.Query(param))
	if raw == "" {
		return defaultUID, defaultCarIdx, nil
	}
	uid, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("%s must be a uint64, got %q", param, raw)
	}
	if uid == defaultUID {
		return defaultUID, defaultCarIdx, nil
	}
	db := deps.Store.Reader()
	players, err := playerIndexBySession(c.Context(), db)
	if err != nil {
		return 0, 0, err
	}
	idx, ok := players[uid]
	if !ok {
		return 0, 0, fmt.Errorf("session not found: %d", uid)
	}
	meta, err := sessionMetaByUID(c.Context(), db)
	if err != nil {
		return 0, 0, err
	}
	m, ok := meta[uid]
	if !ok || m.TrackLength == 0 {
		return 0, 0, fmt.Errorf("session metadata missing or incomplete: %d", uid)
	}
	if float64(m.TrackLength) != defaultTrackLen {
		return 0, 0, fmt.Errorf("%s session track length (%dm) does not match request session (%.0fm) — delta requires matching tracks", param, m.TrackLength, defaultTrackLen)
	}
	return uid, idx, nil
}

// resolveSingleLap interprets the ?lap= parameter for /api/laps/delta. Accepts
// "", "last", or a positive integer. Empty / "last" defaults to the most recent
// completed lap. Returns (lap, lapTimeMs, err); (0, 0, nil) when no lap.
func resolveSingleLap(c *fiber.Ctx, deps *Deps, uid uint64, carIdx int, raw string) (int, int, error) {
	t := strings.ToLower(strings.TrimSpace(raw))
	if t == "" || t == "last" {
		n, err := mostRecentCompletedLap(c.Context(), deps, uid, carIdx)
		if err != nil || n == 0 {
			return 0, 0, err
		}
		ms, _ := lapTimeFromHistory(c.Context(), deps, uid, carIdx, n)
		return n, ms, nil
	}
	n, err := strconv.Atoi(t)
	if err != nil || n <= 0 {
		return 0, 0, fmt.Errorf("lap must be 'last' or a positive integer, got %q", raw)
	}
	ms, _ := lapTimeFromHistory(c.Context(), deps, uid, carIdx, n)
	return n, ms, nil
}

// resolveReferenceLap interprets ?reference=. Accepts "best" or a positive
// integer. "best" returns the driver's best valid lap of this session.
func resolveReferenceLap(c *fiber.Ctx, deps *Deps, uid uint64, carIdx int, raw string) (int, int, error) {
	t := strings.ToLower(strings.TrimSpace(raw))
	if t == "" || t == "best" {
		b, ms, err := bestValidLap(c.Context(), deps, uid, carIdx)
		return b, ms, err
	}
	n, err := strconv.Atoi(t)
	if err != nil || n <= 0 {
		return 0, 0, fmt.Errorf("reference must be 'best' or a positive integer, got %q", raw)
	}
	ms, _ := lapTimeFromHistory(c.Context(), deps, uid, carIdx, n)
	return n, ms, nil
}
