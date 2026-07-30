// =============================================================================
// File: internal/app/palette.go
// Author: Vonzelle Brown
// Created: 2026-07-30
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// palette.go is the command palette: fuzzy search over every action the editor
// can perform, opened with Esc k.
//
// It deliberately builds its command list from menuLayout() rather than keeping
// a list of its own. The menu is already the canonical inventory of actions —
// it carries the label, the shortcut to advertise, and the enablement predicate
// that decides whether an action makes sense right now. A second list would be
// a second thing to forget to update, and the palette would then silently offer
// a command the menu had dropped, or miss one the menu had gained.
//
// Matching reuses internal/finder's scorer for the same reason: the file finder
// already defines what "fuzzy" means in this editor, down to which characters
// get highlighted. Two different notions of fuzzy in one program is a bug the
// user experiences as inconsistency.

package app

import (
	"github.com/gdamore/tcell/v2"

	"github.com/cloudmanic/spice-edit/internal/finder"
)

const (
	paletteMaxWidth      = 72
	paletteResultsMax    = 10
	paletteMinModalWidth = 30
)

// paletteCommand is one invocable action, flattened out of the menu.
type paletteCommand struct {
	label    string
	shortcut string
	action   func(*App)
	enabled  func(*App) bool
}

// paletteResult pairs a command with its fuzzy score and the rune indexes that
// matched, so the renderer can highlight exactly the characters the user typed.
type paletteResult struct {
	cmd   paletteCommand
	score int
	hits  []int
}

// paletteCommands flattens the action menu into a searchable command list.
// Disabled rows are kept rather than filtered: showing "Redo" greyed out when
// there is nothing to redo tells the user the command exists and why it will
// not fire, which is more useful than an empty result list that looks like a
// typo.
func (a *App) paletteCommands() []paletteCommand {
	// An override turns the palette into a general-purpose picker — the outline
	// and the code-action list both use it. Reusing it rather than writing a
	// third list widget keeps fuzzy matching, keyboard selection and mouse
	// clicks defined in exactly one place.
	if a.paletteOverride != nil {
		return a.paletteOverride
	}
	items, _, _ := a.menuLayout()
	out := make([]paletteCommand, 0, len(items))
	for _, it := range items {
		label := it.label
		if it.labelFor != nil {
			label = it.labelFor(a)
		}
		if label == "" || it.action == nil {
			continue
		}
		enabled := it.enabled
		if enabled == nil {
			enabled = alwaysTrue
		}
		out = append(out, paletteCommand{
			label:    label,
			shortcut: it.shortcut,
			action:   it.action,
			enabled:  enabled,
		})
	}
	return out
}

// openPaletteWith opens the palette over an explicit command list instead of
// the action menu, titled for whatever it is picking from.
func (a *App) openPaletteWith(cmds []paletteCommand, title string) {
	if len(cmds) == 0 {
		return
	}
	a.closeAllModals()
	a.paletteOpen = true
	a.paletteOverride = cmds
	a.paletteTitle = title
	a.paletteQuery = nil
	a.paletteCursor = 0
	a.paletteSelected = 0
	a.paletteScroll = 0
	a.refreshPaletteResults()
}

// openCommandPalette shows the palette with an empty query. Bound to Esc k and
// to a menu row of its own.
func (a *App) openCommandPalette() {
	a.closeAllModals()
	a.paletteOpen = true
	a.paletteOverride = nil
	a.paletteTitle = ""
	a.paletteQuery = nil
	a.paletteCursor = 0
	a.paletteSelected = 0
	a.paletteScroll = 0
	a.refreshPaletteResults()
}

// closePalette hides the palette and drops its state.
func (a *App) closePalette() {
	a.paletteOpen = false
	a.paletteQuery = nil
	a.paletteCursor = 0
	a.paletteSelected = 0
	a.paletteScroll = 0
	a.paletteResults = nil
	a.paletteOverride = nil
	a.paletteTitle = ""
}

// menuCommandPalette is the action-menu entry point.
func (a *App) menuCommandPalette() {
	a.closeMenu()
	a.openCommandPalette()
}

