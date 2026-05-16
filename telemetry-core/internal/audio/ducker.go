// Package audio implements volume ducking of external music players
// (Spotify, Apple Music, browsers, VLC, ...) while the race engineer is
// speaking. A single Coordinator owns the debounce timer; OS-specific
// Ducker implementations (build-tagged per platform) only see clean
// on/off edges.
package audio

import (
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// Mode selects how the engineer's speech affects external music.
//
//   - ModeDuck   — lower the music player's volume to a fraction (the
//                  AUDIO_DUCKING_LEVEL config) while the engineer
//                  speaks, then restore. The song keeps playing,
//                  quietly.
//   - ModePause  — pause the music player on speech start, resume on
//                  speech end. The song doesn't progress while paused,
//                  so the driver doesn't miss any of it.
type Mode int

const (
	ModeDuck Mode = iota
	ModePause
)

// ParseMode maps the config string value to a Mode. Unknown values
// fall back to ModeDuck — the historical behaviour.
func ParseMode(s string) Mode {
	switch s {
	case "pause":
		return ModePause
	default:
		return ModeDuck
	}
}

// modeName is the short human-readable tag used in log messages. Lives
// in the shared file so every per-OS ducker can use it without
// re-declaring it under each build tag.
func modeName(m Mode) string {
	if m == ModePause {
		return "pause"
	}
	return "duck"
}

// Ducker is the platform-specific volume controller. SetDucked is called
// only on edges — once with true when speech starts, once with false
// after the unduck timer fires. The interface name is historical; in
// ModePause the same on/off edges drive media play/pause instead of a
// volume change. Implementations MUST be idempotent (the Coordinator
// already filters redundant edges, but a defensive impl costs nothing)
// and SHOULD return quickly: spawn a goroutine internally if the
// underlying syscall is slow.
type Ducker interface {
	SetDucked(ducked bool) error
	// Close releases any held resources (COM uninit on Windows,
	// restoring any saved volumes that the OS impl is still tracking,
	// etc.). Called once at shutdown.
	Close() error
}

// Coordinator turns "speech is happening" notifications from multiple
// sources (batched TTS, streaming Gemini Live) into clean duck/unduck
// edges for the OS-specific Ducker. Safe for concurrent use.
type Coordinator struct {
	duck Ducker

	mu     sync.Mutex
	timer  *time.Timer
	gen    uint64 // increments on every extend; an unduck callback compares.
	active bool   // true once SetDucked(true) has been issued and not yet undone
	closed bool

	// playbackEnd tracks the wall-clock instant by which all chunks
	// fed to SpeakingChunk are expected to have finished playing. Used
	// only by the streaming-aware code path so a fast-than-realtime
	// producer (Gemini Live emits ~5 s of audio in ~1 s wall time)
	// can extend the unduck deadline by actual playback time instead
	// of wall-clock-since-last-chunk. Zero value = no pending playback.
	playbackEnd time.Time

	// afterFunc is a test seam for time.AfterFunc. Production leaves it
	// nil and the Coordinator uses the stdlib.
	afterFunc func(d time.Duration, f func()) *time.Timer
}

// NewCoordinator wires a fresh Coordinator over the given Ducker. The
// returned value is safe to use even when duck is nil — every method
// becomes a no-op, which keeps call sites free of nil checks.
func NewCoordinator(d Ducker) *Coordinator {
	return &Coordinator{duck: d}
}

// SpeakingFor declares that speech of the given duration just started.
// Ducks immediately if not already ducked; schedules the unduck for
// now+d. Overlapping calls extend the unduck deadline to the maximum,
// they don't shorten it. Use this from batched TTS where audio length
// is known up front.
func (c *Coordinator) SpeakingFor(d time.Duration) {
	if c == nil || c.duck == nil || d <= 0 {
		return
	}
	c.extend(d)
}

// SpeakingTick declares that a chunk of streaming speech just landed.
// Ducks immediately if not already ducked; schedules the unduck for
// now+tail.
//
// Deprecated: use SpeakingChunk for streaming sources. SpeakingTick
// only debounces wall-clock-since-last-chunk, which under-counts when
// the producer streams faster than realtime (e.g. Gemini Live emits
// ~5 s of audio in ~1 s wall time) — the unduck fires while the
// consumer is still playing the buffered tail. Kept for callers that
// genuinely don't know per-chunk playback duration.
func (c *Coordinator) SpeakingTick(tail time.Duration) {
	if c == nil || c.duck == nil || tail <= 0 {
		return
	}
	c.extend(tail)
}

// SpeakingChunk declares that `chunkDuration` worth of audio playback
// has just been queued to the consumer. The Coordinator accumulates
// queued playback time so the unduck fires only after the consumer
// has actually finished draining its buffer, regardless of how fast
// the producer pushed bytes. Designed for streaming sources where the
// producer emits faster than realtime (Gemini Live, mic frame
// forwarding).
//
// tail is the safety margin added after playbackEnd so brief inter-
// chunk silences don't toggle the duck. ~800 ms is typical.
func (c *Coordinator) SpeakingChunk(chunkDuration, tail time.Duration) {
	if c == nil || c.duck == nil || chunkDuration <= 0 {
		return
	}
	c.extendChunk(chunkDuration, tail)
}

// Stop forces an immediate unduck and cancels any pending timer. Safe
// to call multiple times. Call from the main shutdown path so music
// isn't left at the ducked level after a SIGINT mid-speech.
func (c *Coordinator) Stop() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	wasActive := c.active
	c.active = false
	c.gen++ // invalidate any in-flight callback
	c.playbackEnd = time.Time{}
	d := c.duck
	c.mu.Unlock()

	if wasActive && d != nil {
		if err := d.SetDucked(false); err != nil {
			log.Warn().Err(err).Msg("audio: unduck on shutdown failed")
		}
	}
	if d != nil {
		if err := d.Close(); err != nil {
			log.Debug().Err(err).Msg("audio: ducker close failed")
		}
	}
}

