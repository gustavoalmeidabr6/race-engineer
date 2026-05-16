package ingestion

import (
	"encoding/binary"
	"testing"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/packets"
)

// makeBUTN builds a valid BUTN event packet with the given button status.
// Same construction pattern as buildEventPacket in testhelpers_test.go but
// specialised so we can poke a 32-bit ButtonStatus into EventDetails.
func makeBUTN(status uint32) []byte {
	pkt := packets.PacketEventData{}
	pkt.Header = makeHeader(3, 0)
	copy(pkt.EventStringCode[:], "BUTN")
	binary.LittleEndian.PutUint32(pkt.EventDetails[:4], status)
	return serialize(&pkt)
}

// drainPTT pulls everything currently buffered on the PTT channel without
// blocking. Tests assert on the slice rather than racing with the channel.
func drainPTT(ch chan bool) []bool {
	var out []bool
	for {
		select {
		case v := <-ch:
			out = append(out, v)
		default:
			return out
		}
	}
}

// TestCheckPTT_HoldSingleButton — sanity: pre-existing single-button hold
// behaviour is preserved by the per-bit-edge rewrite.
func TestCheckPTT_HoldSingleButton(t *testing.T) {
	ch := make(chan bool, 4)
	var prev uint32
	var active bool
	const mask = uint32(0x00001000) // R2

	// 0 → mask: rising edge → mic on.
	checkPTT(makeBUTN(mask), mask, "hold", &prev, &active, ch)
	// mask → 0: falling edge → mic off.
	checkPTT(makeBUTN(0), mask, "hold", &prev, &active, ch)

	got := drainPTT(ch)
	if len(got) != 2 || got[0] != true || got[1] != false {
		t.Fatalf("expected [true,false], got %v", got)
	}
	if active {
		t.Errorf("active should be false after release")
	}
}

// TestCheckPTT_ToggleSingleButton — rising-edge-only toggle still works
// for the single-bit case after the rewrite.
func TestCheckPTT_ToggleSingleButton(t *testing.T) {
	ch := make(chan bool, 8)
	var prev uint32
	var active bool
	const mask = uint32(0x00001000)

	for i := 0; i < 3; i++ {
		// Press
		checkPTT(makeBUTN(mask), mask, "toggle", &prev, &active, ch)
		// Release (must NOT fire again)
		checkPTT(makeBUTN(0), mask, "toggle", &prev, &active, ch)
	}

	got := drainPTT(ch)
	want := []bool{true, false, true} // 3 toggles
	if len(got) != len(want) {
		t.Fatalf("expected %d toggles, got %d (%v)", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("toggle[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestCheckPTT_ToggleMultiBit_FiresOnSecondButton — the bug this fixes:
// pressing R2 while L2 is already held used to be silent. With per-bit
// rising edge it correctly fires a second toggle.
func TestCheckPTT_ToggleMultiBit_FiresOnSecondButton(t *testing.T) {
	ch := make(chan bool, 8)
	var prev uint32
	var active bool
	const (
		l2   = uint32(0x00000800)
		r2   = uint32(0x00001000)
		mask = l2 | r2
	)

	// Press L2 → toggle ON.
	checkPTT(makeBUTN(l2), mask, "toggle", &prev, &active, ch)
	// Now press R2 while L2 is still held (status = l2 | r2).
	// Old behaviour: no toggle (status & mask was already non-zero).
	// New behaviour: per-bit rising edge → another toggle.
	checkPTT(makeBUTN(l2|r2), mask, "toggle", &prev, &active, ch)
	// Release everything (should NOT toggle on release).
	checkPTT(makeBUTN(0), mask, "toggle", &prev, &active, ch)

	got := drainPTT(ch)
	if len(got) != 2 {
		t.Fatalf("expected 2 toggles (L2 press, R2 press), got %d (%v)", len(got), got)
	}
	if got[0] != true || got[1] != false {
		t.Errorf("toggle sequence wrong: got %v, want [true,false]", got)
	}
}

// TestCheckPTT_ToggleIgnoresUnmaskedBits — pressing a button outside
// the configured mask must not flip the mic state.
func TestCheckPTT_ToggleIgnoresUnmaskedBits(t *testing.T) {
	ch := make(chan bool, 4)
	var prev uint32
	var active bool
	const mask = uint32(0x00001000) // only R2 is the PTT button

	// Press L2 (0x00000800) — outside the mask. Should NOT toggle.
	checkPTT(makeBUTN(0x00000800), mask, "toggle", &prev, &active, ch)
	// Now press R2 — should toggle.
	checkPTT(makeBUTN(0x00000800|0x00001000), mask, "toggle", &prev, &active, ch)

	got := drainPTT(ch)
	if len(got) != 1 || got[0] != true {
		t.Errorf("expected exactly one toggle ON from R2, got %v", got)
	}
}

// TestCheckPTT_HoldMultiBit_StaysOnUntilAllReleased — in hold mode,
// holding ANY masked button keeps the mic on; mic only goes off when
// every masked button is released.
func TestCheckPTT_HoldMultiBit_StaysOnUntilAllReleased(t *testing.T) {
	ch := make(chan bool, 8)
	var prev uint32
	var active bool
	const (
		l2   = uint32(0x00000800)
		r2   = uint32(0x00001000)
		mask = l2 | r2
	)

	// Press L2 → mic on.
	checkPTT(makeBUTN(l2), mask, "hold", &prev, &active, ch)
	// Add R2 (both held) → still on, no new event.
	checkPTT(makeBUTN(l2|r2), mask, "hold", &prev, &active, ch)
	// Release L2 (R2 still held) → still on.
	checkPTT(makeBUTN(r2), mask, "hold", &prev, &active, ch)
	// Release R2 → mic off.
	checkPTT(makeBUTN(0), mask, "hold", &prev, &active, ch)

	got := drainPTT(ch)
	if len(got) != 2 || got[0] != true || got[1] != false {
		t.Fatalf("expected [true,false], got %v", got)
	}
}
