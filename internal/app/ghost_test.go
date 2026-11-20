// =============================================================================
// File: internal/app/ghost_test.go
// Author: Vonzelle Brown
// Created: 2026-10-14
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package app

import (
	"strconv"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/cloudmanic/spice-edit/internal/editor"
	"github.com/cloudmanic/spice-edit/internal/lsp"
	"github.com/cloudmanic/spice-edit/internal/theme"
)

// ghostRoute gives the ghost the key first and hands anything it declines to
// the REAL App.handleKey, proving a declined keystroke is still delivered.
func ghostRoute(a *App, g *ghostText, ev *tcell.EventKey) {
	if g.handleKey(ev, a.activeTabPtr(), a.completionOpen) {
		return
	}
	a.handleKey(ev)
}

// ghostApp opens a file and parks the cursor at (line, col).
func ghostApp(t *testing.T, content string, line, col int) *App {
	t.Helper()
	a := seedNavApp(t, content)
	a.activeTabPtr().MoveCursorTo(posAt(line, col), false)
	return a
}

// ghostRow renders the app plus the ghost onto the simulation screen and
// returns the screen row the cursor's line landed on, as plain text, together
// with the cells of that row.
func ghostRow(t *testing.T, a *App, g *ghostText) (string, []tcell.SimCell, int) {
	t.Helper()
	a.draw()
	ex, ey, ew, eh := a.editorRect()
	tab := a.activeTabPtr()
	g.draw(a.screen, a.theme, tab, a.completionOpen, ex, ey, ew, eh)
	scr := a.screen.(tcell.SimulationScreen)
	scr.Show()
	cells, w, _ := scr.GetContents()
	y := ey + tab.Cursor.Line - tab.ScrollY
	row := cells[y*w : (y+1)*w]
	var b strings.Builder
	for _, c := range row {
		if len(c.Runes) > 0 {
			b.WriteRune(c.Runes[0])
		} else {
			b.WriteRune(' ')
		}
	}
	return b.String(), row, ex
}

// TestGhostTabAccepts is the happy path: once a frame has shown it, Tab
// inserts the suggestion at the cursor, the key is consumed (so no indent is
// ALSO inserted), the cursor ends after the inserted text, and a single undo
// removes the whole suggestion.
func TestGhostTabAccepts(t *testing.T) {
	a := ghostApp(t, "fmt.\n", 0, 4)
	tab := a.activeTabPtr()
	var g ghostText
	if !g.offer(tab, g.revision(tab), tab.Cursor, "Println()") {
		t.Fatal("offer at the cursor was refused")
	}
	ghostRow(t, a, &g) // the frame that shows it; Tab accepts only what was drawn

	ghostRoute(a, &g, keyEv(tcell.KeyTab, 0))

	if got := tab.Buffer.Lines[0]; got != "fmt.Println()" {
		t.Fatalf("line = %q, want %q", got, "fmt.Println()")
	}
	if tab.Cursor != posAt(0, 13) {
		t.Errorf("cursor = %+v, want end of the inserted text", tab.Cursor)
	}
	if g.active {
		t.Error("an accepted suggestion must be cleared")
	}
	tab.Undo()
	if got := tab.Buffer.Lines[0]; got != "fmt." {
		t.Errorf("one undo should remove the whole suggestion, line = %q", got)
	}
}

// TestGhostEscDismisses pins Esc: NOT consumed — it is the leader and must
// still reach handleKey (TestInlineEscLeaderStillWorks has the `Esc s` case) —
// but the buffer is untouched and the suggestion gone, and afterwards Tab is an
// ordinary Tab again, not a delayed accept.
func TestGhostEscDismisses(t *testing.T) {
	a := ghostApp(t, "fmt.\n", 0, 4)
	tab := a.activeTabPtr()
	var g ghostText
	g.offer(tab, g.revision(tab), tab.Cursor, "Println()")

	if g.handleKey(keyEv(tcell.KeyEsc, 0), tab, false) {
		t.Fatal("Esc with a visible suggestion was consumed — the leader would never see it")
	}
	if got := tab.Buffer.Lines[0]; got != "fmt." {
		t.Fatalf("Esc changed the buffer: %q", got)
	}
	if g.active {
		t.Fatal("Esc did not dismiss the suggestion")
	}
	if g.handleKey(keyEv(tcell.KeyTab, 0), tab, false) {
		t.Fatal("Tab after a dismiss was consumed by the ghost")
	}
	if strings.Contains(tab.Buffer.Lines[0], "Println") {
		t.Fatal("a dismissed suggestion was applied")
	}
}

