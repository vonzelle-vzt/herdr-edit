// =============================================================================
// File: internal/app/navigate_test.go
// Author: Vonzelle Brown
// Created: 2026-07-30
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudmanic/spice-edit/internal/state"
)

// seedNavApp opens a tab with numbered lines so a jump can be checked by
// position rather than by content.
func seedNavApp(t *testing.T, content string) *App {
	t.Helper()
	dir := t.TempDir()
	target := filepath.Join(dir, "n.txt")
	if err := os.WriteFile(target, []byte(content), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	a := newTestApp(t, dir)
	a.openFile(target)
	return a
}

// TestParseLineRef_Accepts covers the two shapes a user actually types and the
// 1-based to 0-based conversion between them. Users count lines from one and
// the buffer counts from zero, which is invisible until someone jumps to the
// last line of a file and lands past the end.
func TestParseLineRef_Accepts(t *testing.T) {
	cases := []struct {
		in           string
		lines        int
		wantL, wantC int
	}{
		{"1", 10, 0, 0},
		{"7", 10, 6, 0},
		{"10", 10, 9, 0},
		{"  4  ", 10, 3, 0},
		{"3:5", 10, 2, 4},
		{"3:1", 10, 2, 0},
	}
	for _, c := range cases {
		got, ok := parseLineRef(c.in, c.lines)
		if !ok {
			t.Errorf("parseLineRef(%q) rejected a valid reference", c.in)
			continue
		}
		if got.Line != c.wantL || got.Col != c.wantC {
			t.Errorf("parseLineRef(%q) = %d:%d, want %d:%d", c.in, got.Line, got.Col, c.wantL, c.wantC)
		}
	}
}

// TestParseLineRef_Rejects pins the inputs that must NOT move the cursor.
// Jumping somewhere arbitrary on a typo is worse than refusing: the user loses
// their place and does not know why.
func TestParseLineRef_Rejects(t *testing.T) {
	for _, in := range []string{"", "   ", "abc", "0", "-3", "12", "3:0", "3:x", ":", "1.5"} {
		if _, ok := parseLineRef(in, 10); ok {
			t.Errorf("parseLineRef(%q) should have been rejected", in)
		}
	}
}

// TestParseLineRef_UnknownLineCount checks that a zero line count disables the
// upper bound rather than rejecting everything — the clamp in MoveCursorTo is
// the backstop in that case.
func TestParseLineRef_UnknownLineCount(t *testing.T) {
	if _, ok := parseLineRef("9999", 0); !ok {
		t.Fatal("with an unknown line count the range check should not reject")
	}
}

// TestGoToLine_MovesCursor drives the feature the way a user does — through the
// prompt — and asserts the cursor actually moved.
func TestGoToLine_MovesCursor(t *testing.T) {
	a := seedNavApp(t, "one\ntwo\nthree\nfour\nfive\n")
	a.menuGoToLine()
	if !a.promptOpen {
		t.Fatal("Esc g should open the prompt")
	}
	a.promptValue = []rune("4")
	a.promptSubmit()

	tab := a.activeTabPtr()
	if tab.Cursor.Line != 3 {
		t.Fatalf("cursor on line %d, want 3 (line 4, zero-indexed)", tab.Cursor.Line)
	}
	if a.promptOpen {
		t.Error("submitting should have closed the prompt")
	}
}

// TestGoToLine_OutOfRangeDoesNotMove pins that a refused reference leaves the
// cursor where it was.
func TestGoToLine_OutOfRangeDoesNotMove(t *testing.T) {
	a := seedNavApp(t, "one\ntwo\nthree\n")
	tab := a.activeTabPtr()
	tab.MoveCursorTo(tab.Cursor, false)
	before := tab.Cursor

	a.menuGoToLine()
	a.promptValue = []rune("900")
	a.promptSubmit()

	if a.activeTabPtr().Cursor != before {
		t.Fatalf("an out-of-range jump moved the cursor to %v", a.activeTabPtr().Cursor)
	}
}

// TestSelectAll_SelectsWholeBuffer is the caller that did not exist. SelectAll
// itself was complete and unit-tested in internal/editor with zero non-test
// callers; this test fails against that state because Esc a did nothing at all.
func TestSelectAll_SelectsWholeBuffer(t *testing.T) {
	a := seedNavApp(t, "alpha\nbeta\ngamma\n")
	tab := a.activeTabPtr()

	a.menuSelectAll()

	if tab.Anchor.Line != 0 || tab.Anchor.Col != 0 {
		t.Errorf("anchor at %v, want the start of the buffer", tab.Anchor)
	}
	end := tab.Buffer.EndPos()
	if tab.Cursor != end {
		t.Errorf("cursor at %v, want the end of the buffer %v", tab.Cursor, end)
	}
	if !a.hasSelection() {
		t.Error("select-all should leave the app reporting a selection")
	}
}

// TestSelectAll_ReachableFromTheLeaderTable guards the actual regression class
// here: the behaviour existing while nothing calls it. It asserts the binding
// is present rather than trusting that someone remembered to add it.
func TestSelectAll_ReachableFromTheLeaderTable(t *testing.T) {
	for _, k := range []rune{'a', 'g'} {
		if leaderActionFor(k) == nil {
			t.Errorf("Esc %c is not bound — the feature is unreachable", k)
		}
	}
}

// TestConsumeOpenRequest_OpensAtLine is the panel-to-editor direction that
// turns a read-only review into an edit: a panel hands over a path and a line,
// and the editor lands on it.
func TestConsumeOpenRequest_OpensAtLine(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	target := filepath.Join(dir, "target.go")
	if err := os.WriteFile(target, []byte("a\nb\nc\nd\ne\n"), 0644); err != nil {
		t.Fatal(err)
	}
	a := newTestApp(t, dir)

	if err := state.WriteOpenRequest(target, 4, 1); err != nil {
		t.Fatal(err)
	}
	a.consumeOpenRequest()

	tab := a.activeTabPtr()
	if tab == nil || tab.Path != target {
		t.Fatalf("the request did not open the file (tab = %v)", tab)
	}
	if tab.Cursor.Line != 3 {
		t.Fatalf("cursor on line %d, want 3 (1-based line 4)", tab.Cursor.Line)
	}
}

// TestConsumeOpenRequest_HonouredOnce pins the sequence guard. Without it the
// editor would reopen the same file on every event-loop tick, yanking the
// cursor back and making the editor unusable.
func TestConsumeOpenRequest_HonouredOnce(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	target := filepath.Join(dir, "t.go")
	if err := os.WriteFile(target, []byte("1\n2\n3\n4\n5\n6\n"), 0644); err != nil {
		t.Fatal(err)
	}
	a := newTestApp(t, dir)
	if err := state.WriteOpenRequest(target, 5, 1); err != nil {
		t.Fatal(err)
	}
	a.consumeOpenRequest()

	// The user moves away; the same stale request must not drag them back.
	a.activeTabPtr().MoveCursorTo(posAt(0, 0), false)
	a.consumeOpenRequest()
	if got := a.activeTabPtr().Cursor.Line; got != 0 {
		t.Fatalf("a already-honoured request moved the cursor again, to line %d", got)
	}
}

// TestConsumeOpenRequest_MissingFile pins that a request naming a file that is
// gone reports it rather than opening an empty buffer under that name.
func TestConsumeOpenRequest_MissingFile(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	a := newTestApp(t, t.TempDir())
	if err := state.WriteOpenRequest("/definitely/not/here.go", 1, 1); err != nil {
		t.Fatal(err)
	}
	a.consumeOpenRequest()
	if tab := a.activeTabPtr(); tab != nil && tab.Path == "/definitely/not/here.go" {
		t.Fatal("a missing file was opened as a tab")
	}
}
