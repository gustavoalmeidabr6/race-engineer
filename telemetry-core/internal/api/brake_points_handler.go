package api

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/trackmap"
)

// brake_points_handler.go answers "where is the driver actually braking for
// this corner across recent laps, and how consistently?".
//
//   GET /api/laps/brake_points
//     ?corner=T3                  corner_id from the curated trackmap, OR
//     ?lap_distance_m=820         raw lap-distance fallback when track geometry
//                                 isn't curated (used in tests too)
//     &recent_laps=3              how many recent completed laps to scan
//                                 (range 1..15, default 3)
//     &window_before_m=200        brake-window relative to corner apex
//     &window_after_m=50
//
// Reuses lap_analysis_handlers' findBrakePoint / loadLapSamples helpers so the
// "brake onset" definition stays identical to the compare endpoint. Output is
// shaped specifically for coaching: best-lap baseline, your last N laps with
// deltas, stddev across recent laps, and a one-word trend tag.
//
// This is Package A of the brake-point coaching plan.

type brakePointLap struct {
	Lap             int      `json:"lap"`
	BrakePointM     *float64 `json:"brake_point_m,omitempty"`
	DeltaToBestM    *float64 `json:"delta_to_best_m,omitempty"`
	MinSpeedKmh     *float64 `json:"min_speed_kmh,omitempty"`
	MaxBrake        *float64 `json:"max_brake,omitempty"`
	ApexThrottle    *float64 `json:"throttle_at_apex,omitempty"`
	LapTimeMs       int      `json:"lap_time_ms,omitempty"`
	Valid           bool     `json:"valid"`
}

type brakePointsResponse struct {
	SessionUID    string          `json:"session_uid"`
	TrackID       int             `json:"track_id"`
	TrackLengthM  float64         `json:"track_length_m"`
	Corner        brakePointCornerRef `json:"corner"`
	WindowBeforeM float64         `json:"window_before_m"`
	WindowAfterM  float64         `json:"window_after_m"`
	BestLap       *brakePointLap  `json:"best_lap,omitempty"`
	RecentLaps    []brakePointLap `json:"recent_laps"`
	// ConsistencyStddevM is the stddev of brake-onset distance across the
	// recent_laps entries that actually fired. Nil when fewer than 2 laps
	// have data — stddev of one point is meaningless.
	ConsistencyStddevM *float64 `json:"consistency_stddev_m,omitempty"`
	// MeanDeltaToBestM is the mean of recent_laps' delta_to_best_m. Helpful
	// for the rule-based diagnosis ("braking 30m late on average"). Nil when
	// no recent lap has both brake_point_m and best_brake_point_m defined.
	MeanDeltaToBestM *float64 `json:"mean_delta_to_best_m,omitempty"`
	// Trend is a one-word coaching tag derived from the slope of brake_point_m
	// over recent_laps (oldest to newest): "braking later and later",
	// "braking earlier and earlier", "consistent", or "" when too few points.
	Trend string `json:"trend,omitempty"`
	Note  string `json:"note,omitempty"`
}

type brakePointCornerRef struct {
	ID           string  `json:"id"`
	Name         string  `json:"name,omitempty"`
	LapDistanceM float64 `json:"lap_distance_m"`
}

