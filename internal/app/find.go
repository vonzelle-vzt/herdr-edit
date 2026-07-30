// =============================================================================
// File: internal/app/find.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-04-30
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

// find.go owns the in-file search UI: the find bar that lives directly
// above the status bar, the keystroke dispatch while the bar is focused,
// and the Esc-f leader entry point.
//
// The matching and replacing logic itself lives on Tab (see
// internal/editor/find.go) so each tab carries its own query, match list,
// current-index and option toggles. This file only handles UI: the input
// strings, cursors, scroll, focus, and rendering.
//
// The bar is one row for find-only and two rows when the replace field is
// expanded, mirroring VS Code's find widget. Layout for both rows is
// computed by findBarLayout and consumed by BOTH the renderer and the
// mouse hit-test — a second copy of that arithmetic would drift the first
// time a label changed, and a click would then land on a different toggle
// than the one under the pointer.

package app

import (
	"fmt"

	"github.com/gdamore/tcell/v2"
)

// findBarHeight is the cell height of the find bar's query row. Always 1.
// The bar as a whole is findBarH() rows tall, which adds the replace row
// when it is expanded; call sites that mean "the whole bar" must use
// findBarH, not this constant.
const findBarHeight = 1

// findField identifies which of the bar's two text inputs currently owns
// the caret. Editing keys are routed to the focused field, and only the
// query field re-runs the search on every change.
type findField int

const (
	findFieldQuery findField = iota
	findFieldReplace
)

// Bar chrome. The chevron mirrors VS Code's expand control on the left of
// the find widget: collapsed shows a right-pointing arrow, expanded a
// down-pointing one. Toggle labels are VS Code's too, so the icons read
// the same way to anyone coming from there.
const (
	findChevronCollapsed = " › "
	findChevronExpanded  = " ⌄ "
	findLabel            = "Find: "
	replaceLabel         = "Replace: "
	toggleCaseLabel      = "Aa"
	toggleWordLabel      = "ab"
	toggleRegexLabel     = ".*"
	btnReplaceLabel      = "[Replace]"
	btnReplaceAllLabel   = "[All]"
)

// findBarLayout is the resolved geometry of the find bar for one draw.
// Every field is an absolute screen x (or 0 width when the element was
// dropped because the bar is too narrow). The renderer paints from it and
// handleFindMouse hit-tests against it, so what is on screen is exactly
// what a click can reach, by construction.
type findBarLayout struct {
	chevronX, chevronW int
	queryInX, queryInW int

	caseX, caseW   int
	wordX, wordW   int
	regexX, regexW int

	statusX, statusW int // counter or error text
	hintX, hintW     int

	replaceInX, replaceInW int
	btnReplaceX, btnW      int
	btnAllX, btnAllW       int
}

