package packets

import "fmt"

// ButtonName returns the F1 25 wheel/pad label for a single-bit BUTN
// bitmask. Multi-bit masks (simultaneous press of multiple buttons)
// return "(multiple bits set)" so callers can spot composite masks
// in their logs without misattributing.
//
// Names follow F1 25's UDP spec for the ButtonStatus bitfield in the
// "Buttons" event (packet ID 3, sub-event "BUTN"). UDP Action 1–8 are
// the no-side-effect helper bindings the user maps in the controls
// menu — those are the recommended targets for our PTT_BUTTON or
// PTT_BUTTON_START / PTT_BUTTON_END configuration.
func ButtonName(mask uint32) string {
	if mask == 0 {
		return ""
	}
	if mask&(mask-1) != 0 {
		return "(multiple bits set)"
	}
	switch mask {
	case 0x00000001:
		return "Cross / A"
	case 0x00000002:
		return "Triangle / Y"
	case 0x00000004:
		return "Circle / B"
	case 0x00000008:
		return "Square / X"
	case 0x00000010:
		return "D-pad Left"
	case 0x00000020:
		return "D-pad Right"
	case 0x00000040:
		return "D-pad Up"
	case 0x00000080:
		return "D-pad Down"
	case 0x00000100:
		return "Options / Menu"
	case 0x00000200:
		return "L1 / LB"
	case 0x00000400:
		return "R1 / RB"
	case 0x00000800:
		return "L2 / LT"
	case 0x00001000:
		return "R2 / RT"
	case 0x00002000:
		return "Left Stick Click"
	case 0x00004000:
		return "Right Stick Click"
	case 0x00008000:
		return "PS / Xbox"
	case 0x00010000:
		return "Touchpad"
	case 0x00020000:
		return "UDP Action 1"
	case 0x00040000:
		return "UDP Action 2"
	case 0x00080000:
		return "UDP Action 3"
	case 0x00100000:
		return "UDP Action 4"
	case 0x00200000:
		return "UDP Action 5"
	case 0x00400000:
		return "UDP Action 6"
	case 0x00800000:
		return "UDP Action 7"
	case 0x01000000:
		return "UDP Action 8"
	default:
		return fmt.Sprintf("(unknown bit 0x%08X)", mask)
	}
}

// ButtonNamesForStatus returns one ButtonName for every set bit in
// status, in ascending bit order. Useful when several buttons are held
// simultaneously and the caller wants the full list for a log line.
func ButtonNamesForStatus(status uint32) []string {
	if status == 0 {
		return nil
	}
	var out []string
	for i := uint32(0); i < 32; i++ {
		bit := uint32(1) << i
		if status&bit != 0 {
			out = append(out, ButtonName(bit))
		}
	}
	return out
}
