package api

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/trackmap"
)

// brake_balance_handler.go — Package F of the coaching plan.
//
// Walks the player's hi-freq samples for the last N laps, groups contiguous
// brake events (brake > brakePointThreshold for ≥2 samples), and for each
// event computes per-wheel asymmetry signals:
//
//   - delta_front_temp_lr   = max(brake_temp_fl) - max(brake_temp_fr)
//   - delta_rear_temp_lr    = max(brake_temp_rl) - max(brake_temp_rr)
//   - front_rear_bias_temp  = avg(FL+FR) - avg(RL+RR) during the event
//   - lock_up_flags         = bitmap of wheels whose tyre_surf_temp jumped
//                             ≥ lockUpJumpC above pre-event baseline.
//   - pressure_drift_flags  = wheels whose post-event tyre_pressure differs
//                             ≥ pressDriftPsi from the pre-event baseline.
//
// The endpoint returns the raw event list plus a small set of rule-based
// diagnosis strings. Folded into /api/coaching/corner_report as well.

// Tunable thresholds. Conservative defaults — better to under-fire than to
// generate a coaching cue for every minor temperature wobble.
const (
	lockUpJumpC      = 25.0 // °C spike on a single wheel within ~0.5s
	pressDriftPsi    = 0.2  // PSI drift sustained post-event
	frontHotDeltaC   = 30.0 // FL vs FR temp delta that flags asymmetric front braking
	frontRearHotC    = 100.0 // front-vs-rear temp gap that flags front bias
	minEventSamples  = 2     // minimum hifreq samples for a brake event
)

// brakeEvent is one contiguous brake zone observed in the hifreq stream.
type brakeEvent struct {
	Lap                int      `json:"lap"`
	StartLapDistanceM  float64  `json:"start_lap_distance_m"`
	EndLapDistanceM    float64  `json:"end_lap_distance_m"`
	DurationSamples    int      `json:"duration_samples"`
	MaxBrake           float64  `json:"max_brake"`
	MinSpeedKmh        float64  `json:"min_speed_kmh"`
	DeltaFrontTempLR   *float64 `json:"delta_front_temp_lr,omitempty"`
	DeltaRearTempLR    *float64 `json:"delta_rear_temp_lr,omitempty"`
	FrontRearBiasTempC *float64 `json:"front_rear_bias_temp_c,omitempty"`
	LockUpFlags        []string `json:"lock_up_flags,omitempty"`
	PressureDriftFlags []string `json:"pressure_drift_flags,omitempty"`
	NearestCornerID    string   `json:"nearest_corner_id,omitempty"`
	NearestCornerName  string   `json:"nearest_corner_name,omitempty"`
}

type brakeBalanceResponse struct {
	SessionUID   string        `json:"session_uid"`
	TrackID      int           `json:"track_id"`
	TrackLengthM float64       `json:"track_length_m"`
	LapsScanned  []int         `json:"laps_scanned"`
	Events       []brakeEvent  `json:"events"`
	Diagnosis    []string      `json:"diagnosis,omitempty"`
	Note         string        `json:"note,omitempty"`
}

// brakeBalanceHandler powers GET /api/laps/brake_balance.
func brakeBalanceHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		ctx := newLapCtx(c, deps)
		state, err := ctx.resolve()
		if err != nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": err.Error()})
		}
		trackLen := float64(state.trackLength)

		recentLaps := parseIntDefault(c.Query("recent_laps"), 3)
		if recentLaps < 1 {
			recentLaps = 1
		}
		if recentLaps > 15 {
			recentLaps = 15
		}

		// Optional corner filter — limit reported events to those whose start
		// falls inside a corner window.
		cornerFilter := strings.TrimSpace(c.Query("corner"))
		var filterCorner *trackmap.Corner
		if cornerFilter != "" {
			corners := lookupCorners(deps.TrackMap, int8(state.trackID))
			matches := filterCorners(corners, cornerFilter)
			if len(matches) > 0 {
				cc := matches[0]
				filterCorner = &cc
			}
		}

		recents, err := recentCompletedLaps(c.Context(), deps, state.sessionUID, state.playerCarIndex, recentLaps)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}

		lapsScanned := make([]int, 0, len(recents))
		var events []brakeEvent
		corners := lookupCorners(deps.TrackMap, int8(state.trackID))
		for _, r := range recents {
			lapsScanned = append(lapsScanned, r.lap)
			samples, err := loadBrakeBalanceSamples(c.Context(), deps, state.sessionUID, r.lap)
			if err != nil {
				continue
			}
			lapEvents := detectBrakeEvents(samples, r.lap, trackLen)
			for i := range lapEvents {
				lapEvents[i].NearestCornerID, lapEvents[i].NearestCornerName = nearestCorner(corners, lapEvents[i].StartLapDistanceM)
			}
			if filterCorner != nil {
				kept := lapEvents[:0]
				for _, e := range lapEvents {
					if e.NearestCornerID == filterCorner.ID {
						kept = append(kept, e)
					}
				}
				lapEvents = kept
			}
			events = append(events, lapEvents...)
		}

		resp := brakeBalanceResponse{
			SessionUID:   uidString(state.sessionUID),
			TrackID:      state.trackID,
			TrackLengthM: trackLen,
			LapsScanned:  lapsScanned,
			Events:       events,
		}
		resp.Diagnosis = diagnoseBrakeBalance(events)
		if len(events) == 0 {
			resp.Note = "no qualifying brake events in scanned laps — check that hi-freq capture is enabled"
		}
		return c.JSON(resp)
	}
}

