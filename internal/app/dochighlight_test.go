// =============================================================================
// File: internal/app/dochighlight_test.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package app

import (
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/cloudmanic/spice-edit/internal/editor"
)

// tabIndentedFixture is shared by the geometry tests below: two occurrences
// of "foo" on separate hard-tab-indented lines, which is exactly the shape
// that broke ScreenPos historically (see geometry.go's own doc comment).
const tabIndentedFixture = "package main\n\nfunc main() {\n\tfoo := 1\n\tprint(foo)\n}\n"

// TestDocumentHighlightLandsOnTabIndentedLines is the load-bearing oracle
// for this file: it renders the fixture through a real tcell simulation
// screen and asserts the tinted cells are EXACTLY the cells Render painted
// "foo" into — found independently by reading the rendered glyphs off the
// screen, not by recomputing coordinates with the same helper the
// production code uses.
//
// 🔴 Confirmed RED first against `dx := tab.GutterWidth() + col` (the
// historical ScreenPos bug this repo shipped): with TabStop=4, a hard tab at
// the start of a line occupies 4 screen cells, so "foo" at rune column 1
// renders at visual column 4 — the naive formula puts the tint 3 columns to
// the left of the actual glyphs, landing on the gutter/whitespace instead.
// See the RED output captured in this task's report.
func TestDocumentHighlightLandsOnTabIndentedLines(t *testing.T) {
	a, _ := appWithFile(t, tabIndentedFixture)
	tab := a.activeTabPtr()
	// Cursor inside "foo" on line 3 (0-based) — the tab, then "foo := 1".
	tab.Cursor = editor.Position{Line: 3, Col: 2}
	tab.Anchor = tab.Cursor

	scr := a.screen.(tcell.SimulationScreen)
	scr.SetSize(80, 20)
	a.width, a.height = 80, 20
	a.draw()
	scr.Show()

	ex, ey, _, _ := a.editorRect()
	cells, w, _ := scr.GetContents()

	// Locate "foo" on each of the two rows purely by reading the rendered
	// text, independent of any coordinate math this file's production code
	// performs.
	//
	// 🔴 The search MUST be rune-indexed, not byte-indexed: screenText
	// writes one RUNE per screen column (so a column number equals a rune
	// index), but the row also contains the sidebar's multi-byte "│"
	// splitter glyph. strings.Index returns a BYTE offset, which runs ahead
	// of the true column the moment a multi-byte rune precedes the match —
	// exactly the kind of silent unit mismatch this whole file exists to
	// catch, just relocated into the test instead of the production code.
	for _, line := range []int{3, 4} {
		y := ey + line
		row := screenText(cells, w, y)
		idx := runeIndexOf(row, "foo")
		if idx < 0 {
			t.Fatalf("line %d: \"foo\" not found in rendered row %q", line, row)
		}
		for x := idx; x < idx+3; x++ {
			if !cellHasHighlightBG(a, cells, w, x, y) {
				t.Errorf("line %d col %d (rendered 'foo') is not tinted", line, x-idx)
			}
		}
		// Immediately outside the glyph span must NOT be tinted — catches
		// an off-by-N as surely as a missing tint would.
		if idx > ex && cellHasHighlightBG(a, cells, w, idx-1, y) {
			t.Errorf("line %d: tint leaked one column left of \"foo\"", line)
		}
		if cellHasHighlightBG(a, cells, w, idx+3, y) {
			t.Errorf("line %d: tint leaked one column right of \"foo\"", line)
		}
	}
}

// TestDocumentHighlightSkipsWrappedTabs pins the wrap-mode suppression: with
// Tab.Wrap on, ScreenPos no longer maps one buffer line to one screen row
// (see wrap.go), so drawDocumentHighlights must skip entirely rather than
// guess at a row — the same rule drawDiagnosticsInline already follows.
func TestDocumentHighlightSkipsWrappedTabs(t *testing.T) {
	a, _ := appWithFile(t, tabIndentedFixture)
	tab := a.activeTabPtr()
	tab.Cursor = editor.Position{Line: 3, Col: 2}
	tab.Anchor = tab.Cursor
	tab.Wrap = true

	scr := a.screen.(tcell.SimulationScreen)
	scr.SetSize(80, 20)
	a.width, a.height = 80, 20
	a.draw() // must not panic even though the wrapped renderer changed row layout
	scr.Show()

	cells, w, h := scr.GetContents()
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if cellHasHighlightBG(a, cells, w, x, y) {
				t.Fatalf("found a tinted cell at (%d,%d) on a wrapped tab", x, y)
			}
		}
	}
}

