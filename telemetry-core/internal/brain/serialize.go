package brain

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Markdown renders the full snapshot — dynamic layers first, static second.
// Hot facts (L1) are intentionally omitted: callers compose them via
// intelligence.BuildContext to avoid an import cycle on the intelligence pkg.
//
// Section ordering is stable so the prompt prefix stays cache-friendly.
func (s Snapshot) Markdown() string {
	dyn := s.MarkdownDynamic()
	stc := s.MarkdownStatic()
	if dyn == "" {
		return stc
	}
	if stc == "" {
		return dyn
	}
	return dyn + stc
}

// MarkdownDynamic renders only the volatile layers: observations, hypotheses,
// race events, recent radio, driver state. Useful when the caller wants to
// place static context elsewhere in the prompt (e.g., as a stable prefix).
func (s Snapshot) MarkdownDynamic() string {
	var b strings.Builder

	if hasObservations(s) {
		b.WriteString("## Active Observations\n")
		// Build the iteration order: registered topics first (stable), then any
		// analyst.* topics present in the snapshot, sorted alphabetically.
		seen := make(map[Topic]struct{}, len(AllTopics))
		ordered := make([]Topic, 0, len(AllTopics)+len(s.Observations))
		for _, t := range AllTopics {
			ordered = append(ordered, t)
			seen[t] = struct{}{}
		}
		var extra []Topic
		for t := range s.Observations {
			if _, ok := seen[t]; ok {
				continue
			}
			extra = append(extra, t)
			seen[t] = struct{}{}
		}
		for t := range s.Hypotheses {
			if _, ok := seen[t]; ok {
				continue
			}
			extra = append(extra, t)
			seen[t] = struct{}{}
		}
		sort.Slice(extra, func(i, j int) bool { return extra[i] < extra[j] })
		ordered = append(ordered, extra...)

		for _, t := range ordered {
			items := s.Observations[t]
			for _, o := range items {
				prefix := ""
				if o.Urgent {
					prefix = "⚠ URGENT — "
				}
				fmt.Fprintf(&b, "- [%s] %s%s · %s (conf %.2f, %s ago)\n",
					t, prefix, o.Summary, o.Agent, o.Confidence, age(s.Now, o.At))
			}
			if h, ok := s.Hypotheses[t]; ok {
				prefix := ""
				if h.Urgent {
					prefix = "⚠ URGENT — "
				}
				fmt.Fprintf(&b, "- [%s] HYPOTHESIS: %s%s · %s (conf %.2f, %s ago)\n",
					t, prefix, h.Summary, h.Agent, h.Confidence, age(s.Now, h.At))
			}
		}
		b.WriteString("\n")
	}

	if len(s.PendingJobs) > 0 {
		b.WriteString("## Pending Analyst Jobs\n")
		for _, p := range s.PendingJobs {
			topic := p.ContextTopic
			if topic == "" {
				topic = "general"
			}
			fmt.Fprintf(&b, "- %s [%s] \"%s\" (running %s)\n",
				p.JobID, topic, p.Question, age(s.Now, p.StartedAt))
		}
		b.WriteString("\n")
	}

	if len(s.Events) > 0 {
		b.WriteString("## Recent Race Events\n")
		for _, e := range s.Events {
			detail := ""
			if e.Detail != "" {
				detail = " — " + e.Detail
			}
			fmt.Fprintf(&b, "- %s ago: %s%s\n", age(s.Now, e.At), e.Code, detail)
		}
		b.WriteString("\n")
	}

	if len(s.Speech) > 0 {
		b.WriteString("## Recent Radio (already said)\n")
		for _, sp := range s.Speech {
			fmt.Fprintf(&b, "- %s ago (P%d): %s\n", age(s.Now, sp.At), sp.Priority, sp.Text)
		}
		b.WriteString("\n")
	}

	if len(s.DriverEvents) > 0 {
		b.WriteString("## Driver State\n")
		for _, d := range s.DriverEvents {
			fmt.Fprintf(&b, "- %s ago [%s]: %s\n", age(s.Now, d.At), d.Kind, d.Text)
		}
		b.WriteString("\n")
	}

	return b.String()
}

// MarkdownStatic renders the long-lived workspace context: driver profile,
// track setup, past learnings, and the engineer/driver preference blocks.
// Returns "" when no static fields are populated.
func (s Snapshot) MarkdownStatic() string {
	var b strings.Builder
	if s.Static.User != "" {
		b.WriteString("## Driver Preferences\n")
		b.WriteString(strings.TrimSpace(s.Static.User))
		b.WriteString("\n\n")
	}
	if s.Static.DriverProfile != "" {
		b.WriteString("## Driver Profile\n")
		b.WriteString(strings.TrimSpace(s.Static.DriverProfile))
		b.WriteString("\n\n")
	}
	if s.Static.TrackSetup != "" {
		b.WriteString("## Track Setup\n")
		b.WriteString(strings.TrimSpace(s.Static.TrackSetup))
		b.WriteString("\n\n")
	}
	if s.Static.PastLearnings != "" {
		b.WriteString("## Past Learnings\n")
		b.WriteString(strings.TrimSpace(s.Static.PastLearnings))
		b.WriteString("\n\n")
	}
	return b.String()
}

func hasObservations(s Snapshot) bool {
	for _, items := range s.Observations {
		if len(items) > 0 {
			return true
		}
	}
	return len(s.Hypotheses) > 0
}

func age(now, at time.Time) string {
	d := now.Sub(at)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Second:
		return "now"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
