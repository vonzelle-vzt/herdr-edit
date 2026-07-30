// =============================================================================
// File: internal/editor/persist_test.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-07-29
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

// Tests for persist.go — the on-disk undo history that lets undo survive
// closing and reopening a file. XDG_STATE_HOME is redirected into
// t.TempDir() for every test so nothing here ever touches a real home
// directory, and the size/entry caps (vars, not consts, precisely so
// tests can do this) are shrunk where a test needs to exercise the
// trimming/pruning paths without generating megabytes of fixture data.

package editor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPersistUndo_RoundTrip is the core promise of the feature: make some
// edits, persist, "reopen" the file (a fresh NewTab against unchanged
// disk content), and confirm the resumed history actually undoes back to
// the pre-edit content.
func TestPersistUndo_RoundTrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("hello"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tab, err := NewTab(path)
	if err != nil {
		t.Fatalf("NewTab: %v", err)
	}
	tab.Cursor = tab.Buffer.EndPos()
	tab.Anchor = tab.Cursor
	tab.InsertString(" world")
	if err := tab.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if tab.Buffer.String() != "hello world" {
		t.Fatalf("setup: got %q", tab.Buffer.String())
	}

	reopened, err := NewTab(path)
	if err != nil {
		t.Fatalf("reopen NewTab: %v", err)
	}
	if !reopened.CanUndo() {
		t.Fatal("expected the persisted undo history to resume on reopen")
	}
	if !reopened.Undo() {
		t.Fatal("Undo should succeed")
	}
	if reopened.Buffer.String() != "hello" {
		t.Fatalf("resumed undo did not restore pre-edit content, got %q", reopened.Buffer.String())
	}
}

