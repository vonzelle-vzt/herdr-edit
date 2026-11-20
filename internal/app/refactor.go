// =============================================================================
// File: internal/app/refactor.go
// Author: Vonzelle Brown
// Created: 2026-07-30
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// refactor.go wires the two LSP requests that change or survey code across the
// whole project: rename a symbol (Esc r is taken by redo, so Esc n... also
// taken — this uses the menu plus Esc y) and find references (Esc j).
//
// 🔴 THE CALLER IS THE FEATURE. This fork has shipped a complete, tested,
// capability-advertised LSP request with no call site THREE times now — hover,
// definition, and then find/replace outside LSP. Adding References and Rename
// to the client was twenty minutes; this file is the part that makes them real,
// and refactor_test.go asserts both leader keys resolve.
//
// Rename applies edits itself rather than handing them to the user, and that is
// the risky half: a rename spans files that may not be open, so the edits are
// applied to files ON DISK. Files already open in a tab are reloaded afterwards
// so the buffer can never disagree with what was written underneath it.

package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cloudmanic/spice-edit/internal/lsp"
)

// referencesEvent and renameEvent deliver their answers as posted events, like
// every other LSP action here — calling inline would block the event loop on a
// language server.
type referencesEvent struct {
	locs []lsp.Location
	sym  string
	when time.Time
}

func (e *referencesEvent) When() time.Time { return e.when }

type renameEvent struct {
	edits   map[string][]lsp.TextEdit
	newName string
	err     error
	when    time.Time
}

func (e *renameEvent) When() time.Time { return e.when }

// menuFindReferences asks where the symbol under the cursor is used.
func (a *App) menuFindReferences() {
	a.closeMenu()
	path, pos, ok := a.lspCursorPos()
	if !ok {
		a.flash("No language server for this file")
		return
	}
	sym := a.symbolAtCursor()
	mgr := a.lsp
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		locs, err := mgr.References(ctx, path, pos)
		if err != nil {
			locs = nil
		}
		a.post(&referencesEvent{locs: locs, sym: sym, when: time.Now()})
	}()
	a.flash("Finding references…")
}

// handleReferences shows the hits as a synthetic tab — the same mechanism the
// diff view uses, so the list scrolls, searches and closes like any other tab
// instead of needing a bespoke result pane.
func (a *App) handleReferences(e *referencesEvent) {
	if len(e.locs) == 0 {
		a.flash("No references found")
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d reference(s) to %s\n\n", len(e.locs), e.sym)
	for _, loc := range e.locs {
		p := lsp.PathFromURI(loc.URI)
		if p == "" {
			continue
		}
		rel := p
		if r, err := filepath.Rel(a.rootDir, p); err == nil && !strings.HasPrefix(r, "..") {
			rel = r
		}
		fmt.Fprintf(&b, "%s:%d:%d\n", rel, loc.Range.Start.Line+1, loc.Range.Start.Character+1)
	}
	a.tabs = append(a.tabs, editorSyntheticTab("references: "+e.sym+".txt", b.String()))
	a.activeTab = len(a.tabs) - 1
	a.flash("Esc e opens the one under the cursor; Esc w closes the list")
}

// menuRenameSymbol prompts for the new name and applies the server's edits.
func (a *App) menuRenameSymbol() {
	a.closeMenu()
	if _, _, ok := a.lspCursorPos(); !ok {
		a.flash("No language server for this file")
		return
	}
	cur := a.symbolAtCursor()
	a.openPrompt("Rename symbol", "new name for "+cur, cur, (*App).renameSubmit)
}

// renameSubmit fires the request once the user has typed a name.
func (a *App) renameSubmit(newName string) {
	path, pos, ok := a.lspCursorPos()
	if !ok {
		return
	}
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return
	}
	mgr := a.lsp
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		edits, err := mgr.Rename(ctx, path, pos, newName)
		a.post(&renameEvent{edits: edits, newName: newName, err: err, when: time.Now()})
	}()
	a.flash("Renaming to " + newName + "…")
}

