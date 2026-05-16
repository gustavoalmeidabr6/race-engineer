package api

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/storage"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/trackmap"
)

// lap_analysis_handlers.go answers "show me the throttle/brake shape over the
// last 3 laps" and "where am I braking too early vs my best lap, in meters."
//
// Two endpoints, both read-only and powered by the wide telemetry_hifreq
// table populated by ingestion/writer.go at ~10Hz. Bucketing by track_position
// happens at query time so callers can pick a resolution that matches how
// long the track is — tighter buckets for short circuits, wider for big laps.
//
//	GET /api/laps/traces  — bucketed channel arrays per lap
//	GET /api/laps/compare — per-corner brake-point + apex + exit-throttle
//	                        deltas vs the driver's best lap of this session
//
// Best-lap baseline is the driver's own best valid lap of the *current*
// session_uid. Opponent comparison is deferred (writer currently discards
// non-player car_telemetry channel data).

// ---------------------------------------------------------------------------
// /api/laps/traces — bucketed channel traces for one or more laps
// ---------------------------------------------------------------------------

// hifreqChannel maps a public channel id (from ?channels=) to the SQL column
// + JSON key used in the response. Aggregation is always AVG over the
// bucket; channels that are categorical (gear, drs) average to floats which
// is fine for the agent — it sees a smoothed shape, not a strict integer.
type hifreqChannel struct {
	id     string  // public id used in ?channels= and the response key
	column string  // SQL expression evaluated inside the bucket aggregate
	scale  float64 // optional output scale (e.g. brake stored 0..1, surface as 0..100)
}

var allowedTraceChannels = map[string]hifreqChannel{
	"throttle": {id: "throttle", column: "throttle", scale: 1},
	"brake":    {id: "brake", column: "brake", scale: 1},
	"speed":    {id: "speed", column: "speed", scale: 1},
	"gear":     {id: "gear", column: "gear", scale: 1},
	"steering": {id: "steering", column: "steering", scale: 1},
	"rpm":      {id: "rpm", column: "engine_rpm", scale: 1},
	"clutch":   {id: "clutch", column: "clutch", scale: 1},
	"drs":      {id: "drs", column: "drs", scale: 1},
	"g_lat":    {id: "g_lat", column: "g_force_lat", scale: 1},
	"g_lon":    {id: "g_lon", column: "g_force_lon", scale: 1},
	"g_vert":   {id: "g_vert", column: "g_force_vert", scale: 1},
	// 4-wheel averages keep the response shape stable for the AI tools.
	"brake_temp":      {id: "brake_temp", column: "(brake_temp_fl+brake_temp_fr+brake_temp_rl+brake_temp_rr)/4.0", scale: 1},
	"tyre_temp":       {id: "tyre_temp", column: "(tyre_surf_temp_fl+tyre_surf_temp_fr+tyre_surf_temp_rl+tyre_surf_temp_rr)/4.0", scale: 1},
	"tyre_inner_temp": {id: "tyre_inner_temp", column: "(tyre_inner_temp_fl+tyre_inner_temp_fr+tyre_inner_temp_rl+tyre_inner_temp_rr)/4.0", scale: 1},
	"tyre_pressure":   {id: "tyre_pressure", column: "(tyre_pressure_fl+tyre_pressure_fr+tyre_pressure_rl+tyre_pressure_rr)/4.0", scale: 1},
	// Per-wheel breakouts for asymmetry diagnosis (e.g. left-front locking).
	"brake_temp_fl": {id: "brake_temp_fl", column: "brake_temp_fl", scale: 1},
	"brake_temp_fr": {id: "brake_temp_fr", column: "brake_temp_fr", scale: 1},
	"brake_temp_rl": {id: "brake_temp_rl", column: "brake_temp_rl", scale: 1},
	"brake_temp_rr": {id: "brake_temp_rr", column: "brake_temp_rr", scale: 1},
	"tyre_temp_fl":  {id: "tyre_temp_fl", column: "tyre_surf_temp_fl", scale: 1},
	"tyre_temp_fr":  {id: "tyre_temp_fr", column: "tyre_surf_temp_fr", scale: 1},
	"tyre_temp_rl":  {id: "tyre_temp_rl", column: "tyre_surf_temp_rl", scale: 1},
	"tyre_temp_rr":  {id: "tyre_temp_rr", column: "tyre_surf_temp_rr", scale: 1},
	"tyre_press_fl": {id: "tyre_press_fl", column: "tyre_pressure_fl", scale: 1},
	"tyre_press_fr": {id: "tyre_press_fr", column: "tyre_pressure_fr", scale: 1},
	"tyre_press_rl": {id: "tyre_press_rl", column: "tyre_pressure_rl", scale: 1},
	"tyre_press_rr": {id: "tyre_press_rr", column: "tyre_pressure_rr", scale: 1},
	// Energy + fuel — pace-vs-energy analysis at distance resolution.
	"fuel":             {id: "fuel", column: "fuel_in_tank", scale: 1},
	"ers_store":        {id: "ers_store", column: "ers_store_energy", scale: 1},
	"ers_deploy_mode":  {id: "ers_deploy_mode", column: "ers_deploy_mode", scale: 1},
	// Sliding diagnostics — wheel slip ratio (longitudinal slip; spin/lock)
	// and slip angle (lateral slip; understeer/oversteer). 4-wheel averages
	// for the headline shape; per-wheel breakouts for asymmetry analysis
	// (e.g. inside-rear spin under power). Stored as DOUBLE; positive slip
	// ratio = drive wheel spinning up, negative = locking under braking.
	"slip_ratio":       {id: "slip_ratio", column: "(wheel_slip_ratio_fl+wheel_slip_ratio_fr+wheel_slip_ratio_rl+wheel_slip_ratio_rr)/4.0", scale: 1},
	"slip_angle":       {id: "slip_angle", column: "(wheel_slip_angle_fl+wheel_slip_angle_fr+wheel_slip_angle_rl+wheel_slip_angle_rr)/4.0", scale: 1},
	"slip_ratio_fl":    {id: "slip_ratio_fl", column: "wheel_slip_ratio_fl", scale: 1},
	"slip_ratio_fr":    {id: "slip_ratio_fr", column: "wheel_slip_ratio_fr", scale: 1},
	"slip_ratio_rl":    {id: "slip_ratio_rl", column: "wheel_slip_ratio_rl", scale: 1},
	"slip_ratio_rr":    {id: "slip_ratio_rr", column: "wheel_slip_ratio_rr", scale: 1},
	"slip_angle_fl":    {id: "slip_angle_fl", column: "wheel_slip_angle_fl", scale: 1},
	"slip_angle_fr":    {id: "slip_angle_fr", column: "wheel_slip_angle_fr", scale: 1},
	"slip_angle_rl":    {id: "slip_angle_rl", column: "wheel_slip_angle_rl", scale: 1},
	"slip_angle_rr":    {id: "slip_angle_rr", column: "wheel_slip_angle_rr", scale: 1},
	// Surface enum per wheel (0=tarmac, 1=rumblestrip, 4=gravel, 7=grass, …).
	// MAX picks the worst surface any wheel was on across the bucket so the
	// trace flags off-track moments even if one wheel stayed on tarmac.
	"surface":    {id: "surface", column: "GREATEST(surface_type_fl, surface_type_fr, surface_type_rl, surface_type_rr)", scale: 1},
	"surface_fl": {id: "surface_fl", column: "surface_type_fl", scale: 1},
	"surface_fr": {id: "surface_fr", column: "surface_type_fr", scale: 1},
	"surface_rl": {id: "surface_rl", column: "surface_type_rl", scale: 1},
	"surface_rr": {id: "surface_rr", column: "surface_type_rr", scale: 1},
}

