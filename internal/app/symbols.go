// =============================================================================
// File: internal/app/symbols.go
// Author: Vonzelle Brown
// Created: 2026-07-30
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// symbols.go wires the last three LSP requests: the outline (Esc i), code
// actions (Esc l — a lightbulb, like VS Code's), and signature help, which is
// folded into hover rather than given a key of its own.
//
// 🔴 As always here, the request was the easy half. This fork has shipped a
// complete, tested, capability-advertised LSP method with no call site three
// separate times. symbols_test.go asserts both new leader keys resolve and both
// menu rows exist, so a fourth cannot happen quietly.
//
// Signature help has no key because it has no moment of its own: you want it
// while your cursor is inside a call, which is exactly when you would reach for
// hover. Esc h now answers with both when both apply.

package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cloudmanic/spice-edit/internal/lsp"
)

// symbolsEvent and codeActionsEvent deliver their answers as posted events,
// like every other LSP action here.
type symbolsEvent struct {
	syms []lsp.Symbol
	when time.Time
}

func (e *symbolsEvent) When() time.Time { return e.when }

type codeActionsEvent struct {
	actions []lsp.CodeAction
	when    time.Time
}

func (e *codeActionsEvent) When() time.Time { return e.when }

// menuOutline asks for the file's symbols. Bound to Esc i.
func (a *App) menuOutline() {
	a.closeMenu()
	tab := a.activeTabPtr()
	if tab == nil || tab.Path == "" {
		return
	}
	path := tab.Path
	mgr := a.lsp
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		syms, err := mgr.DocumentSymbol(ctx, path)
		if err != nil {
			syms = nil
		}
		a.post(&symbolsEvent{syms: syms, when: time.Now()})
	}()
	a.flash("Reading outline…")
}

// handleSymbols turns the outline into bookmarks-style navigation by loading it
// into the command palette, which already does fuzzy search, keyboard selection
// and mouse clicks. A bespoke symbol picker would be a third list widget in this
// editor doing what the other two already do.
func (a *App) handleSymbols(e *symbolsEvent) {
	if len(e.syms) == 0 {
		a.flash("No symbols — the language server may not support an outline here")
		return
	}
	tab := a.activeTabPtr()
	if tab == nil {
		return
	}
	path := tab.Path
	cmds := make([]paletteCommand, 0, len(e.syms))
	for _, s := range e.syms {
		line := s.Line
		label := strings.Repeat("  ", s.Depth) + s.Name
		detail := lsp.SymbolKindName(s.Kind)
		if s.Detail != "" {
			detail = s.Detail
		}
		cmds = append(cmds, paletteCommand{
			label:    label,
			shortcut: detail,
			enabled:  alwaysTrue,
			action: func(app *App) {
				if t := app.activeTabPtr(); t == nil || t.Path != path {
					app.openFile(path)
				}
				if t := app.activeTabPtr(); t != nil {
					t.MoveCursorTo(posAtLine(line), false)
				}
			},
		})
	}
	a.openPaletteWith(cmds, "Go to symbol")
}

// menuCodeActions asks what can be fixed at the cursor. Bound to Esc l — the
// lightbulb, which is the icon VS Code uses for exactly this.
func (a *App) menuCodeActions() {
	a.closeMenu()
	path, pos, ok := a.lspCursorPos()
	if !ok {
		a.flash("No language server for this file")
		return
	}
	rng := lsp.Range{Start: pos, End: pos}
	// Only the diagnostics that actually cover the cursor: servers key their
	// quick fixes off them, so sending the whole file offers fixes for problems
	// somewhere else entirely.
	var here []lsp.Diagnostic
	for _, d := range a.diagnosticsFor(path) {
		if d.Range.Start.Line == pos.Line {
			here = append(here, d)
		}
	}
	mgr := a.lsp
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		actions, err := mgr.CodeAction(ctx, path, rng, here)
		if err != nil {
			actions = nil
		}
		a.post(&codeActionsEvent{actions: actions, when: time.Now()})
	}()
	a.flash("Looking for fixes…")
}

// handleCodeActions offers the fixes through the palette and applies the chosen
// one with the same disk-writing path a rename uses.
func (a *App) handleCodeActions(e *codeActionsEvent) {
	if len(e.actions) == 0 {
		a.flash("No fixes available here")
		return
	}
	cmds := make([]paletteCommand, 0, len(e.actions))
	for _, act := range e.actions {
		edits := act.Edits
		title := act.Title
		kind := act.Kind
		cmds = append(cmds, paletteCommand{
			label:    title,
			shortcut: kind,
			enabled:  alwaysTrue,
			action: func(app *App) {
				if t := app.activeTabPtr(); t != nil && t.Dirty {
					app.flash("Save first — a fix rewrites files on disk")
					return
				}
				files, count, err := applyWorkspaceEdits(edits)
				if err != nil {
					app.flash("Fix failed: " + err.Error())
					return
				}
				for _, t := range app.tabs {
					if t.Path == "" || t.Synthetic {
						continue
					}
					if _, touched := edits[t.Path]; touched {
						_ = t.Reload()
					}
				}
				app.refreshGitStatus()
				app.flash(fmt.Sprintf("%s — %d edit(s) in %d file(s)", title, count, files))
			},
		})
	}
	a.openPaletteWith(cmds, "Fix")
}

// hasLSPFile gates the menu rows that need a language server.
func (a *App) hasLSPFile() bool {
	t := a.activeTabPtr()
	return t != nil && t.Path != "" && !t.IsImage() && !t.Synthetic
}
