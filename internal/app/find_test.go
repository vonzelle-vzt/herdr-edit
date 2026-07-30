// =============================================================================
// File: internal/app/find_test.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-04-30
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/cloudmanic/spice-edit/internal/editor"
)

// seedFindApp opens a tab with content seeded for find tests so each
// test can focus on the behaviour under test rather than fixture setup.
func seedFindApp(t *testing.T, content string) *App {
	t.Helper()
	dir := t.TempDir()
	target := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(target, []byte(content), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	a := newTestApp(t, dir)
	a.openFile(target)
	return a
}

// TestOpenFind_OpensBarEmpty drops the user into a focused find bar
// with an empty input. Pre-fill from a prior query is intentionally
// not done — closing the bar already clears find state, so each Esc-f
// is a fresh search.
func TestOpenFind_OpensBarEmpty(t *testing.T) {
	a := seedFindApp(t, "foo bar foo")
	a.openFind()
	if !a.findOpen {
		t.Fatal("openFind did not flip findOpen")
	}
	if len(a.findValue) != 0 {
		t.Fatalf("input should be empty, got %q", string(a.findValue))
	}
}

// TestOpenFind_NoTabIsNoOp guards against opening the bar when there's
// no text tab to search. Without this, the bar would float over an
// empty editor with nothing to highlight.
func TestOpenFind_NoTabIsNoOp(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.openFind()
	if a.findOpen {
		t.Fatal("openFind should be a no-op with no tab")
	}
}

// TestHandleFindKey_TypingLiveSearches drives the per-keystroke handler
// the way a user would: type "foo", and the active tab's match list
// should be populated and the cursor should sit on the first match.
func TestHandleFindKey_TypingLiveSearches(t *testing.T) {
	a := seedFindApp(t, "foo bar foo")
	a.openFind()
	for _, r := range "foo" {
		a.handleFindKey(keyEv(tcell.KeyRune, r))
	}
	tab := a.activeTabPtr()
	if len(tab.FindMatches) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(tab.FindMatches))
	}
	if tab.Cursor != (editor.Position{Line: 0, Col: 0}) {
		t.Fatalf("cursor should snap to first match, got %+v", tab.Cursor)
	}
}

// TestHandleFindKey_EnterAdvances simulates Enter inside the bar — it
// should jump to the next match, with wrap-around.
func TestHandleFindKey_EnterAdvances(t *testing.T) {
	a := seedFindApp(t, "foo\nfoo\nfoo")
	a.openFind()
	for _, r := range "foo" {
		a.handleFindKey(keyEv(tcell.KeyRune, r))
	}
	tab := a.activeTabPtr()
	a.handleFindKey(keyEv(tcell.KeyEnter, 0))
	if tab.FindIndex != 1 {
		t.Fatalf("expected FindIndex=1 after Enter, got %d", tab.FindIndex)
	}
	if tab.Cursor.Line != 1 {
		t.Fatalf("cursor should be on line 1, got %+v", tab.Cursor)
	}
}

// TestHandleFindKey_ShiftEnterGoesBack pins down the Shift-Enter -> prev
// behaviour. Enter then Shift-Enter from the first match should leave
// us back at the first match.
func TestHandleFindKey_ShiftEnterGoesBack(t *testing.T) {
	a := seedFindApp(t, "foo\nfoo\nfoo")
	a.openFind()
	for _, r := range "foo" {
		a.handleFindKey(keyEv(tcell.KeyRune, r))
	}
	a.handleFindKey(keyEv(tcell.KeyEnter, 0))
	// Shift+Enter — keyEv default is ModNone, so build it directly.
	a.handleFindKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModShift))
	if a.activeTabPtr().FindIndex != 0 {
		t.Fatalf("Shift-Enter should walk back, got idx=%d", a.activeTabPtr().FindIndex)
	}
}

// TestHandleFindKey_EscClearsHighlights pins the close gesture: Esc
// closes the bar AND wipes the tab's match list so the highlights
// disappear with the UI. Leaving them painted after the bar closes is
// the kind of "did anything happen?" surprise we want to avoid.
func TestHandleFindKey_EscClearsHighlights(t *testing.T) {
	a := seedFindApp(t, "foo bar foo")
	a.openFind()
	for _, r := range "foo" {
		a.handleFindKey(keyEv(tcell.KeyRune, r))
	}
	a.handleFindKey(keyEv(tcell.KeyEsc, 0))
	if a.findOpen {
		t.Fatal("Esc should close the find bar")
	}
	tab := a.activeTabPtr()
	if tab.FindQuery != "" || tab.FindMatches != nil || tab.FindIndex != -1 {
		t.Fatalf("Esc should clear all find state, got %+v", tab)
	}
}

