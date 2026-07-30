// =============================================================================
// File: internal/state/state.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-07-29
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

// Package state publishes what the editor is currently looking at, so tools outside this process
// can react to it.
//
// Why this exists: SpiceEdit has no IPC, no plugin API, and no way to be queried. That is a
// deliberate design choice and this package does not change it — nothing is listening, nothing is
// accepted, and the editor never blocks on a reader. But it means a companion panel (file history,
// blame, "problems in this file", "run the tests for this file") has no way to know which file is
// open, which blocks every cross-tool feature at once. One tiny, debounced, best-effort write to a
// well-known path unblocks all of them without giving anything a way back in.
//
// The file is a snapshot, never a log: it is rewritten in place, readers get whatever was last
// true, and a reader that arrives before the first write simply finds nothing. Every failure is
// swallowed on purpose — a full disk or a read-only home must never disturb editing.
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// debounce is how long a change must settle before it is written. Cursor movement fires on every
// keystroke and every mouse drag, so writing eagerly would mean thousands of syscalls during a
// scroll. 150ms is below human reaction time for a panel refresh yet collapses a held arrow key
// into a handful of writes.
const debounce = 150 * time.Millisecond

// Active is the published snapshot. Line and Col are 1-BASED, matching what editors, compilers and
// `file:line:col` conventions use — the in-memory Position is 0-based, and converting here rather
// than in every consumer keeps the off-by-one in exactly one place.
type Active struct {
	File string `json:"file"` // absolute path; empty when no tab is open
	Line int    `json:"line"`
	Col  int    `json:"col"`
	Root string `json:"root"` // project root the editor was opened on
	TS   int64  `json:"ts"`   // unix milliseconds, so a reader can ignore a stale snapshot
}

// Publisher coalesces updates and writes at most one file per debounce window.
type Publisher struct {
	path string

	mu      sync.Mutex
	latest  Active
	pending bool
	timer   *time.Timer
}

// Dir is the directory the snapshot lives in: $XDG_STATE_HOME/spiceedit, else ~/.local/state/
// spiceedit. This matches where the custom-action log already goes, so the editor keeps to one
// state directory rather than inventing a second convention.
func Dir() string {
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "spiceedit")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "state", "spiceedit")
}

// NewPublisher returns a publisher writing to Dir()/active.json, or nil if there is no usable
// state directory. A nil *Publisher is safe to call every method on, so callers never branch.
func NewPublisher() *Publisher {
	dir := Dir()
	if dir == "" {
		return nil
	}
	return &Publisher{path: filepath.Join(dir, "active.json")}
}

// Set records the current position. Cheap and safe to call on every redraw — that is the intended
// use, because one call site in the draw loop cannot miss a cursor move or a tab switch the way
// hand-placed hooks at each mutation site would.
//
// line and col are 0-based on the way in and converted here.
func (p *Publisher) Set(file string, line, col int, root string) {
	if p == nil {
		return
	}
	next := Active{File: file, Line: line + 1, Col: col + 1, Root: root}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Compare ignoring TS, or every call would look like a change and defeat the debounce.
	if next.File == p.latest.File && next.Line == p.latest.Line &&
		next.Col == p.latest.Col && next.Root == p.latest.Root {
		return
	}
	p.latest = next
	if p.pending {
		return // a write is already scheduled; it will pick up this newer value
	}
	p.pending = true
	p.timer = time.AfterFunc(debounce, p.flush)
}

// Flush writes any pending snapshot immediately. Called on shutdown so the last position is not
// lost inside the debounce window.
func (p *Publisher) Flush() {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.timer != nil {
		p.timer.Stop()
	}
	p.mu.Unlock()
	p.flush()
}

func (p *Publisher) flush() {
	p.mu.Lock()
	snap := p.latest
	p.pending = false
	p.mu.Unlock()

	snap.TS = time.Now().UnixMilli()
	blob, err := json.Marshal(snap)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p.path), 0o755); err != nil {
		return
	}
	// Write via a temp file and rename so a reader never observes a half-written file. Rename is
	// atomic within a directory, which is why the temp file is a sibling and not in /tmp.
	tmp, err := os.CreateTemp(filepath.Dir(p.path), ".active-*.json")
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
	if err := os.Rename(name, p.path); err != nil {
		os.Remove(name)
	}
}
