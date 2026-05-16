package ingestion

import (
	"testing"
)

// Dual-PTT bit masks used across these tests. L2 / R2 because that's the
// real-world configuration we verified live via LOG_BUTTONS — see .env
// commentary. R2 also fans out UDP Action 4 (bit 20) because that binding
// exists in the controls menu, producing a composite 0x00101000 status.
const (
	bitL2          uint32 = 0x00000800
	bitR2          uint32 = 0x00001000
	bitUDPAction4  uint32 = 0x00100000
	r2Composite    uint32 = bitR2 | bitUDPAction4 // observed live
)

// TestCheckDualPTT_L2OpensR2Closes is the canonical user scenario:
// L2 turns the mic on, R2 turns it off.
func TestCheckDualPTT_L2OpensR2Closes(t *testing.T) {
	ch := make(chan bool, 8)
	var prev uint32
	var active bool

	// L2 press → mic ON.
	checkDualPTT(makeBUTN(bitL2), bitL2, bitR2, &prev, &active, ch)
	if !active {
		t.Fatal("expected mic ON after L2 press")
	}
	// L2 release — must be silent (press-edge only).
	checkDualPTT(makeBUTN(0), bitL2, bitR2, &prev, &active, ch)
	if !active {
		t.Fatal("L2 release should not turn mic off")
	}
	// R2 press → mic OFF. (Real R2 fires the composite 0x00101000.)
	checkDualPTT(makeBUTN(r2Composite), bitL2, bitR2, &prev, &active, ch)
	if active {
		t.Fatal("expected mic OFF after R2 press")
	}
	// R2 release — silent.
	checkDualPTT(makeBUTN(0), bitL2, bitR2, &prev, &active, ch)
	if active {
		t.Fatal("R2 release should not turn mic back on")
	}

	got := drainPTT(ch)
	if len(got) != 2 || got[0] != true || got[1] != false {
		t.Errorf("expected [true,false], got %v", got)
	}
}

// TestCheckDualPTT_DoubleStartIsIdempotent: pressing L2 again while the
// mic is already open is a no-op (no spurious channel message).
func TestCheckDualPTT_DoubleStartIsIdempotent(t *testing.T) {
	ch := make(chan bool, 8)
	var prev uint32
	var active bool

	checkDualPTT(makeBUTN(bitL2), bitL2, bitR2, &prev, &active, ch) // ON
	checkDualPTT(makeBUTN(0), bitL2, bitR2, &prev, &active, ch)     // release
	checkDualPTT(makeBUTN(bitL2), bitL2, bitR2, &prev, &active, ch) // press again — no-op

	got := drainPTT(ch)
	if len(got) != 1 || got[0] != true {
		t.Errorf("expected single [true] (no double-fire), got %v", got)
	}
}

// TestCheckDualPTT_DoubleEndIsIdempotent: pressing R2 while the mic is
// already closed is a no-op.
func TestCheckDualPTT_DoubleEndIsIdempotent(t *testing.T) {
	ch := make(chan bool, 8)
	var prev uint32
	var active bool

	// Fresh state (mic off). Pressing R2 should NOT emit a redundant "off".
	checkDualPTT(makeBUTN(r2Composite), bitL2, bitR2, &prev, &active, ch)

	got := drainPTT(ch)
	if len(got) != 0 {
		t.Errorf("R2 with mic already off should be silent, got %v", got)
	}
}

// TestCheckDualPTT_SimultaneousPress: an unlikely but possible packet
// where both L2 and R2 are reported pressed in the same frame. Start
// runs first (mic ON), then end fires within the same call (mic OFF).
// Documents the deterministic order; the user shouldn't hit this in
// normal play because L2 and R2 are physically separate paddles.
func TestCheckDualPTT_SimultaneousPress(t *testing.T) {
	ch := make(chan bool, 8)
	var prev uint32
	var active bool

	checkDualPTT(makeBUTN(bitL2|r2Composite), bitL2, bitR2, &prev, &active, ch)

	got := drainPTT(ch)
	if len(got) != 2 || got[0] != true || got[1] != false {
		t.Errorf("expected [true,false] from simultaneous press, got %v", got)
	}
	if active {
		t.Errorf("final state should be off (end wins), got on")
	}
}

// TestCheckDualPTT_R2CompositeMatches: the live-observed composite value
// (R2 + UDP Action 4 fanout) must be detected as an end-press. This is
// the case the user reported.
func TestCheckDualPTT_R2CompositeMatches(t *testing.T) {
	ch := make(chan bool, 4)
	var prev uint32
	var active = true // start with mic open as if L2 had been pressed earlier

	checkDualPTT(makeBUTN(r2Composite), bitL2, bitR2, &prev, &active, ch)

	got := drainPTT(ch)
	if len(got) != 1 || got[0] != false {
		t.Errorf("expected [false] from R2 composite press, got %v", got)
	}
	if active {
		t.Error("active should be false after R2 composite press")
	}
}
