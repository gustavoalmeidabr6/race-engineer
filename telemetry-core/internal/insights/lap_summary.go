package insights

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/brain"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/enums"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
)

// Race-like session types in F1 25's session_type enum (see CLAUDE.md):
//
//	10 = Race, 11 = Race 2, 12 = Race 3
//
// Practice and qualifying take a different (more detailed) summary shape.
func isRaceLike(sessionType uint8) bool {
	return sessionType >= 10 && sessionType <= 12
}

// emitLapSummary fires a lap_summary Event onto the Interrupts Bus when the
// player's lap counter rolls over. Called from Engine.evaluate() with the
// PREVIOUS lap number (the one that just finished). When the SQL reader is
// not yet set up or when no row exists for the just-finished lap, falls back
// to a state-only summary so the engineer still says SOMETHING.
func (e *Engine) emitLapSummary(state *models.RaceState, completedLap uint8, talkLevel int32) {
	if e.bus == nil {
		return
	}

	idx := int(state.PlayerCarIndex)
	now := time.Now()

	// Pull lap timing from session_history; this is the canonical source
	// once the lap-data writer has flushed (the engine ticks every 5s, so
	// this normally lands a lap or two later — the producer still works).
	var lapMs, s1Ms, s2Ms, s3Ms uint32
	var pbLapMs, pbS1Ms, pbS2Ms, pbS3Ms uint32
	if e.reader != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		// Most recent row for the just-completed lap.
		row := e.reader.QueryRowContext(ctx, `
			SELECT lap_time_in_ms, sector1_time_in_ms, sector2_time_in_ms, sector3_time_in_ms
			FROM session_history
			WHERE car_index = ? AND lap_num = ? AND lap_time_in_ms > 0
			ORDER BY timestamp DESC LIMIT 1`, idx, int(completedLap))
		_ = row.Scan(&lapMs, &s1Ms, &s2Ms, &s3Ms)

		// Personal best for THIS session — used to compute deltas.
		row = e.reader.QueryRowContext(ctx, `
			SELECT MIN(lap_time_in_ms), MIN(sector1_time_in_ms), MIN(sector2_time_in_ms), MIN(sector3_time_in_ms)
			FROM session_history
			WHERE car_index = ? AND lap_time_in_ms > 0`, idx)
		_ = row.Scan(&pbLapMs, &pbS1Ms, &pbS2Ms, &pbS3Ms)
	}

	// Format the time fragment. State.LastLapTimeMs is a fallback when
	// session_history hasn't caught up (early in the session or under heavy
	// flush latency).
	lapTime := lapMs
	if lapTime == 0 {
		lapTime = state.LastLapTimeMs
	}

	// Pull a short competitor blurb (closest threat/target with compound +
	// gap + pace delta). Quietly empty if the reader is missing or no other
	// cars are in DuckDB yet — the lap summary still emits without it.
	compBlurb := competitorBlurb(e.reader, state, lapTime)

	summary, reasoning := buildLapSummaryText(state, completedLap, lapTime, s1Ms, s2Ms, s3Ms,
		pbLapMs, pbS1Ms, pbS2Ms, pbS3Ms, compBlurb)

	debug := map[string]any{
		"lap":           int(completedLap),
		"lap_ms":        int(lapTime),
		"s1_ms":         int(s1Ms),
		"s2_ms":         int(s2Ms),
		"s3_ms":         int(s3Ms),
		"pb_lap_ms":     int(pbLapMs),
		"position":      int(state.Position),
		"total_laps":    int(state.TotalLaps),
		"tyres_wear":    state.TyresWear,
		"fuel_kg":       state.FuelInTank,
		"compound":      enums.Compound(state.VisualCompound), // decoded so the model never voices "compound 17"
		"tyre_age_laps": int(state.TyresAgeLaps),
	}

	ev := brain.Event{
		Type:      brain.EventLapSummary,
		Priority:  brain.DefaultEventPriority(brain.EventLapSummary),
		Summary:   truncate(summary, brain.MaxEventSummaryChars),
		Reasoning: truncate(reasoning, brain.MaxEventReasoningChars),
		DebugData: debug,
		Source:    fmt.Sprintf("rule:lap_summary:lap%d", completedLap),
		DedupKey:  fmt.Sprintf("lap_summary:%d", completedLap),
	}

	stored, err := e.bus.EnqueueEvent(ev, talkLevel)
	switch {
	case err == nil:
		log.Info().
			Str("event_id", stored.ID).
			Uint8("lap", completedLap).
			Str("summary", stored.Summary).
			Msg("Lap summary event emitted")
	case errors.Is(err, brain.ErrEventDeduped):
		log.Debug().Uint8("lap", completedLap).Msg("Lap summary deduped")
	case errors.Is(err, brain.ErrEventTalkLevel):
		log.Debug().Uint8("lap", completedLap).Int32("talk_level", talkLevel).Msg("Lap summary dropped by TalkLevel")
	default:
		log.Warn().Err(err).Uint8("lap", completedLap).Msg("Lap summary enqueue failed")
	}

	_ = now // reserved for future use (e.g. fuel-burn delta vs prior lap)
}

