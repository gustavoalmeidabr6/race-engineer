package voice

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"

	"github.com/rs/zerolog/log"
)

// AckPhrases is the rotating set of "I heard you, give me a second"
// phrases that fire immediately after a driver query so the driver has
// audible confirmation before the real LLM response is ready. The set
// mirrors what voice_service.py served from /synthesize-ack so the
// product feels identical post-migration.
var AckPhrases = []string{
	"Copy, checking the data.",
	"Roger, looking into it.",
	"Understood, stand by for info.",
	"Copy that, we'll come back to you.",
	"Received, give us a moment.",
	"Got it, checking now.",
	"Heard you, one second.",
	"Copy, pulling it up now.",
	"Roger, working on it.",
	"Stand by, we're on it.",
	"Copy, we'll get back to you.",
	"Understood, let us check.",
	"On it, stand by.",
	"Roger, bear with us.",
	"Copy, running the numbers.",
}

// AckCache wraps a Synthesizer with a simple phrase → audio map so the
// 15 ack phrases each cost exactly one synthesis call across the life
// of the process. Without the cache, a chatty driver would burn N TTS
// requests per ack and drag the dashboard's "instant ack" latency back
// into multi-second territory.
//
// The cache is unbounded (15 entries × ~40 KB each = ~600 KB at most),
// safe to keep for the process lifetime.
type AckCache struct {
	syn   Synthesizer
	mu    sync.Mutex
	cache map[string]cachedAudio
	rng   func() int // injection seam for deterministic tests
}

type cachedAudio struct {
	audio     []byte
	mediaType string
}

// NewAckCache wraps the given Synthesizer. The returned cache is
// goroutine-safe and lazy — audio is generated only on first request.
func NewAckCache(syn Synthesizer) *AckCache {
	return &AckCache{
		syn:   syn,
		cache: make(map[string]cachedAudio, len(AckPhrases)),
		rng:   func() int { return rand.IntN(len(AckPhrases)) },
	}
}

// Synthesize picks a random ack phrase, synthesizes it (or returns a
// cached copy) and reports the audio + MIME type. The "priority" arg
// from the legacy /synthesize-ack endpoint isn't kept — acks are
// always urgent enough to play.
func (a *AckCache) Synthesize(ctx context.Context) (string, []byte, string, error) {
	if a == nil || a.syn == nil {
		return "", nil, "", errors.New("ack: no synthesizer configured")
	}
	phrase := AckPhrases[a.rng()]

	a.mu.Lock()
	if hit, ok := a.cache[phrase]; ok {
		a.mu.Unlock()
		log.Debug().Str("phrase", phrase).Msg("ack cache hit")
		return phrase, hit.audio, hit.mediaType, nil
	}
	a.mu.Unlock()

	// Synthesize outside the lock so a slow Gemini round-trip doesn't
	// block other ack requests waiting on different phrases.
	audio, mt, err := a.syn.Synthesize(ctx, phrase, 5)
	if err != nil {
		return phrase, nil, "", err
	}

	a.mu.Lock()
	a.cache[phrase] = cachedAudio{audio: audio, mediaType: mt}
	a.mu.Unlock()
	log.Debug().Str("phrase", phrase).Int("bytes", len(audio)).Msg("ack synthesized + cached")
	return phrase, audio, mt, nil
}

// Warmup pre-generates every ack phrase so the very first /api/voice
// call doesn't pay the cold-start tax. Called once at server boot in
// a background goroutine; non-fatal on errors.
func (a *AckCache) Warmup(ctx context.Context) {
	if a == nil || a.syn == nil {
		return
	}
	ok, fail := 0, 0
	for _, phrase := range AckPhrases {
		select {
		case <-ctx.Done():
			return
		default:
		}
		audio, mt, err := a.syn.Synthesize(ctx, phrase, 5)
		if err != nil {
			log.Warn().Err(err).Str("phrase", phrase).Msg("ack warmup failed")
			fail++
			continue
		}
		a.mu.Lock()
		a.cache[phrase] = cachedAudio{audio: audio, mediaType: mt}
		a.mu.Unlock()
		ok++
	}
	log.Info().Int("ok", ok).Int("failed", fail).Int("total", len(AckPhrases)).Msg("ack cache warmed up")
}