// TestPersistUndo_ContentChangedOnDiskStartsFresh proves the content-hash
// safety check: if the file was modified on disk (by another process,
// git, whatever) after the history was recorded, the history must be
// discarded rather than replayed onto content it no longer matches.
func TestPersistUndo_ContentChangedOnDiskStartsFresh(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("hello"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tab, err := NewTab(path)
	if err != nil {
		t.Fatalf("NewTab: %v", err)
	}
	tab.Cursor = tab.Buffer.EndPos()
	tab.Anchor = tab.Cursor
	tab.InsertString(" world")
	if err := tab.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Simulate an external edit landing after the history was persisted.
	if err := os.WriteFile(path, []byte("something else entirely"), 0644); err != nil {
		t.Fatalf("external edit: %v", err)
	}

	reopened, err := NewTab(path)
	if err != nil {
		t.Fatalf("reopen NewTab: %v", err)
	}
	if reopened.CanUndo() {
		t.Fatal("a stale history (content hash mismatch) must not be resumed")
	}
}

// TestPersistUndo_CorruptHistoryFileDegradesGracefully proves a corrupt
// (or foreign-format) history file never panics NewTab and never blocks
// opening the file — it just degrades to fresh in-memory undo, exactly as
// if no history existed.
func TestPersistUndo_CorruptHistoryFileDegradesGracefully(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("hello"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	undoDir := filepath.Join(stateHome, "spiceedit", "undo")
	if err := os.MkdirAll(undoDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	histFile := persistUndoFile(undoDir, abs)
	if err := os.WriteFile(histFile, []byte("{not valid json"), 0600); err != nil {
		t.Fatalf("seed corrupt history: %v", err)
	}

	tab, err := NewTab(path) // must not panic
	if err != nil {
		t.Fatalf("NewTab should still succeed with a corrupt history file: %v", err)
	}
	if tab.CanUndo() {
		t.Fatal("a corrupt history file must not be applied")
	}
}

// TestPersistUndo_VersionMismatchIgnored proves a history file written by
// an incompatible format version is treated the same as a missing one,
// rather than being force-applied and risking a shape mismatch.
func TestPersistUndo_VersionMismatchIgnored(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	content := []byte("hello")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	undoDir := filepath.Join(stateHome, "spiceedit", "undo")
	if err := os.MkdirAll(undoDir, 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	rec := persistedUndo{
		Version:     persistUndoFormatVersion + 1,
		Path:        abs,
		ContentHash: contentHash(content),
		SavedAt:     time.Now(),
		Undo:        []persistedSnapshot{{Lines: []string{"stale"}}},
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(persistUndoFile(undoDir, abs), data, 0600); err != nil {
		t.Fatalf("seed version-mismatched history: %v", err)
	}

	tab, err := NewTab(path)
	if err != nil {
		t.Fatalf("NewTab: %v", err)
	}
	if tab.CanUndo() {
		t.Fatal("a version-mismatched history file must be ignored")
	}
}

// TestPersistUndo_UnwritableDirDegradesToInMemory proves that when the
// state directory can't be created (XDG_STATE_HOME points at a path that
// is itself a regular file, so MkdirAll fails), PersistUndo returns an
// error but in-memory undo keeps working normally — persistence failing
// must never take editing down with it.
func TestPersistUndo_UnwritableDirDegradesToInMemory(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0644); err != nil {
		t.Fatalf("seed blocker file: %v", err)
	}
	t.Setenv("XDG_STATE_HOME", blocker)

	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("hello"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tab, err := NewTab(path) // load side must not panic either
	if err != nil {
		t.Fatalf("NewTab: %v", err)
	}
	tab.Cursor = tab.Buffer.EndPos()
	tab.Anchor = tab.Cursor
	tab.InsertString(" world")

	if err := tab.PersistUndo(); err == nil {
		t.Fatal("expected PersistUndo to report an error when the state dir can't be created")
	}
	// In-memory undo must still be fully functional despite the
	// persistence failure.
	if !tab.CanUndo() {
		t.Fatal("in-memory undo should be unaffected by a persistence failure")
	}
	if !tab.Undo() {
		t.Fatal("Undo should still work in-memory")
	}
	if tab.Buffer.String() != "hello" {
		t.Fatalf("in-memory undo produced wrong content: %q", tab.Buffer.String())
	}
}

// TestPersistUndo_UntitledTabIsNoOp proves an untitled/scratch tab (no
// Path) can't be persisted — there's no stable key to store it under —
// and that calling PersistUndo on one is simply a no-op, not an error.
func TestPersistUndo_UntitledTabIsNoOp(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	tab := &Tab{Buffer: NewBuffer("scratch")}
	tab.initUndo()
	if err := tab.PersistUndo(); err != nil {
		t.Fatalf("expected nil error for an untitled tab, got %v", err)
	}
}

// TestPersistUndo_NeverSavedFileIsNoOp proves a brand-new file that has
// never existed on disk (no anchor to hash against) doesn't get
// persisted — there'd be nothing safe to verify it against on reopen.
func TestPersistUndo_NeverSavedFileIsNoOp(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)

	dir := t.TempDir()
	path := filepath.Join(dir, "never-existed.txt")
	tab := &Tab{Path: path, Buffer: NewBuffer("new content")}
	tab.initUndo()

	if err := tab.PersistUndo(); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	undoDir := filepath.Join(stateHome, "spiceedit", "undo")
	entries, _ := os.ReadDir(undoDir)
	if len(entries) != 0 {
		t.Fatalf("expected no history file to be written, found %d", len(entries))
	}
}

// TestContentHash_StableAndSensitive proves contentHash is deterministic
// for identical input and changes for different input — the two
// properties the whole safety mechanism depends on.
func TestContentHash_StableAndSensitive(t *testing.T) {
	a := contentHash([]byte("hello"))
	b := contentHash([]byte("hello"))
	c := contentHash([]byte("hello!"))
	if a != b {
		t.Fatal("identical content should hash identically")
	}
	if a == c {
		t.Fatal("different content should hash differently")
	}
}

// TestPersistUndoDir_UsesXDGStateHomeWhenSet proves the directory
// resolution honors XDG_STATE_HOME.
func TestPersistUndoDir_UsesXDGStateHomeWhenSet(t *testing.T) {
	custom := t.TempDir()
	t.Setenv("XDG_STATE_HOME", custom)
	dir, err := persistUndoDir()
	if err != nil {
		t.Fatalf("persistUndoDir: %v", err)
	}
	want := filepath.Join(custom, "spiceedit", "undo")
	if dir != want {
		t.Fatalf("got %q, want %q", dir, want)
	}
}

// TestPersistUndoDir_FallsBackToLocalState proves that with
// XDG_STATE_HOME unset, resolution falls back to the XDG default of
// ~/.local/state.
func TestPersistUndoDir_FallsBackToLocalState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")
	dir, err := persistUndoDir()
	if err != nil {
		t.Fatalf("persistUndoDir: %v", err)
	}
	want := filepath.Join(home, ".local", "state", "spiceedit", "undo")
	if dir != want {
		t.Fatalf("got %q, want %q", dir, want)
	}
}

// TestPersistUndoKey_DeterministicAndDistinct proves the path-hashing key
// is stable for the same path and different across distinct paths (no
// trivial collisions between, say, "/a/b" and "/a/c").
func TestPersistUndoKey_DeterministicAndDistinct(t *testing.T) {
	a := persistUndoKey("/some/path/f.txt")
	b := persistUndoKey("/some/path/f.txt")
	c := persistUndoKey("/some/path/g.txt")
	if a != b {
		t.Fatal("same path should produce the same key")
	}
	if a == c {
		t.Fatal("different paths should produce different keys")
	}
}

// TestCapForPersist_LimitsToMostRecent proves capForPersist keeps the
// tail (most recent) entries and drops the oldest ones, since the most
// recent history is what a user is overwhelmingly likely to want back.
func TestCapForPersist_LimitsToMostRecent(t *testing.T) {
	orig := persistUndoMaxEntries
	persistUndoMaxEntries = 3
	defer func() { persistUndoMaxEntries = orig }()

	var stack []snapshot
	for i := 0; i < 10; i++ {
		stack = append(stack, snapshot{Lines: []string{string(rune('a' + i))}})
	}
	got := capForPersist(stack)
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(got))
	}
	if got[0].Lines[0] != "h" || got[2].Lines[0] != "j" {
		t.Fatalf("expected the last 3 entries (h,i,j), got %v", got)
	}
}

// TestCapForPersist_ShorterThanCapIsUnchanged proves the cap is a no-op
// when the stack is already within budget.
func TestCapForPersist_ShorterThanCapIsUnchanged(t *testing.T) {
	stack := []snapshot{{Lines: []string{"a"}}, {Lines: []string{"b"}}}
	got := capForPersist(stack)
	if len(got) != 2 {
		t.Fatalf("expected 2 entries unchanged, got %d", len(got))
	}
}

// TestTrimSnapshotsToFit_DropsOldestUntilUnderBudget proves the byte-cap
// trimming keeps dropping oldest-first until the marshaled size fits (or
// nothing is left), and never panics doing it.
func TestTrimSnapshotsToFit_DropsOldestUntilUnderBudget(t *testing.T) {
	var entries []persistedSnapshot
	for i := 0; i < 20; i++ {
		entries = append(entries, persistedSnapshot{Lines: []string{"0123456789"}})
	}
	full, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	budget := len(full) / 2

	got := trimSnapshotsToFit(entries, budget)
	trimmed, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("Marshal trimmed: %v", err)
	}
	if len(trimmed) > budget {
		t.Fatalf("trimmed encoding (%d bytes) still exceeds budget (%d bytes)", len(trimmed), budget)
	}
	if len(got) == 0 {
		t.Fatal("some entries should have survived a half-budget trim")
	}
	// Oldest-first: the surviving entries should be a suffix of the
	// original slice, i.e. the last entry is unchanged.
	if got[len(got)-1].Lines[0] != entries[len(entries)-1].Lines[0] {
		t.Fatal("expected the most recent entry to survive trimming")
	}
}

