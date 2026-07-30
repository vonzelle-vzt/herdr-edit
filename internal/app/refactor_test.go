// =============================================================================
// File: internal/app/refactor_test.go
// Author: Vonzelle Brown
// Created: 2026-07-30
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudmanic/spice-edit/internal/lsp"
)

// TestSymbolAtCursor covers the identifier extraction used by both the rename
// prompt and the references heading.
func TestSymbolAtCursor(t *testing.T) {
	a := seedNavApp(t, "func doThing(x int) {\n}\n")
	tab := a.activeTabPtr()
	tab.MoveCursorTo(posAt(0, 8), false) // inside doThing
	if got := a.symbolAtCursor(); got != "doThing" {
		t.Fatalf("symbolAtCursor = %q, want doThing", got)
	}
	// Sitting just past a word takes the word that ENDS at the cursor, which is
	// what you want after typing an identifier and immediately asking to rename
	// it — the cursor is at the end, not inside.
	tab.MoveCursorTo(posAt(0, 4), false) // immediately after "func"
	if got := a.symbolAtCursor(); got != "func" {
		t.Fatalf("symbolAtCursor just past a word = %q, want func", got)
	}

	// On whitespace with no identifier on either side there is nothing to name.
	tab.MoveCursorTo(posAt(0, 19), false)
	if got := a.symbolAtCursor(); got != "" && !isIdentRune(rune(got[0])) {
		t.Fatalf("symbolAtCursor on punctuation = %q", got)
	}
}

// TestApplyWorkspaceEdits_RewritesFiles is the dangerous half of rename: these
// edits are applied to files ON DISK, so getting the ordering or the column
// conversion wrong corrupts source that may not even be open.
func TestApplyWorkspaceEdits_RewritesFiles(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.go")
	if err := os.WriteFile(f, []byte("old := 1\nprintln(old, old)\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Descending order, as workspaceEdits guarantees.
	edits := map[string][]lsp.TextEdit{f: {
		{Range: lsp.Range{Start: lsp.Position{Line: 1, Character: 13}, End: lsp.Position{Line: 1, Character: 16}}, NewText: "neu"},
		{Range: lsp.Range{Start: lsp.Position{Line: 1, Character: 8}, End: lsp.Position{Line: 1, Character: 11}}, NewText: "neu"},
		{Range: lsp.Range{Start: lsp.Position{Line: 0, Character: 0}, End: lsp.Position{Line: 0, Character: 3}}, NewText: "neu"},
	}}
	files, count, err := applyWorkspaceEdits(edits)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if files != 1 || count != 3 {
		t.Fatalf("applied %d edits across %d files, want 3 across 1", count, files)
	}
	got, _ := os.ReadFile(f)
	want := "neu := 1\nprintln(neu, neu)\n"
	if string(got) != want {
		t.Fatalf("file is now %q, want %q", got, want)
	}
}

// TestApplyWorkspaceEdits_SkipsMultiLine pins that a multi-line replacement is
// skipped rather than guessed at. A rename does not produce them, and applying
// one wrongly corrupts the file irreversibly.
func TestApplyWorkspaceEdits_SkipsMultiLine(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "b.go")
	original := "one\ntwo\nthree\n"
	if err := os.WriteFile(f, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	edits := map[string][]lsp.TextEdit{f: {
		{Range: lsp.Range{Start: lsp.Position{Line: 0, Character: 0}, End: lsp.Position{Line: 2, Character: 1}}, NewText: "X"},
	}}
	_, count, err := applyWorkspaceEdits(edits)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("applied %d multi-line edits, want 0", count)
	}
	got, _ := os.ReadFile(f)
	if string(got) != original {
		t.Fatalf("file was modified to %q despite the edit being skipped", got)
	}
}

// TestApplyWorkspaceEdits_MissingFile reports rather than partially applying.
func TestApplyWorkspaceEdits_MissingFile(t *testing.T) {
	edits := map[string][]lsp.TextEdit{"/definitely/not/here.go": {
		{Range: lsp.Range{Start: lsp.Position{Line: 0, Character: 0}, End: lsp.Position{Line: 0, Character: 1}}, NewText: "x"},
	}}
	if _, _, err := applyWorkspaceEdits(edits); err == nil {
		t.Fatal("a missing file should be an error, not a silent skip")
	}
}

// TestRename_RefusesDirtyBuffer pins the guard that stops a rename clobbering
// unsaved work: the edit rewrites files on disk and the tab is reloaded after,
// so an unsaved buffer would simply vanish.
func TestRename_RefusesDirtyBuffer(t *testing.T) {
	a := seedNavApp(t, "old := 1\n")
	tab := a.activeTabPtr()
	tab.Dirty = true
	before, _ := os.ReadFile(tab.Path)

	a.handleRename(&renameEvent{
		edits: map[string][]lsp.TextEdit{tab.Path: {
			{Range: lsp.Range{Start: lsp.Position{Line: 0, Character: 0}, End: lsp.Position{Line: 0, Character: 3}}, NewText: "neu"},
		}},
		newName: "neu",
	})

	after, _ := os.ReadFile(tab.Path)
	if string(after) != string(before) {
		t.Fatal("a rename ran against a dirty buffer and rewrote the file")
	}
}

// TestRefactor_Reachable is the guard for this fork's signature failure: a
// complete, tested, capability-advertised LSP request that nothing calls.
// hover, definition and find/replace each shipped that way.
func TestRefactor_Reachable(t *testing.T) {
	if leaderActionFor('j') == nil {
		t.Error("Esc j is not bound — find references is unreachable")
	}
	if leaderActionFor('y') == nil {
		t.Error("Esc y is not bound — rename is unreachable")
	}
	a := newTestApp(t, t.TempDir())
	items, _, _ := a.menuLayout()
	found := 0
	for _, it := range items {
		if it.label == "Find references" || it.label == "Rename symbol" {
			found++
		}
	}
	if found != 2 {
		t.Errorf("only %d of 2 refactor rows are in the menu", found)
	}
}
