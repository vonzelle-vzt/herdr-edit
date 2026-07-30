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

// GutterWidth is the width of the line-number column for this tab's buffer,
// i.e. how far in from the editor rectangle the text actually starts.
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
	gut := t.GutterWidth()
	dx = gut + (col - t.ScrollX)
	dy = line - t.ScrollY
	if dx < gut || dx >= viewW {
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
