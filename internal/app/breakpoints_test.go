// =============================================================================
// File: internal/app/breakpoints_test.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudmanic/spice-edit/internal/editor"
	"github.com/cloudmanic/spice-edit/internal/state"
)

// seedBreakpointApp opens a fresh file in a fresh temp project, the same
// fixture shape seedNavApp (navigate_test.go) uses for bookmarks.
func seedBreakpointApp(t *testing.T, content string) *App {
	t.Helper()
	dir := t.TempDir()
	target := filepath.Join(dir, "n.go")
	if err := os.WriteFile(target, []byte(content), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	a := newTestApp(t, dir)
	a.openFile(target)
	return a
}

// TestBreakpoint_ToggleIsIdempotent pins that toggling the same line twice
// sets then clears the breakpoint, mirroring bookmarks' toggle contract.
func TestBreakpoint_ToggleIsIdempotent(t *testing.T) {
	a := seedBreakpointApp(t, "one\ntwo\nthree\n")
	tab := a.activeTabPtr()
	tab.MoveCursorTo(posAt(1, 0), false)

	a.menuToggleBreakpoint()
	if _, ok := tab.MarkAt(1); !ok {
		t.Fatal("expected a breakpoint mark after first toggle")
	}
	a.menuToggleBreakpoint()
	if _, ok := tab.MarkAt(1); ok {
		t.Fatal("expected the mark cleared after second toggle")
	}
}

// TestBreakpoint_ToggleEnabled flips Enabled without removing the mark.
func TestBreakpoint_ToggleEnabled(t *testing.T) {
	a := seedBreakpointApp(t, "one\ntwo\n")
	tab := a.activeTabPtr()
	a.menuToggleBreakpoint()

	a.menuToggleBreakpointEnabled()
	m, ok := tab.MarkAt(0)
	if !ok || m.Enabled {
		t.Fatalf("expected a disabled breakpoint, got %+v, ok=%v", m, ok)
	}

	a.menuToggleBreakpointEnabled()
	m, ok = tab.MarkAt(0)
	if !ok || !m.Enabled {
		t.Fatalf("expected re-enabled breakpoint, got %+v, ok=%v", m, ok)
	}
}

// TestSyncBreakpoints_ReflectsOpenTabMarks is the "ONE call site" contract:
// after syncBreakpoints runs, allBreakpoints() must see marks set directly on
// an open tab, and stop seeing them once cleared — without any other call
// site having to know breakpoints exist.
func TestSyncBreakpoints_ReflectsOpenTabMarks(t *testing.T) {
	a := seedBreakpointApp(t, "one\ntwo\nthree\n")
	tab := a.activeTabPtr()
	tab.SetMark(0, editor.Mark{Kind: editor.MarkBreakpoint, Enabled: true})
	tab.SetMark(2, editor.Mark{Kind: editor.MarkBreakpoint, Enabled: false})

	a.syncBreakpoints()
	got := a.allBreakpoints()
	if len(got) != 2 {
		t.Fatalf("got %d breakpoints, want 2: %+v", len(got), got)
	}
	if got[0].Line != 0 || !got[0].Enabled || got[1].Line != 2 || got[1].Enabled {
		t.Fatalf("unexpected breakpoints: %+v", got)
	}

	tab.ClearMark(0)
	a.syncBreakpoints()
	got = a.allBreakpoints()
	if len(got) != 1 || got[0].Line != 2 {
		t.Fatalf("expected only line 2 to survive the clear, got %+v", got)
	}
}

// TestSyncBreakpoints_PreservesClosedTabEntries checks that breakpoints
// belonging to a path with no open tab (e.g. loaded from disk at startup)
// survive a sync that only touches the currently-open tab — syncBreakpoints
// must not wipe out everything it doesn't have live Marks for.
func TestSyncBreakpoints_PreservesClosedTabEntries(t *testing.T) {
	a := seedBreakpointApp(t, "one\ntwo\n")
	a.breakpoints = []Breakpoint{{Path: "/elsewhere/other.go", Line: 5, Enabled: true}}

	a.syncBreakpoints()

	got := a.allBreakpoints()
	if len(got) != 1 || got[0].Path != "/elsewhere/other.go" {
		t.Fatalf("expected the closed-file breakpoint preserved, got %+v", got)
	}
}

// TestBreakpoint_ClearRemovesFromTabAndList checks menuClearBreakpoints wipes
// both the open tab's Marks and the authoritative list.
func TestBreakpoint_ClearRemovesFromTabAndList(t *testing.T) {
	a := seedBreakpointApp(t, "one\ntwo\nthree\n")
	tab := a.activeTabPtr()
	tab.MoveCursorTo(posAt(0, 0), false)
	a.menuToggleBreakpoint()
	tab.MoveCursorTo(posAt(2, 0), false)
	a.menuToggleBreakpoint()
	a.syncBreakpoints()
	if !a.hasBreakpoints() {
		t.Fatal("setup: expected breakpoints before clearing")
	}

	a.menuClearBreakpoints()

	if a.hasBreakpoints() {
		t.Fatal("expected no breakpoints after clear")
	}
	if _, ok := tab.MarkAt(0); ok {
		t.Fatal("expected tab mark at line 0 cleared")
	}
	if _, ok := tab.MarkAt(2); ok {
		t.Fatal("expected tab mark at line 2 cleared")
	}
}

// TestBreakpoint_ListPickerJumpsOnSelect drives menuListBreakpoints' picker
// end to end: opening it populates a paletteOverride, and running the
// selected command moves the cursor to that breakpoint's line.
func TestBreakpoint_ListPickerJumpsOnSelect(t *testing.T) {
	a := seedBreakpointApp(t, "one\ntwo\nthree\nfour\n")
	tab := a.activeTabPtr()
	tab.MoveCursorTo(posAt(3, 0), false)
	a.menuToggleBreakpoint()
	// In real usage Run()'s poll loop calls syncBreakpoints() after every
	// event, so by the time a SEPARATE Esc-5 keypress opens the list, the
	// toggle from the previous event has already been synced.
	a.syncBreakpoints()
	tab.MoveCursorTo(posAt(0, 0), false)

	a.menuListBreakpoints()
	if !a.paletteOpen || len(a.paletteOverride) != 1 {
		t.Fatalf("expected the picker open with 1 entry, got open=%v override=%v", a.paletteOpen, a.paletteOverride)
	}

	a.paletteSelected = 0
	a.refreshPaletteResults()
	a.runSelectedPaletteCommand()

	if got := a.activeTabPtr().Cursor.Line; got != 3 {
		t.Fatalf("expected cursor on line 3 after picking the breakpoint, got %d", got)
	}
}

// TestBreakpoint_ListEmptyFlashesInsteadOfOpening checks the empty-list path
// doesn't open a zero-row picker.
func TestBreakpoint_ListEmptyFlashesInsteadOfOpening(t *testing.T) {
	a := seedBreakpointApp(t, "one\n")
	a.menuListBreakpoints()
	if a.paletteOpen {
		t.Fatal("expected no picker to open with zero breakpoints")
	}
}

// TestExportBreakpointScript_SkipsDisabled pins the export format and the
// "disabled breakpoints don't get exported" rule: a script that re-armed
// something the user deliberately disabled would be a surprise, not a
// convenience.
func TestExportBreakpointScript_SkipsDisabled(t *testing.T) {
	bps := []Breakpoint{
		{Path: "/repo/main.go", Line: 4, Enabled: true},
		{Path: "/repo/main.go", Line: 9, Enabled: false},
	}
	got := exportBreakpointScript(bps, "dlv")
	want := "break /repo/main.go:5\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestExportBreakpointScript_EmptyIsEmpty checks an all-disabled (or empty)
// set renders nothing, which menuExportBreakpoints uses to decide whether
// there's anything to copy.
func TestExportBreakpointScript_EmptyIsEmpty(t *testing.T) {
	got := exportBreakpointScript([]Breakpoint{{Path: "/repo/a.go", Line: 0, Enabled: false}}, "dlv")
	if got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

// TestSyncBreakpoints_Persists checks the polled sync actually writes
// through to internal/state when a bpStore is attached — the app-level half
// of the persistence contract state/breakpoints_test.go covers in isolation.
func TestSyncBreakpoints_Persists(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	a := seedBreakpointApp(t, "one\ntwo\n")
	a.bpStore = state.NewBreakpointStore()

	tab := a.activeTabPtr()
	tab.SetMark(1, editor.Mark{Kind: editor.MarkBreakpoint, Enabled: true})
	a.syncBreakpoints()
	a.bpStore.Flush()

	saved := state.LoadBreakpoints(a.rootDir)
	if len(saved) != 1 || saved[0].Line != 1 {
		t.Fatalf("expected the breakpoint persisted for root %q, got %+v", a.rootDir, saved)
	}
}

// TestLoadPersistedBreakpoints_SeedsAllBreakpoints checks the startup side of
// persistence: a root with a saved breakpoint file should show up via
// allBreakpoints() even before any tab for that path is opened.
func TestLoadPersistedBreakpoints_SeedsAllBreakpoints(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store := state.NewBreakpointStore()
	store.Set("/repo", []state.PersistedBreakpoint{{Path: "/repo/main.go", Line: 3, Enabled: true}})
	store.Flush()

	got := loadPersistedBreakpoints("/repo")
	if len(got) != 1 || got[0].Path != "/repo/main.go" || got[0].Line != 3 {
		t.Fatalf("got %+v", got)
	}
}

// TestHasBreakpointableTab_RejectsSynthetic checks the guard menuToggleBreakpoint
// relies on: a synthetic tab (diff view, problems list, search results) has
// no real path to export or persist against.
func TestHasBreakpointableTab_RejectsSynthetic(t *testing.T) {
	a := seedBreakpointApp(t, "one\n")
	a.tabs = append(a.tabs, editorSyntheticTab("synthetic.txt", "hello"))
	a.activeTab = len(a.tabs) - 1

	if a.hasBreakpointableTab() {
		t.Fatal("expected a synthetic tab to be rejected")
	}
	before := len(a.breakpoints)
	a.menuToggleBreakpoint()
	if len(a.breakpoints) != before {
		t.Fatal("expected menuToggleBreakpoint to no-op on a synthetic tab")
	}
}
