package voice

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// --- wrapPCMAsWAV -----------------------------------------------------------

func TestWrapPCMAsWAV_HeaderShape(t *testing.T) {
	pcm := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	out := wrapPCMAsWAV(pcm, 24000, 1, 16)

	if len(out) != 44+len(pcm) {
		t.Fatalf("expected 44+%d bytes, got %d", len(pcm), len(out))
	}
	if !bytes.Equal(out[0:4], []byte("RIFF")) {
		t.Errorf("missing RIFF magic: %q", out[0:4])
	}
	if !bytes.Equal(out[8:12], []byte("WAVE")) {
		t.Errorf("missing WAVE marker: %q", out[8:12])
	}
	if !bytes.Equal(out[12:16], []byte("fmt ")) {
		t.Errorf("missing fmt chunk: %q", out[12:16])
	}
	// AudioFormat (1 = PCM)
	if got := binary.LittleEndian.Uint16(out[20:22]); got != 1 {
		t.Errorf("AudioFormat=%d, want 1 (PCM)", got)
	}
	// SampleRate
	if got := binary.LittleEndian.Uint32(out[24:28]); got != 24000 {
		t.Errorf("SampleRate=%d, want 24000", got)
	}
	// Data subchunk size matches PCM length
	if got := binary.LittleEndian.Uint32(out[40:44]); got != uint32(len(pcm)) {
		t.Errorf("data Subchunk2Size=%d, want %d", got, len(pcm))
	}
	if !bytes.Equal(out[44:], pcm) {
		t.Errorf("PCM payload not copied verbatim")
	}
}

// --- sampleRateFromMIME -----------------------------------------------------

func TestSampleRateFromMIME(t *testing.T) {
	cases := []struct {
		mime     string
		fallback int
		want     int
	}{
		{"audio/L16;codec=pcm;rate=24000", 24000, 24000},
		{"audio/L16; rate=16000 ; codec=pcm", 24000, 16000},
		{"audio/wav", 24000, 24000},          // no rate → fallback
		{"audio/L16;rate=junk", 24000, 24000}, // unparseable rate → fallback
		{"audio/L16;rate=", 22050, 22050},     // empty rate → fallback
		{"", 22050, 22050},
	}
	for _, c := range cases {
		got := sampleRateFromMIME(c.mime, c.fallback)
		if got != c.want {
			t.Errorf("sampleRateFromMIME(%q, %d) = %d, want %d", c.mime, c.fallback, got, c.want)
		}
	}
}

// --- normaliseAudioMIME -----------------------------------------------------

func TestNormaliseAudioMIME(t *testing.T) {
	cases := []struct {
		hint string
		want string
	}{
		{"audio/webm;codecs=opus", "audio/webm"},
		{"audio/wav", "audio/wav"},
		{"audio/x-wav", "audio/wav"},
		{"AUDIO/WEBM", "audio/webm"},
		{"audio/ogg;codecs=opus", "audio/ogg"},
		{"audio/opus", "audio/ogg"},
		{"audio/mpeg", "audio/mp3"},
		{"", "audio/webm"},
		{"text/plain", "audio/webm"}, // bogus input falls back to webm
		{"audio/flac", "audio/flac"},
	}
	for _, c := range cases {
		got := normaliseAudioMIME(c.hint)
		if got != c.want {
			t.Errorf("normaliseAudioMIME(%q) = %q, want %q", c.hint, got, c.want)
		}
	}
}

// --- AckCache ----------------------------------------------------------------

type fakeSyn struct {
	calls   atomic.Int32
	failMap map[string]error
}

func (f *fakeSyn) Synthesize(_ context.Context, text string, _ int) ([]byte, string, error) {
	f.calls.Add(1)
	if err, ok := f.failMap[text]; ok {
		return nil, "", err
	}
	return []byte("AUDIO:" + text), "audio/wav", nil
}

func TestAckCache_DeduplicatesPerPhrase(t *testing.T) {
	syn := &fakeSyn{}
	ack := NewAckCache(syn)
	// Force every call to pick the same phrase (index 0) so we exercise
	// the cache directly without depending on RNG distribution.
	ack.rng = func() int { return 0 }

	for i := 0; i < 5; i++ {
		_, audio, mt, err := ack.Synthesize(context.Background())
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if mt != "audio/wav" || !bytes.Equal(audio, []byte("AUDIO:"+AckPhrases[0])) {
			t.Errorf("call %d: unexpected audio/MT: %q / %q", i, mt, audio)
		}
	}
	if got := syn.calls.Load(); got != 1 {
		t.Errorf("expected exactly 1 synth call (cached after first), got %d", got)
	}
}

func TestAckCache_DifferentPhrasesEachSynthesized(t *testing.T) {
	syn := &fakeSyn{}
	ack := NewAckCache(syn)
	// Rotate through indices 0, 1, 2, 0, 1, 2.
	seq := []int{0, 1, 2, 0, 1, 2}
	idx := 0
	ack.rng = func() int {
		i := seq[idx%len(seq)]
		idx++
		return i
	}

	for i := 0; i < len(seq); i++ {
		if _, _, _, err := ack.Synthesize(context.Background()); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	// Three distinct phrases → 3 synthesis calls total despite 6 acks.
	if got := syn.calls.Load(); got != 3 {
		t.Errorf("expected 3 synth calls, got %d", got)
	}
}

func TestAckCache_NilSynthesizer(t *testing.T) {
	ack := NewAckCache(nil)
	if _, _, _, err := ack.Synthesize(context.Background()); err == nil {
		t.Errorf("expected error when synthesizer is nil")
	}
	// Warmup should be a no-op, not a crash.
	ack.Warmup(context.Background())
}

func TestAckCache_SynthErrorIsNotCached(t *testing.T) {
	// If the API call fails for a given phrase, the next request for that
	// same phrase should try again rather than serving an empty cached
	// entry. Otherwise a transient quota error would permanently break a
	// random subset of acks for the lifetime of the process.
	phrase := AckPhrases[0]
	syn := &fakeSyn{failMap: map[string]error{phrase: errFlaky}}
	ack := NewAckCache(syn)
	ack.rng = func() int { return 0 }

	if _, _, _, err := ack.Synthesize(context.Background()); err == nil {
		t.Fatalf("expected synth error to surface")
	}
	// Remove the failure and try again.
	syn.failMap = nil
	if _, audio, _, err := ack.Synthesize(context.Background()); err != nil || len(audio) == 0 {
		t.Fatalf("retry should succeed: audio=%q err=%v", audio, err)
	}
}

var errFlaky = errors.New("flaky")

// --- Concurrency -------------------------------------------------------------

func TestAckCache_ConcurrentSafe(t *testing.T) {
	syn := &fakeSyn{}
	ack := NewAckCache(syn)
	idx := atomic.Int32{}
	ack.rng = func() int { return int(idx.Add(1)-1) % len(AckPhrases) }

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _, _ = ack.Synthesize(context.Background())
		}()
	}
	wg.Wait()
	// 50 calls cycling through 15 distinct phrases — exactly 15 synths.
	if got := syn.calls.Load(); got != int32(len(AckPhrases)) {
		t.Errorf("expected %d synth calls, got %d", len(AckPhrases), got)
	}
}