type traceLap struct {
	Lap       int                     `json:"lap"`
	// SessionUID is set when the lap was fetched cross-session. Empty for
	// laps that belong to the resolving session (back-compat with dashboards
	// that key on Lap alone).
	SessionUID string `json:"session_uid,omitempty"`
	LapLabel   string `json:"lap_label,omitempty"`
	LapTimeMs  int    `json:"lap_time_ms,omitempty"`
	// Buckets is the lap_distance midpoint of each output slice in metres.
	Buckets []float64 `json:"track_position_bucket_m"`
	// Channels are pointer slices so empty buckets (no hifreq sample fell in
	// the slice this lap) serialise as JSON null, not NaN — Go's
	// encoding/json refuses NaN, and null is what plotting / agent code
	// already expects for "missing".
	Channels   map[string][]*float64 `json:"channels"`
	SampleCnts []int                 `json:"sample_counts,omitempty"`
}

type tracesResponse struct {
	SessionUID    string     `json:"session_uid"`
	TrackID       int        `json:"track_id"`
	TrackLengthM  float64    `json:"track_length_m"`
	Buckets       int        `json:"buckets"`
	BucketSizeM   float64    `json:"bucket_size_m"`
	Laps          []traceLap `json:"laps"`
}

// lapTracesHandler powers GET /api/laps/traces. See package doc-comment for
// the full param contract; defaults are best+current, throttle/brake/speed,
// 80 buckets. When ?session_uid= is supplied, the handler scopes against the
// historical session instead of the live cache, enabling Deep Insights to
// reuse this endpoint for completed sessions.
func lapTracesHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		ctx := newLapCtx(c, deps)
		state, err := ctx.resolve()
		if err != nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": err.Error(),
			})
		}
		trackLen := float64(state.trackLength)

		buckets := parseIntDefault(c.Query("buckets"), 80)
		if buckets < 20 {
			buckets = 20
		}
		if buckets > 200 {
			buckets = 200
		}
		bucketSize := trackLen / float64(buckets)

		channels := parseChannels(c.Query("channels"))
		if len(channels) == 0 {
			channels = []hifreqChannel{
				allowedTraceChannels["throttle"],
				allowedTraceChannels["brake"],
				allowedTraceChannels["speed"],
			}
		}

		// Resolve the requested laps (numeric, "best", "current", "last", "recent:N").
		lapsParam := c.Query("laps", "best,current")
		resolved, err := resolveLaps(c.Context(), deps, state.sessionUID, state.playerCarIndex, lapsParam)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error": err.Error(),
			})
		}
		if len(resolved) == 0 {
			return c.JSON(tracesResponse{
				SessionUID:   uidString(state.sessionUID),
				TrackID:      state.trackID,
				TrackLengthM: trackLen,
				Buckets:      buckets,
				BucketSizeM:  bucketSize,
				Laps:         []traceLap{},
			})
		}

		// Build SELECT list dynamically: one AVG per requested channel.
		var selectExprs []string
		for _, ch := range channels {
			selectExprs = append(selectExprs, fmt.Sprintf("AVG(%s) AS %s", ch.column, ch.id))
		}
		// Build the WHERE filter: each (session_uid, lap) pair becomes one
		// OR clause. Lap numbers and uids are small / well-defined ints so
		// direct interpolation is safe.
		type lapKey struct {
			uid uint64
			lap int
		}
		keySet := make(map[lapKey]struct{}, len(resolved))
		var orClauses []string
		for _, r := range resolved {
			k := lapKey{uid: r.sessionUID, lap: r.lap}
			if _, ok := keySet[k]; ok {
				continue
			}
			keySet[k] = struct{}{}
			orClauses = append(orClauses, fmt.Sprintf("(session_uid = %s AND lap = %d)", uidString(r.sessionUID), r.lap))
		}
		sql := fmt.Sprintf(`
SELECT session_uid,
       lap,
       FLOOR(track_position / %f)::INT AS bucket,
       %s,
       COUNT(*) AS n
FROM telemetry_hifreq
WHERE (%s)
  AND pit_status = 0
  AND track_position IS NOT NULL
  AND track_position >= 0
GROUP BY session_uid, lap, bucket
ORDER BY session_uid, lap, bucket`, bucketSize, strings.Join(selectExprs, ", "), strings.Join(orClauses, " OR "))

		rows, err := deps.Store.Query(c.Context(), sql)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": err.Error(),
			})
		}

		// Pre-allocate one trace per unique (session, lap) pair.
		traces := make(map[lapKey]*traceLap, len(resolved))
		for _, r := range resolved {
			k := lapKey{uid: r.sessionUID, lap: r.lap}
			if _, ok := traces[k]; ok {
				continue
			}
			tl := &traceLap{
				Lap:        r.lap,
				LapLabel:   r.label,
				LapTimeMs:  r.lapTimeMs,
				Buckets:    make([]float64, buckets),
				Channels:   make(map[string][]*float64, len(channels)),
				SampleCnts: make([]int, buckets),
			}
			// Only stamp SessionUID for cross-session laps — back-compat
			// with dashboards that rely on Lap as the unique key.
			if r.sessionUID != state.sessionUID {
				tl.SessionUID = uidString(r.sessionUID)
			}
			for i := 0; i < buckets; i++ {
				tl.Buckets[i] = (float64(i) + 0.5) * bucketSize
			}
			for _, ch := range channels {
				// Leave each bucket as nil — populated below only for
				// buckets that actually had samples.
				tl.Channels[ch.id] = make([]*float64, buckets)
			}
			traces[k] = tl
		}

		for _, row := range rows {
			lap := toInt(row["lap"])
			uid := toUint64(row["session_uid"])
			bucket := toInt(row["bucket"])
			if bucket < 0 || bucket >= buckets {
				continue
			}
			tl, ok := traces[lapKey{uid: uid, lap: lap}]
			if !ok {
				continue
			}
			tl.SampleCnts[bucket] = toInt(row["n"])
			for _, ch := range channels {
				if row[ch.id] == nil {
					continue
				}
				v := toFloat(row[ch.id]) * ch.scale
				tl.Channels[ch.id][bucket] = &v
			}
		}

		// Build response in the order the caller asked for, deduped on
		// (session, lap).
		seen := make(map[lapKey]struct{}, len(resolved))
		out := make([]traceLap, 0, len(resolved))
		for _, r := range resolved {
			k := lapKey{uid: r.sessionUID, lap: r.lap}
			if _, ok := seen[k]; ok {
				continue
			}
			seen[k] = struct{}{}
			tl := traces[k]
			if tl == nil {
				continue
			}
			out = append(out, *tl)
		}

		return c.JSON(tracesResponse{
			SessionUID:   uidString(state.sessionUID),
			TrackID:      state.trackID,
			TrackLengthM: trackLen,
			Buckets:      buckets,
			BucketSizeM:  bucketSize,
			Laps:         out,
		})
	}
}

