// =============================================================================
// File: internal/app/conflictview.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// conflictview.go is the UI half of merge-conflict support: the body tint that
// says which side is which, and the menu rows that resolve one.
//
// The tint is an OVERLAY, drawn after Tab.Render like the diagnostics
// underline and the document highlight, and for the same reason — the render
// path is hot and conflicts are rare, so a file with none pays nothing. It
// draws BETWEEN drawDocumentHighlights and drawDiagnostics: a squiggle under
// an actual mistake has to win the eye over a region tint, which is only
// saying "this block is contested".
//
// The actions live here rather than inline in builtinMenuGroups for the same
// reason debugMenuGroup does: one definition, picked up by the ≡ menu and — via
// paletteCommands deriving from menuLayout — by the command palette for free.
//
// 🔴 There is no new leader rune, and there cannot be one: every lowercase
// letter is bound or reserved (c / x / v belong to the terminal's own
// clipboard), Ctrl- is forbidden fork-wide, and shifted F-keys need
// modifyOtherKeys and are unreliable through a multiplexer. The menu and the
// palette ARE the surface.
package app

import (
	"fmt"

	"github.com/gdamore/tcell/v2"

	"github.com/cloudmanic/spice-edit/internal/editor"
)

// -----------------------------------------------------------------------------
// The body tint
// -----------------------------------------------------------------------------

// drawConflictRegions tints the background of every visible conflict region on
// the active tab, leaving the syntax foreground untouched — the same
// discipline drawDocumentHighlights uses for its match tint.
//
// Skipped for wrapped, synthetic and image tabs. The GLYPH handles wrap for
// free (it goes through Tab.gutterMarker, which renderWrapped also calls), but
// the tint does not: Tab.ScreenPos assumes one buffer line is one screen row,
// which word wrap breaks, and a tint painted on the wrong rows is worse than
// no tint at all — it would claim lines belong to a side they do not.
func (a *App) drawConflictRegions() {
	tab := a.activeTabPtr()
	if tab == nil || tab.IsImage() || tab.Synthetic || tab.Wrap {
		return
	}
	if len(tab.Conflicts) == 0 {
		return
	}
	ex, ey, ew, eh := a.editorRect()

	for _, c := range tab.Conflicts {
		// A region entirely above or below the viewport costs nothing. Without
		// this, a single enormous conflict — a regenerated lockfile, a
		// re-recorded fixture — would walk every one of its lines on every
		// redraw this app performs, mouse motion included, to paint none of
		// them. That is a stall the user reads as the editor hanging.
		if c.End < tab.ScrollY || c.Start >= tab.ScrollY+eh {
			continue
		}
		// Each section is tinted INCLUDING its own opening marker line, so the
		// `<<<<<<< HEAD` row carries the colour of the block it introduces.
		// Under the merge style Base is -1 and the ours section simply runs up
		// to the ======= line instead.
		oursEnd := c.Sep
		if c.Base >= 0 {
			oursEnd = c.Base
		}
		a.tintLines(tab, c.Start, oursEnd-1, a.theme.ConflictOurs, ex, ey, ew, eh)
		if c.Base >= 0 {
			a.tintLines(tab, c.Base, c.Sep-1, a.theme.ConflictBase, ex, ey, ew, eh)
		}
		a.tintLines(tab, c.Sep, c.End, a.theme.ConflictTheirs, ex, ey, ew, eh)
	}
}

// tintLines repaints the background of buffer lines [lo, hi] inclusive,
// clipped to the visible rows. ScreenPos would reject an off-screen line
// anyway; clipping first is what keeps the cost proportional to the viewport
// rather than to the size of the conflict.
func (a *App) tintLines(tab *editor.Tab, lo, hi int, tint tcell.Color, ex, ey, ew, eh int) {
	if lo < tab.ScrollY {
		lo = tab.ScrollY
	}
	if hi >= tab.ScrollY+eh {
		hi = tab.ScrollY + eh - 1
	}
	for line := lo; line <= hi; line++ {
		a.tintLine(tab, line, tint, ex, ey, ew, eh)
	}
}

