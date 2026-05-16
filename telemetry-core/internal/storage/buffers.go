package storage

import (
	"context"
	"database/sql"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// ---------------------------------------------------------------------------
// Generic batch buffer
// ---------------------------------------------------------------------------

// TableBuffer is a generic batch buffer that accumulates rows of type T and
// flushes them to DuckDB in bulk when the batch size threshold is reached.
type TableBuffer[T any] struct {
	mu        sync.Mutex
	items     []T
	batchSize int
	flushFn   func(ctx context.Context, items []T) error
	tableName string // for logging
}

// NewTableBuffer creates a new TableBuffer for the given table.
func NewTableBuffer[T any](name string, batchSize int, flushFn func(ctx context.Context, items []T) error) *TableBuffer[T] {
	return &TableBuffer[T]{
		items:     make([]T, 0, batchSize),
		batchSize: batchSize,
		flushFn:   flushFn,
		tableName: name,
	}
}

// Add appends an item to the buffer. If the buffer reaches batchSize it is
// automatically flushed in a background goroutine to avoid blocking the caller.
func (b *TableBuffer[T]) Add(item T) {
	b.mu.Lock()
	b.items = append(b.items, item)
	shouldFlush := len(b.items) >= b.batchSize
	b.mu.Unlock()

	if shouldFlush {
		go func() {
			if err := b.Flush(context.Background()); err != nil {
				log.Error().Err(err).Str("table", b.tableName).Msg("auto-flush failed")
			}
		}()
	}
}

// Flush writes all buffered items to DuckDB and resets the buffer.
func (b *TableBuffer[T]) Flush(ctx context.Context) error {
	b.mu.Lock()
	if len(b.items) == 0 {
		b.mu.Unlock()
		return nil
	}
	// Swap out the slice so we can release the lock while flushing.
	batch := b.items
	b.items = make([]T, 0, b.batchSize)
	b.mu.Unlock()

	start := time.Now()
	if err := b.flushFn(ctx, batch); err != nil {
		return fmt.Errorf("flush %s: %w", b.tableName, err)
	}
	log.Debug().
		Str("table", b.tableName).
		Int("rows", len(batch)).
		Dur("duration", time.Since(start)).
		Msg("flushed buffer")
	return nil
}

// Len returns the current number of buffered items.
func (b *TableBuffer[T]) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.items)
}

// ---------------------------------------------------------------------------
// Row types — one per buffered table, plus SessionHistoryRow (unbuffered).
// Field names use short forms that match the ingestion writer's usage.
// ---------------------------------------------------------------------------

// TelemetryRow matches the legacy "telemetry" table columns.
type TelemetryRow struct {
	Speed    float64
	Gear     int
	Throttle float64
	Brake    float64
	Steering float64
	RPM      int
	WearFL   float64
	WearFR   float64
	WearRL   float64
	WearRR   float64
	Lap      int
	TrackPos float64
	Sector   int
}

// SessionRow matches the "session_data" table columns.
type SessionRow struct {
	Weather      int
	TrackTemp    int
	AirTemp      int
	TotalLaps    int
	TrackLength  int
	SessionType  int
	TrackID      int
	TimeLeft     int
	SafetyCar    int
	RainPct      int
	PitIdealLap  int
	PitLatestLap int
}

// LapDataRow matches the "lap_data" table columns.
type LapDataRow struct {
	CarIndex      int
	LastLapMs     int
	CurrentLapMs  int
	Sector1Ms     int
	Sector2Ms     int
	Position      int
	LapNum        int
	PitStatus     int
	NumPitStops   int
	Sector        int
	Penalties     int
	DriverStatus  int
	ResultStatus  int
	DeltaFrontMs  int
	DeltaLeaderMs int
	SpeedTrap     float64
	GridPosition  int
}

// CarStatusRow matches the "car_status" table columns.
type CarStatusRow struct {
	CarIndex   int
	FuelMix    int
	FuelInTank float64
	FuelRemLaps float64
	ERSStore   float64
	ERSMode    int
	ERSMguk    float64
	ERSMguh    float64
	ERSDeployed float64
	ActualComp int
	VisualComp int
	TyresAge   int
	DRSAllowed int
	FIAFlags   int
	PowerICE   float64
	PowerMGUK  float64
}

