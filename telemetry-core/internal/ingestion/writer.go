package ingestion

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/config"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/packets"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/storage"
)

// stateWriter is a goroutine that reads ParsedPackets, updates the atomic
// in-memory RaceState cache, and appends rows to the DuckDB batch buffers.
// It owns all writes to both the cache and the buffers — single-writer pattern.
func stateWriter(
	ctx context.Context,
	parsedChan <-chan models.ParsedPacket,
	store *storage.Storage,
	sampleRate int,
	hifreqRate int,
	cfg *config.Config,
	pttChan chan<- bool,
	eventHook EventHook,
) {
	// Running RaceState that we mutate and atomically swap.
	state := &models.RaceState{}

	// Sampling counters for high-frequency packets (20Hz → ~1Hz).
	sampleCounters := make(map[uint8]int)

	// Track previous BUTN state to detect PTT press/release transitions.
	var prevButtonStatus uint32
	var pttActive bool

	// Independent prev-state for dual-button (start/end) PTT — separate so
	// it doesn't fight with the single-button checkPTT path if both happen
	// to be enabled in some misconfiguration.
	var prevDualStatus uint32

	// Independent probe state so probe logs fire on every status change even
	// when PTT is disabled (PTT_BUTTON=0) or set to a different mask.
	var probePrevStatus uint32
	var probeFirst = true

	// PTT-trigger state for the MFDPanelIndex source. Initialised lazily on
	// the first car_telemetry packet so we don't fire a spurious toggle the
	// moment the listener comes up.
	var prevMFDIndex uint8
	var mfdFirst = true

	// Periodic buffer flush — runs in a separate goroutine to avoid blocking
	// packet processing. The buffer's internal mutex handles concurrent access.
	flushTicker := time.NewTicker(1 * time.Second)
	defer flushTicker.Stop()
	var flushing atomic.Bool

	for {
		select {
		case <-ctx.Done():
			// Final flush before exit (synchronous).
			if err := store.Buffers().FlushAll(context.Background()); err != nil {
				log.Error().Err(err).Msg("Failed final buffer flush")
			}
			log.Info().Msg("State writer stopping")
			return

		case pkt, ok := <-parsedChan:
			if !ok {
				return
			}
			updateState(state, pkt, store, sampleCounters, sampleRate, hifreqRate, eventHook)

			// Check for PTT button transitions on BUTN events (packet ID 3).
			// PTT settings are read fresh on every BUTN packet so dashboard
			// edits via /api/config land without a restart.
			//
			// In live_only mode the entire PTT pipeline is dead code: Gemini
			// Live uses server-side VAD on the continuous mic stream, STT is
			// not initialised, and the dashboard explicitly ignores `ptt`
			// WebSocket frames (see dashboard/src/hooks/useWebSocket.ts).
			// Skip the bit-twiddling and the chan push so we don't spam
			// "PTT button pressed" log lines for events nothing acts on.
			// The button probe stays wired so `buttonwatch` / LogButtons()
			// still surfaces raw bitmasks for users mapping their wheel.
			if pkt.PacketID == 3 {
				probeButton(pkt.Data, &probePrevStatus, &probeFirst, cfg.LogButtons())
				if cfg.PTTPathEnabled() {
					pttTrigger := cfg.PTTTrigger()
					if pttButton := cfg.PTTButton(); pttButton != 0 && pttTrigger == "button" {
						checkPTT(pkt.Data, pttButton, cfg.PTTMode(), &prevButtonStatus, &pttActive, pttChan)
					}
					if pttTrigger == "dual" {
						if start, end := cfg.PTTButtonStart(), cfg.PTTButtonEnd(); start != 0 || end != 0 {
							checkDualPTT(pkt.Data, start, end, &prevDualStatus, &pttActive, pttChan)
						}
					}
				}
			}

			// MFD-based PTT: any change in MFDPanelIndex from car_telemetry
			// (packet 6) is a "press" — driver cycled an MFD page on the wheel.
			// Reliable because car_telemetry is sampled at 60Hz so every
			// transition appears in consecutive packets. Skipped in live_only
			// for the same reason as the BUTN path above.
			if pkt.PacketID == 6 && cfg.PTTPathEnabled() && cfg.PTTTrigger() == "mfd" {
				checkMFDPTT(pkt.Data, cfg.PTTMode(), &prevMFDIndex, &mfdFirst, &pttActive, pttChan)
			}

			// Atomically publish new snapshot.
			snapshot := *state // shallow copy
			store.Cache().Store(&snapshot)

		case <-flushTicker.C:
			// Non-blocking flush: skip if a previous flush is still running.
			if flushing.CompareAndSwap(false, true) {
				go func() {
					defer flushing.Store(false)
					if err := store.Buffers().FlushAll(ctx); err != nil {
						log.Error().Err(err).Msg("Periodic buffer flush failed")
					}
				}()
			}
		}
	}
}

// updateState dispatches packet data into the running RaceState and DuckDB buffers.
func updateState(
	state *models.RaceState,
	pkt models.ParsedPacket,
	store *storage.Storage,
	sampleCounters map[uint8]int,
	sampleRate int,
	hifreqRate int,
	eventHook EventHook,
) {
	switch pkt.PacketID {
	case 0: // Motion
		handleMotion(state, pkt.Data, store, sampleCounters, sampleRate)
	case 1: // Session
		handleSession(state, pkt.Data, store)
	case 2: // Lap Data
		handleLapData(state, pkt.Data, store, sampleCounters, sampleRate)
	case 3: // Event
		handleEvent(state, pkt.Data, store, eventHook)
	case 4: // Participants
		handleParticipants(pkt.Data, store)
	case 5: // Car Setup (driver-controllable cockpit + setup-screen channels)
		handleCarSetup(state, pkt.Data, store, sampleCounters, sampleRate)
	case 6: // Car Telemetry
		handleCarTelemetry(state, pkt.Data, store, sampleCounters, sampleRate, hifreqRate)
	case 7: // Car Status
		handleCarStatus(state, pkt.Data, store, sampleCounters, sampleRate)
	case 10: // Car Damage
		handleCarDamage(state, pkt.Data, store)
	case 11: // Session History
		handleSessionHistory(pkt.Data, store)
	case 13: // Motion Ex (player-only wheel dynamics)
		handleMotionEx(state, pkt.Data)
	}
}

// shouldSample returns true if this packet should be written to DuckDB.
// High-frequency packets are sampled at 1/sampleRate to reduce storage load.
func shouldSample(counters map[uint8]int, packetID uint8, sampleRate int) bool {
	counters[packetID]++
	return counters[packetID]%sampleRate == 0
}

