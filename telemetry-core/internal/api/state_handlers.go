package api

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/enums"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/state"
)

// state_handlers.go exposes 6 topic-clustered endpoints purpose-built for the
// Gemini Live race engineer. Each returns a small, decoded, headline-first
// payload — no raw enum integers, no JSON-blob telemetry dumps. The model
// reads the headline for an instant TL;DR and digs into structured fields
// for follow-ups. See gemini_live_service.py TOOLS array for the matching
// tool definitions.
//
// All handlers gracefully handle the empty-cache case (no telemetry yet)
// with HTTP 503 and a structured error message.

// ─── /api/state/race ─────────────────────────────────────────────────────

// RaceStateView is the situational-awareness bundle: track, session, lap,
// position, gaps, pit/driver status, sector, weather, safety car. Sourced
// entirely from the in-memory cache — sub-microsecond.
type RaceStateView struct {
	Headline string `json:"headline"`

	// Session identity
	Track              string `json:"track"`
	TrackID            int8   `json:"track_id"`
	SessionType        string `json:"session_type"`
	Lap                uint8  `json:"lap"`
	TotalLaps          uint8  `json:"total_laps"`
	SessionTimeLeftSec uint16 `json:"session_time_left_sec"`

	// My position
	Position       uint8    `json:"position"`
	GridPosition   uint8    `json:"grid_position"`
	GapAheadSec    *float64 `json:"gap_ahead_sec,omitempty"`     // nil if leader / no data
	GapToLeaderSec *float64 `json:"gap_to_leader_sec,omitempty"` // nil if leader

	// Where on track
	PitStatus     string  `json:"pit_status"`
	DriverStatus  string  `json:"driver_status"`
	PitStops      uint8   `json:"pit_stops"`
	Sector        uint8   `json:"sector"` // 1-indexed
	LapDistanceM  float32 `json:"lap_distance_m"`
	TrackLengthM  uint16  `json:"track_length_m"`
	LapPercentage float64 `json:"lap_percentage"` // 0-100

	// Lap timing
	LastLapTimeSec    float64 `json:"last_lap_time_sec"`
	CurrentLapTimeSec float64 `json:"current_lap_time_sec"`

	// Conditions
	SafetyCar       string `json:"safety_car"`
	Weather         string `json:"weather"`
	TrackTempC      int8   `json:"track_temp_c"`
	AirTempC        int8   `json:"air_temp_c"`
	RainPercent     uint8  `json:"rain_percent"`
	PitWindowIdeal  uint8  `json:"pit_window_ideal_lap"`
	PitWindowLatest uint8  `json:"pit_window_latest_lap"`

	PlayerCarIndex uint8 `json:"player_car_index"`

	// Race-lifecycle derived fields (race sessions only). Phase moves through
	// grid → formation → lights_out → racing → final_lap → finished. Zero
	// values for non-race sessions or before the first telemetry packet.
	Phase               string `json:"phase"`
	LapsRemaining       int    `json:"laps_remaining"`
	RaceFinished        bool   `json:"race_finished"`
	FinalClassification uint8  `json:"final_classification,omitempty"`
}

func raceStateHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		s := deps.Store.Cache().Load()
		if s == nil {
			return cacheUnavailable(c)
		}
		v := buildRaceStateView(s)
		return c.JSON(v)
	}
}

func buildRaceStateView(s *models.RaceState) RaceStateView {
	v := RaceStateView{
		Track:              enums.Track(int(s.TrackID)),
		TrackID:            s.TrackID,
		SessionType:        enums.SessionType(int(s.SessionType)),
		Lap:                s.CurrentLap,
		TotalLaps:          s.TotalLaps,
		SessionTimeLeftSec: s.SessionTimeLeft,
		Position:           s.Position,
		GridPosition:       s.GridPosition,
		PitStatus:          enums.PitStatus(s.PitStatus),
		DriverStatus:       enums.DriverStatus(s.DriverStatus),
		PitStops:           s.NumPitStops,
		Sector:             s.Sector + 1, // 1-indexed for human readability
		LapDistanceM:       s.LapDistance,
		TrackLengthM:       s.TrackLength,
		LastLapTimeSec:     msToSec(uint32(s.LastLapTimeMs)),
		CurrentLapTimeSec:  msToSec(uint32(s.CurrentLapTimeMs)),
		SafetyCar:          enums.SafetyCar(s.SafetyCarStatus),
		Weather:            enums.Weather(s.Weather),
		TrackTempC:         s.TrackTemp,
		AirTempC:           s.AirTemp,
		RainPercent:        s.RainPercentage,
		PitWindowIdeal:     s.PitWindowIdeal,
		PitWindowLatest:    s.PitWindowLatest,
		PlayerCarIndex:     s.PlayerCarIndex,
	}

	v.Phase = string(s.Phase())
	v.LapsRemaining = s.LapsRemaining()
	v.RaceFinished = s.RaceFinished
	v.FinalClassification = s.FinalClassification

	if s.TrackLength > 0 {
		v.LapPercentage = round1(float64(s.LapDistance) / float64(s.TrackLength) * 100.0)
	}
	if s.DeltaToFrontMs > 0 && s.Position > 1 {
		gap := round3(float64(s.DeltaToFrontMs) / 1000.0)
		v.GapAheadSec = &gap
	}
	if s.DeltaToLeaderMs > 0 && s.Position > 1 {
		gap := round3(float64(s.DeltaToLeaderMs) / 1000.0)
		v.GapToLeaderSec = &gap
	}

	v.Headline = composeRaceHeadline(&v, s)
	return v
}

func composeRaceHeadline(v *RaceStateView, s *models.RaceState) string {
	parts := []string{fmt.Sprintf("P%d at %s", v.Position, v.Track)}
	if v.TotalLaps > 0 {
		parts = append(parts, fmt.Sprintf("lap %d/%d", v.Lap, v.TotalLaps))
	}
	if v.GapAheadSec != nil {
		parts = append(parts, fmt.Sprintf("gap +%.1fs ahead", *v.GapAheadSec))
	}
	if v.PitStatus != "On Track" {
		parts = append(parts, strings.ToLower(v.PitStatus))
	}
	if v.SafetyCar != "None" {
		parts = append(parts, "SC: "+v.SafetyCar)
	}
	if v.RainPercent > 30 {
		parts = append(parts, fmt.Sprintf("rain %d%%", v.RainPercent))
	}
	return strings.Join(parts, ", ")
}

// ─── /api/state/race_phase ───────────────────────────────────────────────

// RacePhaseView is the compact race-lifecycle bundle for the pi-agent /
// data analyst. Lets a specialist branch strategy on phase without
// pulling the full RaceState blob.
type RacePhaseView struct {
	Headline            string `json:"headline"`
	Phase               string `json:"phase"`
	CurrentLap          uint8  `json:"current_lap"`
	TotalLaps           uint8  `json:"total_laps"`
	LapsRemaining       int    `json:"laps_remaining"`
	SessionTimeLeftSec  uint16 `json:"session_time_left_sec"`
	RaceFinished        bool   `json:"race_finished"`
	FinalClassification uint8  `json:"final_classification,omitempty"`
	GridPosition        uint8  `json:"grid_position"`
	Position            uint8  `json:"position"`
	SessionType         string `json:"session_type"`
	SessionUID          string `json:"session_uid"`
	// Coaching hint chosen by phase — lets a downstream consumer get the
	// canonical one-liner without re-implementing the dispatch.
	StrategyHint string `json:"strategy_hint"`
}

func racePhaseHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		s := deps.Store.Cache().Load()
		if s == nil {
			return cacheUnavailable(c)
		}
		return c.JSON(buildRacePhaseView(s))
	}
}

func buildRacePhaseView(s *models.RaceState) RacePhaseView {
	phase := s.Phase()
	v := RacePhaseView{
		Phase:               string(phase),
		CurrentLap:          s.CurrentLap,
		TotalLaps:           s.TotalLaps,
		LapsRemaining:       s.LapsRemaining(),
		SessionTimeLeftSec:  s.SessionTimeLeft,
		RaceFinished:        s.RaceFinished,
		FinalClassification: s.FinalClassification,
		GridPosition:        s.GridPosition,
		Position:            s.Position,
		SessionType:         enums.SessionType(int(s.SessionType)),
		SessionUID:          strconv.FormatUint(s.SessionUID, 10),
		StrategyHint:        strategyHintForPhase(phase, s),
	}
	v.Headline = composeRacePhaseHeadline(&v)
	return v
}

func strategyHintForPhase(phase models.RacePhase, s *models.RaceState) string {
	switch phase {
	case models.PhaseGrid:
		return "Pre-race: ONE short tone-setting line. Set the plan in five words."
	case models.PhaseFormation:
		return "Formation lap: weave to heat tyres, brakes on. One cue, then silent."
	case models.PhaseLightsOut:
		return "Lights out — one urgent line, then ~30s silence on lap 1."
	case models.PhaseFinalLap:
		switch {
		case s.Position == 1:
			return "Last lap, P1 — bring it home clean, no risks."
		case s.Position <= 3:
			return "Last lap, podium on the line — hold position."
		case s.DeltaToFrontMs > 0 && s.DeltaToFrontMs < 2000:
			return "Last lap — car ahead in reach, go for it."
		default:
			return "Last lap — push or protect, no save."
		}
	case models.PhaseFinished:
		finalPos := s.FinalClassification
		if finalPos == 0 {
			finalPos = s.Position
		}
		switch {
		case finalPos == 1:
			return "RACE WIN — celebrate properly on the radio."
		case finalPos <= 3:
			return "PODIUM — acknowledge it before any debrief."
		default:
			return "Race over — lead with the honest result, one constructive note."
		}
	case models.PhaseRacing:
		if rem := s.LapsRemaining(); rem > 0 && rem <= 5 {
			return "Late race — switch from saving to pushing; damage-aware calls favour 'manage to finish'."
		}
		return "Race running normally."
	default:
		return ""
	}
}

func composeRacePhaseHeadline(v *RacePhaseView) string {
	switch v.Phase {
	case string(models.PhaseFinished):
		fp := v.FinalClassification
		if fp == 0 {
			fp = v.Position
		}
		return fmt.Sprintf("FINISHED — final P%d (started P%d)", fp, v.GridPosition)
	case string(models.PhaseFinalLap):
		return fmt.Sprintf("FINAL LAP — P%d, lap %d/%d", v.Position, v.CurrentLap, v.TotalLaps)
	case string(models.PhaseLightsOut):
		return fmt.Sprintf("LIGHTS OUT — started P%d, %d laps", v.GridPosition, v.TotalLaps)
	case string(models.PhaseGrid):
		return fmt.Sprintf("GRID — starting P%d, %d laps", v.GridPosition, v.TotalLaps)
	case string(models.PhaseFormation):
		return fmt.Sprintf("FORMATION LAP — starting P%d, %d laps", v.GridPosition, v.TotalLaps)
	case string(models.PhaseRacing):
		return fmt.Sprintf("RACING — lap %d/%d (%d to go), P%d", v.CurrentLap, v.TotalLaps, v.LapsRemaining, v.Position)
	default:
		return fmt.Sprintf("%s — %s", v.SessionType, v.Phase)
	}
}

// ─── /api/state/tires ────────────────────────────────────────────────────

// CornerFloat is a labeled per-corner float (FL/FR/RL/RR). The F1 25 game
// sends tire arrays in [RL, RR, FL, FR] order — we re-index to FL/FR/RL/RR
// here so the model never has to think about array layouts.
type CornerFloat struct {
	FL float64 `json:"fl"`
	FR float64 `json:"fr"`
	RL float64 `json:"rl"`
	RR float64 `json:"rr"`
}

type CornerInt struct {
	FL int `json:"fl"`
	FR int `json:"fr"`
	RL int `json:"rl"`
	RR int `json:"rr"`
}

// TiresView is the tire-and-damage health bundle.
type TiresView struct {
	Headline string `json:"headline"`

	Compound      string `json:"compound"`       // visual: Soft/Medium/Hard/Inter/Wet — what the engineer actually says
	CompoundGrade string `json:"compound_grade"` // C0-C5 — Pirelli grade for cross-compound comparisons
	AgeLaps       uint8  `json:"age_laps"`

	WearPercent  CornerFloat `json:"wear_percent"`
	SurfaceTempC CornerInt   `json:"surface_temp_c"`
	InnerTempC   CornerInt   `json:"inner_temp_c"`
	PressurePSI  CornerFloat `json:"pressure_psi"`
	BrakeTempC   CornerInt   `json:"brake_temp_c"`

	Damage   DamageView `json:"damage_percent"`
	DRSFault bool       `json:"drs_fault"`
	ERSFault bool       `json:"ers_fault"`
}

// DamageView lists every damage component the game reports. Values are
// percentages 0–100. Aero components plus engine internals so the model
// can answer "is anything broken" without a second tool call.
type DamageView struct {
	FrontLeftWing  uint8 `json:"front_left_wing"`
	FrontRightWing uint8 `json:"front_right_wing"`
	RearWing       uint8 `json:"rear_wing"`
	Floor          uint8 `json:"floor"`
	Diffuser       uint8 `json:"diffuser"`
	Sidepod        uint8 `json:"sidepod"`
	Gearbox        uint8 `json:"gearbox"`
	Engine         uint8 `json:"engine"`
}

func tiresHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		s := deps.Store.Cache().Load()
		if s == nil {
			return cacheUnavailable(c)
		}
		v := buildTiresView(s)
		return c.JSON(v)
	}
}

