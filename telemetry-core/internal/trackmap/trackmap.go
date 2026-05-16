// Package trackmap loads hand-curated corner data per F1 25 track id and
// answers the questions the cornerWatcher and the get_track_position tool
// need to answer:
//
//   - "What corner is this lap_distance?" → Lookup by track id, then
//     NextCornerAfter / FindCorner.
//   - "Where is corner T12?" → Resolve by id to a lap_distance.
//
// The data files live at <workspace>/tracks/<track_id>.json. Track ids are
// the F1 25 enum values (see CLAUDE.md). Files missing from disk are not
// errors — the corresponding tracks just return a nil TrackInfo and the
// agent falls back to lap-distance reminders.
//
// Corner positions are HAND-WRITTEN approximations sourced from public F1
// circuit guides and layout references. They are accurate enough for the
// 80–150m lookahead window the reminder engine uses, but should not be
// trusted for sub-50m precision. Drop better data into the JSON file to
// improve a track over time.
package trackmap

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// CornerType lets the agent reason about brake-zone severity without parsing
// the corner name. Values: "slow" (≤120 km/h apex), "medium" (~150–220),
// "fast" (≥220), "kink" (no real braking).
type CornerType string

const (
	CornerSlow   CornerType = "slow"
	CornerMedium CornerType = "medium"
	CornerFast   CornerType = "fast"
	CornerKink   CornerType = "kink"
)

// Corner is one entry in a track's geometry file.
type Corner struct {
	ID            string     `json:"id"`              // "T1", "T12", "T8a"
	Name          string     `json:"name"`            // "Copse", "Stowe"
	LapDistanceM  float32    `json:"lap_distance_m"`  // metres into a lap
	Type          CornerType `json:"type"`
}

// CenterlinePoint is one sample of the curated racing line for a track —
// (lap_distance_m, world x, world z). The live-map outline endpoint and the
// mock generator both consume this so a fresh install / mock session can
// render the real track shape before any lap has been driven.
//
// Points are sorted by lap_distance_m ascending. Density is the operator's
// choice (typically 100–300 points per lap is plenty for the map).
type CenterlinePoint struct {
	LapDistanceM float32 `json:"lap_distance_m"`
	X            float32 `json:"x"`
	Z            float32 `json:"z"`
}

// TrackInfo is the shape of one track JSON file plus a runtime-built lookup.
type TrackInfo struct {
	Name    string   `json:"name"`
	LengthM float32  `json:"length_m"`
	Note    string   `json:"_note,omitempty"` // optional file-level note
	Corners []Corner `json:"corners"`

	// PitEntryM is the lap distance (metres into a lap) where the racing
	// line meets pit lane entry. Used by the plan ticker to fire spatial
	// pit reminders. Zero when not curated for this track — the ticker
	// silently skips track_position triggers in that case.
	PitEntryM float32 `json:"pit_entry_m,omitempty"`

	// PitEntrySide is "L" or "R" (driver's perspective) so radio calls
	// can say "pit entry on the right". Empty when not curated.
	PitEntrySide string `json:"pit_entry_side,omitempty"`

	// PitLaneSpeedKph is the regulation limit — informational, not used
	// for firing. Curated where known; zero otherwise.
	PitLaneSpeedKph int `json:"pit_lane_speed_kph,omitempty"`

	// Centerline is the curated racing-line trace for the live map. Empty
	// when no centerline has been baked for this track yet — callers fall
	// back to the recorded telemetry_hifreq trace.
	Centerline []CenterlinePoint `json:"centerline,omitempty"`

	// byID is built once at load time for O(1) FindCorner lookups.
	byID map[string]*Corner
}

// SamplePosition linear-interpolates the centerline at a given lap_distance.
// Returns (0, 0, false) when no centerline has been baked or the distance
// falls outside the recorded range. Wraps distance into [0, LengthM) when
// the track length is known so callers can pass raw lap_distance freely.
func (t *TrackInfo) SamplePosition(lapDistanceM float32) (x, z float32, ok bool) {
	if t == nil || len(t.Centerline) < 2 {
		return 0, 0, false
	}
	d := lapDistanceM
	if t.LengthM > 0 {
		for d < 0 {
			d += t.LengthM
		}
		for d >= t.LengthM {
			d -= t.LengthM
		}
	}
	// Linear scan — centerlines are ~200 points, cheap enough that a binary
	// search would be premature optimisation.
	for i := 0; i < len(t.Centerline)-1; i++ {
		a, b := t.Centerline[i], t.Centerline[i+1]
		if d >= a.LapDistanceM && d <= b.LapDistanceM {
			span := b.LapDistanceM - a.LapDistanceM
			if span <= 0 {
				return a.X, a.Z, true
			}
			f := (d - a.LapDistanceM) / span
			return a.X + (b.X-a.X)*f, a.Z + (b.Z-a.Z)*f, true
		}
	}
	// Wrap segment: between last point and first point.
	last := t.Centerline[len(t.Centerline)-1]
	first := t.Centerline[0]
	if d >= last.LapDistanceM || d <= first.LapDistanceM {
		return last.X, last.Z, true
	}
	return 0, 0, false
}

