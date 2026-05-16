package transcript

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// indexFile is the on-disk JSON catalog of session metadata. Updated on
// rotation so the HTTP layer doesn't have to scan + parse every file
// every request.
const indexFile = "index.json"

// indexFormat is what's actually written to disk. Versioned so older
// readers can detect a future schema change.
type indexFormat struct {
	Version  int           `json:"version"`
	Updated  time.Time     `json:"updated"`
	Sessions []SessionMeta `json:"sessions"`
}

const indexVersion = 1

// updateIndex inserts/replaces meta in index.json, sorted by Start asc.
// Caller may hold h.mu (read or write) — index ops are file-level.
func (h *Hub) updateIndex(meta SessionMeta) error {
	idx, _ := h.readIndex()

	replaced := false
	for i := range idx {
		if idx[i].ID == meta.ID && idx[i].File == meta.File {
			idx[i] = meta
			replaced = true
			break
		}
	}
	if !replaced {
		idx = append(idx, meta)
	}
	sort.Slice(idx, func(i, j int) bool { return idx[i].Start.Before(idx[j].Start) })

	return h.writeIndex(idx)
}

func (h *Hub) readIndex() ([]SessionMeta, error) {
	path := filepath.Join(h.cfg.Dir, indexFile)
	blob, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var f indexFormat
	if err := json.Unmarshal(blob, &f); err != nil {
		return nil, err
	}
	return f.Sessions, nil
}

func (h *Hub) writeIndex(sessions []SessionMeta) error {
	f := indexFormat{
		Version:  indexVersion,
		Updated:  h.cfg.Now(),
		Sessions: sessions,
	}
	blob, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(h.cfg.Dir, indexFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// sweepRetentionLocked enforces cfg.Retention by removing the oldest
// session files (and their part files) past the cap. Caller holds h.mu.
// Best-effort: any file delete error is logged but not fatal.
func (h *Hub) sweepRetentionLocked() {
	if h.cfg.Retention <= 0 {
		return
	}
	files, err := os.ReadDir(h.cfg.Dir)
	if err != nil {
		return
	}

	// Group files by session id.
	groups := make(map[string][]string)
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		name := f.Name()
		if name == indexFile || name == ".gitkeep" {
			continue
		}
		// id-part2.jsonl → id; id.jsonl → id
		base := name
		if idx := indexOfPart(base); idx > 0 {
			base = base[:idx]
		} else {
			base = trimSuffix(base, ".jsonl")
		}
		groups[base] = append(groups[base], filepath.Join(h.cfg.Dir, name))
	}

	type idStart struct {
		id    string
		start time.Time
	}
	var entries []idStart
	for id, paths := range groups {
		// Use earliest mtime in the group as the start signal.
		var earliest time.Time
		for _, p := range paths {
			info, err := os.Stat(p)
			if err != nil {
				continue
			}
			if earliest.IsZero() || info.ModTime().Before(earliest) {
				earliest = info.ModTime()
			}
		}
		entries = append(entries, idStart{id: id, start: earliest})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].start.Before(entries[j].start) })

	if len(entries) <= h.cfg.Retention {
		return
	}
	// Drop the oldest excess. Never drop the active session.
	excess := len(entries) - h.cfg.Retention
	for i := 0; i < excess; i++ {
		id := entries[i].id
		if id == h.currentID {
			continue
		}
		for _, p := range groups[id] {
			if err := os.Remove(p); err != nil {
				continue
			}
		}
	}
	// Best-effort index refresh after the sweep.
	if metas, err := h.Sessions(); err == nil {
		_ = h.writeIndex(metas)
	}
}

// indexOfPart returns the byte index of "-part" in name, or -1.
func indexOfPart(name string) int {
	for i := 0; i+5 < len(name); i++ {
		if name[i] == '-' && name[i+1] == 'p' && name[i+2] == 'a' &&
			name[i+3] == 'r' && name[i+4] == 't' {
			return i
		}
	}
	return -1
}

func trimSuffix(s, suf string) string {
	if len(s) >= len(suf) && s[len(s)-len(suf):] == suf {
		return s[:len(s)-len(suf)]
	}
	return s
}