// buildLapSummaryText shapes the Summary + Reasoning strings. Practice =
// detailed debrief (sector deltas), race = competitive context (closest
// threat / target with compound + pace delta). Always under the bus's char
// caps via truncate() in the caller.
//
// compBlurb is the pre-rendered competitor context (see competitorBlurb)
// and may be empty when the reader has no multi-car data yet.
func buildLapSummaryText(
	state *models.RaceState,
	completedLap uint8,
	lapMs, s1Ms, s2Ms, s3Ms uint32,
	pbLapMs, pbS1Ms, pbS2Ms, pbS3Ms uint32,
	compBlurb string,
) (summary, reasoning string) {
	lapStr := "n/a"
	if lapMs > 0 {
		lapStr = fmtMs(lapMs)
	}
	myCompound := enums.Compound(state.VisualCompound)

	if isRaceLike(state.SessionType) {
		// Race shape: lap N of M, position, then competitive context — that's
		// what the engineer says on team radio. Gap-to-leader / fuel are
		// secondary; they go in reasoning only when no competitor blurb is
		// available, so we always say SOMETHING useful.
		summary = fmt.Sprintf("Lap %d of %d done in %s. P%d on %s lap %d.",
			completedLap, state.TotalLaps, lapStr, state.Position, myCompound, state.TyresAgeLaps)
		if compBlurb != "" {
			reasoning = compBlurb
		} else {
			gapAhead := float64(state.DeltaToFrontMs) / 1000.0
			gapLeader := float64(state.DeltaToLeaderMs) / 1000.0
			reasoning = fmt.Sprintf("Gap ahead +%.1fs, gap to leader +%.1fs. Fuel %.1fkg.",
				gapAhead, gapLeader, state.FuelInTank)
		}
		return
	}

	// Practice / quali shape: lead with sector debrief vs PB. Append a
	// short competitor reference (e.g. "Verstappen running 1:28.4 on
	// Mediums lap 6") when available so the driver knows where they stand.
	summary = fmt.Sprintf("Lap %d done in %s.", completedLap, lapStr)
	if pbLapMs > 0 && lapMs > 0 && lapMs == pbLapMs {
		summary = fmt.Sprintf("Lap %d done in %s — new PB.", completedLap, lapStr)
	}
	dS1 := deltaMs(s1Ms, pbS1Ms)
	dS2 := deltaMs(s2Ms, pbS2Ms)
	dS3 := deltaMs(s3Ms, pbS3Ms)
	reasoning = fmt.Sprintf("Sectors vs PB: S1 %s, S2 %s, S3 %s. On %s lap %d.",
		dS1, dS2, dS3, myCompound, state.TyresAgeLaps)
	if compBlurb != "" {
		reasoning += " " + compBlurb
	}
	return
}

