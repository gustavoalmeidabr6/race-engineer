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

// DefaultSTTModel is a Gemini model that accepts inline audio (audio/wav,
// audio/webm, audio/ogg) and returns a transcription when prompted. We
// reuse the flash model used elsewhere so callers don't need an extra
// API key or quota allowance.
const DefaultSTTModel = "gemini-2.5-flash"

// transcribePrompt is the single-turn instruction we send alongside the
// audio bytes. Keeping it short reduces token spend per call without
// affecting accuracy — the model knows what to do from the audio part
// alone, the prompt just gates output format. Returning raw text (no
// JSON wrapper) keeps parsing trivial and resilient to model drift.
const transcribePrompt = `Transcribe the audio verbatim. Output only the transcription text, no commentary, no quotes, no JSON.`

// GeminiSTTConfig is the STT equivalent of GeminiTTSConfig.
type GeminiSTTConfig struct {
	APIKey string
	Model  string // optional, defaults to DefaultSTTModel
}

// GeminiSTT implements Transcriber via the genai SDK's inline audio
// support. Send audio bytes + a transcription prompt, parse the text
// response.
type GeminiSTT struct {
	client *genai.Client
	model  string
}

// NewGeminiSTT constructs an STT provider. Same Available()-driven
// degradation contract as GeminiTTS.
func NewGeminiSTT(ctx context.Context, cfg GeminiSTTConfig) (*GeminiSTT, error) {
	if cfg.Model == "" {
		cfg.Model = DefaultSTTModel
	}
	stt := &GeminiSTT{model: cfg.Model}
	if cfg.APIKey == "" {
		log.Warn().Msg("GEMINI_API_KEY not set — Gemini STT disabled")
		return stt, nil
	}
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  cfg.APIKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		log.Error().Err(err).Msg("Gemini STT client init failed")
		return stt, fmt.Errorf("gemini stt client: %w", err)
	}
	stt.client = client
	log.Info().Str("model", cfg.Model).Msg("Gemini STT ready")
	return stt, nil
}

func (s *GeminiSTT) Available() bool { return s != nil && s.client != nil }

// Transcribe accepts raw audio bytes and a hint MIME type, returns the
// transcription and a stable 0.9 confidence (Gemini doesn't expose a
// real number). The MIME hint comes from the multipart upload's
// Content-Type and is normalised before being passed to the SDK
// because Gemini rejects unknown audio subtypes.
func (s *GeminiSTT) Transcribe(ctx context.Context, audio []byte, mimeHint string) (string, float32, error) {
	if s == nil || s.client == nil {
		return "", 0, errors.New("gemini stt: not configured (missing API key?)")
	}
	if len(audio) == 0 {
		return "", 0, errors.New("gemini stt: empty audio")
	}

	mime := normaliseAudioMIME(mimeHint)

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	parts := []*genai.Part{
		{Text: transcribePrompt},
		{InlineData: &genai.Blob{Data: audio, MIMEType: mime}},
	}
	contents := []*genai.Content{{Role: genai.RoleUser, Parts: parts}}

	resp, err := s.client.Models.GenerateContent(ctx, s.model, contents, nil)
	if err != nil {
		return "", 0, fmt.Errorf("gemini stt generate: %w", err)
	}

	text := strings.TrimSpace(extractText(resp))
	if text == "" {
		return "", 0, nil
	}
	return text, 0.9, nil
}

// extractText walks the response candidates and joins every text part.
// Defensive against the SDK returning text spread across multiple parts
// even though the transcription prompt asks for one block.
func extractText(resp *genai.GenerateContentResponse) string {
	if resp == nil {
		return ""
	}
	var sb strings.Builder
	for _, cand := range resp.Candidates {
		if cand == nil || cand.Content == nil {
			continue
		}
		for _, part := range cand.Content.Parts {
			if part != nil && part.Text != "" {
				if sb.Len() > 0 {
					sb.WriteString(" ")
				}
				sb.WriteString(part.Text)
			}
		}
	}
	return sb.String()
}

// normaliseAudioMIME maps the assortment of MIME strings browsers send
// (audio/webm;codecs=opus, audio/x-wav, audio/ogg) onto the subset
// Gemini's API documents as supported. Empty / unrecognised input
// defaults to audio/webm which covers MediaRecorder's default Opus
// output.
func normaliseAudioMIME(hint string) string {
	h := strings.ToLower(strings.TrimSpace(hint))
	// strip any "; codecs=..." suffix
	if i := strings.Index(h, ";"); i >= 0 {
		h = strings.TrimSpace(h[:i])
	}
	switch h {
	case "audio/wav", "audio/x-wav", "audio/wave":
		return "audio/wav"
	case "audio/mpeg", "audio/mp3":
		return "audio/mp3"
	case "audio/aac", "audio/x-aac":
		return "audio/aac"
	case "audio/flac", "audio/x-flac":
		return "audio/flac"
	case "audio/webm":
		return "audio/webm"
	case "audio/ogg", "audio/opus":
		return "audio/ogg"
	case "":
		return "audio/webm"
	}
	if strings.HasPrefix(h, "audio/") {
		return h
	}
	return "audio/webm"
}