// ---------------------------------------------------------------------------
// /api/laps/compare — per-corner brake-point + apex + exit-throttle deltas
// ---------------------------------------------------------------------------

type cornerCompare struct {
	ID                   string   `json:"id"`
	Name                 string   `json:"name"`
	LapDistanceM         float64  `json:"lap_distance_m"`
	BestBrakePointM      *float64 `json:"best_brake_point_m,omitempty"`
	YourBrakePointM      *float64 `json:"your_brake_point_m,omitempty"`
	DeltaBrakePointM     *float64 `json:"delta_brake_point_m,omitempty"`
	BestApexSpeedKmh     *float64 `json:"best_apex_speed_kmh,omitempty"`
	YourApexSpeedKmh     *float64 `json:"your_apex_speed_kmh,omitempty"`
	DeltaApexSpeedKmh    *float64 `json:"delta_apex_speed_kmh,omitempty"`
	BestExitThrottle     *float64 `json:"best_exit_throttle,omitempty"`
	YourExitThrottle     *float64 `json:"your_exit_throttle,omitempty"`
	DeltaExitThrottle    *float64 `json:"delta_exit_throttle,omitempty"`
	Note                 string   `json:"note,omitempty"`
}

type compareResponse struct {
	SessionUID    string          `json:"session_uid"`
	TrackID       int             `json:"track_id"`
	TrackLengthM  float64         `json:"track_length_m"`
	Lap           int             `json:"lap"`
	LapTimeMs     int             `json:"lap_time_ms,omitempty"`
	BestLap       int             `json:"best_lap"`
	BestLapTimeMs int             `json:"best_lap_time_ms"`
	// BestLapSessionUID is set only when the baseline came from a different
	// session via ?reference_session_uid=. Empty for same-session compares
	// (back-compat with dashboards that read the flat shape).
	BestLapSessionUID string          `json:"best_lap_session_uid,omitempty"`
	DeltaTotalMs      int             `json:"delta_total_ms"`
	WindowBeforeM     float64         `json:"window_before_m"`
	WindowAfterM      float64         `json:"window_after_m"`
	Corners           []cornerCompare `json:"corners"`
	Note              string          `json:"note,omitempty"`
}