// TestGhostUndrawnIsNotAccepted pins the unit half of "Tab accepts what you
// saw": an offered suggestion that no frame has painted — or whose last frame
// had it scrolled off screen — is dismissed by Tab, which is then not consumed.
func TestGhostUndrawnIsNotAccepted(t *testing.T) {
	a := ghostApp(t, "fmt.\n", 0, 4)
	tab := a.activeTabPtr()
	var g ghostText
	g.offer(tab, g.revision(tab), tab.Cursor, "Println()")
	if g.handleKey(keyEv(tcell.KeyTab, 0), tab, false) || g.active {
		t.Fatal("Tab accepted, or left behind, a suggestion no frame had drawn")
	}

	g.offer(tab, g.revision(tab), tab.Cursor, "Println()")
	ghostRow(t, a, &g)
	if !g.drawn {
		t.Fatal("a painted suggestion was not recorded as drawn")
	}
	ex, ey, ew, _ := a.editorRect()
	g.draw(a.screen, a.theme, tab, false, ex, ey, ew, 0) // a frame with no room for the cursor line
	if g.drawn || g.handleKey(keyEv(tcell.KeyTab, 0), tab, false) {
		t.Fatal("Tab accepted a suggestion the last frame did not show")
	}
	if tab.Buffer.Lines[0] != "fmt." {
		t.Fatalf("an undrawn suggestion reached the buffer: %q", tab.Buffer.Lines[0])
	}
}

// TestGhostWithNoSuggestionConsumesNothing pins that the component is inert
// when idle: Tab and Esc keep their normal meanings when nothing is showing.
func TestGhostWithNoSuggestionConsumesNothing(t *testing.T) {
	a := ghostApp(t, "x\n", 0, 1)
	var g ghostText
	for _, k := range []tcell.Key{tcell.KeyTab, tcell.KeyEsc, tcell.KeyEnter} {
		if g.handleKey(keyEv(k, 0), a.activeTabPtr(), false) {
			t.Errorf("idle ghost consumed key %v", k)
		}
	}
}

// TestGhostInvalidatedByTypedKey pins "any edit invalidates": a typed rune is
// NOT swallowed — it reaches the buffer through the real handleKey — and the
// suggestion is gone, so the next Tab is a plain Tab.
func TestGhostInvalidatedByTypedKey(t *testing.T) {
	a := ghostApp(t, "fmt.\n", 0, 4)
	tab := a.activeTabPtr()
	var g ghostText
	g.offer(tab, g.revision(tab), tab.Cursor, "Println()")

	ghostRoute(a, &g, keyEv(tcell.KeyRune, 'E'))

	if got := tab.Buffer.Lines[0]; got != "fmt.E" {
		t.Fatalf("typed rune was swallowed or misplaced: %q", got)
	}
	if g.active || g.valid(tab) {
		t.Fatal("typing did not invalidate the suggestion")
	}
	if g.handleKey(keyEv(tcell.KeyTab, 0), tab, false) {
		t.Fatal("Tab accepted a suggestion that typing should have invalidated")
	}
}

// TestGhostInvalidatedByNonKeyEdit covers the edits that never pass through
// handleKey — paste from the menu, a formatter, a reload, an LSP rename. The
// anchor check must catch them before the next draw or Tab, otherwise the
// suggestion would be drawn and inserted against text it was not computed for.
func TestGhostInvalidatedByNonKeyEdit(t *testing.T) {
	a := ghostApp(t, "fmt.\nnext\n", 0, 4)
	tab := a.activeTabPtr()
	var g ghostText
	g.offer(tab, g.revision(tab), tab.Cursor, "Println()")

	tab.InsertString("X") // an edit that is not a key event
	if g.valid(tab) {
		t.Fatal("an edit outside the keyboard left the suggestion valid")
	}
	row, _, _ := ghostRow(t, a, &g)
	if strings.Contains(row, "Println") {
		t.Fatalf("an invalidated suggestion was drawn: %q", row)
	}
	if g.handleKey(keyEv(tcell.KeyTab, 0), tab, false) {
		t.Fatal("Tab accepted an invalidated suggestion")
	}

	// The same for a pure cursor move (a mouse click elsewhere) and for a
	// change of line count elsewhere that leaves the cursor line alone. The
	// direct Lines write calls Touch, as CLAUDE.md requires of every such
	// writer: Buffer.Version is what the revision reads.
	g.offer(tab, g.revision(tab), tab.Cursor, "Println()")
	tab.MoveCursorTo(posAt(1, 0), false)
	if g.valid(tab) {
		t.Fatal("moving the cursor left the suggestion valid")
	}
	tab.MoveCursorTo(posAt(0, 5), false)
	g.offer(tab, g.revision(tab), tab.Cursor, "Println()")
	tab.Buffer.Lines = append(tab.Buffer.Lines, "appended")
	tab.Buffer.Touch()
	if g.valid(tab) {
		t.Fatal("a change in line count left the suggestion valid")
	}
}

