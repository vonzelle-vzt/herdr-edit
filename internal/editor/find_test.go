// =============================================================================
// File: internal/editor/find_test.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-04-30
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

package editor

import (
	"reflect"
	"testing"
)

// TestFindAll_BasicMatches walks across multiple lines and pins down the
// document-order ordering plus the rune-indexed Col / Width fields.
func TestFindAll_BasicMatches(t *testing.T) {
	buf := NewBuffer("foo bar foo\nbaz foo\n")
	got := FindAll(buf, "foo")
	want := []Match{
		{Line: 0, Col: 0, Width: 3},
		{Line: 0, Col: 8, Width: 3},
		{Line: 1, Col: 4, Width: 3},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FindAll mismatch:\n got=%v\nwant=%v", got, want)
	}
}

// TestFindAll_CaseInsensitive proves matching ignores letter case in
// both the query and the buffer. Without this, the "type to find" UX
// is much less forgiving than users expect from VS Code.
func TestFindAll_CaseInsensitive(t *testing.T) {
	buf := NewBuffer("Foo FOO foO")
	got := FindAll(buf, "fOo")
	if len(got) != 3 {
		t.Fatalf("expected 3 case-insensitive matches, got %d: %v", len(got), got)
	}
}

// TestFindAll_EmptyQuery returns nil so the UI can render an empty
// state without a special "0 of 0" branch.
func TestFindAll_EmptyQuery(t *testing.T) {
	buf := NewBuffer("anything")
	if got := FindAll(buf, ""); got != nil {
		t.Fatalf("empty query should return nil, got %v", got)
	}
}

// TestFindAll_NonOverlapping pins down the scanner's advance-past-match
// behaviour. "aaa" in "aaaaaa" should yield two non-overlapping hits,
// matching VS Code's default search semantics.
func TestFindAll_NonOverlapping(t *testing.T) {
	buf := NewBuffer("aaaaaa")
	got := FindAll(buf, "aaa")
	want := []Match{
		{Line: 0, Col: 0, Width: 3},
		{Line: 0, Col: 3, Width: 3},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected non-overlapping matches, got %v", got)
	}
}

// TestFindAll_MultiByteRunes pins down the rune-indexed column
// convention. The buffer contains a 3-byte UTF-8 character before the
// match — Col must report 1 (one rune in), not 3 (three bytes in).
func TestFindAll_MultiByteRunes(t *testing.T) {
	buf := NewBuffer("✓foo")
	got := FindAll(buf, "foo")
	want := []Match{{Line: 0, Col: 1, Width: 3}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("multi-byte handling wrong, got %v", got)
	}
}

// TestFindAll_NilBuffer is the defensive guard — callers may hold a
// freshly-zeroed Tab during construction. Returning nil rather than
// panicking lets the UI cope without an explicit nil check.
func TestFindAll_NilBuffer(t *testing.T) {
	if got := FindAll(nil, "x"); got != nil {
		t.Fatalf("nil buffer should return nil, got %v", got)
	}
}

// TestFirstMatchAtOrAfter_BasicForward finds the first match at or
// after the cursor, which is what we want when a user types a query
// in the bar — we shouldn't snap them backwards past where they were
// already looking.
func TestFirstMatchAtOrAfter_BasicForward(t *testing.T) {
	matches := []Match{
		{Line: 0, Col: 0, Width: 3},
		{Line: 1, Col: 4, Width: 3},
		{Line: 2, Col: 0, Width: 3},
	}
	idx := FirstMatchAtOrAfter(matches, Position{Line: 1, Col: 0})
	if idx != 1 {
		t.Fatalf("expected idx=1 (line 1 match), got %d", idx)
	}
}

// TestFirstMatchAtOrAfter_WrapsToTop covers the case where the cursor
// is past every match: we wrap to the top so the user can keep
// pressing Enter to cycle.
func TestFirstMatchAtOrAfter_WrapsToTop(t *testing.T) {
	matches := []Match{{Line: 0, Col: 0, Width: 3}}
	idx := FirstMatchAtOrAfter(matches, Position{Line: 99, Col: 0})
	if idx != 0 {
		t.Fatalf("expected wrap to idx=0, got %d", idx)
	}
}