// brakeBalanceSample is the per-wheel sample shape used by the asymmetry
// detector. Extends hifreqSample but with all four corners broken out.
type brakeBalanceSample struct {
	trackPos float64
	totalDist float64
	brake    float64
	speed    float64
	brakeTempFL, brakeTempFR, brakeTempRL, brakeTempRR float64
	surfTempFL, surfTempFR, surfTempRL, surfTempRR    float64
	pressFL, pressFR, pressRL, pressRR float64
}

// loadBrakeBalanceSamples pulls the per-wheel hifreq rows for one lap. SQL
// matches lap_analysis_handlers.loadLapSamples but with the wheel-specific
// columns added.
func loadBrakeBalanceSamples(ctx context.Context, deps *Deps, uid uint64, lap int) ([]brakeBalanceSample, error) {
	sql := fmt.Sprintf(`SELECT track_position, total_distance, speed, brake,
		brake_temp_fl, brake_temp_fr, brake_temp_rl, brake_temp_rr,
		tyre_surf_temp_fl, tyre_surf_temp_fr, tyre_surf_temp_rl, tyre_surf_temp_rr,
		tyre_pressure_fl, tyre_pressure_fr, tyre_pressure_rl, tyre_pressure_rr
FROM telemetry_hifreq
WHERE session_uid = %s AND lap = %d AND pit_status = 0
ORDER BY total_distance`, uidString(uid), lap)
	rows, err := deps.Store.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	out := make([]brakeBalanceSample, 0, len(rows))
	for _, r := range rows {
		out = append(out, brakeBalanceSample{
			trackPos:  toFloat(r["track_position"]),
			totalDist: toFloat(r["total_distance"]),
			speed:     toFloat(r["speed"]),
			brake:     toFloat(r["brake"]),
			brakeTempFL: toFloat(r["brake_temp_fl"]),
			brakeTempFR: toFloat(r["brake_temp_fr"]),
			brakeTempRL: toFloat(r["brake_temp_rl"]),
			brakeTempRR: toFloat(r["brake_temp_rr"]),
			surfTempFL:  toFloat(r["tyre_surf_temp_fl"]),
			surfTempFR:  toFloat(r["tyre_surf_temp_fr"]),
			surfTempRL:  toFloat(r["tyre_surf_temp_rl"]),
			surfTempRR:  toFloat(r["tyre_surf_temp_rr"]),
			pressFL:     toFloat(r["tyre_pressure_fl"]),
			pressFR:     toFloat(r["tyre_pressure_fr"]),
			pressRL:     toFloat(r["tyre_pressure_rl"]),
			pressRR:     toFloat(r["tyre_pressure_rr"]),
		})
	}
	return out, nil
}