// TestHandleFindKey_BackspaceLiveUpdates removes a character from the
// input and confirms matches re-resolve. Without this, deleting the
// query would leave stale highlights painted in the editor.
func TestHandleFindKey_BackspaceLiveUpdates(t *testing.T) {
	a := seedFindApp(t, "foo bar foox")
	a.openFind()
	for _, r := range "foox" {
		a.handleFindKey(keyEv(tcell.KeyRune, r))
	}
	tab := a.activeTabPtr()
	if len(tab.FindMatches) != 1 {
		t.Fatalf("setup expected 1 match for 'foox', got %d", len(tab.FindMatches))
	}
	a.handleFindKey(keyEv(tcell.KeyBackspace, 0))
	if len(tab.FindMatches) != 2 {
		t.Fatalf("after backspace should match 'foo' (2x), got %d", len(tab.FindMatches))
	}
}

// TestEditorRect_ShrinksWhenFindOpen pins down the layout contract: the
// editor body is one row shorter while the find bar is up. Without this
// the bar would paint over the bottom row of code.
func TestEditorRect_ShrinksWhenFindOpen(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	_, _, _, hClosed := a.editorRect()
	a.findOpen = true
	_, _, _, hOpen := a.editorRect()
	if hOpen != hClosed-findBarHeight {
		t.Fatalf("editor height didn't shrink: closed=%d open=%d", hClosed, hOpen)
	}
}

// TestHasFindable_ImageTabIsFalse keeps the menu's Find row disabled on
// image tabs — there's nothing to search inside an image.
func TestHasFindable_ImageTabIsFalse(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	if a.hasFindable() {
		t.Fatal("no tab should not be findable")
	}
}

// TestCounterText_Variants pins the three rendered states of the
// counter so a future refactor can't quietly drop "no results" or the
// blank no-query state.
func TestCounterText_Variants(t *testing.T) {
	a := seedFindApp(t, "foo bar foo")
	a.openFind()
	if got := a.findCounterText(); got != "" {
		t.Fatalf("empty input should yield blank counter, got %q", got)
	}
	for _, r := range "foo" {
		a.handleFindKey(keyEv(tcell.KeyRune, r))
	}
	if got := a.findCounterText(); got != "1 of 2" {
		t.Fatalf("counter for 2 matches should be '1 of 2', got %q", got)
	}
	for _, r := range "zzz" {
		a.handleFindKey(keyEv(tcell.KeyRune, r))
	}
	if got := a.findCounterText(); got != "no results" {
		t.Fatalf("zero hits should yield 'no results', got %q", got)
	}
}

// TestCloseAllModals_ClosesFindBar guards against a regression where
// opening another modal could leave the find bar focused underneath.
func TestCloseAllModals_ClosesFindBar(t *testing.T) {
	a := seedFindApp(t, "foo")
	a.openFind()
	a.closeAllModals()
	if a.findOpen {
		t.Fatal("closeAllModals should close the find bar")
	}
}

// TestHandleFindKey_TabOpensAndFocusesReplace pins the keyboard path into
// the replace field. Tab from a find-only bar expands the replace row and
// moves the caret there in one gesture, so the feature is reachable
// without the mouse on terminals that swallow Alt.
func TestHandleFindKey_TabOpensAndFocusesReplace(t *testing.T) {
	a := seedFindApp(t, "foo bar foo")
	a.openFind()
	if a.findReplaceOpen {
		t.Fatal("replace row should start collapsed")
	}
	a.handleFindKey(keyEv(tcell.KeyTab, 0))
	if !a.findReplaceOpen {
		t.Fatal("Tab should expand the replace row")
	}
	if a.findFocus != findFieldReplace {
		t.Fatal("Tab should move the caret into the replace field")
	}
	// A second Tab swaps back to the query field rather than collapsing.
	a.handleFindKey(keyEv(tcell.KeyTab, 0))
	if a.findFocus != findFieldQuery {
		t.Fatal("second Tab should return focus to the query field")
	}
	if !a.findReplaceOpen {
		t.Fatal("second Tab must not collapse the row")
	}
}

