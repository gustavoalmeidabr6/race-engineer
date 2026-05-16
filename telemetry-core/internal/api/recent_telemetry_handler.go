package api

import (
	"fmt"
	"math"

	"github.com/gofiber/fiber/v2"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
)

// recent_telemetry_handler.go answers "what has been happening on the car
// for the last N seconds?" — the rolling-window the dashboard's Live tab
// already renders, but packaged for AI consumption. Without this an LLM
// only sees the current instant snapshot (get_race_state) or completed-
// lap aggregates (get_lap_traces); both miss the *trend* — was the
// driver stabbing the brake, was steering shaking, is brake temp
// climbing in this stint.
//
//	GET /api/telemetry/recent
//	  ?seconds=60     (1..300, default 60)
//	  &buckets=30     (10..120, default 30; bucket_size = seconds/buckets)
//
// Returns:
//   - current: one-frame snapshot of the player car right now
//   - current_lap: in-progress lap info (lap, elapsed_ms, distance, pit_status)
//   - summary: peak / min / max / average per channel over the window
//   - timeline: bucketed time-series, one entry per bucket, oldest first
//
// This is the single most useful tool for in-the-moment coaching — peak
// g-forces, brake-event count, smoothness — without bloating the LLM
// context with hundreds of raw samples.

type recentSnapshot struct {
	SpeedKmh      int     `json:"speed_kmh"`
	Throttle      float64 `json:"throttle"`
	Brake         float64 `json:"brake"`
	Steering      float64 `json:"steering"`
	Gear          int     `json:"gear"`
	EngineRPM     int     `json:"engine_rpm"`
	DRS           int     `json:"drs"`
	GForceLat     float64 `json:"g_force_lat"`
	GForceLon     float64 `json:"g_force_lon"`
	BrakeTempAvgC float64 `json:"brake_temp_avg_c"`
	TyreTempAvgC  float64 `json:"tyre_surf_temp_avg_c"`
	TyrePressAvg  float64 `json:"tyre_pressure_avg_psi"`
	FuelKg        float64 `json:"fuel_kg"`
	ErsStorePct   float64 `json:"ers_store_pct"`
	ErsDeployMode int     `json:"ers_deploy_mode"`
}

type recentCurrentLap struct {
	Lap            int     `json:"lap"`
	ElapsedMs      int     `json:"elapsed_ms"`
	TrackPositionM float64 `json:"track_position_m"`
	PitStatus      int     `json:"pit_status"`
}

type recentSummary struct {
	MaxSpeedKmh         int     `json:"max_speed_kmh"`
	MinSpeedKmh         int     `json:"min_speed_kmh"`
	AvgSpeedKmh         float64 `json:"avg_speed_kmh"`
	PeakGLat            float64 `json:"peak_g_lat"`
	PeakGLonBrake       float64 `json:"peak_g_lon_braking"`
	PeakGLonAccel       float64 `json:"peak_g_lon_accel"`
	MaxBrake            float64 `json:"max_brake"`
	MaxThrottle         float64 `json:"max_throttle"`
	AvgThrottle         float64 `json:"avg_throttle"`
	BrakeEventCnt       int     `json:"brake_event_count"`
	BrakeTotalSec       float64 `json:"brake_total_seconds"`
	MaxBrakeTempC       int     `json:"max_brake_temp_c"`
	MaxTyreTempC        int     `json:"max_tyre_surf_temp_c"`
	DRSCurrentlyOpen    bool    `json:"drs_currently_open"`
	DRSCurrentlyAllowed bool    `json:"drs_currently_allowed"`
	DRSCurrentlyFault   bool    `json:"drs_currently_fault"`
	DRSOpenSec          float64 `json:"drs_open_seconds"`
	DRSOpenCount        int     `json:"drs_open_events"`
	SamplesUsed         int     `json:"samples_used"`
}

type drsEvent struct {
	TSecondsAgo    float64 `json:"t_seconds_ago"`
	Lap            int     `json:"lap"`
	TrackPositionM float64 `json:"track_position_m"`
	Kind           string  `json:"kind"` // "open" | "close"
}