// brakePointMinSamples is how many consecutive samples must show brake
// pressure above the threshold before we accept the location as the brake
// point. Filters out single-sample bumps / curb hits / phantom braps.
const brakePointMinSamples = 2
const brakePointThreshold = 0.10

// lapCompareHandler powers GET /api/laps/compare.
// Accepts ?session_uid= for historical analysis (Deep Insights). When absent
// the live cache state is used (Race Day live coaching).
// Optional ?reference_session_uid= + ?reference_lap= pull the best-lap
// baseline from a different (same-track) session — enables "compare today's
// lap to my best ever at this track".
func lapCompareHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		ctx := newLapCtx(c, deps)
		state, err := ctx.resolve()
		if err != nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": err.Error(),
			})
		}
		trackLen := float64(state.trackLength)
		uid := state.sessionUID
		carIdx := state.playerCarIndex

		// Optional cross-session baseline. Same track-length guard as
		// /api/laps/delta — the brake-point and apex windows assume the
		// corners line up at the same lap_distance.
		refUID, refCar, err := resolveCrossSession(c, deps, "reference_session_uid", uid, carIdx, trackLen)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
		}

		// Resolve the lap to compare. Default = most recent completed lap.
		lapStr := c.Query("lap")
		var your int
		if lapStr == "" {
			n, err := mostRecentCompletedLap(c.Context(), deps, uid, carIdx)
			if err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
					"error": err.Error(),
				})
			}
			if n == 0 {
				return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
					"error": "no completed laps in this session yet",
				})
			}
			your = n
		} else {
			n, err := strconv.Atoi(lapStr)
			if err != nil || n <= 0 {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
					"error": "lap must be a positive integer",
				})
			}
			your = n
		}

		// Resolve baseline lap. Default = best valid lap in the (possibly
		// overridden) reference session. Explicit ?reference_lap=N picks a
		// specific lap so the caller can stack arbitrary historical laps.
		var (
			best   int
			bestMs int
		)
		if refLapStr := strings.TrimSpace(c.Query("reference_lap")); refLapStr != "" {
			n, perr := strconv.Atoi(refLapStr)
			if perr != nil || n <= 0 {
				return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
					"error": "reference_lap must be a positive integer",
				})
			}
			best = n
			bestMs, _ = lapTimeFromHistory(c.Context(), deps, refUID, refCar, best)
		} else {
			best, bestMs, err = bestValidLap(c.Context(), deps, refUID, refCar)
			if err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
					"error": err.Error(),
				})
			}
		}
		if best == 0 {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
				"error": "no valid completed lap to use as best-lap baseline",
			})
		}

		yourMs, _ := lapTimeFromHistory(c.Context(), deps, uid, carIdx, your)

		// Resolve corners. Allow filtering by ?corner=T3.
		corners := lookupCorners(deps.TrackMap, int8(state.trackID))
		if cornerFilter := strings.TrimSpace(c.Query("corner")); cornerFilter != "" {
			corners = filterCorners(corners, cornerFilter)
		}

		windowBefore := parseFloatDefault(c.Query("window_before_m"), 200)
		windowAfter := parseFloatDefault(c.Query("window_after_m"), 50)

		// Pre-fetch hifreq samples for both laps. One query each — small
		// volume (≈10Hz × 90s × 1 channel ≈ 900 rows worst case). The
		// baseline lap may live in a different session when the caller set
		// reference_session_uid, so honour that here.
		yourSamples, err := loadLapSamples(c.Context(), deps, uid, your)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": err.Error(),
			})
		}
		bestSamples, err := loadLapSamples(c.Context(), deps, refUID, best)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": err.Error(),
			})
		}

		// Per-corner deltas.
		out := make([]cornerCompare, 0, len(corners))
		for _, corner := range corners {
			cc := cornerCompare{
				ID:           corner.ID,
				Name:         corner.Name,
				LapDistanceM: float64(corner.LapDistanceM),
			}
			yBP := findBrakePoint(yourSamples, float64(corner.LapDistanceM), windowBefore, windowAfter, trackLen)
			bBP := findBrakePoint(bestSamples, float64(corner.LapDistanceM), windowBefore, windowAfter, trackLen)
			yA := findApex(yourSamples, float64(corner.LapDistanceM), trackLen)
			bA := findApex(bestSamples, float64(corner.LapDistanceM), trackLen)
			if yBP != nil {
				cc.YourBrakePointM = floatPtr(*yBP)
			}
			if bBP != nil {
				cc.BestBrakePointM = floatPtr(*bBP)
			}
			if yBP != nil && bBP != nil {
				cc.DeltaBrakePointM = floatPtr(*yBP - *bBP)
			}
			if yA != nil {
				cc.YourApexSpeedKmh = floatPtr(yA.speed)
				cc.YourExitThrottle = floatPtr(yA.throttleAtApex)
			}
			if bA != nil {
				cc.BestApexSpeedKmh = floatPtr(bA.speed)
				cc.BestExitThrottle = floatPtr(bA.throttleAtApex)
			}
			if yA != nil && bA != nil {
				cc.DeltaApexSpeedKmh = floatPtr(yA.speed - bA.speed)
				cc.DeltaExitThrottle = floatPtr(yA.throttleAtApex - bA.throttleAtApex)
			}
			if yBP == nil && bBP == nil && yA == nil && bA == nil {
				cc.Note = "no samples in window — likely insufficient hi-freq data"
			}
			out = append(out, cc)
		}

		resp := compareResponse{
			SessionUID:    uidString(uid),
			TrackID:       state.trackID,
			TrackLengthM:  trackLen,
			Lap:           your,
			LapTimeMs:     yourMs,
			BestLap:       best,
			BestLapTimeMs: bestMs,
			DeltaTotalMs:  yourMs - bestMs,
			WindowBeforeM: windowBefore,
			WindowAfterM:  windowAfter,
			Corners:       out,
		}
		if refUID != uid {
			resp.BestLapSessionUID = uidString(refUID)
		}
		if len(corners) == 0 {
			resp.Note = "no curated corners for this track id — supply ?corner= or curate workspace/tracks/<id>.json"
		}
		return c.JSON(resp)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// hifreqSample is a slimmed-down row used by the compare tool's brake-point