// findBarLayout computes the bar's element positions for a bar starting at
// bx with width bw. Elements are dropped right-to-left as the bar narrows:
// the hint goes first, then the counter, then the toggles — the query
// input is never dropped because it is the entire point of the bar.
func (a *App) findBarLayout(bx, bw int) findBarLayout {
	var l findBarLayout

	chev := findChevronCollapsed
	if a.findReplaceOpen {
		chev = findChevronExpanded
	}
	l.chevronX, l.chevronW = bx, runeLen(chev)

	labelEnd := l.chevronX + l.chevronW + runeLen(findLabel)

	// Right-side chrome is admitted in PRIORITY order but positioned in
	// visual order, and those two are not the same. Toggles win first
	// because with regex on they are the only signal for how the query is
	// being read; the hint is a reminder and goes first when space runs
	// short.
	//
	// Admitting in visual (right-to-left) order instead would let the hint
	// eat the budget before the toggles were considered — which it did:
	// at 120 columns the toggles vanished whenever the longer query-field
	// hint was showing, so they blinked in and out as the user tabbed
	// between the two fields.
	const toggleBlock = 8 // "Aa ab .*"
	const minInput = 16   // the query field must stay usable

	hint := a.findHintText()
	status := a.findStatusText()
	budget := (bx + bw) - labelEnd - minInput

	useToggles := budget >= toggleBlock+1
	if useToggles {
		budget -= toggleBlock + 1
	}
	useStatus := status != "" && budget >= runeLen(status)+2
	if useStatus {
		budget -= runeLen(status) + 2
	}
	useHint := budget >= runeLen(hint)+1

	// Now place them right-to-left: hint is rightmost, then the status,
	// then the toggle block.
	right := bx + bw
	if useHint {
		l.hintW = runeLen(hint)
		right -= l.hintW + 1
		l.hintX = right
	}
	if useStatus {
		l.statusW = runeLen(status)
		right -= l.statusW + 2
		l.statusX = right
	}
	if useToggles {
		right -= toggleBlock + 1
		l.caseX, l.caseW = right, runeLen(toggleCaseLabel)
		l.wordX, l.wordW = right+3, runeLen(toggleWordLabel)
		l.regexX, l.regexW = right+6, runeLen(toggleRegexLabel)
	}

	l.queryInX = labelEnd
	l.queryInW = right - 1 - l.queryInX
	if l.queryInW < 1 {
		l.queryInW = 1
	}

	if !a.findReplaceOpen {
		return l
	}

	// Replace row. The input starts at the same column as the query input
	// so the two fields line up, which is what makes the pair read as one
	// widget rather than two unrelated rows.
	rright := bx + bw
	if bw > runeLen(replaceLabel)+runeLen(btnReplaceLabel)+runeLen(btnReplaceAllLabel)+12 {
		l.btnAllW = runeLen(btnReplaceAllLabel)
		rright -= l.btnAllW + 1
		l.btnAllX = rright
		l.btnW = runeLen(btnReplaceLabel)
		rright -= l.btnW + 1
		l.btnReplaceX = rright
	}
	l.replaceInX = l.chevronX + l.chevronW + runeLen(replaceLabel)
	l.replaceInW = rright - 1 - l.replaceInX
	if l.replaceInW < 1 {
		l.replaceInW = 1
	}
	return l
}

// findBarH is the height of the whole bar: one row normally, two when the
// replace field is expanded. editorRect subtracts this, so the editor body
// gives up exactly the rows the bar paints.
func (a *App) findBarH() int {
	if a.findReplaceOpen {
		return findBarHeight + 1
	}
	return findBarHeight
}

// openFind shows the find bar with an empty input and the caret in the
// query field. We don't pre-fill the user's last query because closing the
// bar already clears find state — Esc means "I'm done searching." Each
// Esc-f opens a fresh search. The replace row's expanded/collapsed state is
// deliberately sticky across opens, the way VS Code remembers it.
func (a *App) openFind() {
	tab := a.activeTabPtr()
	if tab == nil || tab.IsImage() {
		return
	}
	a.closeAllModals() // a modal would otherwise eat our keystrokes
	a.findOpen = true
	a.findValue = nil
	a.findCursor = 0
	a.findScroll = 0
	a.findFocus = findFieldQuery
}

// openReplace opens the bar with the replace row expanded and the caret in
// the replace field. Separate entry point from openFind so the menu and a
// future leader key can land the user directly where they intend to work,
// rather than making them open find and then expand.
func (a *App) openReplace() {
	tab := a.activeTabPtr()
	if tab == nil || tab.IsImage() {
		return
	}
	if !a.findOpen {
		a.openFind()
	}
	a.findReplaceOpen = true
	a.findFocus = findFieldReplace
}

// toggleReplaceRow expands or collapses the replace row. Collapsing while
// the caret is in the replace field moves focus back to the query so the
// caret can never end up on a row that is no longer painted.
func (a *App) toggleReplaceRow() {
	a.findReplaceOpen = !a.findReplaceOpen
	if !a.findReplaceOpen {
		a.findFocus = findFieldQuery
	} else {
		a.findFocus = findFieldReplace
	}
}

// closeFind hides the find bar AND clears the active tab's find state so
// the highlights disappear with the bar. Leaving them painted after close
// is surprising — users expect Esc to mean "I'm done searching."
func (a *App) closeFind() {
	a.findOpen = false
	a.findValue = nil
	a.findCursor = 0
	a.findScroll = 0
	a.replaceValue = nil
	a.replaceCursor = 0
	a.replaceScroll = 0
	a.findFocus = findFieldQuery
	if tab := a.activeTabPtr(); tab != nil {
		tab.ClearFind()
	}
}

