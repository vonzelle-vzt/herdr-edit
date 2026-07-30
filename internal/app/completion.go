// =============================================================================
// File: internal/app/completion.go
// Author: Vonzelle Brown
// Created: 2026-07-30
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// completion.go is LSP autocomplete: a popup of suggestions anchored under the
// cursor, opened with Esc c.
//
// It is EXPLICITLY INVOKED rather than firing as you type. That is a deliberate
// choice for this editor, not a shortcut: an as-you-type popup needs a debounce,
// a cancellation story for in-flight requests, and a rule for when it must get
// out of the way — and every one of those failure modes shows up as the editor
// swallowing a keystroke, which is the worst thing a text editor can do. Esc c
// asks for completions at a moment the user chose.
//
// 🔴 The request is the easy half. hover and definition were complete, tested
// and advertised in initialize with ZERO callers for months; find-and-replace
// then did the same thing. This file exists so that does not happen a fourth
// time — completionOpen, handleCompletionKey and applyCompletion below are the
// wiring, and completion_test.go asserts the leader binding exists.

package app

import (
	"context"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/cloudmanic/spice-edit/internal/editor"
	"github.com/cloudmanic/spice-edit/internal/lsp"
)

const (
	completionMaxRows  = 8
	completionMaxWidth = 44
)

// completionEvent delivers a finished request to the main loop. Like every
// other LSP action here it arrives as a posted event: calling inline would
// block Run's PollEvent on a language server, and an indexing server would
// freeze typing for the whole timeout with no way out.
type completionEvent struct {
	items  []lsp.CompletionItem
	prefix string
	at     editor.Position
	when   time.Time
}

// When satisfies tcell.Event.
func (e *completionEvent) When() time.Time { return e.when }

// menuComplete requests completions at the cursor. Bound to Esc c and a menu row.
func (a *App) menuComplete() {
	a.closeMenu()
	tab := a.activeTabPtr()
	if tab == nil || tab.IsImage() {
		return
	}
	path, pos, ok := a.lspCursorPos()
	if !ok {
		a.flash("No language server for this file")
		return
	}
	prefix := wordPrefixAt(tab.LineText(tab.Cursor.Line), tab.Cursor.Col)
	at := tab.Cursor
	mgr := a.lsp
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		items, err := mgr.Completion(ctx, path, pos)
		if err != nil {
			items = nil
		}
		a.post(&completionEvent{items: items, prefix: prefix, at: at, when: time.Now()})
	}()
}

// handleCompletion opens the popup with whatever came back.
func (a *App) handleCompletion(e *completionEvent) {
	tab := a.activeTabPtr()
	if tab == nil || tab.Cursor != e.at {
		// The cursor moved while the server was thinking. Showing suggestions
		// for a position the user has left would insert text somewhere they are
		// not looking.
		return
	}
	items := filterCompletions(e.items, e.prefix)
	if len(items) == 0 {
		a.flash("No completions")
		return
	}
	a.completionOpen = true
	a.completionItems = items
	a.completionSelected = 0
	a.completionScroll = 0
	a.completionPrefix = e.prefix
}

// closeCompletion hides the popup.
func (a *App) closeCompletion() {
	a.completionOpen = false
	a.completionItems = nil
	a.completionSelected = 0
	a.completionScroll = 0
	a.completionPrefix = ""
}

// wordPrefixAt returns the identifier characters immediately before col. That
// prefix is what the popup filters on and what applyCompletion replaces, so a
// completion never duplicates the letters the user already typed.
func wordPrefixAt(line string, col int) string {
	runes := []rune(line)
	if col > len(runes) {
		col = len(runes)
	}
	start := col
	for start > 0 && isIdentRune(runes[start-1]) {
		start--
	}
	return string(runes[start:col])
}

