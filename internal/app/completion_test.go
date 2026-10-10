// =============================================================================
// File: internal/app/completion_test.go
// Author: Vonzelle Brown
// Created: 2026-07-30
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/cloudmanic/spice-edit/internal/editor"
	"github.com/cloudmanic/spice-edit/internal/lsp"
)

// posAt is a tiny constructor so the tests read as line/col rather than as
// struct literals.
func posAt(line, col int) editor.Position {
	return editor.Position{Line: line, Col: col}
}

// TestWordPrefixAt covers what the popup filters on and what a completion
// replaces. Getting this wrong duplicates the letters the user already typed.
func TestWordPrefixAt(t *testing.T) {
	cases := []struct {
		line string
		col  int
		want string
	}{
		{"foo.bar", 7, "bar"},
		{"foo.bar", 3, "foo"},
		{"foo.bar", 4, ""},
		{"  indented", 10, "indented"},
		{"snake_case_1", 12, "snake_case_1"},
		{"", 0, ""},
		{"abc", 99, "abc"},
	}
	for _, c := range cases {
		if got := wordPrefixAt(c.line, c.col); got != c.want {
			t.Errorf("wordPrefixAt(%q, %d) = %q, want %q", c.line, c.col, got, c.want)
		}
	}
}

// TestFilterCompletions pins client-side filtering. Servers routinely return
// the whole symbol table and expect the client to narrow it; without this the
// first suggestion after typing "fo" can be something starting with "a".
func TestFilterCompletions(t *testing.T) {
	items := []lsp.CompletionItem{
		{Label: "format"}, {Label: "Foo"}, {Label: "abs"}, {Label: "fold"},
	}
	got := filterCompletions(items, "fo")
	if len(got) != 3 {
		t.Fatalf("got %d items, want 3 (format, Foo, fold)", len(got))
	}
	for _, it := range got {
		if it.Label == "abs" {
			t.Fatal("a non-matching item survived the filter")
		}
	}
	if len(filterCompletions(items, "")) != len(items) {
		t.Error("an empty prefix should keep every item")
	}
}

// TestCompletionItem_TextPrefersInsertText pins that the popup inserts the
// server's insertText when it differs from the label. Labels are decorated for
// display — "foo(…)" — and inserting one puts punctuation into the user's code.
func TestCompletionItem_TextPrefersInsertText(t *testing.T) {
	if got := (lsp.CompletionItem{Label: "foo(…)", InsertText: "foo"}).Text(); got != "foo" {
		t.Errorf("Text() = %q, want the insertText", got)
	}
	if got := (lsp.CompletionItem{Label: "bar"}).Text(); got != "bar" {
		t.Errorf("Text() = %q, want the label as fallback", got)
	}
}

// TestApplyCompletion_ReplacesThePrefix is the end-to-end behaviour: the typed
// prefix is consumed, not appended to.
func TestApplyCompletion_ReplacesThePrefix(t *testing.T) {
	a := seedNavApp(t, "fo\n")
	tab := a.activeTabPtr()
	tab.MoveCursorTo(tab.Buffer.Clamp(tab.Cursor), false)
	tab.MoveCursorTo(posAt(0, 2), false)

	a.completionOpen = true
	a.completionPrefix = "fo"
	a.completionItems = []lsp.CompletionItem{{Label: "format"}}
	a.completionSelected = 0
	a.applyCompletion()

	if got := tab.Buffer.Lines[0]; got != "format" {
		t.Fatalf("line = %q, want %q — the prefix was not replaced", got, "format")
	}
	if a.completionOpen {
		t.Error("applying should close the popup")
	}
}

// TestHandleCompletionKey_DoesNotSwallowKeystrokes is the contract that matters
// most. A popup that eats a keypress silently discards the user's typing; only
// the keys it genuinely owns may be consumed. Identifier runes pass through
// with the popup still open (it re-filters after the insert); any other rune
// passes through and dismisses it.
func TestHandleCompletionKey_DoesNotSwallowKeystrokes(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.completionOpen = true
	a.completionItems = []lsp.CompletionItem{{Label: "alpha"}, {Label: "beta"}}

	for _, k := range []tcell.Key{tcell.KeyUp, tcell.KeyDown, tcell.KeyEsc} {
		a.completionOpen = true
		if !a.handleCompletionKey(keyEv(k, 0)) {
			t.Errorf("key %v should have been consumed by the popup", k)
		}
	}

	a.completionOpen = true
	a.completionItems = []lsp.CompletionItem{{Label: "alpha"}}
	if a.handleCompletionKey(keyEv(tcell.KeyRune, 'x')) {
		t.Fatal("a printable rune must NOT be consumed — that swallows the keystroke")
	}
	if !a.completionOpen {
		t.Error("an identifier rune should leave the popup open to re-filter")
	}
	if a.handleCompletionKey(keyEv(tcell.KeyRune, ' ')) {
		t.Fatal("a space must NOT be consumed")
	}
	if a.completionOpen {
		t.Error("a non-identifier rune should have dismissed the popup")
	}
}

