package brain

import (
	"sort"
	"strings"
	"time"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
)

// AnalystTopicPrefix is the prefix used for observations written by the
// async analyst pipeline. Topics under this prefix are dynamic (named after
// the caller-supplied context_topic) and are surfaced in snapshots even
// though they aren't in the closed AllTopics registry.
const AnalystTopicPrefix = "analyst."

// SnapshotOpts controls what a snapshot includes. Most callers should use
// DefaultSnapshotOpts and tweak from there.
type SnapshotOpts struct {
	// MaxObservationAge drops observations and hypotheses older than this.
	// Zero keeps everything in the ring buffers.
	MaxObservationAge time.Duration

	// Topics restricts L2/L3 to a subset. Nil = include all known topics.
	Topics []Topic

	IncludeHotFacts     bool
	IncludeStatic       bool
	IncludeRecentSpeech bool
	IncludeDriver       bool
	IncludeEvents       bool

	// EventWindow filters L7 events to the last N. Zero = all retained.
	EventWindow time.Duration

	// MaxSpeech caps the number of recent radio entries returned. Zero = all retained.
	MaxSpeech int
}

// DefaultSnapshotOpts is the standard view used by CommsGate at decision time.
func DefaultSnapshotOpts() SnapshotOpts {
	return SnapshotOpts{
		MaxObservationAge:   2 * time.Minute,
		IncludeHotFacts:     true,
		IncludeStatic:       true,
		IncludeRecentSpeech: true,
		IncludeDriver:       true,
		IncludeEvents:       true,
		EventWindow:         5 * time.Minute,
		MaxSpeech:           6,
	}
}

// Snapshot is an immutable, point-in-time view returned by RaceBrain.Snapshot.
type Snapshot struct {
	Now          time.Time
	HotFacts     *models.RaceState
	Observations map[Topic][]Observation
	Hypotheses   map[Topic]Observation
	Speech       []SpeechRecord
	DriverEvents []DriverEvent
	Events       []RaceEvent
	Static       StaticContext
	PendingJobs  []PendingJob

	// PendingEvents are events with Status ∈ {Queued, InFlight}. The Live
	// agent and pi-agent read these so they know what's still owed to the
	// driver — beyond the "Recent Radio" / "Active Observations" blocks
	// already in the snapshot.
	PendingEvents []Event

	// AwaitingAck holds events that have been spoken but require a driver
	// copy. AwaitingAckSince + NagCount let the agent reason about
	// whether to re-engage the driver.
	AwaitingAck []Event

	// OpenDriverQuestions surfaces transcribed driver utterances the
	// engineer hasn't yet substantively addressed — see brain.OpenDriverQuestions.
	OpenDriverQuestions []DriverUtterance
}

// Snapshot returns a filtered, immutable view of the brain.
func (b *RaceBrain) Snapshot(opts SnapshotOpts) Snapshot {
	b.mu.RLock()
	defer b.mu.RUnlock()

	now := b.now()
	snap := Snapshot{Now: now}

	if opts.IncludeHotFacts && b.stateLoader != nil {
		snap.HotFacts = b.stateLoader()
	}

	topicSet := opts.Topics
	if topicSet == nil {
		// Default: the closed registry plus any dynamic analyst.* topics that
		// happen to have data. The latter is what the async analyst writes to.
		topicSet = make([]Topic, 0, len(AllTopics))
		topicSet = append(topicSet, AllTopics...)
		seen := make(map[Topic]struct{}, len(AllTopics))
		for _, t := range AllTopics {
			seen[t] = struct{}{}
		}
		for t := range b.observations {
			if _, ok := seen[t]; ok {
				continue
			}
			if strings.HasPrefix(string(t), AnalystTopicPrefix) {
				topicSet = append(topicSet, t)
				seen[t] = struct{}{}
			}
		}
		for t := range b.hypotheses {
			if _, ok := seen[t]; ok {
				continue
			}
			if strings.HasPrefix(string(t), AnalystTopicPrefix) {
				topicSet = append(topicSet, t)
				seen[t] = struct{}{}
			}
		}
	}

	snap.Observations = make(map[Topic][]Observation, len(topicSet))
	snap.Hypotheses = make(map[Topic]Observation)

	for _, t := range topicSet {
		if items, ok := b.observations[t]; ok {
			kept := make([]Observation, 0, len(items))
			for _, o := range items {
				if opts.MaxObservationAge > 0 && now.Sub(o.At) > opts.MaxObservationAge {
					continue
				}
				kept = append(kept, o)
			}
			if len(kept) > 0 {
				snap.Observations[t] = kept
			}
		}
		if h, ok := b.hypotheses[t]; ok {
			if opts.MaxObservationAge > 0 && now.Sub(h.At) > opts.MaxObservationAge {
				continue
			}
			snap.Hypotheses[t] = h
		}
	}

	if opts.IncludeRecentSpeech && len(b.speech) > 0 {
		sp := b.speech
		if opts.MaxSpeech > 0 && len(sp) > opts.MaxSpeech {
			sp = sp[len(sp)-opts.MaxSpeech:]
		}
		snap.Speech = append([]SpeechRecord(nil), sp...)
	}

	if opts.IncludeDriver && len(b.driverEvents) > 0 {
		snap.DriverEvents = append([]DriverEvent(nil), b.driverEvents...)
	}

	if opts.IncludeEvents && len(b.events) > 0 {
		for _, e := range b.events {
			if opts.EventWindow > 0 && now.Sub(e.At) > opts.EventWindow {
				continue
			}
			snap.Events = append(snap.Events, e)
		}
		sort.SliceStable(snap.Events, func(i, j int) bool {
			return snap.Events[i].At.Before(snap.Events[j].At)
		})
	}

	if opts.IncludeStatic {
		snap.Static = b.static
	}

	if len(b.pendingJobs) > 0 {
		snap.PendingJobs = append([]PendingJob(nil), b.pendingJobs...)
	}

	// Surface the Interrupts Bus state inline so the agent's prompt
	// reflects exactly what's still owed to the driver. Split into two
	// buckets so the markdown renderer can describe their meanings
	// distinctly.
	if len(b.eventQueue) > 0 {
		for _, e := range b.eventQueue {
			switch e.Status {
			case StatusAwaitingAck:
				snap.AwaitingAck = append(snap.AwaitingAck, e)
			case StatusQueued, StatusInFlight:
				snap.PendingEvents = append(snap.PendingEvents, e)
			}
		}
	}

	if len(b.driverUtterances) > 0 {
		// Inline copy of OpenDriverQuestions logic so we can reuse the
		// already-held read lock instead of double-acquiring.
		const window = 90 * time.Second
		for _, u := range b.driverUtterances {
			if u.Addressed {
				continue
			}
			if now.Sub(u.At) > window {
				continue
			}
			snap.OpenDriverQuestions = append(snap.OpenDriverQuestions, u)
		}
	}

	return snap
}
