package transcript

import (
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
)

// subscriber is one live tap into the hub's event stream. ch carries
// copies of every successfully written event; lagged counts non-blocking
// sends that dropped because the consumer wasn't draining fast enough.
type subscriber struct {
	ch     chan Event
	lagged atomic.Int64
}

// Subscribe registers a new tap on the event stream. The returned channel
// receives a copy of every event after it has been written to disk and
// appended to the in-memory ring. buffer caps how many events can sit in
// the channel before the hub starts dropping (drop-oldest semantics, so
// a slow consumer never blocks producers).
//
// The cancel func unsubscribes and closes the channel. Always call it
// when done — leaking subscribers leak event copies and channel memory.
func (h *Hub) Subscribe(buffer int) (<-chan Event, func()) {
	if buffer <= 0 {
		buffer = 256
	}
	sub := &subscriber{ch: make(chan Event, buffer)}

	h.mu.Lock()
	h.subs = append(h.subs, sub)
	h.mu.Unlock()

	cancel := func() {
		h.mu.Lock()
		for i, s := range h.subs {
			if s == sub {
				h.subs = append(h.subs[:i], h.subs[i+1:]...)
				break
			}
		}
		h.mu.Unlock()
		close(sub.ch)
	}
	return sub.ch, cancel
}

// fanOutLocked sends a copy of e to every subscriber. Non-blocking: if a
// subscriber's channel is full, the event is dropped and the per-subscriber
// lagged counter is incremented. A throttled warn log surfaces lagging
// consumers so they show up in the debug stream itself.
//
// Caller MUST hold h.mu.Lock — this is invoked from write() while we
// already hold the write lock for the disk/ring update.
func (h *Hub) fanOutLocked(e Event) {
	for _, s := range h.subs {
		select {
		case s.ch <- e:
		default:
			n := s.lagged.Add(1)
			h.maybeWarnLagged(s, n)
		}
	}
}

// maybeWarnLagged emits a warn log at most once every laggedWarnEvery for
// a given subscriber that's been dropping events. The window resets after
// each warn so a chronic consumer keeps surfacing in logs without spam.
func (h *Hub) maybeWarnLagged(s *subscriber, n int64) {
	now := h.cfg.Now()
	last := h.laggedWarnAt.Load()
	if last != nil {
		if t, ok := last.(time.Time); ok && now.Sub(t) < laggedWarnEvery {
			return
		}
	}
	h.laggedWarnAt.Store(now)
	log.Warn().Int64("dropped", n).Msg("transcript: subscriber lagging — events dropped")
}

const laggedWarnEvery = 5 * time.Second