// TestGhostPopupWins pins coexistence with the LSP completion popup: while the
// popup is open the ghost is not drawn and Tab goes to the POPUP, which applies
// its item; the ghost is dropped rather than resurfacing afterwards.
func TestGhostPopupWins(t *testing.T) {
	a := ghostApp(t, "fo\n", 0, 2)
	tab := a.activeTabPtr()
	var g ghostText
	g.offer(tab, g.revision(tab), tab.Cursor, "obar()")

	a.completionOpen = true
	a.completionPrefix = "fo"
	a.completionAll = []lsp.CompletionItem{{Label: "format"}}
	a.completionItems = a.completionAll
	a.completionSelected = 0

	row, _, _ := ghostRow(t, a, &g)
	if strings.Contains(row, "obar") {
		t.Fatalf("ghost drawn underneath an open popup: %q", row)
	}

	ghostRoute(a, &g, keyEv(tcell.KeyTab, 0))

	if got := tab.Buffer.Lines[0]; got != "format" {
		t.Fatalf("line = %q, want the popup's item %q — Tab did not reach the popup", got, "format")
	}
	if g.active {
		t.Fatal("the ghost survived a key the popup handled")
	}
	if a.completionOpen {
		t.Fatal("the popup should have closed on accept")
	}

	// Esc with the popup open likewise belongs to the popup.
	g.offer(tab, g.revision(tab), tab.Cursor, "Builder")
	a.completionOpen = true
	a.completionItems = []lsp.CompletionItem{{Label: "formatter"}}
	if g.handleKey(keyEv(tcell.KeyEsc, 0), tab, true) {
		t.Fatal("the ghost consumed Esc while the popup was open")
	}
}

// TestGhostNeverAutoApplies pins contract item 4's last clause: offering and
// drawing a suggestion leave the buffer byte-for-byte unchanged and clean.
func TestGhostNeverAutoApplies(t *testing.T) {
	a := ghostApp(t, "fmt.\n", 0, 4)
	tab := a.activeTabPtr()
	before := tab.Buffer.String()
	var g ghostText
	g.offer(tab, g.revision(tab), tab.Cursor, "Println()")
	ghostRow(t, a, &g)
	ghostRow(t, a, &g)
	if tab.Buffer.String() != before || tab.Dirty {
		t.Fatalf("offer/draw changed the buffer: %q dirty=%v", tab.Buffer.String(), tab.Dirty)
	}
}

// TestGhostStaleOfferRefused pins the race with typing: an answer computed for
// a position the cursor has left is dropped, never shown.
func TestGhostStaleOfferRefused(t *testing.T) {
	a := ghostApp(t, "fmt.\n", 0, 4)
	tab := a.activeTabPtr()
	var g ghostText
	if g.offer(tab, g.revision(tab), posAt(0, 2), "Println()") {
		t.Fatal("an offer for a stale position was accepted")
	}
	if g.active {
		t.Fatal("a refused offer left a suggestion showing")
	}
}

