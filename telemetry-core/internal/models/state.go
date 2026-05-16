package models

// RaceState is the complete live snapshot of the current telemetry state.
// Updated atomically on every parsed packet. The insight engine reads this
// (never DuckDB) for sub-microsecond evaluations.
type RaceState struct {
	// Header info. SessionUID marshals as a string because uint64 values
	// commonly exceed JavaScript's safe integer range (2^53) — any JS
	// client (the dashboard, the Live agent's tool args) would otherwise
	// round and break lookups against DuckDB rows.
	SessionUID     uint64  `json:"session_uid,string"`
	FrameID        uint32  `json:"frame_id"`
	PlayerCarIndex uint8   `json:"player_car_index"`
	SessionTime    float32 `json:"session_time"`

	// Car telemetry (player car)
	Speed      uint16  `json:"speed"`       // km/h
	Throttle   float32 `json:"throttle"`    // 0.0–1.0
	Brake      float32 `json:"brake"`       // 0.0–1.0
	Steering   float32 `json:"steering"`    // -1.0 to 1.0
	Gear       int8    `json:"gear"`        // -1=R, 0=N, 1-8
	EngineRPM  uint16  `json:"engine_rpm"`
	DRS        uint8   `json:"drs"`
	EngineTemp uint16  `json:"engine_temperature"`

	// Tire temperatures (RL, RR, FL, FR)
	BrakesTemp      [4]uint16  `json:"brakes_temp"`
	TyresSurfTemp   [4]uint8   `json:"tyres_surface_temp"`
	TyresInnerTemp  [4]uint8   `json:"tyres_inner_temp"`
	TyresPressure   [4]float32 `json:"tyres_pressure"`
	SuggestedGear   int8       `json:"suggested_gear"`

	// Tire wear & damage (RL, RR, FL, FR)
	TyresWear   [4]float32 `json:"tyres_wear"`   // percentage
	TyresDamage [4]uint8   `json:"tyres_damage"` // percentage
	BrakesDmg   [4]uint8   `json:"brakes_damage"`

	// Aero/body damage
	FrontLeftWingDmg  uint8 `json:"front_left_wing_damage"`
	FrontRightWingDmg uint8 `json:"front_right_wing_damage"`
	RearWingDmg       uint8 `json:"rear_wing_damage"`
	FloorDmg          uint8 `json:"floor_damage"`
	DiffuserDmg       uint8 `json:"diffuser_damage"`
	SidepodDmg        uint8 `json:"sidepod_damage"`
	GearBoxDmg        uint8 `json:"gear_box_damage"`
	EngineDmg         uint8 `json:"engine_damage"`
	DRSFault          uint8 `json:"drs_fault"`
	ERSFault          uint8 `json:"ers_fault"`

	// Lap data (player car)
	CurrentLap        uint8   `json:"current_lap"`
	Position          uint8   `json:"position"`
	Sector            uint8   `json:"sector"`           // 0,1,2
	LapDistance       float32 `json:"lap_distance"`     // metres
	TotalDistance     float32 `json:"total_distance"`
	LastLapTimeMs     uint32  `json:"last_lap_time_ms"`
	CurrentLapTimeMs  uint32  `json:"current_lap_time_ms"`
	PitStatus         uint8   `json:"pit_status"`       // 0=none, 1=pitting, 2=in pit
	NumPitStops       uint8   `json:"num_pit_stops"`
	GridPosition      uint8   `json:"grid_position"`
	DriverStatus      uint8   `json:"driver_status"`
	DeltaToFrontMs    int32   `json:"delta_to_front_ms"`
	DeltaToLeaderMs   int32   `json:"delta_to_leader_ms"`

	// Fuel & ERS
	FuelMix           uint8   `json:"fuel_mix"`
	FuelInTank        float32 `json:"fuel_in_tank"`
	FuelRemainingLaps float32 `json:"fuel_remaining_laps"`
	ERSStoreEnergy    float32 `json:"ers_store_energy"`    // Joules (4MJ max)
	ERSDeployMode     uint8   `json:"ers_deploy_mode"`
	ERSHarvestedMGUK  float32 `json:"ers_harvested_mguk"`
	ERSHarvestedMGUH  float32 `json:"ers_harvested_mguh"`
	ERSDeployedLap    float32 `json:"ers_deployed_this_lap"`
	ActualCompound    uint8   `json:"actual_tyre_compound"`
	VisualCompound    uint8   `json:"visual_tyre_compound"`
	TyresAgeLaps      uint8   `json:"tyres_age_laps"`
	DRSAllowed        uint8   `json:"drs_allowed"`
	VehicleFIAFlags   int8    `json:"vehicle_fia_flags"`

	// Session
	Weather           uint8   `json:"weather"`
	TrackTemp         int8    `json:"track_temperature"`
	AirTemp           int8    `json:"air_temperature"`
	TotalLaps         uint8   `json:"total_laps"`
	TrackLength       uint16  `json:"track_length"`
	SessionType       uint8   `json:"session_type"`
	TrackID           int8    `json:"track_id"`
	SessionTimeLeft   uint16  `json:"session_time_left"`
	SafetyCarStatus   uint8   `json:"safety_car_status"`
	RainPercentage    uint8   `json:"rain_percentage"`      // nearest forecast
	PitWindowIdeal    uint8   `json:"pit_window_ideal_lap"`
	PitWindowLatest   uint8   `json:"pit_window_latest_lap"`

	// Weather forecast (first 5 samples for quick insight checks)
	WeatherForecasts [5]WeatherSample `json:"weather_forecasts"`

	// Motion (player car)
	WorldPosX       float32 `json:"world_pos_x"`
	WorldPosY       float32 `json:"world_pos_y"`
	WorldPosZ       float32 `json:"world_pos_z"`
	GForceLateral   float32 `json:"g_force_lateral"`
	GForceLongitude float32 `json:"g_force_longitudinal"`
	GForceVertical  float32 `json:"g_force_vertical"`
	Yaw             float32 `json:"yaw"`
	Pitch           float32 `json:"pitch"`
	Roll            float32 `json:"roll"`

	// Latest event
	LastEventCode    string `json:"last_event_code"`
	LastEventIdx     uint8  `json:"last_event_vehicle_idx"`
	LastButtonStatus uint32 `json:"last_button_status"` // bitmask from BUTN events

	// Race lifecycle flags. Sticky for the rest of the session once set —
	// the ingestion writer flips RaceFinished true on the first CHQF event
	// that names the player car (or on SEND) and records the snapshot of
	// the final classification position at that moment. Downstream
	// consumers (race-progress panel, emitRaceFinished, MCP get_race_phase)
	// read these to drive end-of-race behaviour without re-deriving from
	// LastEventCode (which is volatile).
	RaceFinished        bool  `json:"race_finished"`
	FinalClassification uint8 `json:"final_classification"` // player's position at CHQF

	// Track geometry (from F1 25 SessionPacket — sectors are lap-distance offsets)
	Sector2DistStart float32 `json:"sector2_dist_start"`
	Sector3DistStart float32 `json:"sector3_dist_start"`

	// Per-car position snapshot for the full grid. Filled by the lap_data
	// and motion handlers — entries with ResultStatus < 2 are inactive
	// slots and should be ignored. Indexed by car_index (matches F1 25
	// packet ordering).
	GridPositions [22]GridPosition `json:"grid_positions"`

	// Driver-controllable setup state. Populated from CarSetup pkt 5.
	// Always-on snapshot — read by /api/state/driver_settings + brain
	// snapshot "Driver Settings" section.
	DriverSettings DriverSettings `json:"driver_settings"`

	// DRS usage accumulators reset each lap. Accumulated tick-by-tick in
	// handleCarTelemetry: AvailableThisLap whenever DRSAllowed != 0,
	// UsedThisLap whenever the actual DRS flag is on. Powers the
	// "available but unused" metric on /api/state/driver_settings.
	DRSAvailableThisLap float32 `json:"drs_available_this_lap_s"`
	DRSUsedThisLap      float32 `json:"drs_used_this_lap_s"`
	LastDRSSampleTime   float32 `json:"-"` // session_time of last sample, internal
	LastDRSLap          uint8   `json:"-"` // for rollover detection

	// Player-only wheel-dynamics + surface channels. WheelSlipRatio /
	// WheelSlipAngle come from MotionEx (packet 13, player car only),
	// SurfaceType comes from each CarTelemetry sample. Stored here so the
	// hi-freq writer can merge them into a single telemetry_hifreq row
	// without an extra cross-table join. Wheel order matches F1 25:
	// RL, RR, FL, FR. SurfaceType values are an enum (0=tarmac, 1=rumblestrip,
	// 2=concrete, 3=rock, 4=gravel, 5=mud, 6=sand, 7=grass, 8=water, etc.).
	WheelSlipRatio [4]float32 `json:"wheel_slip_ratio"`
	WheelSlipAngle [4]float32 `json:"wheel_slip_angle"`
	SurfaceType    [4]uint8   `json:"surface_type"`
}