// + apex computation. Fields beyond this set are not needed.
type hifreqSample struct {
	trackPos float64
	totalDist float64
	speed    float64
	throttle float64
	brake    float64
}

// loadLapSamples returns all hi-freq samples for a single (session, lap),
// ordered by total_distance so consecutive-sample logic (brake-point edge
// detection) is correct across the start/finish line wrap.
func loadLapSamples(ctx context.Context, deps *Deps, uid uint64, lap int) ([]hifreqSample, error) {
	sql := fmt.Sprintf(`SELECT track_position, total_distance, speed, throttle, brake
FROM telemetry_hifreq
WHERE session_uid = %s AND lap = %d AND pit_status = 0
ORDER BY total_distance`, uidString(uid), lap)
	rows, err := deps.Store.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	out := make([]hifreqSample, 0, len(rows))
	for _, r := range rows {
		out = append(out, hifreqSample{
			trackPos:  toFloat(r["track_position"]),
			totalDist: toFloat(r["total_distance"]),
			speed:     toFloat(r["speed"]),
			throttle:  toFloat(r["throttle"]),
			brake:     toFloat(r["brake"]),
		})
	}
	return out, nil
}

// findBrakePoint locates the smallest track_position in the window
// [cornerDist - before, cornerDist + after] where brake exceeds the
// threshold for at least brakePointMinSamples consecutive samples. Returns
// nil if no qualifying point exists. lapWrap-aware via the in-window check.
func findBrakePoint(samples []hifreqSample, cornerDist, before, after, lapLen float64) *float64 {
	if len(samples) == 0 {
		return nil
	}
	// Define the window as [start, end] in lap_distance terms.
	start := cornerDist - before
	end := cornerDist + after
	consec := 0
	var firstAbove float64
	hit := false
	for _, s := range samples {
		// Window check has to handle a window that wraps the S/F line. For
		// typical tracks the window is well inside the lap so the simple
		// inclusive check is enough. The lap-rollover case (corner near S/F)
		// is handled by also accepting samples from the previous lap into
		// the window via the lapWrap shift below.
		pos := s.trackPos
		if start < 0 {
			// window crosses S/F — accept samples in [0, end] OR [lapLen+start, lapLen]
			if !((pos >= 0 && pos <= end) || (pos >= lapLen+start && pos <= lapLen)) {
				consec = 0
				continue
			}
		} else if end > lapLen {
			if !((pos >= start && pos <= lapLen) || (pos >= 0 && pos <= end-lapLen)) {
				consec = 0
				continue
			}
		} else if pos < start || pos > end {
			consec = 0
			continue
		}
		if s.brake > brakePointThreshold {
			if consec == 0 {
				firstAbove = pos
			}
			consec++
			if consec >= brakePointMinSamples && !hit {
				hit = true
				v := firstAbove
				return &v
			}
		} else {
			consec = 0
		}
	}
	return nil
}

// apexInfo summarises the slow point of a corner.
type apexInfo struct {
	trackPos       float64
	speed          float64
	throttleAtApex float64
}

// findApex returns the sample with the smallest speed in
// [cornerDist - 50, cornerDist + 100], plus the throttle reading at that
// sample. Returns nil if no samples fall in the window.
func findApex(samples []hifreqSample, cornerDist, lapLen float64) *apexInfo {
	if len(samples) == 0 {
		return nil
	}
	start := cornerDist - 50
	end := cornerDist + 100
	var best *apexInfo
	for _, s := range samples {
		pos := s.trackPos
		var inWindow bool
		switch {
		case start < 0:
			inWindow = (pos >= 0 && pos <= end) || (pos >= lapLen+start && pos <= lapLen)
		case end > lapLen:
			inWindow = (pos >= start && pos <= lapLen) || (pos >= 0 && pos <= end-lapLen)
		default:
			inWindow = pos >= start && pos <= end
		}
		if !inWindow {
			continue
		}
		if best == nil || s.speed < best.speed {
			best = &apexInfo{trackPos: pos, speed: s.speed, throttleAtApex: s.throttle}
		}
	}
	return best
}

