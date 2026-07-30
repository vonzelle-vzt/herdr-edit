// =============================================================================
// File: internal/app/palette_test.go
// Author: Vonzelle Brown
// Created: 2026-07-30
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package app

import (
	"testing"

	"github.com/gdamore/tcell/v2"
)

// TestPalette_BuildsFromTheMenu pins the design decision that matters: the
// palette has no command list of its own. If it grew one, it would drift from
// the menu and start offering actions that no longer exist.
func TestPalette_BuildsFromTheMenu(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	items, _, _ := a.menuLayout()
	cmds := a.paletteCommands()
	if len(cmds) == 0 {
		t.Fatal("the palette found no commands at all")
	}
	if len(cmds) > len(items) {
		t.Fatalf("palette has %d commands from a %d-row menu", len(cmds), len(items))
	}
	found := false
	for _, c := range cmds {
		if c.label == "Quit editor" {
			found = true
		}
	}
	if !found {
		t.Error("a known menu action is missing from the palette")
	}
}

// TestPalette_FuzzyMatchesAndRanks checks a subsequence query surfaces the
// right command at the top, which is the whole point of a palette.
func TestPalette_FuzzyMatchesAndRanks(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.openCommandPalette()
	for _, r := range "quit" {
		a.handlePaletteKey(keyEv(tcell.KeyRune, r))
	}
	if len(a.paletteResults) == 0 {
		t.Fatal("a query matching a real command returned nothing")
	}
	if got := a.paletteResults[0].cmd.label; got != "Quit editor" {
		t.Fatalf("top result = %q, want Quit editor", got)
	}
}

// TestPalette_NarrowsAsYouType guards the incremental behaviour: more query
// means fewer results, never more.
func TestPalette_NarrowsAsYouType(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.openCommandPalette()
	all := len(a.paletteResults)
	for _, r := range "quit" {
		a.handlePaletteKey(keyEv(tcell.KeyRune, r))
	}
	if len(a.paletteResults) >= all {
		t.Fatalf("typing did not narrow the list: %d -> %d", all, len(a.paletteResults))
	}
}

// TestPalette_EnterRunsTheCommand drives the palette the way a user does and
// asserts the action actually fired — the palette existing is not the feature,
// running a command from it is.
func TestPalette_EnterRunsTheCommand(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.openCommandPalette()
	for _, r := range "run a command" {
		a.handlePaletteKey(keyEv(tcell.KeyRune, r))
	}
	// Select the palette's own row and fire it: it reopens the palette, which
	// is observable without depending on any other action's side effects.
	a.handlePaletteKey(keyEv(tcell.KeyEnter, 0))
	if !a.paletteOpen {
		t.Fatal("running the palette command should have reopened the palette")
	}
}

// TestPalette_DisabledCommandDoesNotFire pins that an unavailable action is
// refused with a message rather than run. Firing a disabled command is worse
// than refusing it: the user cannot tell what happened.
func TestPalette_DisabledCommandDoesNotFire(t *testing.T) {
	a := newTestApp(t, t.TempDir()) // no tab open, so Save is disabled
	a.openCommandPalette()
	for _, r := range "save" {
		a.handlePaletteKey(keyEv(tcell.KeyRune, r))
	}
	if len(a.paletteResults) == 0 {
		t.Skip("no save-like command in this build")
	}
	if a.paletteResults[0].cmd.enabled(a) {
		t.Skip("Save is enabled in this fixture; nothing to assert")
	}
	a.handlePaletteKey(keyEv(tcell.KeyEnter, 0))
	if !a.paletteOpen {
		t.Fatal("a disabled command closed the palette instead of being refused")
	}
}

// TestPalette_EscCloses covers the exit path.
func TestPalette_EscCloses(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.openCommandPalette()
	a.handlePaletteKey(keyEv(tcell.KeyEsc, 0))
	if a.paletteOpen {
		t.Fatal("Esc should close the palette")
	}
}

// TestPalette_ReachableFromLeaderAndMenu guards the failure mode this fork has
// hit three times: a complete feature with nothing calling it.
func TestPalette_ReachableFromLeaderAndMenu(t *testing.T) {
	if leaderActionFor('k') == nil {
		t.Error("Esc k is not bound — the palette is unreachable by keyboard")
	}
	a := newTestApp(t, t.TempDir())
	items, _, _ := a.menuLayout()
	for _, it := range items {
		if it.label == "Run a command" {
			return
		}
	}
	t.Error("the palette has no menu row — unreachable by mouse")
}

// TestPalette_ClosedByOtherModals keeps the modal invariant: only one at a time.
func TestPalette_ClosedByOtherModals(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.openCommandPalette()
	a.closeAllModals()
	if a.paletteOpen {
		t.Fatal("closeAllModals should close the palette")
	}
}