// DriverSettings holds the driver-controllable cockpit + setup-screen state
// from F1 25 CarSetup packet. uint8 values are the raw F1 25 setup units;
// brake_bias is a percentage (0-100, higher = more forward); diff values are
// percentages (lock = high, open = low); engine_braking is 0-100.
type DriverSettings struct {
	FrontWing       uint8  `json:"front_wing"`
	RearWing        uint8  `json:"rear_wing"`
	OnThrottleDiff  uint8  `json:"on_throttle_diff"`
	OffThrottleDiff uint8  `json:"off_throttle_diff"`
	EngineBraking   uint8  `json:"engine_braking"`
	BrakePressure   uint8  `json:"brake_pressure"`
	BrakeBias       uint8  `json:"brake_bias"`
	Ballast         uint8  `json:"ballast"`
	FuelLoad        float32 `json:"fuel_load"`
	// Tyre pressures from setup screen (cold pressures); the in-game live
	// pressures live on TyresPressure higher up in RaceState.
	FrontLeftTyrePressure  float32 `json:"front_left_tyre_pressure"`
	FrontRightTyrePressure float32 `json:"front_right_tyre_pressure"`
	RearLeftTyrePressure   float32 `json:"rear_left_tyre_pressure"`
	RearRightTyrePressure  float32 `json:"rear_right_tyre_pressure"`
	// Ring buffer of the most recent setting changes — pushed by the
	// CarSetup handler when any monitored field changes vs the previous
	// packet. Capped at 8; older entries are overwritten.
	RecentChanges    [8]SettingChange `json:"recent_changes"`
	RecentChangesLen uint8            `json:"recent_changes_len"`
	// Internal: monotonic write head into RecentChanges so we keep the
	// buffer chronological without shuffling on every write.
	RecentChangesHead uint8 `json:"-"`
}