// resolvedLap is one entry in the laps= request, expanded to a concrete lap
// number plus any human-friendly metadata for the response.
//
// sessionUID identifies which session the lap belongs to. Defaults to the
// resolving session (lapCtx.sessionUID), but each ?laps= token can pin a
// different session via the `@<uid>` suffix — that's how the dashboard
// stacks laps from past sessions onto the current lap-trace overlay.
// carIdx is resolved per session at parse time so cross-session lookups
// don't accidentally pull the wrong driver's history.
type resolvedLap struct {
	lap        int
	sessionUID uint64
	carIdx     int
	label      string
	lapTimeMs  int
}

// resolveLaps expands the laps= query parameter into concrete lap numbers.
// Supported tokens: integers, "best", "current", "last", "recent:N".
// Each token may be suffixed with `@<session_uid>` to pull the lap from a
// different session — used by the cross-session compare picker. "current"
// only resolves when the requested session is the live one — for
// historical session_uids it falls through silently.
func resolveLaps(ctx context.Context, deps *Deps, defaultUID uint64, defaultCarIdx int, raw string) ([]resolvedLap, error) {
	tokens := strings.Split(raw, ",")
	out := make([]resolvedLap, 0, len(tokens))
	currentLap := 0
	if state := deps.Store.Cache().Load(); state != nil && state.SessionUID == defaultUID {
		currentLap = int(state.CurrentLap)
	}
	// Cache the player car index per session_uid resolved by `@uid` suffixes.
	// playerIndexBySession is one query; calling it once per unique uid keeps
	// the parse cost predictable when the picker stacks several sessions.
	playerCache := map[uint64]int{defaultUID: defaultCarIdx}
	lookupCarIdx := func(uid uint64) (int, error) {
		if v, ok := playerCache[uid]; ok {
			return v, nil
		}
		players, err := playerIndexBySession(ctx, deps.Store.Reader())
		if err != nil {
			return 0, err
		}
		idx, ok := players[uid]
		if !ok {
			return 0, fmt.Errorf("session not found: %d", uid)
		}
		playerCache[uid] = idx
		return idx, nil
	}
	for _, tok := range tokens {
		raw := strings.TrimSpace(tok)
		if raw == "" {
			continue
		}
		// Split optional `@<uid>` suffix.
		uid := defaultUID
		carIdx := defaultCarIdx
		body := raw
		if at := strings.IndexByte(raw, '@'); at >= 0 {
			body = strings.TrimSpace(raw[:at])
			uidStr := strings.TrimSpace(raw[at+1:])
			parsed, err := strconv.ParseUint(uidStr, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid session_uid in token %q: %w", tok, err)
			}
			uid = parsed
			idx, err := lookupCarIdx(uid)
			if err != nil {
				return nil, err
			}
			carIdx = idx
		}
		t := strings.ToLower(body)
		switch {
		case t == "best":
			b, ms, err := bestValidLap(ctx, deps, uid, carIdx)
			if err != nil {
				return nil, err
			}
			if b > 0 {
				out = append(out, resolvedLap{lap: b, sessionUID: uid, carIdx: carIdx, label: "best", lapTimeMs: ms})
			}
		case t == "current":
			// "current" only meaningful in the live session.
			if uid == defaultUID && currentLap > 0 {
				out = append(out, resolvedLap{lap: currentLap, sessionUID: uid, carIdx: carIdx, label: "current"})
			}
		case t == "last":
			n, err := mostRecentCompletedLap(ctx, deps, uid, carIdx)
			if err != nil {
				return nil, err
			}
			if n > 0 {
				ms, _ := lapTimeFromHistory(ctx, deps, uid, carIdx, n)
				out = append(out, resolvedLap{lap: n, sessionUID: uid, carIdx: carIdx, label: "last", lapTimeMs: ms})
			}
		case strings.HasPrefix(t, "recent:"):
			nStr := strings.TrimPrefix(t, "recent:")
			n, err := strconv.Atoi(nStr)
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("invalid recent count: %q", nStr)
			}
			recents, err := recentCompletedLaps(ctx, deps, uid, carIdx, n)
			if err != nil {
				return nil, err
			}
			for _, l := range recents {
				l.sessionUID = uid
				l.carIdx = carIdx
				out = append(out, l)
			}
		default:
			n, err := strconv.Atoi(t)
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("invalid lap token: %q", tok)
			}
			ms, _ := lapTimeFromHistory(ctx, deps, uid, carIdx, n)
			out = append(out, resolvedLap{lap: n, sessionUID: uid, carIdx: carIdx, lapTimeMs: ms})
		}
	}
	return out, nil
}

// bestValidLap returns the lap_num + lap_time_in_ms of the driver's best
// valid completed lap in this session. Returns (0, 0, nil) if none yet.
func bestValidLap(ctx context.Context, deps *Deps, uid uint64, carIdx int) (int, int, error) {
	sql := fmt.Sprintf(`SELECT lap_num, lap_time_in_ms FROM session_history
WHERE session_uid = %s AND car_index = %d AND lap_valid = 1 AND lap_time_in_ms > 0
ORDER BY lap_time_in_ms ASC LIMIT 1`, uidString(uid), carIdx)
	rows, err := deps.Store.Query(ctx, sql)
	if err != nil {
		return 0, 0, err
	}
	if len(rows) == 0 {
		return 0, 0, nil
	}
	return toInt(rows[0]["lap_num"]), toInt(rows[0]["lap_time_in_ms"]), nil
}