func buildTiresView(s *models.RaceState) TiresView {
	// F1 25 tire array order: [RL, RR, FL, FR] -> FL=2, FR=3, RL=0, RR=1.
	v := TiresView{
		Compound:      enums.Compound(s.VisualCompound),
		CompoundGrade: enums.ActualCompound(s.ActualCompound),
		AgeLaps:       s.TyresAgeLaps,
		WearPercent: CornerFloat{
			FL: round1(float64(s.TyresWear[2])),
			FR: round1(float64(s.TyresWear[3])),
			RL: round1(float64(s.TyresWear[0])),
			RR: round1(float64(s.TyresWear[1])),
		},
		SurfaceTempC: CornerInt{
			FL: int(s.TyresSurfTemp[2]),
			FR: int(s.TyresSurfTemp[3]),
			RL: int(s.TyresSurfTemp[0]),
			RR: int(s.TyresSurfTemp[1]),
		},
		InnerTempC: CornerInt{
			FL: int(s.TyresInnerTemp[2]),
			FR: int(s.TyresInnerTemp[3]),
			RL: int(s.TyresInnerTemp[0]),
			RR: int(s.TyresInnerTemp[1]),
		},
		PressurePSI: CornerFloat{
			FL: round1(float64(s.TyresPressure[2])),
			FR: round1(float64(s.TyresPressure[3])),
			RL: round1(float64(s.TyresPressure[0])),
			RR: round1(float64(s.TyresPressure[1])),
		},
		BrakeTempC: CornerInt{
			FL: int(s.BrakesTemp[2]),
			FR: int(s.BrakesTemp[3]),
			RL: int(s.BrakesTemp[0]),
			RR: int(s.BrakesTemp[1]),
		},
		Damage: DamageView{
			FrontLeftWing:  s.FrontLeftWingDmg,
			FrontRightWing: s.FrontRightWingDmg,
			RearWing:       s.RearWingDmg,
			Floor:          s.FloorDmg,
			Diffuser:       s.DiffuserDmg,
			Sidepod:        s.SidepodDmg,
			Gearbox:        s.GearBoxDmg,
			Engine:         s.EngineDmg,
		},
		DRSFault: s.DRSFault > 0,
		ERSFault: s.ERSFault > 0,
	}

	v.Headline = composeTiresHeadline(&v)
	return v
}

func composeTiresHeadline(v *TiresView) string {
	maxWear := math.Max(math.Max(v.WearPercent.FL, v.WearPercent.FR),
		math.Max(v.WearPercent.RL, v.WearPercent.RR))
	parts := []string{
		fmt.Sprintf("%s tires lap %d", v.Compound, v.AgeLaps),
		fmt.Sprintf("max wear %.0f%%", maxWear),
	}
	worst := worstDamageComponent(&v.Damage)
	if worst != "" {
		parts = append(parts, "damage: "+worst)
	}
	return strings.Join(parts, ", ")
}

// worstDamageComponent returns "name X%" for the highest-damaged component
// when any is over 0, else empty string.
func worstDamageComponent(d *DamageView) string {
	type entry struct {
		name string
		val  uint8
	}
	all := []entry{
		{"FL wing", d.FrontLeftWing}, {"FR wing", d.FrontRightWing},
		{"rear wing", d.RearWing}, {"floor", d.Floor}, {"diffuser", d.Diffuser},
		{"sidepod", d.Sidepod}, {"gearbox", d.Gearbox}, {"engine", d.Engine},
	}
	worst := entry{}
	for _, e := range all {
		if e.val > worst.val {
			worst = e
		}
	}
	if worst.val == 0 {
		return ""
	}
	return fmt.Sprintf("%s %d%%", worst.name, worst.val)
}

// ─── /api/state/energy ───────────────────────────────────────────────────

type FuelView struct {
	InTankKg      float64 `json:"in_tank_kg"`
	RemainingLaps float64 `json:"remaining_laps"`
	Mix           string  `json:"mix"`
}

type ERSView struct {
	StorePercent           float64 `json:"store_percent"`        // 0-100
	StoreJoules            float64 `json:"store_joules"`         // raw
	DeployMode             string  `json:"deploy_mode"`
	HarvestedMGUKThisLapMJ float64 `json:"harvested_mguk_this_lap_mj"`
	HarvestedMGUHThisLapMJ float64 `json:"harvested_mguh_this_lap_mj"`
	DeployedThisLapMJ      float64 `json:"deployed_this_lap_mj"`
}

type DRSView struct {
	Allowed bool `json:"allowed"`
	Active  bool `json:"active"`
	Fault   bool `json:"fault"`
}

// EnergyView bundles fuel + ERS + DRS — the energy management cluster.
type EnergyView struct {
	Headline string   `json:"headline"`
	Fuel     FuelView `json:"fuel"`
	ERS      ERSView  `json:"ers"`
	DRS      DRSView  `json:"drs"`
}

func energyHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		s := deps.Store.Cache().Load()
		if s == nil {
			return cacheUnavailable(c)
		}
		v := buildEnergyView(s)
		return c.JSON(v)
	}
}

func buildEnergyView(s *models.RaceState) EnergyView {
	const ersCapJ = 4_000_000.0 // 4 MJ regulatory cap
	v := EnergyView{
		Fuel: FuelView{
			InTankKg:      round1(float64(s.FuelInTank)),
			RemainingLaps: round1(float64(s.FuelRemainingLaps)),
			Mix:           enums.FuelMix(s.FuelMix),
		},
		ERS: ERSView{
			StorePercent:           round1(float64(s.ERSStoreEnergy) / ersCapJ * 100.0),
			StoreJoules:            float64(s.ERSStoreEnergy),
			DeployMode:             enums.ERSDeployMode(s.ERSDeployMode),
			HarvestedMGUKThisLapMJ: round2(float64(s.ERSHarvestedMGUK) / 1_000_000.0),
			HarvestedMGUHThisLapMJ: round2(float64(s.ERSHarvestedMGUH) / 1_000_000.0),
			DeployedThisLapMJ:      round2(float64(s.ERSDeployedLap) / 1_000_000.0),
		},
		DRS: DRSView{
			Allowed: s.DRSAllowed > 0,
			Active:  s.DRS > 0,
			Fault:   s.DRSFault > 0,
		},
	}
	v.Headline = fmt.Sprintf(
		"fuel %.1fkg (%.1f laps, %s), ERS %.0f%% (%s), DRS %s",
		v.Fuel.InTankKg, v.Fuel.RemainingLaps, v.Fuel.Mix,
		v.ERS.StorePercent, v.ERS.DeployMode, drsLabel(v.DRS),
	)
	return v
}

func drsLabel(d DRSView) string {
	if d.Fault {
		return "fault"
	}
	if d.Active {
		return "active"
	}
	if d.Allowed {
		return "available"
	}
	return "off"
}

// ─── /api/state/competitors ──────────────────────────────────────────────

// CompetitorEntry is one car's snapshot: position, tires, pit info, last lap,
// gap to me. Used in the nearby/leader/recent-pit lists.
type CompetitorEntry struct {
	Position       uint8   `json:"position"`
	CarIndex       uint8   `json:"car_index"`
	DriverName     string  `json:"driver_name,omitempty"` // surname (Hamilton, Norris) — empty until Participants packet lands
	GapToMeSec     float64 `json:"gap_to_me_sec"`     // absolute gap
	PositionRel    string  `json:"position_relative"` // "ahead" | "behind" | "same"
	LastLapTimeSec float64 `json:"last_lap_time_sec"`
	Compound       string  `json:"compound"`        // visual: Soft/Medium/Hard
	CompoundGrade  string  `json:"compound_grade"`  // C0-C5
	TireAgeLaps    uint8   `json:"tire_age_laps"`
	PitStops       uint8   `json:"pit_stops"`
	PitStatus      string  `json:"pit_status"`
	CurrentLapNum  uint8   `json:"current_lap"`
}

