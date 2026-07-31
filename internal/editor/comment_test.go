// =============================================================================
// File: internal/editor/comment_test.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-05-14
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

package editor

import "testing"

// TestLineCommentPrefix_CommonExtensions pins the filename and extension
// lookup used by the toggle action before it mutates a buffer.
func TestLineCommentPrefix_CommonExtensions(t *testing.T) {
	cases := []struct {
		path string
		want string
		ok   bool
	}{
		{"main.go", "//", true},
		{"script.py", "#", true},
		{"query.sql", "--", true},
		{"config.ini", ";", true},
		{"Dockerfile", "#", true},
		{"index.html", "", false},
	}
	for _, c := range cases {
		got, ok := LineCommentPrefix(c.path)
		if got != c.want || ok != c.ok {
			t.Fatalf("LineCommentPrefix(%q) = %q, %v; want %q, %v", c.path, got, ok, c.want, c.ok)
		}
	}
}

// TestToggleLineComment_CommentsSelectedLines checks the headline path:
// every selected non-blank line gets a comment marker at column zero.
func TestToggleLineComment_CommentsSelectedLines(t *testing.T) {
	tab := commentTestTab("main.go", "package main\nfunc main() {\n\tprintln(\"x\")\n}\n")
	tab.Anchor = Position{Line: 1, Col: 0}
	tab.Cursor = Position{Line: 3, Col: 0}

	changed, ok := tab.ToggleLineComment()

	if !ok || !changed {
		t.Fatalf("ToggleLineComment() = %v, %v; want changed and ok", changed, ok)
	}
	want := "package main\n// func main() {\n// \tprintln(\"x\")\n}\n"
	if got := tab.Buffer.String(); got != want {
		t.Fatalf("buffer:\n%q\nwant:\n%q", got, want)
	}
	if !tab.Dirty || !tab.StyleStale {
		t.Fatal("toggle should dirty the tab and invalidate highlighting")
	}
}

// TestToggleLineComment_UncommentsWhenAllLinesCommented proves the toggle
// flips direction only when every non-blank target line is already commented.
func TestToggleLineComment_UncommentsWhenAllLinesCommented(t *testing.T) {
	tab := commentTestTab("main.go", "// one\n// \ttwo\n")
	tab.Anchor = Position{Line: 0, Col: 0}
	tab.Cursor = Position{Line: 2, Col: 0}

	changed, ok := tab.ToggleLineComment()

	if !ok || !changed {
		t.Fatalf("ToggleLineComment() = %v, %v; want changed and ok", changed, ok)
	}
	want := "one\n\ttwo\n"
	if got := tab.Buffer.String(); got != want {
		t.Fatalf("buffer:\n%q\nwant:\n%q", got, want)
	}
}

// TestToggleLineComment_UncommentsIndentedExistingComments keeps the toggle
// tolerant of comments that already sit after indentation.
func TestToggleLineComment_UncommentsIndentedExistingComments(t *testing.T) {
	tab := commentTestTab("main.go", "\t// one\n  // two\n")
	tab.Anchor = Position{Line: 0, Col: 0}
	tab.Cursor = Position{Line: 2, Col: 0}

	changed, ok := tab.ToggleLineComment()

	if !ok || !changed {
		t.Fatalf("ToggleLineComment() = %v, %v; want changed and ok", changed, ok)
	}
	want := "\tone\n  two\n"
	if got := tab.Buffer.String(); got != want {
		t.Fatalf("buffer:\n%q\nwant:\n%q", got, want)
	}
}

// TestToggleLineComment_MixedSelectionCommentsAllLines locks in the common
// editor rule: a mixed selection comments every non-blank line.
func TestToggleLineComment_MixedSelectionCommentsAllLines(t *testing.T) {
	tab := commentTestTab("main.go", "// one\n\n  two")
	tab.SelectAll()

	changed, ok := tab.ToggleLineComment()

	if !ok || !changed {
		t.Fatalf("ToggleLineComment() = %v, %v; want changed and ok", changed, ok)
	}
	want := "// // one\n\n//   two"
	if got := tab.Buffer.String(); got != want {
		t.Fatalf("buffer:\n%q\nwant:\n%q", got, want)
	}
}

