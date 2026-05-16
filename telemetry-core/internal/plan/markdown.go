package plan

import (
	"fmt"
	"strings"
)

// RenderMarkdown formats the plan for injection into the brain snapshot
// or any LLM prompt. Returns "" when the plan is nil or has no active
// entries — the caller can skip the section header in that case.
//
// Cancelled and done entries are excluded. Output is ordered by created_at
// ascending (oldest first), matching how the entries are stored.
func RenderMarkdown(p *Plan) string {
	if p == nil {
		return ""
	}
	active := p.FilterActive()
	if len(active) == 0 {
		return ""
	}
	var b strings.Builder
	for _, e := range active {
		b.WriteString("- ")
		b.WriteString(renderTrigger(e.Trigger))
		b.WriteString(" | ")
		b.WriteString(string(e.Kind))
		b.WriteString(" | ")
		b.WriteString(string(e.Status))
		if hl := entryHeadline(e); hl != "" && hl != string(e.Kind) {
			b.WriteString(" — ")
			b.WriteString(hl)
		}
		if e.Notes != "" {
			b.WriteString(" (")
			b.WriteString(e.Notes)
			b.WriteString(")")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// renderTrigger turns a Trigger into a compact tag like "L14", "L12-16",
// "L14@3500m", or "T-300s". Used as the leading column in RenderMarkdown
// so an LLM can scan plan entries at a glance.
func renderTrigger(t Trigger) string {
	switch t.Type {
	case TriggerLap:
		return fmt.Sprintf("L%d", t.Lap)
	case TriggerLapWindow:
		return fmt.Sprintf("L%d-%d", t.FromLap, t.ToLap)
	case TriggerTrackPosition:
		if t.DistanceM > 0 {
			return fmt.Sprintf("L%d@%.0fm", t.Lap, t.DistanceM)
		}
		return fmt.Sprintf("L%d@pit-entry", t.Lap)
	case TriggerTimeLeftSec:
		return fmt.Sprintf("T-%ds", t.Seconds)
	default:
		return string(t.Type)
	}
}