// TestHandleFindKey_ReplaceCurrentAdvances is the end-to-end oracle for the
// replace feature: drive it entirely through the key handler (the way a
// user does) and assert the buffer actually changed. The engine had a
// complete, tested Replace with no UI caller at all; this test fails
// against that state.
func TestHandleFindKey_ReplaceCurrentAdvances(t *testing.T) {
	a := seedFindApp(t, "foo bar foo")
	a.openFind()
	for _, r := range "foo" {
		a.handleFindKey(keyEv(tcell.KeyRune, r))
	}
	a.handleFindKey(keyEv(tcell.KeyTab, 0))
	for _, r := range "baz" {
		a.handleFindKey(keyEv(tcell.KeyRune, r))
	}
	a.handleFindKey(keyEv(tcell.KeyEnter, 0))

	got := a.activeTabPtr().Buffer.Lines[0]
	if got != "baz bar foo" {
		t.Fatalf("after one replace, line = %q, want %q", got, "baz bar foo")
	}
	if !a.activeTabPtr().Dirty {
		t.Error("replacing should mark the tab dirty")
	}
}

// TestHandleFindKey_ReplaceAllOneUndo checks Shift+Enter in the replace
// field rewrites every match, and that the whole pass is a single undo
// step rather than one per replacement.
func TestHandleFindKey_ReplaceAllOneUndo(t *testing.T) {
	a := seedFindApp(t, "foo bar foo baz foo")
	a.openFind()
	for _, r := range "foo" {
		a.handleFindKey(keyEv(tcell.KeyRune, r))
	}
	a.handleFindKey(keyEv(tcell.KeyTab, 0))
	for _, r := range "qux" {
		a.handleFindKey(keyEv(tcell.KeyRune, r))
	}
	a.handleFindKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModShift))

	if got := a.activeTabPtr().Buffer.Lines[0]; got != "qux bar qux baz qux" {
		t.Fatalf("replace all produced %q", got)
	}
	a.activeTabPtr().Undo()
	if got := a.activeTabPtr().Buffer.Lines[0]; got != "foo bar foo baz foo" {
		t.Fatalf("one undo should restore the whole pass, got %q", got)
	}
}

// TestHandleFindKey_ReplaceFieldDoesNotDisturbMatches pins that typing a
// replacement never re-runs the search. Recomputing on every replace-field
// keystroke would move the highlights out from under a user who is
// mid-edit.
func TestHandleFindKey_ReplaceFieldDoesNotDisturbMatches(t *testing.T) {
	a := seedFindApp(t, "foo bar foo")
	a.openFind()
	for _, r := range "foo" {
		a.handleFindKey(keyEv(tcell.KeyRune, r))
	}
	before := len(a.activeTabPtr().FindMatches)
	a.handleFindKey(keyEv(tcell.KeyTab, 0))
	for _, r := range "zzzz" {
		a.handleFindKey(keyEv(tcell.KeyRune, r))
	}
	if got := len(a.activeTabPtr().FindMatches); got != before {
		t.Fatalf("match count changed while typing a replacement: %d -> %d", before, got)
	}
	if a.activeTabPtr().FindQuery != "foo" {
		t.Fatalf("query changed to %q while typing a replacement", a.activeTabPtr().FindQuery)
	}
}

// TestHandleFindKey_AltTogglesOptions covers the three search-option
// toggles on their VS Code shortcuts, and that flipping one re-runs the
// query immediately instead of waiting for the next keystroke.
func TestHandleFindKey_AltTogglesOptions(t *testing.T) {
	a := seedFindApp(t, "Foo foo FOO")
	a.openFind()
	for _, r := range "foo" {
		a.handleFindKey(keyEv(tcell.KeyRune, r))
	}
	if got := len(a.activeTabPtr().FindMatches); got != 3 {
		t.Fatalf("case-insensitive should match 3, got %d", got)
	}
	a.handleFindKey(tcell.NewEventKey(tcell.KeyRune, 'c', tcell.ModAlt))
	if !a.activeTabPtr().FindCaseSensitive {
		t.Fatal("Alt+c should enable case sensitivity")
	}
	if got := len(a.activeTabPtr().FindMatches); got != 1 {
		t.Fatalf("case-sensitive should match 1, got %d", got)
	}
	a.handleFindKey(tcell.NewEventKey(tcell.KeyRune, 'r', tcell.ModAlt))
	if !a.activeTabPtr().FindRegex {
		t.Fatal("Alt+r should enable regex")
	}
	a.handleFindKey(tcell.NewEventKey(tcell.KeyRune, 'w', tcell.ModAlt))
	if !a.activeTabPtr().FindWholeWord {
		t.Fatal("Alt+w should enable whole-word")
	}
	// The Alt runes must never reach the input.
	if string(a.findValue) != "foo" {
		t.Fatalf("Alt-modified runes leaked into the query: %q", string(a.findValue))
	}
}