// CompetitorsView is the competitive landscape. Always returns the full
// sorted grid (every car the game has reported a position for) plus
// pre-sliced convenience subsets the model usually wants:
//   - leader (P1, omitted when I am the leader)
//   - nearby_ahead (P-3..P-1 relative to me)
//   - nearby_behind (P+1..P+3)
//   - recent_pits (cars whose pit_status flipped to in-pit in the last ~3 min)
//
// full_grid is the source of truth — the slices are derived views, useful
// for compact prompts where the model just wants "who's nearest". When the
// driver asks "where is Hamilton" the agent reads full_grid.
type CompetitorsView struct {
	Headline     string            `json:"headline"`
	MyPosition   uint8             `json:"my_position"`
	MyCarIndex   uint8             `json:"my_car_index"`
	MyCompound   string            `json:"my_compound"`
	MyTireAge    uint8             `json:"my_tire_age_laps"`
	Leader       *CompetitorEntry  `json:"leader,omitempty"`
	NearbyAhead  []CompetitorEntry `json:"nearby_ahead"`
	NearbyBehind []CompetitorEntry `json:"nearby_behind"`
	RecentPits   []CompetitorEntry `json:"recent_pits"`
	FullGrid     []CompetitorEntry `json:"full_grid"`
}

// competitorsSQL pulls the latest snapshot per car from lap_data + car_status.
// Uses a single CTE-of-row-numbers to avoid scanning the whole table twice.
const competitorsSQL = `
WITH latest_lap AS (
  SELECT car_index, car_position, current_lap_num, last_lap_time_in_ms,
         num_pit_stops, pit_status, delta_to_race_leader_in_ms,
         ROW_NUMBER() OVER (PARTITION BY car_index ORDER BY timestamp DESC) AS rn
  FROM lap_data
), latest_status AS (
  SELECT car_index, actual_tyre_compound, visual_tyre_compound, tyres_age_laps,
         ROW_NUMBER() OVER (PARTITION BY car_index ORDER BY timestamp DESC) AS rn
  FROM car_status
)
SELECT ll.car_index, ll.car_position, ll.current_lap_num,
       ll.last_lap_time_in_ms, ll.num_pit_stops, ll.pit_status,
       ll.delta_to_race_leader_in_ms,
       ls.actual_tyre_compound, ls.visual_tyre_compound, ls.tyres_age_laps
FROM latest_lap ll
JOIN latest_status ls ON ll.car_index = ls.car_index AND ls.rn = 1
WHERE ll.rn = 1 AND ll.car_position > 0
ORDER BY ll.car_position
`

// recentPitsSQL finds cars whose pit_status transitioned to in-pit within the
// last ~3 laps (proxy: timestamp within last 3 minutes — F1 lap is ~90s).
const recentPitsSQL = `
WITH transitions AS (
  SELECT car_index, pit_status, timestamp,
         LAG(pit_status) OVER (PARTITION BY car_index ORDER BY timestamp) AS prev_pit
  FROM lap_data
  WHERE timestamp > now() - INTERVAL 3 MINUTE
), pitters AS (
  SELECT DISTINCT car_index FROM transitions
  WHERE pit_status IN (1, 2) AND (prev_pit IS NULL OR prev_pit = 0)
), latest_lap AS (
  SELECT car_index, car_position, current_lap_num, last_lap_time_in_ms,
         num_pit_stops, pit_status, delta_to_race_leader_in_ms,
         ROW_NUMBER() OVER (PARTITION BY car_index ORDER BY timestamp DESC) AS rn
  FROM lap_data
), latest_status AS (
  SELECT car_index, actual_tyre_compound, visual_tyre_compound, tyres_age_laps,
         ROW_NUMBER() OVER (PARTITION BY car_index ORDER BY timestamp DESC) AS rn
  FROM car_status
)
SELECT ll.car_index, ll.car_position, ll.current_lap_num,
       ll.last_lap_time_in_ms, ll.num_pit_stops, ll.pit_status,
       ll.delta_to_race_leader_in_ms,
       ls.actual_tyre_compound, ls.visual_tyre_compound, ls.tyres_age_laps
FROM pitters p
JOIN latest_lap ll ON p.car_index = ll.car_index AND ll.rn = 1
JOIN latest_status ls ON p.car_index = ls.car_index AND ls.rn = 1
WHERE ll.car_position > 0
ORDER BY ll.car_position
`

func competitorsHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		s := deps.Store.Cache().Load()
		if s == nil {
			return cacheUnavailable(c)
		}
		myPos := int(s.Position)
		myIdx := s.PlayerCarIndex
		myDeltaToLeader := int(s.DeltaToLeaderMs)
		roster := deps.Store.Roster()

		rows, err := deps.Store.Query(c.Context(), competitorsSQL)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": fmt.Sprintf("competitor query failed: %v", err),
			})
		}

		var leader *CompetitorEntry
		var ahead, behind, fullGrid []CompetitorEntry
		for _, r := range rows {
			e := rowToCompetitor(r, myDeltaToLeader, roster)
			fullGrid = append(fullGrid, e)
			if e.Position == 1 && e.CarIndex != myIdx {
				ec := e
				leader = &ec
			}
			if e.CarIndex == myIdx {
				continue
			}
			diff := int(e.Position) - myPos
			switch {
			case diff < 0 && diff >= -3:
				ahead = append(ahead, e)
			case diff > 0 && diff <= 3:
				behind = append(behind, e)
			}
		}

		// Recent pits — separate query so we don't drag the whole grid
		// through the in-pit filter every call.
		pitRows, err := deps.Store.Query(c.Context(), recentPitsSQL)
		var recentPits []CompetitorEntry
		if err == nil {
			for _, r := range pitRows {
				e := rowToCompetitor(r, myDeltaToLeader, roster)
				if e.CarIndex == myIdx {
					continue
				}
				recentPits = append(recentPits, e)
			}
		}

		v := CompetitorsView{
			MyPosition:   uint8(myPos),
			MyCarIndex:   myIdx,
			MyCompound:   enums.Compound(s.VisualCompound),
			MyTireAge:    s.TyresAgeLaps,
			Leader:       leader,
			NearbyAhead:  ahead,
			NearbyBehind: behind,
			RecentPits:   recentPits,
			FullGrid:     fullGrid,
		}
		v.Headline = composeCompetitorsHeadline(&v)
		return c.JSON(v)
	}
}

func rowToCompetitor(r map[string]interface{}, myDeltaToLeader int, roster *state.Roster) CompetitorEntry {
	pos := uint8(asInt(r["car_position"]))
	idx := uint8(asInt(r["car_index"]))
	deltaToLeader := asInt(r["delta_to_race_leader_in_ms"])

	gapMs := deltaToLeader - myDeltaToLeader
	rel := "same"
	if gapMs < 0 {
		rel = "ahead"
	} else if gapMs > 0 {
		rel = "behind"
	}

	name := ""
	if roster != nil {
		name = roster.ResolveRadioName(idx)
	}

	return CompetitorEntry{
		Position:       pos,
		CarIndex:       idx,
		DriverName:     name,
		GapToMeSec:     round3(math.Abs(float64(gapMs)) / 1000.0),
		PositionRel:    rel,
		LastLapTimeSec: msToSec(uint32(asInt(r["last_lap_time_in_ms"]))),
		Compound:       enums.Compound(uint8(asInt(r["visual_tyre_compound"]))),
		CompoundGrade:  enums.ActualCompound(uint8(asInt(r["actual_tyre_compound"]))),
		TireAgeLaps:    uint8(asInt(r["tyres_age_laps"])),
		PitStops:       uint8(asInt(r["num_pit_stops"])),
		PitStatus:      enums.PitStatus(uint8(asInt(r["pit_status"]))),
		CurrentLapNum:  uint8(asInt(r["current_lap_num"])),
	}
}

