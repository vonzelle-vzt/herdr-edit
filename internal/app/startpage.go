// =============================================================================
// File: internal/app/startpage.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-07-29
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

package app

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gdamore/tcell/v2"

	"github.com/cloudmanic/spice-edit/internal/filetree"
)

// The start page replaces the old "No file open" placeholder.
//
// Why: with no tab open, the editor pane — often the largest region on screen — rendered two lines
// of centred grey text and nothing else. In a side panel that is most of the width of the terminal
// spent saying "nothing here". VS Code puts recent files and source-control state in exactly this
// space, and we already have the interesting half in memory: refreshGitStatus keeps
// a.tree.DirtyFiles up to date for the tree's colouring, so listing changed files costs no
// additional git call and cannot make startup slower.
//
// Everything here is best-effort and degrades quietly: no repo, no changes, or a pane too narrow
// all fall back to a plain hint rather than an error.

// startPageMaxRows caps the changed-file list. Past a handful of entries this stops being a
// glanceable summary and the fuzzy finder is the better tool, so we say so instead of scrolling.
const startPageMaxRows = 12

// startRow maps a rendered line back to the file it names, so a click can open it. Rebuilt on
// every draw because the dirty set changes underneath us on the 10s git refresh.
type startRow struct {
	y    int
	path string
}

// changedFiles returns the repo-relative dirty paths, most interesting first.
func (a *App) changedFiles() []string {
	if a.tree == nil || len(a.tree.DirtyFiles) == 0 {
		return nil
	}
	out := make([]string, 0, len(a.tree.DirtyFiles))
	for p := range a.tree.DirtyFiles {
		out = append(out, p)
	}
	// Deterministic order. Sorting by path rather than mtime keeps the list stable between the
	// 10-second refreshes — a list that reshuffles under the pointer is worse than an arbitrary
	// but fixed order.
	sort.Strings(out)
	return out
}

// gitMark maps a change kind to the single-letter status column and its colour. Letters match
// git's own porcelain vocabulary so the column reads the same as `git status --short`, and the
// colours are pulled from the theme so a restyle cannot leave this view behind.
func gitMark(k filetree.GitChangeKind) rune {
	switch k {
	case filetree.GitChangeAdded:
		return 'A'
	case filetree.GitChangeDeleted:
		return 'D'
	case filetree.GitChangeRenamed:
		return 'R'
	case filetree.GitChangeMixed:
		return '*'
	case filetree.GitChangeModified:
		return 'M'
	}
	return ' '
}

func (a *App) gitMarkColor(k filetree.GitChangeKind) tcell.Color {
	switch k {
	case filetree.GitChangeAdded:
		return a.theme.GitAdded
	case filetree.GitChangeDeleted:
		return a.theme.GitDeleted
	case filetree.GitChangeRenamed:
		return a.theme.GitRenamed
	case filetree.GitChangeMixed:
		return a.theme.GitMixed
	}
	return a.theme.GitModified
}