// brakePointsHandler powers GET /api/laps/brake_points.
func brakePointsHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		ctx := newLapCtx(c, deps)
		state, err := ctx.resolve()
		if err != nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": err.Error()})
		}
		trackLen := float64(state.trackLength)

		// Resolve corner geometry.  Either ?corner=T3 (looked up in the
		// curated trackmap) or ?lap_distance_m=820 (raw fallback).  When
		// both are supplied, corner wins.
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

		// Best-lap baseline.
		best, bestMs, err := bestValidLap(c.Context(), deps, state.sessionUID, state.playerCarIndex)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}

		// Recent completed laps (most recent first).
		recents, err := recentCompletedLaps(c.Context(), deps, state.sessionUID, state.playerCarIndex, recentLaps)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}

		// Compute brake point + apex per lap. Best lap may overlap with one
		// of the recent ones — that's fine, just compute once and reference.
		samplesCache := map[int][]hifreqSample{}
		ensureSamples := func(lap int) []hifreqSample {
			if v, ok := samplesCache[lap]; ok {
				return v
			}
			s, _ := loadLapSamples(c.Context(), deps, state.sessionUID, lap)
			samplesCache[lap] = s
			return s
		}

		analyze := func(lap int, lapMs int) brakePointLap {
			out := brakePointLap{Lap: lap, LapTimeMs: lapMs, Valid: true}
			samples := ensureSamples(lap)
			if len(samples) == 0 {
				out.Valid = false
				return out
			}
			bp := findBrakePoint(samples, cornerRef.LapDistanceM, windowBefore, windowAfter, trackLen)
			if bp != nil {
				out.BrakePointM = bp
			}
			apex := findApex(samples, cornerRef.LapDistanceM, trackLen)
			if apex != nil {
				out.MinSpeedKmh = floatPtr(apex.speed)
				out.ApexThrottle = floatPtr(apex.throttleAtApex)
			}
			maxB := maxBrakeInWindow(samples, cornerRef.LapDistanceM, windowBefore, windowAfter, trackLen)
			if maxB > 0 {
				out.MaxBrake = floatPtr(maxB)
			}
			return out
		}

		var bestEntry *brakePointLap
		if best > 0 {
			b := analyze(best, bestMs)
			bestEntry = &b
		}

		recentEntries := make([]brakePointLap, 0, len(recents))
		// Walk recents in oldest-first order so Trend computation reads naturally.
		// recentCompletedLaps returns newest first; reverse here.
		orderedRecents := make([]resolvedLap, len(recents))
		for i, r := range recents {
			orderedRecents[len(recents)-1-i] = r
		}
		for _, r := range orderedRecents {
			e := analyze(r.lap, r.lapTimeMs)
			if bestEntry != nil && bestEntry.BrakePointM != nil && e.BrakePointM != nil {
				d := *e.BrakePointM - *bestEntry.BrakePointM
				e.DeltaToBestM = floatPtr(d)
			}
			recentEntries = append(recentEntries, e)
		}

		resp := brakePointsResponse{
			SessionUID:    uidString(state.sessionUID),
			TrackID:       state.trackID,
			TrackLengthM:  trackLen,
			Corner:        cornerRef,
			WindowBeforeM: windowBefore,
			WindowAfterM:  windowAfter,
			BestLap:       bestEntry,
			RecentLaps:    recentEntries,
		}

		// Stddev across recent laps with a defined brake point.
		if std, ok := stddevOfBrakePoints(recentEntries); ok {
			resp.ConsistencyStddevM = floatPtr(std)
		}
		// Mean delta to best across the same set.
		if mean, ok := meanOfDeltas(recentEntries); ok {
			resp.MeanDeltaToBestM = floatPtr(mean)
		}
		resp.Trend = trendOfBrakePoints(recentEntries, std3())
		if bestEntry == nil {
			resp.Note = "no valid best-lap baseline yet — deltas suppressed"
		} else if len(recentEntries) == 0 {
			resp.Note = "no completed recent laps to analyze"
		}

		return c.JSON(resp)
	}
}

// std3 returns a small constant threshold (in metres) below which we tag a
// brake-point series as "consistent" rather than trending. Exposed as a
// function so test files can swap it if needed in the future.
func std3() float64 { return 6.0 }

// resolveBrakeCorner picks the corner geometry from either ?corner= (curated
// trackmap) or ?lap_distance_m= (raw). Returns a ref usable in both the
// response body and the brake-window math.
func resolveBrakeCorner(c *fiber.Ctx, deps *Deps, trackID int) (brakePointCornerRef, error) {
	cornerID := strings.TrimSpace(c.Query("corner"))
	if cornerID != "" {
		corners := lookupCorners(deps.TrackMap, int8(trackID))
		if len(corners) == 0 {
			return brakePointCornerRef{}, fmt.Errorf("no curated corners for track_id=%d; use lap_distance_m instead", trackID)
		}
		filtered := filterCorners(corners, cornerID)
		if len(filtered) == 0 {
			return brakePointCornerRef{}, fmt.Errorf("corner %q not found in trackmap", cornerID)
		}
		var match trackmap.Corner = filtered[0]
		return brakePointCornerRef{
			ID:           match.ID,
			Name:         match.Name,
			LapDistanceM: float64(match.LapDistanceM),
		}, nil
	}
	if raw := strings.TrimSpace(c.Query("lap_distance_m")); raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || v <= 0 {
			return brakePointCornerRef{}, fmt.Errorf("lap_distance_m must be a positive number")
		}
		return brakePointCornerRef{
			ID:           fmt.Sprintf("@%.0fm", v),
			LapDistanceM: v,
		}, nil
	}
	return brakePointCornerRef{}, fmt.Errorf("either ?corner= or ?lap_distance_m= is required")
}

