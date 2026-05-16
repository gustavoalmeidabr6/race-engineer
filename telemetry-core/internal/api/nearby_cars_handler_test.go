package api

import (
	"testing"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/insights"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
)

// stubRoster fakes state.Roster's ResolveRadioName so tests can assert
// driver-name resolution without standing up a full Roster pipeline.
type stubRoster struct{ names map[uint8]string }

func (r *stubRoster) ResolveRadioName(idx uint8) string { return r.names[idx] }

// buildGrid populates state.GridPositions with 22 cars spaced evenly
// around the lap (lapLen / 22 metres apart) starting at offset 0. The
// player sits at the midpoint so we get a symmetric ahead / behind
// split out of the box. Cars at indices in `inactive` are left at
// ResultStatus=0 (slot empty); `inPit` flips their PitStatus to 1.
func buildGrid(t *testing.T, lapLen float32, playerIdx uint8, inactive, inPit map[uint8]bool) *models.RaceState {
	t.Helper()
	st := &models.RaceState{
		TrackLength:    uint16(lapLen),
		PlayerCarIndex: playerIdx,
		Speed:          200,
		DriverStatus:   4,
		PitStatus:      0,
	}
	step := lapLen / 22
	playerDist := float32(playerIdx) * step
	st.LapDistance = playerDist
	for i := uint8(0); i < 22; i++ {
		gp := models.GridPosition{
			CarIndex:     i,
			Position:     i + 1,
			CurrentLap:   3,
			ResultStatus: 2,
			DriverStatus: 4,
			PitStatus:    0,
			LapDistance:  float32(i) * step,
		}
		if inactive[i] {
			gp.ResultStatus = 0
		}
		if inPit[i] {
			gp.PitStatus = 1
		}
		st.GridPositions[i] = gp
	}
	return st
}

// TestSelectNearbyCars_FullGrid — with every car active, the player at
// index 11 should see 5 cars on each side ordered by absolute distance.
func TestSelectNearbyCars_FullGrid(t *testing.T) {
	st := buildGrid(t, 5500, 11, nil, nil)
	ahead, behind := selectNearbyCars(st, nil, nil)
	if len(ahead) < 5 || len(behind) < 5 {
		t.Fatalf("expected at least 5 ahead+behind, got ahead=%d behind=%d", len(ahead), len(behind))
	}
	for i := 1; i < len(ahead); i++ {
		if ahead[i].DistanceM < ahead[i-1].DistanceM {
			t.Errorf("ahead list not sorted ascending at i=%d: %.0f then %.0f", i, ahead[i-1].DistanceM, ahead[i].DistanceM)
		}
	}
	for i := 1; i < len(behind); i++ {
		if behind[i].DistanceM < behind[i-1].DistanceM {
			t.Errorf("behind list not sorted ascending at i=%d: %.0f then %.0f", i, behind[i-1].DistanceM, behind[i].DistanceM)
		}
	}
	// Player must not appear in either list.
	for _, c := range append(append([]insights.NearbyCar{}, ahead...), behind...) {
		if c.CarIndex == 11 {
			t.Errorf("player car_index leaked into nearby list")
		}
	}
	// Signed sign invariant — ahead positive, behind negative.
	for _, c := range ahead {
		if c.SignedM < 0 {
			t.Errorf("ahead car #%d has negative signed_m %.0f", c.CarIndex, c.SignedM)
		}
	}
	for _, c := range behind {
		if c.SignedM >= 0 {
			t.Errorf("behind car #%d has non-negative signed_m %.0f", c.CarIndex, c.SignedM)
		}
	}
}

