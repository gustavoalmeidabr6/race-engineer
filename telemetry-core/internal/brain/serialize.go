package brain

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/enums"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
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

	// Always-on race-progress panel. Renders first so the engineer never
	// has to scroll for "what phase of the race is this?" — the answer is
	// at the top of every snapshot during race sessions.
	if s.HotFacts != nil {
		writeRaceProgressSection(&b, s.HotFacts)
	}

	// Always-on driver-settings panel. This block is rendered even when no
	// observation has been written about settings so the Live agent and
	// CommsGate see "what's currently dialed in" alongside live telemetry.
	// Cockpit-changeable vs setup-screen-only is called out explicitly so
	// the engineer doesn't suggest a wing change mid-stint.
	if s.HotFacts != nil {
		writeDriverSettingsSection(&b, s.HotFacts)
	}

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

	if len(s.PendingEvents) > 0 {
		b.WriteString("## Pending Events (not yet spoken)\n")
		for _, e := range s.PendingEvents {
			fmt.Fprintf(&b, "- [P%d %s] %s (%s, attempt %d)\n",
				e.Priority, e.Type, e.Summary, e.Status, e.Attempts)
		}
		b.WriteString("\n")
	}

	if len(s.AwaitingAck) > 0 {
		b.WriteString("## Awaiting Driver Copy\n")
		b.WriteString("Driver has NOT acknowledged. If their next utterance contains 'copy', 'roger', 'got it', 'yes', or similar — call `confirm_event_copy` with the matching event_id.\n")
		for _, e := range s.AwaitingAck {
			fmt.Fprintf(&b, "- id=%s [P%d %s] %s (spoken %s ago, nag %d/%d)\n",
				e.ID, e.Priority, e.Type, e.Summary, age(s.Now, e.AwaitingAckSince), e.NagCount, MaxAckNags)
		}
		b.WriteString("\n")
	}

	if len(s.OpenDriverQuestions) > 0 {
		b.WriteString("## Open Driver Questions\n")
		b.WriteString("Address these before changing topic. When you have answered, call `mark_question_addressed` with the utterance timestamp + a short summary.\n")
		for _, u := range s.OpenDriverQuestions {
			fmt.Fprintf(&b, "- %s ago: %q\n", age(s.Now, u.At), u.Text)
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

// writeRaceProgressSection renders the always-on race-phase + lap-progress
// panel. Pure read off RaceState — derives phase via state.Phase() so the
// vocabulary stays consistent across snapshot, BuildContext, and MCP tools.
//
// Outside of race sessions the panel is skipped (P/Q phases have their own
// signal flow and don't need lifecycle drama).
func writeRaceProgressSection(b *strings.Builder, s *models.RaceState) {
	if !models.IsRaceSession(s.SessionType) {
		return
	}
	phase := s.Phase()
	rem := s.LapsRemaining()

	b.WriteString("## Race Progress\n")
	switch phase {
	case models.PhaseGrid:
		fmt.Fprintf(b, "- phase: GRID · starting P%d · %d laps · weather %s · track %d°C\n",
			s.GridPosition, s.TotalLaps, enums.Weather(s.Weather), s.TrackTemp)
		b.WriteString("- pre-race: ONE short tone-setting line, no questions. Get the driver ready.\n")
	case models.PhaseFormation:
		fmt.Fprintf(b, "- phase: FORMATION LAP · starting P%d · %d laps total\n",
			s.GridPosition, s.TotalLaps)
		b.WriteString("- formation lap: brief cue — weave to heat tyres, brakes on. No long talk.\n")
	case models.PhaseLightsOut:
		fmt.Fprintf(b, "- phase: LIGHTS OUT · started P%d · lap 1/%d\n",
			s.GridPosition, s.TotalLaps)
		b.WriteString("- LIGHTS OUT: voice the launch in ONE urgent line, then go silent for ~30s unless safety-critical.\n")
	case models.PhaseFinalLap:
		fmt.Fprintf(b, "- phase: FINAL LAP · lap %d/%d · P%d · gap ahead %dms\n",
			s.CurrentLap, s.TotalLaps, s.Position, s.DeltaToFrontMs)
		b.WriteString("- LAST LAP — push or protect, no save. Suppress coaching cues; only safety + result-impacting calls.\n")
	case models.PhaseFinished:
		finalPos := s.FinalClassification
		if finalPos == 0 {
			finalPos = s.Position
		}
		gridDelta := int(s.GridPosition) - int(finalPos)
		fmt.Fprintf(b, "- phase: FINISHED · final P%d (started P%d, %+d) · %d laps · %d pit stops\n",
			finalPos, s.GridPosition, gridDelta, s.TotalLaps, s.NumPitStops)
		switch {
		case finalPos == 1:
			b.WriteString("- RACE OVER — P1, RACE WIN. Lead with a real celebration on the radio. Override 'not sycophantic' — this is the moment.\n")
		case finalPos <= 3:
			b.WriteString("- RACE OVER — PODIUM. Acknowledge it properly before any debrief.\n")
		default:
			b.WriteString("- RACE OVER — lead with the honest result, one constructive takeaway, no more coaching.\n")
		}
	case models.PhaseRacing:
		fmt.Fprintf(b, "- phase: RACING · lap %d/%d (%d to go) · P%d · stint lap %d on %s · fuel %.1f laps\n",
			s.CurrentLap, s.TotalLaps, rem, s.Position, s.TyresAgeLaps,
			enums.Compound(s.ActualCompound), s.FuelRemainingLaps)
		if rem > 0 && rem <= 5 {
			b.WriteString("- LATE RACE — switch from save to push framing; damage-aware calls favour 'manage to finish'.\n")
		}
	default:
		// PhaseUnknown / PhaseNonRace fall through to nothing extra.
	}
	b.WriteString("\n")
}

// writeDriverSettingsSection renders the always-on cockpit + setup snapshot.
// Format is two short lines so the section costs ~80 tokens regardless of
// observation activity, leaving room for the Live agent's other context.
func writeDriverSettingsSection(b *strings.Builder, s *models.RaceState) {
	ds := s.DriverSettings
	if ds.BrakeBias == 0 && ds.OnThrottleDiff == 0 && ds.EngineBraking == 0 && s.ERSDeployMode == 0 && s.FuelMix == 0 {
		// No CarSetup packet has landed yet (and no fallback ERS/fuel
		// mix from CarStatus either). Skip rather than render a row of
		// zeros that the model might misread as "everything is on min".
		return
	}
	b.WriteString("## Driver Settings (current)\n")
	fmt.Fprintf(b, "- cockpit: brake-bias %d%%, diff on/off %d/%d, engine-braking %d, ERS %s, fuel-mix %s\n",
		ds.BrakeBias, ds.OnThrottleDiff, ds.OffThrottleDiff, ds.EngineBraking,
		enums.ERSDeployMode(s.ERSDeployMode), enums.FuelMix(s.FuelMix))

	// DRS usage this lap — silently include only when the lap has accumulated
	// at least 1s of "allowed" window, so partial-lap noise (start of stint,
	// just after pit) doesn't render a misleading 0/0 ratio.
	if s.DRSAvailableThisLap > 1.0 {
		util := s.DRSUsedThisLap / s.DRSAvailableThisLap * 100.0
		fmt.Fprintf(b, "- DRS this lap: %.1fs used / %.1fs available (%.0f%% utilization)\n",
			s.DRSUsedThisLap, s.DRSAvailableThisLap, util)
	}

	// Wing damage — call out only when worth mentioning, and tag as
	// "next-pit only" so the LLM doesn't suggest changing it mid-stint.
	frontDmg := s.FrontLeftWingDmg
	if s.FrontRightWingDmg > frontDmg {
		frontDmg = s.FrontRightWingDmg
	}
	if frontDmg >= 15 || s.RearWingDmg >= 15 {
		fmt.Fprintf(b, "- wing damage (next-pit only): front %d%%, rear %d%%\n", frontDmg, s.RearWingDmg)
	}

	// Recent changes — last 2 chronological entries from the in-state ring.
	// The full ring of 8 stays available via /api/state/driver_settings.
	if ds.RecentChangesLen > 0 {
		n := int(ds.RecentChangesLen)
		start := (int(ds.RecentChangesHead) - n + 8) % 8
		// Show up to the two most recent.
		shown := 0
		for i := n - 1; i >= 0 && shown < 2; i-- {
			ch := ds.RecentChanges[(start+i)%8]
			fmt.Fprintf(b, "- recent change: %s %d→%d (lap %d)\n", ch.Channel, ch.From, ch.To, ch.Lap)
			shown++
		}
	}
	b.WriteString("\n")
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
