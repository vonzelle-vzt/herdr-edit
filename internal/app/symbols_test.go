// =============================================================================
// File: internal/app/symbols_test.go
// Author: Vonzelle Brown
// Created: 2026-07-30
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package app

import (
	"os"
	"path/filepath"
	"strings"
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

// TestWorkspaceSymbol_LoadsIntoPalette pins that workspace/symbol results,
// like the file-local outline, are presented through the palette rather than
// a bespoke picker — and that each row shows which file it belongs to, since
// a workspace search can span the whole project rather than one open tab.
func TestWorkspaceSymbol_LoadsIntoPalette(t *testing.T) {
	a := seedNavApp(t, "one\ntwo\nthree\n")
	a.handleWorkspaceSymbols(&workspaceSymbolsEvent{syms: []lsp.Symbol{
		{Name: "Handler", Kind: 12, URI: "file:///proj/handler.go", Line: 4},
	}, when: time.Now()})

	if !a.paletteOpen {
		t.Fatal("workspace symbols did not open the palette")
	}
	if a.paletteTitle != "Go to symbol in workspace" {
		t.Errorf("palette title = %q", a.paletteTitle)
	}
	if len(a.paletteResults) != 1 {
		t.Fatalf("palette has %d rows, want 1", len(a.paletteResults))
	}
	shortcut := a.paletteResults[0].cmd.shortcut
	if !strings.Contains(shortcut, "handler.go") || !strings.Contains(shortcut, "5") {
		t.Errorf("shortcut = %q, want it to name the file and 1-based line", shortcut)
	}
}

// TestWorkspaceSymbol_JumpsToTheSymbol drives a palette row like the outline
// test does, but across a file that was not already open — proving the
// picker opens the right file, not just moves the cursor in the current tab.
func TestWorkspaceSymbol_JumpsToTheSymbol(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.go")
	if err := os.WriteFile(target, []byte("one\ntwo\nthree\nfour\nfive\n"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	a := newTestApp(t, dir)

	a.handleWorkspaceSymbols(&workspaceSymbolsEvent{syms: []lsp.Symbol{
		{Name: "target", Kind: 12, URI: lsp.URI(target), Line: 3},
	}, when: time.Now()})
	if len(a.paletteResults) != 1 {
		t.Fatalf("expected one symbol row, got %d", len(a.paletteResults))
	}
	a.paletteSelected = 0
	a.runSelectedPaletteCommand()

	tab := a.activeTabPtr()
	if tab == nil || tab.Path != target {
		t.Fatalf("active tab = %+v, want it to be %q", tab, target)
	}
	if got := tab.Cursor.Line; got != 3 {
		t.Fatalf("cursor on line %d, want 3", got)
	}
}

// TestWorkspaceSymbol_UnknownLineDoesNotMoveCursor pins the LSP 3.17
// no-range WorkspaceSymbol case end to end: selecting a result whose line is
// unknown must open the file without pretending the cursor belongs at line 0
// (which would display as line 1 — a location the server never actually sent).
func TestWorkspaceSymbol_UnknownLineDoesNotMoveCursor(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.go")
	if err := os.WriteFile(target, []byte("one\ntwo\nthree\n"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	a := newTestApp(t, dir)
	a.openFile(target)
	a.activeTabPtr().MoveCursorTo(posAtLine(2), false)

	a.handleWorkspaceSymbols(&workspaceSymbolsEvent{syms: []lsp.Symbol{
		{Name: "target", Kind: 12, URI: lsp.URI(target), LineUnknown: true},
	}, when: time.Now()})
	shortcut := a.paletteResults[0].cmd.shortcut
	if !strings.Contains(shortcut, "?") {
		t.Errorf("shortcut = %q, want it to show the line as unknown", shortcut)
	}
	a.paletteSelected = 0
	a.runSelectedPaletteCommand()

	if got := a.activeTabPtr().Cursor.Line; got != 2 {
		t.Fatalf("cursor moved to line %d, want it to stay at 2 (line was unknown)", got)
	}
}

// TestWorkspaceSymbol_EmptyIsSilent pins that no matches says so rather than
// opening an empty picker, mirroring TestOutline_EmptyIsSilent.
func TestWorkspaceSymbol_EmptyIsSilent(t *testing.T) {
	a := seedNavApp(t, "x\n")
	a.handleWorkspaceSymbols(&workspaceSymbolsEvent{syms: nil, when: time.Now()})
	if a.paletteOpen {
		t.Fatal("an empty workspace-symbol result opened the palette")
	}
}

// TestWorkspaceSymbolWithNoServerRunningSaysSo pins the silent-failure guard
// the brief calls out: servers start lazily on the first matching file, so
// before one has opened, Running() is empty and a query would fan out to
// nothing — indistinguishable from "your symbol does not exist" unless the
// menu action checks first and says so explicitly.
func TestWorkspaceSymbolWithNoServerRunningSaysSo(t *testing.T) {
	a := newTestApp(t, t.TempDir()) // a.lsp is nil — no manager, let alone a running server
	a.menuWorkspaceSymbol()

	if a.promptOpen {
		t.Fatal("the prompt opened even though no language server is running")
	}
	if !strings.Contains(a.statusMsg, "No language server is running") {
		t.Errorf("statusMsg = %q, want it to say no server is running", a.statusMsg)
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
	for _, k := range []rune{'i', 'I', 'l'} {
		if leaderActionFor(k) == nil {
			t.Errorf("Esc %c is not bound — the feature is unreachable", k)
		}
	}
	a := newTestApp(t, t.TempDir())
	items, _, _ := a.menuLayout()
	found := 0
	for _, it := range items {
		if it.label == "Go to symbol (outline)" || it.label == "Go to symbol in workspace" || it.label == "Fix at cursor" {
			found++
		}
	}
	if found != 3 {
		t.Errorf("only %d of 3 rows are in the menu", found)
	}
}
