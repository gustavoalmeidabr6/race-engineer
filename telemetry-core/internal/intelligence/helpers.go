package intelligence

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog/log"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/enums"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
)

// BuildContext creates a rich context string from the current RaceState.
// Shared by CommsGate, analyst, and any future components.
func BuildContext(state *models.RaceState) string {
	if state == nil {
		return "No telemetry data available yet."
	}

	var parts []string

	// Session identity. For race sessions we surface the derived phase + laps
	// remaining alongside the raw lap count so the LLM never has to guess
	// whether this is the formation lap, mid-race, or the final lap.
	track := trackName(int(state.TrackID))
	sessType := sessionTypeName(int(state.SessionType))
	if models.IsRaceSession(state.SessionType) {
		parts = append(parts, fmt.Sprintf(
			"SESSION: %s at %s, Lap %d/%d (%d to go) [phase: %s]",
			sessType, track, state.CurrentLap, state.TotalLaps,
			state.LapsRemaining(), state.Phase(),
		))
	} else {
		parts = append(parts, fmt.Sprintf("SESSION: %s at %s, Lap %d/%d",
			sessType, track, state.CurrentLap, state.TotalLaps))
	}

	// Position & gaps
	parts = append(parts, fmt.Sprintf("Position: P%d (started P%d), Pit stops: %d",
		state.Position, state.GridPosition, state.NumPitStops))
	if state.DeltaToFrontMs > 0 {
		parts = append(parts, fmt.Sprintf("Gap to car ahead: +%.3fs", float64(state.DeltaToFrontMs)/1000))
	}
	if state.DeltaToLeaderMs > 0 && state.Position > 1 {
		parts = append(parts, fmt.Sprintf("Gap to leader: +%.3fs", float64(state.DeltaToLeaderMs)/1000))
	}
	if state.LastLapTimeMs > 0 {
		parts = append(parts, fmt.Sprintf("Last lap: %.3fs", float64(state.LastLapTimeMs)/1000))
	}
	if state.CurrentLapTimeMs > 0 {
		parts = append(parts, fmt.Sprintf("Current lap: %.3fs", float64(state.CurrentLapTimeMs)/1000))
	}

	// Car location
	pitSt := pitStatusName(state.PitStatus)
	driverSt := driverStatusName(state.DriverStatus)
	lapPct := 0.0
	if state.TrackLength > 0 {
		lapPct = float64(state.LapDistance) / float64(state.TrackLength) * 100
	}
	parts = append(parts, fmt.Sprintf(
		"Location: Sector %d, %.0f%% into lap (%.0fm / %dm), Status: %s, Driver: %s",
		state.Sector+1, lapPct, state.LapDistance, state.TrackLength, pitSt, driverSt,
	))

	// Basic telemetry
	parts = append(parts, fmt.Sprintf("Speed: %dkm/h, Gear: %d, RPM: %d",
		state.Speed, state.Gear, state.EngineRPM))

	// Tires
	compound := compoundName(state.ActualCompound)
	parts = append(parts, fmt.Sprintf("Tire: %s (Age: %d laps)", compound, state.TyresAgeLaps))
	parts = append(parts, fmt.Sprintf("Tire Wear - FL:%.1f%%, FR:%.1f%%, RL:%.1f%%, RR:%.1f%%",
		state.TyresWear[2], state.TyresWear[3], state.TyresWear[0], state.TyresWear[1]))

	// Fuel & ERS
	fuelMix := fuelMixName(state.FuelMix)
	ersPct := float64(state.ERSStoreEnergy) / 4_000_000.0 * 100
	ersMode := ersModeName(state.ERSDeployMode)
	parts = append(parts, fmt.Sprintf("Fuel: %.1fkg (%.1f laps remaining, Mix: %s)",
		state.FuelInTank, state.FuelRemainingLaps, fuelMix))
	parts = append(parts, fmt.Sprintf("ERS: %.0f%% (%s)", ersPct, ersMode))
	switch {
	case state.DRSFault > 0:
		parts = append(parts, "DRS: FAULT")
	case state.DRS > 0:
		parts = append(parts, "DRS: Open (active)")
	case state.DRSAllowed > 0:
		parts = append(parts, "DRS: Available (closed)")
	default:
		parts = append(parts, "DRS: Not available")
	}

	// Damage
	var dmg []string
	addDmg := func(name string, val uint8) {
		if val > 0 {
			dmg = append(dmg, fmt.Sprintf("%s:%d%%", name, val))
		}
	}
	addDmg("FL Wing", state.FrontLeftWingDmg)
	addDmg("FR Wing", state.FrontRightWingDmg)
	addDmg("Rear Wing", state.RearWingDmg)
	addDmg("Floor", state.FloorDmg)
	addDmg("Diffuser", state.DiffuserDmg)
	addDmg("Sidepod", state.SidepodDmg)
	addDmg("Gearbox", state.GearBoxDmg)
	addDmg("Engine", state.EngineDmg)
	if len(dmg) > 0 {
		parts = append(parts, "DAMAGE: "+strings.Join(dmg, ", "))
	}

	// Session conditions
	weather := weatherName(state.Weather)
	parts = append(parts, fmt.Sprintf("Weather: %s, Track: %dC, Air: %dC, Rain: %d%%",
		weather, state.TrackTemp, state.AirTemp, state.RainPercentage))
	if state.SafetyCarStatus > 0 {
		parts = append(parts, fmt.Sprintf("SAFETY CAR: %s", safetyCarName(state.SafetyCarStatus)))
	}

	// Pit window
	if state.PitWindowIdeal > 0 || state.PitWindowLatest > 0 {
		parts = append(parts, fmt.Sprintf("Pit window: ideal lap %d, latest lap %d",
			state.PitWindowIdeal, state.PitWindowLatest))
	}

	// Session time
	if state.SessionTimeLeft > 0 {
		parts = append(parts, fmt.Sprintf("Time left: %dm %ds",
			state.SessionTimeLeft/60, state.SessionTimeLeft%60))
	}

	return strings.Join(parts, ", ")
}

