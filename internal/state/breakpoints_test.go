// =============================================================================
// File: internal/state/breakpoints_test.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package state

import (
	"os"
	"testing"
	"time"
)

// newBPStore builds a BreakpointStore against a temp XDG_STATE_HOME, failing
// the test outright if the environment can't support persistence at all.
func newBPStore(t *testing.T) *BreakpointStore {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := NewBreakpointStore()
	if s == nil {
		t.Fatal("NewBreakpointStore returned nil with XDG_STATE_HOME set")
	}
	return s
}

// waitForFile polls path until it exists (readable) or the deadline passes,
// so the test tracks the debounce instead of guessing a sleep long enough to
// be reliable on a loaded machine.
func waitForFile(t *testing.T, path string) []byte {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		blob, err := os.ReadFile(path)
		if err == nil {
			return blob
		}
		if time.Now().After(deadline) {
			t.Fatalf("no readable file at %s after 1s", path)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestBreakpointStore_RoundTrips writes a set of breakpoints for one root and
// reads them back via LoadBreakpoints, pinning the basic write-then-read
// contract the app relies on to seed a.breakpoints at startup.
func TestBreakpointStore_RoundTrips(t *testing.T) {
	s := newBPStore(t)
	want := []PersistedBreakpoint{
		{Path: "/repo/main.go", Line: 4, Enabled: true},
		{Path: "/repo/main.go", Line: 9, Enabled: false},
	}
	s.Set("/repo", want)
	waitForFile(t, s.path)

	got := LoadBreakpoints("/repo")
	if len(got) != 2 {
		t.Fatalf("got %d breakpoints, want 2: %+v", len(got), got)
	}
	if got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// TestBreakpointStore_DoesNotClobberOtherRoots writes breakpoints for two
// different roots (as two herdr-edit instances on two projects would) and
// checks both survive — a naive whole-file overwrite would erase whichever
// root wrote first.
func TestBreakpointStore_DoesNotClobberOtherRoots(t *testing.T) {
	s := newBPStore(t)
	s.Set("/repo-a", []PersistedBreakpoint{{Path: "/repo-a/a.go", Line: 1, Enabled: true}})
	waitForFile(t, s.path)

	s.Set("/repo-b", []PersistedBreakpoint{{Path: "/repo-b/b.go", Line: 2, Enabled: true}})
	s.Flush()

	if got := LoadBreakpoints("/repo-a"); len(got) != 1 {
		t.Fatalf("root A lost its breakpoints after root B wrote: %+v", got)
	}
	if got := LoadBreakpoints("/repo-b"); len(got) != 1 {
		t.Fatalf("root B breakpoints missing: %+v", got)
	}
}

// TestBreakpointStore_EmptySetRemovesRoot checks that Set with an empty slice
// (menuClearBreakpoints) deletes the root's entry entirely rather than
// leaving a dangling empty array behind.
func TestBreakpointStore_EmptySetRemovesRoot(t *testing.T) {
	s := newBPStore(t)
	s.Set("/repo", []PersistedBreakpoint{{Path: "/repo/a.go", Line: 1, Enabled: true}})
	waitForFile(t, s.path)

	s.Set("/repo", nil)
	s.Flush()

	if got := LoadBreakpoints("/repo"); len(got) != 0 {
		t.Fatalf("expected root cleared, got %+v", got)
	}
}

// TestBreakpointStore_NilSafe checks every method tolerates a nil receiver,
// matching Publisher's contract so callers never have to branch on whether
// persistence is available.
func TestBreakpointStore_NilSafe(t *testing.T) {
	var s *BreakpointStore
	s.Set("/repo", []PersistedBreakpoint{{Path: "/repo/a.go", Line: 1}})
	s.Flush()
}

// TestLoadBreakpoints_MissingFileReturnsNil checks the "nothing saved yet"
// path is silent rather than an error a caller might mishandle.
func TestLoadBreakpoints_MissingFileReturnsNil(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if got := LoadBreakpoints("/repo"); got != nil {
		t.Fatalf("expected nil for a missing file, got %+v", got)
	}
}
