package plan

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// SessionIDLoader resolves the current F1 session_uid. Same shape as the
// transcript hub uses so plan + transcript rotate on identical boundaries.
type SessionIDLoader func() uint64

// Config controls the on-disk layout and rotation behaviour of the store.
type Config struct {
	// Dir is the parent directory for per-session plan files. Typically
	// the same directory the transcript hub writes to so both live
	// side-by-side.
	Dir string

	// Now overrides time.Now in tests so deterministic CreatedAt /
	// UpdatedAt timestamps are possible.
	Now func() time.Time
}

// DefaultConfig is the production set. Mirrors the transcript Dir so
// `workspace/sessions/` ends up containing both the JSONL transcripts and
// the .plan.json files.
func DefaultConfig() Config {
	return Config{
		Dir: "workspace/sessions",
		Now: time.Now,
	}
}

// Sentinel errors so handlers can map to HTTP status codes.
var (
	ErrPlanEntryNotFound = errors.New("plan: entry not found")
	ErrPlanInvalid       = errors.New("plan: invalid entry")
)

// Store is the in-memory + on-disk session plan. Methods are safe for
// concurrent use. The store auto-rotates when the configured
// SessionIDLoader returns a new UID, so callers don't need to call any
// explicit "new session" method.
type Store struct {
	cfg           Config
	sessionLoader SessionIDLoader

	mu       sync.RWMutex
	plan     *Plan
	filePath string
}

// NewStore constructs the store, loads the existing plan for the current
// session_uid if a file is present, and seeds an empty in-memory plan
// otherwise. Returns an error only if the directory can't be created;
// missing plan files are silently treated as "no plan yet".
//
// sessionLoader may be nil — useful for tests that drive the store
// manually via SetSessionUID().
func NewStore(cfg Config, sessionLoader SessionIDLoader) (*Store, error) {
	if cfg.Dir == "" {
		return nil, errors.New("plan: cfg.Dir is required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("plan: mkdir %s: %w", cfg.Dir, err)
	}

	s := &Store{cfg: cfg, sessionLoader: sessionLoader}

	uid := uint64(0)
	if sessionLoader != nil {
		uid = sessionLoader()
	}
	s.openLocked(uid)
	return s, nil
}

// Current returns a deep copy of the active plan. Safe for concurrent use;
// the returned plan is independent of the in-memory state so callers can
// pass it across goroutines without locking.
func (s *Store) Current() *Plan {
	if s == nil {
		return nil
	}
	s.maybeRotate()

	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.plan == nil {
		return nil
	}
	out := *s.plan
	out.Entries = append([]PlanEntry(nil), s.plan.Entries...)
	return &out
}

// Add inserts a new entry, assigns it an ID + timestamps, sets status to
// pending (unless already provided), persists, and returns the stored
// entry.
func (s *Store) Add(e PlanEntry, actor string) (PlanEntry, error) {
	if err := e.Validate(); err != nil {
		return PlanEntry{}, fmt.Errorf("%w: %v", ErrPlanInvalid, err)
	}
	s.maybeRotate()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.plan == nil {
		s.plan = newEmptyPlan(s.currentUIDLocked(), s.cfg.Now())
	}

	now := s.cfg.Now()
	if e.ID == "" {
		e.ID = newEntryID()
	}
	if e.Status == "" {
		e.Status = StatusPending
	}
	if actor != "" {
		e.CreatedBy = actor
	}
	if e.CreatedBy == "" {
		e.CreatedBy = "unknown"
	}
	e.CreatedAt = now
	e.UpdatedAt = now

	s.plan.Entries = append(s.plan.Entries, e)
	sort.SliceStable(s.plan.Entries, func(i, j int) bool {
		return s.plan.Entries[i].CreatedAt.Before(s.plan.Entries[j].CreatedAt)
	})
	s.plan.UpdatedAt = now

	if err := s.persistLocked(); err != nil {
		log.Warn().Err(err).Str("entry_id", e.ID).Msg("plan: persist failed after add")
	}
	return e, nil
}

// Update patches an entry by id. patch is invoked with a *PlanEntry so the
// caller can mutate fields in place; the returned entry is the post-patch
// value. Returns ErrPlanEntryNotFound when id is unknown.
func (s *Store) Update(id string, patch func(*PlanEntry) error) (PlanEntry, error) {
	s.maybeRotate()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.plan == nil {
		return PlanEntry{}, ErrPlanEntryNotFound
	}
	for i := range s.plan.Entries {
		if s.plan.Entries[i].ID != id {
			continue
		}
		now := s.cfg.Now()
		if err := patch(&s.plan.Entries[i]); err != nil {
			return PlanEntry{}, err
		}
		if err := s.plan.Entries[i].Validate(); err != nil {
			return PlanEntry{}, fmt.Errorf("%w: %v", ErrPlanInvalid, err)
		}
		// Producers may not bump UpdatedAt themselves; do it for them so
		// every mutation reflects in the persisted timestamp.
		s.plan.Entries[i].UpdatedAt = now
		s.plan.UpdatedAt = now
		out := s.plan.Entries[i]
		if err := s.persistLocked(); err != nil {
			log.Warn().Err(err).Str("entry_id", id).Msg("plan: persist failed after update")
		}
		return out, nil
	}
	return PlanEntry{}, ErrPlanEntryNotFound
}

// Cancel marks an entry as cancelled with an optional reason appended to
// notes. Cancelled entries stay in the file so the history is auditable.
func (s *Store) Cancel(id, reason string) (PlanEntry, error) {
	return s.Update(id, func(e *PlanEntry) error {
		e.Status = StatusCancelled
		if reason != "" {
			if e.Notes != "" {
				e.Notes += "; "
			}
			e.Notes += "cancelled: " + reason
		}
		return nil
	})
}

// MarkFired records that an event-type has been emitted for an entry, so
// the ticker can detect "already-fired" on subsequent ticks. Called by the
// ticker after a successful EnqueueEvent.
func (s *Store) MarkFired(id, eventKind string, at time.Time) error {
	_, err := s.Update(id, func(e *PlanEntry) error {
		if e.FiredEvents == nil {
			e.FiredEvents = map[string]time.Time{}
		}
		e.FiredEvents[eventKind] = at
		return nil
	})
	return err
}

// SetStatus is a convenience for the ticker to flip pending→active and
// active→done without going through a closure.
func (s *Store) SetStatus(id string, status EntryStatus) (PlanEntry, error) {
	return s.Update(id, func(e *PlanEntry) error {
		e.Status = status
		return nil
	})
}

// SessionUID returns the UID of the currently-loaded plan file. Returns 0
// when no session has been seen yet (process-start placeholder).
func (s *Store) SessionUID() uint64 {
	s.maybeRotate()
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.plan == nil {
		return 0
	}
	return s.plan.SessionUID
}

// Path returns the on-disk path of the current plan file, mostly for
// debug + tests. Empty when no file has been opened yet.
func (s *Store) Path() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.filePath
}

