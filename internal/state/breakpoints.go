// =============================================================================
// File: internal/state/breakpoints.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// breakpoints.go persists breakpoint marks across process restarts.
//
// internal/editor/marks.go tracks a Tab's breakpoints in memory and keeps
// them pinned to the right line as the buffer edits; that history dies with
// the process. This file mirrors it to Dir()/breakpoints.json so closing and
// reopening herdr-edit on the same project doesn't lose every mark you set.
//
// The file holds every project's breakpoints, keyed by root, because a
// single well-known path is the only address companion tooling and a second
// herdr-edit instance can agree on without a socket. Writing is a full
// read-merge-write of the file rather than one entry: two editors open on
// different roots must not clobber each other's breakpoints, and the write
// volume here (a handful of toggles per session) makes the extra read cheap
// enough not to matter.
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// breakpointDebounce mirrors Publisher's debounce window. Toggling a
// breakpoint is a deliberate, infrequent action (unlike cursor movement), but
// clearing a whole file's marks still fires several SetMark calls back to
// back, and there's no reason to serialize each one to disk.
const breakpointDebounce = 150 * time.Millisecond

// PersistedBreakpoint is one breakpoint as written to disk. It carries Path
// because the value under each root key is a flat list, not a map keyed by
// file — a flat list is what the export-to-dlv/pdb/gdb path wants directly,
// and there's no other reader that would benefit from a second level of
// nesting.
type PersistedBreakpoint struct {
	Path       string `json:"path"`
	Line       int    `json:"line"` // 0-based, matching the buffer — this file has no external reader to convert for.
	Enabled    bool   `json:"enabled"`
	Condition  string `json:"condition,omitempty"`
	LogMessage string `json:"log_message,omitempty"`
}

// BreakpointsFile is the path every project's breakpoints are written to.
func BreakpointsFile() string {
	return filepath.Join(Dir(), "breakpoints.json")
}

// BreakpointStore coalesces breakpoint updates for one root and writes at
// most once per debounce window.
type BreakpointStore struct {
	path string

	mu      sync.Mutex
	root    string
	latest  []PersistedBreakpoint
	pending bool
	timer   *time.Timer
}

// NewBreakpointStore returns a store writing to Dir()/breakpoints.json, or
// nil if there is no usable state directory. A nil *BreakpointStore is safe
// to call every method on, matching Publisher's contract, so callers never
// have to branch on whether persistence is available.
func NewBreakpointStore() *BreakpointStore {
	dir := Dir()
	if dir == "" {
		return nil
	}
	return &BreakpointStore{path: filepath.Join(dir, "breakpoints.json")}
}

// Set records root's current breakpoint list, replacing whatever was there
// before. bps should already be the complete set for root — this is a
// snapshot write, not an append.
func (s *BreakpointStore) Set(root string, bps []PersistedBreakpoint) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.root = root
	s.latest = bps
	if s.pending {
		return // a write is already scheduled; it will pick up this newer value
	}
	s.pending = true
	s.timer = time.AfterFunc(breakpointDebounce, s.flush)
}

// Flush writes any pending update immediately. Called on shutdown so a
// breakpoint toggled just before quitting isn't lost inside the debounce
// window.
func (s *BreakpointStore) Flush() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.timer != nil {
		s.timer.Stop()
	}
	s.mu.Unlock()
	s.flush()
}

// flush merges the pending update into the on-disk map and writes it back.
// Every failure is swallowed — persistence is a convenience, and a full disk
// or an unwritable state dir must never disturb editing.
func (s *BreakpointStore) flush() {
	s.mu.Lock()
	root, bps := s.root, s.latest
	s.pending = false
	s.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return
	}
	all := readBreakpointFile(s.path)
	if all == nil {
		all = make(map[string][]PersistedBreakpoint)
	}
	if len(bps) == 0 {
		delete(all, root)
	} else {
		all[root] = bps
	}
	blob, err := json.Marshal(all)
	if err != nil {
		return
	}
	// Temp file + rename, sibling to the target so the rename is atomic and a
	// reader never observes a half-written file — the same discipline
	// Publisher.flush uses.
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".breakpoints-*.json")
	if err != nil {
		return
	}
	name := tmp.Name()
	if _, err := tmp.Write(blob); err != nil {
		tmp.Close()
		os.Remove(name)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return
	}
	if err := os.Rename(name, s.path); err != nil {
		os.Remove(name)
	}
}

// readBreakpointFile reads and parses path, returning nil on any failure
// (missing file, corrupt JSON) — the caller treats that identically to "no
// breakpoints have ever been saved."
func readBreakpointFile(path string) map[string][]PersistedBreakpoint {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var all map[string][]PersistedBreakpoint
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil
	}
	return all
}

// LoadBreakpoints returns the persisted breakpoints for root, or nil if none
// were ever saved (or the file can't be read/parsed). Used once at startup
// to seed a freshly-opened project; ongoing changes go through
// BreakpointStore.Set instead of round-tripping through disk on every edit.
func LoadBreakpoints(root string) []PersistedBreakpoint {
	all := readBreakpointFile(BreakpointsFile())
	if all == nil {
		return nil
	}
	return all[root]
}