// drawStartPage paints the no-tabs-open view: where you are, what has changed, and how to open
// something. Returns nothing; click targets are recorded in a.startRows for handleStartPageClick.
func (a *App) drawStartPage() {
	ex, ey, ew, eh := a.editorRect()
	a.startRows = a.startRows[:0]

	bg := a.theme.BG
	muted := tcell.StyleDefault.Background(bg).Foreground(a.theme.Muted)
	title := tcell.StyleDefault.Background(bg).Foreground(a.theme.Text).Bold(true)
	accent := tcell.StyleDefault.Background(bg).Foreground(a.theme.Accent)

	for cy := ey; cy < ey+eh; cy++ {
		for cx := ex; cx < ex+ew; cx++ {
			a.screen.SetContent(cx, cy, ' ', nil, muted)
		}
	}

	put := func(x, y int, s string, st tcell.Style) {
		if y < ey || y >= ey+eh {
			return
		}
		for i, r := range s {
			if x+i >= ex+ew {
				return
			}
			a.screen.SetContent(x+i, y, r, nil, st)
		}
	}

	changed := a.changedFiles()
	left := ex + 2
	if ew < 34 {
		// Too narrow for a list. Centre a single hint rather than rendering a ragged column of
		// truncated paths — this is the sliver-of-a-pane case, and less is more legible.
		msg := "Esc p to open a file"
		if ew < len(msg)+2 {
			msg = "Esc p"
		}
		put(ex+(ew-len(msg))/2, ey+eh/2, msg, muted)
		a.screen.HideCursor()
		return
	}

	y := ey + 1
	name := filepath.Base(a.rootDir)
	if name == "" || name == "." || name == string(filepath.Separator) {
		name = a.rootDir
	}
	put(left, y, name, title)
	if a.gitBranch != "" {
		put(left+len(name)+1, y, "on "+a.gitBranch, muted)
	}
	y += 2

	if len(changed) == 0 {
		if a.gitBranch != "" {
			put(left, y, "No uncommitted changes", muted)
		} else {
			put(left, y, "Not a git repository", muted)
		}
		y += 2
	} else {
		heading := fmt.Sprintf("%d changed file", len(changed))
		if len(changed) != 1 {
			heading += "s"
		}
		put(left, y, heading, title)
		y++

		shown := changed
		if len(shown) > startPageMaxRows {
			shown = shown[:startPageMaxRows]
		}
		for _, abs := range shown {
			if y >= ey+eh-3 {
				break
			}
			rel := relativeToRoot(a.rootDir, abs)
			kind := a.tree.DirtyFiles[abs]
			markStyle := tcell.StyleDefault.Background(bg).Foreground(a.gitMarkColor(kind))
			put(left, y, string(gitMark(kind)), markStyle)
			// Truncate from the LEFT so the filename — the part you scan for — always survives.
			room := ew - 4 - (left - ex)
			if room > 0 && len(rel) > room {
				rel = "..." + rel[len(rel)-room+3:]
			}
			put(left+2, y, rel, accent)
			a.startRows = append(a.startRows, startRow{y: y, path: abs})
			y++
		}
		if len(changed) > len(shown) {
			put(left, y, fmt.Sprintf("... and %d more", len(changed)-len(shown)), muted)
			y++
		}
		y++
	}

	for _, hint := range []string{
		"Esc p   find a file",
		"Esc f   find in this file",
		"click   open from the tree",
	} {
		if y >= ey+eh {
			break
		}
		put(left, y, hint, muted)
		y++
	}
	a.screen.HideCursor()
}

// handleStartPageClick opens a changed file when its row is clicked. Reports whether the click was
// consumed, so the caller can fall through to normal editor handling when it was not.
func (a *App) handleStartPageClick(x, y int) bool {
	if a.activeTabPtr() != nil {
		return false // a tab is open; the start page is not on screen
	}
	ex, _, ew, _ := a.editorRect()
	if x < ex || x >= ex+ew {
		return false
	}
	for _, r := range a.startRows {
		if r.y == y {
			a.openFile(r.path)
			return true
		}
	}
	return false
}

// relativeToRoot renders a changed file's path the way a reader expects:
// relative to the project, and never as a chain of "..".
//
// 🔴 filepath.Rel alone is not enough, because the two paths can describe the
// same place through different symlinks. On macOS /tmp IS /private/tmp, so a
// project opened as /tmp/demo against a git status reporting /private/tmp/demo
// produced "../../../../private/tmp/demo/src/api/routes.go" — technically
// correct, useless to read, and it silently pushes the actual filename off the
// end of the line. Resolve both sides first, and if the answer still escapes
// the root, show the absolute path rather than a ladder of dots.
func relativeToRoot(root, abs string) string {
	resolve := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return p
	}
	if rel, err := filepath.Rel(resolve(root), resolve(abs)); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	if rel, err := filepath.Rel(root, abs); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return abs
}
