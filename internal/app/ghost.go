// =============================================================================
// File: internal/app/ghost.go
// Author: Vonzelle Brown
// Created: 2026-10-14
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// ghost.go is inline-completion GHOST TEXT: a suggestion drawn dimmed after the
// cursor that is not part of the buffer until Tab accepts it. Esc dismisses it,
// any edit invalidates it, and the completion popup wins over it.
//
// 🔴 It never auto-applies: offer only stores text, and accept — reachable only
// from a Tab keypress — is the one place the buffer changes.

package app

import (
	"hash/maphash"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"

	"github.com/cloudmanic/spice-edit/internal/editor"
	"github.com/cloudmanic/spice-edit/internal/theme"
)

// ghostAnchor is the exact editor state a suggestion was computed for. A
// suggestion is only ever valid at the position it was asked about; anything
// that changes one of these fields means the text it would insert no longer
// fits where it would go.
type ghostAnchor struct {
	tab    *editor.Tab
	buffer *editor.Buffer
	path   string
	cursor editor.Position
	anchor editor.Position
	// rev is the buffer's revision (see ghostText.revision): it changes on
	// an edit to ANY line, and never returns to an earlier value, so an edit
	// that is later reverted still leaves the revision moved on.
	rev uint64
}

// bufRev is the last content hash seen for a buffer and the revision it got.
type bufRev struct {
	hash uint64
	rev  uint64
}

// maxTrackedBuffers bounds the revision table. Dropping it is safe: a buffer
// seen again gets a FRESH revision, so the worst case is a refused answer.
const maxTrackedBuffers = 256

// ghostText is the ghost-text component. The zero value is ready to use and
// shows nothing.
type ghostText struct {
	text   string
	at     ghostAnchor
	active bool

	// revs is the per-buffer revision table and lastRev the counter it draws
	// from. lastRev only ever increments, so revision 0 means "never seen" and
	// no revision is ever handed out twice.
	revs    map[*editor.Buffer]bufRev
	lastRev uint64

	// watch is the buffer a request is in flight for. Revisions are only
	// tracked while one is watched or a suggestion shows: otherwise nothing
	// can go stale, and hashing the buffer on every key and draw would cost
	// time proportional to the file for no reason.
	watch *editor.Buffer
	// hashes counts full-buffer hashes, so a test can pin the idle path at zero.
	hashes int
}

// ghostSeed keys the content hash; any fixed seed works within one process.
var ghostSeed = maphash.MakeSeed()

// bufferHash hashes every line with a separator, so moving a newline changes it.
func bufferHash(b *editor.Buffer) uint64 {
	var h maphash.Hash
	h.SetSeed(ghostSeed)
	for _, l := range b.Lines {
		h.WriteString(l)
		h.WriteByte('\n')
	}
	return h.Sum64()
}

// revision returns tab's buffer revision for a request built now, and watches
// the buffer until the answer is offered or endRequest is called. Returns 0 for
// a tab with no buffer, which no request can carry.
func (g *ghostText) revision(tab *editor.Tab) uint64 {
	rev := g.observe(tab)
	if rev != 0 {
		g.watch = tab.Buffer
	}
	return rev
}

// endRequest stops watching: the host calls it when it cancels a request
// without an answer to offer.
func (g *ghostText) endRequest() {
	g.watch = nil
}

// tracked reports whether b's revision can matter: a request is in flight for
// it or the showing suggestion belongs to it.
func (g *ghostText) tracked(b *editor.Buffer) bool {
	return b != nil && (b == g.watch || (g.active && b == g.at.buffer))
}

func (g *ghostText) hash(b *editor.Buffer) uint64 {
	g.hashes++
	return bufferHash(b)
}

// observe returns tab's buffer revision: a monotonic counter bumped whenever
// the buffer's content differs from the last time it was observed. The editor
// keeps no revision of its own and Buffer.Lines is mutated directly in places,
// so observing the content is the backstop; noteEdit is the direct signal.
func (g *ghostText) observe(tab *editor.Tab) uint64 {
	if tab == nil || tab.Buffer == nil {
		return 0
	}
	h := g.hash(tab.Buffer)
	if r, ok := g.revs[tab.Buffer]; ok && r.hash == h {
		return r.rev
	}
	return g.bump(tab.Buffer, h)
}

// noteEdit records that tab's buffer was just edited. The host calls it from
// every edit path: it is what makes an edit followed by a revert, with no draw
// between them, still invalidate. An untracked buffer only forgets its entry,
// without hashing: its next request then gets a fresh revision, which keeps
// revisions monotonic.
func (g *ghostText) noteEdit(tab *editor.Tab) {
	if tab == nil || tab.Buffer == nil {
		return
	}
	if !g.tracked(tab.Buffer) {
		delete(g.revs, tab.Buffer)
		return
	}
	g.bump(tab.Buffer, g.hash(tab.Buffer))
}

