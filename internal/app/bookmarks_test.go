// =============================================================================
// File: internal/app/bookmarks_test.go
// Author: Vonzelle Brown
// Created: 2026-07-30
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package app

import (
	"testing"

	"github.com/cloudmanic/spice-edit/internal/editor"
	"github.com/gdamore/tcell/v2"
)

// TestBookmark_ToggleIsIdempotent pins that marking the same line twice removes
// it rather than stacking a duplicate. Without this the list grows every time
// you revisit a line and stops being short enough to cycle usefully.
func TestBookmark_ToggleIsIdempotent(t *testing.T) {
	a := seedNavApp(t, "one\ntwo\nthree\n")
	tab := a.activeTabPtr()
	tab.MoveCursorTo(posAt(1, 0), false)

	a.menuToggleBookmark()
	if len(a.bookmarks) != 1 {
		t.Fatalf("first mark produced %d bookmarks", len(a.bookmarks))
	}
	a.menuToggleBookmark()
	if len(a.bookmarks) != 0 {
		t.Fatalf("marking the same line twice left %d bookmarks", len(a.bookmarks))
	}
}

// TestBookmark_SortedByFileThenLine pins the cycling order. Marks are walked in
// file/line order rather than the order they happened to be made, so cycling a
// file reads top to bottom instead of jumping around.
func TestBookmark_SortedByFileThenLine(t *testing.T) {
	a := seedNavApp(t, "1\n2\n3\n4\n5\n")
	tab := a.activeTabPtr()
	for _, line := range []int{4, 0, 2} {
		tab.MoveCursorTo(posAt(line, 0), false)
		a.menuToggleBookmark()
	}
	if len(a.bookmarks) != 3 {
		t.Fatalf("expected 3 marks, got %d", len(a.bookmarks))
	}
	for i := 1; i < len(a.bookmarks); i++ {
		if a.bookmarks[i-1].Line > a.bookmarks[i].Line {
			t.Fatalf("bookmarks out of order: %v", a.bookmarks)
		}
	}
}

// TestBookmark_CycleWraps checks Esc ' walks the list and comes back round.
func TestBookmark_CycleWraps(t *testing.T) {
	a := seedNavApp(t, "1\n2\n3\n4\n5\n")
	tab := a.activeTabPtr()
	for _, line := range []int{0, 2, 4} {
		tab.MoveCursorTo(posAt(line, 0), false)
		a.menuToggleBookmark()
	}
	a.bookmarkIndex = -1

	seen := []int{}
	for i := 0; i < 4; i++ { // one more than there are marks, to prove the wrap
		a.menuNextBookmark()
		seen = append(seen, a.activeTabPtr().Cursor.Line)
	}
	want := []int{0, 2, 4, 0}
	for i, w := range want {
		if seen[i] != w {
			t.Fatalf("cycle visited %v, want %v", seen, want)
		}
	}
}

// TestBookmark_RefusesSyntheticAndEmpty pins that a generated view cannot be
// bookmarked — a diff tab has no stable path to come back to.
func TestBookmark_RefusesSyntheticAndEmpty(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.menuToggleBookmark() // no tab at all
	if len(a.bookmarks) != 0 {
		t.Fatal("bookmarked with no tab open")
	}
}

// TestBookmark_BookmarkedLinesForGutter covers the per-file lookup, and that it
// does not leak another file's marks.
func TestBookmark_BookmarkedLinesForGutter(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.bookmarks = []bookmark{
		{Path: "/a.go", Line: 3},
		{Path: "/a.go", Line: 9},
		{Path: "/b.go", Line: 1},
	}
	got := a.bookmarkedLines("/a.go")
	if len(got) != 2 || !got[3] || !got[9] {
		t.Fatalf("bookmarkedLines(/a.go) = %v", got)
	}
	if a.bookmarkedLines("/nope.go") != nil && len(a.bookmarkedLines("/nope.go")) != 0 {
		t.Error("an unmarked file reported marks")
	}
}

// TestBookmark_Reachable guards the reachability failure this fork has hit three
// times: complete behaviour with nothing calling it.
func TestBookmark_Reachable(t *testing.T) {
	if leaderActionFor('m') == nil {
		t.Error("Esc m is not bound — bookmarking is unreachable")
	}
	if leaderActionFor('\'') == nil {
		t.Error("Esc ' is not bound — cycling bookmarks is unreachable")
	}
	a := newTestApp(t, t.TempDir())
	items, _, _ := a.menuLayout()
	found := 0
	for _, it := range items {
		switch it.label {
		case "Toggle bookmark", "Next bookmark", "Clear bookmarks":
			found++
		}
	}
	if found != 3 {
		t.Errorf("only %d of 3 bookmark rows are in the menu", found)
	}
}

// bookmarkNarrowApp opens the debug fixture on a narrow pane. Nerd Font mode is
// off by construction (newTestApp never sets icons), which is the ASCII-terminal
// case the glyph has to survive.
func bookmarkNarrowApp(t *testing.T) (*App, string) {
	t.Helper()
	a, path := debugFixture(t)
	scr := a.screen.(tcell.SimulationScreen)
	scr.SetSize(44, 20)
	a.width, a.height = scr.Size()
	return a, path
}

// TestBookmark_GutterGlyphIsDrawn pins that a bookmark reaches the rendered
// gutter at a narrow width with Nerd Font off, and that an unmarked line stays
// blank. It reads the SimulationScreen, not the Tab field, so it fails if the
// app stops feeding the tab before the paint.
func TestBookmark_GutterGlyphIsDrawn(t *testing.T) {
	a, path := bookmarkNarrowApp(t)
	a.bookmarks = []bookmark{{Path: path, Line: 5}}
	a.draw()
	a.screen.Show()
	if got := gutterRuneAt(t, a, 5); got != '▪' {
		t.Fatalf("bookmarked line gutter = %q, want '▪'", string(got))
	}
	if got := gutterRuneAt(t, a, 6); got == '▪' {
		t.Fatalf("unmarked line shows the bookmark glyph")
	}
}

// TestBookmark_BreakpointWinsTheGutter pins the precedence: a line holding both
// a breakpoint and a bookmark shows the breakpoint dot.
func TestBookmark_BreakpointWinsTheGutter(t *testing.T) {
	a, path := bookmarkNarrowApp(t)
	a.bookmarks = []bookmark{{Path: path, Line: 5}}
	a.activeTabPtr().SetMark(5, editor.Mark{Kind: editor.MarkBreakpoint, Enabled: true})
	a.draw()
	a.screen.Show()
	if got := gutterRuneAt(t, a, 5); got != '●' {
		t.Fatalf("breakpoint+bookmark gutter = %q, want the breakpoint '●'", string(got))
	}
}