// TestGhostIneligibleTabs pins where no suggestion may appear: with a
// selection (an insert would replace text) and in word-wrap mode (it could not
// be drawn, so Tab would accept something never seen).
func TestGhostIneligibleTabs(t *testing.T) {
	a := ghostApp(t, "hello world\n", 0, 5)
	tab := a.activeTabPtr()
	var g ghostText

	tab.MoveCursorTo(posAt(0, 0), false)
	tab.MoveCursorTo(posAt(0, 5), true)
	if g.offer(tab, g.revision(tab), tab.Cursor, "X") {
		t.Error("offered with a selection active")
	}

	tab.MoveCursorTo(posAt(0, 5), false)
	tab.Wrap = true
	if g.offer(tab, g.revision(tab), tab.Cursor, "X") {
		t.Error("offered in word-wrap mode")
	}
	tab.Wrap = false
	g.offer(tab, g.revision(tab), tab.Cursor, "X")
	tab.Wrap = true
	if g.handleKey(keyEv(tcell.KeyTab, 0), tab, false) {
		t.Error("Tab accepted after wrap was switched on")
	}
}

// TestGhostSanitize pins what a provider may hand us: CRLF is normalised,
// whitespace-only is refused, and control characters are refused outright —
// drawn they would be terminal escapes, accepted they would be invisible bytes
// in the user's source.
func TestGhostSanitize(t *testing.T) {
	if got, ok := ghostSanitize("a\r\nb\rc"); !ok || got != "a\nb\nc" {
		t.Errorf("line endings: got %q ok=%v", got, ok)
	}
	if got, ok := ghostSanitize("\tx := 1"); !ok || got != "\tx := 1" {
		t.Errorf("a tab must survive: got %q ok=%v", got, ok)
	}
	if got, ok := ghostSanitize("caf\u0065\u0301 // é"); !ok || got != "caf\u0065\u0301 // é" {
		t.Errorf("a visible combining accent must survive: got %q ok=%v", got, ok)
	}
	for _, bad := range []string{"", "  \n\t", "x\x1b[2Jy", "x\x00", "x\x7f", "x\u0085",
		"a\u202eb", "a\u2066b\u2069", "a\u200bb", "a\u200fb", "a\ufeffb", "a\u2028b",
		"a\xffb", "a\u034fb", "a\ufe0fb", "a\U000E0101b"} {
		if _, ok := ghostSanitize(bad); ok {
			t.Errorf("ghostSanitize(%q) accepted", bad)
		}
	}
}

// TestGhostDrawsDimmedAfterCursor reads the rendered screen rather than
// recomputing ScreenPos (CLAUDE.md: an oracle that restates the formula it
// checks measures nothing). The ghost must start in the cell right after the
// glyph before the cursor, be dimmed, and push the rest of the line right
// instead of painting over it.
func TestGhostDrawsDimmedAfterCursor(t *testing.T) {
	a := ghostApp(t, "ab\n", 0, 1)
	tab := a.activeTabPtr()
	var g ghostText
	g.offer(tab, g.revision(tab), tab.Cursor, "XY")

	row, cells, ex := ghostRow(t, a, &g)
	// Index by CELL, not byte: the sidebar part of the row holds multi-byte
	// glyphs, so a byte offset from strings.Index is not a screen column.
	runes := []rune(row)
	x := -1
	for i := ex; i+4 <= len(runes); i++ {
		if string(runes[i:i+4]) == "aXYb" {
			x = i
			break
		}
	}
	if x < 0 {
		t.Fatalf("row %q: want the ghost between 'a' and 'b', with 'b' shifted right", row)
	}
	fg, _, attr := cells[x+1].Style.Decompose()
	if fg != a.theme.Muted || attr&tcell.AttrItalic == 0 {
		t.Errorf("ghost cell is not dimmed: fg=%v attr=%v", fg, attr)
	}
	if fgB, _, _ := cells[x+3].Style.Decompose(); fgB == a.theme.Muted {
		t.Error("the real text after the ghost took the ghost's style")
	}
	if fgA, _, _ := cells[x].Style.Decompose(); fgA == a.theme.Muted {
		t.Error("the real text before the cursor took the ghost's style")
	}
}

