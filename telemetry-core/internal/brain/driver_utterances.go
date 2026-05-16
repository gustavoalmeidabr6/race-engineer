package brain

import "time"

// maxDriverUtterances bounds the ring buffer of driver utterances captured
// from Gemini Live's InputTranscription and the PTT path. 20 covers the
// recent minutes of conversation without ballooning prompt context.
const maxDriverUtterances = 20

// DriverUtterance is one captured driver utterance. The agent reads
// OpenDriverQuestions on every turn so anything the driver said that the
// engineer hasn't yet substantively answered surfaces in the brain
// snapshot. Addressed flips true when the agent calls
// MarkUtteranceAddressed (explicit) or when an engineer-speech record
// lands within the auto-mark window after the utterance.
type DriverUtterance struct {
	At          time.Time `json:"at"`
	Text        string    `json:"text"`
	Addressed   bool      `json:"addressed"`
	AddressedAt time.Time `json:"addressed_at,omitempty"`
	// Reason is optional context recorded when MarkUtteranceAddressed is
	// called explicitly (e.g. "answered via push_strategy_insight"). Kept
	// short so the brain snapshot stays compact.
	Reason string `json:"reason,omitempty"`
}

// AppendDriverUtterance records a transcribed driver utterance into the
// ring buffer. Empty text is dropped. Duplicate runs of the same text
// within 1500ms are coalesced — Gemini Live emits partial transcriptions
// that grow as the model decodes more audio, so we only want the latest
// full utterance, not every intermediate fragment.
func (b *RaceBrain) AppendDriverUtterance(text string) {
	if text == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()

	// Coalesce against the most-recent utterance: if it was within 1500ms
	// AND the new text is a prefix-extension of the existing one (Live's
	// streaming transcription appends chunks), replace in place.
	if n := len(b.driverUtterances); n > 0 {
		prev := &b.driverUtterances[n-1]
		if !prev.Addressed && now.Sub(prev.At) < 1500*time.Millisecond {
			// Replace if the new text is longer and starts with the old
			// text — Gemini Live partial → full progression.
			if len(text) >= len(prev.Text) && (prev.Text == "" || startsWith(text, prev.Text)) {
				prev.Text = text
				prev.At = now
				return
			}
		}
	}

	b.driverUtterances = appendBounded(b.driverUtterances, DriverUtterance{
		At:   now,
		Text: text,
	}, maxDriverUtterances)
}

// MarkUtteranceAddressed flips Addressed=true on the most recent
// unaddressed utterance whose timestamp is within `window` of `at`. If
// `window` is zero, defaults to 30s — wide enough to catch a delayed
// analyst answer landing after the driver's question.
//
// Returns the matched utterance (zero value if none). Reason, when
// non-empty, is stored on the utterance for snapshot context.
func (b *RaceBrain) MarkUtteranceAddressed(at time.Time, reason string) DriverUtterance {
	b.mu.Lock()
	defer b.mu.Unlock()

	window := 30 * time.Second
	if at.IsZero() {
		at = b.now()
	}
	for i := len(b.driverUtterances) - 1; i >= 0; i-- {
		u := &b.driverUtterances[i]
		if u.Addressed {
			continue
		}
		if at.Sub(u.At) > window {
			continue
		}
		u.Addressed = true
		u.AddressedAt = b.now()
		u.Reason = reason
		return *u
	}
	return DriverUtterance{}
}

// OpenDriverQuestions returns utterances captured within `window` that
// have NOT yet been marked Addressed. Zero window defaults to 90s — long
// enough to catch a question that's still hanging while the agent is
// thinking, short enough to evict stale chatter the agent already
// implicitly answered.
func (b *RaceBrain) OpenDriverQuestions(window time.Duration) []DriverUtterance {
	if window <= 0 {
		window = 90 * time.Second
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	now := b.now()
	out := make([]DriverUtterance, 0, len(b.driverUtterances))
	for _, u := range b.driverUtterances {
		if u.Addressed {
			continue
		}
		if now.Sub(u.At) > window {
			continue
		}
		out = append(out, u)
	}
	return out
}

// DriverUtterances returns a copy of the entire ring (newest last). For
// tests and debug surfaces; production code should prefer
// OpenDriverQuestions.
func (b *RaceBrain) DriverUtterances() []DriverUtterance {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.driverUtterances) == 0 {
		return nil
	}
	out := make([]DriverUtterance, len(b.driverUtterances))
	copy(out, b.driverUtterances)
	return out
}

// startsWith is a tiny inline prefix check so this file doesn't need to
// import strings (keeps the brain package's import surface minimal).
func startsWith(s, prefix string) bool {
	if len(prefix) > len(s) {
		return false
	}
	return s[:len(prefix)] == prefix
}