type recentBucket struct {
	TSecondsAgo   float64 `json:"t_seconds_ago"`
	Throttle      float64 `json:"throttle"`
	Brake         float64 `json:"brake"`
	Steering      float64 `json:"steering"`
	SpeedKmh      float64 `json:"speed_kmh"`
	Gear          float64 `json:"gear"`
	EngineRPM     float64 `json:"rpm"`
	GForceLat     float64 `json:"g_lat"`
	GForceLon     float64 `json:"g_lon"`
	BrakeTempAvg  float64 `json:"brake_temp_avg_c"`
	TyreTempAvg   float64 `json:"tyre_surf_temp_avg_c"`
	DRSActiveFrac float64 `json:"drs_active_frac"`
	Lap           int     `json:"lap"`
	SamplesInBkt  int     `json:"samples"`
}

type recentResponse struct {
	SessionUID      string            `json:"session_uid"`
	LookbackSeconds int               `json:"lookback_seconds"`
	Buckets         int               `json:"buckets"`
	BucketSizeSec   float64           `json:"bucket_size_seconds"`
	Current         *recentSnapshot   `json:"current,omitempty"`
	CurrentLap      *recentCurrentLap `json:"current_lap,omitempty"`
	Summary         recentSummary     `json:"summary"`
	Timeline        []recentBucket    `json:"timeline"`
	DRSEvents       []drsEvent        `json:"drs_events,omitempty"`
	Note            string            `json:"note,omitempty"`
}

func recentTelemetryHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		// "Recent" only makes sense for the in-flight session; historical
		// lookups go through /api/laps/traces.
		state := deps.Store.Cache().Load()
		if state == nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"error": "no live telemetry yet",
			})
		}

		seconds := parseIntDefault(c.Query("seconds"), 60)
		if seconds < 1 {
			seconds = 1
		}
		if seconds > 300 {
			seconds = 300
		}
		buckets := parseIntDefault(c.Query("buckets"), 30)
		if buckets < 10 {
			buckets = 10
		}
		if buckets > 120 {
			buckets = 120
		}
		bucketSizeSec := float64(seconds) / float64(buckets)
		uid := state.SessionUID

		// Pull every hi-freq sample in the window, sorted oldest-first so
		// bucket 0 maps to "seconds ago" and bucket n-1 maps to "now". The
		// AI consumer can read the timeline left-to-right just like a chart.
		//
		// Cast now() → TIMESTAMP explicitly: DuckDB returns now() as
		// TIMESTAMPTZ but the `timestamp` column is TIMESTAMP (no zone),
		// and the binder refuses to subtract mixed types. Casting once
		// keeps the predicate and the EXTRACT both well-typed.
		sql := fmt.Sprintf(`
SELECT
  EXTRACT('epoch' FROM (CAST(now() AS TIMESTAMP) - timestamp))::DOUBLE AS ago_s,
  lap, track_position, drs,
  throttle, brake, steering, speed, gear, engine_rpm,
  g_force_lat, g_force_lon,
  (brake_temp_fl+brake_temp_fr+brake_temp_rl+brake_temp_rr)/4.0 AS brake_temp_avg,
  (tyre_surf_temp_fl+tyre_surf_temp_fr+tyre_surf_temp_rl+tyre_surf_temp_rr)/4.0 AS tyre_temp_avg
FROM telemetry_hifreq
WHERE session_uid = %s
  AND timestamp >= CAST(now() AS TIMESTAMP) - INTERVAL %d SECOND
ORDER BY timestamp ASC`, uidString(uid), seconds)

		rows, err := deps.Store.Query(c.Context(), sql)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}

		resp := recentResponse{
			SessionUID:      uidString(uid),
			LookbackSeconds: seconds,
			Buckets:         buckets,
			BucketSizeSec:   bucketSizeSec,
			Timeline:        make([]recentBucket, buckets),
			Current:         snapshotFromState(state),
			CurrentLap: &recentCurrentLap{
				Lap:            int(state.CurrentLap),
				ElapsedMs:      int(state.CurrentLapTimeMs),
				TrackPositionM: float64(state.LapDistance),
				PitStatus:      int(state.PitStatus),
			},
		}

		// Initialise bucket t-axis labels even when empty so the timeline
		// shape is stable for the consumer regardless of sample density.
		for i := 0; i < buckets; i++ {
			midAgo := float64(seconds) - (float64(i)+0.5)*bucketSizeSec
			if midAgo < 0 {
				midAgo = 0
			}
			resp.Timeline[i].TSecondsAgo = round2(midAgo)
		}

		if len(rows) == 0 {
			resp.Note = "no hi-freq samples in the requested window — driver may be in pits or HIFREQ_SAMPLE_RATE=0"
			return c.JSON(resp)
		}

		// Single-pass aggregation: bucket means + window-wide summary stats.
		type bktAcc struct {
			n         int
			throttle  float64
			brake     float64
			steering  float64
			speed     float64
			gear      float64
			rpm       float64
			gLat      float64
			gLon      float64
			brakeTemp float64
			tyreTemp  float64
			drsOn     int
			lastLap   int
		}
		accs := make([]bktAcc, buckets)

		var (
			maxSpeed                     = math.Inf(-1)
			minSpeed                     = math.Inf(1)
			peakGLat                     = 0.0
			peakGLonBrake, peakGLonAccel = 0.0, 0.0
			maxBrake, maxThrottle        = 0.0, 0.0
			throttleSum, speedSum        = 0.0, 0.0
			maxBrakeTemp, maxTyreTemp    = 0, 0
			brakeEventCount              = 0
			brakeSamples                 = 0
			inBrakeEvent                 = false
			drsOnSamples                 = 0
			drsOpenCount                 = 0
			drsEvents                    = make([]drsEvent, 0, 8)
			prevDRS                      = -1 // -1 sentinel: no previous sample yet
		)

		for _, r := range rows {
			ago := toFloat(r["ago_s"])
			bktIdx := int(math.Floor((float64(seconds) - ago) / bucketSizeSec))
			if bktIdx < 0 {
				bktIdx = 0
			}
			if bktIdx >= buckets {
				bktIdx = buckets - 1
			}

			thr := toFloat(r["throttle"])
			brk := toFloat(r["brake"])
			str := toFloat(r["steering"])
			spd := toFloat(r["speed"])
			gr := toFloat(r["gear"])
			rpm := toFloat(r["engine_rpm"])
			gLat := toFloat(r["g_force_lat"])
			gLon := toFloat(r["g_force_lon"])
			btemp := toFloat(r["brake_temp_avg"])
			ttemp := toFloat(r["tyre_temp_avg"])
			lap := toInt(r["lap"])
			drsVal := toInt(r["drs"])
			trackPos := toFloat(r["track_position"])

			a := &accs[bktIdx]
			a.n++
			a.throttle += thr
			a.brake += brk
			a.steering += str
			a.speed += spd
			a.gear += gr
			a.rpm += rpm
			a.gLat += gLat
			a.gLon += gLon
			a.brakeTemp += btemp
			a.tyreTemp += ttemp
			if drsVal > 0 {
				a.drsOn++
				drsOnSamples++
			}
			a.lastLap = lap

			// DRS edge detection: 0→>0 = open, >0→0 = close. Recorded with
			// lap + track position + seconds-ago so the agent can say
			// "DRS opened on lap 12, ~1.2km in, 14s ago".
			if prevDRS >= 0 {
				wasOn := prevDRS > 0
				nowOn := drsVal > 0
				if !wasOn && nowOn {
					drsOpenCount++
					drsEvents = append(drsEvents, drsEvent{
						TSecondsAgo:    round2(ago),
						Lap:            lap,
						TrackPositionM: round2(trackPos),
						Kind:           "open",
					})
				} else if wasOn && !nowOn {
					drsEvents = append(drsEvents, drsEvent{
						TSecondsAgo:    round2(ago),
						Lap:            lap,
						TrackPositionM: round2(trackPos),
						Kind:           "close",
					})
				}
			}
			prevDRS = drsVal

			if spd > maxSpeed {
				maxSpeed = spd
			}
			if spd < minSpeed {
				minSpeed = spd
			}
			speedSum += spd
			throttleSum += thr
			if thr > maxThrottle {
				maxThrottle = thr
			}
			if brk > maxBrake {
				maxBrake = brk
			}
			if math.Abs(gLat) > math.Abs(peakGLat) {
				peakGLat = gLat
			}
			// Long. G: positive = acceleration, negative = braking.
			if gLon < peakGLonBrake {
				peakGLonBrake = gLon
			}
			if gLon > peakGLonAccel {
				peakGLonAccel = gLon
			}
			if int(btemp) > maxBrakeTemp {
				maxBrakeTemp = int(btemp)
			}
			if int(ttemp) > maxTyreTemp {
				maxTyreTemp = int(ttemp)
			}

			// Brake event = contiguous run of brake > 0.10 (same threshold
			// as the corner brake-point detector). Counts let the AI say
			// "you braked 6 times in the last 30s" without parsing the
			// timeline manually.
			if brk > 0.1 {
				brakeSamples++
				if !inBrakeEvent {
					inBrakeEvent = true
					brakeEventCount++
				}
			} else {
				inBrakeEvent = false
			}
		}

		for i := 0; i < buckets; i++ {
			a := accs[i]
			b := &resp.Timeline[i]
			if a.n == 0 {
				continue
			}
			n := float64(a.n)
			b.Throttle = round2(a.throttle / n)
			b.Brake = round2(a.brake / n)
			b.Steering = round2(a.steering / n)
			b.SpeedKmh = round2(a.speed / n)
			b.Gear = round2(a.gear / n)
			b.EngineRPM = round2(a.rpm / n)
			b.GForceLat = round2(a.gLat / n)
			b.GForceLon = round2(a.gLon / n)
			b.BrakeTempAvg = round2(a.brakeTemp / n)
			b.TyreTempAvg = round2(a.tyreTemp / n)
			b.DRSActiveFrac = round2(float64(a.drsOn) / n)
			b.Lap = a.lastLap
			b.SamplesInBkt = a.n
		}

		totalSamples := len(rows)
		resp.Summary = recentSummary{
			MaxSpeedKmh:   int(maxSpeed),
			MinSpeedKmh:   int(minSpeed),
			AvgSpeedKmh:   round2(speedSum / float64(totalSamples)),
			PeakGLat:      round2(peakGLat),
			PeakGLonBrake: round2(peakGLonBrake),
			PeakGLonAccel: round2(peakGLonAccel),
			MaxBrake:      round2(maxBrake),
			MaxThrottle:   round2(maxThrottle),
			AvgThrottle:   round2(throttleSum / float64(totalSamples)),
			BrakeEventCnt: brakeEventCount,
			// Approximate: assumes ~uniform sample density across the window.
			BrakeTotalSec:       round2(float64(brakeSamples) * (float64(seconds) / float64(totalSamples))),
			MaxBrakeTempC:       maxBrakeTemp,
			MaxTyreTempC:        maxTyreTemp,
			DRSCurrentlyOpen:    state.DRS > 0,
			DRSCurrentlyAllowed: state.DRSAllowed > 0,
			DRSCurrentlyFault:   state.DRSFault > 0,
			DRSOpenSec:          round2(float64(drsOnSamples) * (float64(seconds) / float64(totalSamples))),
			DRSOpenCount:        drsOpenCount,
			SamplesUsed:         totalSamples,
		}

		// Cap to most recent 20 transitions so the payload stays small.
		if len(drsEvents) > 20 {
			drsEvents = drsEvents[len(drsEvents)-20:]
		}
		resp.DRSEvents = drsEvents

		return c.JSON(resp)
	}
}