// bump gives b a new revision for content hash h.
func (g *ghostText) bump(b *editor.Buffer, h uint64) uint64 {
	if _, ok := g.revs[b]; !ok && (g.revs == nil || len(g.revs) >= maxTrackedBuffers) {
		g.revs = make(map[*editor.Buffer]bufRev)
	}
	g.lastRev++
	g.revs[b] = bufRev{hash: h, rev: g.lastRev}
	return g.lastRev
}

// anchorFor snapshots the state of tab that a suggestion is bound to.
func (g *ghostText) anchorFor(tab *editor.Tab) ghostAnchor {
	return ghostAnchor{
		tab:    tab,
		buffer: tab.Buffer,
		path:   tab.Path,
		cursor: tab.Cursor,
		anchor: tab.Anchor,
		rev:    g.observe(tab),
	}
}

// ghostEligible reports whether tab can show ghost text at all. Image and
// synthetic tabs have no editable text to complete; a selection means an insert
// would REPLACE text the user can see, which is not what a suggestion after the
// cursor promises; and with word wrap on, ScreenPos no longer maps one buffer
// line to one row, so the overlay cannot be placed — and a suggestion Tab could
// accept without it ever having been drawn would be an auto-apply in disguise.
func ghostEligible(tab *editor.Tab) bool {
	return tab != nil && tab.Buffer != nil && !tab.IsImage() && !tab.Synthetic &&
		!tab.HasSelection() && !tab.Wrap
}

// ghostSanitize normalises a provider's suggestion, returning ok=false when it
// must not be offered. Line endings become "\n" because the buffer splits on
// "\n" only. Any other control character is refused outright: drawn, an ESC
// would reach the terminal as an escape sequence, and accepted, it would put
// bytes into the user's source that they never saw. Unicode format characters
// (bidi overrides and isolates, zero-width marks) and line/paragraph separators
// are refused for the same reason: they make the code read differently from
// what it does, the "Trojan Source" class of deception. Invalid UTF-8 and the
// invisible joiners (combining grapheme joiner, variation selectors) are
// refused too, so what Tab inserts is byte-for-byte what was drawn.
func ghostSanitize(s string) (string, bool) {
	if !utf8.ValidString(s) {
		return "", false
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	for _, r := range s {
		if r == '\n' || r == '\t' {
			continue
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) ||
			unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp, unicode.Variation_Selector) || r == 0x034f {
			return "", false
		}
	}
	if strings.TrimSpace(s) == "" {
		return "", false
	}
	return s, true
}

// offer shows text as a suggestion for position at in tab, answering a request
// built at buffer revision rev (inline.Request.Revision, taken from revision
// when the request was made). It is refused — and any current suggestion is
// left alone — when the buffer has moved past rev or the cursor is no longer
// at, which is how a provider answer that lost a race with the user's editing
// is dropped: a slow provider can only ever be late, never wrong. The anchor is
// the REQUEST's state, not the delivery's: an answer computed before an edit
// to any line never fits the text it would land in. Returns whether the
// suggestion is now showing. It NEVER touches the buffer.
func (g *ghostText) offer(tab *editor.Tab, rev uint64, at editor.Position, text string) bool {
	defer g.endRequest()
	if !ghostEligible(tab) || tab.Cursor != at || rev == 0 || g.observe(tab) != rev {
		return false
	}
	clean, ok := ghostSanitize(text)
	if !ok {
		return false
	}
	g.text = clean
	g.at = g.anchorFor(tab)
	g.active = true
	return true
}

// dismiss drops the suggestion, if any.
func (g *ghostText) dismiss() {
	g.text = ""
	g.at = ghostAnchor{}
	g.active = false
}

// valid reports whether a suggestion is showing and still describes tab
// exactly as it was offered. Called on every key and every draw, so an edit
// that did not come through the keyboard still invalidates it before it can be
// drawn or accepted. A stale suggestion is dismissed, not merely hidden: an
// undo or a cursor returning to the old spot must not resurrect it. It observes
// the buffer's revision while a request for it is in flight, so an edit is
// counted before the older answer arrives; with nothing showing or in flight
// it returns without touching the buffer.
func (g *ghostText) valid(tab *editor.Tab) bool {
	if !g.active {
		if tab != nil && tab.Buffer != nil && tab.Buffer == g.watch {
			g.observe(tab)
		}
		return false
	}
	if !ghostEligible(tab) || g.anchorFor(tab) != g.at {
		g.dismiss()
		return false
	}
	return true
}

// visible is valid, and additionally false while the completion popup is open:
// the popup wins, so the ghost neither draws nor accepts underneath it.
func (g *ghostText) visible(tab *editor.Tab, popupOpen bool) bool {
	return g.valid(tab) && !popupOpen
}

