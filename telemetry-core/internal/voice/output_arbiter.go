package voice

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// OutputArbiter serializes audio output across producers.
//
// At most one producer holds the speaking slot at any moment. The
// arbiter is the single runtime invariant that prevents two voices
// playing simultaneously — every audio producer (the in-process Gemini
// Live agent, the legacy TTS path via VoiceClient, future event-driven
// speakers) must Acquire before emitting audio and Release when done.
//
// The arbiter is intentionally tiny — there is no priority ordering, no
// queue, no fairness logic. The only invariant is "at most one owner."
// Real-world scheduling (interrupting Live mid-phrase to speak an
// urgent strategy insight) is handled by the brain Interrupts Bus
// upstream; the arbiter just enforces "they take turns at the mic."
//
// Today the only active producer is the Live agent. The value of the
// arbiter is not in solving today's bug (Layer 1 + Layer 2 do that) —
// it's in making it physically impossible for a future contributor to
// add a fourth audio path that quietly speaks over Live. They have to
// hold the arbiter; if Live holds it, their broadcast waits.
type OutputArbiter struct {
	mu     sync.Mutex
	owner  string
	holder context.CancelFunc // cancellation handle for preemptable holders
	since  time.Time
}

// NewOutputArbiter returns an arbiter in the idle state.
func NewOutputArbiter() *OutputArbiter {
	return &OutputArbiter{}
}

// ErrPreempted is returned (via the holder's context) when a
// higher-priority Acquire preempts the current owner. Callers should
// stop emitting audio when their context is cancelled with this cause.
var ErrPreempted = errors.New("audio output preempted")

// Acquire blocks until the arbiter's slot is free or ctx is cancelled.
// owner is a short tag ("live", "voice_client", "ack") used for
// diagnostics. When preempt is true the call cancels the current
// holder's ctx (via the cancel func they registered on Acquire) and
// proceeds; the previous holder's ctx will return ctx.Err()==context.Canceled
// with cause ErrPreempted. Returns a release func — call exactly once
// when done speaking.
//
// preemptable: pass the cancel func of a context the caller is willing
// to have cancelled by a future preempter. Pass nil if the caller's
// audio is not preemptable (e.g. a short ack phrase that finishes in
// 200ms anyway — let it run).
func (a *OutputArbiter) Acquire(ctx context.Context, owner string, preempt bool, preemptable context.CancelFunc) (release func(), err error) {
	if owner == "" {
		owner = "anonymous"
	}

	for {
		a.mu.Lock()
		if a.owner == "" {
			a.owner = owner
			a.holder = preemptable
			a.since = time.Now()
			a.mu.Unlock()
			log.Debug().Str("owner", owner).Msg("audio arbiter: acquired")
			return a.releaseFor(owner), nil
		}

		// Slot is held.
		if preempt && a.holder != nil {
			prevOwner := a.owner
			prevHolder := a.holder
			a.owner = owner
			a.holder = preemptable
			a.since = time.Now()
			a.mu.Unlock()
			log.Info().Str("new_owner", owner).Str("preempted", prevOwner).Msg("audio arbiter: preempting holder")
			// Cancel outside the lock — the previous holder's release
			// will try to take the lock when it cleans up.
			prevHolder()
			return a.releaseFor(owner), nil
		}

		// Wait for release. We loop because Go has no condvar primitive
		// that integrates with context.Done; a 25ms poll is fine for an
		// event that fires at most a few times per minute.
		a.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
			// retry
		}
	}
}

// releaseFor returns a one-shot release closure. The closure is no-op
// if the slot has since changed owner (e.g. the holder was preempted
// and called release after the new owner had already claimed it).
func (a *OutputArbiter) releaseFor(claimedBy string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.owner != claimedBy {
				// Already preempted; the new owner holds the slot.
				return
			}
			a.owner = ""
			a.holder = nil
			a.since = time.Time{}
			log.Debug().Str("owner", claimedBy).Msg("audio arbiter: released")
		})
	}
}

// Owner returns the current speaking owner, or "" if idle. For
// diagnostics — exposed on /health so the dashboard can show what's
// speaking.
func (a *OutputArbiter) Owner() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.owner
}

// HeldFor returns how long the current owner has held the slot, or 0
// if idle. Used to detect a stuck arbiter in monitoring.
func (a *OutputArbiter) HeldFor() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.owner == "" {
		return 0
	}
	return time.Since(a.since)
}