// snapshotFromState packages the player-car fields of the cached
// RaceState into the recentSnapshot shape. 4-wheel arrays are averaged
// to single floats — the per-wheel detail lives on /api/laps/traces.
func snapshotFromState(s *models.RaceState) *recentSnapshot {
	if s == nil {
		return nil
	}
	avgU8 := func(a [4]uint8) float64 {
		return float64(int(a[0])+int(a[1])+int(a[2])+int(a[3])) / 4.0
	}
	avgU16 := func(a [4]uint16) float64 {
		return float64(int(a[0])+int(a[1])+int(a[2])+int(a[3])) / 4.0
	}
	avgF32 := func(a [4]float32) float64 {
		return float64(a[0]+a[1]+a[2]+a[3]) / 4.0
	}
	ersPct := 0.0
	if s.ERSStoreEnergy > 0 {
		ersPct = math.Min(100, float64(s.ERSStoreEnergy)/4_000_000.0*100)
	}
	return &recentSnapshot{
		SpeedKmh:      int(s.Speed),
		Throttle:      round2(float64(s.Throttle)),
		Brake:         round2(float64(s.Brake)),
		Steering:      round2(float64(s.Steering)),
		Gear:          int(s.Gear),
		EngineRPM:     int(s.EngineRPM),
		DRS:           int(s.DRS),
		GForceLat:     round2(float64(s.GForceLateral)),
		GForceLon:     round2(float64(s.GForceLongitude)),
		BrakeTempAvgC: round2(avgU16(s.BrakesTemp)),
		TyreTempAvgC:  round2(avgU8(s.TyresSurfTemp)),
		TyrePressAvg:  round2(avgF32(s.TyresPressure)),
		FuelKg:        round2(float64(s.FuelInTank)),
		ErsStorePct:   round2(ersPct),
		ErsDeployMode: int(s.ERSDeployMode),
	}
}
