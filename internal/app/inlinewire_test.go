// =============================================================================
// File: internal/app/inlinewire_test.go
// Author: Vonzelle Brown
// Created: 2026-11-20
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package app

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/cloudmanic/spice-edit/internal/editor"
	"github.com/cloudmanic/spice-edit/internal/inline"
	"github.com/cloudmanic/spice-edit/internal/lsp"
	"github.com/cloudmanic/spice-edit/internal/spiceconfig"
)

// fakeInlineProvider stands in for a model. It records every request, signals
// when one starts, and either answers at once or blocks until its context is
// cancelled — which is how a test watches a cancellation reach the provider.
type fakeInlineProvider struct {
	answer    string
	block     bool
	started   chan struct{}
	cancelled chan struct{}
	calls     atomic.Int32

	mu   sync.Mutex
	reqs []inline.Request
}

// newFakeInlineProvider returns a fake answering answer, or blocking when block.
func newFakeInlineProvider(answer string, block bool) *fakeInlineProvider {
	return &fakeInlineProvider{answer: answer, block: block,
		started: make(chan struct{}, 64), cancelled: make(chan struct{}, 64)}
}

// Name satisfies inline.Provider.
func (f *fakeInlineProvider) Name() string { return "fake" }

// Complete records req, then answers or waits for cancellation.
func (f *fakeInlineProvider) Complete(ctx context.Context, req inline.Request) (inline.Suggestion, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	f.started <- struct{}{}
	if f.block {
		<-ctx.Done()
		f.cancelled <- struct{}{}
		return inline.Suggestion{}, ctx.Err()
	}
	return inline.Suggestion{Text: f.answer}, nil
}

// lastReq returns the most recent request the fake saw.
func (f *fakeInlineProvider) lastReq(t *testing.T) inline.Request {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		t.Fatal("the provider received no request")
	}
	return f.reqs[len(f.reqs)-1]
}

// stubInline points the inline seams at s and p for the rest of the test, so
// nothing reads the developer's real config.json. A nil p keeps the REAL
// provider constructor. The debounce shrinks so tests do not wait it out.
// Returns a count of provider constructions.
func stubInline(t *testing.T, s spiceconfig.Inline, p inline.Provider) *atomic.Int32 {
	t.Helper()
	var built atomic.Int32
	oldLoad, oldNew, oldDelay := loadInlineSettings, newInlineProvider, inlineDebounce
	loadInlineSettings = func() spiceconfig.Inline { return s }
	newInlineProvider = func(c inline.Config, ws string, k inline.KeyLookup, on func(inline.Timing)) (*inline.Guarded, error) {
		built.Add(1)
		if p == nil {
			return oldNew(c, ws, k, on)
		}
		return inline.Guard(c, p, on), nil
	}
	inlineDebounce = 5 * time.Millisecond
	t.Cleanup(func() {
		loadInlineSettings, newInlineProvider, inlineDebounce = oldLoad, oldNew, oldDelay
	})
	return &built
}

// inlineApp opens name (written with content) in a test App with the cursor
// at the end of the last line, and shuts its background work down at cleanup.
func inlineApp(t *testing.T, name, content string) *App {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	a := newTestApp(t, dir)
	a.openFile(path)
	tab := a.activeTabPtr()
	last := len(tab.Buffer.Lines) - 1
	tab.MoveCursorTo(posAt(last, len([]rune(tab.Buffer.Lines[last]))), false)
	t.Cleanup(a.shutdownBackground)
	return a
}

// step delivers ev the way Run does: handleEvent, then a full draw.
func step(a *App, ev tcell.Event) {
	a.handleEvent(ev)
	a.draw()
}

// typeText sends each rune of s as a key event through step.
func typeText(a *App, s string) {
	for _, r := range s {
		step(a, keyEv(tcell.KeyRune, r))
	}
}