// TestGhostMultilineAcceptsAll pins multi-line suggestions: only the first
// line is drawn (the rest would hide real lines below), a marker says how many
// more there are, and Tab inserts every line.
func TestGhostMultilineAcceptsAll(t *testing.T) {
	a := ghostApp(t, "func f() {\n}\n", 0, 10)
	tab := a.activeTabPtr()
	var g ghostText
	g.offer(tab, g.revision(tab), tab.Cursor, "\n\treturn\n\t// done")

	row, _, _ := ghostRow(t, a, &g)
	if !strings.Contains(row, "+2 lines") {
		t.Fatalf("multi-line marker missing: %q", row)
	}
	nextRow := func() string {
		scr := a.screen.(tcell.SimulationScreen)
		cells, w, _ := scr.GetContents()
		_, ey, _, _ := a.editorRect()
		var b strings.Builder
		for _, c := range cells[(ey+1)*w : (ey+2)*w] {
			if len(c.Runes) > 0 {
				b.WriteRune(c.Runes[0])
			}
		}
		return b.String()
	}()
	if strings.Contains(nextRow, "return") {
		t.Fatalf("a later suggestion line was painted over the buffer: %q", nextRow)
	}

	ghostRoute(a, &g, keyEv(tcell.KeyTab, 0))
	want := []string{"func f() {", "\treturn", "\t// done", "}"}
	for i, w := range want {
		if tab.Buffer.Lines[i] != w {
			t.Fatalf("line %d = %q, want %q (buffer %q)", i, tab.Buffer.Lines[i], w, tab.Buffer.String())
		}
	}
}

// TestGhostInvalidatedByEditToAnotherLine pins that ANY edit invalidates: a
// change to a line other than the cursor's, leaving the line count alone (a
// formatter or an LSP rename elsewhere), must drop the suggestion.
func TestGhostInvalidatedByEditToAnotherLine(t *testing.T) {
	a := ghostApp(t, "fmt.\nfoo := 1\nbar\n", 0, 4)
	tab := a.activeTabPtr()
	var g ghostText
	g.offer(tab, g.revision(tab), tab.Cursor, "Println(foo)")

	tab.Buffer.Lines[1] = "renamed := 1"
	tab.Buffer.Touch() // a direct Lines writer must stamp the edit (CLAUDE.md)
	if g.valid(tab) {
		t.Fatal("an edit to another line left the suggestion valid")
	}
	if g.handleKey(keyEv(tcell.KeyTab, 0), tab, false) {
		t.Fatal("Tab accepted a suggestion computed against different text")
	}
	if tab.Buffer.Lines[0] != "fmt." {
		t.Fatalf("stale suggestion applied: %q", tab.Buffer.Lines[0])
	}
}

// TestGhostDoesNotReappearAfterRevert pins that invalidation is final: once a
// non-key edit or a cursor move has been observed, undoing it or moving back
// to the saved anchor must not bring the suggestion back.
func TestGhostDoesNotReappearAfterRevert(t *testing.T) {
	a := ghostApp(t, "fmt.\nnext\n", 0, 4)
	tab := a.activeTabPtr()
	at := tab.Cursor
	var g ghostText
	g.offer(tab, g.revision(tab), at, "Println()")

	tab.InsertString("X")
	ghostRow(t, a, &g) // a draw observes the edit
	tab.Undo()
	if tab.Buffer.Lines[0] != "fmt." || tab.Cursor != at {
		t.Fatalf("undo did not restore the anchor: %q at %+v", tab.Buffer.Lines[0], tab.Cursor)
	}
	if g.valid(tab) {
		t.Fatal("an undone edit resurrected the suggestion")
	}
	if row, _, _ := ghostRow(t, a, &g); strings.Contains(row, "Println") {
		t.Fatalf("a dismissed suggestion was redrawn: %q", row)
	}

	g.offer(tab, g.revision(tab), at, "Println()")
	tab.MoveCursorTo(posAt(1, 0), false)
	ghostRow(t, a, &g)
	tab.MoveCursorTo(at, false)
	if g.handleKey(keyEv(tcell.KeyTab, 0), tab, false) {
		t.Fatal("returning the cursor to the anchor resurrected the suggestion")
	}
}

// TestGhostAnchoredAtRequestTime pins that a suggestion is bound to the text
// it was COMPUTED from: a request made at revision N, then an edit to a line
// other than the cursor's, then delivery, must show nothing and let Tab insert
// nothing — the answer was computed against text that no longer exists.
func TestGhostAnchoredAtRequestTime(t *testing.T) {
	a := ghostApp(t, "fmt.\nfoo := 1\n", 0, 4)
	tab := a.activeTabPtr()
	var g ghostText
	rev, at := g.revision(tab), tab.Cursor // the request is built here

	tab.Buffer.Lines[1] = "renamed := 1" // off-cursor edit while it is in flight
	tab.Buffer.Touch()

	if g.offer(tab, rev, at, "Println(foo)") {
		t.Fatal("a response computed before an edit was offered")
	}
	if row, _, _ := ghostRow(t, a, &g); strings.Contains(row, "Println") {
		t.Fatalf("a stale response was drawn: %q", row)
	}
	if g.handleKey(keyEv(tcell.KeyTab, 0), tab, false) {
		t.Fatal("Tab accepted a stale response")
	}
	if tab.Buffer.Lines[0] != "fmt." {
		t.Fatalf("stale response inserted: %q", tab.Buffer.Lines[0])
	}
}

