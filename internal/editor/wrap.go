// =============================================================================
// File: internal/editor/wrap.go
// Author: Vonzelle Brown
// Created: 2026-07-30
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// wrap.go is the visual-line layer: the mapping between buffer positions and screen rows once one
// buffer line may occupy several rows.
//
// Everything in the editor outside this file assumed one buffer line == one screen row, and that
// assumption is baked into Render, HitTest, EnsureVisible, clampScroll and 21 uses of ScrollX. So
// wrapping is introduced as a SEPARATE path guarded by Tab.Wrap rather than as a rewrite of those:
// with Wrap false, not one line of the original geometry changes. A cursor that lands on the wrong
// character is the worst bug an editor can have, and confining the new arithmetic to one mode is
// what makes it safe to add at all.
//
// Scrolling in wrapped mode needs a sub-row anchor. ScrollY stays a BUFFER line and ScrollSub counts
// how many of that line's wrapped rows are scrolled off the top. Without ScrollSub a single minified
// line longer than the viewport would be unscrollable — you would see its first screenful and have
// no way to reach the rest.

package editor

import (
	"github.com/gdamore/tcell/v2"

	"github.com/cloudmanic/spice-edit/internal/theme"
)

// wrapSegment is one screen row's worth of a buffer line: rune indices into the line, End exclusive.
type wrapSegment struct {
	Start int
	End   int
}

// lineSegments splits a buffer line into the rows it occupies at contentW columns.
//
// Breaks at the last space before the limit when there is one, so words stay whole — that is the
// difference between "word wrap" and merely chopping at the viewport edge. A token longer than the
// whole row (a URL, a base64 blob, a minified line) has no break point available and is cut hard,
// which is what every editor does with it.
//
// Always returns at least one segment, including for an empty line, because an empty line still
// occupies a row and the cursor can sit on it.
func (t *Tab) lineSegments(lineIdx, contentW int) []wrapSegment {
	if contentW < 1 {
		contentW = 1
	}
	runes := t.Buffer.LineRunes(lineIdx)
	if len(runes) == 0 {
		return []wrapSegment{{Start: 0, End: 0}}
	}

	segs := make([]wrapSegment, 0, 4)
	start := 0
	for start < len(runes) {
		// Walk forward while the row still fits. Widths are measured from the START OF THE ROW, so a
		// tab lands on a tab stop within its own row -- anchoring to the start of the buffer line
		// instead would put tab stops at positions that do not exist on screen.
		visual := 0
		lastSpace := -1
		i := start
		for ; i < len(runes); i++ {
			wid := RuneVisualWidth(runes[i], visual)
			if visual+wid > contentW {
				break
			}
			visual += wid
			if runes[i] == ' ' || runes[i] == '\t' {
				lastSpace = i
			}
		}
		if i >= len(runes) {
			segs = append(segs, wrapSegment{Start: start, End: len(runes)})
			break
		}
		end := i
		// Prefer a word boundary, but never produce an empty row: a break at the very start of the
		// segment would loop forever on a line beginning with a long token.
		if lastSpace > start {
			end = lastSpace + 1
		}
		if end <= start {
			end = start + 1
		}
		segs = append(segs, wrapSegment{Start: start, End: end})
		start = end
	}
	if len(segs) == 0 {
		segs = append(segs, wrapSegment{Start: 0, End: 0})
	}
	return segs
}

// wrapRowsFor reports how many screen rows a buffer line occupies.
func (t *Tab) wrapRowsFor(lineIdx, contentW int) int {
	return len(t.lineSegments(lineIdx, contentW))
}

// segmentOfCol returns the index of the segment containing rune column col, and col's visual offset
// within that segment.
//
// Uses <= on End for the last segment so a cursor sitting one past the final rune (the normal
// end-of-line position) belongs to the last row rather than falling off the end.
func (t *Tab) segmentOfCol(lineIdx, col, contentW int) (segIdx, visualOff int) {
	segs := t.lineSegments(lineIdx, contentW)
	runes := t.Buffer.LineRunes(lineIdx)
	for i, s := range segs {
		last := i == len(segs)-1
		if col < s.End || (last && col <= s.End) {
			off := 0
			for r := s.Start; r < col && r < len(runes); r++ {
				off += RuneVisualWidth(runes[r], off)
			}
			return i, off
		}
	}
	return len(segs) - 1, 0
}