func composeCompetitorsHeadline(v *CompetitorsView) string {
	bits := []string{fmt.Sprintf("P%d on %s lap %d", v.MyPosition, v.MyCompound, v.MyTireAge)}
	if len(v.NearbyAhead) > 0 {
		a := v.NearbyAhead[len(v.NearbyAhead)-1] // closest ahead = last in ascending position list
		bits = append(bits, fmt.Sprintf("%s ahead +%.1fs (%s lap %d)",
			competitorLabel(a), a.GapToMeSec, a.Compound, a.TireAgeLaps))
	}
	if len(v.NearbyBehind) > 0 {
		b := v.NearbyBehind[0]
		bits = append(bits, fmt.Sprintf("%s behind -%.1fs (%s lap %d)",
			competitorLabel(b), b.GapToMeSec, b.Compound, b.TireAgeLaps))
	}
	if len(v.RecentPits) > 0 {
		bits = append(bits, fmt.Sprintf("%d recent pit", len(v.RecentPits)))
	}
	return strings.Join(bits, ", ")
}

// competitorLabel renders the surname when populated, falling back to
// "P{N}" so the headline stays useful before Participants land.
func competitorLabel(e CompetitorEntry) string {
	if e.DriverName != "" {
		return e.DriverName
	}
	return fmt.Sprintf("P%d", e.Position)
}

// ─── /api/state/pace ─────────────────────────────────────────────────────

type LapEntry struct {
	Lap        uint8   `json:"lap"`
	LapTimeSec float64 `json:"lap_time_sec"`
	S1Sec      float64 `json:"s1_sec"`
	S2Sec      float64 `json:"s2_sec"`
	S3Sec      float64 `json:"s3_sec"`
	Valid      bool    `json:"valid"`
}

type PaceView struct {
	Headline           string     `json:"headline"`
	SessionUID         string     `json:"session_uid"`            // current session id (uint64 stringified — JSON safe)
	SessionLapCount    int        `json:"session_lap_count"`      // laps already complete this session
	RecentLaps         []LapEntry `json:"recent_laps"`            // current session, most-recent-first, up to 8
	BestLapSec         float64    `json:"best_lap_sec"`           // current session
	BestS1Sec          float64    `json:"best_s1_sec"`            // current session
	BestS2Sec          float64    `json:"best_s2_sec"`            // current session
	BestS3Sec          float64    `json:"best_s3_sec"`            // current session
	BestTheoreticalSec float64    `json:"best_theoretical_sec"`   // current session
	DeltaToBestSec     float64    `json:"delta_to_best_sec"`      // last lap vs current-session best
	DeltaToAheadSec    *float64   `json:"delta_to_ahead_sec,omitempty"`  // last lap vs car ahead
	DeltaToLeaderSec   *float64   `json:"delta_to_leader_sec,omitempty"` // last lap vs leader
	Direction          string     `json:"direction"`              // warming up | stable | improving | going off | n/a

	// All-time numbers across every recorded session for this car.
	AllTimeBestLapSec     float64 `json:"all_time_best_lap_sec"`
	AllTimeBestSession    string  `json:"all_time_best_session,omitempty"`
	AllTimeSessionsCount  int     `json:"all_time_sessions_count"`
	AllTimeLapsCount      int     `json:"all_time_laps_count"`
	DeltaToAllTimeBestSec float64 `json:"delta_to_all_time_best_sec"` // last lap vs all-time best

	// PastSessions ordered most-recent first. Capped by ?past=N (default 3,
	// "all" returns every prior session). Excludes the current session.
	PastSessions []PastSessionSummary `json:"past_sessions"`
}

// PastSessionSummary collapses an entire prior session into one row so the
// driver/agent can see "what did I do here last time" without re-running
// per-lap queries. Times are in seconds; zeros mean no valid sample.
type PastSessionSummary struct {
	SessionUID         string  `json:"session_uid"`
	StartedAt          string  `json:"started_at,omitempty"` // earliest timestamp seen for this car/session
	LapCount           int     `json:"lap_count"`
	BestLapSec         float64 `json:"best_lap_sec"`
	BestS1Sec          float64 `json:"best_s1_sec"`
	BestS2Sec          float64 `json:"best_s2_sec"`
	BestS3Sec          float64 `json:"best_s3_sec"`
	BestTheoreticalSec float64 `json:"best_theoretical_sec"`
	AvgLapSec          float64 `json:"avg_lap_sec"`
}

// paceLapsSQL pulls the player's last 8 laps from the *current* session.
// session_uid is the F1 packet header field — every packet inside one
// session shares it, so filtering on it cleanly separates this run from
// prior recorded sessions in the same DuckDB file.
const paceLapsSQL = `
SELECT lap_num, lap_time_in_ms, sector1_time_in_ms,
       sector2_time_in_ms, sector3_time_in_ms, lap_valid
FROM session_history
WHERE car_index = %d AND session_uid = %d AND lap_time_in_ms > 0
ORDER BY lap_num DESC
LIMIT 8
`

// paceBestSQL pulls best lap + best individual sectors for the player in
// the *current* session.
const paceBestSQL = `
SELECT
  COUNT(*)                           AS lap_count,
  MIN(NULLIF(lap_time_in_ms, 0))     AS best_lap_ms,
  MIN(NULLIF(sector1_time_in_ms, 0)) AS best_s1_ms,
  MIN(NULLIF(sector2_time_in_ms, 0)) AS best_s2_ms,
  MIN(NULLIF(sector3_time_in_ms, 0)) AS best_s3_ms
FROM session_history
WHERE car_index = %d AND session_uid = %d AND lap_time_in_ms > 0
`

// paceAllTimeBestSQL pulls the all-time best lap and which session it was
// set in. Spans every recorded session for this car, including the current
// one (so a fresh PB this session shows up here too).
const paceAllTimeBestSQL = `
SELECT lap_time_in_ms AS best_lap_ms, session_uid AS session
FROM session_history
WHERE car_index = %d AND lap_time_in_ms > 0
ORDER BY lap_time_in_ms ASC
LIMIT 1
`

// paceAllTimeAggSQL counts total recorded sessions and laps for the car.
const paceAllTimeAggSQL = `
SELECT
  COUNT(DISTINCT session_uid) AS session_count,
  COUNT(*)                    AS lap_count
FROM session_history
WHERE car_index = %d AND lap_time_in_ms > 0
`

// pacePastSessionsSQL summarises each prior session for the car, most-recent
// first. The current session is excluded by the WHERE clause; `LIMIT %d`
// caps how far back we go (use a large number for "all"). started_at uses
// MIN(timestamp) per session — this assumes the session row got a fresh
// timestamp on first insert, which is true for new rows but NULL on rows
// migrated from the pre-session_uid schema (those will sort last).
const pacePastSessionsSQL = `
SELECT
  session_uid,
  MIN(timestamp)                     AS started_at,
  COUNT(*)                           AS lap_count,
  MIN(NULLIF(lap_time_in_ms, 0))     AS best_lap_ms,
  MIN(NULLIF(sector1_time_in_ms, 0)) AS best_s1_ms,
  MIN(NULLIF(sector2_time_in_ms, 0)) AS best_s2_ms,
  MIN(NULLIF(sector3_time_in_ms, 0)) AS best_s3_ms,
  AVG(NULLIF(lap_time_in_ms, 0))     AS avg_lap_ms
FROM session_history
WHERE car_index = %d AND session_uid IS NOT NULL AND session_uid <> %d
  AND lap_time_in_ms > 0
GROUP BY session_uid
ORDER BY started_at DESC NULLS LAST
LIMIT %d
`