// tintLine repaints one buffer line's cells with tint, preserving whatever
// rune and foreground are already there.
//
// 🔴 Column math goes through Tab.ScreenPos, no exceptions. It is the only
// arithmetic in this fork that accounts for BOTH the gutter (+1 separator
// column) and a hard tab occupying more than one screen cell — both of which
// have shipped wrong here before, and both of which are invisible on an
// unindented fixture. The per-rune visual width is then taken from
// editor.RuneVisualWidth, the identical call Tab.Render makes to decide how
// many cells a rune covers, so a tab-indented line is tinted across its whole
// indent rather than leaving holes at each tab stop.
func (a *App) tintLine(tab *editor.Tab, line int, tint tcell.Color, ex, ey, ew, eh int) {
	// A region cache one tick behind an external reload can name a line the
	// buffer no longer has. ScreenPos would happily place it — it bounds-checks
	// against the VIEWPORT, not the buffer — and the tint would land on a blank
	// row past the end of the file.
	if line < 0 || line >= tab.Buffer.LineCount() {
		return
	}
	runes := []rune(tab.LineText(line))
	// An empty line still gets one tinted cell. Without it a blank line inside
	// a conflict reads as a gap between two regions rather than part of one.
	cols := len(runes)
	if cols == 0 {
		cols = 1
	}
	visual := 0
	for col := 0; col < cols; col++ {
		width := 1
		if col < len(runes) {
			width = editor.RuneVisualWidth(runes[col], visual)
		}
		dx, dy, ok := tab.ScreenPos(line, col, ew, eh)
		visual += width
		if !ok {
			continue // scrolled out of view on either axis
		}
		for cell := 0; cell < width && dx+cell < ew; cell++ {
			sx, sy := ex+dx+cell, ey+dy
			mainc, combc, existing, _ := a.screen.GetContent(sx, sy)
			a.screen.SetContent(sx, sy, mainc, combc, existing.Background(tint))
		}
	}
}

// -----------------------------------------------------------------------------
// Menu inventory
// -----------------------------------------------------------------------------

// conflictMenuGroup is the action menu's merge-conflict group, and the single
// definition of what the editor can do to a conflict. builtinMenuGroups
// splices it in; paletteCommands picks the rows up for free because it derives
// from menuLayout.
//
// 🔴 Every row uses `visible`, not just `enabled`. The menu is already 33 rows
// and menuModalRect has form on overflowing; a file with no conflict must see
// the menu it has always seen, not seven greyed-out rows pushing Quit off the
// bottom. menuLayout drops a group that filters down to empty, dividers and
// all, so the group vanishes whole.
func conflictMenuGroup() []menuItemDef {
	return []menuItemDef{
		{label: "Take ours (current change)", action: (*App).menuConflictTakeOurs, enabled: (*App).hasConflictAtCursor, visible: (*App).hasConflicts},
		{label: "Take theirs (incoming change)", action: (*App).menuConflictTakeTheirs, enabled: (*App).hasConflictAtCursor, visible: (*App).hasConflicts},
		{label: "Take both", action: (*App).menuConflictTakeBoth, enabled: (*App).hasConflictAtCursor, visible: (*App).hasConflicts},
		{label: "Next conflict", action: (*App).menuConflictNext, enabled: (*App).hasConflicts, visible: (*App).hasConflicts},
		{label: "Previous conflict", action: (*App).menuConflictPrev, enabled: (*App).hasConflicts, visible: (*App).hasConflicts},
		{label: "Resolve all as ours…", action: (*App).menuConflictAllOurs, enabled: (*App).hasConflicts, visible: (*App).hasConflicts},
		{label: "Resolve all as theirs…", action: (*App).menuConflictAllTheirs, enabled: (*App).hasConflicts, visible: (*App).hasConflicts},
	}
}

// hasConflicts reports whether the active tab has any unresolved conflict
// region. This reads Tab.Conflicts, which refreshTabGitState only ever fills
// for a path `git ls-files -u` named — so a file merely CONTAINING markers
// never turns these rows on.
func (a *App) hasConflicts() bool {
	tab := a.activeTabPtr()
	return tab != nil && len(tab.Conflicts) > 0
}

// hasConflictAtCursor reports whether the cursor is inside a conflict region,
// which is the precondition for the three single-region actions.
func (a *App) hasConflictAtCursor() bool {
	tab := a.activeTabPtr()
	if tab == nil {
		return false
	}
	_, _, ok := tab.ConflictAt(tab.Cursor.Line)
	return ok
}

// -----------------------------------------------------------------------------
// Actions
// -----------------------------------------------------------------------------

// menuConflictTakeOurs keeps the current-branch side of the conflict under the
// cursor.
func (a *App) menuConflictTakeOurs() { a.resolveConflictAtCursor(editor.ConflictOurs, "ours") }

// menuConflictTakeTheirs keeps the incoming side of the conflict under the
// cursor.
func (a *App) menuConflictTakeTheirs() { a.resolveConflictAtCursor(editor.ConflictTheirs, "theirs") }

// menuConflictTakeBoth keeps both sides, ours first, dropping only the markers.
func (a *App) menuConflictTakeBoth() { a.resolveConflictAtCursor(editor.ConflictBoth, "both sides") }