// extend is the shared core: duck-if-needed and (re)arm the unduck
// timer for `d` from now.
func (c *Coordinator) extend(d time.Duration) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	needDuck := c.armLocked(d)
	dk := c.duck
	c.mu.Unlock()

	if needDuck && dk != nil {
		if err := dk.SetDucked(true); err != nil {
			log.Warn().Err(err).Msg("audio: duck failed; speech may be drowned out")
		}
	}
}

// extendChunk arms the timer based on cumulative playback time. Each
// chunkDuration extends playbackEnd by exactly its playback length so
// the unduck deadline tracks "buffer drained" instead of "wall-clock
// since last chunk".
func (c *Coordinator) extendChunk(chunkDuration, tail time.Duration) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	now := time.Now()
	if c.playbackEnd.Before(now) {
		// First chunk of a new turn (or a turn that fell behind). Anchor
		// playback at "now" so the chunk's duration accumulates forward.
		c.playbackEnd = now
	}
	c.playbackEnd = c.playbackEnd.Add(chunkDuration)
	delay := c.playbackEnd.Add(tail).Sub(now)
	if delay <= 0 {
		// Defensive: chunkDuration negative was guarded at the API
		// surface, but pin to a millisecond if the calculation rounds
		// to zero so the timer always fires through the gen path.
		delay = time.Millisecond
	}
	needDuck := c.armLocked(delay)
	dk := c.duck
	c.mu.Unlock()

	if needDuck && dk != nil {
		if err := dk.SetDucked(true); err != nil {
			log.Warn().Err(err).Msg("audio: duck failed; speech may be drowned out")
		}
	}
}

// armLocked sets active=true, bumps the generation, cancels any prior
// timer, and schedules a fresh unduck after delay. Returns whether
// SetDucked(true) needs to be issued (i.e. the rising edge). Caller
// must hold c.mu and must check c.closed first.
func (c *Coordinator) armLocked(delay time.Duration) bool {
	needDuck := !c.active
	c.active = true
	c.gen++
	gen := c.gen
	if c.timer != nil {
		c.timer.Stop()
	}
	c.timer = c.after(delay, func() { c.unduck(gen) })
	return needDuck
}

// unduck is the timer callback. gen identifies which extend() armed
// this timer — if a newer extend() bumped the generation we silently
// drop the call (the new timer will fire later).
func (c *Coordinator) unduck(gen uint64) {
	c.mu.Lock()
	if c.closed || !c.active || c.gen != gen {
		c.mu.Unlock()
		return
	}
	c.active = false
	c.timer = nil
	c.playbackEnd = time.Time{}
	d := c.duck
	c.mu.Unlock()

	if d != nil {
		if err := d.SetDucked(false); err != nil {
			log.Warn().Err(err).Msg("audio: unduck failed; player volume may be stuck low")
		}
	}
}

// after is the test seam for time.AfterFunc.
func (c *Coordinator) after(d time.Duration, f func()) *time.Timer {
	if c.afterFunc != nil {
		return c.afterFunc(d, f)
	}
	return time.AfterFunc(d, f)
}
