package packets

// DriverIDName returns the canonical surname for a F1 25 driver-id byte.
// The Name[32] field on ParticipantData is the primary source of truth in
// most game modes — this enum is the safety net for sessions where Name[]
// comes through blank (some private lobbies, dev/test packets). Returns "" for
// unknown ids so callers can decide their own fallback.
//
// The id space below is the historically stable Codemasters/EA F1 mapping
// (carried forward across F1 22 → F1 25). New 2025-grid drivers (Antonelli,
// Hadjar, Bortoleto, etc.) are present in the Name[] field even when their
// numeric id is not in this table; the positional fallback in roster.Resolve
// covers any remaining gaps.
func DriverIDName(id uint8) string {
	switch id {
	case 0:
		return "Sainz"
	case 2:
		return "Ricciardo"
	case 3:
		return "Alonso"
	case 6:
		return "Räikkönen"
	case 7:
		return "Hamilton"
	case 9:
		return "Verstappen"
	case 10:
		return "Hülkenberg"
	case 11:
		return "Magnussen"
	case 13:
		return "Vettel"
	case 14:
		return "Pérez"
	case 15:
		return "Bottas"
	case 17:
		return "Ocon"
	case 19:
		return "Stroll"
	case 22:
		return "Albon"
	case 23:
		return "Gasly"
	case 48:
		return "de Vries"
	case 50:
		return "Russell"
	case 54:
		return "Norris"
	case 58:
		return "Leclerc"
	case 74:
		return "Giovinazzi"
	case 75:
		return "Kubica"
	case 80:
		return "Zhou"
	case 81:
		return "Schumacher"
	case 82:
		return "Ilott"
	case 95:
		return "Button"
	case 96:
		return "Coulthard"
	case 97:
		return "Rosberg"
	case 98:
		return "Piastri"
	case 99:
		return "Lawson"
	case 109:
		return "Webber"
	case 110:
		return "Villeneuve"
	case 113:
		return "Sargeant"
	case 119:
		return "Drugovich"
	case 126:
		return "Maloney"
	// Classic icons that F1 includes in heritage modes.
	case 76:
		return "Prost"
	case 77:
		return "Senna"
	case 90:
		return "Schumacher"
	case 91:
		return "Hill"
	}
	return ""
}

// TeamIDName returns the team name for a F1 25 team-id byte. Used as a
// "human-readable" fallback for online lobbies where Name[] is a gamertag —
// the engineer can say "the McLaren behind us" instead of a random handle.
func TeamIDName(id uint8) string {
	switch id {
	case 0:
		return "Mercedes"
	case 1:
		return "Ferrari"
	case 2:
		return "Red Bull"
	case 3:
		return "Williams"
	case 4:
		return "Aston Martin"
	case 5:
		return "Alpine"
	case 6:
		return "RB"
	case 7:
		return "Haas"
	case 8:
		return "McLaren"
	case 9:
		return "Sauber"
	}
	return ""
}
