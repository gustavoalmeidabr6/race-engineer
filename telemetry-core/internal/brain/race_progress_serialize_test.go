package brain

import (
	"strings"
	"testing"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
)

// renderRaceProgress runs writeRaceProgressSection in isolation so we can
// assert on its output without dragging the full Snapshot.Markdown surface.
func renderRaceProgress(s *models.RaceState) string {
	var b strings.Builder
	writeRaceProgressSection(&b, s)
	return b.String()
}

func TestRaceProgress_SkippedForNonRaceSessions(t *testing.T) {
	s := &models.RaceState{SessionType: 5, CurrentLap: 1, TotalLaps: 10}
	if out := renderRaceProgress(s); out != "" {
		t.Errorf("expected empty panel for quali, got %q", out)
	}
}

func TestRaceProgress_GridPhase(t *testing.T) {
	s := &models.RaceState{
		SessionType:  10,
		CurrentLap:   0,
		GridPosition: 7,
		TotalLaps:    53,
		TrackTemp:    32,
	}
	out := renderRaceProgress(s)
	if !strings.Contains(out, "## Race Progress") {
		t.Errorf("missing header: %q", out)
	}
	if !strings.Contains(out, "phase: GRID") || !strings.Contains(out, "starting P7") {
		t.Errorf("grid line missing context: %q", out)
	}
	if !strings.Contains(out, "pre-race") {
		t.Errorf("missing pre-race guidance: %q", out)
	}
}

func TestRaceProgress_LightsOutPhase(t *testing.T) {
	s := &models.RaceState{
		SessionType:   10,
		CurrentLap:    1,
		GridPosition:  3,
		TotalLaps:     50,
		LastEventCode: "LGOT",
	}
	out := renderRaceProgress(s)
	if !strings.Contains(out, "LIGHTS OUT") {
		t.Errorf("expected LIGHTS OUT header line, got %q", out)
	}
	if !strings.Contains(out, "ONE urgent line") {
		t.Errorf("missing lights-out behaviour rule, got %q", out)
	}
}

func TestRaceProgress_RacingPhase(t *testing.T) {
	s := &models.RaceState{
		SessionType:       10,
		CurrentLap:        25,
		TotalLaps:         53,
		Position:          4,
		ActualCompound:    17,
		TyresAgeLaps:      14,
		FuelRemainingLaps: 12.5,
	}
	out := renderRaceProgress(s)
	if !strings.Contains(out, "RACING") || !strings.Contains(out, "lap 25/53") {
		t.Errorf("missing racing context: %q", out)
	}
	if !strings.Contains(out, "28 to go") {
		t.Errorf("missing laps_remaining: %q", out)
	}
}

func TestRaceProgress_LateRaceHint(t *testing.T) {
	s := &models.RaceState{
		SessionType: 10,
		CurrentLap:  49,
		TotalLaps:   53,
		Position:    4,
	}
	out := renderRaceProgress(s)
	if !strings.Contains(out, "LATE RACE") {
		t.Errorf("expected late-race hint with 4 laps to go, got %q", out)
	}
}

func TestRaceProgress_FinalLapPhase(t *testing.T) {
	s := &models.RaceState{
		SessionType: 10,
		CurrentLap:  53,
		TotalLaps:   53,
		Position:    2,
	}
	out := renderRaceProgress(s)
	if !strings.Contains(out, "FINAL LAP") {
		t.Errorf("missing FINAL LAP header: %q", out)
	}
	if !strings.Contains(out, "push or protect") {
		t.Errorf("missing final-lap guidance: %q", out)
	}
}

func TestRaceProgress_FinishedP1(t *testing.T) {
	s := &models.RaceState{
		SessionType:         10,
		CurrentLap:          53,
		TotalLaps:           53,
		RaceFinished:        true,
		FinalClassification: 1,
		GridPosition:        7,
		NumPitStops:         2,
	}
	out := renderRaceProgress(s)
	if !strings.Contains(out, "FINISHED") {
		t.Errorf("missing FINISHED phase header: %q", out)
	}
	if !strings.Contains(out, "final P1") || !strings.Contains(out, "started P7") {
		t.Errorf("missing P1 / grid context: %q", out)
	}
	if !strings.Contains(out, "RACE WIN") || !strings.Contains(strings.ToLower(out), "celebration") {
		t.Errorf("missing P1 celebration override hint: %q", out)
	}
}

func TestRaceProgress_FinishedPodium(t *testing.T) {
	s := &models.RaceState{
		SessionType:         10,
		TotalLaps:           53,
		RaceFinished:        true,
		FinalClassification: 3,
		GridPosition:        5,
	}
	out := renderRaceProgress(s)
	if !strings.Contains(out, "PODIUM") {
		t.Errorf("missing PODIUM framing for P3: %q", out)
	}
}

func TestRaceProgress_FinishedMidfield(t *testing.T) {
	s := &models.RaceState{
		SessionType:         10,
		TotalLaps:           53,
		RaceFinished:        true,
		FinalClassification: 8,
		GridPosition:        12,
	}
	out := renderRaceProgress(s)
	if !strings.Contains(out, "final P8") || !strings.Contains(out, "+4") {
		t.Errorf("expected grid delta in midfield finish, got %q", out)
	}
	if strings.Contains(out, "RACE WIN") {
		t.Errorf("midfield should not get RACE WIN framing: %q", out)
	}
}