// TestFindStatusText_SurfacesRegexError is the fix for a silent failure:
// an unparseable pattern clears the match list, so without this the bar
// reads "no results" — indistinguishable from a valid pattern that
// genuinely matches nothing.
func TestFindStatusText_SurfacesRegexError(t *testing.T) {
	a := seedFindApp(t, "foo bar")
	a.openFind()
	a.handleFindKey(tcell.NewEventKey(tcell.KeyRune, 'r', tcell.ModAlt))
	for _, r := range "foo(" {
		a.handleFindKey(keyEv(tcell.KeyRune, r))
	}
	if a.activeTabPtr().FindErr == nil {
		t.Fatal("an unclosed group should set FindErr")
	}
	if got := a.findStatusText(); got != "bad pattern" {
		t.Fatalf("status = %q, want %q", got, "bad pattern")
	}
}

// TestFindBarH_GrowsWithReplaceRow pins the geometry contract: the bar is
// two rows when replace is expanded, and the editor body gives up exactly
// those rows. A mismatch here paints the bar over the last line of code.
func TestFindBarH_GrowsWithReplaceRow(t *testing.T) {
	a := seedFindApp(t, "foo")
	_, _, _, closedH := a.editorRect()
	a.openFind()
	if a.findBarH() != 1 {
		t.Fatalf("collapsed bar height = %d, want 1", a.findBarH())
	}
	_, _, _, findH := a.editorRect()
	a.openReplace()
	if a.findBarH() != 2 {
		t.Fatalf("expanded bar height = %d, want 2", a.findBarH())
	}
	_, _, _, replaceH := a.editorRect()

	if findH != closedH-1 {
		t.Errorf("find bar should cost 1 row: %d -> %d", closedH, findH)
	}
	if replaceH != closedH-2 {
		t.Errorf("replace row should cost a 2nd row: %d -> %d", closedH, replaceH)
	}
	// The bar rect must sit directly above the status bar and cover both rows.
	_, by, _, bh := a.findBarRect()
	if by+bh != a.height-1 {
		t.Errorf("bar bottom = %d, want %d (status bar row)", by+bh, a.height-1)
	}
}

// TestHandleFindMouse_ClicksHitTheirControls drives the bar through the
// mouse path. The layout is computed once and shared by the renderer and
// the hit-test, so this also pins that they agree: a click at a toggle's
// drawn column must fire that toggle.
func TestHandleFindMouse_ClicksHitTheirControls(t *testing.T) {
	a := seedFindApp(t, "Foo foo")
	a.openFind()
	for _, r := range "foo" {
		a.handleFindKey(keyEv(tcell.KeyRune, r))
	}
	bx, by, bw, _ := a.findBarRect()
	l := a.findBarLayout(bx, bw)

	if l.caseW == 0 {
		t.Skip("test screen too narrow to lay out the toggles")
	}
	if !a.handleFindMouse(l.caseX, by, tcell.Button1) {
		t.Fatal("a click on the bar should be consumed by it")
	}
	if !a.activeTabPtr().FindCaseSensitive {
		t.Error("clicking Aa should enable case sensitivity")
	}
	a.handleFindMouse(l.regexX, by, tcell.Button1)
	if !a.activeTabPtr().FindRegex {
		t.Error("clicking .* should enable regex")
	}
	a.handleFindMouse(l.wordX, by, tcell.Button1)
	if !a.activeTabPtr().FindWholeWord {
		t.Error("clicking ab should enable whole-word")
	}

	// The chevron expands the replace row.
	a.handleFindMouse(l.chevronX, by, tcell.Button1)
	if !a.findReplaceOpen {
		t.Error("clicking the chevron should expand the replace row")
	}

	// A click outside the bar is not ours.
	if a.handleFindMouse(bx, by-3, tcell.Button1) {
		t.Error("a click above the bar must fall through to the editor")
	}
}