// SettingChange records one monitored-channel transition. Used by Live + the
// pi-agent setup specialist to spot "what did the driver just touch?".
type SettingChange struct {
	SessionTime float32 `json:"session_time"`
	Lap         uint8   `json:"lap"`
	Channel     string  `json:"channel"`
	From        int     `json:"from"`
	To          int     `json:"to"`
}

// GridPosition is the per-car slot the cornerWatcher and track_position tool
// read against. Sourced from LapData (lap_distance, position, sector,
// statuses), Motion (world XYZ), and CarStatus (compound, age).
type GridPosition struct {
	CarIndex      uint8   `json:"car_index"`
	Position      uint8   `json:"position"`
	CurrentLap    uint8   `json:"current_lap"`
	Sector        uint8   `json:"sector"`        // 0..2
	LapDistance   float32 `json:"lap_distance"`  // metres into current lap
	TotalDistance float32 `json:"total_distance"`
	WorldPosX     float32 `json:"world_pos_x"`
	WorldPosY     float32 `json:"world_pos_y"`
	WorldPosZ     float32 `json:"world_pos_z"`
	PitStatus     uint8   `json:"pit_status"`
	DriverStatus  uint8   `json:"driver_status"`
	ResultStatus  uint8   `json:"result_status"` // <2 means slot inactive
	NumPitStops   uint8   `json:"num_pit_stops"`
	// Compound + age land here unconditionally on every car_status packet so
	// the live map / leaderboard reflect a pit-stop compound swap the moment
	// the game sends it (the DuckDB buffer is still rate-sampled).
	ActualCompound uint8 `json:"actual_tyre_compound"`
	VisualCompound uint8 `json:"visual_tyre_compound"`
	TyresAgeLaps   uint8 `json:"tyres_age_laps"`
}