// findFocusedField returns pointers to the value/cursor/scroll triple of
// whichever input currently has the caret. Returning pointers lets the
// editing keys in handleFindKey operate on one field without a copy of the
// insert/delete/cursor logic per field.
func (a *App) findFocusedField() (*[]rune, *int, *int) {
	if a.findFocus == findFieldReplace {
		return &a.replaceValue, &a.replaceCursor, &a.replaceScroll
	}
	return &a.findValue, &a.findCursor, &a.findScroll
}

// findApplyQuery pushes the current input text into the active tab's find
// state and snaps the cursor to the new "current" match (so the user can
// see their result while still typing). Called on every change to the
// query field so the highlights track the query live. Editing the replace
// field never re-runs the search.
func (a *App) findApplyQuery() {
	tab := a.activeTabPtr()
	if tab == nil {
		return
	}
	tab.SetFindQuery(string(a.findValue))
	tab.FocusCurrentMatch()
}

// findNext is the Enter-in-the-query-field action: jump to the next match
// (with wrap).
func (a *App) findNext() {
	if tab := a.activeTabPtr(); tab != nil {
		tab.FindNext()
	}
}

// findPrev is the Shift-Enter action: jump to the previous match.
func (a *App) findPrev() {
	if tab := a.activeTabPtr(); tab != nil {
		tab.FindPrev()
	}
}

// findReplaceCurrent replaces the focused match and advances to the next
// one. The tab re-derives its match list after the edit, so the bar's
// counter updates without any bookkeeping here.
func (a *App) findReplaceCurrent() {
	tab := a.activeTabPtr()
	if tab == nil {
		return
	}
	tab.Replace(string(a.replaceValue))
}

// findReplaceAll replaces every current match as a single undo step.
func (a *App) findReplaceAll() {
	tab := a.activeTabPtr()
	if tab == nil {
		return
	}
	tab.ReplaceAll(string(a.replaceValue))
}

// findToggleCase, findToggleWord and findToggleRegex flip one search option
// and immediately re-run the query through the tab, so the match count and
// highlights reflect the new mode without the user retyping.
func (a *App) findToggleCase() {
	if tab := a.activeTabPtr(); tab != nil {
		tab.SetFindOptions(!tab.FindCaseSensitive, tab.FindWholeWord, tab.FindRegex)
	}
}

func (a *App) findToggleWord() {
	if tab := a.activeTabPtr(); tab != nil {
		tab.SetFindOptions(tab.FindCaseSensitive, !tab.FindWholeWord, tab.FindRegex)
	}
}

func (a *App) findToggleRegex() {
	if tab := a.activeTabPtr(); tab != nil {
		tab.SetFindOptions(tab.FindCaseSensitive, tab.FindWholeWord, !tab.FindRegex)
	}
}

// menuFind is the action menu entry point. Behaves identically to the
// Esc-f leader — opens the bar against the active tab.
func (a *App) menuFind() {
	a.closeMenu()
	a.openFind()
}

// menuReplace is the action menu entry point for find-and-replace. The
// menu is the primary surface for it: macOS Terminal + tmux swallows
// right-click, and the bar's chevron is a mouse-only affordance, so the
// feature has to be reachable here too.
func (a *App) menuReplace() {
	a.closeMenu()
	a.openReplace()
}

// hasFindable reports whether the active tab is a text tab — used to gray
// out the menu row on image tabs / no-tab states.
func (a *App) hasFindable() bool {
	t := a.activeTabPtr()
	return t != nil && !t.IsImage()
}

// findBarRect returns the on-screen rectangle of the whole find bar,
// including the replace row when it is expanded. Caller is expected to
// check a.findOpen before drawing.
func (a *App) findBarRect() (x, y, w, h int) {
	sw := a.sidebarW()
	h = a.findBarH()
	return sw, a.height - 1 - h, a.width - sw, h
}