// CarDamageRow matches the "car_damage" table columns.
type CarDamageRow struct {
	CarIndex int
	WearRL   float64
	WearRR   float64
	WearFL   float64
	WearFR   float64
	DmgRL    int
	DmgRR    int
	DmgFL    int
	DmgFR    int
	FLWing   int
	FRWing   int
	RearWing int
	Floor    int
	Diffuser int
	Sidepod  int
	Gearbox  int
	Engine   int
	MGUHWear int
	ESWear   int
	CEWear   int
	ICEWear  int
	MGUKWear int
	TCWear   int
	DRSFault int
	ERSFault int
}

// TelemetryExtRow matches the "car_telemetry_ext" table columns.
type TelemetryExtRow struct {
	CarIndex        int
	BrakeTempRL     int
	BrakeTempRR     int
	BrakeTempFL     int
	BrakeTempFR     int
	TyreSurfTempRL  int
	TyreSurfTempRR  int
	TyreSurfTempFL  int
	TyreSurfTempFR  int
	TyreInnerTempRL int
	TyreInnerTempRR int
	TyreInnerTempFL int
	TyreInnerTempFR int
	EngineTemp      int
	TyrePressureRL  float64
	TyrePressureRR  float64
	TyrePressureFL  float64
	TyrePressureFR  float64
	DRS             int
	Clutch          int
	SuggestedGear   int
}

// RaceEventRow matches the "race_events" table columns.
type RaceEventRow struct {
	EventCode  string
	VehicleIdx int
	DetailText string
}

// MotionRow matches the "motion_data" table columns.
type MotionRow struct {
	CarIndex  int
	WorldPosX float64
	WorldPosY float64
	WorldPosZ float64
	GForceLat float64
	GForceLon float64
	GForceVer float64
	Yaw       float64
	Pitch     float64
	Roll      float64
}

// TelemetryHifreqRow is one ~10Hz wide sample of the player car. Captured
// inside handleCarTelemetry by reading the in-flight RaceState pointer (NOT
// the published cache snapshot — that lags one tick behind).
type TelemetryHifreqRow struct {
	SessionUID       uint64
	FrameID          int
	Lap              int
	TrackPosition    float64
	TotalDistance    float64
	CurrentLapTimeMs int
	PitStatus        int
	Throttle         float64
	Brake            float64
	Steering         float64
	Speed            int
	Gear             int
	EngineRPM        int
	DRS              int
	BrakeTempFL      int
	BrakeTempFR      int
	BrakeTempRL      int
	BrakeTempRR      int
	TyreSurfTempFL   int
	TyreSurfTempFR   int
	TyreSurfTempRL   int
	TyreSurfTempRR   int
	WorldPosX        float64
	WorldPosY        float64
	WorldPosZ        float64
	GForceLat        float64
	GForceLon        float64
	GForceVert       float64
	TyreInnerTempFL  int
	TyreInnerTempFR  int
	TyreInnerTempRL  int
	TyreInnerTempRR  int
	TyrePressureFL   float64
	TyrePressureFR   float64
	TyrePressureRL   float64
	TyrePressureRR   float64
	FuelInTank       float64
	ErsStoreEnergy   float64
	ErsDeployMode    int
	Clutch           int
	SuggestedGear    int
	BrakeBias        float64
	DiffOnThrottle   int
	EngineBraking    int
	DRSAllowed       int
	// Per-wheel sliding + surface channels. RL/RR/FL/FR ordering matches the
	// rest of this struct; MotionEx (slip) and CarTelemetry (surface) both
	// publish in that order.
	WheelSlipRatioFL float64
	WheelSlipRatioFR float64
	WheelSlipRatioRL float64
	WheelSlipRatioRR float64
	WheelSlipAngleFL float64
	WheelSlipAngleFR float64
	WheelSlipAngleRL float64
	WheelSlipAngleRR float64
	SurfaceTypeFL    int
	SurfaceTypeFR    int
	SurfaceTypeRL    int
	SurfaceTypeRR    int
}

// CarSetupRow matches the "car_setup" table — driver-controllable setup
// channels from F1 25 CarSetup packet (ID 5). Player-only, low frequency.
type CarSetupRow struct {
	CarIndex               int
	FrontWing              int
	RearWing               int
	OnThrottleDiff         int
	OffThrottleDiff        int
	FrontCamber            float64
	RearCamber             float64
	FrontToe               float64
	RearToe                float64
	FrontSuspension        int
	RearSuspension         int
	FrontAntiRollBar       int
	RearAntiRollBar        int
	FrontRideHeight        int
	RearRideHeight         int
	BrakePressure          int
	BrakeBias              int
	EngineBraking          int
	RearLeftTyrePressure   float64
	RearRightTyrePressure  float64
	FrontLeftTyrePressure  float64
	FrontRightTyrePressure float64
	Ballast                int
	FuelLoad               float64
}