func handleMotion(state *models.RaceState, data []byte, store *storage.Storage, counters map[uint8]int, rate int) {
	pkt, err := packets.Parse(0, data)
	if err != nil {
		return
	}
	motion := pkt.(*packets.PacketMotionData)
	idx := int(motion.Header.PlayerCarIndex)
	if idx >= 22 {
		return
	}
	car := motion.CarMotionData[idx]
	state.PlayerCarIndex = motion.Header.PlayerCarIndex
	state.SessionUID = motion.Header.SessionUID
	state.FrameID = motion.Header.FrameIdentifier
	state.SessionTime = motion.Header.SessionTime
	state.WorldPosX = car.WorldPositionX
	state.WorldPosY = car.WorldPositionY
	state.WorldPosZ = car.WorldPositionZ
	state.GForceLateral = car.GForceLateral
	state.GForceLongitude = car.GForceLongitudinal
	state.GForceVertical = car.GForceVertical
	state.Yaw = car.Yaw
	state.Pitch = car.Pitch
	state.Roll = car.Roll

	// Mirror world position into the per-car grid snapshot for the
	// track_position tool. Lap-distance + status come from handleLapData.
	for i := 0; i < 22; i++ {
		c := motion.CarMotionData[i]
		gp := &state.GridPositions[i]
		gp.WorldPosX = c.WorldPositionX
		gp.WorldPosY = c.WorldPositionY
		gp.WorldPosZ = c.WorldPositionZ
	}

	if shouldSample(counters, 0, rate) {
		store.Buffers().Motion.Add(storage.MotionRow{
			CarIndex: idx,
			WorldPosX: float64(car.WorldPositionX),
			WorldPosY: float64(car.WorldPositionY),
			WorldPosZ: float64(car.WorldPositionZ),
			GForceLat: float64(car.GForceLateral),
			GForceLon: float64(car.GForceLongitudinal),
			GForceVer: float64(car.GForceVertical),
			Yaw:       float64(car.Yaw),
			Pitch:     float64(car.Pitch),
			Roll:      float64(car.Roll),
		})
	}
}

// handleMotionEx parses the player-only MotionEx packet (id 13) and stashes
// the per-wheel slip ratio / slip angle arrays into RaceState. The hi-freq
// writer (in handleCarTelemetry) reads them the next time a hi-freq sample
// fires, so the slip channels land in the same telemetry_hifreq row as the
// driver inputs / G-forces. Player-only; no per-car DuckDB row to write.
func handleMotionEx(state *models.RaceState, data []byte) {
	pkt, err := packets.Parse(13, data)
	if err != nil {
		return
	}
	mx, ok := pkt.(*packets.PacketMotionExData)
	if !ok {
		return
	}
	state.WheelSlipRatio = mx.WheelSlipRatio
	state.WheelSlipAngle = mx.WheelSlipAngle
}

func handleSession(state *models.RaceState, data []byte, store *storage.Storage) {
	pkt, err := packets.Parse(1, data)
	if err != nil {
		return
	}
	session := pkt.(*packets.PacketSessionData)
	state.Weather = session.Weather
	state.TrackTemp = session.TrackTemperature
	state.AirTemp = session.AirTemperature
	state.TotalLaps = session.TotalLaps
	state.TrackLength = session.TrackLength
	state.SessionType = session.SessionType
	state.TrackID = session.TrackID
	state.SessionTimeLeft = session.SessionTimeLeft
	state.SafetyCarStatus = session.SafetyCarStatus
	state.PitWindowIdeal = session.PitStopWindowIdealLap
	state.PitWindowLatest = session.PitStopWindowLatestLap
	state.Sector2DistStart = session.Sector2LapDistanceStart
	state.Sector3DistStart = session.Sector3LapDistanceStart

	// Nearest rain forecast.
	var rainPct uint8
	for i := 0; i < int(session.NumWeatherForecastSamples) && i < 64; i++ {
		ws := session.WeatherForecastSamples[i]
		if ws.TimeOffset <= 10 && ws.RainPercentage > rainPct {
			rainPct = ws.RainPercentage
		}
	}
	state.RainPercentage = rainPct

	// Store first 5 forecasts for insight engine.
	for i := 0; i < 5 && i < int(session.NumWeatherForecastSamples); i++ {
		ws := session.WeatherForecastSamples[i]
		state.WeatherForecasts[i] = models.WeatherSample{
			TimeOffset:     ws.TimeOffset,
			Weather:        ws.Weather,
			RainPercentage: ws.RainPercentage,
		}
	}

	store.Buffers().Session.Add(storage.SessionRow{
		Weather:       int(session.Weather),
		TrackTemp:     int(session.TrackTemperature),
		AirTemp:       int(session.AirTemperature),
		TotalLaps:     int(session.TotalLaps),
		TrackLength:   int(session.TrackLength),
		SessionType:   int(session.SessionType),
		TrackID:       int(session.TrackID),
		TimeLeft:      int(session.SessionTimeLeft),
		SafetyCar:     int(session.SafetyCarStatus),
		RainPct:       int(rainPct),
		PitIdealLap:   int(session.PitStopWindowIdealLap),
		PitLatestLap:  int(session.PitStopWindowLatestLap),
	})
}