// TestToggleLineComment_SelectionEndingAtColumnZeroExcludesThatLine keeps
// whole-line selections from unexpectedly changing the first untouched line.
func TestToggleLineComment_SelectionEndingAtColumnZeroExcludesThatLine(t *testing.T) {
	tab := commentTestTab("main.go", "one\ntwo\nthree")
	tab.Anchor = Position{Line: 0, Col: 0}
	tab.Cursor = Position{Line: 2, Col: 0}

	changed, ok := tab.ToggleLineComment()

	if !ok || !changed {
		t.Fatalf("ToggleLineComment() = %v, %v; want changed and ok", changed, ok)
	}
	want := "// one\n// two\nthree"
	if got := tab.Buffer.String(); got != want {
		t.Fatalf("buffer:\n%q\nwant:\n%q", got, want)
	}
}

// TestToggleLineComment_NoSelectionUsesCursorLine makes the menu item useful
// even when the user has not highlighted text first.
func TestToggleLineComment_NoSelectionUsesCursorLine(t *testing.T) {
	tab := commentTestTab("main.go", "one\ntwo\nthree")
	tab.Cursor = Position{Line: 1, Col: 1}
	tab.Anchor = tab.Cursor

	changed, ok := tab.ToggleLineComment()

	if !ok || !changed {
		t.Fatalf("ToggleLineComment() = %v, %v; want changed and ok", changed, ok)
	}
	want := "one\n// two\nthree"
	if got := tab.Buffer.String(); got != want {
		t.Fatalf("buffer:\n%q\nwant:\n%q", got, want)
	}
}

// TestToggleLineComment_BlankSelectionIsNoop avoids adding comment markers
// to whitespace-only lines just because they were inside the selection.
func TestToggleLineComment_BlankSelectionIsNoop(t *testing.T) {
	tab := commentTestTab("main.go", "  \n\t")
	tab.SelectAll()

	changed, ok := tab.ToggleLineComment()

	if !ok {
		t.Fatal("blank Go selection should still have a known comment syntax")
	}
	if changed {
		t.Fatal("blank-only selection should not change the buffer")
	}
	if tab.Dirty || tab.CanUndo() {
		t.Fatal("blank-only selection should not dirty the tab or push undo")
	}
}

// TestToggleLineComment_UnsupportedFileTypeIsNoop protects formats like HTML
// where a line-comment marker would be wrong.
func TestToggleLineComment_UnsupportedFileTypeIsNoop(t *testing.T) {
	tab := commentTestTab("index.html", "<main></main>")

	changed, ok := tab.ToggleLineComment()

	if ok || changed {
		t.Fatalf("ToggleLineComment() = %v, %v; want unsupported noop", changed, ok)
	}
	if got := tab.Buffer.String(); got != "<main></main>" {
		t.Fatalf("buffer changed for unsupported type: %q", got)
	}
}

// TestToggleLineComment_UndoRestoresSelectionAndText confirms the action is
// one structural undo step, including the cursor and active selection.
func TestToggleLineComment_UndoRestoresSelectionAndText(t *testing.T) {
	tab := commentTestTab("main.go", "one\ntwo")
	tab.Anchor = Position{Line: 0, Col: 1}
	tab.Cursor = Position{Line: 1, Col: 2}

	changed, ok := tab.ToggleLineComment()
	if !ok || !changed {
		t.Fatalf("ToggleLineComment() = %v, %v; want changed and ok", changed, ok)
	}
	if !tab.Undo() {
		t.Fatal("Undo should restore the pre-toggle snapshot")
	}
	if got := tab.Buffer.String(); got != "one\ntwo" {
		t.Fatalf("undo buffer = %q, want original", got)
	}
	if tab.Anchor != (Position{Line: 0, Col: 1}) || tab.Cursor != (Position{Line: 1, Col: 2}) {
		t.Fatalf("undo selection = anchor %+v cursor %+v", tab.Anchor, tab.Cursor)
	}
}

// commentTestTab constructs a text tab with undo initialized, without touching
// the filesystem.
func commentTestTab(path, text string) *Tab {
	t := &Tab{
		Path:       path,
		Buffer:     NewBuffer(text),
		StyleStale: false,
	}
	t.initUndo()
	return t
}

