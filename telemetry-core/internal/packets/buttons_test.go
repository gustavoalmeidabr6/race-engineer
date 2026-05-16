package packets

import (
	"strings"
	"testing"
)

func TestButtonName_KnownBits(t *testing.T) {
	cases := map[uint32]string{
		0x00000000: "",
		0x00000001: "Cross / A",
		0x00001000: "R2 / RT",
		0x00020000: "UDP Action 1",
		0x00040000: "UDP Action 2",
		0x01000000: "UDP Action 8",
	}
	for mask, want := range cases {
		if got := ButtonName(mask); got != want {
			t.Errorf("ButtonName(0x%08X) = %q, want %q", mask, got, want)
		}
	}
}

func TestButtonName_MultiBit(t *testing.T) {
	got := ButtonName(0x00000001 | 0x00000002)
	if !strings.Contains(got, "multiple") {
		t.Errorf("expected 'multiple' label for two bits set, got %q", got)
	}
}

func TestButtonName_UnknownBit(t *testing.T) {
	got := ButtonName(0x80000000)
	if !strings.HasPrefix(got, "(unknown bit") {
		t.Errorf("expected '(unknown bit ...)' for bit 31, got %q", got)
	}
}

func TestButtonNamesForStatus_OrdersByBit(t *testing.T) {
	// R2/RT (0x00001000) + UDP Action 1 (0x00020000): R2 should come first
	// because its bit (12) is lower than UDP Action 1's (17).
	names := ButtonNamesForStatus(0x00021000)
	if len(names) != 2 {
		t.Fatalf("expected 2 names, got %d: %v", len(names), names)
	}
	if names[0] != "R2 / RT" {
		t.Errorf("first should be R2 / RT, got %q", names[0])
	}
	if names[1] != "UDP Action 1" {
		t.Errorf("second should be UDP Action 1, got %q", names[1])
	}
}

func TestButtonNamesForStatus_EmptyStatus(t *testing.T) {
	if got := ButtonNamesForStatus(0); got != nil {
		t.Errorf("expected nil for status=0, got %v", got)
	}
}