func handleLapData(state *models.RaceState, data []byte, store *storage.Storage, counters map[uint8]int, rate int) {
	pkt, err := packets.Parse(2, data)
	if err != nil {
		return
	}
	lapPkt := pkt.(*packets.PacketLapData)
	idx := int(lapPkt.Header.PlayerCarIndex)
	if idx >= 22 {
		return
	}

	// Feed every car's race position into the roster so any car that
	// resolved to "" or a team-name fallback gets a "P5"-style label. This
	// is cheap (22-entry map + RWMutex.Lock) and we further reduce cost by
	// piggybacking on the existing 1Hz sample cadence.
	// Sentinel id 200 is well outside F1's real packet-id range (0-13)
	// so it can't collide with another sample counter.
	if shouldSample(counters, 200, rate) {
		positions := make(map[uint8]uint8, 22)
		for i := 0; i < 22; i++ {
			if p := lapPkt.LapData[i].CarPosition; p > 0 {
				positions[uint8(i)] = p
			}
		}
		store.Roster().SetPositionalFallback(positions)
	}

	// Player-only cache update — RaceState mirrors the driver's car. The
	// cache is consumed by sub-microsecond rule evaluations and the WS push,
	// where multi-car data would just be noise.
	lap := lapPkt.LapData[idx]
	state.CurrentLap = lap.CurrentLapNum
	state.Position = lap.CarPosition
	state.Sector = lap.Sector
	state.LapDistance = lap.LapDistance
	state.TotalDistance = lap.TotalDistance
	state.LastLapTimeMs = lap.LastLapTimeInMs
	state.CurrentLapTimeMs = lap.CurrentLapTimeInMs
	state.PitStatus = lap.PitStatus
	state.NumPitStops = lap.NumPitStops
	state.GridPosition = lap.GridPosition
	state.DriverStatus = lap.DriverStatus
	// Reconstruct delta from minutes+ms parts.
	state.DeltaToFrontMs = int32(lap.DeltaToCarInFrontMinutesPart)*60000 + int32(lap.DeltaToCarInFrontMsPart)
	state.DeltaToLeaderMs = int32(lap.DeltaToRaceLeaderMinutesPart)*60000 + int32(lap.DeltaToRaceLeaderMsPart)

	// Per-car grid snapshot for the cornerWatcher (200ms tick) and the
	// /api/state/track_position tool. These are bare struct writes — no
	// sampling gate; the watcher needs the freshest possible lap_distance
	// to fire reminders with corner-precision.
	for i := 0; i < 22; i++ {
		l := lapPkt.LapData[i]
		gp := &state.GridPositions[i]
		gp.CarIndex = uint8(i)
		gp.Position = l.CarPosition
		gp.CurrentLap = l.CurrentLapNum
		gp.Sector = l.Sector
		gp.LapDistance = l.LapDistance
		gp.TotalDistance = l.TotalDistance
		gp.PitStatus = l.PitStatus
		gp.DriverStatus = l.DriverStatus
		gp.ResultStatus = l.ResultStatus
		gp.NumPitStops = l.NumPitStops
	}

	// Persist all 22 cars on each sample tick — the analyst, the competitor
	// landscape tool, and any undercut/overcut SQL all need the full grid.
	// Skip slots where ResultStatus < 2 (invalid/inactive) to avoid writing
	// zeroed-out rows for empty AI slots in single-player sessions.
	if shouldSample(counters, 2, rate) {
		for i := 0; i < 22; i++ {
			l := lapPkt.LapData[i]
			if l.ResultStatus < 2 {
				continue
			}
			deltaFront := int32(l.DeltaToCarInFrontMinutesPart)*60000 + int32(l.DeltaToCarInFrontMsPart)
			deltaLeader := int32(l.DeltaToRaceLeaderMinutesPart)*60000 + int32(l.DeltaToRaceLeaderMsPart)
			store.Buffers().LapData.Add(storage.LapDataRow{
				CarIndex:      i,
				LastLapMs:     int(l.LastLapTimeInMs),
				CurrentLapMs:  int(l.CurrentLapTimeInMs),
				Sector1Ms:     int(l.Sector1TimeInMs),
				Sector2Ms:     int(l.Sector2TimeInMs),
				Position:      int(l.CarPosition),
				LapNum:        int(l.CurrentLapNum),
				PitStatus:     int(l.PitStatus),
				NumPitStops:   int(l.NumPitStops),
				Sector:        int(l.Sector),
				Penalties:     int(l.Penalties),
				DriverStatus:  int(l.DriverStatus),
				ResultStatus:  int(l.ResultStatus),
				DeltaFrontMs:  int(deltaFront),
				DeltaLeaderMs: int(deltaLeader),
				SpeedTrap:     float64(l.SpeedTrapFastestSpeed),
				GridPosition:  int(l.GridPosition),
			})
		}
	}
}

func handleEvent(state *models.RaceState, data []byte, store *storage.Storage, eventHook EventHook) {
	pkt, err := packets.Parse(3, data)
	if err != nil {
		return
	}
	event := pkt.(*packets.PacketEventData)
	code := string(event.EventStringCode[:])
	// Trim null bytes.
	for i, b := range event.EventStringCode {
		if b == 0 {
			code = string(event.EventStringCode[:i])
			break
		}
	}
	state.LastEventCode = code

	// Parse event details for the vehicle index.
	if len(event.EventDetails) > 0 {
		state.LastEventIdx = event.EventDetails[0]
	}

	// Extract button status for BUTN events.
	if code == "BUTN" {
		if btnDetail, parseErr := packets.ParseEventDetails(packets.EventButtons, event.EventDetails); parseErr == nil && btnDetail != nil {
			if btn, ok := btnDetail.(*packets.ButtonsEvent); ok {
				state.LastButtonStatus = btn.ButtonStatus
			}
		}
	}

	detail := ""
	switch code {
	case "FTLP":
		if len(event.EventDetails) >= 5 {
			detail = "fastest_lap"
		}
	case "RTMT":
		detail = "retirement"
	case "SAFC":
		detail = "safety_car"
	case "SSTA":
		detail = "session_started"
	case "SEND":
		detail = "session_ended"
		// Belt-and-braces: if the game emits SEND without a preceding CHQF
		// (rare — replay/disconnect paths), still flag the race as finished
		// so emitRaceFinished has something to latch on. We don't know who
		// crossed the line in what order, so just stamp the current Position.
		if models.IsRaceSession(state.SessionType) && !state.RaceFinished {
			state.RaceFinished = true
			state.FinalClassification = state.Position
		}
	case "LGOT":
		detail = "lights_out"
	case "CHQF":
		detail = "chequered_flag"
		// CHQF is broadcast (no per-vehicle index) — the very first one for
		// this race marks the leader crossing the line. The player's CHQF
		// is whenever they themselves cross, which from our state's POV is
		// the moment Position is locked AND CurrentLap == TotalLaps. We
		// latch on the first CHQF observed while the player is in the
		// race; emitRaceFinished gates the actual radio call on
		// CurrentLap reaching TotalLaps so we don't congratulate P1 with
		// two laps still to drive.
		if models.IsRaceSession(state.SessionType) && !state.RaceFinished &&
			state.TotalLaps > 0 && state.CurrentLap >= state.TotalLaps {
			state.RaceFinished = true
			state.FinalClassification = state.Position
		}
	case "RCWN":
		detail = "race_winner"
	case "OVTK":
		detail = "overtake"
	default:
		detail = code
	}

	vehicleIdx := 0
	if len(event.EventDetails) > 0 {
		vehicleIdx = int(event.EventDetails[0])
	}

	store.Buffers().RaceEvents.Add(storage.RaceEventRow{
		EventCode:  code,
		VehicleIdx: vehicleIdx,
		DetailText: detail,
	})

	if eventHook != nil {
		eventHook(code, detail, vehicleIdx)
	}
}

