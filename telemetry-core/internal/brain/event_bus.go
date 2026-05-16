package brain

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
)

// Bus capacity. Bounded so a stuck Live agent dispatcher can't OOM the brain.
// 32 is enough headroom for: 1 lap-summary + 4 game-state events + 27 of
// whatever else comes in within a single dispatch tick.
const maxEventQueue = 32

// Dedup window: a producer pushing an event with a DedupKey that matches a
// queued or recently-spoken event within this window is rejected.
const eventDedupWindow = 30 * time.Second

// Subject dedup window: shorter than DedupKey because subject dedup spans
// rule families (closing_threat + traffic_update for the same car), and
// we want the cross-rule cooldown to expire while the per-rule DedupKey is
// still active so the rule gets to refire on its own latch logic.
const subjectDedupWindow = 20 * time.Second

// Threat coalesce window: when a threat-class event arrives within this
// window of an already-queued threat for a *different* subject, merge the
// two into a single bundled call rather than firing two back-to-back radio
// calls about different cars. Only applies before the older event has
// been leased — once it's in_flight we let the second one queue normally.
const threatCoalesceWindow = 2 * time.Second

// High-priority coalesce floor + window. A second pass beyond threat
// coalescing: ANY event at priority >= floor that lands within the window
// of an already-queued same-or-higher-priority peer is merged into that
// peer rather than dispatched separately. Catches correlated alerts that
// don't share a type (e.g. tire_cliff + box_now firing within ~tens of ms
// of each other) so the driver hears one bundled radio call.
const (
	highPriorityCoalesceFloor  = 4
	highPriorityCoalesceWindow = 2 * time.Second
)

// isThreatClass returns true for event types that announce a competitor
// threat the driver might want to hear bundled when they fire close
// together. Currently: closing_threat + traffic_update. Lap_summary,
// pit_window etc. are kept separate because their messages don't
// naturally bundle.
func isThreatClass(t EventType) bool {
	switch t {
	case EventThreatOvertake, EventTrafficUpdate:
		return true
	default:
		return false
	}
}

// LeaseTimeout is the default unconfirmed-lease budget. The actual budget
// applied at reap time is LeaseTimeoutFor(event) — see that function for
// the priority-aware schedule. This constant is preserved as the floor /
// fallback so existing tests and callers that don't carry an event keep
// working.
const LeaseTimeout = 8 * time.Second

// LeaseTimeoutFor returns the in-flight budget for one event. Tool-using
// types (closing_threat, box_now) need extra slack because the model often
// chains 1-2 fast tool calls before speaking. Everything else uses the
// default LeaseTimeout floor.
func LeaseTimeoutFor(e Event) time.Duration {
	switch e.Type {
	case EventThreatOvertake, EventBoxNow, EventTireCliff, EventPitWindowOpen:
		return 15 * time.Second
	default:
		return LeaseTimeout
	}
}

// Sentinel errors so HTTP handlers can map to status codes.
var (
	ErrEventDeduped     = errors.New("event: deduped (recent identical key)")
	ErrEventTalkLevel   = errors.New("event: dropped by TalkLevel filter")
	ErrEventDebugTooBig = errors.New("event: debug_data exceeds 1KB serialised")
	ErrEventNotFound    = errors.New("event: id not in queue")
	ErrEventInFlight    = errors.New("event: another event is currently in_flight")
)

