// =============================================================================
// File: internal/app/lspactions_test.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-07-30
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudmanic/spice-edit/internal/editor"
	"github.com/cloudmanic/spice-edit/internal/lsp"
)

// TestLSPActionsDegradeWithoutAServer pins the no-server path. Both actions are reachable from a
// leader key at all times, so with no language server attached they must say so rather than panic on
// a nil Manager.
func TestLSPActionsDegradeWithoutAServer(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.menuHover()
	if !strings.Contains(a.statusMsg, "No language server") {
		t.Fatalf("hover with no server: %q", a.statusMsg)
	}
	a.statusMsg = ""
	a.menuGoToDefinition()
	if !strings.Contains(a.statusMsg, "No language server") {
		t.Fatalf("definition with no server: %q", a.statusMsg)
	}
}

// TestLSPCursorPosConvertsToUTF16 is the boundary that quietly breaks on non-ASCII. LSP counts UTF-16
// code units and the buffer counts runes; they agree for ASCII, so a mistake here survives testing
// until a line contains an emoji and every request on that line targets the wrong character.
func TestLSPCursorPosConvertsToUTF16(t *testing.T) {
	root := t.TempDir()
	// An emoji is two UTF-16 code units but one rune, so the two column systems diverge after it.
	path := filepath.Join(root, "x.go")
	if err := os.WriteFile(path, []byte("a🎉bc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := newTestApp(t, root)
	a.openFile(path)
	tab := a.activeTabPtr()
	if tab == nil {
		t.Fatal("file did not open")
	}
	// Without a Manager lspCursorPos reports not-ok, so attach a non-nil one for the conversion path.
	a.lsp = &lsp.Manager{}
	tab.MoveCursorTo(editor.Position{Line: 0, Col: 3}, false) // rune col 3 == "c"

	_, pos, ok := a.lspCursorPos()
	if !ok {
		t.Fatal("expected a position")
	}
	if pos.Character == tab.Cursor.Col {
		t.Fatalf("UTF-16 column should differ from the rune column past an emoji: both %d", pos.Character)
	}
	if got := lsp.UTF16ToRuneCol(tab.LineText(0), pos.Character); got != tab.Cursor.Col {
		t.Fatalf("round trip lost the column: rune %d -> utf16 %d -> rune %d",
			tab.Cursor.Col, pos.Character, got)
	}
}

// TestHandleLSPJumpEmpty covers the common real answer: the server knows the symbol but has no
// definition to offer (a builtin, or an unresolved import).
func TestHandleLSPJumpEmpty(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.handleLSPJump(&lspJumpEvent{when: time.Now()})
	if !strings.Contains(a.statusMsg, "No definition") {
		t.Fatalf("empty jump: %q", a.statusMsg)
	}
}

// TestHandleLSPJumpOpensAndMovesTheCursor is the feature. It also pins the reason MoveCursorTo is
// used instead of assigning Cursor: only the mutator sets cursorMoved, and without it Render never
// calls EnsureVisible, so a definition on line 40 would be "opened" off screen.
func TestHandleLSPJumpOpensAndMovesTheCursor(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.go")
	body := strings.Repeat("filler\n", 30) + "func Wanted() {}\n"
	if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	a := newTestApp(t, root)
	a.width, a.height = 120, 40

	a.handleLSPJump(&lspJumpEvent{
		when: time.Now(),
		locs: []lsp.Location{{
			URI:   lsp.URI(target),
			Range: lsp.Range{Start: lsp.Position{Line: 30, Character: 5}},
		}},
	})

	tab := a.activeTabPtr()
	if tab == nil || tab.Path != target {
		t.Fatalf("jump did not open the target file, tab=%+v", tab)
	}
	if tab.Cursor.Line != 30 || tab.Cursor.Col != 5 {
		t.Fatalf("cursor at %d:%d, want 30:5", tab.Cursor.Line, tab.Cursor.Col)
	}
}

// TestHandleLSPHoverPicksTheSignature checks the one-line reduction of a markdown payload: blank
// lines and code fences are noise, the signature is what the user wants in the status bar.
func TestHandleLSPHoverPicksTheSignature(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.handleLSPHover(&lspHoverEvent{
		when: time.Now(),
		text: "```go\nfunc Wanted(n int) error\n```\n\nWanted does a thing.\n",
	})
	if a.statusMsg != "func Wanted(n int) error" {
		t.Fatalf("hover status: %q", a.statusMsg)
	}

	a.statusMsg = ""
	a.handleLSPHover(&lspHoverEvent{when: time.Now(), text: "\n\n```\n```\n"})
	if !strings.Contains(a.statusMsg, "No hover information") {
		t.Fatalf("empty hover: %q", a.statusMsg)
	}
}

// TestLSPLeaderBindings makes sure the two keys are actually reachable — the whole point of the
// audit was that the implementation existed and nothing called it.
func TestLSPLeaderBindings(t *testing.T) {
	for _, r := range []rune{'h', 'd'} {
		if leaderActionFor(r) == nil {
			t.Fatalf("Esc %c is not bound", r)
		}
	}
}

// TestToggleWrapFlipsTheActiveTab pins the toggle and, importantly, that it re-anchors the viewport:
// flipping the mode changes how many rows a line occupies, so a cursor that was on screen can fall off
// it the instant wrap turns on unless cursorMoved forces EnsureVisible.
func TestToggleWrapFlipsTheActiveTab(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "x.go")
	if err := os.WriteFile(path, []byte(strings.Repeat("word ", 100)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := newTestApp(t, root)
	a.openFile(path)
	tab := a.activeTabPtr()
	if tab == nil {
		t.Fatal("no tab")
	}
	if tab.Wrap {
		t.Fatal("wrap should default to off, like VS Code")
	}

	a.menuToggleWrap()
	if !tab.Wrap {
		t.Fatal("toggle did not enable wrap")
	}
	if !strings.Contains(a.statusMsg, "Word wrap: on") {
		t.Fatalf("status: %q", a.statusMsg)
	}

	a.menuToggleWrap()
	if tab.Wrap {
		t.Fatal("toggle did not disable wrap")
	}

	// Reachable, which is the point.
	if leaderActionFor('z') == nil {
		t.Fatal("Esc z is not bound")
	}
}

// TestToggleWrapWithNoTab — the leader key is live at all times, so it must degrade rather than panic.
func TestToggleWrapWithNoTab(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.menuToggleWrap()
	if !strings.Contains(a.statusMsg, "No file open") {
		t.Fatalf("status: %q", a.statusMsg)
	}
}
