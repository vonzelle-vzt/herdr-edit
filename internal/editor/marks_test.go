// =============================================================================
// File: internal/editor/marks_test.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package editor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// linesOf builds a buffer with n numbered lines, enough for the shift/drop
// tests below to have real line indices to insert or delete around.
func linesOf(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString("line\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// TestMark_SetClearAt pins the basic map contract: SetMark is visible via
// MarkAt, ClearMark removes it, and a never-set line reports ok=false.
func TestMark_SetClearAt(t *testing.T) {
	tab, _ := NewTab("")
	if _, ok := tab.MarkAt(0); ok {
		t.Fatal("expected no mark on a fresh tab")
	}
	tab.SetMark(3, Mark{Kind: MarkBreakpoint, Enabled: true})
	m, ok := tab.MarkAt(3)
	if !ok || m.Kind != MarkBreakpoint || !m.Enabled {
		t.Fatalf("got %+v, %v", m, ok)
	}
	tab.ClearMark(3)
	if _, ok := tab.MarkAt(3); ok {
		t.Fatal("expected mark removed after ClearMark")
	}
}

// TestMark_LinesSortedAscending pins MarkLines' ordering contract, which the
// gutter, the export list, and the app's breakpoint picker all rely on.
func TestMark_LinesSortedAscending(t *testing.T) {
	tab, _ := NewTab("")
	for _, l := range []int{9, 0, 4} {
		tab.SetMark(l, Mark{Kind: MarkBreakpoint, Enabled: true})
	}
	got := tab.MarkLines()
	want := []int{0, 4, 9}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// TestMarksShiftWhenLinesInsertedAbove is oracle #3 from the Lane B stage 1
// brief: a mark on line 10 must move to line 12 after two lines are inserted
// above it. This is the whole point of routing every edit through bufInsert
// — without shiftMarks running, the breakpoint would silently stay on line
// 10, which is now two lines of DIFFERENT text than what the user marked.
func TestMarksShiftWhenLinesInsertedAbove(t *testing.T) {
	tab, _ := NewTab("")
	tab.Buffer = NewBuffer(linesOf(15))
	tab.SetMark(10, Mark{Kind: MarkBreakpoint, Enabled: true})

	tab.bufInsert(Position{Line: 2, Col: 0}, "new\nnew\n")

	if _, ok := tab.MarkAt(10); ok {
		t.Fatal("mark should have moved off line 10")
	}
	m, ok := tab.MarkAt(12)
	if !ok || m.Kind != MarkBreakpoint {
		t.Fatalf("expected the mark on line 12, got MarkAt(12)=%+v,%v; marks=%v", m, ok, tab.Marks)
	}
}

// TestMarksDieWhenTheirLinesAreDeleted is oracle #4: deleting the exact lines
// a mark sits on must remove the mark, not silently renumber it onto
// whatever content now occupies that line index.
func TestMarksDieWhenTheirLinesAreDeleted(t *testing.T) {
	tab, _ := NewTab("")
	tab.Buffer = NewBuffer(linesOf(15))
	tab.SetMark(5, Mark{Kind: MarkBreakpoint, Enabled: true})

	// Deletes lines 4 and 5 entirely (a.Line=4..c.Line=6 is the half-open
	// span DeleteRange folds away), which is exactly the span containing
	// the marked line.
	tab.bufDelete(Position{Line: 4, Col: 0}, Position{Line: 6, Col: 0})

	if _, ok := tab.MarkAt(5); ok {
		t.Fatal("mark on a deleted line should be gone")
	}
	for _, l := range tab.MarkLines() {
		t.Fatalf("expected no surviving marks, found one at line %d", l)
	}
}

// TestMarksShiftUpWhenLinesDeletedBelow checks the companion case to #4: a
// mark that sits AFTER a deleted span must move up to follow its content,
// not die (it was never inside the deleted range) and not stay put (its
// line number is now wrong).
func TestMarksShiftUpWhenLinesDeletedBelow(t *testing.T) {
	tab, _ := NewTab("")
	tab.Buffer = NewBuffer(linesOf(15))
	tab.SetMark(10, Mark{Kind: MarkBreakpoint, Enabled: true})

	// Deletes lines 2-4 (three lines' worth of line entries), same shape as
	// the undo-restore scenario in undo_test.go.
	tab.bufDelete(Position{Line: 1, Col: 0}, Position{Line: 4, Col: 0})

	if _, ok := tab.MarkAt(10); ok {
		t.Fatal("mark should have shifted off line 10")
	}
	if _, ok := tab.MarkAt(7); !ok {
		t.Fatalf("expected the mark on line 7 (10-3), marks=%v", tab.Marks)
	}
}

// TestMarksUnaffectedBySingleLineEdit checks the common case of typing on a
// marked line: no newline was inserted or removed, so the mark must not
// move at all.
func TestMarksUnaffectedBySingleLineEdit(t *testing.T) {
	tab, _ := NewTab("")
	tab.Buffer = NewBuffer(linesOf(5))
	tab.SetMark(2, Mark{Kind: MarkBreakpoint, Enabled: true})

	tab.bufInsert(Position{Line: 2, Col: 0}, "x")
	if _, ok := tab.MarkAt(2); !ok {
		t.Fatal("a same-line insert must not move the mark")
	}

	tab.bufDelete(Position{Line: 2, Col: 0}, Position{Line: 2, Col: 1})
	if _, ok := tab.MarkAt(2); !ok {
		t.Fatal("a same-line delete must not move the mark")
	}
}

// TestReloadClampsMarksToTheNewLineCount pins what happens when the file
// changes underneath us. Mark tracking works by observing bufInsert/bufDelete,
// so an external rewrite is invisible to it: without clamping, a mark on line
// 40 of a file that shrank to 5 lines survives as a breakpoint on a line that
// does not exist, and any renderer or adapter reading MarkLines would then be
// working from a position no longer in the buffer.
func TestReloadClampsMarksToTheNewLineCount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shrink.go")
	long := ""
	for i := 0; i < 40; i++ {
		long += "line\n"
	}
	if err := os.WriteFile(path, []byte(long), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	tab, err := NewTab(path)
	if err != nil {
		t.Fatalf("NewTab: %v", err)
	}
	tab.SetMark(2, Mark{Kind: MarkBreakpoint, Enabled: true, Verified: true, VerifiedLine: 2})
	tab.SetMark(35, Mark{Kind: MarkBreakpoint, Enabled: true})

	if err := os.WriteFile(path, []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if err := tab.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	if _, ok := tab.MarkAt(35); ok {
		t.Fatal("a mark past the end of the reloaded file survived")
	}
	m, ok := tab.MarkAt(2)
	if !ok {
		t.Fatal("an in-range mark was dropped by Reload")
	}
	if m.Verified || m.VerifiedLine != -1 {
		t.Fatalf("Verified state survived a reload: %+v — it describes the OLD content", m)
	}
}