// EnqueueEvent validates the event, applies TalkLevel filtering, dedups
// against the queue and recent speech, then inserts into the priority queue.
//
// On success, returns the stored Event (with assigned ID + At). On rejection,
// returns nil + a sentinel error so callers can distinguish "client error"
// (validation) from "filtered" (TalkLevel / dedup).
//
// talkLevel is read from cfg.TalkLevel by the caller; the brain doesn't
// know about config. Pass any positive int to disable the filter (eg 10).
func (b *RaceBrain) EnqueueEvent(e Event, talkLevel int32) (*Event, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}

	// debug_data 1KB cap (post-serialise to catch nested bloat).
	if len(e.DebugData) > 0 {
		blob, err := json.Marshal(e.DebugData)
		if err != nil {
			return nil, err
		}
		if len(blob) > MaxEventDebugBytes {
			return nil, ErrEventDebugTooBig
		}
	}

	// TalkLevel floor — applied BEFORE locking so we don't even queue.
	if e.Priority < MinPriorityForTalkLevel(talkLevel) {
		return nil, ErrEventTalkLevel
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()

	// Dedup against recently-spoken radio (last 30s) AND already-queued events.
	if e.DedupKey != "" {
		for _, q := range b.eventQueue {
			if q.DedupKey == e.DedupKey {
				return nil, ErrEventDeduped
			}
		}
		for _, sp := range b.speech {
			if now.Sub(sp.At) > eventDedupWindow {
				continue
			}
			// Speech doesn't carry DedupKey, so we approximate by event-source
			// metadata in Source. Producers that want strong dedup against
			// what's already been said should also include it in DedupKey.
			if sp.Text != "" && sp.Text == e.DedupKey {
				return nil, ErrEventDeduped
			}
		}
	}

	// Subject dedup: if we just spoke about this subject (e.g. car:5) within
	// subjectDedupWindow, drop. Cuts cross-rule overlap — closing_threat for
	// Charles + traffic_update covering Charles within the same window collapse
	// to one announcement instead of three.
	if e.DedupSubject != "" {
		for _, q := range b.eventQueue {
			if q.DedupSubject == e.DedupSubject {
				return nil, ErrEventDeduped
			}
		}
		for _, sp := range b.speech {
			if sp.Subject == "" || sp.Subject != e.DedupSubject {
				continue
			}
			if now.Sub(sp.At) > subjectDedupWindow {
				continue
			}
			return nil, ErrEventDeduped
		}
	}

	// Threat coalescing: if a threat-class event for a different subject
	// is already queued (not yet in_flight) and was added inside the
	// coalesce window, append our facts to it and return the merged peer
	// instead of inserting a second event. Result: two cars closing in
	// near-simultaneously produce one combined radio call ("Leclerc 1.2s,
	// Verstappen 1.8s — both closing.") rather than two separate ones.
	if isThreatClass(e.Type) && e.DedupSubject != "" {
		for i := range b.eventQueue {
			peer := &b.eventQueue[i]
			if peer.Status != StatusQueued {
				continue
			}
			if !isThreatClass(peer.Type) {
				continue
			}
			if peer.DedupSubject == "" || peer.DedupSubject == e.DedupSubject {
				continue
			}
			if now.Sub(peer.At) > threatCoalesceWindow {
				continue
			}
			mergeIntoPeer(peer, e)
			out := *peer
			return &out, nil
		}
	}

	// High-priority coalescing: ANY P4+ event arriving within
	// highPriorityCoalesceWindow of a queued P4+ peer is merged into the
	// peer rather than dispatched separately. Skips the same-subject case
	// (handled by subject dedup above) and the both-threat-class case
	// (already handled by the threat-coalesce block).
	if e.Priority >= highPriorityCoalesceFloor {
		for i := range b.eventQueue {
			peer := &b.eventQueue[i]
			if peer.Status != StatusQueued {
				continue
			}
			if peer.Priority < highPriorityCoalesceFloor {
				continue
			}
			if peer.DedupSubject != "" && peer.DedupSubject == e.DedupSubject {
				continue
			}
			if isThreatClass(peer.Type) && isThreatClass(e.Type) {
				continue
			}
			if now.Sub(peer.At) > highPriorityCoalesceWindow {
				continue
			}
			mergeIntoPeer(peer, e)
			out := *peer
			return &out, nil
		}
	}

	// Assign ID + timestamp if not provided.
	if e.ID == "" {
		e.ID = newEventID()
	}
	if e.At.IsZero() {
		e.At = now
	}

	// Lifecycle defaults. Producers don't set these; the bus owns them.
	e.Status = StatusQueued
	e.InFlightAt = time.Time{}
	e.Attempts = 0
	e.LastNackAt = time.Time{}
	e.AwaitingAckSince = time.Time{}
	e.NagCount = 0
	if e.StaleAfter == 0 {
		e.StaleAfter = DefaultStaleAfter(e.Type)
	}
	// Producer didn't explicitly opt out; apply the per-type default.
	// Producers that DO want an explicit non-default can pass
	// RequiresAck=true on any type — the floor is OR, not equality.
	if !e.RequiresAck && DefaultRequiresAck(e.Type) {
		e.RequiresAck = true
	}

	// Sorted insert: higher priority first; within same priority, older first
	// (FIFO at each priority level). This lets DequeueNext just pop index 0
	// for the highest-priority bucket the dispatcher is asking for.
	b.eventQueue = append(b.eventQueue, e)
	sort.SliceStable(b.eventQueue, func(i, j int) bool {
		if b.eventQueue[i].Priority != b.eventQueue[j].Priority {
			return b.eventQueue[i].Priority > b.eventQueue[j].Priority
		}
		return b.eventQueue[i].At.Before(b.eventQueue[j].At)
	})

	// Cap. If we're over, evict the LOWEST priority OLDEST first (back of slice).
	if len(b.eventQueue) > maxEventQueue {
		b.eventQueue = b.eventQueue[:maxEventQueue]
	}

	stored := b.eventQueue[indexOfEvent(b.eventQueue, e.ID)]
	hook := b.transcriptHook
	// Release the lock before invoking the hook so a slow hook can't
	// stall enqueue critical sections.
	go func(h TranscriptHook, ev Event) {
		if h == nil {
			return
		}
		h.AppendEventLifecycle("event_dispatched", "event_bus", string(ev.Type), map[string]any{
			"event_id": ev.ID,
			"priority": ev.Priority,
			"summary":  ev.Summary,
			"source":   ev.Source,
		})
	}(hook, stored)
	return &stored, nil
}