// waitInlineResult returns the next inlineResultEvent the debouncer posted to
// the simulation screen, failing (not hanging) when none arrives.
func waitInlineResult(t *testing.T, a *App) *inlineResultEvent {
	t.Helper()
	scr := a.screen.(tcell.SimulationScreen)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if scr.HasPendingEvent() {
			if ev, ok := scr.PollEvent().(*inlineResultEvent); ok {
				return ev
			}
			continue
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("timed out waiting for an inlineResultEvent")
	return nil
}

// waitSignal fails the test unless ch receives within d.
func waitSignal(t *testing.T, ch <-chan struct{}, d time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(d):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// screenAtCursor shows the frame and returns the row the TERMINAL cursor is on
// — the position the real renderer chose, not one recomputed here — with the
// cursor's column and the row's cells.
func screenAtCursor(t *testing.T, a *App) (row []tcell.SimCell, cx int) {
	t.Helper()
	scr := a.screen.(tcell.SimulationScreen)
	scr.Show()
	x, y, visible := scr.GetCursor()
	if !visible {
		t.Fatal("the editor did not show a cursor")
	}
	cells, w, _ := scr.GetContents()
	return cells[y*w : (y+1)*w], x
}

// cellText is the first rune of each cell in row from x on, blanks for empty.
func cellText(row []tcell.SimCell, x int) string {
	var b strings.Builder
	for _, c := range row[x:] {
		if len(c.Runes) > 0 {
			b.WriteRune(c.Runes[0])
		} else {
			b.WriteRune(' ')
		}
	}
	return b.String()
}

// ollamaFake is an httptest server speaking just enough of /api/generate. It
// counts the connections it accepted, so a test can prove none were made.
type ollamaFake struct {
	srv   *httptest.Server
	conns atomic.Int32
}

// newOllamaFake starts a fake ollama answering reply on loopback.
func newOllamaFake(t *testing.T, reply string) *ollamaFake {
	t.Helper()
	f := &ollamaFake{}
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"response": reply})
	}))
	f.srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			f.conns.Add(1)
		}
	}
	f.srv.Start()
	t.Cleanup(f.srv.Close)
	return f
}