// SessionHistoryRow matches the "session_history" table columns.
// This is not buffered (written directly via Storage.UpsertSessionHistory)
// but the type is used by the ingestion writer.
type SessionHistoryRow struct {
	SessionUID uint64
	CarIndex   int
	LapNum     int
	LapTimeMs  int
	S1Ms       int
	S2Ms       int
	S3Ms       int
	LapValid   int
}

// ---------------------------------------------------------------------------
// BufferSet — holds all eight table buffers
// ---------------------------------------------------------------------------

// BufferSet aggregates the batch buffers for every buffered table.
type BufferSet struct {
	Telemetry       *TableBuffer[TelemetryRow]
	Session         *TableBuffer[SessionRow]
	LapData         *TableBuffer[LapDataRow]
	CarStatus       *TableBuffer[CarStatusRow]
	CarDamage       *TableBuffer[CarDamageRow]
	TelemetryExt    *TableBuffer[TelemetryExtRow]
	RaceEvents      *TableBuffer[RaceEventRow]
	Motion          *TableBuffer[MotionRow]
	TelemetryHifreq *TableBuffer[TelemetryHifreqRow]
	CarSetup        *TableBuffer[CarSetupRow]
}

// NewBufferSet creates all eight table buffers wired to the given DuckDB writer
// connection with the specified batch size.
func NewBufferSet(db *sql.DB, batchSize int) *BufferSet {
	return &BufferSet{
		Telemetry:       NewTableBuffer[TelemetryRow]("telemetry", batchSize, makeTelemetryFlusher(db)),
		Session:         NewTableBuffer[SessionRow]("session_data", batchSize, makeSessionFlusher(db)),
		LapData:         NewTableBuffer[LapDataRow]("lap_data", batchSize, makeLapDataFlusher(db)),
		CarStatus:       NewTableBuffer[CarStatusRow]("car_status", batchSize, makeCarStatusFlusher(db)),
		CarDamage:       NewTableBuffer[CarDamageRow]("car_damage", batchSize, makeCarDamageFlusher(db)),
		TelemetryExt:    NewTableBuffer[TelemetryExtRow]("car_telemetry_ext", batchSize, makeTelemetryExtFlusher(db)),
		RaceEvents:      NewTableBuffer[RaceEventRow]("race_events", batchSize, makeRaceEventFlusher(db)),
		Motion:          NewTableBuffer[MotionRow]("motion_data", batchSize, makeMotionFlusher(db)),
		TelemetryHifreq: NewTableBuffer[TelemetryHifreqRow]("telemetry_hifreq", batchSize, makeTelemetryHifreqFlusher(db)),
		CarSetup:        NewTableBuffer[CarSetupRow]("car_setup", batchSize, makeCarSetupFlusher(db)),
	}
}

// FlushAll flushes every buffer. Errors are collected; the first is returned.
func (bs *BufferSet) FlushAll(ctx context.Context) error {
	var firstErr error
	collect := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	collect(bs.Telemetry.Flush(ctx))
	collect(bs.Session.Flush(ctx))
	collect(bs.LapData.Flush(ctx))
	collect(bs.CarStatus.Flush(ctx))
	collect(bs.CarDamage.Flush(ctx))
	collect(bs.TelemetryExt.Flush(ctx))
	collect(bs.RaceEvents.Flush(ctx))
	collect(bs.Motion.Flush(ctx))
	collect(bs.TelemetryHifreq.Flush(ctx))
	collect(bs.CarSetup.Flush(ctx))
	return firstErr
}

// ---------------------------------------------------------------------------
// Flush functions — one per table. Each performs a bulk INSERT inside a tx.
// ---------------------------------------------------------------------------

func makeTelemetryFlusher(db *sql.DB) func(ctx context.Context, items []TelemetryRow) error {
	return func(ctx context.Context, items []TelemetryRow) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback() //nolint:errcheck
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO telemetry
			(speed, gear, throttle, brake, steering, engine_rpm,
			 tire_wear_fl, tire_wear_fr, tire_wear_rl, tire_wear_rr,
			 lap, track_position, sector)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, r := range items {
			if _, err := stmt.ExecContext(ctx,
				r.Speed, r.Gear, r.Throttle, r.Brake, r.Steering, r.RPM,
				r.WearFL, r.WearFR, r.WearRL, r.WearRR,
				r.Lap, r.TrackPos, r.Sector,
			); err != nil {
				return err
			}
		}
		return tx.Commit()
	}
}