func handleCarTelemetry(state *models.RaceState, data []byte, store *storage.Storage, counters map[uint8]int, rate int, hifreqRate int) {
	pkt, err := packets.Parse(6, data)
	if err != nil {
		return
	}
	telPkt := pkt.(*packets.PacketCarTelemetryData)
	idx := int(telPkt.Header.PlayerCarIndex)
	if idx >= 22 {
		return
	}
	ct := telPkt.CarTelemetryData[idx]

	// Accumulate DRS usage windows BEFORE we overwrite the previous DRS
	// flag. dt is the gap to the prior CarTelemetry packet (~60Hz ⇒ ~16ms).
	// Reset on lap rollover so the metric is per-lap.
	if state.LastDRSLap != state.CurrentLap {
		state.DRSAvailableThisLap = 0
		state.DRSUsedThisLap = 0
		state.LastDRSLap = state.CurrentLap
		state.LastDRSSampleTime = state.SessionTime
	}
	if state.LastDRSSampleTime > 0 {
		dt := state.SessionTime - state.LastDRSSampleTime
		// Guard against negative dt (session-time reset on respawn / new
		// session) and absurd gaps (>1s) from a paused session.
		if dt > 0 && dt < 1.0 {
			if state.DRSAllowed != 0 {
				state.DRSAvailableThisLap += dt
			}
			if state.DRS != 0 {
				state.DRSUsedThisLap += dt
			}
		}
	}
	state.LastDRSSampleTime = state.SessionTime

	state.Speed = ct.Speed
	state.Throttle = ct.Throttle
	state.Brake = ct.Brake
	state.Steering = ct.Steer
	state.Gear = ct.Gear
	state.EngineRPM = ct.EngineRPM
	state.DRS = ct.DRS
	state.EngineTemp = ct.EngineTemperature
	state.BrakesTemp = ct.BrakesTemperature
	state.TyresSurfTemp = ct.TyresSurfaceTemperature
	state.TyresInnerTemp = ct.TyresInnerTemperature
	state.TyresPressure = ct.TyresPressure
	state.SuggestedGear = telPkt.SuggestedGear
	// Surface enum per wheel — sampled at the same 60Hz cadence as everything
	// else in this packet. Stashed on state so the hi-freq writer below picks
	// it up without re-parsing.
	state.SurfaceType = ct.SurfaceType

	// Hi-freq player-only sample for the lap-trace + brake-point analysis
	// tools. Dedicated sentinel ID (201) so it shares no counter with the
	// 1Hz car_telemetry_ext sampler and can run at a different stride
	// (default 2 → 10Hz). State pointer reads include the just-updated
	// channels above PLUS lap/distance fields landed by handleLapData.
	if hifreqRate > 0 && shouldSample(counters, 201, hifreqRate) {
		store.Buffers().TelemetryHifreq.Add(storage.TelemetryHifreqRow{
			SessionUID:       state.SessionUID,
			FrameID:          int(state.FrameID),
			Lap:              int(state.CurrentLap),
			TrackPosition:    float64(state.LapDistance),
			TotalDistance:    float64(state.TotalDistance),
			CurrentLapTimeMs: int(state.CurrentLapTimeMs),
			PitStatus:        int(state.PitStatus),
			Throttle:         float64(ct.Throttle),
			Brake:            float64(ct.Brake),
			Steering:         float64(ct.Steer),
			Speed:            int(ct.Speed),
			Gear:             int(ct.Gear),
			EngineRPM:        int(ct.EngineRPM),
			DRS:              int(ct.DRS),
			// Brake/tyre temp arrays are F1 25 RL/RR/FL/FR ordered.
			BrakeTempRL:    int(ct.BrakesTemperature[0]),
			BrakeTempRR:    int(ct.BrakesTemperature[1]),
			BrakeTempFL:    int(ct.BrakesTemperature[2]),
			BrakeTempFR:    int(ct.BrakesTemperature[3]),
			TyreSurfTempRL: int(ct.TyresSurfaceTemperature[0]),
			TyreSurfTempRR: int(ct.TyresSurfaceTemperature[1]),
			TyreSurfTempFL: int(ct.TyresSurfaceTemperature[2]),
			TyreSurfTempFR: int(ct.TyresSurfaceTemperature[3]),
			WorldPosX:      float64(state.WorldPosX),
			WorldPosY:      float64(state.WorldPosY),
			WorldPosZ:      float64(state.WorldPosZ),
			GForceLat:      float64(state.GForceLateral),
			GForceLon:      float64(state.GForceLongitude),
			GForceVert:     float64(state.GForceVertical),
			// Inner tyre temps + pressures from the same packet's extended
			// arrays so brake/tyre asymmetry shows up at hi-freq cadence.
			TyreInnerTempRL: int(ct.TyresInnerTemperature[0]),
			TyreInnerTempRR: int(ct.TyresInnerTemperature[1]),
			TyreInnerTempFL: int(ct.TyresInnerTemperature[2]),
			TyreInnerTempFR: int(ct.TyresInnerTemperature[3]),
			TyrePressureRL:  float64(ct.TyresPressure[0]),
			TyrePressureRR:  float64(ct.TyresPressure[1]),
			TyrePressureFL:  float64(ct.TyresPressure[2]),
			TyrePressureFR:  float64(ct.TyresPressure[3]),
			// Energy + driver-aid channels read from RaceState (populated by
			// handleCarStatus on a separate packet stream); fine if the very
			// first samples lag by a tick.
			FuelInTank:     float64(state.FuelInTank),
			ErsStoreEnergy: float64(state.ERSStoreEnergy),
			ErsDeployMode:  int(state.ERSDeployMode),
			Clutch:         int(ct.Clutch),
			SuggestedGear:  int(telPkt.SuggestedGear),
			// Driver-setting + DRS-allowed channels. brake_bias is uint8 in
			// state but stored DOUBLE so per-lap traces can interpolate
			// across mid-stint tweaks without integer steps.
			BrakeBias:      float64(state.DriverSettings.BrakeBias),
			DiffOnThrottle: int(state.DriverSettings.OnThrottleDiff),
			EngineBraking:  int(state.DriverSettings.EngineBraking),
			DRSAllowed:     int(state.DRSAllowed),
			// Sliding + surface. slip values come from MotionEx (packet 13)
			// landed on state by handleMotionEx; surface comes from this
			// same CarTelemetry packet (just stashed above). Both are
			// per-wheel in RL/RR/FL/FR ordering.
			WheelSlipRatioRL: float64(state.WheelSlipRatio[0]),
			WheelSlipRatioRR: float64(state.WheelSlipRatio[1]),
			WheelSlipRatioFL: float64(state.WheelSlipRatio[2]),
			WheelSlipRatioFR: float64(state.WheelSlipRatio[3]),
			WheelSlipAngleRL: float64(state.WheelSlipAngle[0]),
			WheelSlipAngleRR: float64(state.WheelSlipAngle[1]),
			WheelSlipAngleFL: float64(state.WheelSlipAngle[2]),
			WheelSlipAngleFR: float64(state.WheelSlipAngle[3]),
			SurfaceTypeRL:    int(ct.SurfaceType[0]),
			SurfaceTypeRR:    int(ct.SurfaceType[1]),
			SurfaceTypeFL:    int(ct.SurfaceType[2]),
			SurfaceTypeFR:    int(ct.SurfaceType[3]),
		})
	}

	if shouldSample(counters, 6, rate) {
		store.Buffers().TelemetryExt.Add(storage.TelemetryExtRow{
			CarIndex:       idx,
			BrakeTempRL:    int(ct.BrakesTemperature[0]),
			BrakeTempRR:    int(ct.BrakesTemperature[1]),
			BrakeTempFL:    int(ct.BrakesTemperature[2]),
			BrakeTempFR:    int(ct.BrakesTemperature[3]),
			TyreSurfTempRL: int(ct.TyresSurfaceTemperature[0]),
			TyreSurfTempRR: int(ct.TyresSurfaceTemperature[1]),
			TyreSurfTempFL: int(ct.TyresSurfaceTemperature[2]),
			TyreSurfTempFR: int(ct.TyresSurfaceTemperature[3]),
			TyreInnerTempRL: int(ct.TyresInnerTemperature[0]),
			TyreInnerTempRR: int(ct.TyresInnerTemperature[1]),
			TyreInnerTempFL: int(ct.TyresInnerTemperature[2]),
			TyreInnerTempFR: int(ct.TyresInnerTemperature[3]),
			EngineTemp:     int(ct.EngineTemperature),
			TyrePressureRL: float64(ct.TyresPressure[0]),
			TyrePressureRR: float64(ct.TyresPressure[1]),
			TyrePressureFL: float64(ct.TyresPressure[2]),
			TyrePressureFR: float64(ct.TyresPressure[3]),
			DRS:            int(ct.DRS),
			Clutch:         int(ct.Clutch),
			SuggestedGear:  int(telPkt.SuggestedGear),
		})

		// Also write to the legacy telemetry table.
		store.Buffers().Telemetry.Add(storage.TelemetryRow{
			Speed:    float64(ct.Speed),
			Gear:     int(ct.Gear),
			Throttle: float64(ct.Throttle),
			Brake:    float64(ct.Brake),
			Steering: float64(ct.Steer),
			RPM:      int(ct.EngineRPM),
			WearFL:   float64(state.TyresWear[2]),
			WearFR:   float64(state.TyresWear[3]),
			WearRL:   float64(state.TyresWear[0]),
			WearRR:   float64(state.TyresWear[1]),
			Lap:      int(state.CurrentLap),
			TrackPos: float64(state.LapDistance),
			Sector:   int(state.Sector),
		})
	}
}

