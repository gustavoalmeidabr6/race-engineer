package plan

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fixedClock returns a static time.Now stub so tests can assert
// deterministic CreatedAt / UpdatedAt values.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// newTestStore spins up a Store rooted at t.TempDir with a static clock,
// and a session loader that defaults to uid=1.
func newTestStore(t *testing.T, uid uint64) *Store {
	t.Helper()
	cfg := Config{Dir: t.TempDir(), Now: fixedClock(time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC))}
	s, err := NewStore(cfg, func() uint64 { return uid })
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

// TestStore_AddAssignsIDAndPersists — adding an entry assigns a fresh ID,
// timestamps it, and writes a parseable JSON file on disk.
func TestStore_AddAssignsIDAndPersists(t *testing.T) {
	s := newTestStore(t, 42)
	stored, err := s.Add(PlanEntry{
		Kind:    KindPitStop,
		Trigger: Trigger{Type: TriggerLap, Lap: 14},
		Notes:   "hard compound",
	}, "driver")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if stored.ID == "" || stored.ID[:3] != "pe_" {
		t.Errorf("expected pe_ prefix id, got %q", stored.ID)
	}
	if stored.Status != StatusPending {
		t.Errorf("expected default status pending, got %q", stored.Status)
	}
	if stored.CreatedBy != "driver" {
		t.Errorf("expected created_by=driver, got %q", stored.CreatedBy)
	}
	if stored.CreatedAt.IsZero() {
		t.Errorf("CreatedAt was not stamped")
	}
	// On-disk round-trip.
	path := filepath.Join(s.cfg.Dir, "sess_2a.plan.json")
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persisted file: %v", err)
	}
	var p Plan
	if err := json.Unmarshal(buf, &p); err != nil {
		t.Fatalf("parse persisted file: %v", err)
	}
	if len(p.Entries) != 1 || p.Entries[0].ID != stored.ID {
		t.Errorf("persisted plan mismatch: %+v", p)
	}
}

// TestStore_AddValidates — invalid triggers must be rejected before any
// disk write happens.
func TestStore_AddValidates(t *testing.T) {
	s := newTestStore(t, 1)
	cases := []struct {
		name  string
		entry PlanEntry
	}{
		{"no_kind", PlanEntry{Trigger: Trigger{Type: TriggerLap, Lap: 1}}},
		{"no_trigger", PlanEntry{Kind: KindPitStop}},
		{"lap_zero", PlanEntry{Kind: KindPitStop, Trigger: Trigger{Type: TriggerLap, Lap: 0}}},
		{"window_reversed", PlanEntry{Kind: KindPitStop, Trigger: Trigger{Type: TriggerLapWindow, FromLap: 10, ToLap: 5}}},
		{"track_pos_no_lap", PlanEntry{Kind: KindPitStop, Trigger: Trigger{Type: TriggerTrackPosition, Lap: 0, DistanceM: 100}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := s.Add(c.entry, "test"); err == nil {
				t.Errorf("expected validation failure, got nil")
			}
		})
	}
}