// TestHandleFindMouse_ReplaceButtons covers the two clickable actions on
// the replace row — the mouse-first path that works on terminals which
// never deliver Alt.
func TestHandleFindMouse_ReplaceButtons(t *testing.T) {
	a := seedFindApp(t, "foo bar foo")
	a.openFind()
	for _, r := range "foo" {
		a.handleFindKey(keyEv(tcell.KeyRune, r))
	}
	a.openReplace()
	for _, r := range "baz" {
		a.handleFindKey(keyEv(tcell.KeyRune, r))
	}
	bx, by, bw, _ := a.findBarRect()
	l := a.findBarLayout(bx, bw)
	if l.btnW == 0 {
		t.Skip("test screen too narrow to lay out the replace buttons")
	}
	a.handleFindMouse(l.btnAllX, by+1, tcell.Button1)
	if got := a.activeTabPtr().Buffer.Lines[0]; got != "baz bar baz" {
		t.Fatalf("clicking [All] produced %q", got)
	}
}

// TestMenuReplace_OpensExpandedBar checks the action menu reaches the
// feature. The menu is the primary surface per the project's rule that
// right-click can be swallowed by the host terminal.
func TestMenuReplace_OpensExpandedBar(t *testing.T) {
	a := seedFindApp(t, "foo")
	a.menuReplace()
	if !a.findOpen || !a.findReplaceOpen {
		t.Fatal("menuReplace should open the bar with the replace row expanded")
	}
	if a.findFocus != findFieldReplace {
		t.Fatal("menuReplace should focus the replace field")
	}
}

// TestFindBarLayout_TogglesOutrankTheHint pins the drop order of the bar's
// right-hand chrome. The toggles are admitted before the hint, and stay
// put when focus moves between the two fields.
//
// Regression: chrome used to be admitted in visual (right-to-left) order,
// so the longer query-field hint claimed the width first and the toggles
// vanished at 120 columns — then reappeared on Tab, because the replace
// field's hint is shorter. Search options that blink in and out as you
// change fields read as a rendering fault.
func TestFindBarLayout_TogglesOutrankTheHint(t *testing.T) {
	a := seedFindApp(t, "foo bar foo")
	a.openFind()
	for _, r := range "foo" {
		a.handleFindKey(keyEv(tcell.KeyRune, r))
	}
	bx, _, bw, _ := a.findBarRect()

	query := a.findBarLayout(bx, bw)
	if query.caseW == 0 {
		t.Fatal("toggles must be laid out at full width with the query focused")
	}

	a.openReplace()
	replace := a.findBarLayout(bx, bw)
	if replace.caseW == 0 {
		t.Fatal("toggles must survive moving focus to the replace field")
	}

	// Narrow the bar until the hint is dropped; the toggles must outlive it.
	for w := bw; w > 20; w -= 2 {
		l := a.findBarLayout(bx, w)
		if l.hintW > 0 && l.caseW == 0 {
			t.Fatalf("width %d kept the hint but dropped the toggles", w)
		}
	}
}

// TestFindBarLayout_NeverOverlaps walks the bar across every width it can
// be drawn at and asserts the query input never runs into the chrome to
// its right. An overlap here paints the user's query on top of the match
// counter, which is the kind of fault that only shows up on someone else's
// terminal size.
func TestFindBarLayout_NeverOverlaps(t *testing.T) {
	a := seedFindApp(t, "foo bar foo")
	a.openReplace()
	for _, r := range "foo" {
		a.handleFindKey(keyEv(tcell.KeyRune, r))
	}
	bx, _, _, _ := a.findBarRect()

	for w := 20; w <= 200; w++ {
		l := a.findBarLayout(bx, w)
		inputEnd := l.queryInX + l.queryInW
		for _, el := range []struct {
			name string
			x, w int
		}{
			{"toggles", l.caseX, l.caseW},
			{"status", l.statusX, l.statusW},
			{"hint", l.hintX, l.hintW},
		} {
			if el.w > 0 && el.x < inputEnd {
				t.Fatalf("width %d: query input ends at %d but %s starts at %d",
					w, inputEnd, el.name, el.x)
			}
		}
		if l.btnW > 0 && l.btnReplaceX < l.replaceInX+l.replaceInW {
			t.Fatalf("width %d: replace input overlaps the [Replace] button", w)
		}
	}
}
