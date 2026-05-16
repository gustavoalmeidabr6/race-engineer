package storage

import "database/sql"

// schemaSQL contains all CREATE TABLE IF NOT EXISTS statements for the 10 DuckDB tables.
// These match the Python telemetry schema exactly.
const schemaSQL = `
-- Table 1: telemetry (legacy backward-compat)
CREATE TABLE IF NOT EXISTS telemetry (
    timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    speed DOUBLE, gear INTEGER, throttle DOUBLE, brake DOUBLE, steering DOUBLE,
    engine_rpm INTEGER,
    tire_wear_fl DOUBLE, tire_wear_fr DOUBLE, tire_wear_rl DOUBLE, tire_wear_rr DOUBLE,
    lap INTEGER, track_position DOUBLE, sector INTEGER
);

-- Table 2: raw_packets (JSON dump)
CREATE TABLE IF NOT EXISTS raw_packets (
    timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    packet_id INTEGER, packet_name VARCHAR, session_uid UBIGINT,
    frame_id INTEGER, data JSON
);

-- Table 3: session_data
CREATE TABLE IF NOT EXISTS session_data (
    timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    weather INTEGER, track_temperature INTEGER, air_temperature INTEGER,
    total_laps INTEGER, track_length INTEGER, session_type INTEGER, track_id INTEGER,
    session_time_left INTEGER, safety_car_status INTEGER, rain_percentage INTEGER,
    pit_stop_window_ideal_lap INTEGER, pit_stop_window_latest_lap INTEGER
);

-- Table 4: lap_data
CREATE TABLE IF NOT EXISTS lap_data (
    timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    car_index INTEGER, last_lap_time_in_ms INTEGER, current_lap_time_in_ms INTEGER,
    sector1_time_in_ms INTEGER, sector2_time_in_ms INTEGER,
    car_position INTEGER, current_lap_num INTEGER, pit_status INTEGER,
    num_pit_stops INTEGER, sector INTEGER, penalties INTEGER,
    driver_status INTEGER, result_status INTEGER,
    delta_to_car_in_front_in_ms INTEGER, delta_to_race_leader_in_ms INTEGER,
    speed_trap_fastest_speed DOUBLE, grid_position INTEGER
);

-- Table 5: car_status
CREATE TABLE IF NOT EXISTS car_status (
    timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    car_index INTEGER, fuel_mix INTEGER, fuel_in_tank DOUBLE,
    fuel_remaining_laps DOUBLE, ers_store_energy DOUBLE, ers_deploy_mode INTEGER,
    ers_harvested_this_lap_mguk DOUBLE, ers_harvested_this_lap_mguh DOUBLE,
    ers_deployed_this_lap DOUBLE, actual_tyre_compound INTEGER,
    visual_tyre_compound INTEGER, tyres_age_laps INTEGER, drs_allowed INTEGER,
    vehicle_fia_flags INTEGER, engine_power_ice DOUBLE, engine_power_mguk DOUBLE
);

-- Table 6: car_damage
CREATE TABLE IF NOT EXISTS car_damage (
    timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    car_index INTEGER,
    tyres_wear_rl DOUBLE, tyres_wear_rr DOUBLE, tyres_wear_fl DOUBLE, tyres_wear_fr DOUBLE,
    tyres_damage_rl INTEGER, tyres_damage_rr INTEGER, tyres_damage_fl INTEGER, tyres_damage_fr INTEGER,
    front_left_wing_damage INTEGER, front_right_wing_damage INTEGER,
    rear_wing_damage INTEGER, floor_damage INTEGER, diffuser_damage INTEGER,
    sidepod_damage INTEGER, gear_box_damage INTEGER, engine_damage INTEGER,
    engine_mguh_wear INTEGER, engine_es_wear INTEGER, engine_ce_wear INTEGER,
    engine_ice_wear INTEGER, engine_mguk_wear INTEGER, engine_tc_wear INTEGER,
    drs_fault INTEGER, ers_fault INTEGER
);

-- Table 7: car_telemetry_ext
CREATE TABLE IF NOT EXISTS car_telemetry_ext (
    timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    car_index INTEGER,
    brakes_temp_rl INTEGER, brakes_temp_rr INTEGER, brakes_temp_fl INTEGER, brakes_temp_fr INTEGER,
    tyres_surface_temp_rl INTEGER, tyres_surface_temp_rr INTEGER,
    tyres_surface_temp_fl INTEGER, tyres_surface_temp_fr INTEGER,
    tyres_inner_temp_rl INTEGER, tyres_inner_temp_rr INTEGER,
    tyres_inner_temp_fl INTEGER, tyres_inner_temp_fr INTEGER,
    engine_temperature INTEGER,
    tyres_pressure_rl DOUBLE, tyres_pressure_rr DOUBLE,
    tyres_pressure_fl DOUBLE, tyres_pressure_fr DOUBLE,
    drs INTEGER, clutch INTEGER, suggested_gear INTEGER
);

-- Table 8: race_events
CREATE TABLE IF NOT EXISTS race_events (
    timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    event_code VARCHAR, vehicle_idx INTEGER, detail_text VARCHAR
);

-- Table 9: session_history
-- session_uid + car_index + lap_num is unique per lap so the SessionHistory
-- packet (re-broadcast every few seconds with the full per-car history) can
-- safely upsert without exploding the table.
CREATE TABLE IF NOT EXISTS session_history (
    timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    session_uid UBIGINT,
    car_index INTEGER, lap_num INTEGER, lap_time_in_ms INTEGER,
    sector1_time_in_ms INTEGER, sector2_time_in_ms INTEGER,
    sector3_time_in_ms INTEGER, lap_valid INTEGER
);

-- Table 10: motion_data
CREATE TABLE IF NOT EXISTS motion_data (
    timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    car_index INTEGER,
    world_position_x DOUBLE, world_position_y DOUBLE, world_position_z DOUBLE,
    g_force_lateral DOUBLE, g_force_longitudinal DOUBLE, g_force_vertical DOUBLE,
    yaw DOUBLE, pitch DOUBLE, roll DOUBLE
);

-- Table car_setup
-- Driver-controllable setup channels from F1 25 CarSetup packet (ID 5).
-- Player-only at ~2Hz native; sampled further by writer. Powers the
-- "are your current settings appropriate for how you're driving?" analytics
-- consumed by Live agent + pi-agent setup specialist.
CREATE TABLE IF NOT EXISTS car_setup (
    timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    car_index INTEGER,
    front_wing INTEGER, rear_wing INTEGER,
    on_throttle_diff INTEGER, off_throttle_diff INTEGER,
    front_camber DOUBLE, rear_camber DOUBLE,
    front_toe DOUBLE, rear_toe DOUBLE,
    front_suspension INTEGER, rear_suspension INTEGER,
    front_anti_roll_bar INTEGER, rear_anti_roll_bar INTEGER,
    front_ride_height INTEGER, rear_ride_height INTEGER,
    brake_pressure INTEGER, brake_bias INTEGER, engine_braking INTEGER,
    rear_left_tyre_pressure DOUBLE, rear_right_tyre_pressure DOUBLE,
    front_left_tyre_pressure DOUBLE, front_right_tyre_pressure DOUBLE,
    ballast INTEGER, fuel_load DOUBLE
);

-- Table 11: telemetry_hifreq
-- Wide ~30Hz row (stride=2 against the 60Hz UDP packets at default
-- HIFREQ_SAMPLE_RATE), player car only, bundling every channel the lap-trace
-- and brake-point coaching tools need so analysis SQL never has to cross-join
-- car_telemetry / car_telemetry_ext / motion_data on timestamp.
-- Bucketing is performed at query time on track_position; total_distance is
-- monotonic so it disambiguates samples that straddle the start/finish line.
CREATE TABLE IF NOT EXISTS telemetry_hifreq (
    timestamp           TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    session_uid         UBIGINT,
    frame_id            INTEGER,
    lap                 INTEGER,
    track_position      DOUBLE,
    total_distance      DOUBLE,
    current_lap_time_ms INTEGER,
    pit_status          INTEGER,
    throttle            DOUBLE,
    brake               DOUBLE,
    steering            DOUBLE,
    speed               INTEGER,
    gear                INTEGER,
    engine_rpm          INTEGER,
    drs                 INTEGER,
    brake_temp_fl       INTEGER, brake_temp_fr INTEGER,
    brake_temp_rl       INTEGER, brake_temp_rr INTEGER,
    tyre_surf_temp_fl   INTEGER, tyre_surf_temp_fr INTEGER,
    tyre_surf_temp_rl   INTEGER, tyre_surf_temp_rr INTEGER,
    world_pos_x         DOUBLE, world_pos_y DOUBLE, world_pos_z DOUBLE,
    g_force_lat         DOUBLE, g_force_lon DOUBLE, g_force_vert DOUBLE,
    tyre_inner_temp_fl  INTEGER, tyre_inner_temp_fr INTEGER,
    tyre_inner_temp_rl  INTEGER, tyre_inner_temp_rr INTEGER,
    tyre_pressure_fl    DOUBLE, tyre_pressure_fr DOUBLE,
    tyre_pressure_rl    DOUBLE, tyre_pressure_rr DOUBLE,
    fuel_in_tank        DOUBLE,
    ers_store_energy    DOUBLE,
    ers_deploy_mode     INTEGER,
    clutch              INTEGER,
    suggested_gear      INTEGER,
    brake_bias          DOUBLE,
    diff_on_throttle    INTEGER,
    engine_braking      INTEGER,
    drs_allowed         INTEGER,
    -- Per-wheel sliding + surface channels (player-only). slip_ratio /
    -- slip_angle are sourced from MotionEx (packet 13); surface_type is the
    -- F1 surface enum from CarTelemetry (0=tarmac, 4=gravel, 7=grass, ...).
    -- Lets analysis answer "was the car sliding here?" and "did I run wide
    -- onto grass?" without leaving telemetry_hifreq.
    wheel_slip_ratio_fl DOUBLE, wheel_slip_ratio_fr DOUBLE,
    wheel_slip_ratio_rl DOUBLE, wheel_slip_ratio_rr DOUBLE,
    wheel_slip_angle_fl DOUBLE, wheel_slip_angle_fr DOUBLE,
    wheel_slip_angle_rl DOUBLE, wheel_slip_angle_rr DOUBLE,
    surface_type_fl     TINYINT, surface_type_fr TINYINT,
    surface_type_rl     TINYINT, surface_type_rr TINYINT
);
`