// TestGhostRevisionSurvivesRevert pins that the revision is monotonic: edit
// and revert to byte-identical text still leaves the buffer at a NEW revision,
// so neither a showing suggestion nor an in-flight answer comes back.
func TestGhostRevisionSurvivesRevert(t *testing.T) {
	a := ghostApp(t, "fmt.\nfoo := 1\n", 0, 4)
	tab := a.activeTabPtr()
	var g ghostText

	// Showing, then edit + revert with no draw in between.
	if !g.offer(tab, g.revision(tab), tab.Cursor, "Println()") {
		t.Fatal("setup: offer refused")
	}
	tab.Buffer.Lines[1] = "foo := 2"
	tab.Buffer.Touch()
	tab.Buffer.Lines[1] = "foo := 1"
	tab.Buffer.Touch()
	if g.valid(tab) {
		t.Fatal("edit-then-revert resurrected the suggestion")
	}
	if g.handleKey(keyEv(tcell.KeyTab, 0), tab, false) || tab.Buffer.Lines[0] != "fmt." {
		t.Fatalf("Tab accepted after edit-then-revert: %q", tab.Buffer.Lines[0])
	}

	// In flight, then edit + revert with draws between: the answer is refused.
	rev, at := g.revision(tab), tab.Cursor
	tab.Buffer.Lines[1] = "foo := 3"
	tab.Buffer.Touch()
	ghostRow(t, a, &g)
	tab.Buffer.Lines[1] = "foo := 1"
	tab.Buffer.Touch()
	ghostRow(t, a, &g)
	if g.offer(tab, rev, at, "Println()") {
		t.Fatal("an answer computed before edit-then-revert was offered")
	}

	// Through the real edit primitives: an insert and its undo.
	rev = g.revision(tab)
	tab.InsertString("X")
	tab.Undo()
	if tab.Buffer.Lines[0] != "fmt." || tab.Cursor != at {
		t.Fatalf("undo did not restore: %q at %+v", tab.Buffer.Lines[0], tab.Cursor)
	}
	if g.offer(tab, rev, at, "Println()") {
		t.Fatal("an answer computed before an undone edit was offered")
	}
}

// TestGhostRevisionNeverRepeats pins the stamp's two invariants: 0 is never a
// valid revision (a hand-built Buffer literal has it, and must never take an
// offer), and a buffer REPLACED by a byte-identical one — a reload from disk —
// has a new revision, so an answer computed against the old one is refused.
func TestGhostRevisionNeverRepeats(t *testing.T) {
	a := ghostApp(t, "fmt.\n", 0, 4)
	tab := a.activeTabPtr()
	var g ghostText
	if g.offer(tab, 0, tab.Cursor, "X") {
		t.Fatal("revision 0 was accepted")
	}
	old := g.revision(tab)
	tab.Buffer = editor.NewBuffer(tab.Buffer.String()) // as a silent reload does
	if g.revision(tab) == old || g.offer(tab, old, tab.Cursor, "X") {
		t.Fatal("a replaced buffer reused its old revision")
	}
	tab.Buffer = &editor.Buffer{Lines: []string{"fmt.", ""}}
	if g.offer(tab, g.revision(tab), tab.Cursor, "X") {
		t.Fatal("an unstamped (version 0) buffer took an offer")
	}
}