// mostRecentCompletedLap returns the highest lap_num with lap_time_in_ms > 0
// in session_history. Returns 0 if none.
func mostRecentCompletedLap(ctx context.Context, deps *Deps, uid uint64, carIdx int) (int, error) {
	sql := fmt.Sprintf(`SELECT MAX(lap_num) AS lap FROM session_history
WHERE session_uid = %s AND car_index = %d AND lap_time_in_ms > 0`, uidString(uid), carIdx)
	rows, err := deps.Store.Query(ctx, sql)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	return toInt(rows[0]["lap"]), nil
}

// recentCompletedLaps returns the most recent N completed laps in
// descending lap order. Each entry includes lap_time_in_ms when known.
func recentCompletedLaps(ctx context.Context, deps *Deps, uid uint64, carIdx, n int) ([]resolvedLap, error) {
	sql := fmt.Sprintf(`SELECT lap_num, lap_time_in_ms FROM session_history
WHERE session_uid = %s AND car_index = %d AND lap_time_in_ms > 0
ORDER BY lap_num DESC LIMIT %d`, uidString(uid), carIdx, n)
	rows, err := deps.Store.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	out := make([]resolvedLap, 0, len(rows))
	for _, r := range rows {
		out = append(out, resolvedLap{
			lap:       toInt(r["lap_num"]),
			label:     "recent",
			lapTimeMs: toInt(r["lap_time_in_ms"]),
		})
	}
	return out, nil
}

func lapTimeFromHistory(ctx context.Context, deps *Deps, uid uint64, carIdx, lap int) (int, error) {
	sql := fmt.Sprintf(`SELECT lap_time_in_ms FROM session_history
WHERE session_uid = %s AND car_index = %d AND lap_num = %d`, uidString(uid), carIdx, lap)
	rows, err := deps.Store.Query(ctx, sql)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	return toInt(rows[0]["lap_time_in_ms"]), nil
}

// uidString renders a uint64 session UID as DuckDB UBIGINT-safe SQL literal.
// uint64 high bit cannot be passed via ? binding; embedding the digits
// avoids the big.Int dance for read-only handler queries.
func uidString(uid uint64) string {
	v := new(big.Int).SetUint64(uid)
	return v.String()
}

func parseChannels(raw string) []hifreqChannel {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	tokens := strings.Split(raw, ",")
	seen := make(map[string]struct{}, len(tokens))
	out := make([]hifreqChannel, 0, len(tokens))
	for _, t := range tokens {
		k := strings.ToLower(strings.TrimSpace(t))
		if k == "" {
			continue
		}
		ch, ok := allowedTraceChannels[k]
		if !ok {
			continue
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, ch)
	}
	return out
}

func parseIntDefault(s string, def int) int {
	if s == "" {
		return def
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

func parseFloatDefault(s string, def float64) float64 {
	if s == "" {
		return def
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return n
	}
	return def
}

func lookupCorners(reg *trackmap.Registry, trackID int8) []trackmap.Corner {
	if reg == nil {
		return nil
	}
	t := reg.Lookup(trackID)
	if t == nil {
		return nil
	}
	out := make([]trackmap.Corner, len(t.Corners))
	copy(out, t.Corners)
	sort.Slice(out, func(i, j int) bool {
		return out[i].LapDistanceM < out[j].LapDistanceM
	})
	return out
}

func filterCorners(corners []trackmap.Corner, filter string) []trackmap.Corner {
	want := strings.ToUpper(strings.TrimSpace(filter))
	out := make([]trackmap.Corner, 0, 1)
	for _, c := range corners {
		if strings.EqualFold(c.ID, want) || strings.EqualFold(c.Name, filter) {
			out = append(out, c)
		}
	}
	return out
}

func toInt(v interface{}) int {
	switch n := v.(type) {
	case int:
		return n
	case int32:
		return int(n)
	case int64:
		return int(n)
	case float32:
		return int(n)
	case float64:
		return int(n)
	case uint64:
		return int(n)
	case uint32:
		return int(n)
	case *big.Int:
		if n != nil {
			return int(n.Int64())
		}
	}
	return 0
}

func toUint64(v interface{}) uint64 {
	switch n := v.(type) {
	case uint64:
		return n
	case int64:
		if n >= 0 {
			return uint64(n)
		}
	case uint32:
		return uint64(n)
	case int32:
		if n >= 0 {
			return uint64(n)
		}
	case *big.Int:
		if n != nil {
			return n.Uint64()
		}
	}
	return 0
}

func toFloat(v interface{}) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int32:
		return float64(n)
	case int64:
		return float64(n)
	case uint32:
		return float64(n)
	case uint64:
		return float64(n)
	}
	return 0
}

func floatPtr(v float64) *float64 { return &v }

// _ silences unused-import warnings if storage symbols are accidentally
// trimmed during refactors; the import is load-bearing for compilation.
var _ = storage.TelemetryHifreqRow{}

// ---------------------------------------------------------------------------
// /api/laps/drs_usage — per-lap DRS utilization metrics
// ---------------------------------------------------------------------------

