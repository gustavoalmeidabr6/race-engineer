package transcript

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fixedClock makes Now return whatever *cur points to. Tests advance
// time by writing to *cur.
func fixedClock(cur *time.Time) func() time.Time {
	return func() time.Time { return *cur }
}

func makeHub(t *testing.T, uid *uint64, opts ...func(*Config)) (*Hub, func(), *time.Time) {
	t.Helper()
	dir := t.TempDir()
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	cfg.Dir = dir
	cfg.Now = fixedClock(&now)
	cfg.RingSize = 32
	cfg.PromptLines = 5
	cfg.ToolLimit = 50
	cfg.MaxFileLines = 5 // small so part rotation is cheap to exercise
	cfg.Retention = 3
	cfg.ChannelSize = 16
	for _, opt := range opts {
		opt(&cfg)
	}
	loader := func() uint64 {
		if uid == nil {
			return 0
		}
		return *uid
	}
	h, err := New(cfg, loader)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.Run(ctx)
		close(done)
	}()
	teardown := func() {
		cancel()
		<-done
	}
	return h, teardown, &now
}

// drainFor blocks until every Append issued so far has hit disk. Uses
// the in-band Sync barrier so we don't false-positive between writes.
func drainFor(t *testing.T, h *Hub, deadline time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	if err := h.Sync(ctx); err != nil {
		t.Fatalf("transcript: sync timed out: %v", err)
	}
}

func TestHub_AppendsAndPersistsToFile(t *testing.T) {
	uid := uint64(0xdeadbeef)
	h, cancel, _ := makeHub(t, &uid)
	defer cancel()

	for i, text := range []string{"first", "second", "third"} {
		ok := h.Append(Event{
			Kind:  KindEngineerSpeech,
			Actor: "engineer",
			Text:  text,
			Meta:  map[string]any{"i": i},
		})
		if !ok {
			t.Fatalf("append %d returned false", i)
		}
	}
	drainFor(t, h, 200*time.Millisecond)

	files, _ := os.ReadDir(h.cfg.Dir)
	var found string
	for _, f := range files {
		if !f.IsDir() && strings.HasSuffix(f.Name(), ".jsonl") {
			found = filepath.Join(h.cfg.Dir, f.Name())
			break
		}
	}
	if found == "" {
		t.Fatal("no JSONL file written")
	}
	blob, err := os.ReadFile(found)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(blob)), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines on disk, got %d: %q", len(lines), string(blob))
	}
	var first Event
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("unmarshal first line: %v", err)
	}
	if first.Text != "first" || first.Kind != KindEngineerSpeech {
		t.Errorf("first event content wrong: %+v", first)
	}
	if !strings.HasPrefix(first.SessionID, "sess_") {
		t.Errorf("session id wrong: %s", first.SessionID)
	}
}

func TestHub_RotatesOnSessionUIDChange(t *testing.T) {
	uid := uint64(0xaaaaaaaaaaaaaaaa)
	h, cancel, _ := makeHub(t, &uid)
	defer cancel()

	h.Append(Event{Kind: KindEngineerSpeech, Actor: "engineer", Text: "uid-A msg"})
	drainFor(t, h, 200*time.Millisecond)

	// Flip UID — next Append should rotate to a fresh file.
	uid = 0xbbbbbbbbbbbbbbbb
	h.Append(Event{Kind: KindEngineerSpeech, Actor: "engineer", Text: "uid-B msg"})
	drainFor(t, h, 200*time.Millisecond)

	files, _ := os.ReadDir(h.cfg.Dir)
	count := 0
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".jsonl") {
			count++
		}
	}
	if count != 2 {
		t.Errorf("expected 2 session files after UID flip, got %d", count)
	}

	sessions, err := h.Sessions()
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(sessions) < 2 {
		t.Fatalf("expected >=2 sessions, got %d", len(sessions))
	}
}

func TestHub_PartFilesRotateOnMaxFileLines(t *testing.T) {
	uid := uint64(0x1234)
	h, cancel, _ := makeHub(t, &uid)
	defer cancel()

	// MaxFileLines=5 (set by makeHub) so 6 events split across 2 parts.
	for i := 0; i < 6; i++ {
		h.Append(Event{Kind: KindInsightPushed, Actor: "rule_engine", Text: "x"})
	}
	drainFor(t, h, 200*time.Millisecond)

	files, _ := os.ReadDir(h.cfg.Dir)
	var jsonlCount int
	var partCount int
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".jsonl") {
			continue
		}
		jsonlCount++
		if strings.Contains(f.Name(), "-part") {
			partCount++
		}
	}
	if jsonlCount != 2 {
		t.Errorf("expected 2 JSONL files for one session, got %d", jsonlCount)
	}
	if partCount != 1 {
		t.Errorf("expected 1 part-suffixed file, got %d", partCount)
	}

	// Read of the session id concatenates parts.
	id := h.CurrentSession()
	events, err := h.Load(id, 0, nil, 0)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(events) != 6 {
		t.Errorf("expected 6 events across parts, got %d", len(events))
	}
}

