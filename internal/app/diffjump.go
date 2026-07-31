// =============================================================================
// File: internal/app/diffjump.go
// Author: Vonzelle Brown
// Created: 2026-07-30
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// diffjump.go turns the diff view into a navigable index of the change:
// put the cursor on any line of the diff and press Esc e to land on that exact
// line in the real file.
//
// This is the "cursor selection on the diff" the Review panel was missing. That
// panel takes typed `path:line` references because a bash panel has no cursor to
// select with; the editor does, and a diff tab is just a buffer, so the cursor
// is already there. All that was missing is the arithmetic that maps a row of
// unified diff text back to a source line.
//
// 🔴 The mapping is not "count the lines". A unified diff interleaves context,
// additions and deletions, and only two of those three exist in the new file:
//
//	@@ -12,7 +12,9 @@     <- hunk header: the NEW file resumes at line 12
//	 unchanged            <- advances the new-file counter
//	-removed              <- exists ONLY in the old file: does NOT advance it
//	+added                <- advances it
//
// Counting rows instead of honouring that distinction drifts by one line for
// every deletion above the cursor, which is invisible on a small diff and
// wrong by dozens on a real one.

package app

import (
	"strconv"
	"strings"

	"github.com/cloudmanic/spice-edit/internal/editor"
)

// posAtLine is the start of a line, as a Position.
func posAtLine(line int) editor.Position {
	if line < 0 {
		line = 0
	}
	return editor.Position{Line: line, Col: 0}
}

// diffLineTarget maps a zero-based row in unified diff text to the one-based
// line number it refers to in the NEW file. ok is false for rows that name no
// source line at all — the `diff --git` preamble, hunk headers themselves, and
// anything before the first hunk.
//
// Deleted lines map to the position where they USED to be, which is the line
// that now sits there. That is what you want when reviewing: putting the cursor
// on a removed line and jumping takes you to where it was removed from.
func diffLineTarget(lines []string, row int) (int, bool) {
	if row < 0 || row >= len(lines) {
		return 0, false
	}
	newLine := 0
	started := false

	for i := 0; i <= row; i++ {
		line := lines[i]
		if strings.HasPrefix(line, "@@") {
			n, ok := parseHunkNewStart(line)
			if !ok {
				continue
			}
			newLine = n
			started = true
			if i == row {
				// The header itself points at the first line of its hunk.
				return newLine, true
			}
			continue
		}
		if !started {
			continue
		}
		// File headers inside a multi-file diff reset the run.
		if strings.HasPrefix(line, "diff --git") || strings.HasPrefix(line, "--- ") ||
			strings.HasPrefix(line, "+++ ") || strings.HasPrefix(line, "index ") {
			if i == row {
				return 0, false
			}
			continue
		}
		if i == row {
			return newLine, true
		}
		switch {
		case strings.HasPrefix(line, "-"):
			// Old file only — the new-file counter does not advance.
		case strings.HasPrefix(line, "\\"):
			// "\ No newline at end of file" is a note, not a line.
		default:
			// Context (" ") and additions ("+") both exist in the new file.
			newLine++
		}
	}
	if !started {
		return 0, false
	}
	return newLine, true
}

// parseHunkNewStart pulls the new-file start line out of "@@ -a,b +c,d @@".
// The counts are optional in the format (a bare "+c" means one line), which is
// why this parses rather than splitting on commas.
func parseHunkNewStart(header string) (int, bool) {
	plus := strings.Index(header, "+")
	if plus < 0 {
		return 0, false
	}
	rest := header[plus+1:]
	end := strings.IndexAny(rest, " ,")
	if end >= 0 {
		rest = rest[:end]
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// menuJumpToDiffSource opens the real file at the line under the cursor in a
// diff tab. No longer bound to Esc e directly — menuGoToLocation
// (locationref.go) is the Esc e handler now, and calls this as its diff
// branch only after confirming the active tab's Label ends in ".diff". That
// extra check is what stops a stale a.diffSource from a previous Esc o
// hijacking a later, unrelated synthetic tab (this file's own guard below
// only checks a.diffSource != "", which is not enough on its own).
func (a *App) menuJumpToDiffSource() {
	a.closeMenu()
	tab := a.activeTabPtr()
	if tab == nil || !tab.Synthetic || a.diffSource == "" {
		a.flash("Not a diff — Esc o opens the changes for this file")
		return
	}
	line, ok := diffLineTarget(tab.Buffer.Lines, tab.Cursor.Line)
	if !ok {
		a.flash("No source line here")
		return
	}
	target := a.diffSource
	a.openFile(target)
	if t := a.activeTabPtr(); t != nil {
		t.MoveCursorTo(posAtLine(line-1), false)
		a.flash("Jumped to line " + strconv.Itoa(line))
	}
}

// hasDiffTab reports whether the active tab is a diff we can jump out of. No
// longer the menu row's enable predicate (hasJumpableTab is, since Esc e now
// covers non-diff tabs too) — kept for menuJumpToDiffSource's own tests and
// as a building block if a future action needs "is this specifically a diff"
// rather than "is there anything to jump from".
func (a *App) hasDiffTab() bool {
	tab := a.activeTabPtr()
	return tab != nil && tab.Synthetic && a.diffSource != ""
}
