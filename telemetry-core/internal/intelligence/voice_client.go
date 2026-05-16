package intelligence

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/audio"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/models"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/voice"
)

// AudioBroadcaster is the interface for pushing audio to WebSocket clients.
// Implemented by api.Hub.
type AudioBroadcaster interface {
	BroadcastAudio(audioBase64 string, format string)
}

// VoiceClient drains a channel of translated insights, synthesizes them
// via the in-process voice package, and broadcasts the base64-encoded
// audio to all WebSocket clients.
//
// This used to POST to a Python voice service at /synthesize and
// /synthesize-ack; Phase 3.1 moved both functions into the Go voice
// package so the runtime no longer needs Python at all. The HTTP shape
// is gone — the dashboard's audio path is identical because the WS
// payload format (base64 + media type) is unchanged.
type VoiceClient struct {
	syn       voice.Synthesizer
	ack       *voice.AckCache
	voiceChan <-chan models.DrivingInsight
	hub       AudioBroadcaster
	// arbiter, when set, serializes audio output against other producers
	// (e.g. the in-process Gemini Live agent). Optional — if nil the
	// client just speaks. Required when both Live and the TTS path are
	// wired simultaneously (today they aren't, but the gate keeps any
	// future "let's broadcast a TTS phrase while Live is talking"
	// regression from going out the door).
	arbiter *voice.OutputArbiter
	// ducker, when set, lowers external music player volume for the
	// duration of each broadcast. Optional — nil = no ducking.
	ducker *audio.Coordinator
}

// NewVoiceClient creates a voice client backed by the supplied
// Synthesizer (typically *voice.GeminiTTS) and AckCache. Either can be
// a nil-but-Available()==false provider; the client will degrade
// silently — insights still flow to the dashboard via WebSocket, just
// without audio.
func NewVoiceClient(syn voice.Synthesizer, ack *voice.AckCache, voiceChan <-chan models.DrivingInsight, hub AudioBroadcaster) *VoiceClient {
	return &VoiceClient{
		syn:       syn,
		ack:       ack,
		voiceChan: voiceChan,
		hub:       hub,
	}
}

// SetArbiter attaches the audio output arbiter. Call once at boot if
// another audio producer (Live agent) is wired in the same process.
func (vc *VoiceClient) SetArbiter(a *voice.OutputArbiter) {
	if vc == nil {
		return
	}
	vc.arbiter = a
}

// SetDucker attaches the audio-ducking coordinator so external music
// players are lowered for the duration of each broadcast. Optional.
func (vc *VoiceClient) SetDucker(c *audio.Coordinator) {
	if vc == nil {
		return
	}
	vc.ducker = c
}

// Run drains the voice channel, synthesizing each insight into audio.
// Blocks until ctx is cancelled.
func (vc *VoiceClient) Run(ctx context.Context) {
	log.Info().Msg("Voice client started (in-process Gemini TTS)")
	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("Voice client stopping")
			return
		case insight, ok := <-vc.voiceChan:
			if !ok {
				return
			}
			vc.synthesize(ctx, insight)
		}
	}
}

// SynthesizeAck fires a cached ack phrase the moment a driver query
// arrives, so the driver hears "Copy, checking the data" within ~50ms
// while the real LLM response is computed.
//
// Designed to be called from a goroutine — safe to call when the ack
// cache has no synthesizer (it returns an error and we log+drop).
func (vc *VoiceClient) SynthesizeAck(ctx context.Context) {
	if vc == nil || vc.ack == nil || vc.hub == nil {
		return
	}
	phrase, audio, mediaType, err := vc.ack.Synthesize(ctx)
	if err != nil {
		log.Debug().Err(err).Msg("ack synth failed")
		return
	}
	if len(audio) == 0 {
		return
	}
	if vc.ducker != nil {
		vc.ducker.SpeakingFor(audioDuration(audio, mediaType))
	}
	encoded := base64.StdEncoding.EncodeToString(audio)
	vc.hub.BroadcastAudio(encoded, formatFromMIME(mediaType))
	log.Debug().Str("phrase", phrase).Int("audio_bytes", len(audio)).Msg("Ack audio broadcast")
}