func handleCarStatus(state *models.RaceState, data []byte, store *storage.Storage, counters map[uint8]int, rate int) {
	pkt, err := packets.Parse(7, data)
	if err != nil {
		return
	}
	statusPkt := pkt.(*packets.PacketCarStatusData)
	idx := int(statusPkt.Header.PlayerCarIndex)
	if idx >= 22 {
		return
	}

	// Player-only cache update.
	s := statusPkt.CarStatusData[idx]
	state.FuelMix = s.FuelMix
	state.FuelInTank = s.FuelInTank
	state.FuelRemainingLaps = s.FuelRemainingLaps
	state.ERSStoreEnergy = s.ERSStoreEnergy
	state.ERSDeployMode = s.ERSDeployMode
	state.ERSHarvestedMGUK = s.ERSHarvestedThisLapMGUK
	state.ERSHarvestedMGUH = s.ERSHarvestedThisLapMGUH
	state.ERSDeployedLap = s.ERSDeployedThisLap
	state.ActualCompound = s.ActualTyreCompound
	state.VisualCompound = s.VisualTyreCompound
	state.TyresAgeLaps = s.TyresAgeLaps
	state.DRSAllowed = s.DRSAllowed
	state.VehicleFIAFlags = s.VehicleFIAFlags

	// Mirror compound + tire age into the per-car cache for the live map /
	// leaderboard. Unconditional so a pit-stop compound swap shows up the
	// instant the packet lands, not on the next DuckDB sample tick.
	for i := 0; i < 22; i++ {
		cs := statusPkt.CarStatusData[i]
		gp := &state.GridPositions[i]
		gp.ActualCompound = cs.ActualTyreCompound
		gp.VisualCompound = cs.VisualTyreCompound
		gp.TyresAgeLaps = cs.TyresAgeLaps
	}

	// Persist all 22 cars — competitor compound + tire age + fuel/ERS are
	// what the analyst and competitor-landscape tool need to reason about
	// undercut/overcut and stint length. Filter empty AI slots: F1 25 sends
	// zero/garbage fuel for cars that don't exist in the session, so a
	// FuelInTank of zero is a reasonable proxy for "no real car here".
	if shouldSample(counters, 7, rate) {
		for i := 0; i < 22; i++ {
			cs := statusPkt.CarStatusData[i]
			if cs.FuelInTank <= 0 && cs.ActualTyreCompound == 0 {
				continue
			}
			store.Buffers().CarStatus.Add(storage.CarStatusRow{
				CarIndex:    i,
				FuelMix:     int(cs.FuelMix),
				FuelInTank:  float64(cs.FuelInTank),
				FuelRemLaps: float64(cs.FuelRemainingLaps),
				ERSStore:    float64(cs.ERSStoreEnergy),
				ERSMode:     int(cs.ERSDeployMode),
				ERSMguk:     float64(cs.ERSHarvestedThisLapMGUK),
				ERSMguh:     float64(cs.ERSHarvestedThisLapMGUH),
				ERSDeployed: float64(cs.ERSDeployedThisLap),
				ActualComp:  int(cs.ActualTyreCompound),
				VisualComp:  int(cs.VisualTyreCompound),
				TyresAge:    int(cs.TyresAgeLaps),
				DRSAllowed:  int(cs.DRSAllowed),
				FIAFlags:    int(cs.VehicleFIAFlags),
				PowerICE:    float64(cs.EnginePowerICE),
				PowerMGUK:   float64(cs.EnginePowerMGUK),
			})
		}
	}
}