// handleFindKey dispatches a keystroke while the find bar is focused.
// Behavior:
//
//	Esc                     close the bar
//	Tab                     expand/focus the replace field, then swap fields
//	Enter (query)           jump to the next match
//	Shift+Enter (query)     jump to the previous match
//	Enter (replace)         replace the current match and advance
//	Shift+Enter (replace)   replace every match, as one undo step
//	Alt+c / Alt+w / Alt+r   toggle case / whole-word / regex
//	Backspace / Delete      edit the focused input
//	Left / Right / Home/End cursor movement inside the focused input
//	printable rune          insert into the focused input
//
// Anything else is dropped on the floor — the find bar owns the keyboard
// while it's open.
func (a *App) handleFindKey(ev *tcell.EventKey) {
	// Alt-modified letters are the option toggles (VS Code's own
	// shortcuts). Checked before the rune branch so they can never be
	// typed into the input. Terminals that don't deliver Alt simply never
	// reach here; the clickable indicators remain the reliable path.
	if ev.Key() == tcell.KeyRune && ev.Modifiers()&tcell.ModAlt != 0 {
		switch ev.Rune() {
		case 'c', 'C':
			a.findToggleCase()
			return
		case 'w', 'W':
			a.findToggleWord()
			return
		case 'r', 'R':
			a.findToggleRegex()
			return
		}
	}

	value, cursor, _ := a.findFocusedField()

	switch ev.Key() {
	case tcell.KeyEsc:
		a.closeFind()
	case tcell.KeyTab:
		if !a.findReplaceOpen {
			a.findReplaceOpen = true
			a.findFocus = findFieldReplace
			return
		}
		if a.findFocus == findFieldQuery {
			a.findFocus = findFieldReplace
		} else {
			a.findFocus = findFieldQuery
		}
	case tcell.KeyEnter:
		shift := ev.Modifiers()&tcell.ModShift != 0
		if a.findFocus == findFieldReplace {
			if shift {
				a.findReplaceAll()
			} else {
				a.findReplaceCurrent()
			}
			return
		}
		if shift {
			a.findPrev()
		} else {
			a.findNext()
		}
	case tcell.KeyLeft:
		if *cursor > 0 {
			*cursor--
		}
	case tcell.KeyRight:
		if *cursor < len(*value) {
			*cursor++
		}
	case tcell.KeyHome:
		*cursor = 0
	case tcell.KeyEnd:
		*cursor = len(*value)
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if *cursor > 0 {
			*value = append((*value)[:*cursor-1], (*value)[*cursor:]...)
			*cursor--
			a.findInputChanged()
		}
	case tcell.KeyDelete:
		if *cursor < len(*value) {
			*value = append((*value)[:*cursor], (*value)[*cursor+1:]...)
			a.findInputChanged()
		}
	case tcell.KeyRune:
		r := ev.Rune()
		if r < 0x20 {
			return
		}
		next := make([]rune, 0, len(*value)+1)
		next = append(next, (*value)[:*cursor]...)
		next = append(next, r)
		next = append(next, (*value)[*cursor:]...)
		*value = next
		*cursor++
		a.findInputChanged()
	}
}

// findInputChanged re-runs the search when the edited field was the query.
// Typing in the replace field must not disturb the match list — the user is
// mid-way through a replace and moving the highlights out from under them
// would be actively hostile.
func (a *App) findInputChanged() {
	if a.findFocus == findFieldQuery {
		a.findApplyQuery()
	}
}

// handleFindMouse routes a click inside the find bar to the control under
// the pointer. Returns true when the bar consumed the event, so the caller
// can skip the editor's own click handling — otherwise clicking a toggle
// would also move the text cursor behind the bar.
func (a *App) handleFindMouse(x, y int, btn tcell.ButtonMask) bool {
	if !a.findOpen || btn&tcell.Button1 == 0 {
		return false
	}
	bx, by, bw, bh := a.findBarRect()
	if y < by || y >= by+bh || x < bx || x >= bx+bw {
		return false
	}
	l := a.findBarLayout(bx, bw)

	if y == by {
		switch {
		case within(x, l.chevronX, l.chevronW):
			a.toggleReplaceRow()
		case within(x, l.caseX, l.caseW):
			a.findToggleCase()
		case within(x, l.wordX, l.wordW):
			a.findToggleWord()
		case within(x, l.regexX, l.regexW):
			a.findToggleRegex()
		default:
			a.findFocus = findFieldQuery
		}
		return true
	}

	// Replace row.
	switch {
	case within(x, l.btnReplaceX, l.btnW):
		a.findReplaceCurrent()
	case within(x, l.btnAllX, l.btnAllW):
		a.findReplaceAll()
	default:
		a.findFocus = findFieldReplace
	}
	return true
}

