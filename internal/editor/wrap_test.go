// =============================================================================
// File: internal/editor/wrap_test.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-07-30
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

package editor

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/cloudmanic/spice-edit/internal/theme"
)

// wrapTab builds a tab with the given lines and wrapping on.
func wrapTab(lines ...string) *Tab {
	t := &Tab{Wrap: true}
	t.Buffer = &Buffer{Lines: lines}
	return t
}

// TestLineSegmentsBreaksOnWords is the difference between word wrap and chopping at the edge.
func TestLineSegmentsBreaksOnWords(t *testing.T) {
	tab := wrapTab("the quick brown fox jumps")
	segs := tab.lineSegments(0, 10)
	if len(segs) < 2 {
		t.Fatalf("expected several rows, got %d", len(segs))
	}
	runes := tab.Buffer.LineRunes(0)
	for i, s := range segs {
		text := string(runes[s.Start:s.End])
		if len([]rune(text)) > 10 {
			t.Fatalf("segment %d is %d runes, over the 10-column width: %q", i, len([]rune(text)), text)
		}
		// No row may begin mid-word, i.e. every break landed after a space.
		if i > 0 && s.Start > 0 && runes[s.Start-1] != ' ' {
			t.Fatalf("segment %d starts mid-word at %q", i, text)
		}
	}
}

// TestLineSegmentsHardBreaksLongTokens covers the case with no break point available: a minified line
// or a base64 blob must still be shown, cut hard, rather than looping or vanishing.
func TestLineSegmentsHardBreaksLongTokens(t *testing.T) {
	long := strings.Repeat("x", 95)
	tab := wrapTab(long)
	segs := tab.lineSegments(0, 10)
	if len(segs) != 10 {
		t.Fatalf("95 chars at width 10 should be 10 rows, got %d", len(segs))
	}
	// Every rune must be covered exactly once, in order — no gaps, no repeats.
	next := 0
	for _, s := range segs {
		if s.Start != next {
			t.Fatalf("segment starts at %d, expected %d (gap or overlap)", s.Start, next)
		}
		if s.End <= s.Start {
			t.Fatal("empty segment would loop forever")
		}
		next = s.End
	}
	if next != 95 {
		t.Fatalf("segments covered %d runes, want 95", next)
	}
}

// TestLineSegmentsEmptyLine — an empty line still occupies a row and the cursor can sit on it.
func TestLineSegmentsEmptyLine(t *testing.T) {
	tab := wrapTab("")
	if got := tab.wrapRowsFor(0, 20); got != 1 {
		t.Fatalf("empty line should occupy 1 row, got %d", got)
	}
}

// TestWrapColumnRoundTrip is the assertion that matters most: a cursor column must survive the trip
// to a screen position and back. If this drifts, clicks land on the wrong character — the worst class
// of editor bug and the reason wrapping is gated behind its own path.
func TestWrapColumnRoundTrip(t *testing.T) {
	lines := []string{
		"the quick brown fox jumps over the lazy dog",
		strings.Repeat("z", 40),
		"short",
		"",
		"tabs\tand\tmore\ttabs\there\tto\tcheck\tstops",
	}
	tab := wrapTab(lines...)
	for _, contentW := range []int{8, 13, 20, 37} {
		for line := range lines {
			runes := tab.Buffer.LineRunes(line)
			for col := 0; col <= len(runes); col++ {
				seg, off := tab.segmentOfCol(line, col, contentW)
				back := tab.colAtSegmentVisual(line, seg, off, contentW)
				if back != col {
					t.Fatalf("w=%d line=%d col=%d -> seg %d off %d -> col %d",
						contentW, line, col, seg, off, back)
				}
			}
		}
	}
}