// refreshPaletteResults re-scores every command against the current query and
// re-sorts. Called on each keystroke; the command list is a few dozen entries,
// so a full rescan is cheaper than any incremental scheme would be to maintain.
func (a *App) refreshPaletteResults() {
	q := string(a.paletteQuery)
	cmds := a.paletteCommands()
	res := make([]paletteResult, 0, len(cmds))
	for _, c := range cmds {
		score, hits := finder.Score(q, c.label)
		if score <= 0 {
			continue
		}
		res = append(res, paletteResult{cmd: c, score: score, hits: hits})
	}
	// Highest score first; ties keep menu order, which is a deliberate grouping
	// (file actions together, history together) rather than an accident.
	for i := 1; i < len(res); i++ {
		for j := i; j > 0 && res[j].score > res[j-1].score; j-- {
			res[j], res[j-1] = res[j-1], res[j]
		}
	}
	a.paletteResults = res
	if a.paletteSelected >= len(res) {
		a.paletteSelected = 0
	}
	a.clampPaletteScroll()
}

// clampPaletteScroll keeps the selected row inside the visible window.
func (a *App) clampPaletteScroll() {
	visible := paletteResultsMax
	if a.paletteSelected < a.paletteScroll {
		a.paletteScroll = a.paletteSelected
	}
	if a.paletteSelected >= a.paletteScroll+visible {
		a.paletteScroll = a.paletteSelected - visible + 1
	}
	if a.paletteScroll < 0 {
		a.paletteScroll = 0
	}
}

// runSelectedPaletteCommand fires the highlighted command, unless it is
// disabled — in which case the palette stays open so the user can pick another
// rather than being dropped back into the editor wondering what happened.
func (a *App) runSelectedPaletteCommand() {
	if a.paletteSelected < 0 || a.paletteSelected >= len(a.paletteResults) {
		return
	}
	cmd := a.paletteResults[a.paletteSelected].cmd
	if !cmd.enabled(a) {
		a.flash(cmd.label + " is not available right now")
		return
	}
	a.closePalette()
	cmd.action(a)
}

// handlePaletteKey dispatches a keystroke while the palette owns the keyboard.
func (a *App) handlePaletteKey(ev *tcell.EventKey) {
	switch ev.Key() {
	case tcell.KeyEsc:
		a.closePalette()
	case tcell.KeyEnter:
		a.runSelectedPaletteCommand()
	case tcell.KeyUp:
		if a.paletteSelected > 0 {
			a.paletteSelected--
			a.clampPaletteScroll()
		}
	case tcell.KeyDown:
		if a.paletteSelected < len(a.paletteResults)-1 {
			a.paletteSelected++
			a.clampPaletteScroll()
		}
	case tcell.KeyLeft:
		if a.paletteCursor > 0 {
			a.paletteCursor--
		}
	case tcell.KeyRight:
		if a.paletteCursor < len(a.paletteQuery) {
			a.paletteCursor++
		}
	case tcell.KeyHome:
		a.paletteCursor = 0
	case tcell.KeyEnd:
		a.paletteCursor = len(a.paletteQuery)
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if a.paletteCursor > 0 {
			a.paletteQuery = append(a.paletteQuery[:a.paletteCursor-1], a.paletteQuery[a.paletteCursor:]...)
			a.paletteCursor--
			a.paletteSelected = 0
			a.refreshPaletteResults()
		}
	case tcell.KeyDelete:
		if a.paletteCursor < len(a.paletteQuery) {
			a.paletteQuery = append(a.paletteQuery[:a.paletteCursor], a.paletteQuery[a.paletteCursor+1:]...)
			a.paletteSelected = 0
			a.refreshPaletteResults()
		}
	case tcell.KeyRune:
		r := ev.Rune()
		if r < 0x20 {
			return
		}
		next := make([]rune, 0, len(a.paletteQuery)+1)
		next = append(next, a.paletteQuery[:a.paletteCursor]...)
		next = append(next, r)
		next = append(next, a.paletteQuery[a.paletteCursor:]...)
		a.paletteQuery = next
		a.paletteCursor++
		a.paletteSelected = 0
		a.refreshPaletteResults()
	}
}

// handlePaletteMouse lets a click pick a row, matching the finder's behaviour
// so the two modals do not feel like different programs.
func (a *App) handlePaletteMouse(x, y int, btn tcell.ButtonMask) {
	if btn&tcell.Button1 == 0 {
		return
	}
	mx, my, mw, _ := a.paletteModalRect()
	row := y - (my + 4)
	if row < 0 || x < mx || x >= mx+mw {
		return
	}
	idx := a.paletteScroll + row
	if idx >= len(a.paletteResults) {
		return
	}
	a.paletteSelected = idx
	a.runSelectedPaletteCommand()
}