// resolveConflictAtCursor resolves the region the cursor is in and reports what
// happened. A refusal is flashed rather than swallowed: Tab.ResolveConflict
// only returns false when the buffer moved under the cached region, and
// silence there would read as the menu row being broken.
func (a *App) resolveConflictAtCursor(choice editor.ConflictChoice, what string) {
	a.closeMenu()
	tab := a.activeTabPtr()
	if tab == nil {
		return
	}
	_, idx, ok := tab.ConflictAt(tab.Cursor.Line)
	if !ok {
		a.flash("Put the cursor inside a conflict first (Next conflict jumps to one)")
		return
	}
	if !tab.ResolveConflict(idx, choice) {
		a.flash("That conflict moved since it was scanned — nothing changed, try again")
		return
	}
	a.afterConflictResolve(tab, fmt.Sprintf("Took %s", what))
}

// menuConflictAllOurs resolves every conflict in the file as ours, behind a
// confirm. Rewriting a whole file's worth of conflicts in one keystroke is
// exactly the sort of action the fork already gates that way.
func (a *App) menuConflictAllOurs() { a.confirmResolveAll(editor.ConflictOurs, "ours") }

// menuConflictAllTheirs resolves every conflict in the file as theirs, behind
// a confirm.
func (a *App) menuConflictAllTheirs() { a.confirmResolveAll(editor.ConflictTheirs, "theirs") }

// confirmResolveAll asks first, then resolves the whole file as one undo step.
func (a *App) confirmResolveAll(choice editor.ConflictChoice, what string) {
	a.closeMenu()
	tab := a.activeTabPtr()
	if tab == nil || len(tab.Conflicts) == 0 {
		return
	}
	n := len(tab.Conflicts)
	a.openConfirm(
		"Resolve all conflicts",
		fmt.Sprintf("Take %s for all %d conflict(s) in this file? One undo reverses it.", what, n),
		func(app *App) {
			t := app.activeTabPtr()
			if t == nil {
				return
			}
			done := t.ResolveAllConflicts(choice)
			if done == 0 {
				app.flash("Nothing left to resolve")
				return
			}
			app.afterConflictResolve(t, fmt.Sprintf("Took %s for %d conflict(s)", what, done))
		},
	)
}

// menuConflictNext moves the cursor to the next conflict below it, wrapping to
// the first.
func (a *App) menuConflictNext() {
	a.closeMenu()
	tab := a.activeTabPtr()
	if tab == nil || len(tab.Conflicts) == 0 {
		return
	}
	for i, c := range tab.Conflicts {
		if c.Start > tab.Cursor.Line {
			a.gotoConflict(tab, i)
			return
		}
	}
	a.gotoConflict(tab, 0)
}

// menuConflictPrev moves the cursor to the previous conflict above it,
// wrapping to the last.
func (a *App) menuConflictPrev() {
	a.closeMenu()
	tab := a.activeTabPtr()
	if tab == nil || len(tab.Conflicts) == 0 {
		return
	}
	for i := len(tab.Conflicts) - 1; i >= 0; i-- {
		if tab.Conflicts[i].End < tab.Cursor.Line {
			a.gotoConflict(tab, i)
			return
		}
	}
	a.gotoConflict(tab, len(tab.Conflicts)-1)
}

// gotoConflict puts the cursor on region idx's opening marker and says which
// one of how many it is, so navigating a file with a dozen of them tells you
// where you are.
func (a *App) gotoConflict(tab *editor.Tab, idx int) {
	if idx < 0 || idx >= len(tab.Conflicts) {
		return
	}
	c := tab.Conflicts[idx]
	tab.MoveCursorTo(editor.Position{Line: c.Start, Col: 0}, false)
	label := c.OursLabel
	if label == "" {
		label = "ours"
	}
	a.flash(fmt.Sprintf("Conflict %d of %d  (line %d, %s vs %s)",
		idx+1, len(tab.Conflicts), c.Start+1, label, theirsLabelOr(c)))
}

// theirsLabelOr returns the incoming side's label, or a readable stand-in when
// git wrote none (some rebase and stash-pop conflicts leave it bare).
func theirsLabelOr(c editor.ConflictRegion) string {
	if c.TheirsLabel == "" {
		return "theirs"
	}
	return c.TheirsLabel
}

// afterConflictResolve settles the UI once a resolution landed. Tab.Dirty,
// StyleStale, cursorMoved and the region rescan are all done inside the editor
// method, so what is left here is telling the user where they now stand.
//
// 🔴 At zero remaining it says what to do NEXT, because that is the step the
// editor deliberately does not take: writing the buffer clears the markers on
// disk, but only `git add` tells git the path is resolved, and an editor that
// staged for you would be making a commit-shaped decision on your behalf.
func (a *App) afterConflictResolve(tab *editor.Tab, msg string) {
	if n := len(tab.Conflicts); n > 0 {
		a.flash(fmt.Sprintf("%s — %d conflict(s) left", msg, n))
		return
	}
	a.flash(msg + " — all resolved. Save, then `git add` to mark it resolved.")
}