// handleCarSetup parses the F1 25 CarSetup packet (ID 5), mirrors the
// player-car setup channels into state.DriverSettings (always-on; consumed by
// the brain snapshot and /api/state/driver_settings), records a SettingChange
// in the recent-changes ring whenever a monitored value differs from the
// previous packet, and persists all 22 cars on the sampling cadence.
//
// CarSetup arrives at ~2Hz natively so storage cost is minimal; we still pass
// it through the same sample gate as car_status to keep all setup-related
// tables on a consistent stride.
func handleCarSetup(state *models.RaceState, data []byte, store *storage.Storage, counters map[uint8]int, rate int) {
	pkt, err := packets.ParseCarSetupData(data)
	if err != nil {
		return
	}
	idx := int(pkt.Header.PlayerCarIndex)
	if idx >= 22 {
		return
	}

	prev := state.DriverSettings
	s := pkt.CarSetups[idx]

	state.DriverSettings.FrontWing = s.FrontWing
	state.DriverSettings.RearWing = s.RearWing
	state.DriverSettings.OnThrottleDiff = s.OnThrottle
	state.DriverSettings.OffThrottleDiff = s.OffThrottle
	state.DriverSettings.EngineBraking = s.EngineBraking
	state.DriverSettings.BrakePressure = s.BrakePressure
	state.DriverSettings.BrakeBias = s.BrakeBias
	state.DriverSettings.Ballast = s.Ballast
	state.DriverSettings.FuelLoad = s.FuelLoad
	state.DriverSettings.FrontLeftTyrePressure = s.FrontLeftTyrePressure
	state.DriverSettings.FrontRightTyrePressure = s.FrontRightTyrePressure
	state.DriverSettings.RearLeftTyrePressure = s.RearLeftTyrePressure
	state.DriverSettings.RearRightTyrePressure = s.RearRightTyrePressure
	// Preserve the ring buffer fields across the value reassignment above.
	state.DriverSettings.RecentChanges = prev.RecentChanges
	state.DriverSettings.RecentChangesLen = prev.RecentChangesLen
	state.DriverSettings.RecentChangesHead = prev.RecentChangesHead

	// Record meaningful transitions in the recent-changes ring. We only
	// monitor the channels the analyst + Live agent will reason about, to
	// keep the ring informative — wing damage / pressures stay off the
	// monitored list because they don't move mid-stint via cockpit knobs.
	monitored := []struct {
		name    string
		from    int
		to      int
	}{
		{"brake_bias", int(prev.BrakeBias), int(s.BrakeBias)},
		{"on_throttle_diff", int(prev.OnThrottleDiff), int(s.OnThrottle)},
		{"off_throttle_diff", int(prev.OffThrottleDiff), int(s.OffThrottle)},
		{"engine_braking", int(prev.EngineBraking), int(s.EngineBraking)},
		{"brake_pressure", int(prev.BrakePressure), int(s.BrakePressure)},
		{"front_wing", int(prev.FrontWing), int(s.FrontWing)},
		{"rear_wing", int(prev.RearWing), int(s.RearWing)},
	}
	// First packet (prev is zero-valued): skip the diff so we don't emit
	// fake "0 → 56" changes the moment the car appears on track.
	firstPacket := prev.BrakeBias == 0 && prev.BrakePressure == 0 && prev.OnThrottleDiff == 0
	if !firstPacket {
		for _, m := range monitored {
			if m.from == m.to {
				continue
			}
			head := state.DriverSettings.RecentChangesHead % 8
			state.DriverSettings.RecentChanges[head] = models.SettingChange{
				SessionTime: state.SessionTime,
				Lap:         state.CurrentLap,
				Channel:     m.name,
				From:        m.from,
				To:          m.to,
			}
			state.DriverSettings.RecentChangesHead = (head + 1) % 8
			if state.DriverSettings.RecentChangesLen < 8 {
				state.DriverSettings.RecentChangesLen++
			}
		}
	}

	if !shouldSample(counters, 5, rate) {
		return
	}

	// Persist every active car so the analyst can compare the player's
	// setup against the field. Skip empty AI slots — F1 25 emits zeroed
	// rows for inactive grid positions, so a zero fuel_load + zero
	// brake_bias is a reasonable "no real car here" proxy.
	for i := 0; i < 22; i++ {
		cs := pkt.CarSetups[i]
		if cs.FuelLoad == 0 && cs.BrakeBias == 0 {
			continue
		}
		store.Buffers().CarSetup.Add(storage.CarSetupRow{
			CarIndex:               i,
			FrontWing:              int(cs.FrontWing),
			RearWing:               int(cs.RearWing),
			OnThrottleDiff:         int(cs.OnThrottle),
			OffThrottleDiff:        int(cs.OffThrottle),
			FrontCamber:            float64(cs.FrontCamber),
			RearCamber:             float64(cs.RearCamber),
			FrontToe:               float64(cs.FrontToe),
			RearToe:                float64(cs.RearToe),
			FrontSuspension:        int(cs.FrontSuspension),
			RearSuspension:         int(cs.RearSuspension),
			FrontAntiRollBar:       int(cs.FrontAntiRollBar),
			RearAntiRollBar:        int(cs.RearAntiRollBar),
			FrontRideHeight:        int(cs.FrontSuspensionHeight),
			RearRideHeight:         int(cs.RearSuspensionHeight),
			BrakePressure:          int(cs.BrakePressure),
			BrakeBias:              int(cs.BrakeBias),
			EngineBraking:          int(cs.EngineBraking),
			RearLeftTyrePressure:   float64(cs.RearLeftTyrePressure),
			RearRightTyrePressure:  float64(cs.RearRightTyrePressure),
			FrontLeftTyrePressure:  float64(cs.FrontLeftTyrePressure),
			FrontRightTyrePressure: float64(cs.FrontRightTyrePressure),
			Ballast:                int(cs.Ballast),
			FuelLoad:               float64(cs.FuelLoad),
		})
	}
}

func handleCarDamage(state *models.RaceState, data []byte, store *storage.Storage) {
	pkt, err := packets.Parse(10, data)
	if err != nil {
		return
	}
	dmgPkt := pkt.(*packets.PacketCarDamageData)
	idx := int(dmgPkt.Header.PlayerCarIndex)
	if idx >= 22 {
		return
	}
	d := dmgPkt.CarDamageData[idx]

	state.TyresWear = d.TyresWear
	state.TyresDamage = d.TyresDamage
	state.BrakesDmg = d.BrakesDamage
	state.FrontLeftWingDmg = d.FrontLeftWingDamage
	state.FrontRightWingDmg = d.FrontRightWingDamage
	state.RearWingDmg = d.RearWingDamage
	state.FloorDmg = d.FloorDamage
	state.DiffuserDmg = d.DiffuserDamage
	state.SidepodDmg = d.SidepodDamage
	state.GearBoxDmg = d.GearBoxDamage
	state.EngineDmg = d.EngineDamage
	state.DRSFault = d.DRSFault
	state.ERSFault = d.ERSFault

	store.Buffers().CarDamage.Add(storage.CarDamageRow{
		CarIndex:  idx,
		WearRL:    float64(d.TyresWear[0]),
		WearRR:    float64(d.TyresWear[1]),
		WearFL:    float64(d.TyresWear[2]),
		WearFR:    float64(d.TyresWear[3]),
		DmgRL:     int(d.TyresDamage[0]),
		DmgRR:     int(d.TyresDamage[1]),
		DmgFL:     int(d.TyresDamage[2]),
		DmgFR:     int(d.TyresDamage[3]),
		FLWing:    int(d.FrontLeftWingDamage),
		FRWing:    int(d.FrontRightWingDamage),
		RearWing:  int(d.RearWingDamage),
		Floor:     int(d.FloorDamage),
		Diffuser:  int(d.DiffuserDamage),
		Sidepod:   int(d.SidepodDamage),
		Gearbox:   int(d.GearBoxDamage),
		Engine:    int(d.EngineDamage),
		MGUHWear:  int(d.EngineMGUHWear),
		ESWear:    int(d.EngineESWear),
		CEWear:    int(d.EngineCEWear),
		ICEWear:   int(d.EngineICEWear),
		MGUKWear:  int(d.EngineMGUKWear),
		TCWear:    int(d.EngineTCWear),
		DRSFault:  int(d.DRSFault),
		ERSFault:  int(d.ERSFault),
	})
}