// paletteModalRect is the palette's screen rectangle. Anchored in the upper
// third like the file finder, for the same reason: a picker that opens under
// your eyes is faster to read than one centred over the text you were reading.
func (a *App) paletteModalRect() (x, y, w, h int) {
	w = paletteMaxWidth
	if w > a.width-4 {
		w = a.width - 4
	}
	if w < paletteMinModalWidth {
		w = paletteMinModalWidth
	}
	rows := len(a.paletteResults)
	if rows > paletteResultsMax {
		rows = paletteResultsMax
	}
	if rows < 1 {
		rows = 1
	}
	h = rows + 6
	if h > a.height-2 {
		h = a.height - 2
	}
	x = (a.width - w) / 2
	y = (a.height - h) / 3
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	return
}

// drawPalette paints the modal.
//
// Layout (relY):
//
//	0     top border
//	1     title — "Run a command    esc"
//	2     divider
//	3     input          [ query…            7/34 ]
//	4..N  command rows
//	N+1   bottom border
func (a *App) drawPalette() {
	if !a.paletteOpen {
		return
	}
	mx, my, mw, mh := a.paletteModalRect()
	bg := a.theme.LineHL
	bgStyle := tcell.StyleDefault.Background(bg).Foreground(a.theme.Text)
	borderStyle := tcell.StyleDefault.Background(bg).Foreground(a.theme.Subtle)
	titleStyle := tcell.StyleDefault.Background(bg).Foreground(a.theme.Accent).Bold(true)
	mutedStyle := tcell.StyleDefault.Background(bg).Foreground(a.theme.Muted)
	hitStyle := tcell.StyleDefault.Background(bg).Foreground(a.theme.FindCurrent).Bold(true)
	selStyle := tcell.StyleDefault.Background(a.theme.Selection).Foreground(a.theme.Text)

	fillRect(a.screen, mx, my, mw, mh, bgStyle)
	drawBorder(a.screen, mx, my, mw, mh, borderStyle)
	drawHDivider(a.screen, mx, my+2, mw, borderStyle)

	title := " Run a command"
	if a.paletteTitle != "" {
		title = " " + a.paletteTitle
	}
	drawAt(a.screen, mx+1, my+1, title, titleStyle)
	hint := "esc "
	drawAt(a.screen, mx+mw-1-runeLen(hint), my+1, hint, mutedStyle)

	// Input row.
	drawAt(a.screen, mx+2, my+3, "> ", mutedStyle)
	inX := mx + 4
	inW := mw - 6
	for i, r := range a.paletteQuery {
		if i >= inW {
			break
		}
		a.screen.SetContent(inX+i, my+3, r, nil, bgStyle)
	}
	caret := inX + a.paletteCursor
	if caret < inX+inW {
		a.screen.ShowCursor(caret, my+3)
	}

	// Rows.
	visible := mh - 6
	if visible < 1 {
		visible = 1
	}
	for i := 0; i < visible; i++ {
		idx := a.paletteScroll + i
		if idx >= len(a.paletteResults) {
			break
		}
		res := a.paletteResults[idx]
		ry := my + 4 + i
		rowStyle := bgStyle
		if idx == a.paletteSelected {
			rowStyle = selStyle
			for cx := mx + 1; cx < mx+mw-1; cx++ {
				a.screen.SetContent(cx, ry, ' ', nil, rowStyle)
			}
		}
		// A disabled command is dimmed rather than hidden, so the palette
		// answers "does this exist?" as well as "can I run it now?".
		labelStyle := rowStyle
		if !res.cmd.enabled(a) {
			labelStyle = mutedStyle
			if idx == a.paletteSelected {
				labelStyle = tcell.StyleDefault.Background(a.theme.Selection).Foreground(a.theme.Muted)
			}
		}
		a.drawPaletteRow(mx, ry, mw, res, labelStyle, hitStyle, mutedStyle, idx == a.paletteSelected)
	}
}

// drawPaletteRow paints one command: its label with the matched runes
// highlighted, and its shortcut right-aligned.
func (a *App) drawPaletteRow(mx, ry, mw int, res paletteResult, labelStyle, hitStyle, mutedStyle tcell.Style, selected bool) {
	hit := make(map[int]bool, len(res.hits))
	for _, h := range res.hits {
		hit[h] = true
	}
	maxLabel := mw - 4
	if res.cmd.shortcut != "" {
		maxLabel -= runeLen(res.cmd.shortcut) + 2
	}
	x := mx + 2
	for i, r := range []rune(res.cmd.label) {
		if i >= maxLabel {
			break
		}
		st := labelStyle
		if hit[i] && !selected {
			st = hitStyle
		}
		a.screen.SetContent(x+i, ry, r, nil, st)
	}
	if res.cmd.shortcut != "" {
		sx := mx + mw - 2 - runeLen(res.cmd.shortcut)
		drawAt(a.screen, sx, ry, res.cmd.shortcut, mutedStyle)
	}
}
