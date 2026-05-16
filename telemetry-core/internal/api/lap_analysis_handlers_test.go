package api

import (
	"math"
	"testing"
)

// TestFindBrakePoint covers the smallest-window brake-point heuristic plus
// the consecutive-sample requirement that filters out single-sample bumps.
func TestFindBrakePoint(t *testing.T) {
	// A synthetic lap where the brake comes on at 2300m sustained for many
	// samples, with one isolated 2050m bump that should be IGNORED because
	// it's not held for ≥ brakePointMinSamples.
	samples := []hifreqSample{
		{trackPos: 1900, brake: 0.0},
		{trackPos: 2000, brake: 0.0},
		{trackPos: 2050, brake: 0.5}, // single-sample spike
		{trackPos: 2100, brake: 0.0},
		{trackPos: 2200, brake: 0.0},
		{trackPos: 2300, brake: 0.4},
		{trackPos: 2350, brake: 0.7}, // sustained → brake point HERE
		{trackPos: 2400, brake: 0.9},
		{trackPos: 2450, brake: 1.0},
	}

	// Corner anchored at 2480m, 200m before / 50m after window.
	bp := findBrakePoint(samples, 2480, 200, 50, 5891)
	if bp == nil {
		t.Fatal("expected brake point near 2300m, got nil")
	}
	if math.Abs(*bp-2300) > 1 {
		t.Errorf("expected brake point ~2300m, got %v", *bp)
	}
}

// TestFindBrakePointNoSustained returns nil when no run of ≥2 samples
// exceeds the threshold inside the window.
func TestFindBrakePointNoSustained(t *testing.T) {
	samples := []hifreqSample{
		{trackPos: 100, brake: 0.0},
		{trackPos: 200, brake: 0.5}, // single sample
		{trackPos: 300, brake: 0.0},
	}
	if bp := findBrakePoint(samples, 250, 200, 50, 5891); bp != nil {
		t.Errorf("expected nil, got %v", *bp)
	}
}

// TestFindBrakePointEmpty handles the no-data case.
func TestFindBrakePointEmpty(t *testing.T) {
	if bp := findBrakePoint(nil, 100, 200, 50, 5891); bp != nil {
		t.Errorf("expected nil for empty samples, got %v", *bp)
	}
}

// TestFindApex picks the slowest sample in [corner-50, corner+100] and
// returns the throttle at that sample.
func TestFindApex(t *testing.T) {
	samples := []hifreqSample{
		{trackPos: 700, speed: 250, throttle: 0.9},
		{trackPos: 720, speed: 200, throttle: 0.5},
		{trackPos: 740, speed: 90, throttle: 0.1}, // apex
		{trackPos: 760, speed: 110, throttle: 0.4},
		{trackPos: 800, speed: 180, throttle: 0.8},
	}
	apex := findApex(samples, 720, 5891)
	if apex == nil {
		t.Fatal("expected apex, got nil")
	}
	if math.Abs(apex.speed-90) > 0.1 {
		t.Errorf("expected apex speed 90, got %v", apex.speed)
	}
	if math.Abs(apex.throttleAtApex-0.1) > 0.01 {
		t.Errorf("expected throttle at apex 0.1, got %v", apex.throttleAtApex)
	}
}

// TestFindApexOutsideWindow returns nil when no samples fall in the apex
// search window.
func TestFindApexOutsideWindow(t *testing.T) {
	samples := []hifreqSample{
		{trackPos: 100, speed: 90},
		{trackPos: 5000, speed: 80},
	}
	if apex := findApex(samples, 720, 5891); apex != nil {
		t.Errorf("expected nil, got %+v", *apex)
	}
}

// TestParseChannelsDedup drops duplicates and unknown channels.
func TestParseChannelsDedup(t *testing.T) {
	got := parseChannels("throttle,brake, throttle,unknown,SPEED")
	if len(got) != 3 {
		t.Fatalf("expected 3 channels, got %d (%+v)", len(got), got)
	}
	wantOrder := []string{"throttle", "brake", "speed"}
	for i, ch := range got {
		if ch.id != wantOrder[i] {
			t.Errorf("channel[%d]=%q, want %q", i, ch.id, wantOrder[i])
		}
	}
}

// TestParseChannelsEmpty returns nil so the handler can apply defaults.
func TestParseChannelsEmpty(t *testing.T) {
	if got := parseChannels(""); got != nil {
		t.Errorf("expected nil for empty, got %v", got)
	}
	if got := parseChannels("   "); got != nil {
		t.Errorf("expected nil for blank, got %v", got)
	}
}

// TestUidStringFullRange covers the high-bit case so the handler's SQL
// literal renders the full uint64 instead of overflowing into negatives.
func TestUidStringFullRange(t *testing.T) {
	if s := uidString(0); s != "0" {
		t.Errorf("zero uid: got %q", s)
	}
	if s := uidString(0xFFFFFFFFFFFFFFFF); s != "18446744073709551615" {
		t.Errorf("max uid: got %q", s)
	}
}