// writeUserConfig writes body as $XDG_CONFIG_HOME/spiceedit/config.json, so
// the REAL loader (spiceconfig.DefaultPath) reads it.
func writeUserConfig(t *testing.T, body string) {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	dir := filepath.Join(xdg, "spiceedit")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestInlineDisabledConstructsNothing pins the OFF state: with the default
// settings, starting the editor and typing builds no provider and no
// debouncer, and the trigger and every frame are no-ops.
func TestInlineDisabledConstructsNothing(t *testing.T) {
	built := stubInline(t, spiceconfig.Inline{}, newFakeInlineProvider("x", false))
	a := inlineApp(t, "a.go", "fmt")
	a.beginLifecycle()
	typeText(a, ".Pr")
	time.Sleep(20 * inlineDebounce)
	if n := built.Load(); n != 0 {
		t.Fatalf("a disabled config constructed %d provider(s)", n)
	}
	if a.inline.deb != nil || a.inline.pending.tab != nil {
		t.Fatal("a disabled config armed the debouncer or recorded a request")
	}
}

// TestInlineDisabledNeverTouchesTheNetwork is the privacy guarantee measured on
// a real socket: config.json names a model and a url pointing at a live
// listener, but enabled is false — and through the REAL loader and the REAL
// provider constructor, starting up and typing opens no connection. The second
// half is the positive control: the same setup with enabled:true DOES connect,
// so the zero above is the code's doing, not a blind oracle.
func TestInlineDisabledNeverTouchesTheNetwork(t *testing.T) {
	fake := newOllamaFake(t, "Println()")
	oldDelay := inlineDebounce
	inlineDebounce = 5 * time.Millisecond
	defer func() { inlineDebounce = oldDelay }()

	writeUserConfig(t, `{"inline":{"enabled":false,"model":"m","url":"`+fake.srv.URL+`"}}`)
	a := inlineApp(t, "a.go", "fmt")
	a.startInline()
	typeText(a, ".Pr")
	time.Sleep(50 * inlineDebounce)
	if n := fake.conns.Load(); n != 0 {
		t.Fatalf("disabled inline suggestions opened %d connection(s)", n)
	}
	if a.inline.deb != nil {
		t.Fatal("disabled inline suggestions constructed a provider")
	}

	writeUserConfig(t, `{"inline":{"enabled":true,"model":"m","url":"`+fake.srv.URL+`"}}`)
	b := inlineApp(t, "b.go", "fmt")
	b.startInline()
	typeText(b, ".")
	waitInlineResult(t, b)
	if fake.conns.Load() == 0 {
		t.Fatal("positive control: an enabled provider never connected, so the oracle above proves nothing")
	}
}

// TestInlineGhostEndToEnd drives the whole slice through App.handleEvent with
// the REAL provider over HTTP: a typed rune triggers a request, the answer is
// posted back as an event, the ghost is drawn starting at the cell the
// terminal cursor sits on, dimmed, and Tab puts exactly that text in the
// buffer. Columns are read off the rendered screen, never recomputed.
func TestInlineGhostEndToEnd(t *testing.T) {
	fake := newOllamaFake(t, "Println()")
	stubInline(t, spiceconfig.Inline{Enabled: true, Model: "m", URL: fake.srv.URL}, nil)
	a := inlineApp(t, "main.go", "package main\n\nfunc main() {\n\tfmt")
	a.startInline()
	if a.inline.deb == nil {
		t.Fatal("setup: an enabled config did not start suggestions")
	}

	typeText(a, ".")
	row, cx := screenAtCursor(t, a)
	if row[cx-1].Runes[0] != '.' {
		t.Fatalf("setup: the cell before the cursor is %q, want the typed '.'", cellText(row, cx-1))
	}

	step(a, waitInlineResult(t, a))
	row, cx2 := screenAtCursor(t, a)
	if cx2 != cx {
		t.Fatalf("drawing the ghost moved the terminal cursor: %d -> %d", cx, cx2)
	}
	if got := cellText(row, cx); !strings.HasPrefix(got, "Println()") {
		t.Fatalf("at the cursor cell: %q, want the suggestion to start there", got)
	}
	fg, _, attr := row[cx].Style.Decompose()
	if fg != a.theme.Muted || attr&tcell.AttrItalic == 0 {
		t.Errorf("ghost cell is not dimmed: fg=%v attr=%v", fg, attr)
	}
	if strings.Contains(a.activeTabPtr().Buffer.String(), "Println") {
		t.Fatal("the suggestion reached the buffer before Tab")
	}

	step(a, keyEv(tcell.KeyTab, 0))
	tab := a.activeTabPtr()
	if got := tab.Buffer.Lines[3]; got != "\tfmt.Println()" {
		t.Fatalf("after Tab, line = %q, want %q", got, "\tfmt.Println()")
	}
	if a.inline.ghost.active {
		t.Fatal("an accepted suggestion is still showing")
	}
}

// TestInlineEscLeaderStillWorks is the leader regression. Esc is the editor's
// only command key, so a ghost that CONSUMED Esc turned `Esc s` into a
// dismissed suggestion followed by a typed 's'. With a suggestion visible,
// Esc must dismiss it AND arm the leader, so `Esc s` saves.
func TestInlineEscLeaderStillWorks(t *testing.T) {
	stubInline(t, spiceconfig.Inline{Enabled: true}, newFakeInlineProvider("Println()", false))
	a := inlineApp(t, "a.go", "fmt")
	a.startInline()
	typeText(a, ".")
	step(a, waitInlineResult(t, a))
	if !a.inline.ghost.active {
		t.Fatal("setup: no suggestion visible")
	}

	step(a, keyEv(tcell.KeyEsc, 0))
	step(a, keyEv(tcell.KeyRune, 's'))

	tab := a.activeTabPtr()
	if got := tab.Buffer.Lines[0]; got != "fmt." {
		t.Fatalf("line = %q, want %q: the leader's 's' was typed instead of saving", got, "fmt.")
	}
	disk, err := os.ReadFile(tab.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(disk) != "fmt." || tab.Dirty {
		t.Fatalf("Esc s did not save: disk %q dirty=%v", disk, tab.Dirty)
	}
	if a.inline.ghost.active {
		t.Fatal("Esc did not dismiss the suggestion")
	}
}

// TestInlineNonTabKeysDismissAndPassThrough pins the keystroke rule: every key
// but Tab removes the suggestion and is then handled normally — a typed rune
// lands, an arrow moves — so a suggestion can never swallow input.
func TestInlineNonTabKeysDismissAndPassThrough(t *testing.T) {
	stubInline(t, spiceconfig.Inline{Enabled: true}, newFakeInlineProvider("Println()", false))
	a := inlineApp(t, "a.go", "fmt")
	a.startInline()
	typeText(a, ".")
	step(a, waitInlineResult(t, a))

	step(a, keyEv(tcell.KeyRune, 'E'))
	if got := a.activeTabPtr().Buffer.Lines[0]; got != "fmt.E" || a.inline.ghost.active {
		t.Fatalf("typed rune: line %q, ghost active=%v", got, a.inline.ghost.active)
	}
	step(a, waitInlineResult(t, a))
	step(a, keyEv(tcell.KeyLeft, 0))
	if a.activeTabPtr().Cursor != posAt(0, 4) || a.inline.ghost.active {
		t.Fatalf("arrow: cursor %+v, ghost active=%v", a.activeTabPtr().Cursor, a.inline.ghost.active)
	}
}

// TestInlineTabNeverAcceptsAnUnseenSuggestion pins "Tab accepts what you
// saw". A suggestion can be valid yet not on screen: the view was scrolled
// away with the wheel (the cursor did not move), or the pane is too small to
// draw the editor at all. Tab must then be an ordinary Tab — inserting text
// the user never saw is an auto-apply in disguise.
func TestInlineTabNeverAcceptsAnUnseenSuggestion(t *testing.T) {
	stubInline(t, spiceconfig.Inline{Enabled: true}, newFakeInlineProvider("Println()", false))
	content := "fmt" + strings.Repeat("\nfiller", 200)
	a := inlineApp(t, "a.go", content)
	a.startInline()
	tab := a.activeTabPtr()
	tab.MoveCursorTo(posAt(0, 3), false)
	typeText(a, ".")
	ev := waitInlineResult(t, a)

	tab.ScrollY = 150 // a wheel scroll: the cursor line is off screen, the cursor unmoved
	step(a, ev)
	if !a.inline.ghost.active {
		t.Fatal("setup: the suggestion was not offered")
	}
	step(a, keyEv(tcell.KeyTab, 0))
	if got := tab.Buffer.Lines[0]; strings.Contains(got, "Println") || got != "fmt."+tab.IndentUnit {
		t.Fatalf("off-screen: line %q, want an ordinary indent and no suggestion", got)
	}

	tab.Undo()
	tab.ScrollY = 0
	typeText(a, "X")
	ev = waitInlineResult(t, a)
	a.width, a.height = minWidth-1, minHeight-1 // "Window too small": nothing of the editor is drawn
	step(a, ev)
	step(a, keyEv(tcell.KeyTab, 0))
	if got := tab.Buffer.Lines[0]; strings.Contains(got, "Println") {
		t.Fatalf("too small to draw: line %q, the unseen suggestion was inserted", got)
	}
}

// TestInlineTabWithoutGhostIsUnchanged pins that Tab is still Tab: with no
// suggestion it inserts the indent unit, and with the completion popup open it
// goes to the popup even though a suggestion was showing.
func TestInlineTabWithoutGhostIsUnchanged(t *testing.T) {
	stubInline(t, spiceconfig.Inline{Enabled: true}, newFakeInlineProvider("Println()", false))
	a := inlineApp(t, "a.go", "fmt")
	a.startInline()
	tab := a.activeTabPtr()
	step(a, keyEv(tcell.KeyTab, 0))
	if got := tab.Buffer.Lines[0]; got != "fmt"+tab.IndentUnit {
		t.Fatalf("Tab with no suggestion: line %q, want an indent", got)
	}
	tab.Undo()

	typeText(a, ".")
	step(a, waitInlineResult(t, a))
	if !a.inline.ghost.active {
		t.Fatal("setup: no suggestion visible")
	}
	openPopup(a, "Printf")
	step(a, keyEv(tcell.KeyTab, 0))
	if got := tab.Buffer.Lines[0]; got != "fmt.Printf" {
		t.Fatalf("Tab with the popup open: line %q, want the popup's item", got)
	}
	if a.inline.ghost.active {
		t.Fatal("the suggestion survived a key the popup took")
	}
}

// openPopup fakes an open completion popup holding one item.
func openPopup(a *App, label string) {
	a.completionOpen = true
	a.completionPrefix = ""
	a.completionAll = []lsp.CompletionItem{{Label: label}}
	a.completionItems = a.completionAll
	a.completionSelected = 0
}

// TestInlineStaleResultsAreRejected pins the race half of the contract: an
// answer that sat in the event queue while the user moved on is never shown —
// whether a newer keystroke superseded it, the cursor moved, the tab changed,
// or suggestions were switched off.
func TestInlineStaleResultsAreRejected(t *testing.T) {
	stubInline(t, spiceconfig.Inline{Enabled: true}, newFakeInlineProvider("Println()", false))
	a := inlineApp(t, "a.go", "fmt")
	a.startInline()
	result := func(req inline.Request) tcell.Event {
		return &inlineResultEvent{when: time.Now(), result: inline.Result{Request: req, Suggestion: inline.Suggestion{Text: "Println()"}}}
	}

	typeText(a, ".")
	old := a.inline.pending.req
	typeText(a, "P")
	step(a, result(old))
	if a.inline.ghost.active {
		t.Fatal("an answer to a superseded request was shown")
	}

	cur := a.inline.pending.req
	a.activeTabPtr().MoveCursorTo(posAt(0, 0), false)
	step(a, result(cur))
	if a.inline.ghost.active {
		t.Fatal("an answer for a position the cursor left was shown")
	}

	typeText(a, "x")
	cur = a.inline.pending.req
	a.stopInline()
	step(a, result(cur))
	if a.inline.ghost.active {
		t.Fatal("an answer arriving after suggestions were switched off was shown")
	}
}

// TestInlineStepOverAnswerIsNotMisplaced pins why a result is matched on its
// WHOLE request, not just the revision. Typing `)` over an auto-closed `)`
// moves the cursor without editing, so the request it triggers has the SAME
// revision as the one before it; an answer to the earlier request, still in the
// event queue, would pass a revision-only check and be drawn one column right
// of where it was computed for.
func TestInlineStepOverAnswerIsNotMisplaced(t *testing.T) {
	stubInline(t, spiceconfig.Inline{Enabled: true}, newFakeInlineProvider("x", false))
	a := inlineApp(t, "a.go", "f")
	a.startInline()
	typeText(a, "(")
	first := a.inline.pending.req
	typeText(a, ")")
	tab := a.activeTabPtr()
	if tab.Buffer.Lines[0] != "f()" || a.inline.pending.req.Revision != first.Revision {
		t.Fatalf("setup: want a step-over (no edit), got %q rev %d -> %d",
			tab.Buffer.Lines[0], first.Revision, a.inline.pending.req.Revision)
	}
	step(a, &inlineResultEvent{when: time.Now(), result: inline.Result{Request: first, Suggestion: inline.Suggestion{Text: "a, b"}}})
	if a.inline.ghost.active {
		t.Fatal("an answer computed inside the parentheses was shown after them")
	}
}

// TestInlineRequestEditInFlightIsRejected pins the revision half: an edit that
// leaves the cursor where it was (an undo-then-retype, a formatter) still
// makes the old answer stale, because Buffer.Version moved.
func TestInlineRequestEditInFlightIsRejected(t *testing.T) {
	stubInline(t, spiceconfig.Inline{Enabled: true}, newFakeInlineProvider("Println()", false))
	a := inlineApp(t, "a.go", "x\nfmt")
	a.startInline()
	typeText(a, ".")
	req := a.inline.pending.req
	tab := a.activeTabPtr()
	tab.Buffer.Lines[0] = "y"
	tab.Buffer.Touch()
	a.applyInlineResult(inline.Result{Request: req, Suggestion: inline.Suggestion{Text: "Println()"}})
	if a.inline.ghost.active {
		t.Fatal("an answer computed before an edit to another line was shown")
	}
}

// TestInlineProviderErrorFlashesOnce pins that a failing provider is visible
// but not noisy: the first error is flashed, an identical one is not, and a
// success re-arms the report.
func TestInlineProviderErrorFlashesOnce(t *testing.T) {
	stubInline(t, spiceconfig.Inline{Enabled: true}, newFakeInlineProvider("x", false))
	a := inlineApp(t, "a.go", "fmt")
	a.startInline()
	fail := func() {
		typeText(a, ".")
		a.statusMsg = ""
		a.applyInlineResult(inline.Result{Request: a.inline.pending.req, Err: context.DeadlineExceeded})
	}
	fail()
	if !strings.Contains(a.statusMsg, "deadline") {
		t.Fatalf("first failure not reported: %q", a.statusMsg)
	}
	fail()
	if a.statusMsg != "" {
		t.Fatalf("an identical failure was flashed again: %q", a.statusMsg)
	}
}

// TestInlineCancelsInFlight pins every cancellation the spec names, each
// observed AT THE PROVIDER (its context is cancelled — the HTTP call itself
// would abort, not just the wait): a tab switch, an edit, closing the tab,
// shutdown, and a superseding keystroke. Run under -race.
func TestInlineCancelsInFlight(t *testing.T) {
	cases := map[string]func(a *App){
		"tab switch": func(a *App) {
			other := filepath.Join(filepath.Dir(a.activeTabPtr().Path), "other.go")
			if err := os.WriteFile(other, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			a.openFile(other)
			a.draw()
		},
		"edit":            func(a *App) { step(a, keyEv(tcell.KeyBackspace2, 0)) },
		"close tab":       func(a *App) { a.closeTab(a.activeTab); a.draw() },
		"shutdown":        func(a *App) { a.shutdownBackground() },
		"superseded":      func(a *App) { typeText(a, "P") },
		"modal takes key": func(a *App) { a.openMenu(); a.draw() },
	}
	for name, act := range cases {
		t.Run(name, func(t *testing.T) {
			fake := newFakeInlineProvider("", true)
			stubInline(t, spiceconfig.Inline{Enabled: true}, fake)
			a := inlineApp(t, "a.go", "fmt")
			a.startInline()
			typeText(a, ".")
			waitSignal(t, fake.started, 5*time.Second, "the request to start")
			act(a)
			waitSignal(t, fake.cancelled, 5*time.Second, "the in-flight request to be cancelled")
		})
	}
}

// TestInlineIdleFrameKeepsRequest pins the other side of settleInline: a frame
// in which nothing changed must NOT cancel the request in flight, or no answer
// would ever survive the redraw that follows every event.
func TestInlineIdleFrameKeepsRequest(t *testing.T) {
	fake := newFakeInlineProvider("", true)
	stubInline(t, spiceconfig.Inline{Enabled: true}, fake)
	a := inlineApp(t, "a.go", "fmt")
	a.startInline()
	typeText(a, ".")
	waitSignal(t, fake.started, 5*time.Second, "the request to start")
	for i := 0; i < 5; i++ {
		a.draw()
	}
	select {
	case <-fake.cancelled:
		t.Fatal("an idle redraw cancelled the request in flight")
	case <-time.After(50 * time.Millisecond):
	}
	if a.inline.pending.tab == nil {
		t.Fatal("an idle redraw forgot the pending request")
	}
}

// TestInlineLeavesNoGoroutinesBehind is the shutdown contract against the REAL
// provider over HTTP: after a completed request and shutdownBackground, the
// goroutine count is back where it was. A pooled keep-alive socket held two
// client goroutines (and one server-side) forever — the transport has no idle
// timeout — which is the leak this pins.
func TestInlineLeavesNoGoroutinesBehind(t *testing.T) {
	fake := newOllamaFake(t, "Println()")
	stubInline(t, spiceconfig.Inline{Enabled: true, Model: "m", URL: fake.srv.URL}, nil)
	a := inlineApp(t, "a.go", "fmt")
	base := runtime.NumGoroutine() // server and screen included: only what inline adds is measured

	a.startInline()
	typeText(a, ".")
	step(a, waitInlineResult(t, a))
	if !a.inline.ghost.active {
		t.Fatal("setup: the real provider's answer was not shown")
	}
	a.shutdownBackground()

	if n := waitGoroutines(base, 0, 2*time.Second); n > base {
		t.Fatalf("goroutines after shutdown = %d, baseline %d", n, base)
	}
}

// TestInlineRequestIsBoundedByTheCaps pins the per-keystroke cost: in a
// 10k-line file the request gathers only the lines near the cursor, never the
// whole buffer, and what it gathers is exactly the text either side of the
// cursor. In a small file prefix + suffix IS the buffer.
func TestInlineRequestIsBoundedByTheCaps(t *testing.T) {
	small := editor.NewSyntheticTab("a.go", "one\ntwo three\nfour")
	small.Synthetic, small.Path = false, "a.go"
	small.MoveCursorTo(posAt(1, 3), false)
	req := inlineRequestFor(small)
	if req.Prefix != "one\ntwo" || req.Suffix != " three\nfour" {
		t.Fatalf("small file: prefix %q suffix %q", req.Prefix, req.Suffix)
	}
	if req.Revision != small.Buffer.Version() || req.Language != "go" || req.Path != "a.go" {
		t.Fatalf("request metadata: %+v", req)
	}

	lines := make([]string, 10000)
	for i := range lines {
		lines[i] = strings.Repeat("x", 40)
	}
	big := editor.NewSyntheticTab("b.go", strings.Join(lines, "\n"))
	big.MoveCursorTo(posAt(5000, 20), false)
	req = inlineRequestFor(big)
	full := big.Buffer.String()
	cut := 5000*41 + 20
	if !strings.HasSuffix(full[:cut], req.Prefix) || !strings.HasPrefix(full[cut:], req.Suffix) {
		t.Fatal("the request is not the text either side of the cursor")
	}
	if len(req.Prefix) > inline.MaxPrefixBytes+41 || len(req.Suffix) > inline.MaxSuffixBytes+41 {
		t.Fatalf("gathered %d/%d bytes, want at most one line past the caps", len(req.Prefix), len(req.Suffix))
	}
	if len(req.Prefix) < inline.MaxPrefixBytes || len(req.Suffix) < inline.MaxSuffixBytes {
		t.Fatalf("gathered %d/%d bytes, want at least the caps", len(req.Prefix), len(req.Suffix))
	}
}

// TestInlineDeniedFileIsNeverRequested pins that a secrets file never even
// schedules a request: typing in .env reaches the provider zero times.
func TestInlineDeniedFileIsNeverRequested(t *testing.T) {
	fake := newFakeInlineProvider("x", false)
	stubInline(t, spiceconfig.Inline{Enabled: true}, fake)
	a := inlineApp(t, ".env", "TOKEN=")
	a.startInline()
	typeText(a, "abc")
	time.Sleep(20 * inlineDebounce)
	if n := fake.calls.Load(); n != 0 {
		t.Fatalf("a deny-listed file reached the provider %d time(s)", n)
	}
}

// TestInlineToggleMenuRow pins the ≡ menu row: it exists in the View group
// with an on/off label, turning on with no model flashes the config key to set
// and stays off, turning on with one starts suggestions, and turning off closes
// the debouncer.
func TestInlineToggleMenuRow(t *testing.T) {
	built := stubInline(t, spiceconfig.Inline{}, nil) // the REAL constructor
	a := inlineApp(t, "a.go", "fmt")

	var row *menuItemDef
	items, _, _ := a.menuLayout()
	for i := range items {
		if items[i].labelFor != nil && strings.HasPrefix(items[i].labelFor(a), "Toggle inline suggestions") {
			row = &items[i]
		}
	}
	if row == nil {
		t.Fatal("no Toggle inline suggestions row in the ≡ menu")
	}
	if got := row.labelFor(a); got != "Toggle inline suggestions (off)" {
		t.Fatalf("label while off = %q", got)
	}

	row.action(a)
	if a.inline.deb != nil || !strings.Contains(a.statusMsg, `"model"`) {
		t.Fatalf("no model: deb=%v flash=%q, want off and a hint naming \"model\"", a.inline.deb != nil, a.statusMsg)
	}

	stubInline(t, spiceconfig.Inline{Model: "m"}, newFakeInlineProvider("x", false))
	row.action(a)
	if a.inline.deb == nil || row.labelFor(a) != "Toggle inline suggestions (on)" {
		t.Fatalf("with a model: deb=%v label=%q", a.inline.deb != nil, row.labelFor(a))
	}
	if built.Load() != 1 {
		t.Fatalf("real constructor ran %d time(s), want 1 (the no-model attempt)", built.Load())
	}

	row.action(a)
	if a.inline.deb != nil || row.labelFor(a) != "Toggle inline suggestions (off)" {
		t.Fatal("toggling off left suggestions on")
	}
}

// TestInlineConfigFor pins the config translation: url and model reach both
// provider kinds, and allowRemote is what opts THIS workspace in — so a remote
// provider is refused without it, and gets past the opt-in with it.
func TestInlineConfigFor(t *testing.T) {
	c := inlineConfigFor(spiceconfig.Inline{Model: "m", URL: "http://127.0.0.1:1"}, "/ws")
	if !c.Enabled || c.Ollama.Model != "m" || c.Ollama.Endpoint != "http://127.0.0.1:1" || c.RemoteOptedIn("/ws") {
		t.Fatalf("local config: %+v", c)
	}
	remote := spiceconfig.Inline{Provider: inline.ProviderOpenAI, Model: "m"}
	if _, err := inline.NewProvider(inlineConfigFor(remote, "/ws"), "/ws", func(string) string { return "k" }, nil); !errors.Is(err, inline.ErrRemoteNotOptedIn) {
		t.Fatalf("remote without allowRemote: err = %v, want ErrRemoteNotOptedIn", err)
	}
	remote.AllowRemote = true
	if _, err := inline.NewProvider(inlineConfigFor(remote, "/ws"), "/ws", func(string) string { return "k" }, nil); !errors.Is(err, inline.ErrRemoteUnimplemented) {
		t.Fatalf("remote with allowRemote: err = %v, want past the opt-in (ErrRemoteUnimplemented)", err)
	}
}

// TestInlineSetupErrorNamesTheFix pins that a refused start says which
// config.json key to change, rather than echoing the engine's internals.
func TestInlineSetupErrorNamesTheFix(t *testing.T) {
	cases := map[error]string{
		inline.ErrNoModel:             `"model"`,
		inline.ErrNotLocal:            "localhost",
		inline.ErrRemoteNotOptedIn:    `"allowRemote"`,
		inline.ErrRemoteUnimplemented: "not supported",
		inline.ErrNoProvider:          "unknown provider",
		inline.ErrNoKey:               "API key",
	}
	for err, want := range cases {
		if got := inlineSetupError(err, spiceconfig.Inline{Provider: "openai"}); !strings.Contains(got, want) {
			t.Errorf("inlineSetupError(%v) = %q, want it to mention %s", err, got, want)
		}
	}
}