// TestCommentBlockTokensFollowTheLanguage pins the block-comment data the
// editor had none of before this change. Python's block comment is a triple
// quote on both sides while Go's and Rust's are the C form, and a file type
// outside the table (or one with no block form at all) must answer "not
// supported" rather than a half-populated pair.
func TestCommentBlockTokensFollowTheLanguage(t *testing.T) {
	cases := []struct {
		path      string
		wantStart string
		wantEnd   string
		wantOK    bool
	}{
		{"main.go", "/*", "*/", true},
		{"lib.rs", "/*", "*/", true},
		{"main.py", `"""`, `"""`, true},
		{"app.ts", "/*", "*/", true},
		{"page.html", "<!--", "-->", true},
		{"module.wat", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		start, end, ok := BlockCommentTokens(c.path)
		if ok != c.wantOK || start != c.wantStart || end != c.wantEnd {
			t.Errorf("%q: got (%q, %q, %v), want (%q, %q, %v)",
				c.path, start, end, ok, c.wantStart, c.wantEnd, c.wantOK)
		}
	}
}

// TestToggleBlockCommentWrapsAndUnwraps proves the toggle is a true round
// trip: wrapping then unwrapping restores the buffer exactly. Anything that
// padded the delimiters with spaces would have to guess how many to eat again
// on the way out, and this assertion is what would catch the guess.
func TestToggleBlockCommentWrapsAndUnwraps(t *testing.T) {
	for _, path := range []string{"main.go", "lib.rs", "main.py"} {
		open, closer, ok := BlockCommentTokens(path)
		if !ok {
			t.Fatalf("%s should support block comments", path)
		}
		tab := &Tab{Path: path, Buffer: NewBuffer("alpha beta")}
		tab.Anchor = Position{Line: 0, Col: 0}
		tab.Cursor = Position{Line: 0, Col: 5}

		if changed, supported := tab.ToggleBlockComment(); !changed || !supported {
			t.Fatalf("%s: first toggle returned (%v, %v)", path, changed, supported)
		}
		want := open + "alpha" + closer + " beta"
		if tab.Buffer.Lines[0] != want {
			t.Fatalf("%s: got %q, want %q", path, tab.Buffer.Lines[0], want)
		}
		if tab.SelectionText() != "alpha" {
			t.Errorf("%s: the original text should stay selected, got %q", path, tab.SelectionText())
		}

		// Select the whole comment, including its delimiters, and toggle back.
		tab.Anchor = Position{Line: 0, Col: 0}
		tab.Cursor = Position{Line: 0, Col: len([]rune(want)) - len(" beta")}
		if changed, _ := tab.ToggleBlockComment(); !changed {
			t.Fatalf("%s: second toggle did not change anything", path)
		}
		if tab.Buffer.Lines[0] != "alpha beta" {
			t.Errorf("%s: round trip gave %q, want %q", path, tab.Buffer.Lines[0], "alpha beta")
		}
		if tab.SelectionText() != "alpha" {
			t.Errorf("%s: unwrapped selection = %q, want %q", path, tab.SelectionText(), "alpha")
		}
	}
}

// TestToggleBlockCommentSpansLines covers the multi-line case and, more
// importantly, that un-commenting does not renumber marks: the delimiters are
// deleted in place, so a breakpoint inside the block survives. A version that
// deleted the whole range and re-inserted the inner text would silently drop
// it.
func TestToggleBlockCommentSpansLines(t *testing.T) {
	tab := &Tab{Path: "main.go", Buffer: NewBuffer("one\ntwo\nthree")}
	tab.SetMark(1, Mark{Kind: MarkBreakpoint})
	tab.Anchor = Position{Line: 0, Col: 0}
	tab.Cursor = Position{Line: 2, Col: 5}

	if changed, ok := tab.ToggleBlockComment(); !changed || !ok {
		t.Fatalf("toggle returned (%v, %v)", changed, ok)
	}
	if got, want := tab.Buffer.String(), "/*one\ntwo\nthree*/"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if _, ok := tab.MarkAt(1); !ok {
		t.Error("the breakpoint on line 1 was lost by wrapping")
	}

	tab.Anchor = Position{Line: 0, Col: 0}
	tab.Cursor = Position{Line: 2, Col: 7}
	if changed, _ := tab.ToggleBlockComment(); !changed {
		t.Fatal("unwrap did not change anything")
	}
	if got, want := tab.Buffer.String(), "one\ntwo\nthree"; got != want {
		t.Fatalf("unwrap gave %q, want %q", got, want)
	}
	if _, ok := tab.MarkAt(1); !ok {
		t.Error("the breakpoint on line 1 was lost by unwrapping")
	}
}

// TestToggleBlockCommentPythonBareDelimiterIsNotAWrappedBlock is the guard on
// the length check in isBlockCommented. Python opens and closes with the same
// `"""`, so a selection of exactly one delimiter satisfies both HasPrefix and
// HasSuffix; treating it as a wrapped block would delete six characters out
// of three.
func TestToggleBlockCommentPythonBareDelimiterIsNotAWrappedBlock(t *testing.T) {
	tab := &Tab{Path: "main.py", Buffer: NewBuffer(`"""`)}
	tab.Anchor = Position{Line: 0, Col: 0}
	tab.Cursor = Position{Line: 0, Col: 3}
	if changed, ok := tab.ToggleBlockComment(); !changed || !ok {
		t.Fatalf("toggle returned (%v, %v)", changed, ok)
	}
	if got, want := tab.Buffer.Lines[0], `""""""""`+`"`; got != want {
		t.Fatalf("got %q, want %q (the bare delimiter should be wrapped, not unwrapped)", got, want)
	}
}

// TestToggleBlockCommentUnsupportedAndBlankAreNoops separates the two "nothing
// happened" answers: an uncovered file type is not supported at all, while a
// blank cursor line is supported but has nothing to wrap. The caller shows a
// message for the first and stays silent for the second.
func TestToggleBlockCommentUnsupportedAndBlankAreNoops(t *testing.T) {
	unsupported := &Tab{Path: "module.wat", Buffer: NewBuffer("(module)")}
	if changed, ok := unsupported.ToggleBlockComment(); changed || ok {
		t.Errorf("unsupported file type: got (%v, %v), want (false, false)", changed, ok)
	}
	if unsupported.Dirty {
		t.Error("an unsupported toggle must not dirty the buffer")
	}

	blank := &Tab{Path: "main.go", Buffer: NewBuffer("   ")}
	blank.Cursor = Position{Line: 0, Col: 1}
	blank.Anchor = blank.Cursor
	if changed, ok := blank.ToggleBlockComment(); changed || !ok {
		t.Errorf("blank line: got (%v, %v), want (false, true)", changed, ok)
	}
	if blank.Buffer.Lines[0] != "   " {
		t.Errorf("blank line was modified: %q", blank.Buffer.Lines[0])
	}
}

// TestToggleBlockCommentNoSelectionUsesTheCursorLine mirrors ToggleLineComment's
// behaviour so the two commands feel the same with nothing selected.
func TestToggleBlockCommentNoSelectionUsesTheCursorLine(t *testing.T) {
	tab := &Tab{Path: "main.go", Buffer: NewBuffer("alpha\nbeta")}
	tab.Cursor = Position{Line: 1, Col: 2}
	tab.Anchor = tab.Cursor
	if changed, ok := tab.ToggleBlockComment(); !changed || !ok {
		t.Fatalf("toggle returned (%v, %v)", changed, ok)
	}
	if got, want := tab.Buffer.String(), "alpha\n/*beta*/"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestToggleLineCommentStillUsesItsOwnTable is the no-regression guard on the
// `Esc /` path. Adding block comments must not have re-routed line comments
// through internal/langconf: lineCommentByExt covers file types the language
// table does not (.vim, .elm, .jl), and switching tables would silently drop
// them.
func TestToggleLineCommentStillUsesItsOwnTable(t *testing.T) {
	for _, path := range []string{"a.vim", "b.elm", "c.jl", "d.erl", "e.adb"} {
		if _, ok := LineCommentPrefix(path); !ok {
			t.Errorf("%s lost its line-comment marker", path)
		}
	}
	tab := &Tab{Path: "a.vim", Buffer: NewBuffer("set number")}
	if changed, ok := tab.ToggleLineComment(); !changed || !ok {
		t.Fatalf("vim toggle returned (%v, %v)", changed, ok)
	}
	if got, want := tab.Buffer.Lines[0], `" set number`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