// within reports whether x falls inside the half-open span [start, start+w).
// A zero width means the element was dropped from the layout, which can
// never contain a click.
func within(x, start, w int) bool {
	return w > 0 && x >= start && x < start+w
}

// drawFindBar renders the find bar at the bottom of the editor area.
// Layout (left to right):
//
//	" › Find: <input>          Aa ab .*   3 of 12   Enter: next · Esc: close "
//	"   Replace: <input>                            [Replace] [All]          "
//
// The hint on the right is dropped first when the window is too narrow to
// fit it; the counter is dropped next, then the toggles. The inputs always
// stay visible because that's the whole point of the bar.
func (a *App) drawFindBar() {
	if !a.findOpen {
		return
	}
	bx, by, bw, _ := a.findBarRect()
	l := a.findBarLayout(bx, bw)

	bg := a.theme.LineHL
	barStyle := tcell.StyleDefault.Background(bg).Foreground(a.theme.Text)
	labelStyle := tcell.StyleDefault.Background(bg).Foreground(a.theme.Accent).Bold(true)
	mutedStyle := tcell.StyleDefault.Background(bg).Foreground(a.theme.Muted)
	errStyle := tcell.StyleDefault.Background(bg).Foreground(a.theme.Error).Bold(true)

	// Clear every row the bar owns.
	for row := by; row < by+a.findBarH(); row++ {
		for cx := bx; cx < bx+bw; cx++ {
			a.screen.SetContent(cx, row, ' ', nil, barStyle)
		}
	}

	// --- Query row -------------------------------------------------------
	chev := findChevronCollapsed
	if a.findReplaceOpen {
		chev = findChevronExpanded
	}
	drawAt(a.screen, l.chevronX, by, chev, mutedStyle)
	drawAt(a.screen, l.chevronX+l.chevronW, by, findLabel, labelStyle)

	if l.hintW > 0 {
		drawAt(a.screen, l.hintX, by, a.findHintText(), mutedStyle)
	}
	if l.statusW > 0 {
		style := mutedStyle
		if a.findHasNoMatches() || a.findErr() != nil {
			style = errStyle
		}
		drawAt(a.screen, l.statusX, by, a.findStatusText(), style)
	}

	// Option toggles. An enabled toggle is accented and bold; a disabled
	// one is muted — the same on/off language the rest of the UI uses.
	if l.caseW > 0 {
		tab := a.activeTabPtr()
		on := func(v bool) tcell.Style {
			if v {
				return labelStyle
			}
			return mutedStyle
		}
		var cs, ww, rx bool
		if tab != nil {
			cs, ww, rx = tab.FindCaseSensitive, tab.FindWholeWord, tab.FindRegex
		}
		drawAt(a.screen, l.caseX, by, toggleCaseLabel, on(cs))
		drawAt(a.screen, l.wordX, by, toggleWordLabel, on(ww))
		drawAt(a.screen, l.regexX, by, toggleRegexLabel, on(rx))
	}

	a.drawFindInput(a.findValue, &a.findScroll, a.findCursor, l.queryInX, by, l.queryInW,
		barStyle, a.findFocus == findFieldQuery)

	if !a.findReplaceOpen {
		return
	}

	// --- Replace row -----------------------------------------------------
	ry := by + 1
	drawAt(a.screen, l.chevronX+l.chevronW, ry, replaceLabel, labelStyle)
	if l.btnW > 0 {
		drawAt(a.screen, l.btnReplaceX, ry, btnReplaceLabel, mutedStyle)
		drawAt(a.screen, l.btnAllX, ry, btnReplaceAllLabel, mutedStyle)
	}
	a.drawFindInput(a.replaceValue, &a.replaceScroll, a.replaceCursor, l.replaceInX, ry, l.replaceInW,
		barStyle, a.findFocus == findFieldReplace)
}

