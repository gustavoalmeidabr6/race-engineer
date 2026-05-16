package insights

import (
	"context"
	"fmt"
	"time"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/brain"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/enums"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
)

// closing_threat.go fires the two events the driver most needs to hear
// immediately about the car behind:
//
//   - emitOvertaken (P5): position number went UP this tick. Fast-lane
//     priority — interrupts the engineer mid-sentence.
//   - emitClosingThreat (P4): the car at P+1 is closing on us at ≥0.5 s
//     over the sample window AND the gap is under 2.5 s. Latched until
//     the gap re-opens 0.3 s from its minimum, so we don't spam.
//
// Both run from emitGameStateEvents (every 5 s tick).

const (
	closingThreatGapBudgetMs   = 2500
	closingThreatMinShrinkMs   = 500
	closingThreatSamples       = 4
	closingThreatLatchResetMs  = 300
)

// emitOvertaken fires a P5 event the instant our position number goes
// up (we got passed). Per-lap dedup via DedupKey — at most one
// "overtaken" event per lap, even if positions trade twice.
func (e *Engine) emitOvertaken(state *models.RaceState, talkLevel int32) {
	if state == nil || state.Position == 0 {
		return
	}
	// Race sessions only. In practice/quali Position is a nominal lap-time
	// rank that shuffles whenever anyone in the garage sets a new sector,
	// so a "you got overtaken" call there is meaningless noise.
	if !isRaceSession(state.SessionType) {
		e.prevPosition = state.Position
		return
	}
	if e.prevPosition == 0 {
		e.prevPosition = state.Position
		return
	}
	if state.Position <= e.prevPosition {
		// Same position or I passed someone; "passed" is its own future
		// event, we only fire on getting passed.
		e.prevPosition = state.Position
		return
	}

	dropped := state.Position - e.prevPosition
	prev := e.prevPosition
	e.prevPosition = state.Position

	// Find the car that now occupies our previous position — that's whoever
	// passed us. Falls back gracefully if the grid hasn't reported yet.
	passerName, passerIdx := "", uint8(255)
	if e.roster != nil {
		for i := 0; i < 22; i++ {
			gp := state.GridPositions[i]
			if gp.ResultStatus < 2 || uint8(i) == state.PlayerCarIndex {
				continue
			}
			if gp.Position == prev {
				if name := e.roster.ResolveRadioName(uint8(i)); name != "" {
					passerName = name
					passerIdx = uint8(i)
				}
				break
			}
		}
	}

	summary := fmt.Sprintf("Overtaken — now P%d.", state.Position)
	if passerName != "" {
		summary = fmt.Sprintf("%s through — now P%d.", passerName, state.Position)
	}
	reasoning := fmt.Sprintf(
		"Position dropped from P%d to P%d on lap %d. On %s lap %d.",
		prev, state.Position, state.CurrentLap,
		enums.Compound(state.VisualCompound), state.TyresAgeLaps,
	)
	if passerName != "" {
		reasoning = fmt.Sprintf(
			"%s passed us — P%d → P%d on lap %d. On %s lap %d.",
			passerName, prev, state.Position, state.CurrentLap,
			enums.Compound(state.VisualCompound), state.TyresAgeLaps,
		)
	}

	debug := map[string]any{
		"prev_position":  int(prev),
		"new_position":   int(state.Position),
		"positions_lost": int(dropped),
		"lap":            int(state.CurrentLap),
		"compound":       enums.Compound(state.VisualCompound),
	}
	if passerName != "" {
		debug["passer_driver"] = passerName
		debug["passer_car_index"] = int(passerIdx)
	}

	ev := brain.Event{
		Type:      brain.EventOvertaken,
		Priority:  brain.DefaultEventPriority(brain.EventOvertaken),
		Summary:   summary,
		Reasoning: reasoning,
		DebugData: debug,
		Source:    "rule:overtaken",
		DedupKey:  fmt.Sprintf("overtaken:%d", state.CurrentLap),
	}
	if passerIdx != 255 {
		ev.DedupSubject = fmt.Sprintf("car:%d", passerIdx)
	}
	enqueueAndLog(e, ev, talkLevel)
}