// competitorBlurb queries the latest lap_data + car_status for the cars
// nearest to the player's position and returns a short engineer-voice
// summary of the most actionable threat/target. Empty string when no other
// cars are in DuckDB or when the reader hasn't been wired up.
//
// Heuristics for what to surface:
//   - Cars 1 spot ahead (P-1) and 1 behind (P+1) are always interesting.
//   - Pace delta = their_last_lap - my_last_lap. Negative = they're faster
//     than us; positive = slower. A car behind with a NEGATIVE delta is
//     "closing fast" — the most worrying case.
//   - Anyone who pitted in the last ~3 minutes is worth flagging.
//
// We keep the output under ~120 chars so the reasoning string still fits
// the 1KB event cap with room to spare.
func competitorBlurb(reader *sql.DB, state *models.RaceState, myLastLapMs uint32) string {
	if reader == nil || state == nil || state.Position == 0 {
		return ""
	}
	myPos := int(state.Position)
	myIdx := int(state.PlayerCarIndex)

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	const sqlQuery = `
WITH latest_lap AS (
  SELECT car_index, car_position, last_lap_time_in_ms, num_pit_stops, pit_status,
         ROW_NUMBER() OVER (PARTITION BY car_index ORDER BY timestamp DESC) AS rn
  FROM lap_data
), latest_status AS (
  SELECT car_index, visual_tyre_compound, tyres_age_laps,
         ROW_NUMBER() OVER (PARTITION BY car_index ORDER BY timestamp DESC) AS rn
  FROM car_status
)
SELECT ll.car_index, ll.car_position, ll.last_lap_time_in_ms,
       ll.num_pit_stops, ll.pit_status,
       ls.visual_tyre_compound, ls.tyres_age_laps
FROM latest_lap ll
JOIN latest_status ls ON ll.car_index = ls.car_index AND ls.rn = 1
WHERE ll.rn = 1 AND ll.car_position > 0
  AND ll.car_position BETWEEN ? AND ?
ORDER BY ll.car_position
`
	rows, err := reader.QueryContext(ctx, sqlQuery, myPos-1, myPos+1)
	if err != nil {
		return ""
	}
	defer rows.Close()

	type entry struct {
		pos       int
		idx       int
		lastLapMs int
		pitStops  int
		pitStatus int
		compound  uint8
		tireAge   int
	}
	var ahead, behind *entry
	for rows.Next() {
		var e entry
		var compound int
		if err := rows.Scan(&e.idx, &e.pos, &e.lastLapMs, &e.pitStops, &e.pitStatus,
			&compound, &e.tireAge); err != nil {
			continue
		}
		e.compound = uint8(compound)
		if e.idx == myIdx {
			continue
		}
		switch {
		case e.pos == myPos-1:
			c := e
			ahead = &c
		case e.pos == myPos+1:
			c := e
			behind = &c
		}
	}

	parts := []string{}
	myMs := int(myLastLapMs)

	if ahead != nil {
		parts = append(parts, formatNeighbor("ahead", ahead.pos, ahead.compound, ahead.tireAge,
			ahead.lastLapMs, myMs, ahead.pitStatus))
	}
	if behind != nil {
		parts = append(parts, formatNeighbor("behind", behind.pos, behind.compound, behind.tireAge,
			behind.lastLapMs, myMs, behind.pitStatus))
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ")
}

// formatNeighbor builds one phrase like "P2 ahead on Soft lap 4, 0.3s
// faster" or "P5 behind on Medium lap 8, in pit lane". Pace delta is
// expressed as "Xs faster/slower"; when we don't have the comparison
// data we just cite the compound + age.
func formatNeighbor(side string, pos int, compound uint8, tireAge, neighborLapMs, myLapMs, pitStatus int) string {
	compStr := enums.Compound(compound)
	base := fmt.Sprintf("P%d %s on %s lap %d", pos, side, compStr, tireAge)
	if pitStatus != 0 {
		return base + ", in pit lane"
	}
	if neighborLapMs > 0 && myLapMs > 0 {
		delta := float64(neighborLapMs-myLapMs) / 1000.0
		if math.Abs(delta) >= 0.05 {
			if delta < 0 {
				return fmt.Sprintf("%s, %.1fs faster last lap", base, -delta)
			}
			return fmt.Sprintf("%s, %.1fs slower last lap", base, delta)
		}
	}
	return base
}

// fmtMs returns "1:28.421" style for >60s, "13.421" for shorter (sector).
func fmtMs(ms uint32) string {
	if ms == 0 {
		return "n/a"
	}
	totalSec := float64(ms) / 1000.0
	if totalSec >= 60 {
		mins := int(totalSec) / 60
		secs := totalSec - float64(mins*60)
		return fmt.Sprintf("%d:%06.3f", mins, secs)
	}
	return fmt.Sprintf("%.3f", totalSec)
}

// deltaMs formats a sector delta vs PB. Returns "+0.xxx", "-0.xxx", "PB",
// or "n/a" when either side is missing.
func deltaMs(sec, pb uint32) string {
	if sec == 0 || pb == 0 {
		return "n/a"
	}
	if sec == pb {
		return "PB"
	}
	d := float64(int64(sec)-int64(pb)) / 1000.0
	if d >= 0 {
		return fmt.Sprintf("+%.3f", d)
	}
	return fmt.Sprintf("%.3f", d)
}

// truncate caps a string to at most n bytes. Byte-cut (not rune-cut) — fine
// for the lap-summary domain where text is ASCII. The cap exists so the
// bus's length validator doesn't reject our event; we'd rather lose the
// final word than fail validation.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