// TestGhostRevisionIsTheBufferStamp pins the cost model: the revision is
// Buffer.Version, read and never computed, and observing it — offer, valid,
// draw — never moves it. A hash over the buffer made every frame cost time in
// proportion to the file (BenchmarkGhostObserve has the numbers).
func TestGhostRevisionIsTheBufferStamp(t *testing.T) {
	a := ghostApp(t, "fmt.\nfoo := 1\n", 0, 4)
	tab := a.activeTabPtr()
	var g ghostText
	if got, want := g.revision(tab), tab.Buffer.Version(); got != want || got == 0 {
		t.Fatalf("revision = %d, want the buffer's version %d (non-zero)", got, want)
	}
	if g.revision(nil) != 0 {
		t.Fatal("a nil tab must have revision 0")
	}
	before := tab.Buffer.Version()
	if !g.offer(tab, g.revision(tab), tab.Cursor, "Println()") {
		t.Fatal("setup: offer refused")
	}
	for i := 0; i < 5; i++ {
		g.valid(tab)
		ghostRow(t, a, &g)
	}
	if tab.Buffer.Version() != before || !g.valid(tab) {
		t.Fatal("observing the suggestion moved the buffer's stamp or invalidated it")
	}
}

// TestGhostPreviewMatchesInsert pins that what the ghost draws is what the
// editor draws after Tab, rune for rune, including a decomposed accent and a
// wide CJK rune: the preview never promises text the insert does not produce.
func TestGhostPreviewMatchesInsert(t *testing.T) {
	a := ghostApp(t, "x := \n", 0, 5)
	tab := a.activeTabPtr()
	var g ghostText
	suggestion := "\"café 世界\""
	if !g.offer(tab, g.revision(tab), tab.Cursor, suggestion) {
		t.Fatal("setup: offer refused")
	}
	_, previewCells, _ := ghostRow(t, a, &g)
	preview := cellRunes(previewCells)
	if !g.handleKey(keyEv(tcell.KeyTab, 0), tab, false) {
		t.Fatal("Tab did not accept")
	}
	var none ghostText
	_, insertedCells, _ := ghostRow(t, a, &none)
	inserted := cellRunes(insertedCells)
	if preview != inserted {
		t.Fatalf("preview %q differs from inserted %q", preview, inserted)
	}
	// The editor's renderer draws every non-tab rune as one cell, so it does
	// not compose the accent or reserve two cells for 世 in buffer text
	// either; the ghost reproduces that exactly rather than promising better.
}

// benchGhostTab builds a tab of n lines with a suggestion showing at the end
// of line 0, plus a simulation screen and theme to draw it onto.
func benchGhostTab(b *testing.B, n int) (*editor.Tab, *ghostText, tcell.SimulationScreen, theme.Theme) {
	b.Helper()
	lines := make([]string, n)
	for i := range lines {
		lines[i] = "\tfoo := bar(baz, " + strconv.Itoa(i) + ") // a line of ordinary Go"
	}
	lines[0] = "fmt."
	tab := editor.NewSyntheticTab("bench.go", strings.Join(lines, "\n"))
	tab.Synthetic = false // ghostEligible refuses synthetic tabs; this one stands in for a file
	tab.MoveCursorTo(posAt(0, 4), false)
	var g ghostText
	if !g.offer(tab, g.revision(tab), tab.Cursor, "Println()") {
		b.Fatal("setup: offer refused")
	}
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(scr.Fini)
	scr.SetSize(120, 40)
	return tab, &g, scr, theme.Default()
}

// BenchmarkGhostObserve measures the check every key and every frame runs
// while a suggestion is showing. It must cost the same at 10k lines as at 100:
// the staleness test is a version stamp, not a walk over the buffer.
func BenchmarkGhostObserve(b *testing.B) {
	for _, n := range []int{100, 10000} {
		b.Run("lines="+strconv.Itoa(n), func(b *testing.B) {
			tab, g, _, _ := benchGhostTab(b, n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if !g.valid(tab) {
					b.Fatal("suggestion went stale with nothing edited")
				}
			}
		})
	}
}

// BenchmarkGhostDraw measures one frame of the overlay with a suggestion
// showing, at two buffer sizes; like Observe it must not grow with the file.
func BenchmarkGhostDraw(b *testing.B) {
	for _, n := range []int{100, 10000} {
		b.Run("lines="+strconv.Itoa(n), func(b *testing.B) {
			tab, g, scr, th := benchGhostTab(b, n)
			tab.Render(scr, th, 0, 0, 120, 40)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				g.draw(scr, th, tab, false, 0, 0, 120, 40)
			}
		})
	}
}

// cellRunes joins every rune of every cell, combining marks included, with
// trailing blanks dropped.
func cellRunes(cells []tcell.SimCell) string {
	var b strings.Builder
	for _, c := range cells {
		for _, r := range c.Runes {
			b.WriteRune(r)
		}
	}
	return strings.TrimRight(b.String(), " ")
}
