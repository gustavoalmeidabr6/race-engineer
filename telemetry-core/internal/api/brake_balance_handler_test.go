package api

import (
	"math"
	"testing"
)

// TestDetectBrakeEvents groups consecutive brake samples into one event
// and skips runs below the minimum length.
func TestDetectBrakeEvents(t *testing.T) {
	samples := []brakeBalanceSample{
		// 5-sample baseline (no brake)
		{trackPos: 100, brake: 0.0, surfTempFL: 100, surfTempFR: 100, surfTempRL: 100, surfTempRR: 100, pressFL: 25, pressFR: 25, pressRL: 25, pressRR: 25},
		{trackPos: 110, brake: 0.0, surfTempFL: 100, surfTempFR: 100, surfTempRL: 100, surfTempRR: 100, pressFL: 25, pressFR: 25, pressRL: 25, pressRR: 25},
		{trackPos: 120, brake: 0.0, surfTempFL: 100, surfTempFR: 100, surfTempRL: 100, surfTempRR: 100, pressFL: 25, pressFR: 25, pressRL: 25, pressRR: 25},
		{trackPos: 130, brake: 0.0, surfTempFL: 100, surfTempFR: 100, surfTempRL: 100, surfTempRR: 100, pressFL: 25, pressFR: 25, pressRL: 25, pressRR: 25},
		{trackPos: 140, brake: 0.0, surfTempFL: 100, surfTempFR: 100, surfTempRL: 100, surfTempRR: 100, pressFL: 25, pressFR: 25, pressRL: 25, pressRR: 25},
		// Brake event — 4 consecutive samples, FL runs 40°C hotter than FR,
		// surface temp on FL spikes (lock-up signature).
		{trackPos: 800, brake: 0.8, brakeTempFL: 600, brakeTempFR: 560, brakeTempRL: 500, brakeTempRR: 500, surfTempFL: 140, surfTempFR: 105, surfTempRL: 100, surfTempRR: 100, pressFL: 24.8, pressFR: 25, pressRL: 25, pressRR: 25},
		{trackPos: 810, brake: 0.9, brakeTempFL: 620, brakeTempFR: 580, brakeTempRL: 510, brakeTempRR: 510, surfTempFL: 145, surfTempFR: 110, surfTempRL: 100, surfTempRR: 100, pressFL: 24.7, pressFR: 25, pressRL: 25, pressRR: 25},
		{trackPos: 820, brake: 0.9, brakeTempFL: 630, brakeTempFR: 585, brakeTempRL: 515, brakeTempRR: 515, surfTempFL: 148, surfTempFR: 112, surfTempRL: 100, surfTempRR: 100, pressFL: 24.7, pressFR: 25, pressRL: 25, pressRR: 25},
		{trackPos: 830, brake: 0.6, brakeTempFL: 635, brakeTempFR: 590, brakeTempRL: 520, brakeTempRR: 520, surfTempFL: 150, surfTempFR: 113, surfTempRL: 100, surfTempRR: 100, pressFL: 24.6, pressFR: 25, pressRL: 25, pressRR: 25},
		// Post-brake cool-down (lower brake + reduced press FL)
		{trackPos: 850, brake: 0.0, surfTempFL: 145, surfTempFR: 108, surfTempRL: 100, surfTempRR: 100, pressFL: 24.5, pressFR: 25, pressRL: 25, pressRR: 25},
		{trackPos: 860, brake: 0.0, surfTempFL: 142, surfTempFR: 105, surfTempRL: 100, surfTempRR: 100, pressFL: 24.5, pressFR: 25, pressRL: 25, pressRR: 25},
	}

	events := detectBrakeEvents(samples, 21, 5891)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	e := events[0]
	if e.DurationSamples != 4 {
		t.Errorf("expected 4 samples, got %d", e.DurationSamples)
	}
	if e.DeltaFrontTempLR == nil || math.Abs(*e.DeltaFrontTempLR-(635-590)) > 0.01 {
		t.Errorf("expected front L-R delta 45, got %v", e.DeltaFrontTempLR)
	}
	if !containsStr(e.LockUpFlags, "FL") {
		t.Errorf("expected FL lock-up, got %v", e.LockUpFlags)
	}
}

func containsStr(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// TestDiagnoseBrakeBalanceFLHot flags front-left hot when multiple events
// show >30°C L-R delta.
func TestDiagnoseBrakeBalanceFLHot(t *testing.T) {
	d := func(v float64) *float64 { return &v }
	events := []brakeEvent{
		{DeltaFrontTempLR: d(45), FrontRearBiasTempC: d(50)},
		{DeltaFrontTempLR: d(50), FrontRearBiasTempC: d(60)},
	}
	out := diagnoseBrakeBalance(events)
	if len(out) == 0 {
		t.Fatal("expected at least one diagnosis string")
	}
	found := false
	for _, s := range out {
		if containsSubstring(s, "front-left") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected front-left mention, got %v", out)
	}
}

func containsSubstring(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
