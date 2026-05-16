package trackmap

import (
	"path/filepath"
	"testing"
)

// TestRegistryLoadsRepoTracks — points the loader at the repo's
// workspace/tracks directory and asserts a baseline set of tracks
// parses cleanly. Drop-in protection against malformed JSON edits.
func TestRegistryLoadsRepoTracks(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "workspace", "tracks")
	r := NewRegistry()
	loaded, err := r.Load(dir)
	if err != nil {
		t.Fatalf("Load(%q) failed: %v", dir, err)
	}
	if loaded < 5 {
		t.Errorf("expected ≥5 tracks loaded from %s, got %d", dir, loaded)
	}

	// Spot-check Silverstone (id 7).
	silverstone := r.Lookup(7)
	if silverstone == nil {
		t.Fatal("expected Silverstone (id 7) loaded")
	}
	if silverstone.Name == "" || silverstone.LengthM == 0 || len(silverstone.Corners) == 0 {
		t.Errorf("Silverstone missing fields: %+v", silverstone)
	}
	copse := silverstone.FindCorner("T9")
	if copse == nil {
		t.Fatal("expected T9 (Copse) on Silverstone")
	}
	if copse.LapDistanceM <= 0 || copse.LapDistanceM >= silverstone.LengthM {
		t.Errorf("Copse lap_distance out of range: %f", copse.LapDistanceM)
	}
}

// TestNextCornerAfter_Wrap — past the last corner should wrap to first.
func TestNextCornerAfter_Wrap(t *testing.T) {
	t1 := &Corner{ID: "T1", LapDistanceM: 100}
	t2 := &Corner{ID: "T2", LapDistanceM: 500}
	t3 := &Corner{ID: "T3", LapDistanceM: 900}
	track := &TrackInfo{
		Name:    "x",
		LengthM: 1000,
		Corners: []Corner{*t1, *t2, *t3},
	}

	if c := track.NextCornerAfter(50); c == nil || c.ID != "T1" {
		t.Errorf("expected T1 after 50, got %+v", c)
	}
	if c := track.NextCornerAfter(200); c == nil || c.ID != "T2" {
		t.Errorf("expected T2 after 200, got %+v", c)
	}
	if c := track.NextCornerAfter(950); c == nil || c.ID != "T1" {
		t.Errorf("expected wrap to T1 after 950, got %+v", c)
	}
}

// TestPitEntry_CuratedTracks — the curated subset must return non-zero
// pit-entry coordinates so the plan ticker's spatial pit reminders fire.
func TestPitEntry_CuratedTracks(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "workspace", "tracks")
	r := NewRegistry()
	if _, err := r.Load(dir); err != nil {
		t.Fatalf("Load(%q) failed: %v", dir, err)
	}
	curated := []int{3, 4, 7, 10, 11, 13, 14}
	for _, id := range curated {
		distM, side, ok := r.PitEntry(id)
		if !ok {
			t.Errorf("track %d: PitEntry not curated", id)
			continue
		}
		track := r.Lookup(int8(id))
		if distM <= 0 || distM >= float64(track.LengthM) {
			t.Errorf("track %d: pit_entry_m out of range: %f (length %f)", id, distM, track.LengthM)
		}
		if side != "left" && side != "right" {
			t.Errorf("track %d: pit_entry_side must be 'left' or 'right', got %q", id, side)
		}
	}
}

// TestPitEntry_UnknownTrack — a non-existent track id returns ok=false.
func TestPitEntry_UnknownTrack(t *testing.T) {
	r := NewRegistry()
	if _, _, ok := r.PitEntry(999); ok {
		t.Errorf("expected ok=false for unknown track id")
	}
}

// TestFindCorner_CaseInsensitive — agent-supplied ids might be lowercase.
func TestFindCorner_CaseInsensitive(t *testing.T) {
	tr := &TrackInfo{
		Corners: []Corner{{ID: "T9", LapDistanceM: 2480}},
		byID:    map[string]*Corner{"T9": {ID: "T9", LapDistanceM: 2480}},
	}
	if c := tr.FindCorner("t9"); c == nil {
		t.Errorf("FindCorner(t9) should be case-insensitive")
	}
	if c := tr.FindCorner(" T9 "); c == nil {
		t.Errorf("FindCorner should trim whitespace")
	}
}