// TestStore_UpdateMutatesAndPersists — Update calls the patch closure and
// re-writes the file.
func TestStore_UpdateMutatesAndPersists(t *testing.T) {
	s := newTestStore(t, 1)
	stored, _ := s.Add(PlanEntry{Kind: KindPitStop, Trigger: Trigger{Type: TriggerLap, Lap: 14}}, "test")
	updated, err := s.Update(stored.ID, func(e *PlanEntry) error {
		e.Status = StatusActive
		e.Notes = "in progress"
		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Status != StatusActive {
		t.Errorf("expected status=active, got %q", updated.Status)
	}
	if updated.Notes != "in progress" {
		t.Errorf("expected notes=in progress, got %q", updated.Notes)
	}
}

// TestStore_UpdateValidatesPostPatch — a patch that produces an invalid
// entry must roll back instead of persisting a corrupt file.
func TestStore_UpdateValidatesPostPatch(t *testing.T) {
	s := newTestStore(t, 1)
	stored, _ := s.Add(PlanEntry{Kind: KindPitStop, Trigger: Trigger{Type: TriggerLap, Lap: 14}}, "test")
	_, err := s.Update(stored.ID, func(e *PlanEntry) error {
		e.Trigger = Trigger{Type: TriggerLap, Lap: 0} // invalid
		return nil
	})
	if err == nil {
		t.Fatalf("expected validation failure on bad patch")
	}
}

// TestStore_UpdateUnknown — patching a missing id returns
// ErrPlanEntryNotFound.
func TestStore_UpdateUnknown(t *testing.T) {
	s := newTestStore(t, 1)
	_, err := s.Update("pe_missing", func(e *PlanEntry) error { return nil })
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
}

// TestStore_CancelKeepsHistory — cancelled entries stay in the plan with
// status=cancelled and the reason appended to notes.
func TestStore_CancelKeepsHistory(t *testing.T) {
	s := newTestStore(t, 1)
	stored, _ := s.Add(PlanEntry{Kind: KindPitStop, Trigger: Trigger{Type: TriggerLap, Lap: 14}, Notes: "original"}, "test")
	cancelled, err := s.Cancel(stored.ID, "weather change")
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if cancelled.Status != StatusCancelled {
		t.Errorf("expected status=cancelled, got %q", cancelled.Status)
	}
	if cancelled.Notes != "original; cancelled: weather change" {
		t.Errorf("notes not appended correctly: %q", cancelled.Notes)
	}
	if got := len(s.Current().Entries); got != 1 {
		t.Errorf("expected 1 entry kept, got %d", got)
	}
	if got := len(s.Current().FilterActive()); got != 0 {
		t.Errorf("expected 0 active entries after cancel, got %d", got)
	}
}

// TestStore_MarkFiredAccumulates — successive MarkFired calls add entries
// to FiredEvents without overwriting prior records.
func TestStore_MarkFiredAccumulates(t *testing.T) {
	s := newTestStore(t, 1)
	stored, _ := s.Add(PlanEntry{Kind: KindPitStop, Trigger: Trigger{Type: TriggerLapWindow, FromLap: 12, ToLap: 16}}, "test")
	now1 := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	now2 := time.Date(2026, 5, 11, 12, 5, 0, 0, time.UTC)
	if err := s.MarkFired(stored.ID, "plan_window_open", now1); err != nil {
		t.Fatalf("MarkFired open: %v", err)
	}
	if err := s.MarkFired(stored.ID, "plan_window_close", now2); err != nil {
		t.Fatalf("MarkFired close: %v", err)
	}
	current := s.Current()
	got := current.Entries[0].FiredEvents
	if got["plan_window_open"].IsZero() || got["plan_window_close"].IsZero() {
		t.Errorf("expected both events recorded, got %+v", got)
	}
}

// TestStore_SessionRotation — when the session_uid loader returns a new
// uid, the next Current() opens a fresh plan file. Old file stays
// untouched on disk.
func TestStore_SessionRotation(t *testing.T) {
	var uid uint64 = 100
	cfg := Config{Dir: t.TempDir(), Now: fixedClock(time.Now())}
	s, err := NewStore(cfg, func() uint64 { return uid })
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	_, _ = s.Add(PlanEntry{Kind: KindPitStop, Trigger: Trigger{Type: TriggerLap, Lap: 5}}, "test")
	firstPath := s.Path()
	if firstPath == "" {
		t.Fatalf("expected file path after first Add")
	}

	uid = 200
	// Trigger maybeRotate via Current()
	cur := s.Current()
	if len(cur.Entries) != 0 {
		t.Errorf("expected empty plan after session rotation, got %d entries", len(cur.Entries))
	}
	if s.Path() == firstPath {
		t.Errorf("expected new file path after rotation")
	}
	if _, err := os.Stat(firstPath); err != nil {
		t.Errorf("expected old plan file to persist, got %v", err)
	}
}

// TestStore_ReloadFromDisk — a fresh Store backed by the same Dir loads
// existing entries from the prior session.
func TestStore_ReloadFromDisk(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Dir: dir, Now: fixedClock(time.Now())}
	s, err := NewStore(cfg, func() uint64 { return 77 })
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	_, _ = s.Add(PlanEntry{Kind: KindPitStop, Trigger: Trigger{Type: TriggerLap, Lap: 14}}, "first")
	_, _ = s.Add(PlanEntry{Kind: KindReminder, Trigger: Trigger{Type: TriggerLap, Lap: 20}}, "first")

	s2, err := NewStore(cfg, func() uint64 { return 77 })
	if err != nil {
		t.Fatalf("re-open NewStore: %v", err)
	}
	cur := s2.Current()
	if len(cur.Entries) != 2 {
		t.Fatalf("expected 2 reloaded entries, got %d", len(cur.Entries))
	}
}

// TestStore_NoSessionUIDStaysInMemory — when the loader returns 0 (no F1
// session yet), additions stay in-memory and no file is written.
func TestStore_NoSessionUIDStaysInMemory(t *testing.T) {
	cfg := Config{Dir: t.TempDir(), Now: fixedClock(time.Now())}
	s, err := NewStore(cfg, func() uint64 { return 0 })
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	_, _ = s.Add(PlanEntry{Kind: KindPitStop, Trigger: Trigger{Type: TriggerLap, Lap: 5}}, "test")
	if s.Path() != "" {
		t.Errorf("expected no file path while uid=0, got %q", s.Path())
	}
	entries, _ := os.ReadDir(cfg.Dir)
	for _, e := range entries {
		if !e.IsDir() {
			t.Errorf("expected no files written while uid=0, found %s", e.Name())
		}
	}
}
