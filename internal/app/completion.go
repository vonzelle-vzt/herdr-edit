// =============================================================================
// File: internal/app/completion.go
// Author: Vonzelle Brown
// Created: 2026-07-30
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// completion.go is LSP autocomplete: a popup of suggestions anchored under the
// cursor. It opens two ways — MANUALLY with Esc SPACE (or the ≡ menu row), and
// AUTOMATICALLY while typing, 120 ms after an identifier prefix reaches two
// runes or a trigger character ('.') is typed.
//
// The automatic popup was deliberately absent for a long time, because every
// way it can go wrong shows up as the editor swallowing a keystroke — the worst
// thing a text editor can do. It exists now under a contract that keeps the
// keyboard the user's:
//
//   - Debounce + generations. Every key event bumps completionGen. A debounce
//     timer, or a server answer, that carries an older generation is dropped,
//     and so is one whose cursor or file no longer matches. Scheduling a new
//     request cancels the previous request's context. A slow server can only
//     ever be late, never wrong.
//   - While the popup is open, identifier keys PASS THROUGH to the buffer and
//     the list re-filters on the new prefix (closing when nothing matches).
//     Typing a word with the popup open leaves exactly that word in the buffer.
//   - An automatic popup consumes ONLY Tab (accept), Esc (close), Up and Down.
//     🔴 It does NOT consume Enter. The user did not ask for this popup; it
//     appeared because they paused mid-line. Enter at the end of a line means
//     "newline" to someone who never looked at the popup, and accepting a
//     suggestion instead would rewrite their code behind their back. So Enter
//     closes it and inserts the newline. A MANUAL (Esc SPACE) popup was asked
//     for, so there Enter accepts, exactly as before.
//   - No automatic request on image or synthetic tabs, with a selection, for a
//     file with no language, or with completion.auto=false in config.json.
//
// 🔴 The request is the easy half. hover and definition were complete, tested
// and advertised in initialize with ZERO callers for months; find-and-replace
// then did the same thing. menuComplete (Esc SPACE) and afterTypedRune (called
// from handleKey on every typed rune) are the real callers here, and
// completion_test.go drives them through handleKey rather than calling the
// internals directly.

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

// completionDebounce is how long typing must pause before an automatic request
// goes out. Short enough to feel immediate, long enough that a word typed at
// speed sends one request rather than one per letter.
const completionDebounce = 120 * time.Millisecond

// completionFunc is the completion source: lsp.Manager.Completion in
// production, a stub in tests (App.completionSource).
type completionFunc func(ctx context.Context, path string, pos lsp.Position) ([]lsp.CompletionItem, error)

// completionEvent delivers a finished request to the main loop. Like every
// other LSP action here it arrives as a posted event: calling inline would
// block Run's PollEvent on a language server, and an indexing server would
// freeze typing for the whole timeout with no way out.
type completionEvent struct {
	items  []lsp.CompletionItem
	prefix string
	path   string
	at     editor.Position
	gen    uint64
	auto   bool
	when   time.Time
}

// When satisfies tcell.Event.
func (e *completionEvent) When() time.Time { return e.when }

// completionTimerEvent is the debounce firing. It goes through the event queue
// rather than requesting from the timer goroutine, so the generation check and
// the cursor read happen on the main loop like every other UI state access.
type completionTimerEvent struct {
	gen  uint64
	when time.Time
}

// When satisfies tcell.Event.
func (e *completionTimerEvent) When() time.Time { return e.when }

// completionFetcher returns the completion source: the test stub when one is
// set, otherwise the language-server manager. nil means there is no source.
func (a *App) completionFetcher() completionFunc {
	if a.completionSource != nil {
		return a.completionSource
	}
	if a.lsp == nil {
		return nil
	}
	return a.lsp.Completion
}

// menuComplete requests completions at the cursor. Bound to Esc SPACE and a
// menu row. The popup it opens is MANUAL, so Enter accepts.
func (a *App) menuComplete() {
	a.closeMenu()
	tab := a.activeTabPtr()
	if tab == nil || tab.IsImage() {
		return
	}
	fetch := a.completionFetcher()
	if fetch == nil || tab.Path == "" {
		a.flash("No language server for this file")
		return
	}
	a.requestCompletion(tab, fetch, false)
}

// requestCompletion sends a completion request for the cursor in tab, off the
// event loop. Any earlier in-flight request is cancelled first; its answer
// would be stale by generation anyway, and a busy server should not keep
// working on it.
func (a *App) requestCompletion(tab *editor.Tab, fetch completionFunc, auto bool) {
	a.cancelCompletionRequest()
	ctx, cancel := context.WithTimeout(context.Background(), lspRequestTimeout)
	a.completionCancel = cancel

	line := tab.LineText(tab.Cursor.Line)
	pos := lsp.Position{Line: tab.Cursor.Line, Character: lsp.RuneColToUTF16(line, tab.Cursor.Col)}
	ev := &completionEvent{
		prefix: wordPrefixAt(line, tab.Cursor.Col),
		path:   tab.Path,
		at:     tab.Cursor,
		gen:    a.completionGen,
		auto:   auto,
	}
	go func() {
		defer cancel()
		items, err := fetch(ctx, ev.path, pos)
		if err != nil {
			items = nil
		}
		ev.items = items
		ev.when = time.Now()
		a.post(ev)
	}()
}