// paceCompetitorLastLapSQL pulls a specific car's last lap time for delta math.
const paceCompetitorLastLapSQL = `
SELECT last_lap_time_in_ms
FROM lap_data
WHERE car_index = %d AND last_lap_time_in_ms > 0
ORDER BY timestamp DESC LIMIT 1
`

func paceHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		s := deps.Store.Cache().Load()
		if s == nil {
			return cacheUnavailable(c)
		}
		myIdx := int(s.PlayerCarIndex)
		curUID := s.SessionUID

		// ?past=N caps how many prior sessions to surface. Default 3 keeps
		// the response compact for the Live agent while still answering
		// "how was last time here?" Use ?past=all (or any large number) to
		// page through every recorded session. ?past=0 disables the past
		// block entirely.
		pastLimit := 3
		if q := c.Query("past"); q != "" {
			switch strings.ToLower(q) {
			case "all":
				pastLimit = 1000
			case "0", "none":
				pastLimit = 0
			default:
				if n, err := strconv.Atoi(q); err == nil && n >= 0 && n <= 1000 {
					pastLimit = n
				}
			}
		}

		lapRows, err := deps.Store.Query(c.Context(), fmt.Sprintf(paceLapsSQL, myIdx, curUID))
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": fmt.Sprintf("pace lap query failed: %v", err),
			})
		}
		bestRows, err := deps.Store.Query(c.Context(), fmt.Sprintf(paceBestSQL, myIdx, curUID))
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": fmt.Sprintf("pace best query failed: %v", err),
			})
		}

		v := PaceView{
			RecentLaps:   []LapEntry{},
			PastSessions: []PastSessionSummary{},
			SessionUID:   strconv.FormatUint(curUID, 10),
		}
		for _, r := range lapRows {
			s1 := msToSec(uint32(asInt(r["sector1_time_in_ms"])))
			s2 := msToSec(uint32(asInt(r["sector2_time_in_ms"])))
			s3 := msToSec(uint32(asInt(r["sector3_time_in_ms"])))
			v.RecentLaps = append(v.RecentLaps, LapEntry{
				Lap:        uint8(asInt(r["lap_num"])),
				LapTimeSec: msToSec(uint32(asInt(r["lap_time_in_ms"]))),
				S1Sec:      s1,
				S2Sec:      s2,
				S3Sec:      s3,
				Valid:      asInt(r["lap_valid"]) > 0,
			})
		}
		if len(bestRows) > 0 {
			b := bestRows[0]
			v.SessionLapCount = asInt(b["lap_count"])
			v.BestLapSec = msToSec(uint32(asInt(b["best_lap_ms"])))
			v.BestS1Sec = msToSec(uint32(asInt(b["best_s1_ms"])))
			v.BestS2Sec = msToSec(uint32(asInt(b["best_s2_ms"])))
			v.BestS3Sec = msToSec(uint32(asInt(b["best_s3_ms"])))
			if v.BestS1Sec > 0 && v.BestS2Sec > 0 && v.BestS3Sec > 0 {
				v.BestTheoreticalSec = round3(v.BestS1Sec + v.BestS2Sec + v.BestS3Sec)
			}
		}
		if len(v.RecentLaps) > 0 && v.BestLapSec > 0 {
			v.DeltaToBestSec = round3(v.RecentLaps[0].LapTimeSec - v.BestLapSec)
		}

		// All-time numbers — best lap & aggregate counts across every
		// recorded session for this car. Best/forever queries are cheap
		// (one row) so they always run.
		if rows, err := deps.Store.Query(c.Context(), fmt.Sprintf(paceAllTimeBestSQL, myIdx)); err == nil && len(rows) > 0 {
			r := rows[0]
			v.AllTimeBestLapSec = msToSec(uint32(asInt(r["best_lap_ms"])))
			if uid := asUint64(r["session"]); uid != 0 {
				v.AllTimeBestSession = strconv.FormatUint(uid, 10)
			}
		}
		if rows, err := deps.Store.Query(c.Context(), fmt.Sprintf(paceAllTimeAggSQL, myIdx)); err == nil && len(rows) > 0 {
			r := rows[0]
			v.AllTimeSessionsCount = asInt(r["session_count"])
			v.AllTimeLapsCount = asInt(r["lap_count"])
		}
		if len(v.RecentLaps) > 0 && v.AllTimeBestLapSec > 0 {
			v.DeltaToAllTimeBestSec = round3(v.RecentLaps[0].LapTimeSec - v.AllTimeBestLapSec)
		}

		// Past-session block. Skipped when pastLimit==0.
		if pastLimit > 0 {
			rows, err := deps.Store.Query(c.Context(), fmt.Sprintf(pacePastSessionsSQL, myIdx, curUID, pastLimit))
			if err == nil {
				for _, r := range rows {
					ps := PastSessionSummary{
						SessionUID: strconv.FormatUint(asUint64(r["session_uid"]), 10),
						LapCount:   asInt(r["lap_count"]),
						BestLapSec: msToSec(uint32(asInt(r["best_lap_ms"]))),
						BestS1Sec:  msToSec(uint32(asInt(r["best_s1_ms"]))),
						BestS2Sec:  msToSec(uint32(asInt(r["best_s2_ms"]))),
						BestS3Sec:  msToSec(uint32(asInt(r["best_s3_ms"]))),
						AvgLapSec:  msToSec(uint32(asInt(r["avg_lap_ms"]))),
					}
					if ps.BestS1Sec > 0 && ps.BestS2Sec > 0 && ps.BestS3Sec > 0 {
						ps.BestTheoreticalSec = round3(ps.BestS1Sec + ps.BestS2Sec + ps.BestS3Sec)
					}
					if t, ok := r["started_at"].(time.Time); ok && !t.IsZero() {
						ps.StartedAt = t.UTC().Format(time.RFC3339)
					}
					v.PastSessions = append(v.PastSessions, ps)
				}
			}
		}

		// Deltas to neighbors require knowing their car_index by position.
		// We piggyback on the competitors snapshot logic (single SQL) so the
		// numbers stay consistent.
		if rows, err := deps.Store.Query(c.Context(), competitorsSQL); err == nil {
			myPos := int(s.Position)
			for _, r := range rows {
				p := asInt(r["car_position"])
				idx := asInt(r["car_index"])
				if idx == myIdx {
					continue
				}
				lap := msToSec(uint32(asInt(r["last_lap_time_in_ms"])))
				if lap == 0 || len(v.RecentLaps) == 0 {
					continue
				}
				myLast := v.RecentLaps[0].LapTimeSec
				if p == myPos-1 {
					d := round3(myLast - lap)
					v.DeltaToAheadSec = &d
				}
				if p == 1 {
					d := round3(myLast - lap)
					v.DeltaToLeaderSec = &d
				}
			}
		}

		v.Direction = paceDirection(v.RecentLaps)
		v.Headline = composePaceHeadline(&v)
		return c.JSON(v)
	}
}

