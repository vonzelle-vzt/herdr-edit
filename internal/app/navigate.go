// =============================================================================
// File: internal/app/navigate.go
// Author: Vonzelle Brown
// Created: 2026-07-30
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// navigate.go holds the two whole-buffer navigation actions: jump to a line,
// and select everything.
//
// Select-all needed no new logic at all. Tab.SelectAll() has been complete and
// unit-tested in internal/editor since well before this file existed, with
// **zero non-test callers** — a sweep of every exported Tab method found it was
// the only genuinely unreachable one (EnsureVisible, GutterWidth and
// PersistUndo each have a real caller inside the editor package). That is the
// third time this fork has shipped a tested engine nobody could reach, after
// hover/go-to-definition and after find-and-replace. So menuSelectAll below is
// deliberately a one-line call: the behaviour was never missing, the wiring was.

package app

import (
	"strconv"
	"strings"

	"github.com/cloudmanic/spice-edit/internal/editor"
)

// menuSelectAll selects the whole buffer. Bound to Esc a and to a menu row.
func (a *App) menuSelectAll() {
	a.closeMenu()
	tab := a.activeTabPtr()
	if tab == nil || tab.IsImage() {
		return
	}
	tab.SelectAll()
}

// menuGoToLine asks for a line number and jumps there. Bound to Esc g.
//
// The prompt is reused rather than reimplemented — it already owns input
// editing, horizontal scroll and Esc-to-cancel, and a second text field in this
// codebase would be a second set of those bugs.
func (a *App) menuGoToLine() {
	a.closeMenu()
	tab := a.activeTabPtr()
	if tab == nil || tab.IsImage() {
		return
	}
	a.openPrompt("Go to line", "line, or line:column", "", (*App).goToLineSubmit)
}

// goToLineSubmit parses the prompt's answer and moves the cursor. Kept separate
// from menuGoToLine so the prompt callback stays a named method rather than a
// closure over app state.
func (a *App) goToLineSubmit(value string) {
	tab := a.activeTabPtr()
	if tab == nil {
		return
	}
	pos, ok := parseLineRef(value, tab.Buffer.LineCount())
	if !ok {
		a.flash("Not a line number: " + value)
		return
	}
	// MoveCursorTo clamps into the buffer and sets the cursorMoved flag that
	// makes the viewport follow. Assigning Cursor directly from this package is
	// not possible anyway — cursorMoved is unexported.
	tab.MoveCursorTo(pos, false)
}

// parseLineRef turns "N" or "N:C" into a zero-indexed Position, given how many
// lines the buffer has. Returns ok=false for anything unparseable or for a line
// outside the file, so the caller can say so rather than jumping somewhere
// arbitrary.
//
// Pure, and separate from the UI, so every edge case is testable without a
// screen: users count lines from 1 and the buffer counts from 0, which is
// exactly the kind of off-by-one that is invisible until someone jumps to the
// last line of a file.
func parseLineRef(value string, lineCount int) (editor.Position, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return editor.Position{}, false
	}

	lineStr, colStr := value, ""
	if i := strings.IndexByte(value, ':'); i >= 0 {
		lineStr, colStr = value[:i], value[i+1:]
	}

	line, err := strconv.Atoi(strings.TrimSpace(lineStr))
	if err != nil || line < 1 {
		return editor.Position{}, false
	}
	if lineCount > 0 && line > lineCount {
		return editor.Position{}, false
	}

	col := 1
	if strings.TrimSpace(colStr) != "" {
		col, err = strconv.Atoi(strings.TrimSpace(colStr))
		if err != nil || col < 1 {
			return editor.Position{}, false
		}
	}
	return editor.Position{Line: line - 1, Col: col - 1}, true
}