// drawFindInput paints one of the bar's text fields with horizontal scroll
// so a long value keeps its caret visible, and places the screen cursor
// when the field is the focused one. Shared by both rows so the two fields
// can never drift in behaviour.
func (a *App) drawFindInput(value []rune, scroll *int, cursor, x, y, w int, st tcell.Style, focused bool) {
	if w < 1 {
		return
	}
	adjustInputScroll(scroll, cursor, w)
	for i := 0; i < w; i++ {
		idx := *scroll + i
		if idx >= len(value) {
			break
		}
		a.screen.SetContent(x+i, y, value[idx], nil, st)
	}
	if !focused {
		return
	}
	caret := x + (cursor - *scroll)
	if caret >= x && caret <= x+w {
		a.screen.ShowCursor(caret, y)
	}
}

// findHintText is the right-hand key reminder. It changes with focus so it
// always describes what Enter will do right now, rather than listing every
// binding and making the user work out which applies.
//
// Kept deliberately terse. The hint is the lowest-priority element in the
// bar, so every character it spends is a character that can cost it its
// place entirely — a fuller version listing Shift+Enter on both rows was
// long enough to be dropped at 120 columns, which is not a hint at all.
func (a *App) findHintText() string {
	if a.findFocus == findFieldReplace {
		return " Enter: replace · Shift+Enter: all "
	}
	return " Enter: next · Tab: replace · Esc: close "
}

// findErr returns the active tab's last regex compile error, if any.
func (a *App) findErr() error {
	tab := a.activeTabPtr()
	if tab == nil {
		return nil
	}
	return tab.FindErr
}

// findStatusText renders the right-hand status: a regex compile error when
// the pattern is broken, otherwise the "N of M" match counter.
//
// Surfacing the error matters more than it looks. Without it a bad pattern
// clears the match list and the bar reads "no results" — indistinguishable
// from a pattern that is valid and genuinely matches nothing, which sends
// the user hunting through their file instead of their regex.
func (a *App) findStatusText() string {
	if len(a.findValue) == 0 {
		return ""
	}
	if err := a.findErr(); err != nil {
		return "bad pattern"
	}
	tab := a.activeTabPtr()
	if tab == nil {
		return ""
	}
	if len(tab.FindMatches) == 0 {
		return "no results"
	}
	return fmt.Sprintf("%d of %d", tab.FindIndex+1, len(tab.FindMatches))
}

// findCounterText is retained as the plain "N of M" accessor for callers
// (and tests) that want the counter without the error-state substitution.
func (a *App) findCounterText() string {
	tab := a.activeTabPtr()
	if len(a.findValue) == 0 || tab == nil {
		return ""
	}
	if len(tab.FindMatches) == 0 {
		return "no results"
	}
	return fmt.Sprintf("%d of %d", tab.FindIndex+1, len(tab.FindMatches))
}

// findHasNoMatches reports whether the user has typed a query that returned
// zero hits, so the counter can flip color.
func (a *App) findHasNoMatches() bool {
	if len(a.findValue) == 0 {
		return false
	}
	tab := a.activeTabPtr()
	return tab != nil && len(tab.FindMatches) == 0
}

// adjustInputScroll keeps an input caret inside the visible window of its
// field, sliding the scroll offset left or right as needed. Shared by both
// bar fields; mirrors the prompt modal's adjustPromptScroll.
func adjustInputScroll(scroll *int, cursor, width int) {
	if width <= 0 {
		*scroll = 0
		return
	}
	if cursor < *scroll {
		*scroll = cursor
	}
	if cursor >= *scroll+width {
		*scroll = cursor - width + 1
	}
	if *scroll < 0 {
		*scroll = 0
	}
}

// adjustFindScroll keeps the query input's caret visible. Kept as a thin
// wrapper over adjustInputScroll for the existing callers and tests.
func (a *App) adjustFindScroll(width int) {
	adjustInputScroll(&a.findScroll, a.findCursor, width)
}