// checkPTT detects PTT activations from F1 BUTN events. Two modes:
//   - "hold"   (default): mic active while ANY masked button is held. true on
//                         the first press, false when the last masked button
//                         is released. OR multiple bits into PTT_BUTTON to
//                         accept any of several wheel buttons.
//   - "toggle":           each press of any masked bit flips the latched
//                         mic state. Releases ignored. Survives BUTN-packet
//                         chatter (repeated identical status). Multi-bit
//                         masks work per-bit: pressing R2 while L2 is held
//                         still fires a toggle.
//
// Tip: PTT_BUTTON is a bitmask. To accept L2 OR R2, OR them: 0x00000800 |
// 0x00001000 = 0x00001800. Mix-in UDP Action slots the same way.
func checkPTT(data []byte, pttButton uint32, pttMode string, prevStatus *uint32, pttActive *bool, pttChan chan<- bool) {
	pkt, err := packets.ParseEventData(data)
	if err != nil {
		return
	}
	code := pkt.GetEventCode()
	if code != packets.EventButtons {
		return
	}

	detail, err := packets.ParseEventDetails(code, pkt.EventDetails)
	if err != nil || detail == nil {
		return
	}

	btn, ok := detail.(*packets.ButtonsEvent)
	if !ok {
		return
	}

	prev := *prevStatus
	*prevStatus = btn.ButtonStatus

	// Per-bit rising / falling edges *within the mask*. Subtle: for a
	// single-bit pttButton these reduce to the old nowPressed/wasPressed
	// semantics (rising != 0 ⇔ press, falling != 0 ⇔ release). For a
	// multi-bit mask they correctly fire on each individual button
	// transition, even when other masked bits stay held.
	risingMasked := btn.ButtonStatus & ^prev & pttButton
	fallingMasked := prev & ^btn.ButtonStatus & pttButton
	anyMaskedHeld := btn.ButtonStatus&pttButton != 0

	if pttMode == "toggle" {
		// Toggle once per fresh press, regardless of how many masked bits
		// are currently held. risingMasked could carry multiple bits in a
		// single packet (rare with discrete wheel buttons but possible);
		// we still fire one toggle so a "two buttons pressed simultaneously"
		// frame doesn't oscillate the mic state twice in one tick.
		if risingMasked != 0 {
			*pttActive = !*pttActive
			msg := "PTT toggled OFF"
			if *pttActive {
				msg = "PTT toggled ON"
			}
			log.Info().
				Uint32("button_status", btn.ButtonStatus).
				Str("via_hex", fmt.Sprintf("0x%08X", risingMasked)).
				Str("via_name", packets.ButtonName(risingMasked)).
				Msg(msg)
			select {
			case pttChan <- *pttActive:
			default:
			}
		}
		return
	}

	// "hold" mode (default). Mic on when any masked bit is held; off when
	// all masked bits release. Works whether the user holds one or several
	// of the configured buttons in any order.
	if anyMaskedHeld && !*pttActive {
		*pttActive = true
		log.Info().
			Uint32("button_status", btn.ButtonStatus).
			Str("via_hex", fmt.Sprintf("0x%08X", risingMasked)).
			Str("via_name", packets.ButtonName(risingMasked)).
			Msg("PTT button pressed")
		select {
		case pttChan <- true:
		default:
		}
	} else if !anyMaskedHeld && *pttActive {
		*pttActive = false
		log.Info().
			Uint32("button_status", btn.ButtonStatus).
			Str("via_hex", fmt.Sprintf("0x%08X", fallingMasked)).
			Str("via_name", packets.ButtonName(fallingMasked)).
			Msg("PTT button released")
		select {
		case pttChan <- false:
		default:
		}
	}
}

// checkDualPTT handles two-button PTT: one bitmask opens the mic, another
// closes it. Press-edge only — releases are ignored, so a missed release
// can't strand the mic. Idempotent: pressing "start" while already active
// is a no-op (same for "end" while inactive). Designed for UDP Action
// bindings on dedicated wheel buttons.
func checkDualPTT(data []byte, startMask, endMask uint32, prevStatus *uint32, pttActive *bool, pttChan chan<- bool) {
	pkt, err := packets.ParseEventData(data)
	if err != nil {
		return
	}
	code := pkt.GetEventCode()
	if code != packets.EventButtons {
		return
	}

	detail, err := packets.ParseEventDetails(code, pkt.EventDetails)
	if err != nil || detail == nil {
		return
	}

	btn, ok := detail.(*packets.ButtonsEvent)
	if !ok {
		return
	}

	prev := *prevStatus
	*prevStatus = btn.ButtonStatus

	startNow := startMask != 0 && btn.ButtonStatus&startMask != 0
	startWas := startMask != 0 && prev&startMask != 0
	endNow := endMask != 0 && btn.ButtonStatus&endMask != 0
	endWas := endMask != 0 && prev&endMask != 0

	// Rising edge on start → mic ON (idempotent).
	if startNow && !startWas && !*pttActive {
		*pttActive = true
		log.Info().
			Uint32("button_status", btn.ButtonStatus).
			Uint32("start_mask", startMask).
			Msg("PTT mic ON (dual: start press)")
		select {
		case pttChan <- true:
		default:
		}
	}

	// Rising edge on end → mic OFF (idempotent).
	if endNow && !endWas && *pttActive {
		*pttActive = false
		log.Info().
			Uint32("button_status", btn.ButtonStatus).
			Uint32("end_mask", endMask).
			Msg("PTT mic OFF (dual: end press)")
		select {
		case pttChan <- false:
		default:
		}
	}
}

// checkMFDPTT treats any change in the MFD panel index as a single PTT
// "press" event. MFDPanelIndex is part of the car_telemetry packet (id 6),
// which F1 25 sends at 60Hz with the *current* index as a snapshot — so
// no edge is ever missed regardless of how fast the driver cycles pages.
//
// Modes:
//   - "toggle" (recommended for MFD): each MFD change flips _ptt_active.
//     One press = mic on, next press = mic off. Driver may end up on a
//     different MFD page, but with no driving impact.
//   - "hold": treats the change as a momentary press+release. Not very
//     useful with MFD because we have no "release" signal — the index
//     just sits at its new value. We still emit a true→false pair so
//     hold-mode consumers don't get stuck.
func checkMFDPTT(data []byte, pttMode string, prev *uint8, first *bool, pttActive *bool, pttChan chan<- bool) {
	pkt, err := packets.Parse(6, data)
	if err != nil {
		return
	}
	telPkt, ok := pkt.(*packets.PacketCarTelemetryData)
	if !ok {
		return
	}
	idx := telPkt.MFDPanelIndex

	if *first {
		*prev = idx
		*first = false
		return
	}
	if idx == *prev {
		return
	}
	from := *prev
	*prev = idx

	if pttMode == "toggle" {
		*pttActive = !*pttActive
		msg := "PTT toggled OFF (MFD)"
		if *pttActive {
			msg = "PTT toggled ON (MFD)"
		}
		log.Info().
			Uint8("mfd_from", from).
			Uint8("mfd_to", idx).
			Msg(msg)
		select {
		case pttChan <- *pttActive:
		default:
		}
		return
	}

	// "hold" semantics on a discrete signal: emit a brief on→off pulse so
	// downstream consumers see the press without staying latched.
	log.Info().Uint8("mfd_from", from).Uint8("mfd_to", idx).Msg("PTT pulse (MFD)")
	select {
	case pttChan <- true:
	default:
	}
	select {
	case pttChan <- false:
	default:
	}
}

// BUTN diagnostics. Counters survive across calls so we can periodically
// summarise rate, dropped packets, and coalesced changes.
var (
	butnTotal     atomic.Uint64 // every BUTN packet seen by the writer
	butnNoChange  atomic.Uint64 // BUTN packets where the bitmask didn't change (game heartbeat)
	butnLastStats atomic.Int64  // unix nanos of last stats log
)

