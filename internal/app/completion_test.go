// =============================================================================
// File: internal/app/completion_test.go
// Author: Vonzelle Brown
// Created: 2026-07-30
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package app

import (
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/cloudmanic/spice-edit/internal/editor"
	"github.com/cloudmanic/spice-edit/internal/lsp"
)

// posAt is a tiny constructor so the tests read as line/col rather than as
// struct literals.
func posAt(line, col int) editor.Position {
	return editor.Position{Line: line, Col: col}
}

// TestWordPrefixAt covers what the popup filters on and what a completion
// replaces. Getting this wrong duplicates the letters the user already typed.
func TestWordPrefixAt(t *testing.T) {
	cases := []struct {
		line string
		col  int
		want string
	}{
		{"foo.bar", 7, "bar"},
		{"foo.bar", 3, "foo"},
		{"foo.bar", 4, ""},
		{"  indented", 10, "indented"},
		{"snake_case_1", 12, "snake_case_1"},
		{"", 0, ""},
		{"abc", 99, "abc"},
	}
	for _, c := range cases {
		if got := wordPrefixAt(c.line, c.col); got != c.want {
			t.Errorf("wordPrefixAt(%q, %d) = %q, want %q", c.line, c.col, got, c.want)
		}
	}
}

// TestFilterCompletions pins client-side filtering. Servers routinely return
// the whole symbol table and expect the client to narrow it; without this the
// first suggestion after typing "fo" can be something starting with "a".
func TestFilterCompletions(t *testing.T) {
	items := []lsp.CompletionItem{
		{Label: "format"}, {Label: "Foo"}, {Label: "abs"}, {Label: "fold"},
	}
	got := filterCompletions(items, "fo")
	if len(got) != 3 {
		t.Fatalf("got %d items, want 3 (format, Foo, fold)", len(got))
	}
	for _, it := range got {
		if it.Label == "abs" {
			t.Fatal("a non-matching item survived the filter")
		}
	}
	if len(filterCompletions(items, "")) != len(items) {
		t.Error("an empty prefix should keep every item")
	}
}

// TestCompletionItem_TextPrefersInsertText pins that the popup inserts the
// server's insertText when it differs from the label. Labels are decorated for
// display — "foo(…)" — and inserting one puts punctuation into the user's code.
func TestCompletionItem_TextPrefersInsertText(t *testing.T) {
	if got := (lsp.CompletionItem{Label: "foo(…)", InsertText: "foo"}).Text(); got != "foo" {
		t.Errorf("Text() = %q, want the insertText", got)
	}
	if got := (lsp.CompletionItem{Label: "bar"}).Text(); got != "bar" {
		t.Errorf("Text() = %q, want the label as fallback", got)
	}
}

// TestApplyCompletion_ReplacesThePrefix is the end-to-end behaviour: the typed
// prefix is consumed, not appended to.
func TestApplyCompletion_ReplacesThePrefix(t *testing.T) {
	a := seedNavApp(t, "fo\n")
	tab := a.activeTabPtr()
	tab.MoveCursorTo(tab.Buffer.Clamp(tab.Cursor), false)
	tab.MoveCursorTo(posAt(0, 2), false)

	a.completionOpen = true
	a.completionPrefix = "fo"
	a.completionItems = []lsp.CompletionItem{{Label: "format"}}
	a.completionSelected = 0
	a.applyCompletion()

	if got := tab.Buffer.Lines[0]; got != "format" {
		t.Fatalf("line = %q, want %q — the prefix was not replaced", got, "format")
	}
	if a.completionOpen {
		t.Error("applying should close the popup")
	}
}

// TestHandleCompletionKey_DoesNotSwallowKeystrokes is the contract that matters
// most. A popup that eats a keypress silently discards the user's typing; only
// the keys it genuinely owns may be consumed.
func TestHandleCompletionKey_DoesNotSwallowKeystrokes(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.completionOpen = true
	a.completionItems = []lsp.CompletionItem{{Label: "alpha"}, {Label: "beta"}}

	for _, k := range []tcell.Key{tcell.KeyUp, tcell.KeyDown, tcell.KeyEsc} {
		a.completionOpen = true
		if !a.handleCompletionKey(keyEv(k, 0)) {
			t.Errorf("key %v should have been consumed by the popup", k)
		}
	}

	a.completionOpen = true
	a.completionItems = []lsp.CompletionItem{{Label: "alpha"}}
	if a.handleCompletionKey(keyEv(tcell.KeyRune, 'x')) {
		t.Fatal("a printable rune must NOT be consumed — that swallows the keystroke")
	}
	if a.completionOpen {
		t.Error("a printable rune should have dismissed the popup")
	}
}

// TestHandleCompletion_IgnoresStaleAnswers pins that a response arriving after
// the cursor moved is dropped. Inserting text where the user is no longer
// looking is worse than showing nothing.
func TestHandleCompletion_IgnoresStaleAnswers(t *testing.T) {
	a := seedNavApp(t, "one\ntwo\n")
	tab := a.activeTabPtr()
	tab.MoveCursorTo(posAt(1, 0), false)

	a.handleCompletion(&completionEvent{
		items:  []lsp.CompletionItem{{Label: "stale"}},
		prefix: "",
		at:     posAt(0, 0), // where the cursor WAS
		when:   time.Now(),
	})
	if a.completionOpen {
		t.Fatal("a completion for a stale cursor position opened the popup")
	}
}

// TestCompletion_Reachable guards the failure this fork has now hit three
// times: a complete, tested request with no caller.
func TestCompletion_Reachable(t *testing.T) {
	// c stays UNBOUND: CLAUDE.md reserves c/x/v so the host terminal's own
	// clipboard is the only clipboard channel. Completion lives on SPACE, which
	// is also the muscle memory from VS Code's Ctrl+Space.
	if leaderActionFor(' ') == nil {
		t.Fatal("Esc space is not bound — completion is unreachable")
	}
	if leaderActionFor('c') != nil {
		t.Fatal("c must stay unbound — it is reserved for the terminal's clipboard")
	}
	a := newTestApp(t, t.TempDir())
	items, _, _ := a.menuLayout()
	for _, it := range items {
		if it.label == "Complete at cursor" {
			return
		}
	}
	t.Error("completion has no menu row — unreachable by mouse")
}
