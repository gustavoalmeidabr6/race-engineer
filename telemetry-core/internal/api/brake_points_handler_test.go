package api

import (
	"math"
	"testing"
)

// TestStddevOfBrakePoints covers the typical 3-lap input with varied brake
// points and confirms the sample-stddev calculation.
func TestStddevOfBrakePoints(t *testing.T) {
	entries := []brakePointLap{
		{Lap: 21, BrakePointM: floatPtr(700)},
		{Lap: 22, BrakePointM: floatPtr(685)},
		{Lap: 23, BrakePointM: floatPtr(660)},
	}
	std, ok := stddevOfBrakePoints(entries)
	if !ok {
		t.Fatal("expected stddev to compute")
	}
	// (700-681.67)^2 + (685-681.67)^2 + (660-681.67)^2 ≈ 815.5; ÷ 2 ÷ sqrt
	// ≈ 20.21
	if math.Abs(std-20.207) > 0.5 {
		t.Errorf("expected ≈20.2, got %v", std)
	}
}

// TestStddevTooFewPoints returns (0, false) with <2 samples.
func TestStddevTooFewPoints(t *testing.T) {
	if _, ok := stddevOfBrakePoints(nil); ok {
		t.Error("expected ok=false for empty input")
	}
	one := []brakePointLap{{Lap: 1, BrakePointM: floatPtr(700)}}
	if _, ok := stddevOfBrakePoints(one); ok {
		t.Error("expected ok=false for single point")
	}
}

// TestMeanOfDeltas averages the populated delta_to_best_m entries.
func TestMeanOfDeltas(t *testing.T) {
	entries := []brakePointLap{
		{DeltaToBestM: floatPtr(-15)},
		{DeltaToBestM: floatPtr(-25)},
		{DeltaToBestM: floatPtr(-50)},
		{DeltaToBestM: nil}, // ignored
	}
	mean, ok := meanOfDeltas(entries)
	if !ok || math.Abs(mean-(-30)) > 0.01 {
		t.Errorf("expected -30, got %v ok=%v", mean, ok)
	}
}

// TestTrendOfBrakePoints labels a positive-slope series as "braking later"
// and a flat series as "consistent".
func TestTrendOfBrakePoints(t *testing.T) {
	cases := []struct {
		name string
		bps  []float64
		want string
	}{
		{"later", []float64{660, 685, 720}, "braking later and later"},
		{"earlier", []float64{720, 685, 660}, "braking earlier and earlier"},
		{"consistent", []float64{700, 702, 698}, "consistent"},
		{"single", []float64{700}, ""},
	}
	for _, tc := range cases {
		entries := make([]brakePointLap, len(tc.bps))
		for i, v := range tc.bps {
			v := v
			entries[i] = brakePointLap{Lap: i + 1, BrakePointM: &v}
		}
		got := trendOfBrakePoints(entries, std3())
		if got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

// TestMaxBrakeInWindow returns the peak brake reading inside the window.
func TestMaxBrakeInWindow(t *testing.T) {
	samples := []hifreqSample{
		{trackPos: 100, brake: 0.0},
		{trackPos: 700, brake: 0.4},
		{trackPos: 750, brake: 0.95},
		{trackPos: 800, brake: 0.8},
		{trackPos: 1500, brake: 1.0}, // outside window
	}
	v := maxBrakeInWindow(samples, 800, 200, 50, 5000)
	if math.Abs(v-0.95) > 0.001 {
		t.Errorf("expected 0.95, got %v", v)
	}
}