// InitSchema creates all 10 tables if they do not already exist, then runs
// idempotent migrations so older DBs (created before session_uid existed on
// session_history) gain the new columns and unique index.
func InitSchema(db *sql.DB) error {
	if _, err := db.Exec(schemaSQL); err != nil {
		return err
	}
	// session_history was historically schema'd without session_uid /
	// timestamp; ADD COLUMN IF NOT EXISTS makes the migration idempotent.
	// CREATE UNIQUE INDEX IF NOT EXISTS keeps SessionHistory packet
	// re-broadcasts (same lap, fresh sector splits) collapsing into upserts
	// instead of growing the row count unboundedly.
	migrations := []string{
		`ALTER TABLE session_history ADD COLUMN IF NOT EXISTS timestamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP`,
		`ALTER TABLE session_history ADD COLUMN IF NOT EXISTS session_uid UBIGINT`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_session_history_unique ON session_history(session_uid, car_index, lap_num)`,
		// telemetry_hifreq: widen to cover every channel the live charts and
		// AI lap-trace tools need at ~30Hz (stride=2 against the 60Hz F1 25
		// CarTelemetry packet at default HIFREQ_SAMPLE_RATE). Old DBs created
		// before these columns existed must gain them transparently on startup.
		`ALTER TABLE telemetry_hifreq ADD COLUMN IF NOT EXISTS g_force_vert DOUBLE`,
		`ALTER TABLE telemetry_hifreq ADD COLUMN IF NOT EXISTS tyre_inner_temp_fl INTEGER`,
		`ALTER TABLE telemetry_hifreq ADD COLUMN IF NOT EXISTS tyre_inner_temp_fr INTEGER`,
		`ALTER TABLE telemetry_hifreq ADD COLUMN IF NOT EXISTS tyre_inner_temp_rl INTEGER`,
		`ALTER TABLE telemetry_hifreq ADD COLUMN IF NOT EXISTS tyre_inner_temp_rr INTEGER`,
		`ALTER TABLE telemetry_hifreq ADD COLUMN IF NOT EXISTS tyre_pressure_fl DOUBLE`,
		`ALTER TABLE telemetry_hifreq ADD COLUMN IF NOT EXISTS tyre_pressure_fr DOUBLE`,
		`ALTER TABLE telemetry_hifreq ADD COLUMN IF NOT EXISTS tyre_pressure_rl DOUBLE`,
		`ALTER TABLE telemetry_hifreq ADD COLUMN IF NOT EXISTS tyre_pressure_rr DOUBLE`,
		`ALTER TABLE telemetry_hifreq ADD COLUMN IF NOT EXISTS fuel_in_tank DOUBLE`,
		`ALTER TABLE telemetry_hifreq ADD COLUMN IF NOT EXISTS ers_store_energy DOUBLE`,
		`ALTER TABLE telemetry_hifreq ADD COLUMN IF NOT EXISTS ers_deploy_mode INTEGER`,
		`ALTER TABLE telemetry_hifreq ADD COLUMN IF NOT EXISTS clutch INTEGER`,
		`ALTER TABLE telemetry_hifreq ADD COLUMN IF NOT EXISTS suggested_gear INTEGER`,
		// Driver-setting + DRS-allowed channels stamped into hi-freq so
		// per-lap traces correlate with mid-stint cockpit tweaks and DRS
		// availability windows. Sourced from RaceState (CarSetup + CarStatus
		// land on different packets).
		`ALTER TABLE telemetry_hifreq ADD COLUMN IF NOT EXISTS brake_bias DOUBLE`,
		`ALTER TABLE telemetry_hifreq ADD COLUMN IF NOT EXISTS diff_on_throttle INTEGER`,
		`ALTER TABLE telemetry_hifreq ADD COLUMN IF NOT EXISTS engine_braking INTEGER`,
		`ALTER TABLE telemetry_hifreq ADD COLUMN IF NOT EXISTS drs_allowed INTEGER`,
		// Slip + surface channels — added so coaching tools can answer
		// "was I sliding?" and "did I run off track?" without leaving
		// the hi-freq table. Backfilled NULL for pre-existing rows.
		`ALTER TABLE telemetry_hifreq ADD COLUMN IF NOT EXISTS wheel_slip_ratio_fl DOUBLE`,
		`ALTER TABLE telemetry_hifreq ADD COLUMN IF NOT EXISTS wheel_slip_ratio_fr DOUBLE`,
		`ALTER TABLE telemetry_hifreq ADD COLUMN IF NOT EXISTS wheel_slip_ratio_rl DOUBLE`,
		`ALTER TABLE telemetry_hifreq ADD COLUMN IF NOT EXISTS wheel_slip_ratio_rr DOUBLE`,
		`ALTER TABLE telemetry_hifreq ADD COLUMN IF NOT EXISTS wheel_slip_angle_fl DOUBLE`,
		`ALTER TABLE telemetry_hifreq ADD COLUMN IF NOT EXISTS wheel_slip_angle_fr DOUBLE`,
		`ALTER TABLE telemetry_hifreq ADD COLUMN IF NOT EXISTS wheel_slip_angle_rl DOUBLE`,
		`ALTER TABLE telemetry_hifreq ADD COLUMN IF NOT EXISTS wheel_slip_angle_rr DOUBLE`,
		`ALTER TABLE telemetry_hifreq ADD COLUMN IF NOT EXISTS surface_type_fl TINYINT`,
		`ALTER TABLE telemetry_hifreq ADD COLUMN IF NOT EXISTS surface_type_fr TINYINT`,
		`ALTER TABLE telemetry_hifreq ADD COLUMN IF NOT EXISTS surface_type_rl TINYINT`,
		`ALTER TABLE telemetry_hifreq ADD COLUMN IF NOT EXISTS surface_type_rr TINYINT`,
	}
	for _, m := range migrations {
		if _, err := db.Exec(m); err != nil {
			return err
		}
	}
	return nil
}