// BUTN_PROBE_RAW / BUTN_STATS_LOG gate the diagnostic logs that flooded the
// console while we were identifying buttons. Default OFF — the only
// always-on BUTN output is "PTT toggled ON/OFF" from checkPTT, which is
// operationally relevant. Counters keep ticking either way so the values
// are correct if the user flips the flag and reads /health.
//
// The user-facing LOG_BUTTONS knob lives on *config.Config and is read
// live each BUTN packet (see stateWriter), so toggling it from the
// dashboard takes effect immediately. The remaining env-only flags here
// are internal debug switches.
var butnProbeEnv = envBool("BUTN_PROBE")
var butnProbeRaw = envBool("BUTN_PROBE_RAW")
var butnStatsLog = envBool("BUTN_STATS_LOG")

func envBool(key string) bool {
	v := os.Getenv(key)
	return v == "true" || v == "1"
}

// probeButton logs every BUTN status change with the exact bitmask. Used
// to identify which controller button maps to which bit so the user can
// pick a PTT_BUTTON value. Independent of the PTT_BUTTON config.
func probeButton(data []byte, prev *uint32, first *bool, logEnabled bool) {
	pkt, err := packets.ParseEventData(data)
	if err != nil || pkt.GetEventCode() != packets.EventButtons {
		return
	}
	detail, err := packets.ParseEventDetails(packets.EventButtons, pkt.EventDetails)
	if err != nil || detail == nil {
		return
	}
	btn, ok := detail.(*packets.ButtonsEvent)
	if !ok {
		return
	}

	// Diagnostics: count every BUTN packet the pipeline successfully decoded.
	// Drops earlier in the pipeline (udp.go / parser.go) are counted there.
	butnTotal.Add(1)

	// Millisecond-precision wall clock for all log lines below.
	now := time.Now().Format("15:04:05.000")

	// Raw mode: log every BUTN packet, even no-change ones. The game emits
	// BUTN on every state transition, so a "missed" press should still
	// appear here as a same-status event if F1 sent anything at all.
	// Distinguishes "game didn't emit" from "we dropped it".
	if butnProbeRaw {
		log.Info().
			Str("at", now).
			Str("status_hex", fmt.Sprintf("0x%08X", btn.ButtonStatus)).
			Str("prev_hex", fmt.Sprintf("0x%08X", *prev)).
			Bool("changed", btn.ButtonStatus != *prev).
			Uint64("total", butnTotal.Load()).
			Msg("BUTN raw")
	}

	// Periodic stats every 30s — gated by BUTN_STATS_LOG so it's silent
	// once the user is past the diagnostic phase. Drop counters are still
	// updated (non-zero values are surfaced via the existing WRN logs).
	if butnStatsLog {
		logStatsIfDue(now)
	}

	if *first {
		*prev = btn.ButtonStatus
		*first = false
		return
	}
	if btn.ButtonStatus == *prev {
		butnNoChange.Add(1)
		return
	}
	pressed := btn.ButtonStatus & ^(*prev)
	released := *prev & ^btn.ButtonStatus
	*prev = btn.ButtonStatus

	if !logEnabled && !butnProbeEnv {
		return
	}
	if pressed != 0 {
		log.Info().
			Str("at", now).
			Str("event", "PRESS").
			Str("hex", fmt.Sprintf("0x%08X", pressed)).
			Str("name", packets.ButtonName(pressed)).
			Str("status_hex", fmt.Sprintf("0x%08X", btn.ButtonStatus)).
			Msg("BUTN probe")
	}
	if released != 0 {
		log.Info().
			Str("at", now).
			Str("event", "RELEASE").
			Str("hex", fmt.Sprintf("0x%08X", released)).
			Str("name", packets.ButtonName(released)).
			Str("status_hex", fmt.Sprintf("0x%08X", btn.ButtonStatus)).
			Msg("BUTN probe")
	}
}

// logStatsIfDue emits one summary line every 30s with BUTN/drop counters
// so the user can see the live rate and detect pipeline starvation. Called
// from probeButton on every BUTN packet — cheap because it just compares
// a unix nano timestamp.
func logStatsIfDue(now string) {
	const intervalNs = int64(30 * time.Second)
	last := butnLastStats.Load()
	cur := time.Now().UnixNano()
	if last != 0 && cur-last < intervalNs {
		return
	}
	if !butnLastStats.CompareAndSwap(last, cur) {
		return
	}
	if last == 0 {
		// First call: just seed the timestamp, no log yet.
		return
	}
	log.Info().
		Str("at", now).
		Uint64("butn_total", butnTotal.Load()).
		Uint64("butn_no_change", butnNoChange.Load()).
		Uint64("packetchan_drops", PacketChanDrops.Load()).
		Uint64("parsedchan_drops", ParsedChanDrops.Load()).
		Msg("BUTN stats (30s)")
}

// PacketChanDrops counts UDP packets dropped at the udp→parser boundary
// because packetChan was full. Surfaced via /health.
var PacketChanDrops atomic.Uint64

// ParsedChanDrops counts packets dropped at the parser→writer boundary
// because parsedChan was full. Surfaced via /health.
var ParsedChanDrops atomic.Uint64

// handleParticipants refreshes the in-memory driver roster from a F1
// Participants packet. The packet arrives ~5Hz and is the canonical source
// for per-car names, team ids, race numbers, and AI/network flags. The
// roster is reset internally whenever SessionUID changes (custom champ,
// new session) so stale names from a prior race never leak through.
func handleParticipants(data []byte, store *storage.Storage) {
	pkt, err := packets.ParseParticipantsData(data)
	if err != nil {
		return
	}
	store.Roster().Update(
		pkt.Header.SessionUID,
		pkt.Header.PlayerCarIndex,
		pkt.NumActiveCars,
		pkt.Participants,
	)
}

func handleSessionHistory(data []byte, store *storage.Storage) {
	pkt, err := packets.Parse(11, data)
	if err != nil {
		return
	}
	histPkt := pkt.(*packets.PacketSessionHistoryData)
	sessionUID := histPkt.Header.SessionUID
	carIdx := int(histPkt.CarIdx)

	// SessionHistory rebroadcasts the full per-car history every few seconds.
	// We push every completed lap (lap_time_in_ms > 0) — the upsert keyed on
	// (session_uid, car_index, lap_num) collapses re-broadcasts so the table
	// stays one row per lap regardless of how many times the packet repeats.
	rows := make([]storage.SessionHistoryRow, 0, int(histPkt.NumLaps))
	for i := 0; i < int(histPkt.NumLaps) && i < 100; i++ {
		lh := histPkt.LapHistoryData[i]
		if lh.LapTimeInMs == 0 {
			continue
		}
		rows = append(rows, storage.SessionHistoryRow{
			SessionUID: sessionUID,
			CarIndex:   carIdx,
			LapNum:     i + 1,
			LapTimeMs:  int(lh.LapTimeInMs),
			S1Ms:       int(lh.Sector1TimeInMs),
			S2Ms:       int(lh.Sector2TimeInMs),
			S3Ms:       int(lh.Sector3TimeInMs),
			LapValid:   int(lh.LapValidBitFlags & 0x01),
		})
	}
	if err := store.UpsertSessionHistory(rows); err != nil {
		log.Warn().Err(err).Uint64("session_uid", sessionUID).Int("car_index", carIdx).Msg("session_history upsert failed")
	}
}