// TestFirstMatchAtOrAfter_Empty is the no-matches case — return -1 so
// the caller can short-circuit without checking length again.
func TestFirstMatchAtOrAfter_Empty(t *testing.T) {
	if got := FirstMatchAtOrAfter(nil, Position{}); got != -1 {
		t.Fatalf("expected -1 for empty matches, got %d", got)
	}
}

// TestTab_SetFindQuery_PicksNearestMatch installs a query and pins the
// "land on the nearest hit, not always the first hit" contract: with the
// cursor on line 1, the index should point at the line-1 match, not the
// earlier line-0 one.
func TestTab_SetFindQuery_PicksNearestMatch(t *testing.T) {
	tab, _ := NewTab("")
	tab.Buffer = NewBuffer("foo\nfoo\nfoo")
	tab.Cursor = Position{Line: 1, Col: 0}

	tab.SetFindQuery("foo")
	if got, want := tab.FindIndex, 1; got != want {
		t.Fatalf("FindIndex = %d, want %d (nearest to cursor)", got, want)
	}
}

// TestTab_SetFindQuery_EmptyClears proves an empty query clears every
// piece of find state. Closing the bar relies on this behaviour to wipe
// out the highlight band.
func TestTab_SetFindQuery_EmptyClears(t *testing.T) {
	tab, _ := NewTab("")
	tab.Buffer = NewBuffer("foo")
	tab.SetFindQuery("foo")
	if tab.FindIndex < 0 {
		t.Fatal("setup expected a current match")
	}
	tab.SetFindQuery("")
	if tab.FindMatches != nil || tab.FindIndex != -1 || tab.FindQuery != "" {
		t.Fatalf("empty query should clear all find state, got %+v", tab)
	}
}

// TestTab_FindNext_WrapsAndMovesCursor exercises the Enter-in-the-bar
// path. After three Next presses we should land on match 0 again (wrap)
// with the cursor on top of it.
func TestTab_FindNext_WrapsAndMovesCursor(t *testing.T) {
	tab, _ := NewTab("")
	tab.Buffer = NewBuffer("foo\nfoo\nfoo")
	tab.SetFindQuery("foo") // FindIndex = 0
	tab.FindNext()          // -> 1
	tab.FindNext()          // -> 2
	tab.FindNext()          // -> 0 (wrap)
	if tab.FindIndex != 0 {
		t.Fatalf("expected wrap to 0, got %d", tab.FindIndex)
	}
	if tab.Cursor != (Position{Line: 0, Col: 0}) {
		t.Fatalf("cursor should follow the active match, got %+v", tab.Cursor)
	}
}

// TestTab_FindPrev_WrapsBackwards is the Shift-Enter equivalent — from
// the first match, Prev wraps to the last.
func TestTab_FindPrev_WrapsBackwards(t *testing.T) {
	tab, _ := NewTab("")
	tab.Buffer = NewBuffer("foo\nfoo\nfoo")
	tab.SetFindQuery("foo")
	tab.FindPrev()
	if tab.FindIndex != 2 {
		t.Fatalf("expected wrap to last (2), got %d", tab.FindIndex)
	}
}

// TestTab_FindNext_NoMatchesIsSafe pins the contract that Find ops are
// no-ops when there's nothing to find. Without this, a stray hotkey on
// an empty result set would crash.
func TestTab_FindNext_NoMatchesIsSafe(t *testing.T) {
	tab, _ := NewTab("")
	tab.Buffer = NewBuffer("hello world")
	tab.SetFindQuery("zzz")
	tab.FindNext() // must not panic
	tab.FindPrev() // must not panic
	if tab.FindIndex != -1 {
		t.Fatalf("FindIndex should stay -1 with no matches, got %d", tab.FindIndex)
	}
}

// TestTab_ClearFind wipes everything so the renderer stops highlighting.
func TestTab_ClearFind(t *testing.T) {
	tab, _ := NewTab("")
	tab.Buffer = NewBuffer("foo")
	tab.SetFindQuery("foo")
	tab.ClearFind()
	if tab.FindQuery != "" || tab.FindMatches != nil || tab.FindIndex != -1 {
		t.Fatalf("ClearFind left residue: %+v", tab)
	}
}