// DequeueNext is the legacy "pop and forget" path. New code should prefer
// LeaseNext + AckDelivered/NackInterrupted so the queue can retry on user
// interrupt. Kept for callers that don't need delivery confirmation
// semantics (debug tools, tests). Skips events that are currently in_flight
// — those have already been handed to a dispatcher.
func (b *RaceBrain) DequeueNext(minPriority, maxPriority int) *Event {
	minPriority, maxPriority = clampPriority(minPriority, maxPriority)

	b.mu.Lock()
	defer b.mu.Unlock()

	for i, e := range b.eventQueue {
		if e.Status == StatusInFlight {
			continue
		}
		if e.Priority >= minPriority && e.Priority <= maxPriority {
			b.eventQueue = append(b.eventQueue[:i], b.eventQueue[i+1:]...)
			return &e
		}
	}
	return nil
}

// LeaseNext returns the highest-priority queued event in [minPriority,
// maxPriority] and marks it in_flight. Single-flight: returns nil if any
// other event in the queue is already in_flight, so the dispatcher can never
// have two outstanding leases at once.
//
// The lease is closed by AckDelivered (event removed, RecordSpeech called)
// or NackInterrupted (event flipped back to queued, Attempts incremented).
// If neither lands within LeaseTimeout, ReapStaleLeases nacks it for retry.
//
// LeaseNext also evicts events past their StaleAfter TTL on the way through
// — that keeps the queue from growing stale entries that would never be
// useful to deliver.
func (b *RaceBrain) LeaseNext(minPriority, maxPriority int) *Event {
	minPriority, maxPriority = clampPriority(minPriority, maxPriority)

	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()

	// Drop stale events first. We scan the whole queue rather than only the
	// requested priority band so a stale low-priority event doesn't sit
	// forever just because nobody's polling for it.
	b.eventQueue = filterStaleLocked(b.eventQueue, now)

	// Single-flight invariant: refuse to lease while another event is in_flight.
	for _, e := range b.eventQueue {
		if e.Status == StatusInFlight {
			return nil
		}
	}

	// Queue is sorted by priority desc; first matching queued event wins.
	for i := range b.eventQueue {
		e := &b.eventQueue[i]
		if e.Status != StatusQueued {
			continue
		}
		if e.Priority < minPriority || e.Priority > maxPriority {
			continue
		}
		e.Status = StatusInFlight
		e.InFlightAt = now
		e.Attempts++
		// Return a copy so callers can't mutate queue state without going
		// through Ack/Nack.
		out := *e
		return &out
	}
	return nil
}

