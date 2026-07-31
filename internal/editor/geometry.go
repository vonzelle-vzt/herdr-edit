// =============================================================================
// File: internal/editor/geometry.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-07-29
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

package editor

// Screen geometry that callers outside this package need in order to draw ON
// TOP of a rendered tab.
//
// The diagnostics overlay is the reason this exists: it underlines a range the
// language server reported, which means it has to land on exactly the cells
// Render just painted. Re-deriving the gutter width in the app package would
// work right up until this one changed, and the failure would be silently
// misplaced squiggles rather than anything that looks like a bug.

// GutterWidth is the width of the line-number column for this tab's buffer.
// Text starts one cell further in than this (see ScreenPos's contentStart) —
// that extra cell is the blank separator Render paints between the numbers
// and the code.
func (t *Tab) GutterWidth() int {
	return gutterWidthFor(len(t.Buffer.Lines))
}

// ScreenPos maps a buffer position to a cell offset within the editor
// rectangle, accounting for the gutter and both scroll axes.
//
// The bool reports whether the position is currently visible. Callers must
// honour it: a position scrolled out of view yields coordinates that would
// otherwise be drawn over the file tree or the status bar.
func (t *Tab) ScreenPos(line, col, viewW, viewH int) (dx, dy int, visible bool) {
	if line < t.ScrollY || line >= t.ScrollY+viewH {
		return 0, 0, false
	}
	// 🔴 Render's contentX is GutterWidth()+1, not GutterWidth(): one extra
	// cell separates the line-number column from the text (see the "1  "
	// vs "1   package" gap Render paints, and the cursor placement below).
	// A version of this function that omitted the +1 shipped and passed
	// every unit test in this package, because those tests only checked
	// this formula against ITSELF — never against a rendered screen. Found
	// by building a tcell.SimulationScreen, rendering, and reading where
	// the glyphs actually landed (per CLAUDE.md's "verify by rendering"
	// rule): every overlay was one column left of the real text on every
	// line, tabs or not.
	contentStart := t.GutterWidth() + 1
	// col is a RUNE index; the screen is measured in CELLS, and a hard tab is
	// not one cell. Treating them as interchangeable put every overlay a few
	// columns to the left of the text it was describing on any tab-indented
	// line -- which is most Go source. The diagnostic underline landed on the
	// wrong characters and the inline message overwrote the end of the line
	// instead of following it.
	//
	// This is the SAME arithmetic Render uses to place the cursor
	// (LineVisualCol against ScrollX, offset from contentX), and it has to
	// stay that way: the cursor is the position users verify by eye, so
	// anything that disagrees with it is wrong by definition.
	runes := []rune(t.LineText(line))
	dx = contentStart + (LineVisualCol(runes, col) - LineVisualCol(runes, t.ScrollX))
	dy = line - t.ScrollY
	if dx < contentStart || dx >= viewW {
		return 0, 0, false
	}
	return dx, dy, true
}

// LineRuneLen is the length of a buffer line in runes, which is the unit
// positions are measured in throughout this package. Bounds-checked so callers
// can ask about a line number a stale diagnostic still refers to.
func (t *Tab) LineRuneLen(line int) int {
	if line < 0 || line >= len(t.Buffer.Lines) {
		return 0
	}
	return len([]rune(t.Buffer.Lines[line]))
}

// LineText returns a buffer line, or "" when the index is out of range. Same
// motivation as LineRuneLen: diagnostics can outlive the text they describe.
func (t *Tab) LineText(line int) string {
	if line < 0 || line >= len(t.Buffer.Lines) {
		return ""
	}
	return t.Buffer.Lines[line]
}

// GutterRowFor reports the screen row, relative to the top of the editor rect,
// at which line's gutter cell is drawn — for BOTH geometries. visible is false
// when the line is scrolled out of view or out of the buffer.
//
// Overlays that mark a whole LINE (a breakpoint dot, the debugger's stopped
// arrow) need only a row, not a column, so ScreenPos is the wrong tool: it
// resolves a column too and returns invisible whenever that column is scrolled
// off, which for a gutter glyph is not a reason to skip painting.
//
// 🔴 The wrapped branch is a DISPATCH to wrap.go, not wrapping threaded through
// this file. One buffer line occupies several rows when Tab.Wrap is on, so
// `line - ScrollY` is simply a different quantity there — it was wrong by the
// number of continuation rows above it, which is why the debug gutter used to
// refuse to paint on a wrapped tab at all rather than paint in the wrong place.
// The row returned is the line's FIRST segment: the one renderWrapped puts the
// line number on, and continuation rows draw ↪ instead.
func (t *Tab) GutterRowFor(line, viewW, viewH int) (row int, visible bool) {
	if line < 0 || line >= t.Buffer.LineCount() {
		return 0, false
	}
	if !t.Wrap {
		row = line - t.ScrollY
	} else {
		// Same arithmetic renderWrapped uses (gw + 1 for the separator column),
		// because a marker that disagrees with the renderer lands on the wrong row.
		contentW := viewW - t.GutterWidth() - 1
		if contentW < 1 {
			contentW = 1
		}
		row = t.wrapVisualRows(t.ScrollY, t.ScrollSub, line, 0, contentW)
	}
	if row < 0 || row >= viewH {
		return 0, false
	}
	return row, true
}