// WeatherSample is a compact forecast entry for the insight engine.
type WeatherSample struct {
	TimeOffset     uint8 `json:"time_offset"`     // minutes ahead
	Weather        uint8 `json:"weather"`          // 0=clear..5=storm
	RainPercentage uint8 `json:"rain_percentage"`
}

// IsRaceSession reports whether the F1 25 SessionType enum value names a race
// (10=Race, 11=Race2, 12=Race3). Exported so non-insights packages (ingestion
// writer, brain serializer, MCP tools) can branch on race-only logic without
// re-implementing the magic numbers.
func IsRaceSession(t uint8) bool { return t >= 10 && t <= 12 }

// RacePhase is the derived lifecycle bucket the LLM keys behaviour off. Not
// stored on the packet wire — computed lazily from the live RaceState. The
// closed set lets the brain snapshot, BuildContext, MCP tools, and the Live
// system prompt share one vocabulary.
type RacePhase string

const (
	PhaseUnknown    RacePhase = "unknown"
	PhaseGrid       RacePhase = "grid"        // race session, CurrentLap == 0, no LGOT yet
	PhaseFormation  RacePhase = "formation"   // SafetyCarStatus == 3 (formation lap)
	PhaseLightsOut  RacePhase = "lights_out"  // LGOT just observed, first ~30s of lap 1
	PhaseRacing     RacePhase = "racing"      // mid-race, regular running
	PhaseFinalLap   RacePhase = "final_lap"   // CurrentLap == TotalLaps and not yet RaceFinished
	PhaseFinished   RacePhase = "finished"    // RaceFinished latched true
	PhaseNonRace    RacePhase = "non_race"    // Practice, Qual, Time Trial — no lifecycle drama
)

// Phase returns the current RacePhase derived from the live state. Pure
// function over RaceState fields; safe to call from any goroutine that holds
// a snapshot.
func (s *RaceState) Phase() RacePhase {
	if s == nil {
		return PhaseUnknown
	}
	if !IsRaceSession(s.SessionType) {
		return PhaseNonRace
	}
	if s.RaceFinished {
		return PhaseFinished
	}
	if s.SafetyCarStatus == 3 {
		return PhaseFormation
	}
	if s.CurrentLap == 0 {
		return PhaseGrid
	}
	if s.LastEventCode == "LGOT" {
		return PhaseLightsOut
	}
	if s.TotalLaps > 0 && s.CurrentLap >= s.TotalLaps {
		return PhaseFinalLap
	}
	return PhaseRacing
}

// LapsRemaining returns total - current, clamped to >= 0. Returns 0 outside
// of race sessions or when TotalLaps is unknown.
func (s *RaceState) LapsRemaining() int {
	if s == nil || !IsRaceSession(s.SessionType) || s.TotalLaps == 0 {
		return 0
	}
	rem := int(s.TotalLaps) - int(s.CurrentLap)
	if rem < 0 {
		return 0
	}
	return rem
}
