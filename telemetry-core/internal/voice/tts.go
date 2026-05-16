package voice

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"google.golang.org/genai"
)

// DefaultTTSModel is the Gemini TTS-capable model we synthesize against by
// default. Per Google's docs, the TTS-preview models accept SpeechConfig
// and return AUDIO when the response modality is set accordingly.
const DefaultTTSModel = "gemini-2.5-flash-preview-tts"

// DefaultTTSVoice is a neutral male voice that reads cleanly at race-comm
// cadence. Users can override via TTS_VOICE / the Settings page once the
// dashboard surface is added.
const DefaultTTSVoice = "Kore"

// validGeminiVoices is the set of prebuilt voice names Gemini's TTS
// endpoint accepts. We sanity-check the TTS_VOICE config against this
// list so a legacy edge-tts name like "en-GB-RyanNeural" (which Gemini
// would reject) silently falls back to DefaultTTSVoice instead of
// breaking every voice reply with a 400.
var validGeminiVoices = map[string]struct{}{
	"Kore": {}, "Puck": {}, "Charon": {}, "Aoede": {}, "Fenrir": {},
	"Leda": {}, "Orus": {}, "Zephyr": {},
}

// GeminiTTSConfig collects the knobs the TTS provider needs. APIKey is
// the only required field; everything else falls back to documented
// defaults.
type GeminiTTSConfig struct {
	APIKey string
	Model  string // optional, defaults to DefaultTTSModel
	Voice  string // optional, defaults to DefaultTTSVoice
}

// GeminiTTS implements Synthesizer via the google.golang.org/genai SDK's
// AUDIO response modality. It produces 24kHz mono PCM-16 wrapped in a
// WAV header so the WebSocket audio broadcast can hand the bytes
// straight to a <audio> element in the dashboard without an extra
// decode step.
type GeminiTTS struct {
	client *genai.Client
	model  string
	voice  string
}

// NewGeminiTTS constructs a TTS provider from the given config. Returns
// a non-nil *GeminiTTS that reports Available()==false when the API key
// is missing — callers should keep working in that mode (e.g. drop voice
// output silently) rather than crashing the server at boot.
func NewGeminiTTS(ctx context.Context, cfg GeminiTTSConfig) (*GeminiTTS, error) {
	if cfg.Model == "" {
		cfg.Model = DefaultTTSModel
	}
	if cfg.Voice == "" {
		cfg.Voice = DefaultTTSVoice
	}
	if _, ok := validGeminiVoices[cfg.Voice]; !ok {
		log.Warn().
			Str("requested", cfg.Voice).
			Str("falling_back_to", DefaultTTSVoice).
			Msg("TTS_VOICE is not a known Gemini prebuilt voice (probably a legacy edge-tts name); using default")
		cfg.Voice = DefaultTTSVoice
	}
	tts := &GeminiTTS{model: cfg.Model, voice: cfg.Voice}
	if cfg.APIKey == "" {
		log.Warn().Msg("GEMINI_API_KEY not set — Gemini TTS disabled")
		return tts, nil
	}
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  cfg.APIKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		log.Error().Err(err).Msg("Gemini TTS client init failed")
		return tts, fmt.Errorf("gemini tts client: %w", err)
	}
	tts.client = client
	log.Info().Str("model", cfg.Model).Str("voice", cfg.Voice).Msg("Gemini TTS ready")
	return tts, nil
}

// Available reports whether the provider has a usable client. The
// VoiceClient and /api/voice handler call this before invoking
// Synthesize so they can short-circuit cleanly when the API key isn't
// configured.
func (t *GeminiTTS) Available() bool { return t != nil && t.client != nil }

// Synthesize implements the Synthesizer interface. Returns audio/wav
// bytes ready for broadcast.
//
// priority is accepted for interface symmetry but ignored — Gemini's
// standard TTS doesn't expose an "urgency" voice knob and we don't want
// to silently truncate or alter the spoken message based on it.
func (t *GeminiTTS) Synthesize(ctx context.Context, text string, _ int) ([]byte, string, error) {
	if t == nil || t.client == nil {
		return nil, "", errors.New("gemini tts: not configured (missing API key?)")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, "", errors.New("gemini tts: empty text")
	}

	// 30s is conservative for a one-paragraph engineer call. Anything
	// longer almost certainly means the API stalled and the dashboard's
	// audio queue would back up anyway.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	resp, err := t.client.Models.GenerateContent(ctx, t.model,
		[]*genai.Content{genai.NewContentFromText(text, genai.RoleUser)},
		&genai.GenerateContentConfig{
			ResponseModalities: []string{"AUDIO"},
			SpeechConfig: &genai.SpeechConfig{
				VoiceConfig: &genai.VoiceConfig{
					PrebuiltVoiceConfig: &genai.PrebuiltVoiceConfig{
						VoiceName: t.voice,
					},
				},
			},
		},
	)
	if err != nil {
		return nil, "", fmt.Errorf("gemini tts generate: %w", err)
	}

	// The audio payload is returned as InlineData on one of the
	// candidate parts. Walk every candidate/part pair defensively — the
	// schema technically allows multiple audio chunks even though the
	// TTS path returns one in practice.
	var (
		pcm      []byte
		mimeType string
	)
	for _, cand := range resp.Candidates {
		if cand == nil || cand.Content == nil {
			continue
		}
		for _, part := range cand.Content.Parts {
			if part == nil || part.InlineData == nil {
				continue
			}
			if !strings.HasPrefix(part.InlineData.MIMEType, "audio/") {
				continue
			}
			pcm = append(pcm, part.InlineData.Data...)
			mimeType = part.InlineData.MIMEType
		}
	}
	if len(pcm) == 0 {
		return nil, "", errors.New("gemini tts: no audio in response")
	}

	// Gemini TTS returns headerless PCM (audio/L16 or audio/pcm with a
	// rate=24000 hint). Browsers can't play raw PCM directly, so wrap
	// it in a WAV header before broadcasting. If a future model returns
	// a self-contained container (audio/wav, audio/mpeg) we pass it
	// through untouched.
	sampleRate := sampleRateFromMIME(mimeType, 24000)
	if mimeType == "audio/wav" || mimeType == "audio/x-wav" || strings.HasPrefix(mimeType, "audio/mpeg") {
		return pcm, mimeType, nil
	}
	wav := wrapPCMAsWAV(pcm, sampleRate, 1, 16)
	return wav, "audio/wav", nil
}
