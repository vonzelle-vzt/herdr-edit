// =============================================================================
// File: internal/app/problems.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// problems.go turns the diagnostics this editor already collects
// (internal/app/diagnostics.go) into something you can actually navigate:
// a jumpable list (Esc ;) and VS Code's F8 muscle memory for "next problem"
// / "previous problem" (also Esc . / Esc ,).
//
// The list is a synthetic tab whose rows read "path:line:col: [sev] message"
// — the exact shape locationRefAt (Esc e's parser) already understands, so
// this file adds no second jump mechanism. It reuses the one that already
// ships for diff views, references and search results.
//
// 🔴 Scope honesty: a language server only reports on documents it has
// opened, so this list is "problems the language servers have reported",
// never "problems in the workspace". Silently presenting a partial scan as
// complete is exactly the failure mode CLAUDE.md warns about elsewhere in
// this fork (search's "truncated" suffix, the diagnostics "empty publish
// clears" rule) — the header says so plainly instead.
package app

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cloudmanic/spice-edit/internal/editor"
	"github.com/cloudmanic/spice-edit/internal/lsp"
)

// problemRef is one navigable diagnostic: a path, a zero-based line, a
// zero-based RUNE column (or 0 when it can't be resolved — see problemColumn),
// the severity, and the first line of the message.
type problemRef struct {
	Path string
	Line int
	Col  int
	Sev  lsp.Severity
	Msg  string
}

// allProblems flattens a.diagnostics into document order: sorted by path
// (a map has no order of its own), then by position within each file — the
// same order handleDiagnostics already sorts each file's slice into. This is
// read fresh on every call rather than cached, because menuNextProblem /
// menuPrevProblem must see the live set even when a previously-opened list
// tab has gone stale (see stepProblem's doc comment).
func (a *App) allProblems() []problemRef {
	if len(a.diagnostics) == 0 {
		return nil
	}
	paths := make([]string, 0, len(a.diagnostics))
	for p := range a.diagnostics {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var out []problemRef
	for _, path := range paths {
		for _, d := range a.diagnostics[path] {
			out = append(out, problemRef{
				Path: path,
				Line: d.Range.Start.Line,
				Col:  a.problemColumn(path, d),
				Sev:  d.Severity,
				Msg:  lsp.FirstLine(d.Message),
			})
		}
	}
	return out
}

// problemColumn resolves a diagnostic's UTF-16 start column to a rune column.
//
// 🔴 The conversion needs the line's actual TEXT (UTF16ToRuneCol counts UTF-16
// code units by walking runes), and the only place that text is available is
// an already-open tab's buffer. Guessing at the conversion without it would
// mean writing a UTF-16 count into a rune-indexed field — a lie that renders,
// per this file's brief. When no tab for path is open, 0 is honest: "column 1"
// on the right line beats a fabricated column on a line no one can see yet.
func (a *App) problemColumn(path string, d lsp.Diagnostic) int {
	for _, t := range a.tabs {
		if t.Path == path {
			return lsp.UTF16ToRuneCol(t.LineText(d.Range.Start.Line), d.Range.Start.Character)
		}
	}
	return 0
}

// renderProblems is the pure rendering step: a header stating the honest
// scope and the count, then one FLAT "path:line:col: [sev] message" row per
// problem — never grouped under per-file headers, because a group heading
// would be a row locationRefAt cannot parse, silently breaking Esc e on it.
// Paths are printed relative to root when possible, matching every other
// jump list in this fork (references, search results).
//
// Kept as a function of (root, []problemRef) — no App — so the layout can be
// pinned down in a test without a screen, the same reasoning renderSearchResults
// documents for itself.
func renderProblems(root string, probs []problemRef) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s — problems the language servers have reported (not a full workspace scan)\n", plural(len(probs), "problem"))
	fmt.Fprintf(&b, "Esc e jumps to the problem under the cursor; Esc w closes this list\n\n")
	for _, p := range probs {
		rel := p.Path
		if r, err := filepath.Rel(root, p.Path); err == nil && !strings.HasPrefix(r, "..") {
			rel = r
		}
		fmt.Fprintf(&b, "%s:%d:%d: [%s] %s\n", rel, p.Line+1, p.Col+1, p.Sev.String(), p.Msg)
	}
	return b.String()
}

// menuProblems opens the navigable problems list as a synthetic tab. Bound to
// Esc ;.
func (a *App) menuProblems() {
	a.closeMenu()
	probs := a.allProblems()
	if len(probs) == 0 {
		a.flash("No problems reported")
		return
	}
	a.tabs = append(a.tabs, editorSyntheticTab("problems.txt", renderProblems(a.rootDir, probs)))
	a.activeTab = len(a.tabs) - 1
	a.flash("Esc e opens the problem under the cursor; Esc w closes the list")
}