// TestWrapRowAtWalksRowsInOrder pins the row->line mapping the renderer and the click router share.
func TestWrapRowAtWalksRowsInOrder(t *testing.T) {
	tab := wrapTab(strings.Repeat("a", 25), "b", strings.Repeat("c", 12))
	contentW := 10
	// 25/10 = 3 rows, then 1, then 2 = 6 rows total.
	want := []struct{ line, seg int }{
		{0, 0}, {0, 1}, {0, 2}, {1, 0}, {2, 0}, {2, 1},
	}
	for row, exp := range want {
		line, seg, ok := tab.wrapRowAt(row, contentW)
		if !ok {
			t.Fatalf("row %d: not ok", row)
		}
		if line != exp.line || seg != exp.seg {
			t.Fatalf("row %d -> line %d seg %d, want line %d seg %d", row, line, seg, exp.line, exp.seg)
		}
	}
	if _, _, ok := tab.wrapRowAt(len(want), contentW); ok {
		t.Fatal("a row past the end of the document must report not-ok")
	}
}

// TestWrapEnsureVisibleReachesDeepIntoALongLine is why ScrollSub exists. A single line longer than the
// viewport was unscrollable without it: you saw the first screenful and could not reach the rest.
func TestWrapEnsureVisibleReachesDeepIntoALongLine(t *testing.T) {
	tab := wrapTab(strings.Repeat("q", 500))
	contentW, viewH := 10, 5 // 50 rows of content in a 5-row window

	tab.Cursor = Position{Line: 0, Col: 480}
	tab.wrapEnsureVisible(contentW, viewH)

	rows := tab.wrapVisualRows(tab.ScrollY, tab.ScrollSub, 0, 480, contentW)
	if rows < 0 || rows >= viewH {
		t.Fatalf("cursor is %d rows from the top of a %d-row viewport", rows, viewH)
	}
	if tab.ScrollSub == 0 {
		t.Fatal("expected a sub-row scroll offset for a cursor deep inside one line")
	}

	// And back to the start.
	tab.Cursor = Position{Line: 0, Col: 0}
	tab.wrapEnsureVisible(contentW, viewH)
	if tab.ScrollY != 0 || tab.ScrollSub != 0 {
		t.Fatalf("cursor at 0:0 should reset the top, got line %d sub %d", tab.ScrollY, tab.ScrollSub)
	}
}

// TestWrapEnsureVisibleClearsScrollX — a stale horizontal offset from unwrapped mode would shift every
// wrapped row sideways.
func TestWrapEnsureVisibleClearsScrollX(t *testing.T) {
	tab := wrapTab("hello world")
	tab.ScrollX = 7
	tab.wrapEnsureVisible(20, 10)
	if tab.ScrollX != 0 {
		t.Fatalf("ScrollX should be cleared when wrapping, got %d", tab.ScrollX)
	}
}

// TestWrapScrollMovesByScreenRows — a wheel notch should travel the same visual distance whether the
// lines under it wrap or not.
func TestWrapScrollMovesByScreenRows(t *testing.T) {
	tab := wrapTab(strings.Repeat("a", 30), "b", "c")
	contentW := 10 // line 0 is 3 rows

	tab.wrapScroll(2, contentW)
	if tab.ScrollY != 0 || tab.ScrollSub != 2 {
		t.Fatalf("two rows down should stay inside line 0 at sub 2, got line %d sub %d",
			tab.ScrollY, tab.ScrollSub)
	}
	tab.wrapScroll(1, contentW)
	if tab.ScrollY != 1 || tab.ScrollSub != 0 {
		t.Fatalf("the next row should be line 1, got line %d sub %d", tab.ScrollY, tab.ScrollSub)
	}
	tab.wrapScroll(-1, contentW)
	if tab.ScrollY != 0 || tab.ScrollSub != 2 {
		t.Fatalf("scrolling back should re-enter line 0 at its last row, got line %d sub %d",
			tab.ScrollY, tab.ScrollSub)
	}
	// Cannot scroll above the document.
	tab.wrapScroll(-99, contentW)
	if tab.ScrollY != 0 || tab.ScrollSub != 0 {
		t.Fatalf("clamped at the top, got line %d sub %d", tab.ScrollY, tab.ScrollSub)
	}
}