// TestMatches_CaseSensitive proves the CaseSensitive toggle actually
// restricts matching — without it every FindOptions test below could pass
// vacuously against the always-case-insensitive FindAll path.
func TestMatches_CaseSensitive(t *testing.T) {
	buf := NewBuffer("Foo foo FOO")
	got, err := Matches(buf, "foo", FindOptions{CaseSensitive: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].Col != 4 {
		t.Fatalf("expected exactly one case-sensitive hit at col 4, got %v", got)
	}
}

// TestMatches_WholeWord proves whole-word matching rejects a hit that's
// embedded inside a longer identifier ("foobar" containing "foo") while
// still accepting a standalone occurrence.
func TestMatches_WholeWord(t *testing.T) {
	buf := NewBuffer("foobar foo_bar foo bar")
	got, err := Matches(buf, "foo", FindOptions{WholeWord: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []Match{{Line: 0, Col: 15, Width: 3}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("whole-word mismatch: got=%v want=%v", got, want)
	}
}

// TestMatches_WholeWord_CaseInsensitiveByDefault proves the two toggles
// are independent: whole-word alone should still match case-insensitively.
func TestMatches_WholeWord_CaseInsensitiveByDefault(t *testing.T) {
	buf := NewBuffer("FOO bar")
	got, err := Matches(buf, "foo", FindOptions{WholeWord: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 case-insensitive whole-word hit, got %v", got)
	}
}

// TestMatches_Regex_Basic pins down that a regex pattern matches
// literally as a regular expression (not as a literal substring), and
// that byte offsets from the stdlib matcher get converted to the same
// rune-indexed Col/Width contract as every other match source.
func TestMatches_Regex_Basic(t *testing.T) {
	buf := NewBuffer("foo1 foo22 foo333")
	got, err := Matches(buf, `foo\d+`, FindOptions{Regex: true, CaseSensitive: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []Match{
		{Line: 0, Col: 0, Width: 4},
		{Line: 0, Col: 5, Width: 5},
		{Line: 0, Col: 11, Width: 6},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("regex mismatch: got=%v want=%v", got, want)
	}
}

// TestMatches_Regex_CaseInsensitiveDefault proves the CaseSensitive
// toggle also governs regex matching (via the injected (?i) flag), not
// just the plain-substring path.
func TestMatches_Regex_CaseInsensitiveDefault(t *testing.T) {
	buf := NewBuffer("FOO foo")
	got, err := Matches(buf, `foo`, FindOptions{Regex: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 case-insensitive regex hits, got %v", got)
	}
}

// TestMatches_Regex_InvalidPatternReturnsUsableError is the correctness
// bar from the brief: an invalid regex must return an error the caller
// can display, not panic and not silently report zero matches (which
// would look like the pattern legitimately didn't match anything).
func TestMatches_Regex_InvalidPatternReturnsUsableError(t *testing.T) {
	buf := NewBuffer("anything")
	got, err := Matches(buf, `(unclosed`, FindOptions{Regex: true})
	if err == nil {
		t.Fatal("expected an error for an invalid regex pattern")
	}
	if got != nil {
		t.Fatalf("expected nil matches alongside the error, got %v", got)
	}
	if err.Error() == "" {
		t.Fatal("error message should not be empty")
	}
}

// TestMatches_Regex_MultiByteRunes pins down UTF-8 safety for the regex
// path specifically, since it converts *byte* offsets from the stdlib
// matcher back to rune columns — getting that wrong corrupts every
// Position downstream (cursor placement, replace ranges, ...).
func TestMatches_Regex_MultiByteRunes(t *testing.T) {
	buf := NewBuffer("✓✓foo")
	got, err := Matches(buf, `foo`, FindOptions{Regex: true, CaseSensitive: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []Match{{Line: 0, Col: 2, Width: 3}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("multi-byte regex handling wrong, got %v", got)
	}
}

// TestMatches_Regex_ZeroWidthSkipped proves a pattern that can match zero
// runes (like "a*" against a line with no "a") is dropped rather than
// producing a Match the UI could never usefully jump to or replace, and
// which FindNext could loop on forever without advancing.
func TestMatches_Regex_ZeroWidthSkipped(t *testing.T) {
	buf := NewBuffer("bbb")
	got, err := Matches(buf, `a*`, FindOptions{Regex: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected zero-width matches to be filtered out, got %v", got)
	}
}

// TestTab_SetFindOptions_RecomputesImmediately proves flipping a toggle
// takes effect right away against the existing query, without the caller
// having to re-set the query string.
func TestTab_SetFindOptions_RecomputesImmediately(t *testing.T) {
	tab, _ := NewTab("")
	tab.Buffer = NewBuffer("Foo foo")
	tab.SetFindQuery("foo")
	if len(tab.FindMatches) != 2 {
		t.Fatalf("setup: expected 2 case-insensitive matches, got %d", len(tab.FindMatches))
	}
	tab.SetFindOptions(true, false, false)
	if len(tab.FindMatches) != 1 {
		t.Fatalf("expected 1 case-sensitive match after toggling, got %d", len(tab.FindMatches))
	}
}

// TestTab_SetFindQuery_RegexErrorClearsMatchesAndSetsFindErr proves an
// invalid regex query surfaces through FindErr instead of the tab quietly
// reporting "no matches" for a query the user just mistyped.
func TestTab_SetFindQuery_RegexErrorClearsMatchesAndSetsFindErr(t *testing.T) {
	tab, _ := NewTab("")
	tab.Buffer = NewBuffer("anything")
	tab.SetFindOptions(false, false, true)
	tab.SetFindQuery(`(unclosed`)
	if tab.FindErr == nil {
		t.Fatal("expected FindErr to be set for an invalid regex")
	}
	if tab.FindMatches != nil || tab.FindIndex != -1 {
		t.Fatalf("expected matches cleared on regex error, got matches=%v index=%d", tab.FindMatches, tab.FindIndex)
	}
}

// TestTab_Replace_ReplacesCurrentMatchAndAdvances proves Replace swaps
// the focused match's text and moves on to the next match, mirroring the
// "replace and jump to the next one" flow of a find/replace bar.
func TestTab_Replace_ReplacesCurrentMatchAndAdvances(t *testing.T) {
	tab, _ := NewTab("")
	tab.Buffer = NewBuffer("foo foo foo")
	tab.SetFindQuery("foo") // FindIndex = 0, at col 0

	if ok := tab.Replace("bar"); !ok {
		t.Fatal("Replace should report success on a valid current match")
	}
	if tab.Buffer.Lines[0] != "bar foo foo" {
		t.Fatalf("got %q", tab.Buffer.Lines[0])
	}
	// The match list was recomputed against the mutated buffer; the
	// remaining two "foo"s should still be found.
	if len(tab.FindMatches) != 2 {
		t.Fatalf("expected 2 remaining matches, got %d: %v", len(tab.FindMatches), tab.FindMatches)
	}
}

// TestTab_Replace_NoCurrentMatchIsNoOp proves Replace is safe to call
// with no active query — a stray hotkey shouldn't panic or corrupt state.
func TestTab_Replace_NoCurrentMatchIsNoOp(t *testing.T) {
	tab, _ := NewTab("")
	tab.Buffer = NewBuffer("hello")
	if tab.Replace("x") {
		t.Fatal("Replace should report false with no current match")
	}
	if tab.Buffer.Lines[0] != "hello" {
		t.Fatalf("buffer should be untouched, got %q", tab.Buffer.Lines[0])
	}
}

// TestTab_ReplaceAll_ReplacesEveryMatch proves ReplaceAll swaps every
// match, including on multiple lines, and correctness holds when
// replacement text is longer or shorter than the match (so a naive
// fixed-offset implementation would misplace later matches on the same
// line).
func TestTab_ReplaceAll_ReplacesEveryMatch(t *testing.T) {
	tab, _ := NewTab("")
	tab.Buffer = NewBuffer("foo bar foo\nfoo baz")
	tab.SetFindQuery("foo")

	n := tab.ReplaceAll("quux")
	if n != 3 {
		t.Fatalf("expected 3 replacements, got %d", n)
	}
	want := "quux bar quux\nquux baz"
	if tab.Buffer.String() != want {
		t.Fatalf("got %q, want %q", tab.Buffer.String(), want)
	}
}

// TestTab_ReplaceAll_ReplacementContainingQueryDoesNotReMatch is the
// explicit infinite-rematch guard from the brief: replacing "foo" with
// "foofoo" must not cause the newly inserted text to be picked up as
// additional matches in the same pass.
func TestTab_ReplaceAll_ReplacementContainingQueryDoesNotReMatch(t *testing.T) {
	tab, _ := NewTab("")
	tab.Buffer = NewBuffer("foo foo")
	tab.SetFindQuery("foo")

	n := tab.ReplaceAll("foofoo")
	if n != 2 {
		t.Fatalf("expected exactly 2 replacements (the original matches), got %d", n)
	}
	want := "foofoo foofoo"
	if tab.Buffer.String() != want {
		t.Fatalf("got %q, want %q", tab.Buffer.String(), want)
	}
}

// TestTab_ReplaceAll_IsOneUndoStep is the coalescing requirement from the
// brief: undoing a ReplaceAll of N matches must be a single Undo call,
// not N of them, and that one Undo must fully restore the pre-replace
// buffer.
func TestTab_ReplaceAll_IsOneUndoStep(t *testing.T) {
	tab, _ := NewTab("")
	tab.Buffer = NewBuffer("foo bar foo baz foo")
	tab.initUndo() // establish a clean baseline, same as NewTab would
	tab.SetFindQuery("foo")

	before := tab.Buffer.String()
	n := tab.ReplaceAll("X")
	if n != 3 {
		t.Fatalf("expected 3 replacements, got %d", n)
	}
	if !tab.CanUndo() {
		t.Fatal("ReplaceAll should have pushed an undo entry")
	}
	if len(tab.undoStack) != 1 {
		t.Fatalf("ReplaceAll should be exactly one undo entry, got %d", len(tab.undoStack))
	}
	if !tab.Undo() {
		t.Fatal("Undo should succeed")
	}
	if tab.Buffer.String() != before {
		t.Fatalf("single Undo did not fully restore the pre-ReplaceAll buffer: got %q, want %q", tab.Buffer.String(), before)
	}
	if tab.CanUndo() {
		t.Fatal("expected no further undo history after the one ReplaceAll step")
	}
}

// TestTab_ReplaceAll_MultiByteSafe proves ReplaceAll's rune-indexed
// positions stay correct across a replacement that changes a multi-byte
// line's rune-count, both before and after the edited match on the same
// line.
func TestTab_ReplaceAll_MultiByteSafe(t *testing.T) {
	tab, _ := NewTab("")
	tab.Buffer = NewBuffer("✓foo✓foo")
	tab.SetFindQuery("foo")

	n := tab.ReplaceAll("π")
	if n != 2 {
		t.Fatalf("expected 2 replacements, got %d", n)
	}
	want := "✓π✓π"
	if tab.Buffer.String() != want {
		t.Fatalf("got %q, want %q", tab.Buffer.String(), want)
	}
}

// TestTab_ReplaceAll_NoMatchesIsNoOp proves ReplaceAll is safe to call
// with an empty match list.
func TestTab_ReplaceAll_NoMatchesIsNoOp(t *testing.T) {
	tab, _ := NewTab("")
	tab.Buffer = NewBuffer("hello")
	if n := tab.ReplaceAll("x"); n != 0 {
		t.Fatalf("expected 0 replacements, got %d", n)
	}
	if tab.CanUndo() {
		t.Fatal("a no-op ReplaceAll should not push an undo entry")
	}
}

// TestMatchAtRune_HitAndMiss proves the per-cell renderer probe finds
// the right match index for cells inside a hit and -1 outside.
func TestMatchAtRune_HitAndMiss(t *testing.T) {
	tab, _ := NewTab("")
	tab.Buffer = NewBuffer("foo bar foo")
	tab.SetFindQuery("foo") // matches at (0,0) and (0,8)

	if got := tab.matchAtRune(0, 1); got != 0 {
		t.Fatalf("col 1 should be inside match 0, got %d", got)
	}
	if got := tab.matchAtRune(0, 4); got != -1 {
		t.Fatalf("col 4 (the space) should miss, got %d", got)
	}
	if got := tab.matchAtRune(0, 9); got != 1 {
		t.Fatalf("col 9 should be inside match 1, got %d", got)
	}
}