// TestHandleCompletion_IgnoresStaleAnswers pins that a response arriving after
// the cursor moved is dropped. Inserting text where the user is no longer
// looking is worse than showing nothing.
func TestHandleCompletion_IgnoresStaleAnswers(t *testing.T) {
	a := seedNavApp(t, "one\ntwo\n")
	tab := a.activeTabPtr()
	tab.MoveCursorTo(posAt(1, 0), false)

	a.handleCompletion(&completionEvent{
		items:  []lsp.CompletionItem{{Label: "stale"}},
		prefix: "",
		path:   tab.Path,
		at:     posAt(0, 0), // where the cursor WAS
		gen:    a.completionGen,
		when:   time.Now(),
	})
	if a.completionOpen {
		t.Fatal("a completion for a stale cursor position opened the popup")
	}
}

// TestCompletion_Reachable guards the failure this fork has now hit three
// times: a complete, tested request with no caller.
func TestCompletion_Reachable(t *testing.T) {
	// c stays UNBOUND: CLAUDE.md reserves c/x/v so the host terminal's own
	// clipboard is the only clipboard channel. Completion lives on SPACE, which
	// is also the muscle memory from VS Code's Ctrl+Space.
	if leaderActionFor(' ') == nil {
		t.Fatal("Esc space is not bound — completion is unreachable")
	}
	if leaderActionFor('c') != nil {
		t.Fatal("c must stay unbound — it is reserved for the terminal's clipboard")
	}
	a := newTestApp(t, t.TempDir())
	items, _, _ := a.menuLayout()
	for _, it := range items {
		if it.label == "Complete at cursor" {
			return
		}
	}
	t.Error("completion has no menu row — unreachable by mouse")
}

// stubSource is a fake language server for completion: it records each request
// and answers with fixed labels. When gate is non-nil it holds every answer
// until the gate closes, so a test can type while a request is in flight.
type stubSource struct {
	mu     sync.Mutex
	calls  int
	last   lsp.Position
	labels []string
	gate   chan struct{}
}

// complete satisfies completionFunc.
func (s *stubSource) complete(ctx context.Context, path string, pos lsp.Position) ([]lsp.CompletionItem, error) {
	s.mu.Lock()
	s.calls++
	s.last = pos
	gate := s.gate
	s.mu.Unlock()
	if gate != nil {
		<-gate
	}
	out := make([]lsp.CompletionItem, 0, len(s.labels))
	for _, l := range s.labels {
		out = append(out, lsp.CompletionItem{Label: l})
	}
	return out, nil
}

// callCount reads calls under the lock (the stub runs on a request goroutine).
func (s *stubSource) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// completionHarness is an App with a .go file open (a language a server could
// cover), a stubbed completion source, and a pump that feeds posted events —
// the debounce timer, the answer — back through handleEvent like Run does.
type completionHarness struct {
	t      *testing.T
	a      *App
	tab    *editor.Tab
	stub   *stubSource
	events chan tcell.Event
}