// --- enum helpers ---
//
// Thin wrappers over internal/enums so existing call sites in this package
// (BuildContext above, analyst.go, comms_gate.go) keep their familiar names.
// The canonical decoders live in the enums package.

func compoundName(c uint8) string      { return enums.Compound(c) }
func fuelMixName(m uint8) string       { return enums.FuelMix(m) }
func ersModeName(m uint8) string       { return enums.ERSDeployMode(m) }
func weatherName(w uint8) string       { return enums.Weather(w) }
func pitStatusName(s uint8) string     { return enums.PitStatus(s) }
func driverStatusName(s uint8) string  { return enums.DriverStatus(s) }
func safetyCarName(s uint8) string     { return enums.SafetyCar(s) }
func trackName(id int) string          { return enums.Track(id) }
func sessionTypeName(t int) string     { return enums.SessionType(t) }

// LoadFileWithFallback tries workspace/primary first, then root/secondary, then returns fallback.
func LoadFileWithFallback(workspaceDir, primary, secondary, fallback string) string {
	// Try workspace directory first (e.g. workspace/soul.md)
	path := filepath.Join(workspaceDir, primary)
	data, err := os.ReadFile(path)
	if err == nil {
		log.Info().Str("file", path).Msg("Loaded personality file from workspace")
		return string(data)
	}

	// Fall back to project root (e.g. SOUL.md)
	data, err = os.ReadFile(secondary)
	if err == nil {
		log.Info().Str("file", secondary).Msg("Loaded personality file from project root")
		return string(data)
	}

	log.Warn().Str("primary", path).Str("secondary", secondary).Msg("Could not load personality file, using default")
	return fallback
}