// TestTrimSnapshotsToFit_ImpossibleBudgetReturnsEmpty proves a budget too
// small for even one entry degrades to an empty (not nil-panicking)
// slice rather than looping forever.
func TestTrimSnapshotsToFit_ImpossibleBudgetReturnsEmpty(t *testing.T) {
	entries := []persistedSnapshot{{Lines: []string{"some content"}}}
	got := trimSnapshotsToFit(entries, 2) // smaller than any valid JSON array
	if len(got) != 0 {
		t.Fatalf("expected an empty result, got %d entries", len(got))
	}
}

// TestPrunePersistUndoDir_DeletesOldestFilesFirst proves the directory-
// wide size cap removes the least-recently-written files first, and
// stops once it's back under budget rather than deleting everything.
func TestPrunePersistUndoDir_DeletesOldestFirst(t *testing.T) {
	orig := persistUndoMaxTotalBytes
	persistUndoMaxTotalBytes = 30
	defer func() { persistUndoMaxTotalBytes = orig }()

	dir := t.TempDir()
	payload := []byte("0123456789") // 10 bytes each, 4 files = 40 > 30 budget
	names := []string{"a.json", "b.json", "c.json", "d.json"}
	now := time.Now()
	for i, name := range names {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, payload, 0600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		// Stagger mtimes so ordering is unambiguous: a is oldest, d newest.
		mt := now.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatalf("Chtimes %s: %v", name, err)
		}
	}

	prunePersistUndoDir(dir)

	remaining, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	names2 := make(map[string]bool, len(remaining))
	for _, e := range remaining {
		names2[e.Name()] = true
	}
	// 40 bytes over a 30-byte budget: pruning stops as soon as it's back
	// under budget, so only the single oldest file (10 bytes) needs to go.
	if names2["a.json"] {
		t.Fatalf("expected the oldest file to be pruned, remaining=%v", names2)
	}
	if !names2["b.json"] || !names2["c.json"] || !names2["d.json"] {
		t.Fatalf("expected the newer files to survive pruning, remaining=%v", names2)
	}
}