// paceDirection categorises the trend of the last 4 valid lap times. Stable
// = within 0.3s; improving/going-off if a clear monotonic trend across laps.
func paceDirection(laps []LapEntry) string {
	valid := []LapEntry{}
	for _, l := range laps {
		if l.LapTimeSec > 0 && l.Valid {
			valid = append(valid, l)
		}
	}
	if len(valid) < 3 {
		return "n/a"
	}
	// laps[0] is most recent — reverse for chronological scan.
	chrono := make([]LapEntry, len(valid))
	for i, l := range valid {
		chrono[len(valid)-1-i] = l
	}
	// Compare last 3 vs preceding window.
	first := chrono[0].LapTimeSec
	last := chrono[len(chrono)-1].LapTimeSec
	delta := last - first
	switch {
	case delta < -0.3:
		return "improving"
	case delta > 0.5:
		return "going off"
	case math.Abs(delta) <= 0.3:
		return "stable"
	default:
		return "drifting"
	}
}

func composePaceHeadline(v *PaceView) string {
	if len(v.RecentLaps) == 0 {
		// Be explicit about why we have nothing — a blank current session
		// with prior laps in the DB tells the agent "fresh outing" rather
		// than "system is broken".
		if v.AllTimeLapsCount > 0 {
			return fmt.Sprintf("no laps yet this session (%d sessions on record)", v.AllTimeSessionsCount)
		}
		return "no completed laps yet"
	}
	last := v.RecentLaps[0]
	parts := []string{fmt.Sprintf("last %.3f", last.LapTimeSec)}
	if v.BestLapSec > 0 {
		parts = append(parts, fmt.Sprintf("session best %.3f (Δ%+.3f)", v.BestLapSec, v.DeltaToBestSec))
	}
	// Mention all-time best only when it differs from the current-session
	// best — otherwise we'd just say the same number twice.
	if v.AllTimeBestLapSec > 0 && v.AllTimeBestLapSec < v.BestLapSec {
		parts = append(parts, fmt.Sprintf("all-time %.3f (Δ%+.3f)", v.AllTimeBestLapSec, v.DeltaToAllTimeBestSec))
	}
	if v.Direction != "" && v.Direction != "n/a" {
		parts = append(parts, v.Direction)
	}
	if v.DeltaToAheadSec != nil {
		parts = append(parts, fmt.Sprintf("vs ahead %+.3f", *v.DeltaToAheadSec))
	}
	return strings.Join(parts, ", ")
}

// ─── /api/state/events ───────────────────────────────────────────────────

type EventEntry struct {
	Code       string `json:"code"`
	Label      string `json:"label"`
	VehicleIdx int    `json:"vehicle_idx,omitempty"`
	Detail     string `json:"detail,omitempty"`
	AgoSec     int64  `json:"ago_sec"`
	At         string `json:"at"`
}

type EventsView struct {
	Events []EventEntry `json:"events"`
}

// recentEventsSQL pulls events ordered by recency. ago_seconds is computed
// in DuckDB to avoid the Go vs DuckDB timezone-handling mismatch on naïve
// TIMESTAMP columns: comparing time.Now() against a DuckDB-read time.Time
// can shift by the local UTC offset.
const recentEventsSQL = `
SELECT timestamp, event_code, vehicle_idx, detail_text,
       date_diff('second', timestamp, current_timestamp::TIMESTAMP) AS ago_seconds
FROM race_events
ORDER BY timestamp DESC
LIMIT %d
`

func eventsHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		limit := 10
		if q := c.Query("limit"); q != "" {
			if n, err := strconv.Atoi(q); err == nil && n > 0 && n <= 100 {
				limit = n
			}
		}
		rows, err := deps.Store.Query(c.Context(), fmt.Sprintf(recentEventsSQL, limit))
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
				"error": fmt.Sprintf("events query failed: %v", err),
			})
		}
		out := EventsView{Events: make([]EventEntry, 0, len(rows))}
		for _, r := range rows {
			code, _ := r["event_code"].(string)
			detail, _ := r["detail_text"].(string)
			at, _ := r["timestamp"].(time.Time)
			out.Events = append(out.Events, EventEntry{
				Code:       code,
				Label:      enums.EventCode(code),
				VehicleIdx: asInt(r["vehicle_idx"]),
				Detail:     detail,
				AgoSec:     int64(asInt(r["ago_seconds"])),
				At:         at.UTC().Format(time.RFC3339),
			})
		}
		return c.JSON(out)
	}
}

// ─── /api/state/driver_settings ──────────────────────────────────────────

// DriverSettingsView exposes the driver-controllable F1 25 setup channels
// in a form the Live agent and pi-agent setup specialist can act on.
// "cockpit" knobs are changeable mid-stint via the wheel/MFD; "setup_screen"
// values are only changeable in the garage or at the next pit. DRS usage
// metrics let the analyst flag missed/late zone openings.
type DriverSettingsView struct {
	Headline    string                 `json:"headline"`
	Cockpit     CockpitSettings        `json:"cockpit"`
	SetupScreen SetupScreenSettings    `json:"setup_screen"`
	DRS         DRSUsageView           `json:"drs"`
	Recent      []SettingChangeEntry   `json:"recent_changes"`
}

// CockpitSettings holds the channels the driver can change mid-lap via
// rotary switches and MFD pages. Each value carries both the raw F1 25
// unit and a human-readable annotation so the LLM doesn't have to guess.
type CockpitSettings struct {
	BrakeBias        uint8   `json:"brake_bias"`         // % forward, higher = more front
	OnThrottleDiff   uint8   `json:"on_throttle_diff"`   // diff lock 0–100, higher = more locked
	OffThrottleDiff  uint8   `json:"off_throttle_diff"`
	EngineBraking    uint8   `json:"engine_braking"`     // 0–100, higher = more off-throttle braking
	ERSDeployMode    uint8   `json:"ers_deploy_mode"`
	ERSDeployModeName string `json:"ers_deploy_mode_name"`
	FuelMix          uint8   `json:"fuel_mix"`
	FuelMixName      string  `json:"fuel_mix_name"`
}

// SetupScreenSettings holds garage-only values (wings + cold pressures) plus
// the current wing damage so the analyst can flag "note for next pit"
// recommendations without conflating them with cockpit-changeable advice.
type SetupScreenSettings struct {
	FrontWing             uint8       `json:"front_wing"`
	RearWing              uint8       `json:"rear_wing"`
	FrontWingDamagePctMax uint8       `json:"front_wing_damage_pct_max"` // max(FL, FR)
	RearWingDamagePct     uint8       `json:"rear_wing_damage_pct"`
	BrakePressure         uint8       `json:"brake_pressure"`
	ColdPressurePSI       CornerFloat `json:"cold_pressure_psi"`
	Ballast               uint8       `json:"ballast"`
	FuelLoadKg            float64     `json:"fuel_load_kg"`
}

