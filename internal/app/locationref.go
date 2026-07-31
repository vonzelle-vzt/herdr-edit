// =============================================================================
// File: internal/app/locationref.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// locationref.go widens Esc e from "jump out of a diff" to "jump from
// wherever a location reference sits under the cursor" — a diff tab, a
// references list (handleReferences in refactor.go), or any other tab whose
// current line happens to name a file:line:col.
//
// Bug 1 in this fork's history: handleReferences built a synthetic tab of
// "rel:line:col" rows and told the user "Esc p opens any of these" — but Esc p
// is openFinder, the fuzzy FILENAME finder, which takes no line number. The
// list was decorative. menuGoToLocation is the real caller those rows needed.
//
// Bug 2: menuJumpToDiffSource's old guard (tab.Synthetic && a.diffSource !=
// "") stayed true for the rest of the session after ANY Esc o, because
// a.diffSource is set once and never cleared. Opening a references list after
// that made the guard pass on a non-diff synthetic tab, running diff-hunk
// parsing over reference rows. Requiring the tab's Label to end in ".diff" —
// checked here, in the dispatcher, before menuJumpToDiffSource is ever called
// — is what actually distinguishes a diff tab from every other synthetic tab.

package app

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/cloudmanic/spice-edit/internal/editor"
	"github.com/cloudmanic/spice-edit/internal/state"
)

// locationRefAt extracts a "path[:line[:col]]" reference from one line of
// text, the shape handleReferences prints and --open-at accepts. ok is false
// when the row names no location — state.SplitLocation always succeeds (it
// defaults line/col to 1 on ordinary text), so "no explicit numeric suffix was
// stripped" is what actually tells a location row apart from a line of code
// or the list's own header line. Returning false here is what stops the
// caller from jumping somewhere arbitrary.
func locationRefAt(row string) (path string, line, col int, ok bool) {
	row = strings.TrimSpace(row)
	if row == "" {
		return "", 0, 0, false
	}
	p, l, c := state.SplitLocation(row)
	if p == row {
		// Nothing numeric was trimmed off the end — this line named no
		// location, whatever else it contains.
		return "", 0, 0, false
	}
	if p == "" {
		return "", 0, 0, false
	}
	return p, l, c, true
}

// resolveRef turns a reference string (project-relative, like the rows
// handleReferences prints, or absolute) into an existing absolute path, or
// ok=false when nothing on disk matches. Relative references are resolved
// against a.rootDir, not the process cwd — the editor may be running from
// anywhere, but rootDir is always the project the reference was written
// relative to.
func (a *App) resolveRef(ref string) (string, bool) {
	if ref == "" {
		return "", false
	}
	candidate := ref
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(a.rootDir, candidate)
	}
	info, err := os.Stat(candidate)
	if err != nil || info.IsDir() {
		return "", false
	}
	return candidate, true
}

// menuGoToLocation is the single "go to location under cursor" action bound
// to Esc e. It replaces the narrower "jump out of a diff" binding
// (menuJumpToDiffSource still does that job, now as an internal branch here)
// so the same key works from a diff, a references list, or any other tab
// whose current line names a file.
//
// Dispatch order matters: a diff tab is checked FIRST and by a stronger test
// than the old one (tab.Synthetic && a.diffSource != "") — requiring the
// tab's Label to end in ".diff" is what stops a stale a.diffSource from a
// previous Esc o hijacking an unrelated synthetic tab (bug 2). Only when that
// fails do we fall through to treating the current line as a plain
// file:line:col reference.
func (a *App) menuGoToLocation() {
	a.closeMenu()
	tab := a.activeTabPtr()
	if tab == nil {
		a.flash("No file:line on this line")
		return
	}

	if tab.Synthetic && strings.HasSuffix(tab.Label, ".diff") && a.diffSource != "" {
		a.menuJumpToDiffSource()
		return
	}

	path, line, col, ok := locationRefAt(tab.LineText(tab.Cursor.Line))
	if !ok {
		a.flash("No file:line on this line")
		return
	}
	abs, ok := a.resolveRef(path)
	if !ok {
		a.flash("Cannot find " + path)
		return
	}
	a.openFile(abs)
	if t := a.activeTabPtr(); t != nil {
		l, c := line-1, col-1
		if l < 0 {
			l = 0
		}
		if c < 0 {
			c = 0
		}
		t.MoveCursorTo(editor.Position{Line: l, Col: c}, false)
	}
}

// hasJumpableTab gates the "Go to location under cursor" menu row. It only
// needs an active tab — whether that tab's current line actually names a
// location is decided at press time by menuGoToLocation itself, the same way
// hasFileTab and hasFindable stay coarse rather than re-deriving the action's
// own logic.
func (a *App) hasJumpableTab() bool {
	return a.activeTabPtr() != nil
}