func TestHub_RingEvictsOldestNewestKept(t *testing.T) {
	uid := uint64(7)
	h, cancel, _ := makeHub(t, &uid, func(c *Config) {
		c.RingSize = 4
		c.PromptLines = 4
	})
	defer cancel()

	for i := 0; i < 10; i++ {
		h.Append(Event{Kind: KindEngineerSpeech, Actor: "engineer", Text: text(i)})
	}
	drainFor(t, h, 200*time.Millisecond)

	recent := h.Recent(0, nil)
	if len(recent) != 4 {
		t.Fatalf("expected 4 recent events, got %d", len(recent))
	}
	if recent[0].Text != "msg-6" {
		t.Errorf("oldest wrong: %s", recent[0].Text)
	}
	if recent[3].Text != "msg-9" {
		t.Errorf("newest wrong: %s", recent[3].Text)
	}
}

func TestHub_RecentFiltersByKind(t *testing.T) {
	uid := uint64(7)
	h, cancel, _ := makeHub(t, &uid)
	defer cancel()

	h.Append(Event{Kind: KindEngineerSpeech, Actor: "engineer", Text: "speak1"})
	h.Append(Event{Kind: KindToolCall, Actor: "live_agent", Text: "get_race_state"})
	h.Append(Event{Kind: KindEngineerSpeech, Actor: "engineer", Text: "speak2"})
	drainFor(t, h, 200*time.Millisecond)

	got := h.Recent(0, []Kind{KindEngineerSpeech})
	if len(got) != 2 {
		t.Fatalf("expected 2 engineer_speech events, got %d", len(got))
	}
	for _, e := range got {
		if e.Kind != KindEngineerSpeech {
			t.Errorf("filter leak: %s", e.Kind)
		}
	}
}

func TestHub_LoadCurrentReturnsAll(t *testing.T) {
	uid := uint64(7)
	h, cancel, _ := makeHub(t, &uid)
	defer cancel()

	for i := 0; i < 3; i++ {
		h.Append(Event{Kind: KindAnalystAnswer, Actor: "analyst", Text: text(i)})
	}
	drainFor(t, h, 200*time.Millisecond)

	got, err := h.Load("current", 0, nil, 0)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("expected 3 events, got %d", len(got))
	}
}

func TestHub_LoadPreviousReturnsPriorSession(t *testing.T) {
	uid := uint64(0x111)
	h, cancel, _ := makeHub(t, &uid)
	defer cancel()

	h.Append(Event{Kind: KindEngineerSpeech, Actor: "engineer", Text: "old session msg"})
	drainFor(t, h, 200*time.Millisecond)

	uid = 0x222
	h.Append(Event{Kind: KindEngineerSpeech, Actor: "engineer", Text: "new session msg"})
	drainFor(t, h, 200*time.Millisecond)

	prev, err := h.Load("previous", 0, nil, 0)
	if err != nil {
		t.Fatalf("Load previous: %v", err)
	}
	if len(prev) != 1 || prev[0].Text != "old session msg" {
		t.Errorf("previous wrong: %+v", prev)
	}
}

func TestHub_RetentionDropsOldestSessions(t *testing.T) {
	uid := uint64(1)
	h, cancel, _ := makeHub(t, &uid, func(c *Config) {
		c.Retention = 2
	})
	defer cancel()

	for i := 0; i < 4; i++ {
		uid = uint64(10 + i)
		h.Append(Event{Kind: KindEngineerSpeech, Actor: "engineer", Text: text(i)})
		drainFor(t, h, 200*time.Millisecond)
	}

	// Force one more rotation so sweepRetentionLocked runs again.
	uid = 99
	h.Append(Event{Kind: KindEngineerSpeech, Actor: "engineer", Text: "trigger sweep"})
	drainFor(t, h, 200*time.Millisecond)

	files, _ := os.ReadDir(h.cfg.Dir)
	var jsonlNames []string
	for _, f := range files {
		if !f.IsDir() && strings.HasSuffix(f.Name(), ".jsonl") {
			jsonlNames = append(jsonlNames, f.Name())
		}
	}
	if len(jsonlNames) > h.cfg.Retention {
		t.Errorf("expected <= %d files after retention sweep, got %d (%v)",
			h.cfg.Retention, len(jsonlNames), jsonlNames)
	}
}

