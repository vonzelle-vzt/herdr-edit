// =============================================================================
// File: internal/editor/geometry_test.go
// Author: Vonzelle Brown
// Created: 2026-07-30
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package editor

import (
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/cloudmanic/spice-edit/internal/theme"
)

// TestScreenPos_ExpandsTabs pins the bug a real render exposed: col is a RUNE
// index but the screen is measured in CELLS, and a hard tab is not one cell.
// Treating them as interchangeable put the diagnostic underline on the wrong
// characters and made the inline message overwrite the end of the line, on any
// tab-indented line — which is most Go source.
//
// Expected values are gut+1 (contentStart), not gut — see ScreenPos's own
// comment: Render's contentX reserves one extra cell past the gutter for the
// number/code separator, confirmed against a real tcell.SimulationScreen.
func TestScreenPos_ExpandsTabs(t *testing.T) {
	tab := &Tab{Buffer: NewBuffer("\tw.Write(x)\n"), IndentUnit: "\t"}
	contentStart := tab.GutterWidth() + 1

	// Rune 0 is the tab itself: it starts where the text content starts.
	if dx, _, ok := tab.ScreenPos(0, 0, 200, 50); !ok || dx != contentStart {
		t.Fatalf("rune 0 -> dx=%d, want %d", dx, contentStart)
	}
	// Rune 1 is the 'w', which sits AFTER the expanded tab, not one cell in.
	dx, _, ok := tab.ScreenPos(0, 1, 200, 50)
	if !ok {
		t.Fatal("rune 1 not visible")
	}
	if want := contentStart + TabStop; dx != want {
		t.Fatalf("rune 1 (after a tab) -> dx=%d, want %d — the tab was not expanded", dx, want)
	}
	// End of line: the tab plus every following rune.
	runes := []rune("\tw.Write(x)")
	endDx, _, _ := tab.ScreenPos(0, len(runes), 200, 50)
	if want := contentStart + TabStop + (len(runes) - 1); endDx != want {
		t.Fatalf("end of line -> dx=%d, want %d", endDx, want)
	}
}

// TestScreenPos_AgreesWithNoTabs guards that the fix did not change the
// tab-free case, which is the overwhelming majority of lines.
func TestScreenPos_AgreesWithNoTabs(t *testing.T) {
	tab := &Tab{Buffer: NewBuffer("plain line here\n"), IndentUnit: "    "}
	contentStart := tab.GutterWidth() + 1
	for col := 0; col <= 15; col++ {
		dx, _, ok := tab.ScreenPos(0, col, 200, 50)
		if !ok {
			t.Fatalf("col %d not visible", col)
		}
		if dx != contentStart+col {
			t.Fatalf("col %d -> dx=%d, want %d", col, dx, contentStart+col)
		}
	}
}

// TestScreenPos_MatchesRenderedGlyphs is an INDEPENDENT check of ScreenPos,
// written without reference to its formula: it renders a tab, finds where a
// known token's glyphs ACTUALLY landed by scanning the screen, and compares
// that against what ScreenPos claims. The fixture is tab-indented, where a
// rune index and a screen cell diverge.
//
// 🔴 This exists because the two tests above cannot catch the bug that shipped.
// They assert ScreenPos against a restatement of its own arithmetic, so when
// contentStart was GutterWidth() instead of GutterWidth()+1 they passed while
// EVERY overlay in the editor — diagnostic underlines, Error Lens messages,
// inline blame, the completion popup anchor — drew one column left of its text
// on every line. Same family as the geometry constants and the panel-label
// list: an oracle that restates the thing it checks cannot police it. Reading
// rendered glyphs is the only assertion here that is independent of the code
// under test, and it reports "off by N" against the old formula.
func TestScreenPos_MatchesRenderedGlyphs(t *testing.T) {
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer scr.Fini()
	scr.SetSize(60, 10)

	tab := &Tab{Buffer: &Buffer{Lines: []string{
		"package main",
		"\tZZZZ := 1",
	}}}
	th := theme.Default()
	tab.Render(scr, th, 0, 0, 60, 10)
	scr.Show()

	cells, w, _ := scr.GetContents()

	// Where did the Z run actually land on row 1?
	firstZ := -1
	for x := 0; x < w; x++ {
		if cells[1*w+x].Runes[0] == 'Z' {
			firstZ = x
			break
		}
	}
	if firstZ < 0 {
		t.Fatal("probe fixture never rendered: no Z glyph on row 1")
	}

	// Rune index of the first Z on that line is 1 (index 0 is the hard tab).
	dx, dy, ok := tab.ScreenPos(1, 1, 60, 10)
	if !ok {
		t.Fatal("ScreenPos reported the position invisible")
	}
	t.Logf("rendered first Z at column %d; ScreenPos says %d (row %d)", firstZ, dx, dy)
	if dx != firstZ {
		t.Fatalf("ScreenPos is off by %d: says column %d, glyph is at %d", dx-firstZ, dx, firstZ)
	}
	if dy != 1 {
		t.Fatalf("ScreenPos row = %d, want 1", dy)
	}
}
