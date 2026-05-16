package intelligence

import (
	"strings"
	"testing"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
)

func TestBuildContextDRSStates(t *testing.T) {
	tests := []struct {
		name    string
		drs     uint8
		allowed uint8
		fault   uint8
		want    string
	}{
		{"open", 1, 1, 0, "DRS: Open (active)"},
		{"available_closed", 0, 1, 0, "DRS: Available (closed)"},
		{"not_available", 0, 0, 0, "DRS: Not available"},
		{"fault_overrides", 1, 1, 1, "DRS: FAULT"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state := &models.RaceState{
				DRS:        tc.drs,
				DRSAllowed: tc.allowed,
				DRSFault:   tc.fault,
			}
			got := BuildContext(state)
			if !strings.Contains(got, tc.want) {
				t.Fatalf("BuildContext missing %q\nfull context: %s", tc.want, got)
			}
		})
	}
}
