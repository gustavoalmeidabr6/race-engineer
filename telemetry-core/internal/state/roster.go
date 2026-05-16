// Package state holds runtime, in-memory state that is shared across the
// ingestion writer and the API handlers but is not persisted to DuckDB.
//
// The Roster is the canonical car_index → driver-name lookup for the active
// session. It is updated whenever the F1 game emits a Participants packet
// (~5Hz, every car), reset on SessionUID change, and read by the analyst,
// CommsGate, insight engine, and dashboard so all downstream components agree
// on what to call each car. The goal: never have the engineer say "car 14"
// when it could say "Hamilton".
package state

import (
	"strings"
	"sync"
	"unicode"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/packets"
)

// DriverInfo is the cached, render-ready facts about a single car. Surname
// is the radio-style display name (no first name, no number). Source records
// where the surname came from so callers can diagnose unexpected fallbacks.
type DriverInfo struct {
	Surname     string `json:"surname"`
	Team        string `json:"team,omitempty"`
	RaceNumber  uint8  `json:"race_number,omitempty"`
	IsPlayer    bool   `json:"is_player,omitempty"`
	IsAI        bool   `json:"is_ai,omitempty"`
	NetworkName string `json:"network_name,omitempty"` // raw Name[] from packet, when it looked like a real surname
	Source      string `json:"source"`                 // "name", "driver_id", "team", "position", "unknown"
}

// Roster tracks every car in the current session by car_index. Reads and
// writes go through a single RWMutex — Update fires ~5Hz (cheap), reads can
// fire on every API request (also cheap). No atomic.Pointer optimisation
// needed for this rate.
type Roster struct {
	mu         sync.RWMutex
	sessionUID uint64
	cars       map[uint8]DriverInfo
}

// NewRoster returns an empty roster.
func NewRoster() *Roster {
	return &Roster{cars: make(map[uint8]DriverInfo)}
}

// Update consumes a parsed Participants packet for the given session uid.
// If the session uid changed, the previous roster is discarded — driver ids
// can shuffle between sessions (custom championships, online lobbies) and we
// never want to surface stale names. playerCarIndex is needed to mark the
// player's own slot.
func (r *Roster) Update(sessionUID uint64, playerCarIndex uint8, numActiveCars uint8, participants [22]packets.ParticipantData) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if sessionUID != r.sessionUID {
		r.cars = make(map[uint8]DriverInfo, 22)
		r.sessionUID = sessionUID
	}

	limit := int(numActiveCars)
	if limit > 22 || limit == 0 {
		limit = 22
	}
	for i := 0; i < limit; i++ {
		p := participants[i]
		idx := uint8(i)
		surname, source := resolveSurname(&p)
		info := DriverInfo{
			Surname:    surname,
			Team:       packets.TeamIDName(p.TeamID),
			RaceNumber: p.RaceNumber,
			IsPlayer:   idx == playerCarIndex,
			IsAI:       p.AIControlled == 1,
			Source:     source,
		}
		// Stash the raw Name[] only when it was the source — saves bytes for
		// the common AI-roster case without losing diagnostic value.
		if source == "name" {
			info.NetworkName = p.GetName()
		}
		r.cars[idx] = info
	}
}

// SetPositionalFallback fills any car_index entries that resolved to "" with
// "P{position}" labels using a positional map provided by the caller. Run
// from the writer when lap_data lands so cars without a useful surname still
// get a radio-friendly tag like "P5". `positions` maps car_index → race
// position (1-based). Missing positions are left as the existing surname.
func (r *Roster) SetPositionalFallback(positions map[uint8]uint8) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for idx, info := range r.cars {
		if info.Surname != "" && info.Source != "team" && info.Source != "unknown" {
			continue
		}
		pos, ok := positions[idx]
		if !ok || pos == 0 {
			continue
		}
		info.Surname = posLabel(pos)
		info.Source = "position"
		r.cars[idx] = info
	}
}