func TestHub_DropsOnFullChannel(t *testing.T) {
	// ChannelSize=1 to force a drop. Block the worker by holding the
	// hub's mutex via a deliberate slow Recent call? Simpler: never
	// start Run, so the channel fills on its own.
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.Dir = dir
	cfg.ChannelSize = 1
	uid := uint64(7)
	h, err := New(cfg, func() uint64 { return uid })
	if err != nil {
		t.Fatal(err)
	}
	// Don't call Run — channel will saturate after the first send.

	first := h.Append(Event{Kind: KindEngineerSpeech, Actor: "x", Text: "1"})
	if !first {
		t.Fatal("first append should succeed")
	}
	second := h.Append(Event{Kind: KindEngineerSpeech, Actor: "x", Text: "2"})
	if second {
		t.Error("expected drop when channel full")
	}
}

func TestEvent_ValidateRejectsUnknown(t *testing.T) {
	cases := []Event{
		{Kind: "", Actor: "a"},                 // empty kind
		{Kind: "totally_made_up", Actor: "a"},  // unknown kind
		{Kind: KindEngineerSpeech, Actor: ""},  // empty actor
		{Kind: KindEngineerSpeech, Actor: " "}, // whitespace actor
	}
	for i, c := range cases {
		if err := c.Validate(); err == nil {
			t.Errorf("case %d: expected error, got nil", i)
		}
	}
}

func TestHub_TruncatesOversizedText(t *testing.T) {
	uid := uint64(7)
	h, cancel, _ := makeHub(t, &uid)
	defer cancel()

	huge := strings.Repeat("a", MaxTextChars+500)
	h.Append(Event{Kind: KindEngineerSpeech, Actor: "engineer", Text: huge})
	drainFor(t, h, 200*time.Millisecond)

	got := h.Recent(1, nil)
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	// "…" is a 3-byte UTF-8 ellipsis appended to the cap.
	if len(got[0].Text) != MaxTextChars+len("…") {
		t.Errorf("text not truncated cleanly: len=%d (want %d)", len(got[0].Text), MaxTextChars+len("…"))
	}
	if !strings.HasSuffix(got[0].Text, "…") {
		t.Errorf("expected ellipsis suffix on truncated text")
	}
}

func TestHub_MarkdownRenderShape(t *testing.T) {
	at := time.Date(2026, 5, 10, 14, 30, 5, 0, time.UTC)
	events := []Event{
		{
			At: at, SessionID: "sess_1", Kind: KindEngineerSpeech,
			Actor: "engineer", Text: "Box, box.",
			Meta: map[string]any{"priority": 5, "skip_me": map[string]any{"k": "v"}},
		},
		{
			At: at.Add(2 * time.Second), SessionID: "sess_1", Kind: KindToolCall,
			Actor: "live_agent", Text: "ask_data_analyst",
			Meta: map[string]any{"latency_ms": 412.0, "urgent": true},
		},
	}
	out := RenderMarkdown(events)
	if !strings.Contains(out, "14:30:05 · engineer_speech · engineer · Box, box.") {
		t.Errorf("first line missing/wrong: %s", out)
	}
	if !strings.Contains(out, "(priority=5)") {
		t.Errorf("scalar meta missing: %s", out)
	}
	if strings.Contains(out, "skip_me") {
		t.Errorf("complex meta should be skipped, got: %s", out)
	}
	if !strings.Contains(out, "(latency_ms=412 urgent=true)") {
		t.Errorf("expected sorted scalar meta, got: %s", out)
	}
}

func TestHub_PartialJSONLLineDoesNotPanic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess_x.jsonl")
	good := `{"at":"2026-05-10T12:00:00Z","session_id":"sess_x","kind":"engineer_speech","actor":"engineer","text":"hi"}` + "\n"
	partial := `{"at":"2026-05-10T12:00`
	if err := os.WriteFile(path, []byte(good+partial), 0o644); err != nil {
		t.Fatal(err)
	}
	events, err := parseJSONL(path)
	if err != nil {
		t.Fatalf("parseJSONL surfaced an error on partial line: %v", err)
	}
	if len(events) != 1 || events[0].Text != "hi" {
		t.Errorf("expected 1 event from good line only, got %+v", events)
	}
}

func TestHub_ConcurrentAppendsAreSerialised(t *testing.T) {
	uid := uint64(0x555)
	h, cancel, _ := makeHub(t, &uid, func(c *Config) {
		c.ChannelSize = 512
		c.RingSize = 512
		c.MaxFileLines = 0 // disable rotation for this test
	})
	defer cancel()

	var wg sync.WaitGroup
	const writers = 8
	const perWriter = 25
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				h.Append(Event{
					Kind:  KindAnalystQuery,
					Actor: "analyst",
					Text:  text(w*1000 + i),
				})
			}
		}(w)
	}
	wg.Wait()
	drainFor(t, h, time.Second)

	got, err := h.Load("current", writers*perWriter+10, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := writers * perWriter
	if len(got) != want {
		t.Errorf("expected %d events on disk, got %d", want, len(got))
	}
}

func text(i int) string {
	return "msg-" + itoa(i)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