// TestPrunePersistUndoDir_UnderBudgetIsNoOp proves pruning leaves
// everything alone when the directory is already within budget.
func TestPrunePersistUndoDir_UnderBudgetIsNoOp(t *testing.T) {
	orig := persistUndoMaxTotalBytes
	persistUndoMaxTotalBytes = 1 << 20
	defer func() { persistUndoMaxTotalBytes = orig }()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.json"), []byte("x"), 0600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	prunePersistUndoDir(dir)
	remaining, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf("expected the one file to survive, got %d entries", len(remaining))
	}
}

// TestPrunePersistUndoDir_MissingDirIsNoOp proves pruning a directory
// that doesn't exist (e.g. persistence never wrote anything yet) is a
// silent no-op, not a panic.
func TestPrunePersistUndoDir_MissingDirIsNoOp(t *testing.T) {
	prunePersistUndoDir(filepath.Join(t.TempDir(), "does-not-exist"))
}

// TestPersistUndo_ReplaceAllHistorySurvivesRoundTrip is an integration
// check that a ReplaceAll's single coalesced undo entry — the shape
// TASK 1 introduces — persists and resumes correctly, not just the
// simpler per-keystroke entries exercised by the other round-trip test.
func TestPersistUndo_ReplaceAllHistorySurvivesRoundTrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("foo bar foo"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tab, err := NewTab(path)
	if err != nil {
		t.Fatalf("NewTab: %v", err)
	}
	tab.SetFindQuery("foo")
	if n := tab.ReplaceAll("baz"); n != 2 {
		t.Fatalf("expected 2 replacements, got %d", n)
	}
	if err := tab.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reopened, err := NewTab(path)
	if err != nil {
		t.Fatalf("reopen NewTab: %v", err)
	}
	if !reopened.Undo() {
		t.Fatal("expected the persisted ReplaceAll entry to be undoable")
	}
	if reopened.Buffer.String() != "foo bar foo" {
		t.Fatalf("got %q, want the pre-ReplaceAll content restored", reopened.Buffer.String())
	}
}