// detectBrakeEvents groups consecutive samples with brake > threshold into
// brakeEvents and computes asymmetry signals. A short ≤1-sample gap inside
// an event is tolerated to avoid splitting a single event on a noisy
// release.
func detectBrakeEvents(samples []brakeBalanceSample, lap int, trackLen float64) []brakeEvent {
	if len(samples) == 0 {
		return nil
	}
	var events []brakeEvent
	type span struct {
		startIdx, endIdx int
	}
	var spans []span
	inEvent := false
	var cur span
	gap := 0
	for i, s := range samples {
		if s.brake > brakePointThreshold {
			if !inEvent {
				cur = span{startIdx: i, endIdx: i}
				inEvent = true
			} else {
				cur.endIdx = i
			}
			gap = 0
		} else {
			if inEvent {
				gap++
				if gap > 1 {
					if cur.endIdx-cur.startIdx+1 >= minEventSamples {
						spans = append(spans, cur)
					}
					inEvent = false
					gap = 0
				}
			}
		}
	}
	if inEvent && cur.endIdx-cur.startIdx+1 >= minEventSamples {
		spans = append(spans, cur)
	}

	for _, sp := range spans {
		ev := brakeEvent{Lap: lap, DurationSamples: sp.endIdx - sp.startIdx + 1}
		ev.StartLapDistanceM = samples[sp.startIdx].trackPos
		ev.EndLapDistanceM = samples[sp.endIdx].trackPos
		// Per-event aggregates
		var maxFL, maxFR, maxRL, maxRR float64
		var sumFront, sumRear float64
		minSpeed := math.MaxFloat64
		maxBrake := 0.0
		for i := sp.startIdx; i <= sp.endIdx; i++ {
			s := samples[i]
			if s.brakeTempFL > maxFL {
				maxFL = s.brakeTempFL
			}
			if s.brakeTempFR > maxFR {
				maxFR = s.brakeTempFR
			}
			if s.brakeTempRL > maxRL {
				maxRL = s.brakeTempRL
			}
			if s.brakeTempRR > maxRR {
				maxRR = s.brakeTempRR
			}
			sumFront += s.brakeTempFL + s.brakeTempFR
			sumRear += s.brakeTempRL + s.brakeTempRR
			if s.speed < minSpeed {
				minSpeed = s.speed
			}
			if s.brake > maxBrake {
				maxBrake = s.brake
			}
		}
		n := float64(ev.DurationSamples)
		if n > 0 {
			frontAvg := sumFront / (2 * n)
			rearAvg := sumRear / (2 * n)
			bias := frontAvg - rearAvg
			ev.FrontRearBiasTempC = floatPtr(bias)
		}
		if maxFL > 0 || maxFR > 0 {
			d := maxFL - maxFR
			ev.DeltaFrontTempLR = floatPtr(d)
		}
		if maxRL > 0 || maxRR > 0 {
			d := maxRL - maxRR
			ev.DeltaRearTempLR = floatPtr(d)
		}
		ev.MaxBrake = maxBrake
		if minSpeed != math.MaxFloat64 {
			ev.MinSpeedKmh = minSpeed
		}

		// Lock-up detection — look at the 1-second pre-event baseline for
		// each tyre surface temp and flag any wheel that jumped ≥ lockUpJumpC
		// within the first 0.5s of the event.
		baseStart := sp.startIdx
		// Walk back ~10 samples (~1s at 10Hz) for baseline.
		baseEnd := baseStart - 10
		if baseEnd < 0 {
			baseEnd = 0
		}
		baseFL, baseFR, baseRL, baseRR := meanSurfTemps(samples, baseEnd, baseStart-1)
		windowEnd := sp.startIdx + 5
		if windowEnd > sp.endIdx {
			windowEnd = sp.endIdx
		}
		peakFL, peakFR, peakRL, peakRR := maxSurfTemps(samples, sp.startIdx, windowEnd)
		var locks []string
		if peakFL-baseFL >= lockUpJumpC {
			locks = append(locks, "FL")
		}
		if peakFR-baseFR >= lockUpJumpC {
			locks = append(locks, "FR")
		}
		if peakRL-baseRL >= lockUpJumpC {
			locks = append(locks, "RL")
		}
		if peakRR-baseRR >= lockUpJumpC {
			locks = append(locks, "RR")
		}
		ev.LockUpFlags = locks

		// Pressure drift — compare 5-sample mean pre vs 5-sample mean post.
		postStart := sp.endIdx + 1
		postEnd := postStart + 5
		if postEnd >= len(samples) {
			postEnd = len(samples) - 1
		}
		preFL, preFR, preRL, preRR := meanPressures(samples, baseEnd, sp.startIdx-1)
		postFLm, postFRm, postRLm, postRRm := meanPressures(samples, postStart, postEnd)
		var drift []string
		if math.Abs(postFLm-preFL) >= pressDriftPsi {
			drift = append(drift, "FL")
		}
		if math.Abs(postFRm-preFR) >= pressDriftPsi {
			drift = append(drift, "FR")
		}
		if math.Abs(postRLm-preRL) >= pressDriftPsi {
			drift = append(drift, "RL")
		}
		if math.Abs(postRRm-preRR) >= pressDriftPsi {
			drift = append(drift, "RR")
		}
		ev.PressureDriftFlags = drift

		_ = trackLen
		events = append(events, ev)
	}
	return events
}