// synthesize converts a translated insight to audio and broadcasts it.
// Errors are logged at Warn — the dashboard still receives the text
// message through a separate WS channel, so a silent voice failure is
// recoverable.
func (vc *VoiceClient) synthesize(ctx context.Context, insight models.DrivingInsight) {
	if vc.syn == nil || vc.hub == nil {
		return
	}
	if avail, ok := vc.syn.(voice.Available); ok && !avail.Available() {
		// Synthesizer is constructed but disabled (missing API key etc.).
		// Don't spam Warn for every insight — Info once at startup is
		// enough, the dashboard handles missing audio gracefully.
		return
	}
	if strings.TrimSpace(insight.Message) == "" {
		return
	}
	audio, mediaType, err := vc.syn.Synthesize(ctx, insight.Message, insight.Priority)
	if err != nil {
		log.Warn().Err(err).Str("message", insight.Message).Msg("voice synth failed")
		return
	}
	if len(audio) == 0 {
		log.Warn().Msg("voice synth returned empty audio")
		return
	}
	// Take the speaking slot before broadcasting. With Live disabled
	// (the only mode where VoiceClient.Run runs today) this resolves
	// instantly; in a future "both" surface where Live and TTS share
	// the process, this is what prevents the user from hearing two
	// voices at once.
	if vc.arbiter != nil {
		release, aerr := vc.arbiter.Acquire(ctx, "voice_client", false, nil)
		if aerr != nil {
			log.Debug().Err(aerr).Msg("voice synth dropped: arbiter wait cancelled")
			return
		}
		defer release()
	}
	if vc.ducker != nil {
		vc.ducker.SpeakingFor(audioDuration(audio, mediaType))
	}
	encoded := base64.StdEncoding.EncodeToString(audio)
	vc.hub.BroadcastAudio(encoded, formatFromMIME(mediaType))
	log.Debug().
		Str("type", insight.Type).
		Int("priority", insight.Priority).
		Int("audio_bytes", len(audio)).
		Msg("Audio synthesized and broadcast")
}

// audioDuration estimates how long the buffer will play. We only need
// rough accuracy here — the ducker's job is to keep music low through
// the speech, not to time it to the millisecond. A small fixed tail
// covers WAV-header overhead, network/playback latency, and the
// trailing fade so music doesn't pop back too early.
//
// WAV path (the GeminiTTS default): read sampleRate/channels/bits from
// the header and compute from the data chunk size. Non-WAV (mp3, ogg)
// falls back to a fixed 3 s — we don't pull in a decoder just to time
// a clip that almost certainly came out of Gemini.
func audioDuration(buf []byte, mediaType string) time.Duration {
	const fallback = 3 * time.Second
	const tail = 400 * time.Millisecond
	mt := strings.ToLower(mediaType)
	if mt == "audio/wav" || mt == "audio/x-wav" || mt == "audio/wave" || mt == "" {
		if d, ok := wavDuration(buf); ok {
			return d + tail
		}
	}
	return fallback + tail
}

// wavDuration parses a canonical RIFF/WAVE header. Returns ok=false if
// the buffer doesn't look like a WAV we can read.
func wavDuration(buf []byte) (time.Duration, bool) {
	if len(buf) < 44 {
		return 0, false
	}
	if string(buf[0:4]) != "RIFF" || string(buf[8:12]) != "WAVE" {
		return 0, false
	}
	// Walk chunks looking for "fmt " and "data". Most WAVs from
	// wrapPCMAsWAV land at fixed offsets but a defensive walk costs
	// nothing.
	var (
		sampleRate    uint32
		channels      uint16
		bitsPerSample uint16
		dataSize      uint32
		haveFmt, haveData bool
	)
	i := 12
	for i+8 <= len(buf) {
		id := string(buf[i : i+4])
		sz := binary.LittleEndian.Uint32(buf[i+4 : i+8])
		body := i + 8
		end := body + int(sz)
		if end > len(buf) {
			end = len(buf)
		}
		switch id {
		case "fmt ":
			if end-body >= 16 {
				channels = binary.LittleEndian.Uint16(buf[body+2 : body+4])
				sampleRate = binary.LittleEndian.Uint32(buf[body+4 : body+8])
				bitsPerSample = binary.LittleEndian.Uint16(buf[body+14 : body+16])
				haveFmt = true
			}
		case "data":
			dataSize = sz
			haveData = true
		}
		// Chunks are word-aligned (1-byte pad if size is odd).
		next := body + int(sz)
		if sz%2 == 1 {
			next++
		}
		if next <= i {
			break
		}
		i = next
		if haveFmt && haveData {
			break
		}
	}
	if !haveFmt || !haveData || sampleRate == 0 || channels == 0 || bitsPerSample == 0 {
		return 0, false
	}
	bytesPerSec := uint64(sampleRate) * uint64(channels) * uint64(bitsPerSample/8)
	if bytesPerSec == 0 {
		return 0, false
	}
	secs := float64(dataSize) / float64(bytesPerSec)
	return time.Duration(secs * float64(time.Second)), true
}

// formatFromMIME maps the audio MIME the synthesizer returned to the
// short tag the WS audio frame carries ("wav" / "mp3"). Defaults to
// "wav" since that's what GeminiTTS+wrapPCMAsWAV produces.
func formatFromMIME(mt string) string {
	switch strings.ToLower(mt) {
	case "audio/mpeg", "audio/mp3":
		return "mp3"
	case "audio/ogg":
		return "ogg"
	case "audio/wav", "audio/x-wav", "audio/wave":
		return "wav"
	case "":
		return "wav"
	}
	return "wav"
}
