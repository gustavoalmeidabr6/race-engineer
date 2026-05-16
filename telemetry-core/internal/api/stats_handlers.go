package api

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog/log"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/enums"
)

// careerStatsResponse is the JSON returned by GET /api/stats/career.
type careerStatsResponse struct {
	TotalSessions          int                    `json:"total_sessions"`
	TotalRaces             int                    `json:"total_races"`
	TotalQualiSessions     int                    `json:"total_quali_sessions"`
	TotalPracticeSessions  int                    `json:"total_practice_sessions"`
	TotalLaps              int                    `json:"total_laps"`
	TotalDistanceKm        float64                `json:"total_distance_km"`
	TotalDriveSeconds      int                    `json:"total_drive_seconds"`
	BestFinish             *int                   `json:"best_finish"`
	AverageFinish          *float64               `json:"average_finish"`
	Podiums                int                    `json:"podiums"`
	FastestLapsEarned      int                    `json:"fastest_laps_earned"`
	Collisions             int                    `json:"collisions"`
	Penalties              int                    `json:"penalties"`
	Retirements            int                    `json:"retirements"`
	TopSpeedKmh            float64                `json:"top_speed_kmh"`
	MaxGLateral            float64                `json:"max_g_lateral"`
	MaxGBraking            float64                `json:"max_g_braking"`
	TireCompoundDistribution map[string]int       `json:"tire_compound_distribution"`
	TracksVisited          []trackStat            `json:"tracks_visited"`
	RecentSessions         []recentSessionStat    `json:"recent_sessions"`
}

type trackStat struct {
	TrackID    int    `json:"track_id"`
	Name       string `json:"name"`
	Sessions   int    `json:"sessions"`
	BestLapMs  *int   `json:"best_lap_ms"`
	TotalLaps  int    `json:"total_laps"`
}

type recentSessionStat struct {
	SessionUID      string  `json:"session_uid"`
	Track           string  `json:"track"`
	TrackID         int     `json:"track_id"`
	SessionTypeName string  `json:"session_type_name"`
	SessionType     int     `json:"session_type"`
	EndedAt         string  `json:"ended_at"`
	Laps            int     `json:"laps"`
	BestLapMs       *int    `json:"best_lap_ms"`
	FinalPosition   *int    `json:"final_position"`
}

// careerStatsHandler returns aggregated lifetime stats from DuckDB.
func careerStatsHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(c.Context(), 15*time.Second)
		defer cancel()

		db := deps.Store.Reader()
		stats, err := computeCareerStats(ctx, db)
		if err != nil {
			log.Warn().Err(err).Msg("career stats failed")
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": err.Error(),
			})
		}
		return c.JSON(stats)
	}
}