// DRSUsagePerLap is the response for /api/laps/drs_usage. Two callers:
//   - Live agent's get_drs_usage tool (when driver asks about DRS timing)
//   - pi-agent setup specialist (decides whether to push "open DRS earlier")
// available_s = total seconds DRS was permitted on this lap (in zone or post-
// activation point); active_s = total seconds DRS flap was actually open.
// utilization_pct = active_s / available_s × 100. first_open_distance_m is
// where the driver first opened the flap on the lap.
type DRSUsagePerLap struct {
	Lap                  int     `json:"lap"`
	LapTimeMs            int     `json:"lap_time_ms,omitempty"`
	LapLabel             string  `json:"lap_label,omitempty"`
	SamplesTotal         int     `json:"samples_total"`
	AllowedSamples       int     `json:"allowed_samples"`
	ActiveSamples        int     `json:"active_samples"`
	AvailableSec         float64 `json:"available_s"`
	ActiveSec            float64 `json:"active_s"`
	UtilizationPct       float64 `json:"utilization_pct"`
	FirstOpenDistanceM   float64 `json:"first_open_distance_m"`
	FirstAllowedDistanceM float64 `json:"first_allowed_distance_m"`
	OpenDelaySec         float64 `json:"open_delay_s"` // first_open - first_allowed (proxy for "late open")
}

// DRSUsageResponse is the wrapper for one or more laps' DRS analytics.
type DRSUsageResponse struct {
	SessionUID string           `json:"session_uid"`
	TrackID    int              `json:"track_id"`
	Laps       []DRSUsagePerLap `json:"laps"`
	Note       string           `json:"note,omitempty"`
}

// drsUsageSQL aggregates per-lap DRS metrics from telemetry_hifreq. The
// telemetry_hifreq stores `drs_allowed` (sourced from RaceState which mirrors
// CarStatus.DRSAllowed) and `drs` (raw CarTelemetry.DRS flag). We approximate
// per-lap seconds as (samples × lap_span / total_samples) which is robust to
// variable sampling cadence.
const drsUsageSQL = `
SELECT
  lap,
  COUNT(*)                                                       AS samples_total,
  SUM(CASE WHEN drs_allowed > 0 THEN 1 ELSE 0 END)               AS allowed_samples,
  SUM(CASE WHEN drs > 0 THEN 1 ELSE 0 END)                       AS active_samples,
  CAST(MAX(current_lap_time_ms) - MIN(current_lap_time_ms) AS BIGINT) AS lap_span_ms,
  MIN(CASE WHEN drs > 0 THEN track_position END)                 AS first_open_m,
  MIN(CASE WHEN drs_allowed > 0 THEN track_position END)         AS first_allowed_m
FROM telemetry_hifreq
WHERE session_uid = %s AND lap = %d
GROUP BY lap
`

func drsUsageHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		ctx := newLapCtx(c, deps)
		state, err := ctx.resolve()
		if err != nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": err.Error()})
		}
		uid := state.sessionUID
		carIdx := state.playerCarIndex

		lapsParam := strings.TrimSpace(c.Query("lap", c.Query("laps", "last")))
		if lapsParam == "" {
			lapsParam = "last"
		}
		resolved, err := resolveLaps(c.Context(), deps, uid, carIdx, lapsParam)
		if err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
		}

		out := DRSUsageResponse{
			SessionUID: uidString(uid),
			TrackID:    state.trackID,
			Laps:       make([]DRSUsagePerLap, 0, len(resolved)),
		}
		for _, r := range resolved {
			sql := fmt.Sprintf(drsUsageSQL, uidString(r.sessionUID), r.lap)
			rows, err := deps.Store.Query(c.Context(), sql)
			if err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
			}
			entry := DRSUsagePerLap{
				Lap:       r.lap,
				LapTimeMs: r.lapTimeMs,
				LapLabel:  r.label,
			}
			if len(rows) > 0 {
				row := rows[0]
				entry.SamplesTotal = toInt(row["samples_total"])
				entry.AllowedSamples = toInt(row["allowed_samples"])
				entry.ActiveSamples = toInt(row["active_samples"])
				lapSpanMs := toFloat(row["lap_span_ms"])
				if entry.SamplesTotal > 0 && lapSpanMs > 0 {
					perSample := lapSpanMs / float64(entry.SamplesTotal) / 1000.0
					entry.AvailableSec = round2(perSample * float64(entry.AllowedSamples))
					entry.ActiveSec = round2(perSample * float64(entry.ActiveSamples))
				}
				if entry.AvailableSec > 0 {
					entry.UtilizationPct = round1(entry.ActiveSec / entry.AvailableSec * 100.0)
				}
				if row["first_open_m"] != nil {
					entry.FirstOpenDistanceM = round1(toFloat(row["first_open_m"]))
				}
				if row["first_allowed_m"] != nil {
					entry.FirstAllowedDistanceM = round1(toFloat(row["first_allowed_m"]))
				}
				if entry.FirstOpenDistanceM > 0 && entry.FirstAllowedDistanceM > 0 {
					// Approximate "open delay" via distance / speed estimate
					// is fragile; report the raw distance gap so the caller
					// can reason about it. Seconds-equivalent skipped to
					// avoid claiming precision we don't have.
					entry.OpenDelaySec = round2((entry.FirstOpenDistanceM - entry.FirstAllowedDistanceM) / 60.0)
				}
			}
			out.Laps = append(out.Laps, entry)
		}
		if len(out.Laps) == 0 {
			out.Note = "no completed laps with telemetry_hifreq coverage yet"
		}
		return c.JSON(out)
	}
}
