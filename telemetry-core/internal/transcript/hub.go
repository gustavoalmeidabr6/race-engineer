package transcript

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
)

// Config tunes the transcript hub. Zero values produce sensible defaults
// (see DefaultConfig) so callers only override what they care about.
type Config struct {
	// Dir is the on-disk session log directory. One file per session.
	Dir string

	// RingSize is the in-memory ring of recent events for prompt
	// injection. Must be >= PromptLines.
	RingSize int

	// PromptLines is the default count returned by Recent() — the value
	// callers inject into LLM prompts.
	PromptLines int

	// ToolLimit is the default cap for Load() / HTTP tool consumers.
	ToolLimit int

	// MaxFileLines splits a single-session file into _part2, _part3 …
	// once it grows past this count. 0 disables splitting.
	MaxFileLines int

	// Retention is the number of session files to keep. Older files are
	// removed (or archived) on each rotation.
	Retention int

	// ChannelSize bounds the producer→hub channel. Producers Send()
	// non-blocking; over-channel events are dropped with a warn log.
	ChannelSize int

	// Now overrides time.Now for tests.
	Now func() time.Time
}

// DefaultConfig is the production set. Tests override individual fields.
func DefaultConfig() Config {
	return Config{
		Dir:          "workspace/sessions",
		RingSize:     200,
		PromptLines:  25,
		ToolLimit:    200,
		MaxFileLines: 10000,
		Retention:    30,
		ChannelSize:  256,
		Now:          time.Now,
	}
}

// SessionMeta is a one-line summary of a session log file, surfaced via
// /api/transcript/sessions and the index.json file.
type SessionMeta struct {
	ID    string    `json:"id"`
	File  string    `json:"file"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end,omitempty"`
	Lines int       `json:"lines"`
}

// SessionIDLoader resolves the current F1 session_uid (uint64). 0 means
// "no session yet" — the hub falls back to a process-start ID until a
// real uid arrives, then rotates files cleanly.
type SessionIDLoader func() uint64

// workItem is what travels on the hub's input channel. Either an Event
// to write, or a barrier with an ack channel — barriers let Sync() wait
// for prior writes to drain without polling the channel length (which
// can momentarily go to zero while the worker is mid-flush).
type workItem struct {
	event Event
	ack   chan struct{}
}

// Hub is the central transcript writer. One goroutine drains the channel
// and serialises to disk + the ring. Methods are safe for concurrent use.
type Hub struct {
	cfg           Config
	sessionLoader SessionIDLoader

	mu              sync.RWMutex
	currentID       string
	currentFile     *os.File
	currentWriter   *bufio.Writer
	currentLines    int
	currentSeenUID  uint64
	currentStart    time.Time
	currentLastAt   time.Time
	procStartID     string

	ring     []Event
	ringHead int
	ringFull bool

	in chan workItem

	// Live taps for /api/debug/stream and any other subscriber. Mutated
	// under h.mu (write lock) for the slice itself; sends to s.ch are
	// non-blocking and happen under the write-locked fan-out path.
	subs         []*subscriber
	laggedWarnAt atomic.Value // time.Time, last lagged-warn timestamp
}

// New constructs a hub. sessionLoader may be nil — in that case every
// event ends up in the process-start session file.
func New(cfg Config, sessionLoader SessionIDLoader) (*Hub, error) {
	if cfg.Dir == "" {
		return nil, errors.New("transcript: Dir is required")
	}
	if cfg.RingSize <= 0 {
		cfg.RingSize = DefaultConfig().RingSize
	}
	if cfg.PromptLines <= 0 {
		cfg.PromptLines = DefaultConfig().PromptLines
	}
	if cfg.PromptLines > cfg.RingSize {
		cfg.PromptLines = cfg.RingSize
	}
	if cfg.ToolLimit <= 0 {
		cfg.ToolLimit = DefaultConfig().ToolLimit
	}
	if cfg.ChannelSize <= 0 {
		cfg.ChannelSize = DefaultConfig().ChannelSize
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("transcript: mkdir %s: %w", cfg.Dir, err)
	}

	now := cfg.Now()
	procID := fmt.Sprintf("sess_proc_%d", now.Unix())

	h := &Hub{
		cfg:           cfg,
		sessionLoader: sessionLoader,
		ring:          make([]Event, cfg.RingSize),
		in:            make(chan workItem, cfg.ChannelSize),
		procStartID:   procID,
	}
	return h, nil
}