func makeSessionFlusher(db *sql.DB) func(ctx context.Context, items []SessionRow) error {
	return func(ctx context.Context, items []SessionRow) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback() //nolint:errcheck
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO session_data
			(weather, track_temperature, air_temperature,
			 total_laps, track_length, session_type, track_id,
			 session_time_left, safety_car_status, rain_percentage,
			 pit_stop_window_ideal_lap, pit_stop_window_latest_lap)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, r := range items {
			if _, err := stmt.ExecContext(ctx,
				r.Weather, r.TrackTemp, r.AirTemp,
				r.TotalLaps, r.TrackLength, r.SessionType, r.TrackID,
				r.TimeLeft, r.SafetyCar, r.RainPct,
				r.PitIdealLap, r.PitLatestLap,
			); err != nil {
				return err
			}
		}
		return tx.Commit()
	}
}

func makeLapDataFlusher(db *sql.DB) func(ctx context.Context, items []LapDataRow) error {
	return func(ctx context.Context, items []LapDataRow) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback() //nolint:errcheck
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO lap_data
			(car_index, last_lap_time_in_ms, current_lap_time_in_ms,
			 sector1_time_in_ms, sector2_time_in_ms,
			 car_position, current_lap_num, pit_status,
			 num_pit_stops, sector, penalties,
			 driver_status, result_status,
			 delta_to_car_in_front_in_ms, delta_to_race_leader_in_ms,
			 speed_trap_fastest_speed, grid_position)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, r := range items {
			if _, err := stmt.ExecContext(ctx,
				r.CarIndex, r.LastLapMs, r.CurrentLapMs,
				r.Sector1Ms, r.Sector2Ms,
				r.Position, r.LapNum, r.PitStatus,
				r.NumPitStops, r.Sector, r.Penalties,
				r.DriverStatus, r.ResultStatus,
				r.DeltaFrontMs, r.DeltaLeaderMs,
				r.SpeedTrap, r.GridPosition,
			); err != nil {
				return err
			}
		}
		return tx.Commit()
	}
}

func makeCarStatusFlusher(db *sql.DB) func(ctx context.Context, items []CarStatusRow) error {
	return func(ctx context.Context, items []CarStatusRow) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback() //nolint:errcheck
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO car_status
			(car_index, fuel_mix, fuel_in_tank,
			 fuel_remaining_laps, ers_store_energy, ers_deploy_mode,
			 ers_harvested_this_lap_mguk, ers_harvested_this_lap_mguh,
			 ers_deployed_this_lap, actual_tyre_compound,
			 visual_tyre_compound, tyres_age_laps, drs_allowed,
			 vehicle_fia_flags, engine_power_ice, engine_power_mguk)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, r := range items {
			if _, err := stmt.ExecContext(ctx,
				r.CarIndex, r.FuelMix, r.FuelInTank,
				r.FuelRemLaps, r.ERSStore, r.ERSMode,
				r.ERSMguk, r.ERSMguh,
				r.ERSDeployed, r.ActualComp,
				r.VisualComp, r.TyresAge, r.DRSAllowed,
				r.FIAFlags, r.PowerICE, r.PowerMGUK,
			); err != nil {
				return err
			}
		}
		return tx.Commit()
	}
}

func makeCarDamageFlusher(db *sql.DB) func(ctx context.Context, items []CarDamageRow) error {
	return func(ctx context.Context, items []CarDamageRow) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback() //nolint:errcheck
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO car_damage
			(car_index,
			 tyres_wear_rl, tyres_wear_rr, tyres_wear_fl, tyres_wear_fr,
			 tyres_damage_rl, tyres_damage_rr, tyres_damage_fl, tyres_damage_fr,
			 front_left_wing_damage, front_right_wing_damage,
			 rear_wing_damage, floor_damage, diffuser_damage,
			 sidepod_damage, gear_box_damage, engine_damage,
			 engine_mguh_wear, engine_es_wear, engine_ce_wear,
			 engine_ice_wear, engine_mguk_wear, engine_tc_wear,
			 drs_fault, ers_fault)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, r := range items {
			if _, err := stmt.ExecContext(ctx,
				r.CarIndex,
				r.WearRL, r.WearRR, r.WearFL, r.WearFR,
				r.DmgRL, r.DmgRR, r.DmgFL, r.DmgFR,
				r.FLWing, r.FRWing,
				r.RearWing, r.Floor, r.Diffuser,
				r.Sidepod, r.Gearbox, r.Engine,
				r.MGUHWear, r.ESWear, r.CEWear,
				r.ICEWear, r.MGUKWear, r.TCWear,
				r.DRSFault, r.ERSFault,
			); err != nil {
				return err
			}
		}
		return tx.Commit()
	}
}