// handleRename applies the workspace edit.
func (a *App) handleRename(e *renameEvent) {
	if e.err != nil {
		a.flash("Rename failed: " + e.err.Error())
		return
	}
	if len(e.edits) == 0 {
		a.flash("The server proposed no edits — the symbol may not be renameable here")
		return
	}
	// Save first. A rename rewrites files on disk, and an unsaved buffer would
	// be silently clobbered by the very next reload.
	if tab := a.activeTabPtr(); tab != nil && tab.Dirty {
		a.flash("Save before renaming — the edit rewrites files on disk")
		return
	}
	files, count, err := applyWorkspaceEdits(e.edits)
	if err != nil {
		a.flash("Rename failed: " + err.Error())
		return
	}
	// Any open tab may now disagree with disk, so pull each one back in.
	for _, t := range a.tabs {
		if t.Path == "" || t.Synthetic {
			continue
		}
		if _, touched := e.edits[t.Path]; touched {
			_ = t.Reload()
		}
	}
	a.refreshGitStatus()
	a.flash(fmt.Sprintf("Renamed to %s — %d edit(s) across %d file(s)", e.newName, count, files))
}

// applyWorkspaceEdits writes a WorkspaceEdit to disk, ALL OR NOTHING. It serves
// both rename and code actions, so there is one applier and one set of rules.
//
// Nothing is written until every file has been read and every edit in every file
// has been validated and applied in memory (lsp.ApplyTextEdits rejects ranges out
// of bounds, backwards, overlapping or off a UTF-16 boundary). The old shape —
// skip an edit that does not fit and write the rest — left a half-applied rename
// or fix with a success message on top. A disk error part-way through the writes
// themselves rolls the files already written back to their original bytes.
//
// Edits arrive sorted descending by position (see lsp.decodeWorkspaceEdit),
// which is what makes applying them safe without recomputing offsets.
func applyWorkspaceEdits(edits map[string][]lsp.TextEdit) (files, count int, err error) {
	paths := make([]string, 0, len(edits))
	for p := range edits {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	type pending struct {
		path          string
		before, after []byte
		mode          os.FileMode
	}
	var plan []pending
	for _, path := range paths {
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return 0, 0, fmt.Errorf("%s: %w", filepath.Base(path), readErr)
		}
		info, statErr := os.Stat(path)
		if statErr != nil {
			return 0, 0, fmt.Errorf("%s: %w", filepath.Base(path), statErr)
		}
		out, applyErr := lsp.ApplyTextEdits(string(raw), edits[path])
		if applyErr != nil {
			return 0, 0, fmt.Errorf("%s: %w (nothing was changed)", filepath.Base(path), applyErr)
		}
		plan = append(plan, pending{path: path, before: raw, after: []byte(out), mode: info.Mode().Perm()})
		count += len(edits[path])
	}

	for i, pf := range plan {
		if writeErr := os.WriteFile(pf.path, pf.after, pf.mode); writeErr != nil {
			for _, done := range plan[:i] {
				_ = os.WriteFile(done.path, done.before, done.mode)
			}
			return 0, 0, fmt.Errorf("%s: %w (earlier files were restored)", filepath.Base(pf.path), writeErr)
		}
	}
	return len(plan), count, nil
}

// symbolAtCursor returns the identifier under the cursor, for prompts and
// headings. Empty when the cursor is not on one.
func (a *App) symbolAtCursor() string {
	tab := a.activeTabPtr()
	if tab == nil {
		return ""
	}
	line := []rune(tab.LineText(tab.Cursor.Line))
	col := tab.Cursor.Col
	if col > len(line) {
		col = len(line)
	}
	start := col
	for start > 0 && isIdentRune(line[start-1]) {
		start--
	}
	end := col
	for end < len(line) && isIdentRune(line[end]) {
		end++
	}
	return string(line[start:end])
}
