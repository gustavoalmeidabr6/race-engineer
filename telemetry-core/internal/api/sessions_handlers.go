package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog/log"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/enums"
)

// sessions_handlers.go powers the Deep Insights page:
//   GET /api/sessions             — picker source (last 100 sessions)
//   GET /api/sessions/:uid/summary — bundled per-session timeline
//
// Both share the per-session_uid metadata helper used by stats_handlers.go;
// the player car_index detection is identical (best-effort match against the
// telemetry_hifreq lap progression).

type sessionListItem struct {
	SessionUID      string `json:"session_uid"`
	TrackID         int    `json:"track_id"`
	TrackName       string `json:"track_name"`
	SessionType     int    `json:"session_type"`
	SessionTypeName string `json:"session_type_name"`
	FirstSeen       string `json:"first_seen"`
	LastSeen        string `json:"last_seen"`
	TotalLaps       int    `json:"total_laps"`
	PlayerCarIndex  int    `json:"player_car_index"`
	BestLapMs       *int   `json:"best_lap_ms"`
	FinalPosition   *int   `json:"final_position"`
}

func sessionsListHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(c.Context(), 10*time.Second)
		defer cancel()
		db := deps.Store.Reader()

		meta, err := sessionMetaByUID(ctx, db)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}
		players, err := playerIndexBySession(ctx, db)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}

		var items []sessionListItem
		for uid, m := range meta {
			pidx := players[uid]
			it := sessionListItem{
				SessionUID:      fmt.Sprintf("%d", uid),
				TrackID:         m.TrackID,
				TrackName:       enums.Track(m.TrackID),
				SessionType:     m.SessionType,
				SessionTypeName: enums.SessionType(m.SessionType),
				FirstSeen:       m.StartTS.UTC().Format(time.RFC3339),
				LastSeen:        m.EndTS.UTC().Format(time.RFC3339),
				PlayerCarIndex:  pidx,
				TotalLaps:       m.TotalLaps,
			}

			var laps int
			var best sql.NullInt64
			if err := db.QueryRowContext(ctx, `
				SELECT COUNT(*),
				       MIN(CASE WHEN lap_valid = 1 AND lap_time_in_ms > 0 THEN lap_time_in_ms END)
				FROM session_history
				WHERE session_uid = ? AND car_index = ?`, uid, pidx).Scan(&laps, &best); err == nil {
				if laps > 0 {
					it.TotalLaps = laps
				}
				if best.Valid {
					b := int(best.Int64)
					it.BestLapMs = &b
				}
			}

			if m.SessionType >= 10 && m.SessionType <= 12 {
				var pos sql.NullInt64
				if err := db.QueryRowContext(ctx, `
					SELECT car_position FROM lap_data
					WHERE car_index = ? AND timestamp BETWEEN ? AND ? AND car_position > 0
					ORDER BY timestamp DESC LIMIT 1`,
					pidx,
					m.StartTS.Add(-30*time.Second),
					m.EndTS.Add(30*time.Second)).Scan(&pos); err == nil && pos.Valid {
					p := int(pos.Int64)
					it.FinalPosition = &p
				}
			}

			items = append(items, it)
		}
		sort.Slice(items, func(i, j int) bool {
			return items[i].LastSeen > items[j].LastSeen
		})
		if len(items) > 100 {
			items = items[:100]
		}
		if items == nil {
			items = []sessionListItem{}
		}
		return c.JSON(items)
	}
}

// sessionSummary bundles every per-session timeseries the Deep Insights page
// needs in one round-trip.
type sessionSummary struct {
	SessionUID      string                  `json:"session_uid"`
	TrackID         int                     `json:"track_id"`
	TrackName       string                  `json:"track_name"`
	SessionType     int                     `json:"session_type"`
	SessionTypeName string                  `json:"session_type_name"`
	TrackLengthM    int                     `json:"track_length_m"`
	PlayerCarIndex  int                     `json:"player_car_index"`
	StartedAt       string                  `json:"started_at"`
	EndedAt         string                  `json:"ended_at"`
	Laps            []lapSummary            `json:"laps"`
	TyreWear        []tyreWearSample        `json:"tyre_wear"`
	Fuel            []fuelSample            `json:"fuel"`
	Damage          []damageSample          `json:"damage"`
	Events          []eventSummary          `json:"events"`
	BestLapMs       *int                    `json:"best_lap_ms"`
	FinalPosition   *int                    `json:"final_position"`
}