func makeTelemetryExtFlusher(db *sql.DB) func(ctx context.Context, items []TelemetryExtRow) error {
	return func(ctx context.Context, items []TelemetryExtRow) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback() //nolint:errcheck
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO car_telemetry_ext
			(car_index,
			 brakes_temp_rl, brakes_temp_rr, brakes_temp_fl, brakes_temp_fr,
			 tyres_surface_temp_rl, tyres_surface_temp_rr,
			 tyres_surface_temp_fl, tyres_surface_temp_fr,
			 tyres_inner_temp_rl, tyres_inner_temp_rr,
			 tyres_inner_temp_fl, tyres_inner_temp_fr,
			 engine_temperature,
			 tyres_pressure_rl, tyres_pressure_rr,
			 tyres_pressure_fl, tyres_pressure_fr,
			 drs, clutch, suggested_gear)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, r := range items {
			if _, err := stmt.ExecContext(ctx,
				r.CarIndex,
				r.BrakeTempRL, r.BrakeTempRR, r.BrakeTempFL, r.BrakeTempFR,
				r.TyreSurfTempRL, r.TyreSurfTempRR,
				r.TyreSurfTempFL, r.TyreSurfTempFR,
				r.TyreInnerTempRL, r.TyreInnerTempRR,
				r.TyreInnerTempFL, r.TyreInnerTempFR,
				r.EngineTemp,
				r.TyrePressureRL, r.TyrePressureRR,
				r.TyrePressureFL, r.TyrePressureFR,
				r.DRS, r.Clutch, r.SuggestedGear,
			); err != nil {
				return err
			}
		}
		return tx.Commit()
	}
}

func makeRaceEventFlusher(db *sql.DB) func(ctx context.Context, items []RaceEventRow) error {
	return func(ctx context.Context, items []RaceEventRow) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback() //nolint:errcheck
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO race_events
			(event_code, vehicle_idx, detail_text)
			VALUES (?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, r := range items {
			if _, err := stmt.ExecContext(ctx,
				r.EventCode, r.VehicleIdx, r.DetailText,
			); err != nil {
				return err
			}
		}
		return tx.Commit()
	}
}

