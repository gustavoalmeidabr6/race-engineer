package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/config"
	"github.com/tusharbhardwaj/race-engineer/telemetry-core/internal/voice"
)

// --- fake Transcriber + Available helpers ------------------------------------

type fakeTranscriber struct {
	text       string
	confidence float32
	err        error
	gotMime    string
	gotBytes   int
	available  bool
}

func (f *fakeTranscriber) Transcribe(_ context.Context, audio []byte, mime string) (string, float32, error) {
	f.gotMime = mime
	f.gotBytes = len(audio)
	return f.text, f.confidence, f.err
}
func (f *fakeTranscriber) Available() bool { return f.available }

// --- helpers -----------------------------------------------------------------

func buildVoiceForm(t *testing.T, mime string, audio []byte) (*bytes.Buffer, string) {
	t.Helper()
	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	hdr := map[string][]string{
		"Content-Disposition": {`form-data; name="audio"; filename="clip.webm"`},
	}
	if mime != "" {
		hdr["Content-Type"] = []string{mime}
	}
	part, err := w.CreatePart(hdr)
	if err != nil {
		t.Fatalf("CreatePart: %v", err)
	}
	if _, err := part.Write(audio); err != nil {
		t.Fatalf("part.Write: %v", err)
	}
	w.Close()
	return body, w.FormDataContentType()
}

func newVoiceApp(t *testing.T, deps *Deps) *fiber.App {
	t.Helper()
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Post("/api/voice", voiceHandler(deps))
	return app
}