func meanSurfTemps(samples []brakeBalanceSample, lo, hi int) (fl, fr, rl, rr float64) {
	if lo < 0 || hi < lo || hi >= len(samples) {
		return 0, 0, 0, 0
	}
	var n float64
	for i := lo; i <= hi; i++ {
		fl += samples[i].surfTempFL
		fr += samples[i].surfTempFR
		rl += samples[i].surfTempRL
		rr += samples[i].surfTempRR
		n++
	}
	if n == 0 {
		return 0, 0, 0, 0
	}
	return fl / n, fr / n, rl / n, rr / n
}

func maxSurfTemps(samples []brakeBalanceSample, lo, hi int) (fl, fr, rl, rr float64) {
	if lo < 0 || hi < lo || hi >= len(samples) {
		return 0, 0, 0, 0
	}
	for i := lo; i <= hi; i++ {
		if samples[i].surfTempFL > fl {
			fl = samples[i].surfTempFL
		}
		if samples[i].surfTempFR > fr {
			fr = samples[i].surfTempFR
		}
		if samples[i].surfTempRL > rl {
			rl = samples[i].surfTempRL
		}
		if samples[i].surfTempRR > rr {
			rr = samples[i].surfTempRR
		}
	}
	return
}

func meanPressures(samples []brakeBalanceSample, lo, hi int) (fl, fr, rl, rr float64) {
	if lo < 0 || hi < lo || hi >= len(samples) {
		return 0, 0, 0, 0
	}
	var n float64
	for i := lo; i <= hi; i++ {
		fl += samples[i].pressFL
		fr += samples[i].pressFR
		rl += samples[i].pressRL
		rr += samples[i].pressRR
		n++
	}
	if n == 0 {
		return 0, 0, 0, 0
	}
	return fl / n, fr / n, rl / n, rr / n
}

// nearestCorner returns the curated corner whose lap distance is closest to
// pos. Empty strings if no corners are curated. Distance is computed on the
// straight line without wrap-around — good enough; the S/F-line edge case
// rarely matters for asymmetry diagnosis.
func nearestCorner(corners []trackmap.Corner, pos float64) (id, name string) {
	if len(corners) == 0 {
		return "", ""
	}
	best := math.MaxFloat64
	for _, c := range corners {
		d := math.Abs(float64(c.LapDistanceM) - pos)
		if d < best {
			best = d
			id, name = c.ID, c.Name
		}
	}
	return
}

// diagnoseBrakeBalance turns the per-event metrics into a small list of
// coaching strings. Empty when nothing crosses the threshold.
func diagnoseBrakeBalance(events []brakeEvent) []string {
	if len(events) == 0 {
		return nil
	}
	var (
		hotFL, hotFR    int
		lockCount       = map[string]int{}
		biasFrontEvents int
		total           int
		sumFrontLR      float64
		sumRearLR       float64
		sumBias         float64
		nFront          int
		nRear           int
		nBias           int
	)
	for _, e := range events {
		total++
		if e.DeltaFrontTempLR != nil {
			sumFrontLR += *e.DeltaFrontTempLR
			nFront++
			if *e.DeltaFrontTempLR > frontHotDeltaC {
				hotFL++
			} else if *e.DeltaFrontTempLR < -frontHotDeltaC {
				hotFR++
			}
		}
		if e.DeltaRearTempLR != nil {
			sumRearLR += *e.DeltaRearTempLR
			nRear++
		}
		if e.FrontRearBiasTempC != nil {
			sumBias += *e.FrontRearBiasTempC
			nBias++
			if *e.FrontRearBiasTempC > frontRearHotC {
				biasFrontEvents++
			}
		}
		for _, w := range e.LockUpFlags {
			lockCount[w]++
		}
	}
	var out []string
	if nFront >= 2 && hotFL >= (nFront+1)/2 && nFront > 0 {
		avg := sumFrontLR / float64(nFront)
		out = append(out, fmt.Sprintf("front-left brake running hot — avg L-R delta %.0f°C across %d events (check bias or trail-braking line)", avg, hotFL))
	}
	if nFront >= 2 && hotFR >= (nFront+1)/2 && nFront > 0 {
		avg := sumFrontLR / float64(nFront)
		out = append(out, fmt.Sprintf("front-right brake running hot — avg L-R delta %.0f°C across %d events", avg, hotFR))
	}
	if biasFrontEvents >= 2 && nBias > 0 {
		out = append(out, fmt.Sprintf("front bias appears too aggressive — avg front-rear gap %.0f°C, try shifting bias rearward", sumBias/float64(nBias)))
	}
	for _, w := range []string{"FL", "FR", "RL", "RR"} {
		if lockCount[w] >= 2 {
			out = append(out, fmt.Sprintf("locking %s on %d brake events — ease initial pedal pressure", w, lockCount[w]))
		}
	}
	return out
}