// PitEntry returns the curated lap-distance + side for the track. ok is
// false when no pit-entry data is on file — callers should skip spatial
// pit reminders for that track.
func (t *TrackInfo) PitEntry() (distM float64, side string, ok bool) {
	if t == nil || t.PitEntryM <= 0 {
		return 0, "", false
	}
	return float64(t.PitEntryM), t.PitEntrySide, true
}

// FindCorner returns the corner matching id (case-insensitive) or nil.
func (t *TrackInfo) FindCorner(id string) *Corner {
	if t == nil || t.byID == nil {
		return nil
	}
	return t.byID[strings.ToUpper(strings.TrimSpace(id))]
}

// NextCornerAfter returns the corner whose lap_distance is the smallest
// strictly greater than `dist`, wrapping to the first corner if `dist` is
// past the last corner. Returns nil if the track has no corners.
func (t *TrackInfo) NextCornerAfter(dist float32) *Corner {
	if t == nil || len(t.Corners) == 0 {
		return nil
	}
	for i := range t.Corners {
		if t.Corners[i].LapDistanceM > dist {
			return &t.Corners[i]
		}
	}
	return &t.Corners[0]
}

// Registry is a thread-safe lookup keyed by F1 25 track id.
type Registry struct {
	mu     sync.RWMutex
	tracks map[int8]*TrackInfo
}

// NewRegistry returns an empty registry. Call Load(dir) to populate it.
func NewRegistry() *Registry {
	return &Registry{tracks: map[int8]*TrackInfo{}}
}

// Lookup returns the TrackInfo for the given F1 25 track id, or nil if
// none was loaded for that id.
func (r *Registry) Lookup(trackID int8) *TrackInfo {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tracks[trackID]
}

// PitEntry returns the curated pit-entry data for a track id. ok is
// false when the track has no curated pit-entry coordinates — the plan
// ticker uses that to silently skip spatial pit reminders.
//
// Implements the plan.TrackInfoProvider interface so the plan package
// doesn't need to import trackmap at compile time.
func (r *Registry) PitEntry(trackID int) (distM float64, side string, ok bool) {
	if r == nil {
		return 0, "", false
	}
	if trackID < -128 || trackID > 127 {
		return 0, "", false
	}
	t := r.Lookup(int8(trackID))
	if t == nil {
		return 0, "", false
	}
	return t.PitEntry()
}

// Count returns how many tracks were successfully loaded.
func (r *Registry) Count() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.tracks)
}

// Load walks `dir` for *.json files named "<track_id>.json" (e.g. "7.json"
// for Silverstone) and parses each into the registry. Returns the number
// of tracks loaded plus the first error encountered. Missing dir is not
// an error — the registry just stays empty.
func (r *Registry) Load(dir string) (int, error) {
	if dir == "" {
		return 0, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("trackmap: read dir %s: %w", dir, err)
	}
	loaded := 0
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ".json")
		id64, err := strconv.ParseInt(base, 10, 8)
		if err != nil {
			// Skip files that don't parse — allows non-track json (e.g. README).
			continue
		}
		path := filepath.Join(dir, e.Name())
		buf, err := os.ReadFile(path)
		if err != nil {
			return loaded, fmt.Errorf("trackmap: read %s: %w", path, err)
		}
		var t TrackInfo
		if err := json.Unmarshal(buf, &t); err != nil {
			return loaded, fmt.Errorf("trackmap: parse %s: %w", path, err)
		}
		t.byID = make(map[string]*Corner, len(t.Corners))
		for i := range t.Corners {
			t.byID[strings.ToUpper(t.Corners[i].ID)] = &t.Corners[i]
		}
		r.tracks[int8(id64)] = &t
		loaded++
	}
	return loaded, nil
}