// isIdentRune reports whether r can appear in an identifier.
func isIdentRune(r rune) bool {
	return r == '_' ||
		(r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9')
}

// filterCompletions keeps the items that actually start with what the user has
// typed, case-insensitively, and preserves server order otherwise.
//
// Servers routinely return the whole symbol table and expect the client to
// filter; skipping this shows "abs" as the first suggestion after typing "fo".
func filterCompletions(items []lsp.CompletionItem, prefix string) []lsp.CompletionItem {
	if prefix == "" {
		return items
	}
	lower := strings.ToLower(prefix)
	out := make([]lsp.CompletionItem, 0, len(items))
	for _, it := range items {
		if strings.HasPrefix(strings.ToLower(it.Label), lower) {
			out = append(out, it)
		}
	}
	return out
}

// applyCompletion inserts the selected item, replacing the prefix already typed.
func (a *App) applyCompletion() {
	if a.completionSelected < 0 || a.completionSelected >= len(a.completionItems) {
		return
	}
	item := a.completionItems[a.completionSelected]
	tab := a.activeTabPtr()
	if tab == nil {
		return
	}
	text := item.Text()
	prefixLen := len([]rune(a.completionPrefix))
	a.closeCompletion()

	// Delete the typed prefix first so the result is the completion, not the
	// prefix with the completion appended to it. Done by selecting it through
	// the exported cursor API rather than reaching into the buffer: cursorMoved
	// is unexported, and a direct buffer edit from this package would leave the
	// viewport anchored to a position that no longer exists.
	if prefixLen > 0 {
		end := tab.Cursor
		start := editor.Position{Line: end.Line, Col: end.Col - prefixLen}
		if start.Col < 0 {
			start.Col = 0
		}
		tab.MoveCursorTo(start, false)
		tab.MoveCursorTo(end, true)
		tab.DeleteSelection()
	}
	tab.InsertString(text)
}

// handleCompletionKey drives the popup. Returns true when it consumed the key.
//
// 🔴 Returning false rather than eating the key is the whole contract. A popup
// that swallows a keystroke is the worst failure a text editor has: the user
// typed something and the editor silently discarded it. Only the four keys the
// popup genuinely owns are consumed; everything else dismisses it and is then
// handled exactly as if the popup had never been open.
func (a *App) handleCompletionKey(ev *tcell.EventKey) bool {
	switch ev.Key() {
	case tcell.KeyEsc:
		a.closeCompletion()
		return true
	case tcell.KeyEnter, tcell.KeyTab:
		a.applyCompletion()
		return true
	case tcell.KeyUp:
		if a.completionSelected > 0 {
			a.completionSelected--
			a.clampCompletionScroll()
		}
		return true
	case tcell.KeyDown:
		if a.completionSelected < len(a.completionItems)-1 {
			a.completionSelected++
			a.clampCompletionScroll()
		}
		return true
	default:
		a.closeCompletion()
		return false
	}
}

// clampCompletionScroll keeps the selection visible.
func (a *App) clampCompletionScroll() {
	if a.completionSelected < a.completionScroll {
		a.completionScroll = a.completionSelected
	}
	if a.completionSelected >= a.completionScroll+completionMaxRows {
		a.completionScroll = a.completionSelected - completionMaxRows + 1
	}
	if a.completionScroll < 0 {
		a.completionScroll = 0
	}
}

// completionRect anchors the popup under the cursor, flipping above it when
// there is not enough room below — the same thing every IDE does, and the
// reason a completion at the bottom of the screen is still readable.
func (a *App) completionRect() (x, y, w, h int) {
	ex, ey, ew, eh := a.editorRect()
	tab := a.activeTabPtr()
	if tab == nil {
		return 0, 0, 0, 0
	}
	rows := len(a.completionItems)
	if rows > completionMaxRows {
		rows = completionMaxRows
	}
	h = rows + 2

	w = 0
	for _, it := range a.completionItems {
		if l := runeLen(it.Label) + runeLen(it.Detail) + 6; l > w {
			w = l
		}
	}
	if w > completionMaxWidth {
		w = completionMaxWidth
	}
	if w < 16 {
		w = 16
	}

	cx, cy, ok := tab.ScreenPos(tab.Cursor.Line, tab.Cursor.Col, ew, eh)
	if !ok {
		cx, cy = 0, 0
	}
	x = ex + cx
	y = ey + cy + 1
	if x+w > ex+ew {
		x = ex + ew - w
	}
	if x < ex {
		x = ex
	}
	if y+h > ey+eh {
		y = ey + cy - h
	}
	if y < ey {
		y = ey
	}
	return
}

// drawCompletion paints the popup: label on the left, kind and detail dimmed
// on the right, selection highlighted.
func (a *App) drawCompletion() {
	if !a.completionOpen || len(a.completionItems) == 0 {
		return
	}
	x, y, w, h := a.completionRect()
	if w <= 0 || h <= 0 {
		return
	}
	bg := a.theme.LineHL
	bgStyle := tcell.StyleDefault.Background(bg).Foreground(a.theme.Text)
	borderStyle := tcell.StyleDefault.Background(bg).Foreground(a.theme.Subtle)
	mutedStyle := tcell.StyleDefault.Background(bg).Foreground(a.theme.Muted)
	selStyle := tcell.StyleDefault.Background(a.theme.Selection).Foreground(a.theme.Text).Bold(true)

	fillRect(a.screen, x, y, w, h, bgStyle)
	drawBorder(a.screen, x, y, w, h, borderStyle)

	rows := h - 2
	for i := 0; i < rows; i++ {
		idx := a.completionScroll + i
		if idx >= len(a.completionItems) {
			break
		}
		it := a.completionItems[idx]
		ry := y + 1 + i
		st := bgStyle
		if idx == a.completionSelected {
			st = selStyle
			for cx := x + 1; cx < x+w-1; cx++ {
				a.screen.SetContent(cx, ry, ' ', nil, st)
			}
		}
		label := truncateEllipsis(it.Label, w-4)
		drawAt(a.screen, x+1, ry, label, st)

		tail := lsp.CompletionKindName(it.Kind)
		if it.Detail != "" {
			tail = it.Detail
		}
		if tail != "" {
			tail = truncateEllipsis(tail, w-runeLen(label)-4)
			tx := x + w - 1 - runeLen(tail)
			if tx > x+1+runeLen(label) {
				drawAt(a.screen, tx, ry, tail, mutedStyle)
			}
		}
	}
}
