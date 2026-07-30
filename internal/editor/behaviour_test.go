// =============================================================================
// File: internal/editor/behaviour_test.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-07-29
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

// Behavioural checks for the editing features, written against the public API rather than the
// implementation, so a refactor that keeps the behaviour keeps these passing.
package editor

import "testing"

// TestReplaceAllIsOneUndo — the brief's hardest requirement.
func TestReplaceAllIsOneUndo(t *testing.T) {
	b := NewBuffer("foo a foo b foo c\nfoo d\n")
	tb := &Tab{Buffer: b}
	tb.SetFindQuery("foo")
	n := tb.ReplaceAll("bar")
	if n != 4 {
		t.Fatalf("replaced %d, want 4", n)
	}
	got := tb.Buffer.String()
	if got != "bar a bar b bar c\nbar d\n" {
		t.Fatalf("after: %q", got)
	}
	tb.Undo()
	if after := tb.Buffer.String(); after != "foo a foo b foo c\nfoo d\n" {
		t.Fatalf("ONE undo must restore all 4: %q", after)
	}
}

// TestReplacementContainingQueryTerminates — the infinite-loop trap.
func TestReplacementContainingQueryTerminates(t *testing.T) {
	tb := &Tab{Buffer: NewBuffer("a a a\n")}
	tb.SetFindQuery("a")
	if n := tb.ReplaceAll("aa"); n != 3 {
		t.Fatalf("replaced %d, want 3", n)
	}
	if got := tb.Buffer.String(); got != "aa aa aa\n" {
		t.Fatalf("got %q", got)
	}
}

// TestUnicodeReplaceDoesNotCorrupt — Position is RUNE indexed.
func TestUnicodeReplaceDoesNotCorrupt(t *testing.T) {
	tb := &Tab{Buffer: NewBuffer("日本語 target 日本語\n")}
	tb.SetFindQuery("target")
	tb.ReplaceAll("置換")
	if got := tb.Buffer.String(); got != "日本語 置換 日本語\n" {
		t.Fatalf("corrupted: %q", got)
	}
}

// TestApostropheNotAutoClosed — don't -> don”t would be maddening.
func TestApostropheNotAutoClosed(t *testing.T) {
	tb := &Tab{Buffer: NewBuffer("don")}
	// Anchor must equal Cursor or the tab reads as having a selection, and typing a quote then
	// correctly SURROUNDS it — which is a different feature, not the one under test here.
	tb.Cursor = Position{Line: 0, Col: 3}
	tb.Anchor = tb.Cursor
	tb.InsertRune('\'')
	if got := tb.Buffer.Lines[0]; got != "don'" {
		t.Fatalf("apostrophe after a word char must not auto-close, got %q", got)
	}
}

// TestBracketAutoCloseAndStepOver
func TestBracketAutoCloseAndStepOver(t *testing.T) {
	tb := &Tab{Buffer: NewBuffer("")}
	tb.InsertRune('(')
	if got := tb.Buffer.Lines[0]; got != "()" {
		t.Fatalf("auto-close: %q", got)
	}
	if tb.Cursor.Col != 1 {
		t.Fatalf("cursor should sit between the pair, col=%d", tb.Cursor.Col)
	}
	tb.InsertRune(')')
	if got := tb.Buffer.Lines[0]; got != "()" {
		t.Fatalf("typing the closer must step over, got %q", got)
	}
	if tb.Cursor.Col != 2 {
		t.Fatalf("cursor col=%d, want 2", tb.Cursor.Col)
	}
}

// TestSurroundSelection — typing an opener with a selection must wrap it, not replace it.
func TestSurroundSelection(t *testing.T) {
	tb := &Tab{Buffer: NewBuffer("hello world")}
	tb.Anchor = Position{Line: 0, Col: 0}
	tb.Cursor = Position{Line: 0, Col: 5}
	tb.InsertRune('(')
	if got := tb.Buffer.Lines[0]; got != "(hello) world" {
		t.Fatalf("selection should be surrounded, got %q", got)
	}
}