// colAtSegmentVisual maps a visual cell offset within a segment back to a rune column, the inverse of
// segmentOfCol and the function a click goes through.
func (t *Tab) colAtSegmentVisual(lineIdx, segIdx, visualOff, contentW int) int {
	segs := t.lineSegments(lineIdx, contentW)
	if segIdx < 0 {
		segIdx = 0
	}
	if segIdx >= len(segs) {
		segIdx = len(segs) - 1
	}
	seg := segs[segIdx]
	runes := t.Buffer.LineRunes(lineIdx)

	visual := 0
	for r := seg.Start; r < seg.End && r < len(runes); r++ {
		wid := RuneVisualWidth(runes[r], visual)
		// Land on the rune whose cell span contains the click, so clicking the right half of a wide
		// glyph still selects that glyph rather than the next one.
		if visualOff < visual+wid {
			return r
		}
		visual += wid
	}
	return seg.End
}

// wrapVisualRows counts the screen rows between the top of the viewport and the cursor, honouring
// ScrollSub. Negative when the cursor is above the viewport.
func (t *Tab) wrapVisualRows(fromLine, fromSub, toLine, toCol, contentW int) int {
	if toLine < fromLine {
		rows := 0
		for l := toLine; l < fromLine; l++ {
			rows -= t.wrapRowsFor(l, contentW)
		}
		seg, _ := t.segmentOfCol(toLine, toCol, contentW)
		return rows + seg - fromSub
	}
	rows := -fromSub
	for l := fromLine; l < toLine && l < t.Buffer.LineCount(); l++ {
		rows += t.wrapRowsFor(l, contentW)
	}
	seg, _ := t.segmentOfCol(toLine, toCol, contentW)
	return rows + seg
}

// wrapEnsureVisible is EnsureVisible for wrapped mode. ScrollX is forced to zero: with wrapping there
// is nothing off to the right, and a stale ScrollX left over from unwrapped mode would shift every
// row sideways.
func (t *Tab) wrapEnsureVisible(contentW, viewH int) {
	t.ScrollX = 0
	if viewH < 1 {
		viewH = 1
	}

	// Above the viewport: anchor the top to the cursor's own row.
	if t.Cursor.Line < t.ScrollY {
		t.ScrollY = t.Cursor.Line
		t.ScrollSub, _ = t.segmentOfCol(t.Cursor.Line, t.Cursor.Col, contentW)
		return
	}
	if t.Cursor.Line == t.ScrollY {
		if seg, _ := t.segmentOfCol(t.Cursor.Line, t.Cursor.Col, contentW); seg < t.ScrollSub {
			t.ScrollSub = seg
			return
		}
	}

	// Below the viewport: walk the top down, a row at a time, until the cursor fits. Row-at-a-time
	// rather than arithmetic because each line contributes a different number of rows.
	for t.wrapVisualRows(t.ScrollY, t.ScrollSub, t.Cursor.Line, t.Cursor.Col, contentW) >= viewH {
		rows := t.wrapRowsFor(t.ScrollY, contentW)
		if t.ScrollSub+1 < rows {
			t.ScrollSub++
			continue
		}
		if t.ScrollY+1 >= t.Buffer.LineCount() {
			break
		}
		t.ScrollY++
		t.ScrollSub = 0
	}
}