type lapSummary struct {
	Lap        int   `json:"lap"`
	LapTimeMs  int   `json:"lap_time_ms"`
	S1Ms       int   `json:"sector1_ms"`
	S2Ms       int   `json:"sector2_ms"`
	S3Ms       int   `json:"sector3_ms"`
	Valid      bool  `json:"valid"`
	Position   *int  `json:"position,omitempty"`
}

type tyreWearSample struct {
	Lap   int     `json:"lap"`
	WearFL float64 `json:"wear_fl"`
	WearFR float64 `json:"wear_fr"`
	WearRL float64 `json:"wear_rl"`
	WearRR float64 `json:"wear_rr"`
}

type fuelSample struct {
	Lap          int     `json:"lap"`
	FuelInTank   float64 `json:"fuel_in_tank"`
	FuelLapsLeft float64 `json:"fuel_laps_left"`
}

type damageSample struct {
	Lap            int `json:"lap"`
	FrontWingLeft  int `json:"front_wing_left"`
	FrontWingRight int `json:"front_wing_right"`
	RearWing       int `json:"rear_wing"`
	Floor          int `json:"floor"`
	Engine         int `json:"engine"`
	Gearbox        int `json:"gearbox"`
}

type eventSummary struct {
	Timestamp  string `json:"timestamp"`
	Code       string `json:"code"`
	Label      string `json:"label"`
	VehicleIdx int    `json:"vehicle_idx"`
	Detail     string `json:"detail,omitempty"`
}

func sessionSummaryHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		uidStr := c.Params("uid")
		uid, err := strconv.ParseUint(uidStr, 10, 64)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "session_uid must be an unsigned integer"})
		}

		ctx, cancel := context.WithTimeout(c.Context(), 15*time.Second)
		defer cancel()
		db := deps.Store.Reader()

		players, err := playerIndexBySession(ctx, db)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}
		pidx, hasPlayer := players[uid]
		if !hasPlayer {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "session not found"})
		}
		metaMap, err := sessionMetaByUID(ctx, db)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}
		meta, ok := metaMap[uid]
		if !ok {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "session metadata missing"})
		}

		summary := &sessionSummary{
			SessionUID:      uidStr,
			TrackID:         meta.TrackID,
			TrackName:       enums.Track(meta.TrackID),
			SessionType:     meta.SessionType,
			SessionTypeName: enums.SessionType(meta.SessionType),
			TrackLengthM:    meta.TrackLength,
			PlayerCarIndex:  pidx,
			StartedAt:       meta.StartTS.UTC().Format(time.RFC3339),
			EndedAt:         meta.EndTS.UTC().Format(time.RFC3339),
			Laps:            []lapSummary{},
			TyreWear:        []tyreWearSample{},
			Fuel:            []fuelSample{},
			Damage:          []damageSample{},
			Events:          []eventSummary{},
		}

		// Lap times from session_history.
		lRows, err := db.QueryContext(ctx, `
			SELECT lap_num, lap_time_in_ms,
			       sector1_time_in_ms, sector2_time_in_ms, sector3_time_in_ms, lap_valid
			FROM session_history
			WHERE session_uid = ? AND car_index = ? AND lap_time_in_ms > 0
			ORDER BY lap_num`, uid, pidx)
		if err == nil {
			for lRows.Next() {
				var ls lapSummary
				var v sql.NullInt64
				if err := lRows.Scan(&ls.Lap, &ls.LapTimeMs, &ls.S1Ms, &ls.S2Ms, &ls.S3Ms, &v); err == nil {
					ls.Valid = v.Valid && v.Int64 == 1
					summary.Laps = append(summary.Laps, ls)
				}
			}
			lRows.Close()
		} else {
			log.Debug().Err(err).Msg("session_history lookup")
		}

		// Best lap + final position.
		var best sql.NullInt64
		if err := db.QueryRowContext(ctx, `
			SELECT MIN(CASE WHEN lap_valid = 1 AND lap_time_in_ms > 0 THEN lap_time_in_ms END)
			FROM session_history
			WHERE session_uid = ? AND car_index = ?`, uid, pidx).Scan(&best); err == nil && best.Valid {
			b := int(best.Int64)
			summary.BestLapMs = &b
		}
		if meta.SessionType >= 10 && meta.SessionType <= 12 {
			var pos sql.NullInt64
			if err := db.QueryRowContext(ctx, `
				SELECT car_position FROM lap_data
				WHERE car_index = ? AND timestamp BETWEEN ? AND ? AND car_position > 0
				ORDER BY timestamp DESC LIMIT 1`,
				pidx,
				meta.StartTS.Add(-30*time.Second),
				meta.EndTS.Add(30*time.Second)).Scan(&pos); err == nil && pos.Valid {
				p := int(pos.Int64)
				summary.FinalPosition = &p
			}
		}

		// Position per lap — last car_position per lap from lap_data.
		posMap := map[int]int{}
		pRows, err := db.QueryContext(ctx, `
			SELECT current_lap_num, car_position
			FROM lap_data
			WHERE car_index = ? AND timestamp BETWEEN ? AND ? AND car_position > 0
			ORDER BY timestamp ASC`,
			pidx,
			meta.StartTS.Add(-30*time.Second),
			meta.EndTS.Add(30*time.Second))
		if err == nil {
			for pRows.Next() {
				var lap, pos int
				if err := pRows.Scan(&lap, &pos); err == nil {
					posMap[lap] = pos
				}
			}
			pRows.Close()
		}
		for i := range summary.Laps {
			if p, ok := posMap[summary.Laps[i].Lap]; ok {
				pp := p
				summary.Laps[i].Position = &pp
			}
		}

		// Tyre wear sampled at lap boundaries — pick the latest car_damage row
		// per lap from lap_data join. Approximation: use the wear row whose
		// timestamp falls in the second half of each lap.
		wRows, err := db.QueryContext(ctx, `
			SELECT current_lap_num,
			       LAST(tyres_wear_fl), LAST(tyres_wear_fr),
			       LAST(tyres_wear_rl), LAST(tyres_wear_rr)
			FROM (
				SELECT cd.timestamp, ld.current_lap_num,
				       cd.tyres_wear_fl, cd.tyres_wear_fr,
				       cd.tyres_wear_rl, cd.tyres_wear_rr
				FROM car_damage cd
				JOIN lap_data ld
				  ON ld.car_index = cd.car_index
				 AND ABS(EXTRACT(EPOCH FROM (ld.timestamp - cd.timestamp))) < 1
				WHERE cd.car_index = ?
				  AND cd.timestamp BETWEEN ? AND ?
				ORDER BY cd.timestamp ASC
			)
			GROUP BY current_lap_num
			ORDER BY current_lap_num`,
			pidx,
			meta.StartTS.Add(-30*time.Second),
			meta.EndTS.Add(30*time.Second))
		if err == nil {
			for wRows.Next() {
				var s tyreWearSample
				if err := wRows.Scan(&s.Lap, &s.WearFL, &s.WearFR, &s.WearRL, &s.WearRR); err == nil && s.Lap > 0 {
					summary.TyreWear = append(summary.TyreWear, s)
				}
			}
			wRows.Close()
		} else {
			log.Debug().Err(err).Msg("tyre wear lookup")
		}

		// Fuel per lap — latest car_status row's fuel reading per lap.
		fRows, err := db.QueryContext(ctx, `
			SELECT current_lap_num,
			       LAST(fuel_in_tank), LAST(fuel_remaining_laps)
			FROM (
				SELECT cs.timestamp, ld.current_lap_num,
				       cs.fuel_in_tank, cs.fuel_remaining_laps
				FROM car_status cs
				JOIN lap_data ld
				  ON ld.car_index = cs.car_index
				 AND ABS(EXTRACT(EPOCH FROM (ld.timestamp - cs.timestamp))) < 1
				WHERE cs.car_index = ?
				  AND cs.timestamp BETWEEN ? AND ?
				ORDER BY cs.timestamp ASC
			)
			GROUP BY current_lap_num
			ORDER BY current_lap_num`,
			pidx,
			meta.StartTS.Add(-30*time.Second),
			meta.EndTS.Add(30*time.Second))
		if err == nil {
			for fRows.Next() {
				var s fuelSample
				if err := fRows.Scan(&s.Lap, &s.FuelInTank, &s.FuelLapsLeft); err == nil && s.Lap > 0 {
					summary.Fuel = append(summary.Fuel, s)
				}
			}
			fRows.Close()
		}

		// Damage timeline — per lap, latest damage components.
		dRows, err := db.QueryContext(ctx, `
			SELECT current_lap_num,
			       LAST(front_left_wing_damage), LAST(front_right_wing_damage),
			       LAST(rear_wing_damage), LAST(floor_damage),
			       LAST(engine_damage), LAST(gear_box_damage)
			FROM (
				SELECT cd.timestamp, ld.current_lap_num,
				       cd.front_left_wing_damage, cd.front_right_wing_damage,
				       cd.rear_wing_damage, cd.floor_damage,
				       cd.engine_damage, cd.gear_box_damage
				FROM car_damage cd
				JOIN lap_data ld
				  ON ld.car_index = cd.car_index
				 AND ABS(EXTRACT(EPOCH FROM (ld.timestamp - cd.timestamp))) < 1
				WHERE cd.car_index = ?
				  AND cd.timestamp BETWEEN ? AND ?
				ORDER BY cd.timestamp ASC
			)
			GROUP BY current_lap_num
			ORDER BY current_lap_num`,
			pidx,
			meta.StartTS.Add(-30*time.Second),
			meta.EndTS.Add(30*time.Second))
		if err == nil {
			for dRows.Next() {
				var s damageSample
				if err := dRows.Scan(&s.Lap, &s.FrontWingLeft, &s.FrontWingRight, &s.RearWing, &s.Floor, &s.Engine, &s.Gearbox); err == nil && s.Lap > 0 {
					summary.Damage = append(summary.Damage, s)
				}
			}
			dRows.Close()
		}

		// Race events scoped to this session window.
		eRows, err := db.QueryContext(ctx, `
			SELECT timestamp, event_code, vehicle_idx, COALESCE(detail_text, '')
			FROM race_events
			WHERE timestamp BETWEEN ? AND ?
			ORDER BY timestamp ASC`,
			meta.StartTS.Add(-30*time.Second),
			meta.EndTS.Add(30*time.Second))
		if err == nil {
			for eRows.Next() {
				var ts time.Time
				var code, detail string
				var vehicle int
				if err := eRows.Scan(&ts, &code, &vehicle, &detail); err == nil {
					summary.Events = append(summary.Events, eventSummary{
						Timestamp:  ts.UTC().Format(time.RFC3339),
						Code:       code,
						Label:      enums.EventCode(code),
						VehicleIdx: vehicle,
						Detail:     detail,
					})
				}
			}
			eRows.Close()
		}

		return c.JSON(summary)
	}
}