// newCompletionHarness writes content to main.go, opens it, puts the cursor at
// at, and installs a stub answering with labels.
func newCompletionHarness(t *testing.T, name, content string, at editor.Position, labels ...string) *completionHarness {
	t.Helper()
	dir := t.TempDir()
	target := filepath.Join(dir, name)
	if err := os.WriteFile(target, []byte(content), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	a := newTestApp(t, dir)
	a.openFile(target)
	tab := a.activeTabPtr()
	if tab == nil {
		t.Fatal("no tab opened")
	}
	tab.MoveCursorTo(at, false)
	stub := &stubSource{labels: labels}
	a.completionSource = stub.complete

	h := &completionHarness{t: t, a: a, tab: tab, stub: stub, events: make(chan tcell.Event, 64)}
	scr := a.screen
	go func() {
		for {
			ev := scr.PollEvent()
			if ev == nil {
				return // screen finalised by newTestApp's cleanup
			}
			h.events <- ev
		}
	}()
	return h
}

// key sends one key event through handleKey, exactly as the main loop would.
func (h *completionHarness) key(k tcell.Key, r rune) {
	h.a.handleKey(keyEv(k, r))
}

// typeText sends each rune of s as a separate key event.
func (h *completionHarness) typeText(s string) {
	for _, r := range s {
		h.key(tcell.KeyRune, r)
	}
}

// pumpUntil dispatches posted events until cond holds or 2s pass. cond is
// re-checked on a short tick too, since some conditions (a stub being called)
// change on another goroutine without posting anything.
func (h *completionHarness) pumpUntil(cond func() bool) bool {
	deadline := time.After(2 * time.Second)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for !cond() {
		select {
		case ev := <-h.events:
			h.a.handleEvent(ev)
		case <-tick.C:
		case <-deadline:
			return false
		}
	}
	return true
}

// pumpFor dispatches posted events for d — long enough for a debounce to fire
// and a stub to answer — so a test can assert that nothing happened.
func (h *completionHarness) pumpFor(d time.Duration) {
	deadline := time.After(d)
	for {
		select {
		case ev := <-h.events:
			h.a.handleEvent(ev)
		case <-deadline:
			return
		}
	}
}

// waitOpen pumps until the popup opens, failing the test if it never does.
func (h *completionHarness) waitOpen() {
	h.t.Helper()
	if !h.pumpUntil(func() bool { return h.a.completionOpen }) {
		h.t.Fatalf("popup never opened (stub calls: %d)", h.stub.callCount())
	}
}

// labels lists what the popup currently shows.
func (h *completionHarness) labels() []string {
	out := make([]string, 0, len(h.a.completionItems))
	for _, it := range h.a.completionItems {
		out = append(out, it.Label)
	}
	return out
}

// line returns buffer line n.
func (h *completionHarness) line(n int) string {
	return h.tab.Buffer.Lines[n]
}

// TestAutoCompletion_OpensAfterDebounce pins the trigger: two identifier runes
// schedule ONE request, after the debounce rather than immediately, and the
// popup it opens is an automatic one.
func TestAutoCompletion_OpensAfterDebounce(t *testing.T) {
	h := newCompletionHarness(t, "main.go", "\n", posAt(0, 0), "greet", "grow", "abs")
	h.typeText("g")
	h.pumpFor(2 * completionDebounce)
	if h.stub.callCount() != 0 || h.a.completionOpen {
		t.Fatal("a one-rune prefix must not request completions")
	}
	h.typeText("r")
	if h.a.completionOpen || h.stub.callCount() != 0 {
		t.Fatal("the request went out before the debounce")
	}
	h.waitOpen()
	if got := h.stub.callCount(); got != 1 {
		t.Errorf("stub called %d times, want 1", got)
	}
	if h.stub.last.Character != 2 {
		t.Errorf("request at character %d, want 2", h.stub.last.Character)
	}
	if !h.a.completionAuto {
		t.Error("a popup opened by typing must be marked automatic")
	}
	if got := strings.Join(h.labels(), ","); got != "greet,grow" {
		t.Errorf("popup shows %q, want greet,grow (filtered on the prefix)", got)
	}
}

// TestAutoCompletion_TypingRefilters pins that identifier keys reach the buffer
// AND narrow the open list, and that the popup closes once nothing matches.
func TestAutoCompletion_TypingRefilters(t *testing.T) {
	h := newCompletionHarness(t, "main.go", "\n", posAt(0, 0), "greet", "grow", "group")
	h.typeText("gr")
	h.waitOpen()
	h.typeText("o")
	if got := strings.Join(h.labels(), ","); got != "grow,group" {
		t.Fatalf("after 'gro' popup shows %q, want grow,group", got)
	}
	h.typeText("u")
	if got := strings.Join(h.labels(), ","); got != "group" {
		t.Fatalf("after 'grou' popup shows %q, want group", got)
	}
	h.typeText("z")
	if h.a.completionOpen {
		t.Error("the popup should close when nothing matches")
	}
	if got := h.line(0); got != "grouz" {
		t.Errorf("line = %q, want %q — a key was lost while filtering", got, "grouz")
	}
}

// TestAutoCompletion_NoSwallowedKeyTypingAWord is the as-you-type failure mode
// this editor feared for months: a word typed at speed, with the popup opening
// partway through, must arrive in the buffer whole.
func TestAutoCompletion_NoSwallowedKeyTypingAWord(t *testing.T) {
	h := newCompletionHarness(t, "main.go", "\n", posAt(0, 0), "greeting", "greet")
	h.typeText("gr")
	h.waitOpen()
	for _, r := range "eeting" {
		h.key(tcell.KeyRune, r)
		h.pumpFor(10 * time.Millisecond)
		if !h.a.completionOpen {
			t.Fatalf("popup closed after %q though 'greeting' still matches", h.line(0))
		}
	}
	if got := h.line(0); got != "greeting" {
		t.Fatalf("line = %q, want %q — the popup swallowed a key", got, "greeting")
	}
}

// TestAutoCompletion_EnterInsertsNewline pins the one deliberate difference
// from a manual popup: the user never asked for this one, so Enter means
// newline, not "rewrite my word".
func TestAutoCompletion_EnterInsertsNewline(t *testing.T) {
	h := newCompletionHarness(t, "main.go", "\n", posAt(0, 0), "greet")
	h.typeText("gr")
	h.waitOpen()
	h.key(tcell.KeyEnter, 0)
	if h.a.completionOpen {
		t.Error("Enter should close the automatic popup")
	}
	if got := h.line(0); got != "gr" {
		t.Errorf("line 0 = %q, want %q — Enter accepted the suggestion", got, "gr")
	}
	if h.tab.Cursor.Line != 1 {
		t.Errorf("cursor on line %d, want 1 — the newline was swallowed", h.tab.Cursor.Line)
	}
}

// TestAutoCompletion_TabAccepts pins Tab as the accept key of an automatic
// popup, applying the SELECTED row and replacing the typed prefix.
func TestAutoCompletion_TabAccepts(t *testing.T) {
	h := newCompletionHarness(t, "main.go", "\n", posAt(0, 0), "greet", "grow")
	h.typeText("gr")
	h.waitOpen()
	h.key(tcell.KeyDown, 0)
	h.key(tcell.KeyTab, 0)
	if got := h.line(0); got != "grow" {
		t.Fatalf("line = %q, want %q", got, "grow")
	}
	if h.a.completionOpen {
		t.Error("accepting should close the popup")
	}
}

// TestManualCompletion_EnterAccepts pins that Esc SPACE keeps today's
// behaviour: the user asked for this popup, so Enter accepts.
func TestManualCompletion_EnterAccepts(t *testing.T) {
	h := newCompletionHarness(t, "main.go", "gr\n", posAt(0, 2), "greet")
	h.key(tcell.KeyEsc, 0)
	h.key(tcell.KeyRune, ' ')
	h.waitOpen()
	if h.a.completionAuto {
		t.Fatal("an Esc SPACE popup must not be marked automatic")
	}
	h.key(tcell.KeyEnter, 0)
	if got := h.line(0); got != "greet" {
		t.Fatalf("line = %q, want %q — Enter did not accept a manual popup", got, "greet")
	}
	if len(h.tab.Buffer.Lines) != 2 {
		t.Errorf("buffer has %d lines — Enter also inserted a newline", len(h.tab.Buffer.Lines))
	}
}

// TestAutoCompletion_StaleTimerIsDropped pins the debounce generation: a key
// pressed before the timer fires (here a cursor move) cancels the request.
func TestAutoCompletion_StaleTimerIsDropped(t *testing.T) {
	h := newCompletionHarness(t, "main.go", "\n", posAt(0, 0), "greet")
	h.typeText("gr")
	h.key(tcell.KeyLeft, 0)
	h.pumpFor(3 * completionDebounce)
	if n := h.stub.callCount(); n != 0 {
		t.Errorf("stub called %d times after a key superseded the timer", n)
	}
	if h.a.completionOpen {
		t.Error("a superseded timer opened the popup")
	}
}

// TestAutoCompletion_StaleAnswerIsDropped pins the answer generation: a reply
// to a request sent before the latest key is dropped even when the cursor did
// not move — the user is no longer looking at the text it describes.
func TestAutoCompletion_StaleAnswerIsDropped(t *testing.T) {
	h := newCompletionHarness(t, "main.go", "\n", posAt(0, 0), "greet")
	gate := make(chan struct{})
	h.stub.gate = gate
	h.typeText("gr")
	if !h.pumpUntil(func() bool { return h.stub.callCount() == 1 }) {
		t.Fatal("the request never went out")
	}
	h.key(tcell.KeyEnd, 0) // cursor is already at the end: only the generation changes
	close(gate)
	h.pumpFor(3 * completionDebounce)
	if h.a.completionOpen {
		t.Fatal("an answer from an older generation opened the popup")
	}
}

// TestAutoCompletion_MovedCursorAnswerIsDropped pins the cursor check for a
// current-generation answer: a mouse click moves the cursor without a key.
func TestAutoCompletion_MovedCursorAnswerIsDropped(t *testing.T) {
	h := newCompletionHarness(t, "main.go", "x\n", posAt(0, 1), "greet")
	gate := make(chan struct{})
	h.stub.gate = gate
	h.key(tcell.KeyHome, 0)
	h.typeText("gr")
	if !h.pumpUntil(func() bool { return h.stub.callCount() == 1 }) {
		t.Fatal("the request never went out")
	}
	h.tab.MoveCursorTo(posAt(0, 0), false) // what a click does: no key event
	close(gate)
	h.pumpFor(3 * completionDebounce)
	if h.a.completionOpen {
		t.Fatal("an answer for a cursor position the user left opened the popup")
	}
}

// TestAutoCompletion_NotRequested pins every place an automatic request must
// not go out: a synthetic tab, typing over a selection, a file with no
// language, and completion.auto=false in config.json.
func TestAutoCompletion_NotRequested(t *testing.T) {
	assertQuiet := func(t *testing.T, h *completionHarness) {
		t.Helper()
		h.pumpFor(3 * completionDebounce)
		if n := h.stub.callCount(); n != 0 {
			t.Errorf("stub called %d times", n)
		}
		if h.a.completionOpen {
			t.Error("popup opened")
		}
	}
	t.Run("synthetic tab", func(t *testing.T) {
		h := newCompletionHarness(t, "main.go", "\n", posAt(0, 0), "greet")
		h.tab.Synthetic = true
		h.typeText("gr")
		assertQuiet(t, h)
	})
	t.Run("selection", func(t *testing.T) {
		h := newCompletionHarness(t, "main.go", "gxyz\n", posAt(0, 1), "greet")
		h.tab.MoveCursorTo(posAt(0, 4), true)
		h.typeText("r")
		if got := h.line(0); got != "gr" {
			t.Fatalf("line = %q, want the selection replaced by %q", got, "gr")
		}
		assertQuiet(t, h)
	})
	t.Run("no language", func(t *testing.T) {
		h := newCompletionHarness(t, "notes.txt", "\n", posAt(0, 0), "greet")
		h.typeText("gr")
		assertQuiet(t, h)
	})
	t.Run("completion.auto false", func(t *testing.T) {
		h := newCompletionHarness(t, "main.go", "\n", posAt(0, 0), "greet")
		xdg := t.TempDir()
		if err := os.MkdirAll(filepath.Join(xdg, "spiceedit"), 0o755); err != nil {
			t.Fatal(err)
		}
		cfg := `{"completion":{"auto":false}}`
		if err := os.WriteFile(filepath.Join(xdg, "spiceedit", "config.json"), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("XDG_CONFIG_HOME", xdg)
		h.a.loadSpiceConfig()
		h.typeText("gr")
		assertQuiet(t, h)
		h.key(tcell.KeyEsc, 0)
		h.key(tcell.KeyRune, ' ')
		h.waitOpen() // Esc SPACE still works with auto off
	})
}

// TestAutoCompletion_RenderedUnderCursorOnTabIndentedLine renders the popup
// through a SimulationScreen and checks it against where the typed glyphs
// actually landed — never against a second copy of the geometry formula. A tab
// indent is where a rune-indexed column would put the popup several cells left
// of the cursor.
func TestAutoCompletion_RenderedUnderCursorOnTabIndentedLine(t *testing.T) {
	h := newCompletionHarness(t, "main.go", "func f() {\n\t\n}\n", posAt(1, 1), "greet")
	h.typeText("gr")
	h.waitOpen()
	h.a.draw()
	h.a.screen.Show()

	cells, w, ht := h.a.screen.(tcell.SimulationScreen).GetContents()
	at := func(x, y int) rune {
		c := cells[y*w+x]
		if len(c.Runes) == 0 {
			return ' '
		}
		return c.Runes[0]
	}
	gx, gy := -1, -1
	for y := 0; y < ht && gy < 0; y++ {
		for x := 0; x+2 < w; x++ {
			if at(x, y) == 'g' && at(x+1, y) == 'r' && at(x+2, y) == ' ' {
				gx, gy = x, y
				break
			}
		}
	}
	if gy < 0 {
		t.Fatal("typed text 'gr' not found on screen")
	}
	cursorX := gx + 2 // the cell after the 'r' just typed
	if got := at(cursorX, gy+1); got != '┌' {
		t.Errorf("cell under the cursor (%d,%d) = %q, want the popup's top-left corner", cursorX, gy+1, got)
	}
	var label strings.Builder
	for x := cursorX + 1; x < cursorX+6 && x < w; x++ {
		label.WriteRune(at(x, gy+2))
	}
	if label.String() != "greet" {
		t.Errorf("popup row under the cursor reads %q, want %q", label.String(), "greet")
	}
}
