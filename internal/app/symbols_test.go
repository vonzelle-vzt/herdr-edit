// =============================================================================
// File: internal/app/symbols_test.go
// Author: Vonzelle Brown
// Created: 2026-07-30
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package app

import (
	"testing"
	"time"

	"github.com/cloudmanic/spice-edit/internal/lsp"
)

// TestOutline_LoadsIntoThePalette pins that the outline reuses the palette
// rather than adding a third list widget, and that nesting is indented.
func TestOutline_LoadsIntoThePalette(t *testing.T) {
	a := seedNavApp(t, "package main\n\nfunc one() {}\n\nfunc two() {}\n")
	a.handleSymbols(&symbolsEvent{syms: []lsp.Symbol{
		{Name: "one", Kind: 12, Line: 2},
		{Name: "two", Kind: 12, Line: 4, Depth: 1},
	}, when: time.Now()})

	if !a.paletteOpen {
		t.Fatal("the outline did not open the palette")
	}
	if a.paletteTitle != "Go to symbol" {
		t.Errorf("palette title = %q", a.paletteTitle)
	}
	if len(a.paletteResults) != 2 {
		t.Fatalf("palette has %d rows, want 2", len(a.paletteResults))
	}
	// A nested symbol is indented so the outline reads as a tree.
	labels := []string{a.paletteResults[0].cmd.label, a.paletteResults[1].cmd.label}
	nested := false
	for _, l := range labels {
		if len(l) > 0 && l[0] == ' ' {
			nested = true
		}
	}
	if !nested {
		t.Errorf("no symbol was indented for depth: %q", labels)
	}
}

// TestOutline_JumpsToTheSymbol drives a palette row and checks the cursor moved.
func TestOutline_JumpsToTheSymbol(t *testing.T) {
	a := seedNavApp(t, "one\ntwo\nthree\nfour\nfive\n")
	a.handleSymbols(&symbolsEvent{syms: []lsp.Symbol{{Name: "target", Kind: 12, Line: 3}}, when: time.Now()})
	if len(a.paletteResults) != 1 {
		t.Fatalf("expected one symbol row, got %d", len(a.paletteResults))
	}
	a.paletteSelected = 0
	a.runSelectedPaletteCommand()

	if got := a.activeTabPtr().Cursor.Line; got != 3 {
		t.Fatalf("cursor on line %d, want 3", got)
	}
}

// TestOutline_EmptyIsSilent pins that a server without outline support says so
// rather than opening an empty picker.
func TestOutline_EmptyIsSilent(t *testing.T) {
	a := seedNavApp(t, "x\n")
	a.handleSymbols(&symbolsEvent{syms: nil, when: time.Now()})
	if a.paletteOpen {
		t.Fatal("an empty outline opened the palette")
	}
}

// TestCodeActions_RefusesDirtyBuffer pins the same guard rename has: a fix
// rewrites files on disk and the tab reloads after, so unsaved work would vanish.
func TestCodeActions_RefusesDirtyBuffer(t *testing.T) {
	a := seedNavApp(t, "old\n")
	tab := a.activeTabPtr()
	before := tab.Buffer.String()

	a.handleCodeActions(&codeActionsEvent{actions: []lsp.CodeAction{{
		Title: "Fix it",
		Edits: map[string][]lsp.TextEdit{tab.Path: {
			{Range: lsp.Range{Start: lsp.Position{Line: 0, Character: 0}, End: lsp.Position{Line: 0, Character: 3}}, NewText: "new"},
		}},
	}}, when: time.Now()})

	if !a.paletteOpen {
		t.Fatal("the fix list did not open")
	}
	tab.Dirty = true
	a.paletteSelected = 0
	a.runSelectedPaletteCommand()

	if a.activeTabPtr().Buffer.String() != before {
		t.Fatal("a fix ran against a dirty buffer")
	}
}

// TestCodeActions_EmptyIsSilent covers the no-fix case.
func TestCodeActions_EmptyIsSilent(t *testing.T) {
	a := seedNavApp(t, "x\n")
	a.handleCodeActions(&codeActionsEvent{actions: nil, when: time.Now()})
	if a.paletteOpen {
		t.Fatal("an empty fix list opened the palette")
	}
}

// TestSymbols_Reachable is the guard against this fork's signature failure: a
// complete, tested, capability-advertised LSP method with no call site. It has
// happened three times.
func TestSymbols_Reachable(t *testing.T) {
	for _, k := range []rune{'i', 'l'} {
		if leaderActionFor(k) == nil {
			t.Errorf("Esc %c is not bound — the feature is unreachable", k)
		}
	}
	a := newTestApp(t, t.TempDir())
	items, _, _ := a.menuLayout()
	found := 0
	for _, it := range items {
		if it.label == "Go to symbol (outline)" || it.label == "Fix at cursor" {
			found++
		}
	}
	if found != 2 {
		t.Errorf("only %d of 2 rows are in the menu", found)
	}
}