// maxBrakeInWindow returns the peak brake reading inside the corner window.
// Used as the "max_brake" coaching signal — a driver who never crosses ~0.9
// is leaving margin on the table.
func maxBrakeInWindow(samples []hifreqSample, cornerDist, before, after, lapLen float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	start := cornerDist - before
	end := cornerDist + after
	var peak float64
	for _, s := range samples {
		pos := s.trackPos
		var in bool
		switch {
		case start < 0:
			in = (pos >= 0 && pos <= end) || (pos >= lapLen+start && pos <= lapLen)
		case end > lapLen:
			in = (pos >= start && pos <= lapLen) || (pos >= 0 && pos <= end-lapLen)
		default:
			in = pos >= start && pos <= end
		}
		if !in {
			continue
		}
		if s.brake > peak {
			peak = s.brake
		}
	}
	return peak
}

// stddevOfBrakePoints computes the sample stddev of brake_point_m across
// entries that actually fired. Returns (0, false) when fewer than 2 points.
func stddevOfBrakePoints(entries []brakePointLap) (float64, bool) {
	vals := make([]float64, 0, len(entries))
	for _, e := range entries {
		if e.BrakePointM != nil {
			vals = append(vals, *e.BrakePointM)
		}
	}
	if len(vals) < 2 {
		return 0, false
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	mean := sum / float64(len(vals))
	var sq float64
	for _, v := range vals {
		d := v - mean
		sq += d * d
	}
	return math.Sqrt(sq / float64(len(vals)-1)), true
}

func meanOfDeltas(entries []brakePointLap) (float64, bool) {
	vals := make([]float64, 0, len(entries))
	for _, e := range entries {
		if e.DeltaToBestM != nil {
			vals = append(vals, *e.DeltaToBestM)
		}
	}
	if len(vals) == 0 {
		return 0, false
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals)), true
}

// trendOfBrakePoints assigns a one-word coaching tag from the per-lap
// brake-point series (oldest to newest). When the linear-fit slope's
// absolute value is below stableThresh metres-per-lap, returns "consistent";
// positive slope → braking later and later; negative → earlier and earlier.
func trendOfBrakePoints(entries []brakePointLap, stableThresh float64) string {
	type pt struct{ x, y float64 }
	pts := make([]pt, 0, len(entries))
	for i, e := range entries {
		if e.BrakePointM == nil {
			continue
		}
		pts = append(pts, pt{x: float64(i), y: *e.BrakePointM})
	}
	if len(pts) < 2 {
		return ""
	}
	// Simple least-squares slope.
	var sx, sy, sxy, sxx float64
	for _, p := range pts {
		sx += p.x
		sy += p.y
		sxy += p.x * p.y
		sxx += p.x * p.x
	}
	n := float64(len(pts))
	denom := n*sxx - sx*sx
	if denom == 0 {
		return "consistent"
	}
	slope := (n*sxy - sx*sy) / denom
	switch {
	case math.Abs(slope) < stableThresh:
		return "consistent"
	case slope > 0:
		return "braking later and later"
	default:
		return "braking earlier and earlier"
	}
}

// sortRecents keeps a stable oldest-first ordering by lap number. Not
// currently used by the handler but kept for tests that need deterministic
// input ordering.
func sortRecents(in []brakePointLap) []brakePointLap {
	out := make([]brakePointLap, len(in))
	copy(out, in)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Lap < out[j].Lap })
	return out
}

// _ silences the unused-variable warning in builds that strip sortRecents.
var _ context.Context
