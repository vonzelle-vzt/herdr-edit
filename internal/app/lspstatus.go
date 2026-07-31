// =============================================================================
// File: internal/app/lspstatus.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// lspstatus.go gives lsp.Manager.Describe (and the Running/Available methods
// behind it) a real call site.
//
// All three were complete and tested with nothing but _test.go callers —
// Describe's own doc comment says it exists so "no diagnostics" can be
// explained rather than just experienced, but nothing in the UI ever asked
// it to explain anything. This is the same fork-wide pattern CLAUDE.md
// documents for hover/definition and Replace/ReplaceAll/SetFindOptions: a
// green test suite proves the engine works, not that anyone can reach it.
// This file is the missing status-bar tag and menu row, not new LSP work.

package app

import "strings"

// lspStatusText returns a short right-hand status-bar tag summarising the
// project's language servers, or "" when there is nothing worth a tag.
//
// Deliberately terse: the status bar already carries the git branch and the
// diagnostics summary, so this competes with both for a narrow strip of a
// pane that may be sitting beside an agent at 60-odd columns. A server
// actually running gets named (what the user wants to know: "is it
// working"); one merely installed but not yet started gets a bare "lsp?"
// hint rather than a full server list, which belongs to the menu row's full
// Describe() output, not the status bar.
func (a *App) lspStatusText() string {
	if a.lsp == nil {
		return ""
	}
	if running := a.lsp.Running(); len(running) > 0 {
		return "lsp:" + strings.Join(running, ",")
	}
	if avail := a.lsp.Available(); len(avail) > 0 {
		return "lsp?"
	}
	return ""
}

// menuLSPStatus opens the full Manager.Describe() output in the info modal,
// the same surface used for a failed custom action's captured stderr. The
// status-bar tag from lspStatusText is intentionally too short to explain
// itself; this is where "why are there no squiggles" gets a real answer.
func (a *App) menuLSPStatus() {
	a.closeMenu()
	if a.lsp == nil {
		a.openInfo("Language server status", []string{"No language server manager for this project."})
		return
	}
	a.openInfo("Language server status", []string{a.lsp.Describe()})
}
