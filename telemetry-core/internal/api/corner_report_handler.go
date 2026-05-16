package api

import (
	"fmt"
	"strings"

	"github.com/gofiber/fiber/v2"
)

// corner_report_handler.go — Package B of the coaching plan.
//
//   GET /api/coaching/corner_report?corner=T3&recent_laps=3
//
// Single-call synthesis the AI calls when the driver asks about a corner.
// Bundles:
//   - brake-history (Package A) for the corner
//   - corner-compare apex / exit deltas vs best (existing cornerCompare)
//   - brake-balance asymmetry events filtered to this corner's window
//   - a small set of rule-based diagnosis strings
//
// Saves the LLM 3+ tool calls and gives it a pre-digested signal.

type cornerReportResponse struct {
	SessionUID    string                 `json:"session_uid"`
	TrackID       int                    `json:"track_id"`
	TrackLengthM  float64                `json:"track_length_m"`
	Corner        brakePointCornerRef    `json:"corner"`
	BestLap       *brakePointLap         `json:"best_lap,omitempty"`
	RecentLaps    []brakePointLap        `json:"recent_laps"`
	Consistency   *float64               `json:"consistency_stddev_m,omitempty"`
	MeanDelta     *float64               `json:"mean_delta_to_best_m,omitempty"`
	Trend         string                 `json:"trend,omitempty"`
	ApexDelta     *cornerCompare         `json:"apex_delta,omitempty"`
	BrakeBalance  *brakeBalanceForCorner `json:"brake_balance,omitempty"`
	Diagnosis     []string               `json:"diagnosis"`
	Headline      string                 `json:"headline"`
}

type brakeBalanceForCorner struct {
	Events    []brakeEvent `json:"events"`
	Diagnosis []string     `json:"diagnosis,omitempty"`
}

func cornerReportHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		ctx := newLapCtx(c, deps)
		state, err := ctx.resolve()
		if err != nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": err.Error()})
		}
		trackLen := float64(state.trackLength)

		cornerRef, err := resolveBrakeCorner(c, deps, state.trackID)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
		}

		recentLaps := parseIntDefault(c.Query("recent_laps"), 3)
		if recentLaps < 1 {
			recentLaps = 1
		}
		if recentLaps > 15 {
			recentLaps = 15
		}
		windowBefore := parseFloatDefault(c.Query("window_before_m"), 200)
		windowAfter := parseFloatDefault(c.Query("window_after_m"), 50)

		// --- Brake-history (Package A logic, inlined to avoid an HTTP hop)
		best, bestMs, _ := bestValidLap(c.Context(), deps, state.sessionUID, state.playerCarIndex)
		recents, _ := recentCompletedLaps(c.Context(), deps, state.sessionUID, state.playerCarIndex, recentLaps)

		samplesCache := map[int][]hifreqSample{}
		ensureSamples := func(lap int) []hifreqSample {
			if v, ok := samplesCache[lap]; ok {
				return v
			}
			s, _ := loadLapSamples(c.Context(), deps, state.sessionUID, lap)
			samplesCache[lap] = s
			return s
		}
		analyze := func(lap, lapMs int) brakePointLap {
			out := brakePointLap{Lap: lap, LapTimeMs: lapMs, Valid: true}
			samples := ensureSamples(lap)
			if len(samples) == 0 {
				out.Valid = false
				return out
			}
			if bp := findBrakePoint(samples, cornerRef.LapDistanceM, windowBefore, windowAfter, trackLen); bp != nil {
				out.BrakePointM = bp
			}
			if apex := findApex(samples, cornerRef.LapDistanceM, trackLen); apex != nil {
				out.MinSpeedKmh = floatPtr(apex.speed)
				out.ApexThrottle = floatPtr(apex.throttleAtApex)
			}
			if m := maxBrakeInWindow(samples, cornerRef.LapDistanceM, windowBefore, windowAfter, trackLen); m > 0 {
				out.MaxBrake = floatPtr(m)
			}
			return out
		}

		var bestEntry *brakePointLap
		if best > 0 {
			b := analyze(best, bestMs)
			bestEntry = &b
		}

		// recents → oldest first
		ordered := make([]resolvedLap, len(recents))
		for i, r := range recents {
			ordered[len(recents)-1-i] = r
		}
		recentEntries := make([]brakePointLap, 0, len(ordered))
		for _, r := range ordered {
			e := analyze(r.lap, r.lapTimeMs)
			if bestEntry != nil && bestEntry.BrakePointM != nil && e.BrakePointM != nil {
				d := *e.BrakePointM - *bestEntry.BrakePointM
				e.DeltaToBestM = floatPtr(d)
			}
			recentEntries = append(recentEntries, e)
		}

		resp := cornerReportResponse{
			SessionUID:   uidString(state.sessionUID),
			TrackID:      state.trackID,
			TrackLengthM: trackLen,
			Corner:       cornerRef,
			BestLap:      bestEntry,
			RecentLaps:   recentEntries,
		}
		if std, ok := stddevOfBrakePoints(recentEntries); ok {
			resp.Consistency = floatPtr(std)
		}
		if m, ok := meanOfDeltas(recentEntries); ok {
			resp.MeanDelta = floatPtr(m)
		}
		resp.Trend = trendOfBrakePoints(recentEntries, std3())

		// --- Apex / exit-throttle delta vs best (reuses findApex)
		if bestEntry != nil && len(recentEntries) > 0 {
			latest := recentEntries[len(recentEntries)-1]
			if latest.MinSpeedKmh != nil && bestEntry.MinSpeedKmh != nil {
				cc := &cornerCompare{
					ID:           cornerRef.ID,
					Name:         cornerRef.Name,
					LapDistanceM: cornerRef.LapDistanceM,
					BestApexSpeedKmh:  bestEntry.MinSpeedKmh,
					YourApexSpeedKmh:  latest.MinSpeedKmh,
					DeltaApexSpeedKmh: floatPtr(*latest.MinSpeedKmh - *bestEntry.MinSpeedKmh),
					BestExitThrottle:  bestEntry.ApexThrottle,
					YourExitThrottle:  latest.ApexThrottle,
				}
				if latest.ApexThrottle != nil && bestEntry.ApexThrottle != nil {
					cc.DeltaExitThrottle = floatPtr(*latest.ApexThrottle - *bestEntry.ApexThrottle)
				}
				if bestEntry.BrakePointM != nil {
					cc.BestBrakePointM = bestEntry.BrakePointM
				}
				if latest.BrakePointM != nil {
					cc.YourBrakePointM = latest.BrakePointM
				}
				if latest.BrakePointM != nil && bestEntry.BrakePointM != nil {
					cc.DeltaBrakePointM = floatPtr(*latest.BrakePointM - *bestEntry.BrakePointM)
				}
				resp.ApexDelta = cc
			}
		}

		// --- Brake-balance (Package F) filtered to this corner.
		var bb brakeBalanceForCorner
		for _, r := range recents {
			s, err := loadBrakeBalanceSamples(c.Context(), deps, state.sessionUID, r.lap)
			if err != nil {
				continue
			}
			evts := detectBrakeEvents(s, r.lap, trackLen)
			for _, e := range evts {
				// Keep events whose start falls inside the corner window.
				if inCornerWindow(e.StartLapDistanceM, cornerRef.LapDistanceM, windowBefore, windowAfter, trackLen) {
					bb.Events = append(bb.Events, e)
				}
			}
		}
		if len(bb.Events) > 0 {
			bb.Diagnosis = diagnoseBrakeBalance(bb.Events)
			resp.BrakeBalance = &bb
		}

		// --- Rule-based diagnosis
		resp.Diagnosis = synthesizeDiagnosis(resp)
		resp.Headline = synthesizeHeadline(resp)
		return c.JSON(resp)
	}
}

