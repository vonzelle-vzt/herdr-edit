// =============================================================================
// File: internal/app/locationref_test.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudmanic/spice-edit/internal/editor"
)

// TestGoToLocationJumpsFromAReferencesList pins bug 1: handleReferences
// builds a synthetic tab of "rel:line:col" rows and tells the user "Esc p
// opens any of these", but Esc p is openFinder — the fuzzy filename finder,
// which takes no line number. This test synthesizes the exact row shape
// handleReferences prints, puts the cursor on it, and asserts Esc e's
// handler (menuGoToLocation) actually lands on that file and line.
func TestGoToLocationJumpsFromAReferencesList(t *testing.T) {
	dir := t.TempDir()
	targetRel := "target.go"
	target := filepath.Join(dir, targetRel)
	if err := os.WriteFile(target, []byte("package p\n\nfunc T() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "other.go")
	if err := os.WriteFile(other, []byte("package p\n"), 0644); err != nil {
		t.Fatal(err)
	}

	a := newTestApp(t, dir)

	// The exact shape handleReferences (refactor.go) builds.
	var b strings.Builder
	fmt.Fprintf(&b, "%d reference(s) to %s\n\n", 1, "Foo")
	fmt.Fprintf(&b, "%s:%d:%d\n", targetRel, 3, 1)
	tab := editorSyntheticTab("references: Foo.txt", b.String())
	a.tabs = append(a.tabs, tab)
	a.activeTab = len(a.tabs) - 1

	// Line 0 is the count header, line 1 is blank, line 2 is the reference row.
	tab.MoveCursorTo(editor.Position{Line: 2, Col: 0}, false)

	a.menuGoToLocation()

	got := a.activeTabPtr()
	if got == nil || got.Path != target {
		t.Fatalf("did not jump to %s from the references row, got tab %+v", target, got)
	}
	if got.Cursor.Line != 2 {
		t.Errorf("cursor landed on line %d, want 2 (0-based for the 1-based source line 3)", got.Cursor.Line)
	}
}

// TestStaleDiffSourceDoesNotHijackANonDiffTab pins bug 2: a.diffSource is set
// once by menuOpenChanges and never cleared, so the OLD guard on
// menuJumpToDiffSource (tab.Synthetic && a.diffSource != "") stayed true for
// the rest of the session after a single Esc o — including on a later
// references-list tab that is not a diff at all. This is RED against
// unmodified main: leaderActionFor('e') resolves to menuJumpToDiffSource,
// whose guard passes on the references tab below and runs diff-hunk parsing
// over reference rows (which finds no "@@" header and just flashes "No
// source line here", leaving the tab untouched — never jumping to x.go).
// After the fix, Esc e resolves to menuGoToLocation, which requires the
// tab's Label to end in ".diff" before taking the diff branch, so it falls
// through to the location branch and actually jumps.
func TestStaleDiffSourceDoesNotHijackANonDiffTab(t *testing.T) {
	dir := t.TempDir()
	referenced := filepath.Join(dir, "x.go")
	if err := os.WriteFile(referenced, []byte("package p\n"), 0644); err != nil {
		t.Fatal(err)
	}
	diffSourceFile := filepath.Join(dir, "y.go")
	if err := os.WriteFile(diffSourceFile, []byte("package p\n"), 0644); err != nil {
		t.Fatal(err)
	}

	a := newTestApp(t, dir)
	a.diffSource = diffSourceFile // set by an earlier Esc o, never cleared

	tab := editorSyntheticTab("references: Bar.txt", "1 reference(s) to Bar\n\nx.go:1:1\n")
	a.tabs = append(a.tabs, tab)
	a.activeTab = len(a.tabs) - 1
	tab.MoveCursorTo(editor.Position{Line: 2, Col: 0}, false)

	action := leaderActionFor('e')
	if action == nil {
		t.Fatal("Esc e is not bound")
	}
	action(a)

	got := a.activeTabPtr()
	if got == nil || got.Path != referenced {
		t.Fatalf("stale diffSource hijacked the jump: got tab %+v, want a jump to %s", got, referenced)
	}
}
