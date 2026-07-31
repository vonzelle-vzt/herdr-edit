// =============================================================================
// File: internal/app/leader.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-04-30
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

// leader.go defines the editor's Esc-leader hotkey table. Esc-Esc still opens
// the action menu (handled in handleKey); the bindings here handle the
// "Esc, then one rune within doubleEscMs" sequences for common
// actions. We deliberately avoid Ctrl-key shortcuts because they fight
// tmux/zellij prefixes and the terminal's own bindings — Esc is the only
// modifier we trust over SSH.

package app

// leaderBinding is one Esc-leader entry: the trigger rune and the App method
// that fires when the user presses Esc, <rune> in quick succession. Each method
// already handles its own preconditions — calling menuUndo with no active tab,
// for example, is a safe no-op — so the leader dispatch doesn't need to
// re-check enable predicates.
type leaderBinding struct {
	key    rune
	action func(*App)
}

// leaderBindings is the editor's full Esc-leader table. The order is purely
// presentational: tests iterate it to assert every binding fires, and a
// future help screen can render the table directly. Letter bindings are
// chosen to be mnemonic and avoid collisions; punctuation bindings mirror
// familiar editor gestures where they make sense.
//
// Intentionally not bound:
//   - c / x / v (clipboard) — the host terminal's Cmd+C/V already covers
//     that path; adding a third channel just adds confusion. Completion was
//     briefly bound to c in violation of this and has moved to SPACE, which is
//     both free and the muscle memory from VS Code's Ctrl+Space.
//   - rename / delete / revert — destructive enough that we want the
//     menu's confirm dialog to gate the action as a deliberate gesture.
func leaderBindings() []leaderBinding {
	return []leaderBinding{
		{'s', (*App).menuSave},
		{'u', (*App).menuUndo},
		{'r', (*App).menuRedo},
		{'w', (*App).menuClose},
		{'q', (*App).menuQuit},
		{'n', (*App).menuNewFile},
		{'t', (*App).menuToggleSidebar},
		{'/', (*App).menuToggleLineComment},
		{'f', (*App).openFind},
		{'F', (*App).menuSearchInFiles},
		{'p', (*App).openFinder},
		{'g', (*App).menuGoToLine},
		{'a', (*App).menuSelectAll},
		{'k', (*App).openCommandPalette},
		{'b', (*App).menuToggleInlineBlame},
		{' ', (*App).menuComplete},
		{'o', (*App).menuOpenChanges},
		{'e', (*App).menuGoToLocation},
		{'i', (*App).menuOutline},
		{'I', (*App).menuWorkspaceSymbol},
		{'l', (*App).menuCodeActions},
		{'j', (*App).menuFindReferences},
		{'y', (*App).menuRenameSymbol},
		{'m', (*App).menuToggleBookmark},
		{'\'', (*App).menuNextBookmark},
		// Debugging (fork, Lane B). '9' mnemonic: F9, the universal
		// toggle-breakpoint key. '5' mnemonic: F5, the universal
		// start/resume key.
		//
		// Esc 5 opened the breakpoint list in stage 1, when that was the
		// whole feature; stage 3 added stepping, the call stack, the
		// goroutine list, variables and a console, and there is no leader
		// rune left to give any of them — every lowercase letter is bound
		// or reserved (c/x/v belong to the terminal's clipboard), Ctrl- is
		// forbidden fork-wide, and shifted F-keys need modifyOtherKeys and
		// are unreliable through a multiplexer. So Esc 5 is now the debug
		// PICKER over all of them, with the breakpoint list as one row.
		{'9', (*App).menuToggleBreakpoint},
		{'5', (*App).menuDebugPicker},
		// LSP. Both requests were implemented, tested and advertised in initialize with no caller;
		// these two keys are the entire wiring.
		{'h', (*App).menuHover},
		{'d', (*App).menuGoToDefinition},
		{'z', (*App).menuToggleWrap},
		// Problems list + next/prev, the VS Code F8 muscle memory. F8 /
		// Shift+F8 are wired directly in handleKey (not KeyRune, so they
		// can't go through this table) — these three are the primary path.
		{';', (*App).menuProblems},
		{'.', (*App).menuNextProblem},
		{',', (*App).menuPrevProblem},
	}
}

// leaderActionFor looks up the App method bound to r in the leader table,
// or returns nil when r isn't bound. Returning nil rather than a no-op
// lets the caller distinguish "leader fired" from "key was unbound — fall
// through to normal handling", which matters for typing flow: pressing
// Esc then a non-leader letter must still let that letter reach the
// editor's normal key handler.
func leaderActionFor(r rune) func(*App) {
	for _, b := range leaderBindings() {
		if b.key == r {
			return b.action
		}
	}
	return nil
}