// ---------------------------------------------------------------------------
// lapCtx — shared "session resolution" helper for lap_analysis_handlers.
// Encapsulates the choice between live cache and historical session_uid so
// /api/laps/traces and /api/laps/compare can serve both Race Day and Deep
// Insights without forking.
// ---------------------------------------------------------------------------

type lapCtxState struct {
	sessionUID     uint64
	playerCarIndex int
	trackID        int
	trackLength    int
}

type lapCtx struct {
	c    *fiber.Ctx
	deps *Deps
}

func newLapCtx(c *fiber.Ctx, deps *Deps) *lapCtx {
	return &lapCtx{c: c, deps: deps}
}

func (l *lapCtx) resolve() (*lapCtxState, error) {
	uidParam := l.c.Query("session_uid")
	if uidParam == "" {
		state := l.deps.Store.Cache().Load()
		if state == nil {
			return nil, errors.New("no telemetry yet")
		}
		if state.TrackLength == 0 {
			return nil, errors.New("track length unknown — wait for first session packet")
		}
		return &lapCtxState{
			sessionUID:     state.SessionUID,
			playerCarIndex: int(state.PlayerCarIndex),
			trackID:        int(state.TrackID),
			trackLength:    int(state.TrackLength),
		}, nil
	}

	uid, err := strconv.ParseUint(uidParam, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid session_uid: %w", err)
	}

	ctx, cancel := context.WithTimeout(l.c.Context(), 5*time.Second)
	defer cancel()
	db := l.deps.Store.Reader()

	players, err := playerIndexBySession(ctx, db)
	if err != nil {
		return nil, err
	}
	pidx, ok := players[uid]
	if !ok {
		return nil, fmt.Errorf("session not found: %d", uid)
	}
	meta, err := sessionMetaByUID(ctx, db)
	if err != nil {
		return nil, err
	}
	m, ok := meta[uid]
	if !ok || m.TrackLength == 0 {
		return nil, fmt.Errorf("session metadata missing or incomplete: %d", uid)
	}
	return &lapCtxState{
		sessionUID:     uid,
		playerCarIndex: pidx,
		trackID:        m.TrackID,
		trackLength:    m.TrackLength,
	}, nil
}