// wrapClampScroll keeps the wrapped viewport inside the document, mirroring clampScroll's overscroll
// allowance so the last line can still be read comfortably.
func (t *Tab) wrapClampScroll(contentW, viewH int) {
	if t.ScrollY < 0 {
		t.ScrollY = 0
	}
	if t.ScrollY >= t.Buffer.LineCount() {
		t.ScrollY = t.Buffer.LineCount() - 1
		if t.ScrollY < 0 {
			t.ScrollY = 0
		}
	}
	if t.ScrollSub < 0 {
		t.ScrollSub = 0
	}
	if rows := t.wrapRowsFor(t.ScrollY, contentW); t.ScrollSub >= rows {
		t.ScrollSub = rows - 1
	}

	// Do not let the top scroll so far that the document runs out. Counting total rows would be O(n)
	// on every render, so only the tail is measured: walk back from the end until we have a
	// viewport's worth plus the overscroll allowance.
	overscroll := viewH / 2
	if overscroll < 3 {
		overscroll = 3
	}
	budget := viewH - overscroll
	if budget < 1 {
		budget = 1
	}
	rows := 0
	limitLine := t.Buffer.LineCount() - 1
	for l := t.Buffer.LineCount() - 1; l >= 0; l-- {
		rows += t.wrapRowsFor(l, contentW)
		limitLine = l
		if rows >= budget {
			break
		}
	}
	if t.ScrollY > limitLine {
		t.ScrollY = limitLine
		t.ScrollSub = 0
	}
}

// wrapScroll moves the wrapped viewport by delta SCREEN rows, so a wheel notch travels the same
// visual distance whether or not the lines under it happen to be wrapped.
func (t *Tab) wrapScroll(delta, contentW int) {
	for ; delta > 0; delta-- {
		rows := t.wrapRowsFor(t.ScrollY, contentW)
		if t.ScrollSub+1 < rows {
			t.ScrollSub++
			continue
		}
		if t.ScrollY+1 >= t.Buffer.LineCount() {
			return
		}
		t.ScrollY++
		t.ScrollSub = 0
	}
	for ; delta < 0; delta++ {
		if t.ScrollSub > 0 {
			t.ScrollSub--
			continue
		}
		if t.ScrollY == 0 {
			return
		}
		t.ScrollY--
		t.ScrollSub = t.wrapRowsFor(t.ScrollY, contentW) - 1
	}
}

// wrapRowAt resolves a screen row to the buffer line and segment drawn there, which is what both the
// renderer and the click router need. ok is false when the row is past the end of the document.
func (t *Tab) wrapRowAt(row, contentW int) (lineIdx, segIdx int, ok bool) {
	line := t.ScrollY
	seg := t.ScrollSub
	for r := 0; ; r++ {
		if line >= t.Buffer.LineCount() {
			return 0, 0, false
		}
		if r == row {
			return line, seg, true
		}
		if seg+1 < t.wrapRowsFor(line, contentW) {
			seg++
			continue
		}
		line++
		seg = 0
	}
}