// SetSessionUID forces a rotation to the given uid. Intended for tests
// (mock telemetry) where SessionIDLoader isn't appropriate. In production
// the loader is the source of truth.
func (s *Store) SetSessionUID(uid uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.plan != nil && s.plan.SessionUID == uid {
		return
	}
	s.openLocked(uid)
}

// maybeRotate consults the SessionIDLoader; if the UID has changed since
// the last open, the store rotates to the new file. No-op when the loader
// is nil or the UID hasn't changed.
func (s *Store) maybeRotate() {
	if s == nil || s.sessionLoader == nil {
		return
	}
	uid := s.sessionLoader()
	if uid == 0 {
		// No real session yet — keep whatever placeholder file is open.
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.plan != nil && s.plan.SessionUID == uid {
		return
	}
	s.openLocked(uid)
}

// openLocked loads or creates the plan file for the given uid. Caller
// must hold s.mu.
func (s *Store) openLocked(uid uint64) {
	path := s.pathForLocked(uid)
	s.filePath = path
	if uid == 0 {
		// No real session — start with an empty in-memory plan and don't
		// touch disk until we get a real uid.
		s.plan = newEmptyPlan(0, s.cfg.Now())
		return
	}
	buf, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Warn().Err(err).Str("path", path).Msg("plan: read failed; starting fresh")
		}
		s.plan = newEmptyPlan(uid, s.cfg.Now())
		return
	}
	var p Plan
	if err := json.Unmarshal(buf, &p); err != nil {
		log.Warn().Err(err).Str("path", path).Msg("plan: parse failed; starting fresh")
		s.plan = newEmptyPlan(uid, s.cfg.Now())
		return
	}
	if p.SessionUID == 0 {
		p.SessionUID = uid
	}
	s.plan = &p
	log.Info().
		Str("path", path).
		Int("entries", len(p.Entries)).
		Uint64("session_uid", uid).
		Msg("plan: loaded existing")
}

// pathForLocked computes the file path for a given uid. Caller must hold
// s.mu (this only reads cfg.Dir but living under the lock keeps the
// concurrency contract simple).
func (s *Store) pathForLocked(uid uint64) string {
	if uid == 0 {
		return ""
	}
	return filepath.Join(s.cfg.Dir, fmt.Sprintf("sess_%x.plan.json", uid))
}

// persistLocked writes the in-memory plan to disk via tmpfile + rename.
// Caller must hold s.mu. No-op when filePath is empty (no real session).
func (s *Store) persistLocked() error {
	if s.filePath == "" || s.plan == nil {
		return nil
	}
	buf, err := json.MarshalIndent(s.plan, "", "  ")
	if err != nil {
		return fmt.Errorf("plan: marshal: %w", err)
	}
	tmp, err := os.CreateTemp(s.cfg.Dir, "plan-*.tmp")
	if err != nil {
		return fmt.Errorf("plan: tmpfile: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op if rename succeeded
	if _, err := tmp.Write(buf); err != nil {
		tmp.Close()
		return fmt.Errorf("plan: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("plan: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("plan: close: %w", err)
	}
	if err := os.Rename(tmpName, s.filePath); err != nil {
		return fmt.Errorf("plan: rename: %w", err)
	}
	return nil
}

// currentUIDLocked returns the SessionUID currently associated with the
// open file. Caller must hold s.mu.
func (s *Store) currentUIDLocked() uint64 {
	if s.plan == nil {
		return 0
	}
	return s.plan.SessionUID
}

// newEmptyPlan returns a fresh Plan stamped with now. Used both at first
// boot and at every session rotation.
func newEmptyPlan(uid uint64, now time.Time) *Plan {
	return &Plan{
		SessionUID: uid,
		Entries:    nil,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
}

// newEntryID returns "pe_<8-hex>" using crypto/rand. Matches the brain
// event-id style so debug logs read consistently.
func newEntryID() string {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("pe_%08x", time.Now().UnixNano()&0xffffffff)
	}
	return "pe_" + hex.EncodeToString(buf[:])
}
