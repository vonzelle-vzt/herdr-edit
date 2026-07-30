// =============================================================================
// File: internal/app/diffview.go
// Author: Vonzelle Brown
// Created: 2026-07-30
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// diffview.go opens the active file's diff as a real tab: "Open changes",
// bound to Esc o.
//
// It is a SYNTHETIC TAB rather than a new render mode, and that is the whole
// design. Word wrap taught this codebase what a second geometry path costs —
// Render, HitTest, EnsureVisible, clampScroll and every use of ScrollX assume
// one buffer line is one screen row, and a diff view built as a new mode would
// have to re-derive all of it. A tab whose buffer happens to contain unified
// diff text inherits scrolling, selection, search, mouse hit-testing and the
// tab bar for free, and Chroma highlights it because the label ends in ".diff".
//
// The baseline matters as much as the diff. `git diff HEAD` answers "what have
// I not committed", while `git diff <merge-base>` answers "what does this
// branch change" — which is the question you are actually asking when reading
// an agent's work, because the agent has usually committed as it went. Esc o on
// an already-open diff flips between them rather than needing a second key.

package app

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/cloudmanic/spice-edit/internal/editor"
)

// editorSyntheticTab is a thin alias so this file reads in terms of the view it
// is building rather than the package it borrows from.
func editorSyntheticTab(label, body string) *editor.Tab {
	return editor.NewSyntheticTab(label, body)
}

// diffBaseline selects what the working tree is compared against.
type diffBaseline int

const (
	// baselineMergeBase compares against where this branch left its parent —
	// the whole branch's work, committed or not.
	baselineMergeBase diffBaseline = iota
	// baselineHead compares against the last commit — uncommitted work only.
	baselineHead
)

// String names the baseline for the tab label and the status flash.
func (b diffBaseline) String() string {
	if b == baselineHead {
		return "HEAD"
	}
	return "merge-base"
}

// menuOpenChanges opens (or flips the baseline of) the active file's diff.
func (a *App) menuOpenChanges() {
	a.closeMenu()
	tab := a.activeTabPtr()
	if tab == nil {
		return
	}

	// Already looking at a diff? Then this keypress means "the other baseline".
	if tab.Synthetic && a.diffSource != "" {
		if a.diffBaseline == baselineMergeBase {
			a.diffBaseline = baselineHead
		} else {
			a.diffBaseline = baselineMergeBase
		}
		a.showDiff(a.diffSource, true)
		return
	}

	if tab.IsImage() || tab.Path == "" {
		a.flash("No file to diff")
		return
	}
	a.diffSource = tab.Path
	a.showDiff(tab.Path, false)
}

// showDiff builds the diff and puts it on screen. reuse replaces the current
// synthetic tab in place rather than stacking a second one, so flipping the
// baseline does not leave a trail of diff tabs behind.
func (a *App) showDiff(path string, reuse bool) {
	body, err := gitDiffFor(path, a.diffBaseline)
	if err != nil {
		a.flash("Not a git repository")
		return
	}
	if strings.TrimSpace(body) == "" {
		a.flash(fmt.Sprintf("No changes in %s against %s", filepath.Base(path), a.diffBaseline))
		return
	}
	label := fmt.Sprintf("%s ⟷ %s.diff", filepath.Base(path), a.diffBaseline)
	tab := a.activeTabPtr()
	if reuse && tab != nil && tab.Synthetic {
		a.tabs[a.activeTab] = editorSyntheticTab(label, body)
	} else {
		a.tabs = append(a.tabs, editorSyntheticTab(label, body))
		a.activeTab = len(a.tabs) - 1
	}
	a.flash("Diff against " + a.diffBaseline.String() + " — Esc o flips the baseline")
}

// gitDiffFor returns the unified diff of one file against the chosen baseline.
//
// The merge-base is resolved against whichever of the usual trunk names the
// repository actually has; a repo with neither falls back to HEAD rather than
// failing, because a diff against the last commit is still useful and an error
// message here would be one the user cannot act on.
func gitDiffFor(path string, base diffBaseline) (string, error) {
	dir := filepath.Dir(path)
	rev := "HEAD"
	if base == baselineMergeBase {
		if mb := mergeBaseFor(dir); mb != "" {
			rev = mb
		}
	}
	cmd := exec.Command("git", "diff", rev, "--", filepath.Base(path))
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// mergeBaseFor finds where this branch diverged from a trunk, or "" when it
// cannot tell.
func mergeBaseFor(dir string) string {
	for _, trunk := range []string{"origin/main", "origin/master", "main", "master"} {
		cmd := exec.Command("git", "merge-base", "HEAD", trunk)
		cmd.Dir = dir
		if out, err := cmd.Output(); err == nil {
			if sha := strings.TrimSpace(string(out)); sha != "" {
				return sha
			}
		}
	}
	return ""
}