// renderWrapped is Render for wrapped mode. It mirrors the unwrapped renderer's decisions -- cursor
// line highlight, gutter, syntax styles, selection, find matches, hardware cursor -- but paints each
// buffer line across as many rows as it needs instead of scrolling sideways.
//
// A separate function rather than branches threaded through the 171-line original, because the two
// differ in their innermost loop: the unwrapped path offsets every cell by ScrollX, this one divides
// the line into rows. Interleaving them would make both harder to reason about, and the unwrapped
// path is the one that must not regress.
func (t *Tab) renderWrapped(scr tcell.Screen, th theme.Theme, x, y, w, h int) {
	gw := gutterWidthFor(t.Buffer.LineCount())
	contentX := x + gw + 1
	contentW := w - gw - 1
	if contentW < 1 {
		contentW = 1
	}
	t.lastWrapContentW = contentW

	if t.cursorMoved {
		t.wrapEnsureVisible(contentW, h)
		t.cursorMoved = false
	}
	t.wrapClampScroll(contentW, h)

	// Re-tokenise on the same triggers as the unwrapped path. Highlighting is indexed by absolute
	// line, so it is unaffected by wrapping; only the number of lines to cover changes, and a
	// viewport of h rows can never span more than h buffer lines.
	if t.StyleStale || t.ScrollY != t.lastHighlightScrollY || h != t.lastHighlightHeight {
		t.Styles = HighlightVisible(t.Path, t.Buffer.Lines, t.ScrollY, h, th)
		t.StyleStale = false
		t.lastHighlightScrollY = t.ScrollY
		t.lastHighlightHeight = h
	}

	bg := th.BG
	bgStyle := tcell.StyleDefault.Background(bg).Foreground(th.Text)
	for cy := y; cy < y+h; cy++ {
		for cx := x; cx < x+w; cx++ {
			scr.SetContent(cx, cy, ' ', nil, bgStyle)
		}
	}

	selStart, selEnd := PosOrdered(t.Anchor, t.Cursor)
	hasSel := t.HasSelection()

	cursorCX, cursorCY, cursorOn := 0, 0, false

	for row := 0; row < h; row++ {
		lineIdx, segIdx, ok := t.wrapRowAt(row, contentW)
		if !ok {
			break
		}
		cy := y + row
		isCursorLine := lineIdx == t.Cursor.Line

		lineBg := bg
		if isCursorLine {
			lineBg = th.LineHL
		}
		lineBgStyle := tcell.StyleDefault.Background(lineBg).Foreground(th.Text)
		for cx := x; cx < x+w; cx++ {
			scr.SetContent(cx, cy, ' ', nil, lineBgStyle)
		}

		gutterStyle := tcell.StyleDefault.Background(lineBg).Foreground(th.Muted)
		if isCursorLine {
			gutterStyle = gutterStyle.Foreground(th.AccentSoft)
		}
		if segIdx == 0 {
			// Line number only on a line's FIRST row. Repeating it on continuation rows would read as
			// several separate lines with the same number, which is worse than a blank gutter.
			numStr := fmtRightAligned(lineIdx+1, gw-1)
			if marker, mok := t.GitLines[lineIdx]; mok && marker != GitLineNone {
				scr.SetContent(x, cy, gitLineMarkerRune(marker), nil,
					gutterStyle.Foreground(gitLineMarkerColor(th, marker)))
			}
			for i, r := range numStr {
				if i == 0 && t.GitLines[lineIdx] != GitLineNone {
					continue
				}
				scr.SetContent(x+i, cy, r, nil, gutterStyle)
			}
		} else {
			// A muted marker so a continuation row is visibly a continuation, not a new line.
			scr.SetContent(x+gw-1, cy, '↪', nil, gutterStyle)
		}

		seg := t.lineSegments(lineIdx, contentW)[segIdx]
		runes := t.Buffer.LineRunes(lineIdx)
		var styles []tcell.Style
		if lineIdx < len(t.Styles) {
			styles = t.Styles[lineIdx]
		}

		visual := 0
		for runeIdx := seg.Start; runeIdx < seg.End && runeIdx < len(runes); runeIdx++ {
			r := runes[runeIdx]
			width := RuneVisualWidth(r, visual)

			st := bgStyle
			if runeIdx < len(styles) {
				st = styles[runeIdx]
			}
			st = st.Background(lineBg)
			if hasSel {
				p := Position{Line: lineIdx, Col: runeIdx}
				if !PosLess(p, selStart) && PosLess(p, selEnd) {
					st = st.Background(th.Selection)
				}
			}
			if mIdx := t.matchAtRune(lineIdx, runeIdx); mIdx >= 0 {
				if mIdx == t.FindIndex {
					st = st.Background(th.FindCurrent).Foreground(th.BG)
				} else {
					st = st.Background(th.FindMatch)
				}
			}

			glyph := r
			if r == '\t' {
				glyph = ' '
			}
			for cell := 0; cell < width; cell++ {
				sc := visual + cell
				if sc >= contentW {
					break
				}
				ch := glyph
				if cell > 0 {
					ch = ' '
				}
				scr.SetContent(contentX+sc, cy, ch, nil, st)
			}
			visual += width
		}

		// The hardware cursor, if it lives on this row. Computed here rather than afterwards because
		// this is where the row's identity is already known.
		if isCursorLine {
			if cSeg, cOff := t.segmentOfCol(lineIdx, t.Cursor.Col, contentW); cSeg == segIdx {
				if cOff <= contentW-1 {
					cursorCX, cursorCY, cursorOn = contentX+cOff, cy, true
				}
			}
		}
	}

	if cursorOn {
		scr.ShowCursor(cursorCX, cursorCY)
	} else {
		scr.HideCursor()
	}
}

// fmtRightAligned renders n right-aligned in width cells, without pulling fmt into this file's hot
// loop for a single integer.
func fmtRightAligned(n, width int) string {
	s := itoa(n)
	for len(s) < width {
		s = " " + s
	}
	return s
}

// itoa is strconv.Itoa without the import, kept tiny and local.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