// Resolve returns the surname for a car_index, or "" if unknown. Cheap RLock.
//
// NOTE: this returns the Surname *as stored*, which includes positional
// fallbacks like "P5" for unidentified AI cars. Callers that only want a
// real driver name (so the engineer doesn't say "P5 closing" when it
// really means "the car at P5") should use ResolveRadioName instead.
func (r *Roster) Resolve(idx uint8) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	info, ok := r.cars[idx]
	if !ok {
		return ""
	}
	return info.Surname
}

// ResolveRadioName returns the driver name only when it came from a real
// source — gamertag, driver-id enum, or team name. Returns "" for the
// positional fallback ("P5") and for the unknown case so callers can
// build their own context-aware label (e.g., "P5 ahead").
//
// This is the right call for radio-message construction: a real engineer
// would never voice "P5 is closing" — they'd say "Hamilton" if known,
// otherwise "the car ahead" or "the McLaren". Reserving Resolve() for
// dashboard display preserves backwards compatibility.
func (r *Roster) ResolveRadioName(idx uint8) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	info, ok := r.cars[idx]
	if !ok {
		return ""
	}
	switch info.Source {
	case "name", "driver_id", "team":
		return info.Surname
	}
	return ""
}

// Lookup returns the full DriverInfo for a car_index, or zero-value and false.
func (r *Roster) Lookup(idx uint8) (DriverInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	info, ok := r.cars[idx]
	return info, ok
}

// Snapshot returns a copy of the full roster keyed by car_index. Suitable
// for JSON marshalling. Returns nil if the roster has never been populated
// so omitempty fields drop cleanly.
func (r *Roster) Snapshot() map[uint8]DriverInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.cars) == 0 {
		return nil
	}
	out := make(map[uint8]DriverInfo, len(r.cars))
	for k, v := range r.cars {
		out[k] = v
	}
	return out
}

// SessionUID exposes the uid of the session whose participants are currently
// loaded — useful for debugging stale-roster scenarios.
func (r *Roster) SessionUID() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sessionUID
}

// resolveSurname picks the best display name for a participant. Priority:
//  1. Name[] from the packet, if it passes the gamertag heuristic.
//  2. Static DriverID enum lookup.
//  3. Team name (so the engineer can say "the McLaren") if neither hit.
//  4. "" — caller falls back to the positional label later.
//
// The split between source values lets the gamertag heuristic stay tunable
// without scattering logic across the rest of the codebase.
func resolveSurname(p *packets.ParticipantData) (surname, source string) {
	raw := p.GetName()
	if looksLikeRealName(raw) {
		return raw, "name"
	}
	if name := packets.DriverIDName(p.DriverID); name != "" {
		return name, "driver_id"
	}
	if team := packets.TeamIDName(p.TeamID); team != "" {
		return team, "team"
	}
	return "", "unknown"
}

// looksLikeRealName is the gamertag filter. We accept a name only if every
// signal we can cheaply check matches a typical European-style F1 surname:
// length within reason, no digits, no underscores, mostly letters, no
// spaces-other-than-internal (e.g. "de Vries" is fine).
//
// The bias is intentional: false negatives (rejecting an unusual real name
// like "AL-OBAIDLY") just push us to the DriverID enum or positional
// fallback, both of which are still useful. False positives (accepting a
// gamertag like "MaxFan33") cause the engineer to say "MaxFan33 is closing"
// over the radio — way worse.
func looksLikeRealName(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || len(s) < 2 || len(s) > 16 {
		return false
	}
	letters := 0
	for _, r := range s {
		switch {
		case unicode.IsDigit(r):
			return false
		case r == '_':
			return false
		case r == '-' || r == '\'' || r == ' ':
			// Allowed: hyphen ("Pérez-Romero"), apostrophe, space ("de Vries").
			continue
		case unicode.IsLetter(r):
			letters++
		default:
			return false
		}
	}
	if letters < 2 {
		return false
	}
	return true
}

// posLabel maps a race position to a radio-friendly tag.
func posLabel(pos uint8) string {
	if pos == 0 {
		return ""
	}
	return "P" + itoa(pos)
}

// itoa is a tiny no-fmt-import integer formatter for the position fallback.
func itoa(n uint8) string {
	if n == 0 {
		return "0"
	}
	var buf [3]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