// cancelCompletionRequest cancels the in-flight request's context, if any.
func (a *App) cancelCompletionRequest() {
	if a.completionCancel != nil {
		a.completionCancel()
		a.completionCancel = nil
	}
}

// isCompletionTrigger reports whether typing r should ask for completions even
// with no identifier prefix. The client does not surface the server's
// completionProvider.triggerCharacters, so this is the spec's fallback: '.'.
func isCompletionTrigger(r rune) bool {
	return r == '.'
}

// afterTypedRune runs after handleKey inserted a typed rune. With the popup
// open it re-filters on the new prefix (the key itself already reached the
// buffer — the popup never eats it); otherwise it may schedule an automatic
// request. hadSelection is whether a selection existed BEFORE the insert, since
// typing over a selection replaces it and leaves no trace afterwards.
func (a *App) afterTypedRune(tab *editor.Tab, r rune, hadSelection bool) {
	if a.completionOpen {
		a.refilterCompletion(tab)
		if a.completionOpen {
			return
		}
	}
	if a.completionAutoOff || hadSelection || !autoCompletionTab(tab) {
		return
	}
	if a.completionSource == nil && (a.lsp == nil || len(a.lsp.Running()) == 0) {
		return
	}
	prefix := wordPrefixAt(tab.LineText(tab.Cursor.Line), tab.Cursor.Col)
	if runeLen(prefix) < 2 && !isCompletionTrigger(r) {
		return
	}
	a.cancelCompletionRequest()
	gen := a.completionGen
	time.AfterFunc(completionDebounce, func() {
		a.post(&completionTimerEvent{gen: gen, when: time.Now()})
	})
}

// autoCompletionTab reports whether tab may receive an AUTOMATIC request: a
// real text file in a language a server could cover, with no selection.
// Synthetic tabs (diffs, search results) are generated text a server has never
// seen, and an image has no text at all.
func autoCompletionTab(tab *editor.Tab) bool {
	return tab != nil && !tab.IsImage() && !tab.Synthetic && tab.Path != "" &&
		!tab.HasSelection() && lsp.LanguageID(tab.Path) != "plaintext"
}

// handleCompletionTimer fires the debounced request if nothing was typed since
// it was scheduled.
func (a *App) handleCompletionTimer(e *completionTimerEvent) {
	if e.gen != a.completionGen || a.completionOpen {
		return
	}
	tab := a.activeTabPtr()
	if !autoCompletionTab(tab) {
		return
	}
	fetch := a.completionFetcher()
	if fetch == nil {
		return
	}
	a.requestCompletion(tab, fetch, true)
}

// handleCompletion opens the popup with whatever came back.
func (a *App) handleCompletion(e *completionEvent) {
	if e.gen != a.completionGen {
		// A key arrived after this request went out. The answer describes text
		// the user has already changed.
		return
	}
	tab := a.activeTabPtr()
	if tab == nil || tab.Cursor != e.at || tab.Path != e.path {
		// The cursor moved while the server was thinking. Showing suggestions
		// for a position the user has left would insert text somewhere they are
		// not looking.
		return
	}
	items := filterCompletions(e.items, e.prefix)
	if len(items) == 0 {
		if !e.auto {
			a.flash("No completions")
		}
		return
	}
	a.completionOpen = true
	a.completionAuto = e.auto
	a.completionAll = e.items
	a.completionItems = items
	a.completionSelected = 0
	a.completionScroll = 0
	a.completionPrefix = e.prefix
}

// refilterCompletion narrows the open popup to the prefix now before the
// cursor, closing it when nothing matches.
func (a *App) refilterCompletion(tab *editor.Tab) {
	prefix := wordPrefixAt(tab.LineText(tab.Cursor.Line), tab.Cursor.Col)
	items := filterCompletions(a.completionAll, prefix)
	if len(items) == 0 {
		a.closeCompletion()
		return
	}
	a.completionItems = items
	a.completionPrefix = prefix
	a.completionSelected = 0
	a.completionScroll = 0
}

// closeCompletion hides the popup.
func (a *App) closeCompletion() {
	a.completionOpen = false
	a.completionAuto = false
	a.completionAll = nil
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
// typed something and the editor silently discarded it. Only Tab, Esc, Up and
// Down are consumed (plus Enter in a MANUAL popup — see the file header for why
// an automatic one lets Enter through). Identifier runes pass through with the
// popup still open, so handleKey inserts them and afterTypedRune re-filters;
// every other key dismisses it and is then handled exactly as if the popup had
// never been open.
func (a *App) handleCompletionKey(ev *tcell.EventKey) bool {
	switch ev.Key() {
	case tcell.KeyEsc:
		a.closeCompletion()
		return true
	case tcell.KeyTab:
		a.applyCompletion()
		return true
	case tcell.KeyEnter:
		if a.completionAuto {
			a.closeCompletion()
			return false
		}
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
	case tcell.KeyRune:
		if isIdentRune(ev.Rune()) {
			return false
		}
		a.closeCompletion()
		return false
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