// TestSelectNearbyCars_FiltersPitAndInactive — pit-lane cars and inactive
// slots must be skipped entirely.
func TestSelectNearbyCars_FiltersPitAndInactive(t *testing.T) {
	// Car 12 (one slot ahead of the player) is in the pit; car 10 (one
	// slot behind) is an inactive slot. The closest ahead must become
	// car 13, and the closest behind must become car 9.
	st := buildGrid(t, 5500, 11,
		map[uint8]bool{10: true},
		map[uint8]bool{12: true},
	)
	ahead, behind := selectNearbyCars(st, nil, nil)
	if len(ahead) == 0 || ahead[0].CarIndex != 13 {
		t.Errorf("expected closest ahead = 13 (12 in pit), got %+v", firstIdx(ahead))
	}
	if len(behind) == 0 || behind[0].CarIndex != 9 {
		t.Errorf("expected closest behind = 9 (10 inactive), got %+v", firstIdx(behind))
	}
	for _, c := range ahead {
		if c.CarIndex == 12 || c.CarIndex == 10 {
			t.Errorf("filter leaked car_index=%d", c.CarIndex)
		}
	}
	for _, c := range behind {
		if c.CarIndex == 12 || c.CarIndex == 10 {
			t.Errorf("filter leaked car_index=%d", c.CarIndex)
		}
	}
}

// TestSelectNearbyCars_PlayerAtBack — when the player is at the front of
// the lap-distance ordering, ahead+behind should still total ≤21 with no
// crossover, and the most-distant car can still be reachable behind via
// wrap.
func TestSelectNearbyCars_PlayerAtBack(t *testing.T) {
	// Place the player at index 0 — lap distance ~0. Every other active
	// car is somewhere later in the lap. With track wrap, half of them
	// (the far side) become "behind" via wrap-distance.
	st := buildGrid(t, 5500, 0, nil, nil)
	ahead, behind := selectNearbyCars(st, nil, nil)
	if got := len(ahead) + len(behind); got > 21 {
		t.Errorf("ahead+behind=%d, want ≤21 (excluding self)", got)
	}
	if got := len(ahead) + len(behind); got < 21 {
		// With 22 cars all active and one excluded (self), every other car
		// must classify into one side or the other.
		t.Errorf("ahead+behind=%d, want exactly 21", got)
	}
}

// TestSelectNearbyCars_RosterDriverNames — driver_name is filled from
// the roster when available.
func TestSelectNearbyCars_RosterDriverNames(t *testing.T) {
	st := buildGrid(t, 5500, 11, nil, nil)
	roster := &stubRoster{names: map[uint8]string{12: "Verstappen", 10: "Hamilton"}}
	ahead, behind := selectNearbyCars(st, nil, roster)
	if len(ahead) == 0 || ahead[0].DriverName != "Verstappen" {
		t.Errorf("expected closest ahead = Verstappen, got %+v", firstIdx(ahead))
	}
	if len(behind) == 0 || behind[0].DriverName != "Hamilton" {
		t.Errorf("expected closest behind = Hamilton, got %+v", firstIdx(behind))
	}
}

// TestClampNearbyCount — out-of-range values clamp to the handler bounds.
func TestClampNearbyCount(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, 1}, {-5, 1}, {1, 1}, {5, 5}, {15, 15}, {99, 15},
	}
	for _, c := range cases {
		if got := clampNearbyCount(c.in); got != c.want {
			t.Errorf("clampNearbyCount(%d)=%d, want %d", c.in, got, c.want)
		}
	}
}

// TestCapList — capping a list keeps the first N entries (which are
// already sorted closest-first by selectNearbyCars).
func TestCapList(t *testing.T) {
	in := []insights.NearbyCar{
		{CarIndex: 1, DistanceM: 10},
		{CarIndex: 2, DistanceM: 20},
		{CarIndex: 3, DistanceM: 30},
	}
	got := capList(in, 2)
	if len(got) != 2 || got[0].CarIndex != 1 || got[1].CarIndex != 2 {
		t.Errorf("capList(2) = %+v, want [1,2]", got)
	}
	// N larger than the list returns the full list.
	if got := capList(in, 99); len(got) != 3 {
		t.Errorf("capList(99) lost entries: %+v", got)
	}
}

func firstIdx(list []insights.NearbyCar) any {
	if len(list) == 0 {
		return "<empty>"
	}
	return list[0]
}