func makeTelemetryHifreqFlusher(db *sql.DB) func(ctx context.Context, items []TelemetryHifreqRow) error {
	return func(ctx context.Context, items []TelemetryHifreqRow) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback() //nolint:errcheck
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO telemetry_hifreq
			(session_uid, frame_id, lap, track_position, total_distance,
			 current_lap_time_ms, pit_status,
			 throttle, brake, steering, speed, gear, engine_rpm, drs,
			 brake_temp_fl, brake_temp_fr, brake_temp_rl, brake_temp_rr,
			 tyre_surf_temp_fl, tyre_surf_temp_fr, tyre_surf_temp_rl, tyre_surf_temp_rr,
			 world_pos_x, world_pos_y, world_pos_z,
			 g_force_lat, g_force_lon, g_force_vert,
			 tyre_inner_temp_fl, tyre_inner_temp_fr, tyre_inner_temp_rl, tyre_inner_temp_rr,
			 tyre_pressure_fl, tyre_pressure_fr, tyre_pressure_rl, tyre_pressure_rr,
			 fuel_in_tank, ers_store_energy, ers_deploy_mode,
			 clutch, suggested_gear,
			 brake_bias, diff_on_throttle, engine_braking, drs_allowed,
			 wheel_slip_ratio_fl, wheel_slip_ratio_fr, wheel_slip_ratio_rl, wheel_slip_ratio_rr,
			 wheel_slip_angle_fl, wheel_slip_angle_fr, wheel_slip_angle_rl, wheel_slip_angle_rr,
			 surface_type_fl, surface_type_fr, surface_type_rl, surface_type_rr)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		// F1 session UIDs are random 64-bit; the high bit trips
		// database/sql's uint64 check. Bind via *big.Int → HUGEINT, same
		// trick as UpsertSessionHistory.
		uid := new(big.Int)
		for _, r := range items {
			uid.SetUint64(r.SessionUID)
			if _, err := stmt.ExecContext(ctx,
				uid, r.FrameID, r.Lap, r.TrackPosition, r.TotalDistance,
				r.CurrentLapTimeMs, r.PitStatus,
				r.Throttle, r.Brake, r.Steering, r.Speed, r.Gear, r.EngineRPM, r.DRS,
				r.BrakeTempFL, r.BrakeTempFR, r.BrakeTempRL, r.BrakeTempRR,
				r.TyreSurfTempFL, r.TyreSurfTempFR, r.TyreSurfTempRL, r.TyreSurfTempRR,
				r.WorldPosX, r.WorldPosY, r.WorldPosZ,
				r.GForceLat, r.GForceLon, r.GForceVert,
				r.TyreInnerTempFL, r.TyreInnerTempFR, r.TyreInnerTempRL, r.TyreInnerTempRR,
				r.TyrePressureFL, r.TyrePressureFR, r.TyrePressureRL, r.TyrePressureRR,
				r.FuelInTank, r.ErsStoreEnergy, r.ErsDeployMode,
				r.Clutch, r.SuggestedGear,
				r.BrakeBias, r.DiffOnThrottle, r.EngineBraking, r.DRSAllowed,
				r.WheelSlipRatioFL, r.WheelSlipRatioFR, r.WheelSlipRatioRL, r.WheelSlipRatioRR,
				r.WheelSlipAngleFL, r.WheelSlipAngleFR, r.WheelSlipAngleRL, r.WheelSlipAngleRR,
				r.SurfaceTypeFL, r.SurfaceTypeFR, r.SurfaceTypeRL, r.SurfaceTypeRR,
			); err != nil {
				return err
			}
		}
		return tx.Commit()
	}
}

func makeCarSetupFlusher(db *sql.DB) func(ctx context.Context, items []CarSetupRow) error {
	return func(ctx context.Context, items []CarSetupRow) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback() //nolint:errcheck
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO car_setup
			(car_index,
			 front_wing, rear_wing,
			 on_throttle_diff, off_throttle_diff,
			 front_camber, rear_camber, front_toe, rear_toe,
			 front_suspension, rear_suspension,
			 front_anti_roll_bar, rear_anti_roll_bar,
			 front_ride_height, rear_ride_height,
			 brake_pressure, brake_bias, engine_braking,
			 rear_left_tyre_pressure, rear_right_tyre_pressure,
			 front_left_tyre_pressure, front_right_tyre_pressure,
			 ballast, fuel_load)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, r := range items {
			if _, err := stmt.ExecContext(ctx,
				r.CarIndex,
				r.FrontWing, r.RearWing,
				r.OnThrottleDiff, r.OffThrottleDiff,
				r.FrontCamber, r.RearCamber, r.FrontToe, r.RearToe,
				r.FrontSuspension, r.RearSuspension,
				r.FrontAntiRollBar, r.RearAntiRollBar,
				r.FrontRideHeight, r.RearRideHeight,
				r.BrakePressure, r.BrakeBias, r.EngineBraking,
				r.RearLeftTyrePressure, r.RearRightTyrePressure,
				r.FrontLeftTyrePressure, r.FrontRightTyrePressure,
				r.Ballast, r.FuelLoad,
			); err != nil {
				return err
			}
		}
		return tx.Commit()
	}
}

func makeMotionFlusher(db *sql.DB) func(ctx context.Context, items []MotionRow) error {
	return func(ctx context.Context, items []MotionRow) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback() //nolint:errcheck
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO motion_data
			(car_index,
			 world_position_x, world_position_y, world_position_z,
			 g_force_lateral, g_force_longitudinal, g_force_vertical,
			 yaw, pitch, roll)
			VALUES (?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, r := range items {
			if _, err := stmt.ExecContext(ctx,
				r.CarIndex,
				r.WorldPosX, r.WorldPosY, r.WorldPosZ,
				r.GForceLat, r.GForceLon, r.GForceVer,
				r.Yaw, r.Pitch, r.Roll,
			); err != nil {
				return err
			}
		}
		return tx.Commit()
	}
}
