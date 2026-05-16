package api

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/enums"
)

// lap_best_at_track_handler.go answers "what is my fastest lap ever at this
// track, and where did I drive it?". Drives the cross-session reference
// picker on the dashboard's lap-trace overlay and the AI tools'
// "compare today's lap vs my best ever at Monza" flow.
//
//	GET /api/laps/best_at_track
//	  ?track_id=N               (integer; defaults to the live session's track)
//	  ?session_type=race|quali|practice|all   (default: all)
//
// Returns the single fastest valid lap for the player driver across every
// session that maps to the requested track_id, plus enough metadata that the
// caller can chain straight into /api/laps/delta with
// reference_session_uid + reference=<lap_num>.

type bestAtTrackResponse struct {
	TrackID         int    `json:"track_id"`
	TrackName       string `json:"track_name"`
	SessionUID      string `json:"session_uid"`
	SessionType     int    `json:"session_type"`
	SessionTypeName string `json:"session_type_name"`
	PlayerCarIndex  int    `json:"player_car_index"`
	Lap             int    `json:"lap"`
	LapTimeMs       int    `json:"lap_time_ms"`
	EndedAt         string `json:"ended_at"`
	Note            string `json:"note,omitempty"`
}

func bestAtTrackHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(c.Context(), 8*time.Second)
		defer cancel()

		// Resolve target track_id. Explicit ?track_id= wins; otherwise pull
		// from the live cache so a no-arg call works mid-race.
		var trackID int
		if raw := strings.TrimSpace(c.Query("track_id")); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "track_id must be an integer"})
			}
			trackID = n
		} else if state := deps.Store.Cache().Load(); state != nil {
			trackID = int(state.TrackID)
		}
		if trackID == 0 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "track_id is required (no live session to infer it from)"})
		}

		// Optional session_type filter — race-day driving and a Time Trial
		// hot lap aren't comparable; let callers ask for "best race lap"
		// specifically.
		sessTypeFilter := strings.ToLower(strings.TrimSpace(c.Query("session_type", "all")))
		var sessTypeLow, sessTypeHigh int
		switch sessTypeFilter {
		case "", "all":
			sessTypeLow, sessTypeHigh = 0, 99
		case "practice":
			sessTypeLow, sessTypeHigh = 1, 4
		case "quali", "qualifying":
			sessTypeLow, sessTypeHigh = 5, 9
		case "race":
			sessTypeLow, sessTypeHigh = 10, 12
		case "time_trial", "tt":
			sessTypeLow, sessTypeHigh = 13, 13
		default:
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": "session_type must be one of: all, practice, quali, race, time_trial",
			})
		}

		db := deps.Store.Reader()
		players, err := playerIndexBySession(ctx, db)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}
		meta, err := sessionMetaByUID(ctx, db)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}

		// Walk every session that maps to the requested track + session_type,
		// query the player's best valid lap in that session, keep the min.
		// Done in Go rather than a single SQL query because the player
		// car_index per session lives in the playerIndexBySession map, not in
		// session_history itself.
		best := bestAtTrackResponse{TrackID: trackID, TrackName: enums.Track(trackID)}
		bestMs := 0
		for uid, m := range meta {
			if m == nil || m.TrackID != trackID {
				continue
			}
			if m.SessionType < sessTypeLow || m.SessionType > sessTypeHigh {
				continue
			}
			pidx, ok := players[uid]
			if !ok {
				continue
			}
			var lap, ms sql.NullInt64
			err := db.QueryRowContext(ctx, `
				SELECT lap_num, lap_time_in_ms
				FROM session_history
				WHERE session_uid = ? AND car_index = ? AND lap_valid = 1 AND lap_time_in_ms > 0
				ORDER BY lap_time_in_ms ASC LIMIT 1`, uid, pidx).Scan(&lap, &ms)
			if err != nil || !ms.Valid {
				continue
			}
			if bestMs == 0 || int(ms.Int64) < bestMs {
				bestMs = int(ms.Int64)
				best.SessionUID = fmt.Sprintf("%d", uid)
				best.SessionType = m.SessionType
				best.SessionTypeName = enums.SessionType(m.SessionType)
				best.PlayerCarIndex = pidx
				best.Lap = int(lap.Int64)
				best.LapTimeMs = bestMs
				best.EndedAt = m.EndTS.UTC().Format(time.RFC3339)
			}
		}
		if bestMs == 0 {
			best.Note = "no valid completed laps for this track yet"
			return c.Status(fiber.StatusNotFound).JSON(best)
		}
		return c.JSON(best)
	}
}
