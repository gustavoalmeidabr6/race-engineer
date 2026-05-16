package transcript

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// RenderMarkdown converts a chronologically-ordered slice of events to a
// compact markdown view suitable for prompt injection. One bullet per
// event, prefixed with HH:MM:SS · KIND · ACTOR · text. Meta fields with
// scalar values are appended in parens; complex meta is dropped (the
// LLM has the structured form via the JSON tool surface if it needs it).
func RenderMarkdown(events []Event) string {
	if len(events) == 0 {
		return "_(no transcript yet)_"
	}
	var b strings.Builder
	for _, e := range events {
		b.WriteString("- ")
		b.WriteString(formatTime(e.At))
		b.WriteString(" · ")
		b.WriteString(string(e.Kind))
		b.WriteString(" · ")
		b.WriteString(e.Actor)
		if e.Text != "" {
			b.WriteString(" · ")
			b.WriteString(escapeMarkdown(e.Text))
		}
		if extras := scalarMeta(e.Meta); extras != "" {
			b.WriteString(" (")
			b.WriteString(extras)
			b.WriteString(")")
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "--:--:--"
	}
	return t.Format("15:04:05")
}

// escapeMarkdown trims newlines so a single transcript event stays on
// one bullet. Other markdown chars are left alone — readability matters
// more than strict escaping for an LLM consumer.
func escapeMarkdown(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 240 {
		s = s[:240] + "…"
	}
	return s
}

// scalarMeta serialises only string/number/bool meta fields, sorted for
// determinism. Maps and slices are skipped — they belong in a
// structured tool response, not in a one-line markdown bullet.
func scalarMeta(m map[string]any) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var parts []string
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			if v == "" {
				continue
			}
			parts = append(parts, fmt.Sprintf("%s=%s", k, v))
		case bool:
			parts = append(parts, fmt.Sprintf("%s=%t", k, v))
		case int, int32, int64, uint, uint32, uint64:
			parts = append(parts, fmt.Sprintf("%s=%d", k, v))
		case float32, float64:
			parts = append(parts, fmt.Sprintf("%s=%v", k, v))
		}
	}
	return strings.Join(parts, " ")
}