// AckDelivered marks the in_flight event as successfully spoken: records the
// spoken text into the L4 speech buffer for dedup-against-recent-radio, then
// either removes the event from the queue (RequiresAck=false) or flips it to
// StatusAwaitingAck and emits an `event_awaiting_ack` transcript line so the
// next snapshot surface advertises it.
//
// spokenText may differ from the event's summary (the dispatcher's TTS
// rephrases) — store whatever the dispatcher reports.
//
// Returns ErrEventNotFound if the id isn't present (already removed by an
// earlier ack, or the lease was reaped).
func (b *RaceBrain) AckDelivered(eventID, spokenText string, priority int) error {
	if eventID == "" {
		return ErrEventNotFound
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	for i := range b.eventQueue {
		if b.eventQueue[i].ID != eventID {
			continue
		}
		e := b.eventQueue[i]
		text := spokenText
		if text == "" {
			text = e.Summary
		}
		p := priority
		if p < 1 || p > 5 {
			p = e.Priority
		}
		b.speech = appendBounded(b.speech, SpeechRecord{
			At:       b.now(),
			Text:     text,
			Priority: p,
			Subject:  e.DedupSubject,
		}, maxRecentSpeech)

		hook := b.transcriptHook

		if e.RequiresAck {
			// Hold the event in the queue under StatusAwaitingAck. It is
			// no longer in-flight (we just finished speaking it) but
			// must not be re-leased until either:
			//   - ConfirmAck removes it (driver copied), or
			//   - ReapAwaitingAcks flips it back to queued for a renag,
			//   - NagCount exceeds MaxAckNags (silently dropped + Observation).
			b.eventQueue[i].Status = StatusAwaitingAck
			b.eventQueue[i].InFlightAt = time.Time{}
			b.eventQueue[i].AwaitingAckSince = b.now()

			evCopy := b.eventQueue[i]
			spoken := text
			go func(h TranscriptHook) {
				if h == nil {
					return
				}
				h.AppendEventLifecycle("event_awaiting_ack", "event_bus", string(evCopy.Type), map[string]any{
					"event_id":    evCopy.ID,
					"priority":    p,
					"attempts":    evCopy.Attempts,
					"spoken_text": spoken,
					"nag_count":   evCopy.NagCount,
				})
			}(hook)
			return nil
		}

		// No-ack path: original semantics — drop from the queue.
		b.eventQueue = append(b.eventQueue[:i], b.eventQueue[i+1:]...)
		evCopy := e
		spoken := text
		go func(h TranscriptHook) {
			if h == nil {
				return
			}
			h.AppendEventLifecycle("event_delivered", "event_bus", string(evCopy.Type), map[string]any{
				"event_id":    evCopy.ID,
				"priority":    p,
				"attempts":    evCopy.Attempts,
				"spoken_text": spoken,
			})
		}(hook)
		return nil
	}
	return ErrEventNotFound
}

// ConfirmAck removes an awaiting-ack event from the queue, emitting an
// `event_acked` transcript record with the driver-evidence string.
// Idempotent: returns ErrEventNotFound when no such event exists (e.g.
// double-ack from a stale snapshot in the agent's prompt).
func (b *RaceBrain) ConfirmAck(eventID, evidence string) error {
	if eventID == "" {
		return ErrEventNotFound
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	for i, e := range b.eventQueue {
		if e.ID != eventID {
			continue
		}
		if e.Status != StatusAwaitingAck {
			// Only awaiting-ack events are eligible for ConfirmAck.
			// If somebody calls this on a queued or in-flight event
			// we surface the same not-found sentinel so the agent
			// doesn't double-treat a non-ack-bearing event.
			return ErrEventNotFound
		}
		acked := e
		b.eventQueue = append(b.eventQueue[:i], b.eventQueue[i+1:]...)
		hook := b.transcriptHook
		ev := acked
		go func(h TranscriptHook) {
			if h == nil {
				return
			}
			h.AppendEventLifecycle("event_acked", "event_bus", string(ev.Type), map[string]any{
				"event_id":  ev.ID,
				"priority":  ev.Priority,
				"nag_count": ev.NagCount,
				"evidence":  evidence,
			})
		}(hook)
		return nil
	}
	return ErrEventNotFound
}

// NagAck handles an awaiting-ack event whose AckNagAfter quiet period has
// elapsed without a confirm. Flips status back to queued so LeaseNext picks
// it up again, increments NagCount, and resets Attempts so the priority's
// retry cap doesn't immediately burn out on the renag.
//
// If NagCount would exceed MaxAckNags, the event is dropped instead, an
// Observation is recorded under TopicDelivery, and an event_dropped
// transcript record is emitted.
//
// Returns ErrEventNotFound if the id isn't present or the event is not in
// StatusAwaitingAck.
func (b *RaceBrain) NagAck(eventID string) error {
	if eventID == "" {
		return ErrEventNotFound
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	for i := range b.eventQueue {
		e := &b.eventQueue[i]
		if e.ID != eventID {
			continue
		}
		if e.Status != StatusAwaitingAck {
			return ErrEventNotFound
		}

		if e.NagCount+1 >= MaxAckNags {
			dropped := *e
			b.eventQueue = append(b.eventQueue[:i], b.eventQueue[i+1:]...)
			obs := Observation{
				At:    b.now(),
				Topic: TopicDelivery,
				Agent: "event_bus",
				Summary: "dropped " + string(dropped.Type) +
					" after " + itoa(dropped.NagCount+1) +
					" unacknowledged nags",
			}
			bucket := append(b.observations[obs.Topic], obs)
			if len(bucket) > maxObservationsPerTopic {
				bucket = bucket[len(bucket)-maxObservationsPerTopic:]
			}
			b.observations[obs.Topic] = bucket

			hook := b.transcriptHook
			go func(h TranscriptHook) {
				if h == nil {
					return
				}
				h.AppendEventLifecycle("event_dropped", "event_bus", string(dropped.Type), map[string]any{
					"event_id":  dropped.ID,
					"priority":  dropped.Priority,
					"nag_count": dropped.NagCount + 1,
					"reason":    "unacknowledged_nags_exceeded",
				})
			}(hook)
			return nil
		}

		e.Status = StatusQueued
		e.NagCount++
		e.Attempts = 0
		e.InFlightAt = time.Time{}
		e.AwaitingAckSince = time.Time{}
		// Refresh the queued-since timestamp so renagged events compete
		// fairly with newly-enqueued same-priority peers (we don't want
		// the old timestamp to permanently pin them to the front).
		e.At = b.now()

		// Re-sort to maintain priority/FIFO invariant.
		sort.SliceStable(b.eventQueue, func(i, j int) bool {
			if b.eventQueue[i].Priority != b.eventQueue[j].Priority {
				return b.eventQueue[i].Priority > b.eventQueue[j].Priority
			}
			return b.eventQueue[i].At.Before(b.eventQueue[j].At)
		})

		nagged := *e
		hook := b.transcriptHook
		go func(h TranscriptHook) {
			if h == nil {
				return
			}
			h.AppendEventLifecycle("event_nagged", "event_bus", string(nagged.Type), map[string]any{
				"event_id":  nagged.ID,
				"priority":  nagged.Priority,
				"nag_count": nagged.NagCount,
			})
		}(hook)
		return nil
	}
	return ErrEventNotFound
}

// ReapAwaitingAcks scans for events in StatusAwaitingAck whose quiet period
// has elapsed (AckNagAfter) and calls NagAck on each. Run alongside the
// existing lease reaper. Safe to call directly from tests.
func (b *RaceBrain) ReapAwaitingAcks() int {
	b.mu.Lock()
	now := b.now()
	due := make([]string, 0, 2)
	for _, e := range b.eventQueue {
		if e.Status == StatusAwaitingAck && now.Sub(e.AwaitingAckSince) > AckNagAfter {
			due = append(due, e.ID)
		}
	}
	b.mu.Unlock()

	for _, id := range due {
		_ = b.NagAck(id)
	}
	return len(due)
}

// DropDelivered removes an in_flight event from the queue WITHOUT
// re-queueing it for retry. Used by the dispatcher when it detects a
// spurious server-side `interrupted` (no driver audio, no streamed model
// audio) — re-queueing in that case just produces the same redundant
// radio call. The next refire of the underlying rule will create a fresh
// event with fresh dedup.
//
// Records an Observation under TopicDelivery so the analyst can see the
// drop happened. Returns ErrEventNotFound if the id isn't present.
func (b *RaceBrain) DropDelivered(eventID, reason string) error {
	if eventID == "" {
		return ErrEventNotFound
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	for i, e := range b.eventQueue {
		if e.ID != eventID {
			continue
		}
		dropped := e
		b.eventQueue = append(b.eventQueue[:i], b.eventQueue[i+1:]...)
		obs := Observation{
			At:    b.now(),
			Topic: TopicDelivery,
			Agent: "event_bus",
			Summary: "dropped " + string(dropped.Type) +
				" without retry: " + reason,
		}
		bucket := append(b.observations[obs.Topic], obs)
		if len(bucket) > maxObservationsPerTopic {
			bucket = bucket[len(bucket)-maxObservationsPerTopic:]
		}
		b.observations[obs.Topic] = bucket

		hook := b.transcriptHook
		go func(h TranscriptHook) {
			if h == nil {
				return
			}
			h.AppendEventLifecycle("event_dropped", "event_bus", string(dropped.Type), map[string]any{
				"event_id": dropped.ID,
				"priority": dropped.Priority,
				"attempts": dropped.Attempts,
				"reason":   reason,
			})
		}(hook)
		return nil
	}
	return ErrEventNotFound
}

// NackInterrupted flips the in_flight event back to queued so a future
// LeaseNext can pick it up again. Records the time and reason on the event
// for diagnostics. If Attempts exceeds the priority's retry cap, the event
// is dropped entirely with reason recorded as a brain Observation under
// topic "delivery".
//
// Returns ErrEventNotFound if the id isn't present.
func (b *RaceBrain) NackInterrupted(eventID, reason string) error {
	if eventID == "" {
		return ErrEventNotFound
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	for i := range b.eventQueue {
		e := &b.eventQueue[i]
		if e.ID != eventID {
			continue
		}
		e.LastNackAt = b.now()

		// Dead-consumer reasons short-circuit the retry budget: the
		// downstream is gone (or in the middle of reconnecting), so
		// burning all 3 attempts against the same dead socket within a
		// single second just churns the log without ever delivering.
		// The supervisor in api/live_ws.go re-opens the session; any
		// event still useful by the time it's back will be re-emitted
		// by its producer with fresh state.
		if isDeadConsumerReason(reason) {
			e.Attempts = MaxAttemptsForPriority(e.Priority)
		}

		cap := MaxAttemptsForPriority(e.Priority)
		if e.Attempts >= cap {
			// Drop. Record an Observation so the analyst can see what
			// happened. We're already holding b.mu, so write directly into
			// the observations map instead of calling Write().
			dropped := *e
			b.eventQueue = append(b.eventQueue[:i], b.eventQueue[i+1:]...)
			obs := Observation{
				At:    b.now(),
				Topic: TopicDelivery,
				Agent: "event_bus",
				Summary: "dropped " + string(dropped.Type) +
					" after " + itoa(dropped.Attempts) +
					" interrupted attempts: " + reason,
			}
			bucket := append(b.observations[obs.Topic], obs)
			if len(bucket) > maxObservationsPerTopic {
				bucket = bucket[len(bucket)-maxObservationsPerTopic:]
			}
			b.observations[obs.Topic] = bucket

			hook := b.transcriptHook
			go func(h TranscriptHook) {
				if h == nil {
					return
				}
				h.AppendEventLifecycle("event_dropped", "event_bus", string(dropped.Type), map[string]any{
					"event_id": dropped.ID,
					"priority": dropped.Priority,
					"attempts": dropped.Attempts,
					"reason":   reason,
				})
			}(hook)
			return nil
		}

		// Re-queue: flip status back, clear InFlightAt. Attempts already
		// incremented at lease time, so no double-count here.
		e.Status = StatusQueued
		e.InFlightAt = time.Time{}
		return nil
	}
	return ErrEventNotFound
}

// ReapStaleLeases scans for events that have been in_flight longer than
// their per-event lease budget (LeaseTimeoutFor) and nacks them with reason
// "lease_timeout". Run periodically by RunReaper. Safe to call directly
// from tests.
func (b *RaceBrain) ReapStaleLeases() int {
	b.mu.Lock()
	now := b.now()
	stale := make([]string, 0, 2)
	for _, e := range b.eventQueue {
		if e.Status == StatusInFlight && now.Sub(e.InFlightAt) > LeaseTimeoutFor(e) {
			stale = append(stale, e.ID)
		}
	}
	b.mu.Unlock()

	// Call NackInterrupted outside the lock so it can re-acquire and apply
	// the retry-cap drop logic uniformly.
	for _, id := range stale {
		_ = b.NackInterrupted(id, "lease_timeout")
	}
	return len(stale)
}

// RunReaper runs ReapStaleLeases AND ReapAwaitingAcks on a ticker until ctx
// is cancelled. Call once from main on startup; one goroutine is enough.
func (b *RaceBrain) RunReaper(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.ReapStaleLeases()
			b.ReapAwaitingAcks()
		}
	}
}

// filterStaleLocked drops events whose age exceeds StaleAfter. Caller must
// hold b.mu. In_flight and awaiting-ack events are exempt — in-flight may be
// slow to deliver, awaiting-ack is too important to silently TTL out
// (MaxAckNags is its real ceiling); the lease/ack reapers handle those.
func filterStaleLocked(q []Event, now time.Time) []Event {
	out := q[:0]
	for _, e := range q {
		if e.Status == StatusInFlight || e.Status == StatusAwaitingAck {
			out = append(out, e)
			continue
		}
		if e.StaleAfter > 0 && now.Sub(e.At) > e.StaleAfter {
			continue
		}
		out = append(out, e)
	}
	return out
}

func clampPriority(minP, maxP int) (int, int) {
	if minP < 1 {
		minP = 1
	}
	if maxP < minP {
		maxP = minP
	}
	if maxP > 5 {
		maxP = 5
	}
	return minP, maxP
}

// itoa is an inline integer-to-string used by NackInterrupted to keep the
// observation message allocation-free without importing strconv.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// PeekEvents returns a snapshot copy of the current queue (oldest of each
// priority first within the priority order). For debug + dashboard.
func (b *RaceBrain) PeekEvents() []Event {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.eventQueue) == 0 {
		return nil
	}
	out := make([]Event, len(b.eventQueue))
	copy(out, b.eventQueue)
	return out
}

// mergeIntoPeer folds the incoming event's facts into an existing queued
// peer. Used by both the threat-coalesce and the high-priority-coalesce
// passes in EnqueueEvent, and by MergePendingInto when the live dispatcher
// asks the bus to fold any other pending P4+ events into the one already
// in_flight.
//
// Merge policy:
//   - Summary: " | "-joined, capped at MaxEventSummaryChars; if the join
//     would overflow, the appended part is truncated rather than dropped
//     so the model still sees a hint of the second event.
//   - Reasoning: " ALSO: "-joined, capped at MaxEventReasoningChars.
//   - Priority: max(peer, incoming) — bundling a P4 with a P5 stays P5.
//   - DedupSubject: "+"-joined when both sides are non-empty; otherwise
//     whichever side has a value.
func mergeIntoPeer(peer *Event, incoming Event) {
	if peer == nil {
		return
	}
	if incoming.Summary != "" && !strings.Contains(peer.Summary, incoming.Summary) {
		candidate := peer.Summary + " | " + incoming.Summary
		if len(candidate) <= MaxEventSummaryChars {
			peer.Summary = candidate
		} else if len(peer.Summary) < MaxEventSummaryChars {
			room := MaxEventSummaryChars - len(peer.Summary) - 3
			if room > 8 {
				if room > len(incoming.Summary) {
					room = len(incoming.Summary)
				}
				peer.Summary = peer.Summary + " | " + incoming.Summary[:room]
			}
		}
	}
	if extraReason := strings.TrimSpace(incoming.Reasoning); extraReason != "" {
		combined := peer.Reasoning + " ALSO: " + extraReason
		if len(combined) > MaxEventReasoningChars {
			combined = combined[:MaxEventReasoningChars]
		}
		peer.Reasoning = combined
	}
	if incoming.Priority > peer.Priority {
		peer.Priority = incoming.Priority
	}
	switch {
	case peer.DedupSubject == "" && incoming.DedupSubject != "":
		peer.DedupSubject = incoming.DedupSubject
	case peer.DedupSubject != "" && incoming.DedupSubject != "" && peer.DedupSubject != incoming.DedupSubject:
		peer.DedupSubject = peer.DedupSubject + "+" + incoming.DedupSubject
	}
}

// MergePendingInto folds every queued P>=floor event into the in_flight
// event with id targetID, returning the (mutated) target plus the list of
// merged sibling ids. Used by the live dispatcher's pre-dispatch bundle
// window: after leasing an event and pausing briefly, the dispatcher
// asks the bus to absorb any siblings that arrived during the gather
// window so they all dispatch as one prompt and ack in a single
// turn_complete.
//
// Returns (nil, nil) if targetID isn't currently in_flight. Siblings that
// share a DedupSubject with the target are skipped (already represented).
// Each merged sibling is removed from the queue and gets an
// `event_delivered` lifecycle entry with `merged_into: <targetID>` so the
// transcript shows what was folded together.
func (b *RaceBrain) MergePendingInto(targetID string, floor int) (*Event, []string) {
	if targetID == "" {
		return nil, nil
	}
	if floor < 1 {
		floor = 1
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	var target *Event
	for i := range b.eventQueue {
		if b.eventQueue[i].ID == targetID {
			target = &b.eventQueue[i]
			break
		}
	}
	if target == nil || target.Status != StatusInFlight {
		return nil, nil
	}

	hook := b.transcriptHook
	merged := make([]string, 0, 2)
	merges := make([]Event, 0, 2)

	// Two-pass: collect ids to merge, then rebuild the queue without
	// them. Mutating the slice during iteration is doable but error-prone.
	keep := make([]Event, 0, len(b.eventQueue))
	for _, ev := range b.eventQueue {
		if ev.ID == targetID {
			keep = append(keep, ev)
			continue
		}
		if ev.Status != StatusQueued {
			keep = append(keep, ev)
			continue
		}
		if ev.Priority < floor {
			keep = append(keep, ev)
			continue
		}
		if ev.DedupSubject != "" && ev.DedupSubject == target.DedupSubject {
			keep = append(keep, ev)
			continue
		}
		merges = append(merges, ev)
		merged = append(merged, ev.ID)
	}
	if len(merges) == 0 {
		return nil, nil
	}
	b.eventQueue = keep

	// Re-find the target in the rebuilt slice.
	target = nil
	for i := range b.eventQueue {
		if b.eventQueue[i].ID == targetID {
			target = &b.eventQueue[i]
			break
		}
	}
	if target == nil {
		return nil, nil
	}
	for i := range merges {
		mergeIntoPeer(target, merges[i])
	}
	out := *target

	go func(h TranscriptHook, parent string, siblings []Event) {
		if h == nil {
			return
		}
		for _, s := range siblings {
			h.AppendEventLifecycle("event_delivered", "event_bus", string(s.Type), map[string]any{
				"event_id":    s.ID,
				"priority":    s.Priority,
				"merged_into": parent,
				"summary":     s.Summary,
			})
		}
	}(hook, targetID, merges)

	return &out, merged
}

// ClearEventQueue is for tests only — wipes the queue without consuming.
func (b *RaceBrain) ClearEventQueue() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.eventQueue = nil
}

func indexOfEvent(q []Event, id string) int {
	for i, e := range q {
		if e.ID == id {
			return i
		}
	}
	return 0 // unreachable in practice — we just inserted it
}