// hasProblems gates the problems-list and next/prev-problem menu rows.
// handleDiagnostics deletes a path's entry the moment its diagnostics go
// empty (see diagnostics.go), so a.diagnostics holding any key at all means
// there is something to show.
func (a *App) hasProblems() bool {
	return len(a.diagnostics) > 0
}

// compareProblemPos orders two problem positions in the same document order
// allProblems flattens into: path alphabetically, then line, then column.
// Shared by nextProblemIndex so "where does the cursor sit relative to every
// problem" has exactly one definition.
func compareProblemPos(aPath string, aLine, aCol int, bPath string, bLine, bCol int) int {
	if aPath != bPath {
		if aPath < bPath {
			return -1
		}
		return 1
	}
	if aLine != bLine {
		if aLine < bLine {
			return -1
		}
		return 1
	}
	if aCol != bCol {
		if aCol < bCol {
			return -1
		}
		return 1
	}
	return 0
}

// nextProblemIndex returns the index into probs the cursor should move to
// from (curPath, curLine, curCol), stepping by dir (+1 forward, -1 backward)
// and WRAPPING at either end.
//
// 🔴 This is the arithmetic that must wrap. A naive "advance until the next
// entry is past the cursor" loop returns "not found" once the cursor is past
// every problem and stops there — which reads as F8 doing nothing at
// end-of-file instead of cycling back to the first problem, the behaviour
// every editor with this feature has trained users to expect.
func nextProblemIndex(probs []problemRef, curPath string, curLine, curCol, dir int) int {
	if len(probs) == 0 {
		return -1
	}
	if dir >= 0 {
		for i, p := range probs {
			if compareProblemPos(p.Path, p.Line, p.Col, curPath, curLine, curCol) > 0 {
				return i
			}
		}
		return 0 // wrap to the first problem, first file
	}
	for i := len(probs) - 1; i >= 0; i-- {
		p := probs[i]
		if compareProblemPos(p.Path, p.Line, p.Col, curPath, curLine, curCol) < 0 {
			return i
		}
	}
	return len(probs) - 1 // wrap to the last problem, last file
}

// stepProblem is the shared arithmetic behind menuNextProblem / menuPrevProblem.
// dir is +1 or -1. ok is false only when there are no problems at all.
//
// 🔴 Reads a.diagnostics LIVE via allProblems on every call, not a snapshot of
// an open "problems.txt" tab. That asymmetry is deliberate: a list tab is a
// real editable buffer that can go stale the moment the user types in it or a
// server republishes, while traversal has no such staleness to protect
// against — it just needs the current truth. "Fixing" this by re-reading an
// open list tab instead would make traversal wrong exactly when the list is
// wrong, which is worse.
func (a *App) stepProblem(dir int) (problemRef, bool) {
	probs := a.allProblems()
	if len(probs) == 0 {
		return problemRef{}, false
	}
	var curPath string
	var curLine, curCol int
	if t := a.activeTabPtr(); t != nil && !t.Synthetic {
		curPath, curLine, curCol = t.Path, t.Cursor.Line, t.Cursor.Col
	}
	idx := nextProblemIndex(probs, curPath, curLine, curCol, dir)
	return probs[idx], true
}

// jumpToProblem opens (or switches to) p's file and moves the cursor onto it,
// the same open-then-move sequence menuGoToLocation uses.
func (a *App) jumpToProblem(p problemRef) {
	if t := a.activeTabPtr(); t == nil || t.Path != p.Path {
		a.openFile(p.Path)
	}
	if t := a.activeTabPtr(); t != nil {
		t.MoveCursorTo(editor.Position{Line: p.Line, Col: p.Col}, false)
	}
	a.flash(p.Sev.String() + ": " + p.Msg)
}

// menuNextProblem jumps to the next problem after the cursor, wrapping to the
// first. Bound to Esc . and F8.
func (a *App) menuNextProblem() {
	a.closeMenu()
	p, ok := a.stepProblem(1)
	if !ok {
		a.flash("No problems reported")
		return
	}
	a.jumpToProblem(p)
}

// menuPrevProblem jumps to the problem before the cursor, wrapping to the
// last. Bound to Esc , and Shift+F8.
func (a *App) menuPrevProblem() {
	a.closeMenu()
	p, ok := a.stepProblem(-1)
	if !ok {
		a.flash("No problems reported")
		return
	}
	a.jumpToProblem(p)
}