func doMultipart(t *testing.T, app *fiber.App, body *bytes.Buffer, ct string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/voice", body)
	req.Header.Set("Content-Type", ct)
	resp, err := app.Test(req, 5000)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// --- tests -------------------------------------------------------------------

func TestVoiceHandler_HappyPath(t *testing.T) {
	tr := &fakeTranscriber{text: "box this lap", confidence: 0.9, available: true}
	deps := &Deps{
		Cfg:         &config.Config{},
		Transcriber: tr,
	}
	app := newVoiceApp(t, deps)

	body, ct := buildVoiceForm(t, "audio/webm;codecs=opus", []byte("FAKE_AUDIO"))
	code, resp := doMultipart(t, app, body, ct)
	if code != 200 {
		t.Fatalf("status %d: %s", code, resp)
	}
	var out struct {
		Transcription string  `json:"transcription"`
		Confidence    float32 `json:"confidence"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if out.Transcription != "box this lap" {
		t.Errorf("transcription mismatch: %q", out.Transcription)
	}
	if out.Confidence != 0.9 {
		t.Errorf("confidence mismatch: %v", out.Confidence)
	}
	// The handler must hand the MIME hint through; the Transcribe impl
	// decides what to do with it.
	if !strings.Contains(tr.gotMime, "audio/webm") {
		t.Errorf("expected webm hint passed through, got %q", tr.gotMime)
	}
	if tr.gotBytes != len("FAKE_AUDIO") {
		t.Errorf("audio truncated: got %d bytes", tr.gotBytes)
	}
}

func TestVoiceHandler_MissingTranscriberReturns503(t *testing.T) {
	deps := &Deps{Cfg: &config.Config{}, Transcriber: nil}
	app := newVoiceApp(t, deps)
	body, ct := buildVoiceForm(t, "audio/webm", []byte("A"))
	code, _ := doMultipart(t, app, body, ct)
	if code != 503 {
		t.Errorf("expected 503 when transcriber missing, got %d", code)
	}
}

func TestVoiceHandler_UnavailableTranscriberReturns503(t *testing.T) {
	tr := &fakeTranscriber{available: false}
	deps := &Deps{Cfg: &config.Config{}, Transcriber: tr}
	app := newVoiceApp(t, deps)
	body, ct := buildVoiceForm(t, "audio/webm", []byte("A"))
	code, _ := doMultipart(t, app, body, ct)
	if code != 503 {
		t.Errorf("expected 503 when transcriber not available, got %d", code)
	}
}

func TestVoiceHandler_EmptyAudioReturns400(t *testing.T) {
	tr := &fakeTranscriber{available: true}
	deps := &Deps{Cfg: &config.Config{}, Transcriber: tr}
	app := newVoiceApp(t, deps)
	body, ct := buildVoiceForm(t, "audio/webm", []byte{})
	code, resp := doMultipart(t, app, body, ct)
	if code != 400 {
		t.Errorf("expected 400 on empty audio, got %d (%s)", code, resp)
	}
}

func TestVoiceHandler_TranscriberErrorReturns502(t *testing.T) {
	tr := &fakeTranscriber{available: true, err: errors.New("upstream blew up")}
	deps := &Deps{Cfg: &config.Config{}, Transcriber: tr}
	app := newVoiceApp(t, deps)
	body, ct := buildVoiceForm(t, "audio/webm", []byte("AUD"))
	code, resp := doMultipart(t, app, body, ct)
	if code != 502 {
		t.Errorf("expected 502 on transcriber error, got %d (%s)", code, resp)
	}
}

func TestVoiceHandler_EmptyTranscriptionReturnsOKWithNullInsight(t *testing.T) {
	tr := &fakeTranscriber{available: true, text: ""}
	deps := &Deps{Cfg: &config.Config{}, Transcriber: tr}
	app := newVoiceApp(t, deps)
	body, ct := buildVoiceForm(t, "audio/webm", []byte("AUD"))
	code, resp := doMultipart(t, app, body, ct)
	if code != 200 {
		t.Fatalf("expected 200, got %d (%s)", code, resp)
	}
	if !strings.Contains(string(resp), `"transcription":""`) {
		t.Errorf("expected empty transcription, got %s", resp)
	}
}

func TestVoiceHandler_GeminiLiveOwnedDropsAudio(t *testing.T) {
	// When Gemini Live owns the voice surface, /api/voice must be a
	// no-op rather than running a parallel transcription pipeline.
	tr := &fakeTranscriber{available: true, text: "should-not-be-seen"}
	deps := &Deps{
		Cfg:         &config.Config{GeminiLiveEnabled: true},
		Transcriber: tr,
	}
	app := newVoiceApp(t, deps)
	body, ct := buildVoiceForm(t, "audio/webm", []byte("AUD"))
	code, resp := doMultipart(t, app, body, ct)
	if code != 200 {
		t.Fatalf("expected 200 (ignored), got %d", code)
	}
	if !strings.Contains(string(resp), `"status":"ignored"`) {
		t.Errorf("expected ignored status, got %s", resp)
	}
	if tr.gotBytes != 0 {
		t.Errorf("transcriber should not have been called; got %d bytes", tr.gotBytes)
	}
}

// TestVoiceHandler_LiveOnlyReturns503 — voice_mode=live_only means STT
// isn't even initialised at boot, so /api/voice must refuse with 503 and
// surface the voice_mode in the body so callers know why.
func TestVoiceHandler_LiveOnlyReturns503(t *testing.T) {
	// Transcriber is set to confirm we DON'T fall through to it.
	tr := &fakeTranscriber{available: true, text: "should-not-be-seen"}
	deps := &Deps{
		Cfg:         &config.Config{VoiceMode: "live_only", GeminiLiveEnabled: true},
		Transcriber: tr,
	}
	app := newVoiceApp(t, deps)
	body, ct := buildVoiceForm(t, "audio/webm", []byte("AUD"))
	code, resp := doMultipart(t, app, body, ct)
	if code != 503 {
		t.Fatalf("expected 503, got %d (%s)", code, resp)
	}
	if !strings.Contains(string(resp), `"voice_mode":"live_only"`) {
		t.Errorf("expected voice_mode in body, got %s", resp)
	}
	if tr.gotBytes != 0 {
		t.Errorf("transcriber should not run in live_only mode; got %d bytes", tr.gotBytes)
	}
}

// TestVoiceHandler_PTTOnlyTranscribes — voice_mode=ptt_only routes the
// audio through the full STT + CommsGate path. The legacy
// GeminiLiveEnabled flag is false in this mode so the "ignored" branch
// must NOT fire.
func TestVoiceHandler_PTTOnlyTranscribes(t *testing.T) {
	tr := &fakeTranscriber{available: true, text: "hi engineer", confidence: 0.9}
	cfg := &config.Config{VoiceMode: "ptt_only"}
	// GeminiLiveEnabled defaults to zero-value false — matches what
	// config.Load() would set for ptt_only.
	deps := &Deps{Cfg: cfg, Transcriber: tr}
	app := newVoiceApp(t, deps)
	body, ct := buildVoiceForm(t, "audio/webm", []byte("AUD"))
	code, resp := doMultipart(t, app, body, ct)
	if code != 200 {
		t.Fatalf("expected 200, got %d (%s)", code, resp)
	}
	if tr.gotBytes == 0 {
		t.Errorf("expected transcriber to receive audio bytes")
	}
	// We don't run a CommsGate stub here — the existing happy-path test
	// covers that. The point of this test is the routing gate.
}

// TestVoiceHandler_BothModeIgnores — voice_mode=both still keeps Live as
// the speaking surface (matching legacy GEMINI_LIVE_ENABLED=true), so the
// PTT input path replies with "ignored" rather than 503.
func TestVoiceHandler_BothModeIgnores(t *testing.T) {
	tr := &fakeTranscriber{available: true}
	deps := &Deps{
		Cfg:         &config.Config{VoiceMode: "both", GeminiLiveEnabled: true},
		Transcriber: tr,
	}
	app := newVoiceApp(t, deps)
	body, ct := buildVoiceForm(t, "audio/webm", []byte("AUD"))
	code, resp := doMultipart(t, app, body, ct)
	if code != 200 {
		t.Fatalf("expected 200 (ignored), got %d", code)
	}
	if !strings.Contains(string(resp), `"status":"ignored"`) {
		t.Errorf("expected ignored status in body, got %s", resp)
	}
}

// Build-time check: the production GeminiSTT type satisfies the
// Transcriber+Available interfaces that voiceHandler relies on.
var (
	_ voice.Transcriber = (*voice.GeminiSTT)(nil)
	_ voice.Available   = (*voice.GeminiSTT)(nil)
	_ voice.Synthesizer = (*voice.GeminiTTS)(nil)
	_ voice.Available   = (*voice.GeminiTTS)(nil)
)
