// Package enums decodes F1 25 telemetry integer enums into human-readable
// strings. It is the single source of truth for these mappings — both the
// intelligence layer (LLM context strings) and the api layer (state-bundle
// handlers) call into it. Reference table lives in CLAUDE.md.
package enums

import "fmt"

// Compound decodes the visual tyre compound — what the driver and engineer
// actually call out on the radio: Soft, Medium, Hard, Inter, Wet. Source
// field: car_status.visual_tyre_compound.
func Compound(c uint8) string {
	switch c {
	case 16:
		return "Soft"
	case 17:
		return "Medium"
	case 18:
		return "Hard"
	case 7:
		return "Inter"
	case 8:
		return "Wet"
	default:
		return "Unknown"
	}
}

// ActualCompound decodes the underlying C-grade Pirelli code (C0–C5) used
// by F1 25's car_status.actual_tyre_compound. Different from the visual
// Soft/Medium/Hard which depend on which three compounds the FIA selected
// for the weekend. Use this when the engineer needs precise grade detail
// (e.g. "we're on C3, they're on C4"); use Compound() for normal radio.
func ActualCompound(c uint8) string {
	switch c {
	case 16:
		return "C5"
	case 17:
		return "C4"
	case 18:
		return "C3"
	case 19:
		return "C2"
	case 20:
		return "C1"
	case 21:
		return "C0"
	case 7:
		return "Inter"
	case 8:
		return "Wet"
	default:
		return "Unknown"
	}
}

// FuelMix decodes fuel_mix mode.
func FuelMix(m uint8) string {
	switch m {
	case 0:
		return "Lean"
	case 1:
		return "Standard"
	case 2:
		return "Rich"
	case 3:
		return "Max"
	default:
		return "Unknown"
	}
}

// ERSDeployMode decodes ers_deploy_mode.
func ERSDeployMode(m uint8) string {
	switch m {
	case 0:
		return "None"
	case 1:
		return "Medium"
	case 2:
		return "Hotlap"
	case 3:
		return "Overtake"
	default:
		return "Unknown"
	}
}

// Weather decodes the weather code.
func Weather(w uint8) string {
	switch w {
	case 0:
		return "Clear"
	case 1:
		return "Light Cloud"
	case 2:
		return "Overcast"
	case 3:
		return "Light Rain"
	case 4:
		return "Heavy Rain"
	case 5:
		return "Storm"
	default:
		return "Unknown"
	}
}

// PitStatus decodes lap_data.pit_status.
func PitStatus(s uint8) string {
	switch s {
	case 0:
		return "On Track"
	case 1:
		return "Pit Lane"
	case 2:
		return "In Pit Area"
	default:
		return "Unknown"
	}
}

// DriverStatus decodes lap_data.driver_status.
func DriverStatus(s uint8) string {
	switch s {
	case 0:
		return "In Garage"
	case 1:
		return "Flying Lap"
	case 2:
		return "In Lap"
	case 3:
		return "Out Lap"
	case 4:
		return "On Track"
	default:
		return "Unknown"
	}
}

// SafetyCar decodes session_data.safety_car_status.
func SafetyCar(s uint8) string {
	switch s {
	case 0:
		return "None"
	case 1:
		return "Full"
	case 2:
		return "Virtual"
	case 3:
		return "Formation Lap"
	default:
		return "Unknown"
	}
}

var trackNames = map[int]string{
	0: "Melbourne", 1: "Paul Ricard", 2: "Shanghai", 3: "Bahrain",
	4: "Catalunya", 5: "Monaco", 6: "Montreal", 7: "Silverstone",
	8: "Hockenheim", 9: "Hungaroring", 10: "Spa", 11: "Monza",
	12: "Singapore", 13: "Suzuka", 14: "Abu Dhabi", 15: "Austin",
	16: "Interlagos", 17: "Red Bull Ring", 18: "Sochi", 19: "Mexico City",
	20: "Baku", 21: "Sakhir Short", 22: "Silverstone Short",
	23: "Austin Short", 24: "Suzuka Short", 25: "Hanoi",
	26: "Zandvoort", 27: "Imola", 28: "Portimao", 29: "Jeddah",
	30: "Miami", 31: "Las Vegas", 32: "Losail",
}

// Track decodes session_data.track_id. Returns "Track-N" for unknown ids so
// callers can still cite a stable token.
func Track(id int) string {
	if n, ok := trackNames[id]; ok {
		return n
	}
	return fmt.Sprintf("Track-%d", id)
}

var sessionTypeNames = map[int]string{
	0: "Unknown", 1: "P1", 2: "P2", 3: "P3", 4: "Short P",
	5: "Q1", 6: "Q2", 7: "Q3", 8: "Short Q", 9: "OSQ",
	10: "Race", 11: "Race 2", 12: "Race 3", 13: "Time Trial",
}

// SessionType decodes session_data.session_type.
func SessionType(t int) string {
	if n, ok := sessionTypeNames[t]; ok {
		return n
	}
	return fmt.Sprintf("Session-%d", t)
}

// EventCode decodes the 4-char F1 25 event_code into a human label. Codes
// come from packet 3 (Event). The set is closed and listed in
// packets/event.go.
func EventCode(code string) string {
	switch code {
	case "SSTA":
		return "Session Started"
	case "SEND":
		return "Session Ended"
	case "FTLP":
		return "Fastest Lap"
	case "RTMT":
		return "Retirement"
	case "DRSE":
		return "DRS Enabled"
	case "DRSD":
		return "DRS Disabled"
	case "TMPT":
		return "Teammate in Pits"
	case "CHQF":
		return "Chequered Flag"
	case "RCWN":
		return "Race Winner"
	case "PENA":
		return "Penalty"
	case "SPTP":
		return "Speed Trap"
	case "STLG":
		return "Start Lights"
	case "LGOT":
		return "Lights Out"
	case "DTSV":
		return "Drive-Through Served"
	case "SGSV":
		return "Stop-Go Served"
	case "FLBK":
		return "Flashback"
	case "BUTN":
		return "Button"
	case "OVTK":
		return "Overtake"
	case "SAFC":
		return "Safety Car"
	case "COLL":
		return "Collision"
	case "REDF":
		return "Red Flag"
	default:
		return code
	}
}
