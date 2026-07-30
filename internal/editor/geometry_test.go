// =============================================================================
// File: internal/editor/geometry_test.go
// Author: Vonzelle Brown
// Created: 2026-07-30
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package editor

import "testing"

// TestScreenPos_ExpandsTabs pins the bug a real render exposed: col is a RUNE
// index but the screen is measured in CELLS, and a hard tab is not one cell.
// Treating them as interchangeable put the diagnostic underline on the wrong
// characters and made the inline message overwrite the end of the line, on any
// tab-indented line — which is most Go source.
func TestScreenPos_ExpandsTabs(t *testing.T) {
	tab := &Tab{Buffer: NewBuffer("\tw.Write(x)\n"), IndentUnit: "\t"}
	gut := tab.GutterWidth()

	// Rune 0 is the tab itself: it starts at the gutter.
	if dx, _, ok := tab.ScreenPos(0, 0, 200, 50); !ok || dx != gut {
		t.Fatalf("rune 0 -> dx=%d, want %d", dx, gut)
	}
	// Rune 1 is the 'w', which sits AFTER the expanded tab, not one cell in.
	dx, _, ok := tab.ScreenPos(0, 1, 200, 50)
	if !ok {
		t.Fatal("rune 1 not visible")
	}
	if want := gut + TabStop; dx != want {
		t.Fatalf("rune 1 (after a tab) -> dx=%d, want %d — the tab was not expanded", dx, want)
	}
	// End of line: the tab plus every following rune.
	runes := []rune("\tw.Write(x)")
	endDx, _, _ := tab.ScreenPos(0, len(runes), 200, 50)
	if want := gut + TabStop + (len(runes) - 1); endDx != want {
		t.Fatalf("end of line -> dx=%d, want %d", endDx, want)
	}
}

// TestScreenPos_AgreesWithNoTabs guards that the fix did not change the
// tab-free case, which is the overwhelming majority of lines.
func TestScreenPos_AgreesWithNoTabs(t *testing.T) {
	tab := &Tab{Buffer: NewBuffer("plain line here\n"), IndentUnit: "    "}
	gut := tab.GutterWidth()
	for col := 0; col <= 15; col++ {
		dx, _, ok := tab.ScreenPos(0, col, 200, 50)
		if !ok {
			t.Fatalf("col %d not visible", col)
		}
		if dx != gut+col {
			t.Fatalf("col %d -> dx=%d, want %d", col, dx, gut+col)
		}
	}
}