// TestVisibleOccurrencesAreWindowRelative pins visibleOccurrences' contract
// directly, with no screen involved: matches are found only within
// [top, top+height) and Match.Line comes back relative to top, not to the
// tab's absolute buffer line.
func TestVisibleOccurrencesAreWindowRelative(t *testing.T) {
	a, _ := appWithFile(t, tabIndentedFixture)
	tab := a.activeTabPtr()

	// Window starting at line 3 covering both "foo" lines (3 and 4).
	matches := a.visibleOccurrences(tab, "foo", 3, 2)
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches in the window, got %d: %+v", len(matches), matches)
	}
	if matches[0].Line != 0 {
		t.Errorf("first match Line = %d, want 0 (window-relative for absolute line 3)", matches[0].Line)
	}
	if matches[1].Line != 1 {
		t.Errorf("second match Line = %d, want 1 (window-relative for absolute line 4)", matches[1].Line)
	}

	// A window that excludes line 4 must not find its occurrence.
	matches = a.visibleOccurrences(tab, "foo", 3, 1)
	if len(matches) != 1 {
		t.Fatalf("expected 1 match in the narrower window, got %d: %+v", len(matches), matches)
	}
}

// TestDrawDocumentHighlightsSuppressedCases covers the suppression rules
// that don't need pixel-level verification: an active selection, an open
// find query, and a too-short symbol must each produce zero tinted cells.
func TestDrawDocumentHighlightsSuppressedCases(t *testing.T) {
	scr := func(a *App) (tcell.SimulationScreen, []tcell.SimCell, int) {
		s := a.screen.(tcell.SimulationScreen)
		s.SetSize(80, 20)
		a.width, a.height = 80, 20
		a.draw()
		s.Show()
		cells, w, _ := s.GetContents()
		return s, cells, w
	}
	anyTinted := func(a *App, cells []tcell.SimCell, w int) bool {
		for i := range cells {
			if _, bg, _ := cells[i].Style.Decompose(); bg == a.theme.FindMatch {
				return true
			}
		}
		return false
	}

	t.Run("active selection", func(t *testing.T) {
		a, _ := appWithFile(t, tabIndentedFixture)
		tab := a.activeTabPtr()
		tab.Cursor = editor.Position{Line: 3, Col: 2}
		tab.Anchor = editor.Position{Line: 3, Col: 0}
		_, cells, w := scr(a)
		if anyTinted(a, cells, w) {
			t.Fatal("no highlight should draw while a selection is active")
		}
	})

	t.Run("open find query", func(t *testing.T) {
		a, _ := appWithFile(t, tabIndentedFixture)
		tab := a.activeTabPtr()
		tab.Cursor = editor.Position{Line: 3, Col: 2}
		tab.Anchor = tab.Cursor
		tab.FindQuery = "1"
		_, cells, w := scr(a)
		if anyTinted(a, cells, w) {
			t.Fatal("no highlight should draw while a find query is open")
		}
	})

	t.Run("symbol too short", func(t *testing.T) {
		a, _ := appWithFile(t, "package main\n\nfunc f() {}\n")
		tab := a.activeTabPtr()
		tab.Cursor = editor.Position{Line: 2, Col: 6} // inside the single-rune name "f"
		tab.Anchor = tab.Cursor
		_, cells, w := scr(a)
		if anyTinted(a, cells, w) {
			t.Fatal("a one-rune symbol should never trigger a highlight")
		}
	})
}

// cellHasHighlightBG reports whether the cell at (x, y) carries the
// document-highlight background colour.
func cellHasHighlightBG(a *App, cells []tcell.SimCell, w, x, y int) bool {
	_, bg, _ := cells[y*w+x].Style.Decompose()
	return bg == a.theme.FindMatch
}

// runeIndexOf returns the rune-indexed position of needle within haystack,
// or -1 if absent. Unlike strings.Index (byte-indexed), the result is
// directly usable as a screen column against a row screenText built — that
// helper writes exactly one rune per column, so a rune index and a column
// index are the same number, which a byte index is not once a multi-byte
// glyph like the sidebar's "│" splitter appears earlier in the row.
func runeIndexOf(haystack, needle string) int {
	hr := []rune(haystack)
	nr := []rune(needle)
	if len(nr) == 0 || len(nr) > len(hr) {
		return -1
	}
	for i := 0; i+len(nr) <= len(hr); i++ {
		match := true
		for j := range nr {
			if hr[i+j] != nr[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
