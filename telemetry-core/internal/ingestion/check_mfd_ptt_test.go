package ingestion

import (
	"testing"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/packets"
)

// buildMFDPacket builds a minimal CarTelemetry packet that carries a given
// MFDPanelIndex. Only the field checkMFDPTT reads is set; the rest of the
// packet uses zero values.
func buildMFDPacket(mfd uint8) []byte {
	pkt := packets.PacketCarTelemetryData{}
	pkt.Header = makeHeader(6, 0)
	pkt.MFDPanelIndex = mfd
	return serialize(&pkt)
}

// TestCheckMFDPTT_FirstPacketLatches — the very first packet seen by the
// writer only seeds the previous-index state and emits nothing. Otherwise
// startup would synthesise a spurious toggle when the listener comes up
// mid-stint.
func TestCheckMFDPTT_FirstPacketLatches(t *testing.T) {
	ch := make(chan bool, 4)
	var prev uint8
	first := true
	var active bool

	checkMFDPTT(buildMFDPacket(2), "toggle", &prev, &first, &active, ch)

	if first {
		t.Errorf("first should flip to false after seeding")
	}
	if prev != 2 {
		t.Errorf("expected prev=2 after seed, got %d", prev)
	}
	if got := drainPTT(ch); len(got) != 0 {
		t.Errorf("expected no PTT events on first packet, got %v", got)
	}
}

// TestCheckMFDPTT_NoChangeIsSilent — repeated identical MFD indices must
// not fire toggles. F1 25 broadcasts the index as a snapshot at 60Hz so
// >99% of packets carry the same value.
func TestCheckMFDPTT_NoChangeIsSilent(t *testing.T) {
	ch := make(chan bool, 4)
	var prev uint8
	first := true
	var active bool

	// Seed.
	checkMFDPTT(buildMFDPacket(2), "toggle", &prev, &first, &active, ch)
	// Two more packets with the same index.
	checkMFDPTT(buildMFDPacket(2), "toggle", &prev, &first, &active, ch)
	checkMFDPTT(buildMFDPacket(2), "toggle", &prev, &first, &active, ch)

	if got := drainPTT(ch); len(got) != 0 {
		t.Errorf("no-change packets should be silent, got %v", got)
	}
}

// TestCheckMFDPTT_ToggleOnChange — each distinct MFD index change is one
// toggle, regardless of which direction (next / prev) it moved.
func TestCheckMFDPTT_ToggleOnChange(t *testing.T) {
	ch := make(chan bool, 8)
	var prev uint8
	first := true
	var active bool

	// Seed at index 0.
	checkMFDPTT(buildMFDPacket(0), "toggle", &prev, &first, &active, ch)
	// 0 → 1 → 2 → 1 → 0: four changes, four toggles.
	checkMFDPTT(buildMFDPacket(1), "toggle", &prev, &first, &active, ch)
	checkMFDPTT(buildMFDPacket(2), "toggle", &prev, &first, &active, ch)
	checkMFDPTT(buildMFDPacket(1), "toggle", &prev, &first, &active, ch)
	checkMFDPTT(buildMFDPacket(0), "toggle", &prev, &first, &active, ch)

	got := drainPTT(ch)
	want := []bool{true, false, true, false}
	if len(got) != len(want) {
		t.Fatalf("expected %d toggles, got %d (%v)", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("toggle[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestCheckMFDPTT_HoldModePulses — hold mode on a discrete signal emits
// a brief on→off pulse per MFD change so downstream consumers see the
// "press" without staying latched.
func TestCheckMFDPTT_HoldModePulses(t *testing.T) {
	ch := make(chan bool, 8)
	var prev uint8
	first := true
	var active bool

	// Seed.
	checkMFDPTT(buildMFDPacket(0), "hold", &prev, &first, &active, ch)
	// One change → expect on,off pair.
	checkMFDPTT(buildMFDPacket(1), "hold", &prev, &first, &active, ch)

	got := drainPTT(ch)
	if len(got) != 2 || got[0] != true || got[1] != false {
		t.Fatalf("expected [true,false] pulse, got %v", got)
	}
}
