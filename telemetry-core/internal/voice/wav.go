package voice

import (
	"encoding/binary"
	"strconv"
	"strings"
)

// wrapPCMAsWAV prepends a 44-byte canonical RIFF/WAV header to a raw PCM
// buffer so browsers can play it via <audio>. Defaults match Gemini TTS
// output: 24kHz, mono, 16-bit little-endian PCM.
//
// We intentionally hand-roll this rather than pulling in a WAV library
// because the format is fixed and adding a CGO/CGO-free decoder for one
// header is overkill.
func wrapPCMAsWAV(pcm []byte, sampleRate, channels, bitsPerSample int) []byte {
	dataLen := uint32(len(pcm))
	byteRate := uint32(sampleRate * channels * bitsPerSample / 8)
	blockAlign := uint16(channels * bitsPerSample / 8)

	out := make([]byte, 44+len(pcm))
	copy(out[0:4], []byte("RIFF"))
	binary.LittleEndian.PutUint32(out[4:8], 36+dataLen) // ChunkSize
	copy(out[8:12], []byte("WAVE"))
	copy(out[12:16], []byte("fmt "))
	binary.LittleEndian.PutUint32(out[16:20], 16)        // Subchunk1Size (PCM)
	binary.LittleEndian.PutUint16(out[20:22], 1)         // AudioFormat = PCM
	binary.LittleEndian.PutUint16(out[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(out[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(out[28:32], byteRate)
	binary.LittleEndian.PutUint16(out[32:34], blockAlign)
	binary.LittleEndian.PutUint16(out[34:36], uint16(bitsPerSample))
	copy(out[36:40], []byte("data"))
	binary.LittleEndian.PutUint32(out[40:44], dataLen)
	copy(out[44:], pcm)
	return out
}

// sampleRateFromMIME extracts a "rate=N" parameter from an audio/L16-style
// MIME string. Gemini TTS typically returns something like
// "audio/L16;codec=pcm;rate=24000". Falls back to fallback when the rate
// isn't parseable.
func sampleRateFromMIME(mime string, fallback int) int {
	for _, part := range strings.Split(mime, ";") {
		part = strings.TrimSpace(part)
		const prefix = "rate="
		if strings.HasPrefix(part, prefix) {
			if n, err := strconv.Atoi(strings.TrimPrefix(part, prefix)); err == nil && n > 0 {
				return n
			}
		}
	}
	return fallback
}
