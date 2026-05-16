package state

import (
	"testing"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/packets"
)

// participant builds a ParticipantData with a name copied into the [32]byte
// field. Anything beyond 32 bytes is truncated; null-terminator handling is
// the responsibility of GetName(). Tests only need the writable fields.
func participant(driverID, teamID, raceNumber uint8, ai uint8, name string) packets.ParticipantData {
	p := packets.ParticipantData{
		AIControlled: ai,
		DriverID:     driverID,
		TeamID:       teamID,
		RaceNumber:   raceNumber,
	}
	for i := 0; i < len(name) && i < 32; i++ {
		p.Name[i] = name[i]
	}
	return p
}

func mkRoster(t *testing.T, sessionUID uint64, playerIdx uint8, parts ...packets.ParticipantData) *Roster {
	t.Helper()
	r := NewRoster()
	var arr [22]packets.ParticipantData
	for i, p := range parts {
		arr[i] = p
	}
	r.Update(sessionUID, playerIdx, uint8(len(parts)), arr)
	return r
}

func TestResolve_NamePreferredWhenRealistic(t *testing.T) {
	r := mkRoster(t, 1, 0,
		participant(7, 0, 44, 1, "Hamilton"),
		participant(9, 2, 33, 1, "Verstappen"),
	)
	if got := r.Resolve(0); got != "Hamilton" {
		t.Fatalf("car 0 name: want Hamilton, got %q", got)
	}
	if got := r.Resolve(1); got != "Verstappen" {
		t.Fatalf("car 1 name: want Verstappen, got %q", got)
	}
	info, _ := r.Lookup(0)
	if info.Source != "name" {
		t.Errorf("expected source=name, got %q", info.Source)
	}
	if !info.IsPlayer {
		t.Error("car 0 should be flagged as player")
	}
}

func TestResolve_GamertagFallsBackToDriverID(t *testing.T) {
	cases := []struct {
		gamertag string
		desc     string
	}{
		{"xX_F1GOAT_69_Xx", "underscore + digits"},
		{"MaxFan33", "trailing digits"},
		{"thisistoolongtobeasurname", "too long"},
		{"a", "too short"},
		{"", "empty"},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			r := mkRoster(t, 42, 5, participant(7, 0, 44, 0, tc.gamertag))
			if got := r.Resolve(0); got != "Hamilton" {
				t.Fatalf("expected DriverID fallback to Hamilton, got %q", got)
			}
			info, _ := r.Lookup(0)
			if info.Source != "driver_id" {
				t.Errorf("expected source=driver_id, got %q", info.Source)
			}
		})
	}
}

func TestResolve_TeamFallbackWhenDriverIDUnknown(t *testing.T) {
	// DriverID 250 is not in the enum — should land on the team name.
	r := mkRoster(t, 1, 0, participant(250, 8, 12, 0, "PlayerXYZ_99"))
	if got := r.Resolve(0); got != "McLaren" {
		t.Fatalf("expected McLaren team fallback, got %q", got)
	}
	info, _ := r.Lookup(0)
	if info.Source != "team" {
		t.Errorf("expected source=team, got %q", info.Source)
	}
}

func TestResolve_PositionalFallbackFillsTeamAndUnknown(t *testing.T) {
	r := mkRoster(t, 1, 0,
		participant(250, 8, 12, 0, "PlayerXYZ_99"), // → "McLaren" via team
		participant(251, 250, 88, 0, "X_X_X"),      // → "" (team unknown too)
		participant(7, 0, 44, 1, "Hamilton"),       // → "Hamilton" via name
	)
	r.SetPositionalFallback(map[uint8]uint8{0: 4, 1: 11, 2: 1})

	// Real surname (Hamilton) survives the fallback.
	if got := r.Resolve(2); got != "Hamilton" {
		t.Errorf("Hamilton clobbered by positional fallback: got %q", got)
	}
	// Team-only fallback gets upgraded to the position label (more useful).
	if got := r.Resolve(0); got != "P4" {
		t.Errorf("team→position upgrade: want P4, got %q", got)
	}
	// "Unknown" (no name, no driver id, no team) gets the position label.
	if got := r.Resolve(1); got != "P11" {
		t.Errorf("unknown→position: want P11, got %q", got)
	}
}

func TestUpdate_ResetsOnSessionUIDChange(t *testing.T) {
	r := NewRoster()
	var arr [22]packets.ParticipantData
	arr[0] = participant(7, 0, 44, 1, "Hamilton")
	arr[1] = participant(9, 2, 33, 1, "Verstappen")
	r.Update(100, 0, 2, arr)

	if got := r.Resolve(0); got != "Hamilton" {
		t.Fatalf("setup: want Hamilton, got %q", got)
	}
	if got := r.Resolve(1); got != "Verstappen" {
		t.Fatalf("setup: want Verstappen, got %q", got)
	}

	// New session — single-car grid this time. The old idx 1 must be
	// evicted on session change rather than lingering as a stale slot.
	var arr2 [22]packets.ParticipantData
	arr2[0] = participant(54, 8, 4, 1, "Norris")
	r.Update(200, 0, 1, arr2)

	if _, ok := r.Lookup(1); ok {
		t.Error("car 1 from prior session should have been evicted")
	}
	if got := r.Resolve(0); got != "Norris" {
		t.Errorf("new session: want Norris at idx 0, got %q", got)
	}
}

func TestSnapshot_CopyIndependence(t *testing.T) {
	r := mkRoster(t, 1, 0, participant(7, 0, 44, 1, "Hamilton"))
	snap := r.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot length: want 1, got %d", len(snap))
	}

	// Mutating the snapshot must not affect the live roster.
	mutated := snap[0]
	mutated.Surname = "MUTATED"
	snap[0] = mutated
	if got := r.Resolve(0); got != "Hamilton" {
		t.Errorf("snapshot mutation leaked into live roster: got %q", got)
	}
}

func TestLooksLikeRealName_AcceptsHyphenAndSpace(t *testing.T) {
	if !looksLikeRealName("de Vries") {
		t.Error("'de Vries' should pass the real-name filter")
	}
	if !looksLikeRealName("Pérez") {
		t.Error("'Pérez' (accent) should pass the real-name filter")
	}
	if !looksLikeRealName("O'Sullivan") {
		t.Error("apostrophe surname should pass the real-name filter")
	}
}