// emitClosingThreat fires a P4 event when the car directly behind us is
// closing fast and within striking distance. Computed by sampling the
// behind car's delta_to_car_in_front_in_ms over a sliding window — that
// delta IS our gap to them when they sit at P+1.
//
// Silent when:
//   - we're in pit / out lap / in lap (gap is meaningless)
//   - we're the last car (no one behind)
//   - the reader isn't wired
//   - we have <3 samples (need a trend)
//   - the gap is widening or already over the budget
//   - the latch is set and the gap hasn't re-opened from its minimum
func (e *Engine) emitClosingThreat(state *models.RaceState, talkLevel int32) {
	if state == nil || state.Position == 0 || e.reader == nil {
		return
	}
	// Race sessions only — same reason as emitOvertaken. The car at "P+1"
	// in practice may be on the other side of the circuit on a totally
	// different program. EventTrafficUpdate covers the proximity case
	// for non-race sessions using actual on-track distance.
	if !isRaceSession(state.SessionType) {
		e.gapBehindSamples = nil
		e.closingThreatLatch = false
		return
	}
	// Only on flying / on-track laps. Pit / out / in laps invalidate the
	// closing math.
	switch state.DriverStatus {
	case 1, 4: // Flying Lap, On Track
	default:
		e.gapBehindSamples = nil
		e.closingThreatLatch = false
		return
	}
	if state.PitStatus != 0 {
		e.gapBehindSamples = nil
		e.closingThreatLatch = false
		return
	}

	gapMs, behindIdx, behindCompound, behindTireAge, ok := e.readGapBehind(int(state.Position), int(state.PlayerCarIndex))
	if !ok || gapMs <= 0 {
		return
	}

	// Discard the ring on a position change — pit cycles or trades poison
	// the trend.
	if len(e.gapBehindSamples) > 0 && e.gapBehindSamples[len(e.gapBehindSamples)-1].myPos != state.Position {
		e.gapBehindSamples = nil
		e.closingThreatLatch = false
	}

	now := time.Now()
	e.gapBehindSamples = append(e.gapBehindSamples, gapSample{
		at:    now,
		gapMs: gapMs,
		myPos: state.Position,
	})
	if len(e.gapBehindSamples) > closingThreatSamples {
		e.gapBehindSamples = e.gapBehindSamples[len(e.gapBehindSamples)-closingThreatSamples:]
	}

	// Latch reset: gap re-opened beyond the threshold from the minimum
	// of the current window → allow a fresh fire.
	if e.closingThreatLatch {
		minGap := e.gapBehindSamples[0].gapMs
		for _, s := range e.gapBehindSamples {
			if s.gapMs < minGap {
				minGap = s.gapMs
			}
		}
		if gapMs-minGap > closingThreatLatchResetMs {
			e.closingThreatLatch = false
		}
		return
	}

	if len(e.gapBehindSamples) < 3 {
		return
	}
	if gapMs > closingThreatGapBudgetMs {
		return
	}

	oldest := e.gapBehindSamples[0]
	newest := e.gapBehindSamples[len(e.gapBehindSamples)-1]
	shrinkMs := int32(oldest.gapMs - newest.gapMs)
	if shrinkMs < closingThreatMinShrinkMs {
		return
	}

	dt := newest.at.Sub(oldest.at).Seconds()
	if dt <= 0 {
		dt = 1
	}
	rateMsPerSec := float64(shrinkMs) / dt
	// 90s-lap-equivalent shrink rate, in seconds per lap, for the radio call.
	ratePerLap := rateMsPerSec * 90.0 / 1000.0

	e.closingThreatLatch = true
	label := fmt.Sprintf("P%d", int(state.Position)+1)
	if e.roster != nil {
		if name := e.roster.ResolveRadioName(uint8(behindIdx)); name != "" {
			label = name
		}
	}
	ev := brain.Event{
		Type:     brain.EventThreatOvertake,
		Priority: 4, // override default P3 — this is "they're about to pass you"
		Summary: fmt.Sprintf("%s closing — gap %.1fs.",
			label, float64(gapMs)/1000.0),
		Reasoning: fmt.Sprintf(
			"%s on %s lap %d, gap shrunk %.1fs over the last %.0fs (~%.1fs/lap).",
			label, behindCompound, behindTireAge,
			float64(shrinkMs)/1000.0, dt, ratePerLap,
		),
		DebugData: map[string]any{
			"car_index":          behindIdx,
			"driver":             label,
			"position":           int(state.Position) + 1,
			"compound":           behindCompound,
			"tire_age_laps":      behindTireAge,
			"gap_to_me_sec":      round1Sec(gapMs),
			"shrink_sec":         round1Sec(shrinkMs),
			"window_sec":         int(dt),
			"rate_ms_per_second": int(rateMsPerSec),
		},
		Source:       "rule:closing_threat",
		DedupKey:     fmt.Sprintf("closing_threat:%d:%d", state.CurrentLap, behindIdx),
		DedupSubject: fmt.Sprintf("car:%d", behindIdx),
	}
	enqueueAndLog(e, ev, talkLevel)
}

// readGapBehind queries the latest lap_data + car_status for the car at
// myPos+1 and returns its gap to the player (positive ms = behind us),
// car_index, decoded compound name, and tire age. Returns ok=false on
// any error or missing data.
func (e *Engine) readGapBehind(myPos, myIdx int) (gapMs int32, behindIdx int, compound string, tireAge int, ok bool) {
	if e.reader == nil {
		return 0, 0, "", 0, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	const q = `
WITH latest_lap AS (
  SELECT car_index, car_position, delta_to_car_in_front_in_ms,
         ROW_NUMBER() OVER (PARTITION BY car_index ORDER BY timestamp DESC) AS rn
  FROM lap_data
), latest_status AS (
  SELECT car_index, visual_tyre_compound, tyres_age_laps,
         ROW_NUMBER() OVER (PARTITION BY car_index ORDER BY timestamp DESC) AS rn
  FROM car_status
)
SELECT ll.car_index, ll.delta_to_car_in_front_in_ms,
       ls.visual_tyre_compound, ls.tyres_age_laps
FROM latest_lap ll
JOIN latest_status ls ON ll.car_index = ls.car_index AND ls.rn = 1
WHERE ll.rn = 1 AND ll.car_position = ?
LIMIT 1
`
	var idx int
	var deltaMs int
	var compInt int
	var age int
	if err := e.reader.QueryRowContext(ctx, q, myPos+1).Scan(&idx, &deltaMs, &compInt, &age); err != nil {
		return 0, 0, "", 0, false
	}
	if idx == myIdx {
		return 0, 0, "", 0, false
	}
	return int32(deltaMs), idx, enums.Compound(uint8(compInt)), age, true
}

func round1Sec(ms int32) float64 {
	v := float64(ms) / 1000.0
	return float64(int64(v*10+0.5)) / 10.0
}