func inCornerWindow(pos, corner, before, after, lapLen float64) bool {
	start := corner - before
	end := corner + after
	switch {
	case start < 0:
		return (pos >= 0 && pos <= end) || (pos >= lapLen+start && pos <= lapLen)
	case end > lapLen:
		return (pos >= start && pos <= lapLen) || (pos >= 0 && pos <= end-lapLen)
	default:
		return pos >= start && pos <= end
	}
}

// synthesizeDiagnosis produces the rule-based coaching strings folded into
// the response. Mirrors the heuristics in the plan: stddev → consistency,
// mean delta → braking-too-late, apex+exit divergence → entry-speed issue.
func synthesizeDiagnosis(r cornerReportResponse) []string {
	var out []string
	if r.Consistency != nil && *r.Consistency > 30 {
		out = append(out, fmt.Sprintf("inconsistent brake point — stddev %.0fm across recent laps, pick a fixed reference marker", *r.Consistency))
	}
	if r.MeanDelta != nil {
		switch {
		case *r.MeanDelta < -25:
			out = append(out, fmt.Sprintf("braking %.0fm too early — try braking later by about %.0fm", -*r.MeanDelta, -*r.MeanDelta))
		case *r.MeanDelta > 25:
			out = append(out, fmt.Sprintf("braking %.0fm too late — back the brake point up by about %.0fm", *r.MeanDelta, *r.MeanDelta))
		}
	}
	if r.ApexDelta != nil && r.ApexDelta.DeltaApexSpeedKmh != nil && r.ApexDelta.DeltaExitThrottle != nil {
		if *r.ApexDelta.DeltaApexSpeedKmh > 5 && *r.ApexDelta.DeltaExitThrottle < -0.10 {
			out = append(out, "carrying too much entry speed — apex is fast but exit throttle is choked, costing the run onto the straight")
		}
	}
	if r.BrakeBalance != nil {
		out = append(out, r.BrakeBalance.Diagnosis...)
	}
	return out
}

func synthesizeHeadline(r cornerReportResponse) string {
	corner := strings.TrimSpace(r.Corner.ID)
	if corner == "" {
		corner = "this corner"
	}
	if len(r.Diagnosis) == 0 {
		if len(r.RecentLaps) == 0 {
			return fmt.Sprintf("no recent data for %s yet", corner)
		}
		return fmt.Sprintf("%s looks clean across last %d laps", corner, len(r.RecentLaps))
	}
	return fmt.Sprintf("%s — %s", corner, r.Diagnosis[0])
}
