package api

import (
	"strconv"

	"github.com/gofiber/fiber/v2"
)

// lap_list_handler.go answers "what laps does this driver have in this
// session, with their times and validity, in a payload light enough to
// re-fetch every few seconds". Used by the dashboard Telemetry page's
// multi-lap picker and by the analyst's list_laps tool. The richer
// /api/sessions/:uid/summary is too heavy for poll-driven UI.
//
//	GET /api/laps/list
//	  ?session_uid=…   (optional; live cache if absent)
//
// Returns [{lap, lap_time_ms, sector1_ms, sector2_ms, sector3_ms, valid}].

type lapListItem struct {
	Lap        int  `json:"lap"`
	LapTimeMs  int  `json:"lap_time_ms"`
	Sector1Ms  int  `json:"sector1_ms"`
	Sector2Ms  int  `json:"sector2_ms"`
	Sector3Ms  int  `json:"sector3_ms"`
	Valid      bool `json:"valid"`
}

type lapListResponse struct {
	SessionUID string        `json:"session_uid"`
	CarIndex   int           `json:"car_index"`
	BestLap    int           `json:"best_lap,omitempty"`
	Laps       []lapListItem `json:"laps"`
}

func lapListHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		ctx := newLapCtx(c, deps)
		state, err := ctx.resolve()
		if err != nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": err.Error()})
		}

		// Pull every completed lap for this driver. Filter > 0 so the
		// row that the SessionHistory packet emits for the in-progress
		// lap (lap_time_in_ms == 0) doesn't pollute the picker.
		sql := `SELECT lap_num, lap_time_in_ms,
		               sector1_time_in_ms, sector2_time_in_ms, sector3_time_in_ms,
		               lap_valid
		        FROM session_history
		        WHERE session_uid = ` + uidString(state.sessionUID) + `
		          AND car_index = ` + strconv.Itoa(state.playerCarIndex) + `
		          AND lap_time_in_ms > 0
		        ORDER BY lap_num`
		rows, err := deps.Store.Query(c.Context(), sql)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}

		laps := make([]lapListItem, 0, len(rows))
		bestLap := 0
		bestMs := 0
		for _, r := range rows {
			item := lapListItem{
				Lap:       toInt(r["lap_num"]),
				LapTimeMs: toInt(r["lap_time_in_ms"]),
				Sector1Ms: toInt(r["sector1_time_in_ms"]),
				Sector2Ms: toInt(r["sector2_time_in_ms"]),
				Sector3Ms: toInt(r["sector3_time_in_ms"]),
				Valid:     toInt(r["lap_valid"]) == 1,
			}
			laps = append(laps, item)
			if item.Valid && item.LapTimeMs > 0 && (bestMs == 0 || item.LapTimeMs < bestMs) {
				bestMs = item.LapTimeMs
				bestLap = item.Lap
			}
		}

		return c.JSON(lapListResponse{
			SessionUID: uidString(state.sessionUID),
			CarIndex:   state.playerCarIndex,
			BestLap:    bestLap,
			Laps:       laps,
		})
	}
}