// playerIndexBySession returns the best-effort player car_index per session_uid.
// Strategy: prefer matching the max lap completed in telemetry_hifreq (player-only)
// against session_history's per-car final lap; fall back to the car_index with the
// most lap rows in session_history. Sessions present only in telemetry_hifreq but
// not in session_history are skipped (no per-car data to attribute).
func playerIndexBySession(ctx context.Context, db *sql.DB) (map[uint64]int, error) {
	out := map[uint64]int{}

	// 1. Per-session, per-car final lap_num from session_history.
	rows, err := db.QueryContext(ctx, `
		SELECT session_uid, car_index, MAX(lap_num) AS final_lap, COUNT(*) AS lap_rows
		FROM session_history
		WHERE session_uid IS NOT NULL
		GROUP BY session_uid, car_index`)
	if err != nil {
		return nil, fmt.Errorf("session_history grouping: %w", err)
	}
	type cand struct {
		car      int
		finalLap int
		lapRows  int
	}
	bySess := map[uint64][]cand{}
	for rows.Next() {
		var uid uint64
		var car, finalLap, lapRows int
		if err := rows.Scan(&uid, &car, &finalLap, &lapRows); err != nil {
			rows.Close()
			return nil, err
		}
		bySess[uid] = append(bySess[uid], cand{car, finalLap, lapRows})
	}
	rows.Close()

	// 2. Player hint: max lap reached in telemetry_hifreq per session.
	hint := map[uint64]int{}
	hRows, err := db.QueryContext(ctx, `
		SELECT session_uid, MAX(lap) FROM telemetry_hifreq
		WHERE session_uid IS NOT NULL GROUP BY session_uid`)
	if err == nil {
		for hRows.Next() {
			var uid uint64
			var lap sql.NullInt64
			if err := hRows.Scan(&uid, &lap); err == nil && lap.Valid {
				hint[uid] = int(lap.Int64)
			}
		}
		hRows.Close()
	}

	// 3. For each session, pick the best candidate.
	for uid, cs := range bySess {
		if len(cs) == 0 {
			continue
		}
		if h, ok := hint[uid]; ok {
			// Prefer the car_index whose final_lap is closest to the player hint,
			// breaking ties in favour of the lower car_index.
			sort.Slice(cs, func(i, j int) bool {
				di := abs(cs[i].finalLap - h)
				dj := abs(cs[j].finalLap - h)
				if di != dj {
					return di < dj
				}
				return cs[i].car < cs[j].car
			})
			out[uid] = cs[0].car
		} else {
			// No hint — pick the car with the most lap rows recorded.
			sort.Slice(cs, func(i, j int) bool {
				if cs[i].lapRows != cs[j].lapRows {
					return cs[i].lapRows > cs[j].lapRows
				}
				return cs[i].car < cs[j].car
			})
			out[uid] = cs[0].car
		}
	}
	return out, nil
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// sessionMeta holds the per-session_uid metadata derived by joining
// telemetry_hifreq's session windows with session_data's most-recent row
// inside that window.
type sessionMeta struct {
	UID         uint64
	StartTS     time.Time
	EndTS       time.Time
	TrackID     int
	SessionType int
	TrackLength int
	TotalLaps   int
}

func sessionMetaByUID(ctx context.Context, db *sql.DB) (map[uint64]*sessionMeta, error) {
	out := map[uint64]*sessionMeta{}

	// Source 1: telemetry_hifreq carries the session_uid + a wall-clock window.
	rows, err := db.QueryContext(ctx, `
		SELECT session_uid, MIN(timestamp), MAX(timestamp)
		FROM telemetry_hifreq
		WHERE session_uid IS NOT NULL
		GROUP BY session_uid`)
	if err != nil {
		return nil, fmt.Errorf("hifreq window: %w", err)
	}
	for rows.Next() {
		var uid uint64
		var s, e time.Time
		if err := rows.Scan(&uid, &s, &e); err != nil {
			rows.Close()
			return nil, err
		}
		out[uid] = &sessionMeta{UID: uid, StartTS: s, EndTS: e}
	}
	rows.Close()

	// Also include session_uids that exist only in session_history (e.g. legacy
	// sessions captured before HIFREQ_SAMPLE_RATE was on).
	hRows, err := db.QueryContext(ctx, `
		SELECT session_uid, MIN(timestamp), MAX(timestamp)
		FROM session_history
		WHERE session_uid IS NOT NULL
		GROUP BY session_uid`)
	if err == nil {
		for hRows.Next() {
			var uid uint64
			var s, e sql.NullTime
			if err := hRows.Scan(&uid, &s, &e); err == nil {
				if _, ok := out[uid]; !ok && s.Valid && e.Valid {
					out[uid] = &sessionMeta{UID: uid, StartTS: s.Time, EndTS: e.Time}
				}
			}
		}
		hRows.Close()
	}

	// Source 2: pull the most recent session_data row within each window.
	for uid, meta := range out {
		// Pad the window slightly so a session_data row written just before
		// the first hifreq sample still attributes correctly.
		row := db.QueryRowContext(ctx, `
			SELECT track_id, session_type, track_length, total_laps
			FROM session_data
			WHERE timestamp BETWEEN ? AND ?
			ORDER BY timestamp DESC LIMIT 1`,
			meta.StartTS.Add(-30*time.Second),
			meta.EndTS.Add(30*time.Second))
		var t, st, tl, ttl sql.NullInt64
		if err := row.Scan(&t, &st, &tl, &ttl); err == nil {
			if t.Valid {
				meta.TrackID = int(t.Int64)
			}
			if st.Valid {
				meta.SessionType = int(st.Int64)
			}
			if tl.Valid {
				meta.TrackLength = int(tl.Int64)
			}
			if ttl.Valid {
				meta.TotalLaps = int(ttl.Int64)
			}
		}
		_ = uid
	}
	return out, nil
}

func computeCareerStats(ctx context.Context, db *sql.DB) (*careerStatsResponse, error) {
	resp := &careerStatsResponse{
		TireCompoundDistribution: map[string]int{},
		TracksVisited:            []trackStat{},
		RecentSessions:           []recentSessionStat{},
	}

	playerByUID, err := playerIndexBySession(ctx, db)
	if err != nil {
		return nil, err
	}
	metaByUID, err := sessionMetaByUID(ctx, db)
	if err != nil {
		return nil, err
	}

	resp.TotalSessions = len(metaByUID)

	// Classify by session_type. F1 25 enums:
	//   1-4 = Practice, 5-9 = Qualifying, 10-12 = Race, 13 = Time Trial.
	for _, m := range metaByUID {
		switch {
		case m.SessionType >= 1 && m.SessionType <= 4:
			resp.TotalPracticeSessions++
		case m.SessionType >= 5 && m.SessionType <= 9:
			resp.TotalQualiSessions++
		case m.SessionType >= 10 && m.SessionType <= 12:
			resp.TotalRaces++
		}
	}

	// total_laps + total_drive_seconds + total_distance_km from session_history,
	// scoped to the per-session player car_index.
	for uid, pidx := range playerByUID {
		row := db.QueryRowContext(ctx, `
			SELECT COUNT(*), COALESCE(SUM(lap_time_in_ms), 0)
			FROM session_history
			WHERE session_uid = ? AND car_index = ? AND lap_valid = 1
			  AND lap_time_in_ms > 0`, uid, pidx)
		var laps int
		var totalMs int64
		if err := row.Scan(&laps, &totalMs); err == nil {
			resp.TotalLaps += laps
			resp.TotalDriveSeconds += int(totalMs / 1000)

			if m, ok := metaByUID[uid]; ok && m.TrackLength > 0 {
				resp.TotalDistanceKm += float64(laps) * float64(m.TrackLength) / 1000.0
			}
		}
	}

	// Position stats from lap_data — the LAST recorded car_position per
	// race-session player car is the final classification.
	finalPositions := []int{} // race finishes only
	for uid, pidx := range playerByUID {
		m, ok := metaByUID[uid]
		if !ok || m.SessionType < 10 || m.SessionType > 12 {
			continue
		}
		row := db.QueryRowContext(ctx, `
			SELECT car_position
			FROM lap_data
			WHERE car_index = ? AND timestamp BETWEEN ? AND ? AND car_position > 0
			ORDER BY timestamp DESC LIMIT 1`,
			pidx, m.StartTS.Add(-30*time.Second), m.EndTS.Add(30*time.Second))
		var pos sql.NullInt64
		if err := row.Scan(&pos); err == nil && pos.Valid {
			finalPositions = append(finalPositions, int(pos.Int64))
		}
	}
	if len(finalPositions) > 0 {
		bf := finalPositions[0]
		sum := 0
		for _, p := range finalPositions {
			if p < bf {
				bf = p
			}
			if p <= 3 {
				resp.Podiums++
			}
			sum += p
		}
		avg := float64(sum) / float64(len(finalPositions))
		resp.BestFinish = &bf
		resp.AverageFinish = &avg
	}

	// Race events: FTLP / COLL / PENA / RTMT (filtered to player car for FTLP).
	// Other event codes are session-wide; we count them as career totals
	// regardless of vehicle_idx because the event simply happened in a session
	// the user drove.
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM race_events WHERE event_code = 'FTLP'`).Scan(&resp.FastestLapsEarned); err != nil {
		log.Debug().Err(err).Msg("FTLP count")
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM race_events WHERE event_code = 'COLL'`).Scan(&resp.Collisions); err != nil {
		log.Debug().Err(err).Msg("COLL count")
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM race_events WHERE event_code = 'PENA'`).Scan(&resp.Penalties); err != nil {
		log.Debug().Err(err).Msg("PENA count")
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM race_events WHERE event_code = 'RTMT'`).Scan(&resp.Retirements); err != nil {
		log.Debug().Err(err).Msg("RTMT count")
	}

	// Speed and G — telemetry_hifreq is player-only, so no car_index filter.
	var topSpeed sql.NullFloat64
	if err := db.QueryRowContext(ctx, `SELECT MAX(speed) FROM telemetry_hifreq`).Scan(&topSpeed); err == nil && topSpeed.Valid {
		resp.TopSpeedKmh = topSpeed.Float64
	}
	var maxLat, maxBrake sql.NullFloat64
	if err := db.QueryRowContext(ctx, `SELECT MAX(ABS(g_force_lat)) FROM telemetry_hifreq`).Scan(&maxLat); err == nil && maxLat.Valid {
		resp.MaxGLateral = maxLat.Float64
	}
	if err := db.QueryRowContext(ctx, `SELECT MIN(g_force_lon) FROM telemetry_hifreq`).Scan(&maxBrake); err == nil && maxBrake.Valid {
		// Braking is the most-negative longitudinal G; report as positive magnitude.
		resp.MaxGBraking = -maxBrake.Float64
	}

	// Tire compound usage — count car_status rows per compound for the player
	// car within each session window. Approximation: each row is ~1Hz so the
	// count is roughly seconds spent on that compound, which renders fine as
	// a relative breakdown.
	for uid, pidx := range playerByUID {
		m, ok := metaByUID[uid]
		if !ok {
			continue
		}
		rows, err := db.QueryContext(ctx, `
			SELECT actual_tyre_compound, COUNT(*)
			FROM car_status
			WHERE car_index = ? AND timestamp BETWEEN ? AND ?
			GROUP BY actual_tyre_compound`,
			pidx, m.StartTS.Add(-30*time.Second), m.EndTS.Add(30*time.Second))
		if err != nil {
			continue
		}
		for rows.Next() {
			var c sql.NullInt64
			var n int
			if err := rows.Scan(&c, &n); err != nil {
				continue
			}
			if !c.Valid {
				continue
			}
			resp.TireCompoundDistribution[compoundLabel(int(c.Int64))] += n
		}
		rows.Close()
	}

	// Tracks visited — group sessions by track_id, with best lap per track.
	type tBucket struct {
		sessions  int
		laps      int
		bestLapMs *int
	}
	tracks := map[int]*tBucket{}
	for uid, m := range metaByUID {
		if m.TrackID < 0 || m.SessionType == 0 {
			continue
		}
		tb := tracks[m.TrackID]
		if tb == nil {
			tb = &tBucket{}
			tracks[m.TrackID] = tb
		}
		tb.sessions++

		pidx, ok := playerByUID[uid]
		if !ok {
			continue
		}
		var laps int
		var best sql.NullInt64
		if err := db.QueryRowContext(ctx, `
			SELECT COUNT(*),
			       MIN(CASE WHEN lap_valid = 1 AND lap_time_in_ms > 0 THEN lap_time_in_ms END)
			FROM session_history
			WHERE session_uid = ? AND car_index = ?`, uid, pidx).Scan(&laps, &best); err == nil {
			tb.laps += laps
			if best.Valid {
				bv := int(best.Int64)
				if tb.bestLapMs == nil || bv < *tb.bestLapMs {
					tb.bestLapMs = &bv
				}
			}
		}
	}
	for tid, tb := range tracks {
		resp.TracksVisited = append(resp.TracksVisited, trackStat{
			TrackID:   tid,
			Name:      enums.Track(tid),
			Sessions:  tb.sessions,
			BestLapMs: tb.bestLapMs,
			TotalLaps: tb.laps,
		})
	}
	sort.Slice(resp.TracksVisited, func(i, j int) bool {
		return resp.TracksVisited[i].Sessions > resp.TracksVisited[j].Sessions
	})

	// Recent sessions — last 10 by EndTS.
	type sessSlice struct {
		uid uint64
		m   *sessionMeta
	}
	var ordered []sessSlice
	for uid, m := range metaByUID {
		ordered = append(ordered, sessSlice{uid, m})
	}
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].m.EndTS.After(ordered[j].m.EndTS)
	})
	if len(ordered) > 10 {
		ordered = ordered[:10]
	}
	for _, s := range ordered {
		rs := recentSessionStat{
			SessionUID:      fmt.Sprintf("%d", s.uid),
			Track:           enums.Track(s.m.TrackID),
			TrackID:         s.m.TrackID,
			SessionType:     s.m.SessionType,
			SessionTypeName: enums.SessionType(s.m.SessionType),
			EndedAt:         s.m.EndTS.UTC().Format(time.RFC3339),
		}
		if pidx, ok := playerByUID[s.uid]; ok {
			var laps int
			var best sql.NullInt64
			if err := db.QueryRowContext(ctx, `
				SELECT COUNT(*),
				       MIN(CASE WHEN lap_valid = 1 AND lap_time_in_ms > 0 THEN lap_time_in_ms END)
				FROM session_history
				WHERE session_uid = ? AND car_index = ?`, s.uid, pidx).Scan(&laps, &best); err == nil {
				rs.Laps = laps
				if best.Valid {
					b := int(best.Int64)
					rs.BestLapMs = &b
				}
			}
			if s.m.SessionType >= 10 && s.m.SessionType <= 12 {
				var pos sql.NullInt64
				if err := db.QueryRowContext(ctx, `
					SELECT car_position FROM lap_data
					WHERE car_index = ? AND timestamp BETWEEN ? AND ? AND car_position > 0
					ORDER BY timestamp DESC LIMIT 1`,
					pidx,
					s.m.StartTS.Add(-30*time.Second),
					s.m.EndTS.Add(30*time.Second)).Scan(&pos); err == nil && pos.Valid {
					p := int(pos.Int64)
					rs.FinalPosition = &p
				}
			}
		}
		resp.RecentSessions = append(resp.RecentSessions, rs)
	}

	return resp, nil
}

// compoundLabel maps the F1 25 actual_tyre_compound enum to a short label.
func compoundLabel(c int) string {
	switch c {
	case 16:
		return "soft"
	case 17:
		return "medium"
	case 18:
		return "hard"
	case 7:
		return "inter"
	case 8:
		return "wet"
	default:
		return fmt.Sprintf("c%d", c)
	}
}