// DRSUsageView surfaces this-lap and last-lap DRS utilization for the
// analyst. utilization_pct is unused_s / available_s × 100; values under
// ~70% on a long DRS straight suggest the driver is leaving lap time on
// the table.
type DRSUsageView struct {
	Allowed              bool    `json:"allowed"`
	Active               bool    `json:"active"`
	Fault                bool    `json:"fault"`
	UsedThisLapSec       float64 `json:"used_this_lap_s"`
	AvailableThisLapSec  float64 `json:"available_this_lap_s"`
	UtilizationPctThisLap float64 `json:"utilization_pct_this_lap"`
}

// SettingChangeEntry is one row of the in-state recent-changes ring,
// rendered chronologically oldest-first.
type SettingChangeEntry struct {
	SessionTime float64 `json:"session_time"`
	Lap         uint8   `json:"lap"`
	Channel     string  `json:"channel"`
	From        int     `json:"from"`
	To          int     `json:"to"`
}

func driverSettingsHandler(deps *Deps) fiber.Handler {
	return func(c *fiber.Ctx) error {
		s := deps.Store.Cache().Load()
		if s == nil {
			return cacheUnavailable(c)
		}
		v := buildDriverSettingsView(s)
		return c.JSON(v)
	}
}

func buildDriverSettingsView(s *models.RaceState) DriverSettingsView {
	ds := s.DriverSettings
	frontDmgMax := s.FrontLeftWingDmg
	if s.FrontRightWingDmg > frontDmgMax {
		frontDmgMax = s.FrontRightWingDmg
	}
	util := 0.0
	if s.DRSAvailableThisLap > 0 {
		util = round1(float64(s.DRSUsedThisLap) / float64(s.DRSAvailableThisLap) * 100.0)
	}
	v := DriverSettingsView{
		Cockpit: CockpitSettings{
			BrakeBias:         ds.BrakeBias,
			OnThrottleDiff:    ds.OnThrottleDiff,
			OffThrottleDiff:   ds.OffThrottleDiff,
			EngineBraking:     ds.EngineBraking,
			ERSDeployMode:     s.ERSDeployMode,
			ERSDeployModeName: enums.ERSDeployMode(s.ERSDeployMode),
			FuelMix:           s.FuelMix,
			FuelMixName:       enums.FuelMix(s.FuelMix),
		},
		SetupScreen: SetupScreenSettings{
			FrontWing:             ds.FrontWing,
			RearWing:              ds.RearWing,
			FrontWingDamagePctMax: frontDmgMax,
			RearWingDamagePct:     s.RearWingDmg,
			BrakePressure:         ds.BrakePressure,
			ColdPressurePSI: CornerFloat{
				FL: round1(float64(ds.FrontLeftTyrePressure)),
				FR: round1(float64(ds.FrontRightTyrePressure)),
				RL: round1(float64(ds.RearLeftTyrePressure)),
				RR: round1(float64(ds.RearRightTyrePressure)),
			},
			Ballast:    ds.Ballast,
			FuelLoadKg: round1(float64(ds.FuelLoad)),
		},
		DRS: DRSUsageView{
			Allowed:               s.DRSAllowed > 0,
			Active:                s.DRS > 0,
			Fault:                 s.DRSFault > 0,
			UsedThisLapSec:        round2(float64(s.DRSUsedThisLap)),
			AvailableThisLapSec:   round2(float64(s.DRSAvailableThisLap)),
			UtilizationPctThisLap: util,
		},
		Recent: collectRecentSettingChanges(ds),
	}
	v.Headline = composeSettingsHeadline(&v)
	return v
}

// collectRecentSettingChanges materializes the ring buffer into a
// chronological slice (oldest-first) capped at ds.RecentChangesLen.
func collectRecentSettingChanges(ds models.DriverSettings) []SettingChangeEntry {
	n := int(ds.RecentChangesLen)
	if n == 0 {
		return []SettingChangeEntry{}
	}
	out := make([]SettingChangeEntry, 0, n)
	// Head points to the next write slot; oldest is at head-n (mod 8).
	start := (int(ds.RecentChangesHead) - n + 8) % 8
	for i := 0; i < n; i++ {
		idx := (start + i) % 8
		ch := ds.RecentChanges[idx]
		out = append(out, SettingChangeEntry{
			SessionTime: round1(float64(ch.SessionTime)),
			Lap:         ch.Lap,
			Channel:     ch.Channel,
			From:        ch.From,
			To:          ch.To,
		})
	}
	return out
}

func composeSettingsHeadline(v *DriverSettingsView) string {
	parts := []string{
		fmt.Sprintf("BB %d%%", v.Cockpit.BrakeBias),
		fmt.Sprintf("diff %d/%d", v.Cockpit.OnThrottleDiff, v.Cockpit.OffThrottleDiff),
		fmt.Sprintf("EngBrk %d", v.Cockpit.EngineBraking),
		fmt.Sprintf("ERS %s", v.Cockpit.ERSDeployModeName),
		fmt.Sprintf("Mix %s", v.Cockpit.FuelMixName),
	}
	if v.DRS.AvailableThisLapSec > 0 {
		parts = append(parts, fmt.Sprintf("DRS used %.1f/%.1fs (%.0f%%)",
			v.DRS.UsedThisLapSec, v.DRS.AvailableThisLapSec, v.DRS.UtilizationPctThisLap))
	}
	if v.SetupScreen.FrontWingDamagePctMax >= 25 {
		parts = append(parts, fmt.Sprintf("FW dmg %d%%", v.SetupScreen.FrontWingDamagePctMax))
	}
	if len(v.Recent) > 0 {
		last := v.Recent[len(v.Recent)-1]
		parts = append(parts, fmt.Sprintf("just changed %s %d→%d", last.Channel, last.From, last.To))
	}
	return strings.Join(parts, ", ")
}

// ─── shared helpers ──────────────────────────────────────────────────────

func cacheUnavailable(c *fiber.Ctx) error {
	return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
		"error": "no telemetry data received yet",
	})
}

func msToSec(ms uint32) float64 {
	if ms == 0 {
		return 0
	}
	return round3(float64(ms) / 1000.0)
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }
func round2(f float64) float64 { return math.Round(f*100) / 100 }
func round3(f float64) float64 { return math.Round(f*1000) / 1000 }

// asUint64 coerces a DuckDB row scalar to uint64 without sign-truncating
// large values — needed for UBIGINT columns like session_uid that can sit
// above the int64 max. Returns 0 on nil or unknown.
func asUint64(v interface{}) uint64 {
	switch x := v.(type) {
	case nil:
		return 0
	case uint64:
		return x
	case int64:
		if x < 0 {
			return 0
		}
		return uint64(x)
	case int32:
		if x < 0 {
			return 0
		}
		return uint64(x)
	case int:
		if x < 0 {
			return 0
		}
		return uint64(x)
	case uint32:
		return uint64(x)
	case uint16:
		return uint64(x)
	case uint8:
		return uint64(x)
	default:
		return 0
	}
}

// asInt coerces a DuckDB row scalar (which arrives as int32/int64/float64
// depending on column type) to a plain int. Returns 0 on nil or unknown.
func asInt(v interface{}) int {
	switch x := v.(type) {
	case nil:
		return 0
	case int:
		return x
	case int32:
		return int(x)
	case int64:
		return int(x)
	case uint8:
		return int(x)
	case uint16:
		return int(x)
	case uint32:
		return int(x)
	case uint64:
		return int(x)
	case float32:
		return int(x)
	case float64:
		return int(x)
	default:
		return 0
	}
}