// TestWrapNeverLoops guards the shape of bug that hangs an editor rather than misdrawing it: a
// segmenter that can emit a zero-width row never terminates.
func TestWrapNeverLoops(t *testing.T) {
	for _, contentW := range []int{1, 2, 3} {
		tab := wrapTab("ab cd ef", strings.Repeat("x", 20), "\t\t\t")
		for line := 0; line < tab.Buffer.LineCount(); line++ {
			segs := tab.lineSegments(line, contentW)
			if len(segs) == 0 {
				t.Fatalf("w=%d line=%d: no segments", contentW, line)
			}
			for _, s := range segs {
				if s.End < s.Start {
					t.Fatalf("w=%d line=%d: inverted segment %+v", contentW, line, s)
				}
			}
		}
	}
}

// TestRenderWrappedPaintsContinuationRows is the end-to-end check: a long line must actually appear on
// more than one row, with no content lost off the right edge and no '›' overflow marker, because
// nothing is off-screen any more.
func TestRenderWrappedPaintsContinuationRows(t *testing.T) {
	text := "alpha bravo charlie delta echo foxtrot golf hotel india"
	tab := wrapTab(text)
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatal(err)
	}
	defer scr.Fini()
	w, h := 30, 10
	scr.SetSize(w, h)

	tab.Render(scr, theme.Default(), 0, 0, w, h)
	scr.Show()

	rows := screenRows(scr, w, h)
	if strings.Contains(strings.Join(rows, "\n"), "›") {
		t.Fatalf("wrapped output must not show the horizontal-overflow marker:\n%s", strings.Join(rows, "\n"))
	}

	// Every word must be present somewhere on screen — that is the user-visible promise.
	flat := strings.Join(rows, " ")
	for _, word := range strings.Fields(text) {
		if !strings.Contains(flat, word) {
			t.Fatalf("word %q was lost:\n%s", word, strings.Join(rows, "\n"))
		}
	}

	// It has to occupy more than one row, or nothing wrapped.
	used := 0
	for _, r := range rows {
		if strings.TrimSpace(r) != "" {
			used++
		}
	}
	if used < 2 {
		t.Fatalf("expected continuation rows, only %d row(s) had content:\n%s", used, strings.Join(rows, "\n"))
	}

	// The line number appears once; continuation rows carry the ↪ marker instead.
	if n := strings.Count(flat, "↪"); n != used-1 {
		t.Fatalf("expected %d continuation markers, got %d:\n%s", used-1, n, strings.Join(rows, "\n"))
	}
}

// TestRenderUnwrappedStillClips is the regression guard for the mode that already worked: with Wrap
// off, the original geometry must be untouched, overflow marker and all.
func TestRenderUnwrappedStillClips(t *testing.T) {
	tab := wrapTab(strings.Repeat("word ", 40))
	tab.Wrap = false
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatal(err)
	}
	defer scr.Fini()
	w, h := 30, 10
	scr.SetSize(w, h)

	tab.Render(scr, theme.Default(), 0, 0, w, h)
	scr.Show()
	flat := strings.Join(screenRows(scr, w, h), "\n")
	if !strings.Contains(flat, "›") {
		t.Fatalf("unwrapped mode must still show the overflow marker:\n%s", flat)
	}
	if strings.Contains(flat, "↪") {
		t.Fatalf("unwrapped mode must not draw continuation markers:\n%s", flat)
	}
}

// screenRows reads the simulation screen back as strings.
func screenRows(scr tcell.SimulationScreen, w, h int) []string {
	cells, cw, _ := scr.GetContents()
	out := make([]string, 0, h)
	for y := 0; y < h; y++ {
		var b strings.Builder
		for x := 0; x < w; x++ {
			r := cells[y*cw+x].Runes
			if len(r) == 0 {
				b.WriteByte(' ')
				continue
			}
			b.WriteRune(r[0])
		}
		out = append(out, b.String())
	}
	return out
}