// Run drains the input channel until ctx is cancelled. Performs file
// open on first event, rotates on session_uid change, and flushes on
// shutdown. Single goroutine — call once.
func (h *Hub) Run(ctx context.Context) {
	log.Info().Str("dir", h.cfg.Dir).Msg("transcript: hub started")
	defer func() {
		h.mu.Lock()
		h.closeCurrentLocked()
		h.mu.Unlock()
		log.Info().Msg("transcript: hub stopping")
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case item, ok := <-h.in:
			if !ok {
				return
			}
			if item.ack != nil {
				close(item.ack)
				continue
			}
			h.write(item.event)
		}
	}
}

// Sync blocks until every Append issued *before* this call has been
// written to disk. Returns context error if ctx expires first. Safe for
// concurrent use; a single goroutine workflow can use the package-level
// Sync helper. In tests this replaces fragile channel-length polling.
func (h *Hub) Sync(ctx context.Context) error {
	ack := make(chan struct{})
	select {
	case h.in <- workItem{ack: ack}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-ack:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Append queues an event for write. Non-blocking — drops with warn if
// the channel is full (a stuck disk shouldn't stall telemetry). The
// returned bool reports whether the event was queued (false = dropped).
func (h *Hub) Append(e Event) bool {
	if err := e.Validate(); err != nil {
		log.Warn().Err(err).Msg("transcript: invalid event dropped")
		return false
	}
	if e.At.IsZero() {
		e.At = h.cfg.Now()
	}
	if len(e.Text) > MaxTextChars {
		e.Text = e.Text[:MaxTextChars] + "…"
	}
	if e.Meta != nil {
		if blob, err := json.Marshal(e.Meta); err == nil && len(blob) > MaxMetaBytes {
			// Strip meta entirely rather than ship a half-truncated map.
			e.Meta = map[string]any{
				"_truncated":  true,
				"_orig_bytes": len(blob),
			}
		}
	}

	select {
	case h.in <- workItem{event: e}:
		return true
	default:
		log.Warn().Str("kind", string(e.Kind)).Msg("transcript: channel full, dropping event")
		return false
	}
}

// Recent returns up to n most-recent events from the in-memory ring,
// optionally filtered by kinds. Newest last (chronological). nil kinds
// = all kinds. Safe to call concurrently with Run.
func (h *Hub) Recent(n int, kinds []Kind) []Event {
	if n <= 0 {
		n = h.cfg.PromptLines
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	all := h.ringSnapshotLocked()
	return filterAndTail(all, kinds, n)
}

// CurrentSession returns the active session id (whatever the hub is
// currently writing to), or empty string before the first event.
func (h *Hub) CurrentSession() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.currentID
}

// Load reads transcript events from disk for the requested scope.
// scope = "current"   → the active session
// scope = "previous"  → the most recent session before the active one
// scope = "<id>"      → an explicit session id
// scope = "" / "all"  → concatenated across all sessions (newest last)
//
// limit caps the returned count. kinds filters. since trims any events
// older than now-since when non-zero.
func (h *Hub) Load(scope string, limit int, kinds []Kind, since time.Duration) ([]Event, error) {
	if limit <= 0 {
		limit = h.cfg.ToolLimit
	}
	scope = strings.TrimSpace(scope)
	switch scope {
	case "":
		scope = "current"
	case "all":
		// fall through to readAll
	}

	if scope == "all" {
		all, err := h.readAll()
		if err != nil {
			return nil, err
		}
		return filterAndTail(applySince(all, h.cfg.Now(), since), kinds, limit), nil
	}

	id, err := h.resolveScope(scope)
	if err != nil {
		return nil, err
	}
	events, err := h.readSession(id)
	if err != nil {
		return nil, err
	}
	return filterAndTail(applySince(events, h.cfg.Now(), since), kinds, limit), nil
}

// Sessions lists session metadata. Always scans disk (cheap — typically
// <30 files) so the active session is included; index.json is consulted
// only to enrich End/Lines when a file is mid-write.
func (h *Hub) Sessions() ([]SessionMeta, error) {
	files, err := os.ReadDir(h.cfg.Dir)
	if err != nil {
		return nil, fmt.Errorf("transcript: read dir: %w", err)
	}
	// Group by session id so part files merge into one entry.
	groups := make(map[string][]string)
	for _, f := range files {
		name := f.Name()
		if f.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(name, ".jsonl")
		if i := strings.Index(id, "-part"); i > 0 {
			id = id[:i]
		}
		groups[id] = append(groups[id], filepath.Join(h.cfg.Dir, name))
	}

	var out []SessionMeta
	for id, paths := range groups {
		sort.Strings(paths)
		var (
			lines int
			start time.Time
			end   time.Time
		)
		for _, p := range paths {
			evs, err := parseJSONL(p)
			if err != nil {
				log.Warn().Err(err).Str("file", p).Msg("transcript: scan failed")
				continue
			}
			lines += len(evs)
			if len(evs) > 0 {
				if start.IsZero() || evs[0].At.Before(start) {
					start = evs[0].At
				}
				if end.IsZero() || evs[len(evs)-1].At.After(end) {
					end = evs[len(evs)-1].At
				}
			}
		}
		out = append(out, SessionMeta{
			ID:    id,
			File:  filepath.Base(paths[0]),
			Start: start,
			End:   end,
			Lines: lines,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, nil
}

// ── internal ────────────────────────────────────────────────────────

func (h *Hub) write(e Event) {
	wantID := h.resolveCurrentID()
	h.mu.Lock()
	defer h.mu.Unlock()

	if wantID != h.currentID {
		h.closeCurrentLocked()
		h.openLocked(wantID)
	} else if h.currentFile == nil {
		h.openLocked(wantID)
	}

	e.SessionID = h.currentID

	if h.cfg.MaxFileLines > 0 && h.currentLines >= h.cfg.MaxFileLines {
		// Rotate to a part-N suffix. We keep the same ID logically
		// (queries return all parts as one session) by reusing the
		// session id in the file name with -partN.
		h.closeCurrentLocked()
		h.openLocked(h.currentID) // openLocked picks the next free part
	}

	if h.currentFile == nil || h.currentWriter == nil {
		// Open failed earlier; nothing to do but record into the ring.
		h.appendToRingLocked(e)
		h.fanOutLocked(e)
		return
	}

	blob, err := json.Marshal(e)
	if err != nil {
		log.Warn().Err(err).Msg("transcript: marshal failed")
		return
	}
	blob = append(blob, '\n')
	if _, err := h.currentWriter.Write(blob); err != nil {
		log.Warn().Err(err).Msg("transcript: write failed")
		return
	}
	if err := h.currentWriter.Flush(); err != nil {
		log.Warn().Err(err).Msg("transcript: flush failed")
		return
	}
	h.currentLines++
	h.currentLastAt = e.At
	h.appendToRingLocked(e)
	h.fanOutLocked(e)
}

// resolveCurrentID figures out which session id should own the next
// write. Honours the session loader; falls back to process-start.
func (h *Hub) resolveCurrentID() string {
	if h.sessionLoader != nil {
		uid := h.sessionLoader()
		if uid != 0 {
			return fmt.Sprintf("sess_%016x", uid)
		}
	}
	return h.procStartID
}

// openLocked opens (or reopens for append) the file backing id. Caller
// holds h.mu. The first available filename in the part series wins:
// id.jsonl, id-part2.jsonl, id-part3.jsonl …
func (h *Hub) openLocked(id string) {
	base := filepath.Join(h.cfg.Dir, id+".jsonl")
	target := base
	part := 2
	for h.cfg.MaxFileLines > 0 {
		// If the base file is past MaxFileLines, walk forward until we
		// find a part file with room or the next non-existent slot.
		info, err := os.Stat(target)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			log.Warn().Err(err).Str("file", target).Msg("transcript: stat failed")
			break
		}
		if info.Size() == 0 {
			break
		}
		// Cheaper than counting lines: if size is small, just append.
		// We only rotate when the in-process counter exceeds MaxFileLines.
		// External truncation is allowed.
		if h.cfg.MaxFileLines == 0 {
			break
		}
		// Count lines in this candidate file to decide.
		n, _ := countLines(target)
		if n < h.cfg.MaxFileLines {
			break
		}
		target = filepath.Join(h.cfg.Dir, fmt.Sprintf("%s-part%d.jsonl", id, part))
		part++
	}

	f, err := os.OpenFile(target, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Warn().Err(err).Str("file", target).Msg("transcript: open failed")
		return
	}
	info, _ := f.Stat()
	startedNow := info.Size() == 0

	h.currentID = id
	h.currentFile = f
	h.currentWriter = bufio.NewWriterSize(f, 4096)
	if startedNow {
		h.currentLines = 0
		h.currentStart = h.cfg.Now()
	} else {
		// Best-effort line count for the in-process counter, so part
		// rotation across restarts still triggers.
		n, _ := countLines(target)
		h.currentLines = n
		// We don't know the original start time from the file alone;
		// fall back to mtime if available.
		if info != nil {
			h.currentStart = info.ModTime()
		} else {
			h.currentStart = h.cfg.Now()
		}
	}
	h.currentLastAt = time.Time{}

	log.Info().
		Str("session", id).
		Str("file", filepath.Base(target)).
		Int("existing_lines", h.currentLines).
		Msg("transcript: session opened")

	// Sweep retention any time we open a new session id.
	h.sweepRetentionLocked()
}

// closeCurrentLocked flushes and closes the active file, updates the
// index.json, and clears state. Caller holds h.mu.
func (h *Hub) closeCurrentLocked() {
	if h.currentFile == nil {
		return
	}
	if h.currentWriter != nil {
		_ = h.currentWriter.Flush()
	}
	_ = h.currentFile.Close()

	// Update index.
	meta := SessionMeta{
		ID:    h.currentID,
		File:  filepath.Base(h.currentFile.Name()),
		Start: h.currentStart,
		End:   h.currentLastAt,
		Lines: h.currentLines,
	}
	if err := h.updateIndex(meta); err != nil {
		log.Warn().Err(err).Msg("transcript: index update failed")
	}

	h.currentFile = nil
	h.currentWriter = nil
	h.currentLines = 0
}

func (h *Hub) appendToRingLocked(e Event) {
	if len(h.ring) == 0 {
		return
	}
	h.ring[h.ringHead] = e
	h.ringHead = (h.ringHead + 1) % len(h.ring)
	if h.ringHead == 0 {
		h.ringFull = true
	}
}

// ringSnapshotLocked returns a chronological copy of the ring (newest
// last). Caller holds h.mu (read).
func (h *Hub) ringSnapshotLocked() []Event {
	if !h.ringFull {
		out := make([]Event, h.ringHead)
		copy(out, h.ring[:h.ringHead])
		return out
	}
	out := make([]Event, len(h.ring))
	copy(out, h.ring[h.ringHead:])
	copy(out[len(h.ring)-h.ringHead:], h.ring[:h.ringHead])
	return out
}

// resolveScope maps "current" / "previous" / "<id>" to a concrete id.
func (h *Hub) resolveScope(scope string) (string, error) {
	switch scope {
	case "current":
		id := h.CurrentSession()
		if id == "" {
			id = h.resolveCurrentID()
		}
		return id, nil
	case "previous":
		sessions, err := h.Sessions()
		if err != nil {
			return "", err
		}
		// Sessions are in chronological order; "previous" = second-to-last
		// when current is the latest, else the last entry.
		current := h.CurrentSession()
		for i := len(sessions) - 1; i >= 0; i-- {
			if sessions[i].ID != current {
				return sessions[i].ID, nil
			}
		}
		return "", fmt.Errorf("transcript: no previous session")
	default:
		return scope, nil
	}
}

// readSession reads every part file for the given id and returns events
// in chronological order.
func (h *Hub) readSession(id string) ([]Event, error) {
	files, err := h.partFilesFor(id)
	if err != nil {
		return nil, err
	}
	var out []Event
	for _, f := range files {
		evs, err := parseJSONL(f)
		if err != nil {
			return nil, err
		}
		out = append(out, evs...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// partFilesFor returns base + part2..N for a given session id, in order.
func (h *Hub) partFilesFor(id string) ([]string, error) {
	files, err := os.ReadDir(h.cfg.Dir)
	if err != nil {
		return nil, fmt.Errorf("transcript: read dir: %w", err)
	}
	prefix := id + ".jsonl"
	partPrefix := id + "-part"
	var matches []string
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		name := f.Name()
		if name == prefix || strings.HasPrefix(name, partPrefix) {
			matches = append(matches, filepath.Join(h.cfg.Dir, name))
		}
	}
	sort.Strings(matches)
	return matches, nil
}

// readAll reads every transcript file in the dir, ordered by mtime.
func (h *Hub) readAll() ([]Event, error) {
	files, err := os.ReadDir(h.cfg.Dir)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
			continue
		}
		paths = append(paths, filepath.Join(h.cfg.Dir, f.Name()))
	}
	sort.Strings(paths)

	var out []Event
	for _, p := range paths {
		evs, err := parseJSONL(p)
		if err != nil {
			log.Warn().Err(err).Str("file", p).Msg("transcript: parse failed")
			continue
		}
		out = append(out, evs...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

func (h *Hub) scanFile(path string) (SessionMeta, error) {
	evs, err := parseJSONL(path)
	if err != nil {
		return SessionMeta{}, err
	}
	id := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	id = strings.SplitN(id, "-part", 2)[0]
	meta := SessionMeta{
		ID:    id,
		File:  filepath.Base(path),
		Lines: len(evs),
	}
	if len(evs) > 0 {
		meta.Start = evs[0].At
		meta.End = evs[len(evs)-1].At
	}
	return meta, nil
}

// parseJSONL reads a JSONL file, tolerating a partially-written final
// line (truncated JSON is dropped with a debug log, not an error).
func parseJSONL(path string) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	br := bufio.NewReader(f)
	var out []Event
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			line = []byte(strings.TrimRight(string(line), "\n\r"))
			if len(line) == 0 {
				continue
			}
			var e Event
			if jerr := json.Unmarshal(line, &e); jerr != nil {
				if errors.Is(err, io.EOF) {
					// Final partial write — quietly skip.
					break
				}
				log.Debug().Err(jerr).Str("file", path).Msg("transcript: skipping malformed line")
				continue
			}
			out = append(out, e)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// countLines returns the line count of a file. Best-effort; returns 0
// on any error (caller falls back gracefully).
func countLines(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	br := bufio.NewReader(f)
	count := 0
	buf := make([]byte, 32*1024)
	for {
		n, err := br.Read(buf)
		for i := 0; i < n; i++ {
			if buf[i] == '\n' {
				count++
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return count, err
		}
	}
	return count, nil
}

// filterAndTail keeps only events matching kinds and returns the last n.
// nil/empty kinds means accept all.
func filterAndTail(in []Event, kinds []Kind, n int) []Event {
	if len(kinds) > 0 {
		set := make(map[Kind]struct{}, len(kinds))
		for _, k := range kinds {
			set[k] = struct{}{}
		}
		filtered := in[:0]
		for _, e := range in {
			if _, ok := set[e.Kind]; ok {
				filtered = append(filtered, e)
			}
		}
		in = filtered
	}
	if n > 0 && len(in) > n {
		in = in[len(in)-n:]
	}
	out := make([]Event, len(in))
	copy(out, in)
	return out
}

func applySince(in []Event, now time.Time, since time.Duration) []Event {
	if since <= 0 {
		return in
	}
	cutoff := now.Add(-since)
	out := in[:0]
	for _, e := range in {
		if e.At.Before(cutoff) {
			continue
		}
		out = append(out, e)
	}
	cp := make([]Event, len(out))
	copy(cp, out)
	return cp
}
