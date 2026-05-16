// Package voice is the in-process replacement for the old Python
// voice_service.py.
//
// It provides two small interfaces — Synthesizer (text → audio) and
// Transcriber (audio → text) — and a Gemini-backed implementation of each
// using google.golang.org/genai. Callers (the VoiceClient, /api/voice
// handler, /api/voice/ack handler) work against the interfaces so unit
// tests can plug in fakes and so a future Phase 3 refinement can swap in
// edge-tts or Whisper without touching anything outside this package.
//
// The package is intentionally narrow: no HTTP server, no audio
// broadcasting, no transcript logging. Those concerns live in the
// intelligence/api layers, where they already do for the legacy Python
// path.
package voice

import "context"

// Synthesizer is the text-to-speech contract. Synthesize returns raw audio
// bytes plus the MIME type the HTTP layer should set on the response (or
// pass to the WebSocket audio broadcast). The implementation owns format
// choices — callers must not assume a specific encoding.
type Synthesizer interface {
	// Synthesize converts a single utterance to audio. Priority is passed
	// through from the insight metadata for implementations that want to
	// tune voice / cadence by urgency; Gemini's standard TTS ignores it.
	Synthesize(ctx context.Context, text string, priority int) (audio []byte, mediaType string, err error)
}

// Transcriber is the speech-to-text contract. mimeHint is the Content-Type
// the upload arrived with (e.g. audio/webm, audio/wav); implementations
// may use it or ignore it. The returned confidence is normalised 0..1; a
// transcriber that doesn't expose confidence should return 0.9 as a
// stable default rather than 0 so callers can keep distinguishing
// "transcribed but unsure" from "no transcript".
type Transcriber interface {
	Transcribe(ctx context.Context, audio []byte, mimeHint string) (text string, confidence float32, err error)
}

// Available is the lightweight readiness check the API surface uses to
// decide whether to enable voice features in the dashboard. Returning
// false here lets the handler short-circuit with a 503 (or quietly drop
// audio uploads) instead of trying to call into a half-initialised
// provider.
type Available interface {
	Available() bool
}
