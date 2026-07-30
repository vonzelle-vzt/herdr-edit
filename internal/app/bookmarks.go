// =============================================================================
// File: internal/app/bookmarks.go
// Author: Vonzelle Brown
// Created: 2026-07-30
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// bookmarks.go pins file:line locations you want to come back to.
//
// A 401-repo sweep of herdr's marketplace found NO plugin that bookmarks a file
// and a line. The two "harpoon" ports mark panes, tabs and workspaces — which is
// navigation between *windows*, not between *places in the code*. So this is
// genuinely unclaimed ground rather than a reimplementation.
//
// It earns its place in an agent workflow specifically: reading a diff an agent
// produced, you keep finding the three places you want to revisit, and losing
// them the moment you follow a definition somewhere else. Esc m marks, Esc '
// jumps back through the list.
//
// Marks live in memory for the session and are keyed by path AND line, so
// marking the same line twice toggles it off rather than stacking duplicates —
// the behaviour every editor's bookmark toggle has, and the one that makes the
// list stay short enough to be useful.

package app

import (
	"fmt"
	"path/filepath"
	"sort"

	"github.com/cloudmanic/spice-edit/internal/editor"
)

// bookmark is one pinned location.
type bookmark struct {
	Path string
	Line int // 0-based, matching the buffer
}

// menuToggleBookmark marks or unmarks the cursor's line. Bound to Esc m.
func (a *App) menuToggleBookmark() {
	a.closeMenu()
	tab := a.activeTabPtr()
	if tab == nil || tab.IsImage() || tab.Synthetic || tab.Path == "" {
		a.flash("Nothing here to bookmark")
		return
	}
	b := bookmark{Path: tab.Path, Line: tab.Cursor.Line}
	for i, existing := range a.bookmarks {
		if existing == b {
			a.bookmarks = append(a.bookmarks[:i], a.bookmarks[i+1:]...)
			a.flash(fmt.Sprintf("Unmarked %s:%d", filepath.Base(b.Path), b.Line+1))
			return
		}
	}
	a.bookmarks = append(a.bookmarks, b)
	a.sortBookmarks()
	a.flash(fmt.Sprintf("Marked %s:%d  (%d total)", filepath.Base(b.Path), b.Line+1, len(a.bookmarks)))
}

// sortBookmarks keeps the list in a stable, human order — by file, then by line
// — so cycling through it walks a file top to bottom instead of jumping around
// in the order things happened to be marked.
func (a *App) sortBookmarks() {
	sort.SliceStable(a.bookmarks, func(i, j int) bool {
		if a.bookmarks[i].Path != a.bookmarks[j].Path {
			return a.bookmarks[i].Path < a.bookmarks[j].Path
		}
		return a.bookmarks[i].Line < a.bookmarks[j].Line
	})
}

// menuNextBookmark jumps to the next mark, wrapping. Bound to Esc '.
func (a *App) menuNextBookmark() {
	a.closeMenu()
	if len(a.bookmarks) == 0 {
		a.flash("No bookmarks — Esc m marks the cursor's line")
		return
	}
	a.bookmarkIndex = (a.bookmarkIndex + 1) % len(a.bookmarks)
	a.gotoBookmark(a.bookmarks[a.bookmarkIndex])
}

// gotoBookmark opens the file if needed and puts the cursor on the line.
func (a *App) gotoBookmark(b bookmark) {
	tab := a.activeTabPtr()
	if tab == nil || tab.Path != b.Path {
		a.openFile(b.Path)
		tab = a.activeTabPtr()
	}
	if tab == nil {
		a.flash("Could not open " + filepath.Base(b.Path))
		return
	}
	tab.MoveCursorTo(editor.Position{Line: b.Line, Col: 0}, false)
	a.flash(fmt.Sprintf("%s:%d  (%d of %d)", filepath.Base(b.Path), b.Line+1,
		a.bookmarkIndex+1, len(a.bookmarks)))
}

// menuClearBookmarks drops every mark.
func (a *App) menuClearBookmarks() {
	a.closeMenu()
	n := len(a.bookmarks)
	a.bookmarks = nil
	a.bookmarkIndex = -1
	a.flash(fmt.Sprintf("Cleared %d bookmark(s)", n))
}

// hasBookmarks gates the menu rows that only make sense with marks set.
func (a *App) hasBookmarks() bool { return len(a.bookmarks) > 0 }

// bookmarkedLines returns the marked lines for a path, for the gutter.
func (a *App) bookmarkedLines(path string) map[int]bool {
	if len(a.bookmarks) == 0 {
		return nil
	}
	out := make(map[int]bool, len(a.bookmarks))
	for _, b := range a.bookmarks {
		if b.Path == path {
			out[b.Line] = true
		}
	}
	return out
}