// accept inserts the suggestion at the cursor as ONE undo step and clears it.
// Returns false, changing nothing, when there is no valid suggestion.
func (g *ghostText) accept(tab *editor.Tab) bool {
	if !g.valid(tab) {
		g.dismiss()
		return false
	}
	text := g.text
	g.dismiss()
	tab.InsertString(text)
	return true
}

// handleKey is the seam into App.handleKey. Returns true when it consumed the
// key; on false the caller handles the key exactly as if no ghost existed.
//
// 🔴 Same rule as handleCompletionKey: swallowing a keystroke is the worst
// thing an editor can do. Only Tab and Esc are consumed, and only while a
// suggestion is visible. Every other key dismisses the ghost and passes
// through, so typing never interacts with a suggestion except to remove it.
func (g *ghostText) handleKey(ev *tcell.EventKey, tab *editor.Tab, popupOpen bool) bool {
	if !g.valid(tab) {
		return false
	}
	if popupOpen {
		// Stale, or the popup owns the keyboard. Either way the key is not
		// ours; the popup gets Tab and Esc untouched.
		g.dismiss()
		return false
	}
	// Tab and Esc arrive as KeyTab/KeyEsc only when unmodified; Shift+Tab is
	// KeyBacktab, and Alt-chorded keys are left to the leader logic.
	if ev.Modifiers()&(tcell.ModAlt|tcell.ModCtrl) == 0 {
		switch ev.Key() {
		case tcell.KeyTab:
			return g.accept(tab)
		case tcell.KeyEsc:
			g.dismiss()
			return true
		}
	}
	g.dismiss()
	return false
}

// draw paints the suggestion's first line dimmed at the cursor, pushing the
// rest of the line right. Remaining lines of a multi-line suggestion are not
// drawn over the buffer lines below (that would hide real text); a dimmed
// "+N lines" marker says they exist and will be inserted on Tab.
//
// Reads the cells tab.Render already drew to the right of the cursor and
// repaints them shifted, so it must run after Render and before anything that
// paints over the editor (the popup).
func (g *ghostText) draw(scr tcell.Screen, th theme.Theme, tab *editor.Tab, popupOpen bool, ex, ey, ew, eh int) {
	if !g.visible(tab, popupOpen) {
		return
	}
	cx, cy, ok := tab.ScreenPos(tab.Cursor.Line, tab.Cursor.Col, ew, eh)
	if !ok {
		return
	}
	x0, y, right := ex+cx, ey+cy, ex+ew

	lines := strings.Split(g.text, "\n")
	cells := ghostCells(lines[0], editor.LineVisualCol([]rune(tab.LineText(tab.Cursor.Line)), tab.Cursor.Col))
	if extra := len(lines) - 1; extra > 0 {
		cells = append(cells, []rune(" +"+strconv.Itoa(extra)+" lines")...)
	}

	type cell struct {
		main  rune
		comb  []rune
		style tcell.Style
	}
	tail := make([]cell, 0, right-x0)
	for x := x0; x < right; x++ {
		m, c, st, _ := scr.GetContent(x, y)
		tail = append(tail, cell{m, c, st})
	}
	// Trailing blanks are not text; dropping them keeps a ghost at the end of a
	// line from "shifting" the background fill and changes nothing visible.
	for len(tail) > 0 && tail[len(tail)-1].main == ' ' && tail[len(tail)-1].comb == nil {
		tail = tail[:len(tail)-1]
	}

	ghostStyle := tcell.StyleDefault.Background(th.BG).Foreground(th.Muted).Italic(true)
	x := x0
	for _, r := range cells {
		if x >= right {
			return
		}
		scr.SetContent(x, y, r, nil, ghostStyle)
		x++
	}
	for _, c := range tail {
		if x >= right {
			return
		}
		scr.SetContent(x, y, c.main, c.comb, c.style)
		x++
	}
}

// ghostCells expands the first line of a suggestion into screen cells, turning
// a tab into the spaces it would occupy at visual column startCol — the same
// tab-stop arithmetic Render uses, so the ghost is as wide as the text it
// becomes. Every other rune is one cell, exactly as the editor's own renderer
// lays out buffer text (editor.RuneVisualWidth), so a combining or wide rune
// previews the way it will look once inserted; TestGhostPreviewMatchesInsert
// pins that equivalence.
func ghostCells(line string, startCol int) []rune {
	out := make([]rune, 0, len(line))
	col := startCol
	for _, r := range line {
		w := editor.RuneVisualWidth(r, col)
		if r == '\t' {
			for i := 0; i < w; i++ {
				out = append(out, ' ')
			}
		} else {
			out = append(out, r)
		}
		col += w
	}
	return out
}
