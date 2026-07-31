// =============================================================================
// File: internal/app/debug_test.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package app

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/cloudmanic/spice-edit/internal/dap"
	"github.com/cloudmanic/spice-edit/internal/editor"
)

// debugFixture writes a small Go file and opens it, returning the app and path.
// Every test here needs a real file tab, because that is what the debugger's
// answers are painted onto.
func debugFixture(t *testing.T) (*App, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	src := "package main\n\nimport \"fmt\"\n\nfunc add(a, b int) int {\n\tsum := a + b\n\treturn sum\n}\n\nfunc main() {\n\tfmt.Println(add(2, 3))\n}\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	a := newTestApp(t, dir)
	a.openFile(path)
	if a.activeTabPtr() == nil {
		t.Fatal("fixture file did not open")
	}
	return a, path
}

// gutterRuneAt reads the leftmost gutter cell for a 0-based buffer line off the
// simulation screen. Reading the RENDERED screen rather than any intermediate
// state is the point: this fork has shipped overlay arithmetic that agreed with
// itself and disagreed with the pixels.
func gutterRuneAt(t *testing.T, a *App, line int) rune {
	t.Helper()
	tab := a.activeTabPtr()
	if tab == nil {
		t.Fatal("no active tab")
	}
	ex, ey, _, _ := a.editorRect()
	row := line - tab.ScrollY
	mainc, _, _, _ := a.screen.GetContent(ex, ey+row)
	return mainc
}

// TestDebugStoppedEventPaintsTheStoppedMarker is the definition of done for
// this stage, at the UI layer: when the adapter says the program stopped, the
// editor opens that file, moves the cursor there, and paints ▶ in the gutter.
//
// It asserts against the rendered simulation screen and against a 0-BASED
// cursor line, which is the boundary the whole coordinate conversion exists
// for: the event carries buffer coordinates, converted once in debug.go.
func TestDebugStoppedEventPaintsTheStoppedMarker(t *testing.T) {
	a, path := debugFixture(t)
	a.debug = &debugSession{adapter: "delve", running: true, bound: map[string][]boundBreakpoint{}}

	// Buffer line 5 (0-based) is `sum := a + b`, the 6th line of the fixture.
	const stoppedLine = 5
	if got := a.activeTabPtr().LineText(stoppedLine); !strings.Contains(got, "sum := a + b") {
		t.Fatalf("fixture line %d is %q, not the line the test means to stop on", stoppedLine, got)
	}

	a.handleDebugStopped(&debugStoppedEvent{
		when: time.Now(), path: path, line: stoppedLine,
		frame: "main.add", reason: "breakpoint", threadID: 1,
	})
	a.draw()

	if got := gutterRuneAt(t, a, stoppedLine); got != '▶' {
		t.Errorf("gutter cell on line %d is %q, want '▶'", stoppedLine, got)
	}
	// A marker painted on EVERY line would satisfy the assertion above.
	if got := gutterRuneAt(t, a, stoppedLine+1); got == '▶' {
		t.Errorf("line %d also got a stopped marker; only the stopped line may have one", stoppedLine+1)
	}
	if got := a.activeTabPtr().Cursor.Line; got != stoppedLine {
		t.Errorf("cursor is on 0-based line %d, want %d", got, stoppedLine)
	}
	if got := a.debugStatus(); !strings.Contains(got, "main.go:6") {
		t.Errorf("status = %q, want the 1-BASED line 6 for 0-based buffer line 5", got)
	}
	if !strings.Contains(a.debugStatus(), "main.add") {
		t.Errorf("status = %q, want the frame name", a.debugStatus())
	}
}

// TestDebugStoppedMarkerClearsWhenTheSessionEnds pins the other half: a program
// that finished must not leave ▶ painted under it. This is the visible symptom
// of the adapter-death trap, since a dead adapter reaches the same handler
// through internal/dap's synthetic terminated event.
func TestDebugStoppedMarkerClearsWhenTheSessionEnds(t *testing.T) {
	a, path := debugFixture(t)
	a.debug = &debugSession{adapter: "delve", running: true, bound: map[string][]boundBreakpoint{}}
	a.handleDebugStopped(&debugStoppedEvent{when: time.Now(), path: path, line: 5, reason: "breakpoint"})
	a.draw()
	if gutterRuneAt(t, a, 5) != '▶' {
		t.Fatal("precondition failed: the marker was never painted")
	}

	a.handleDAPTerminated(0, true)
	a.draw()

	if got := gutterRuneAt(t, a, 5); got == '▶' {
		t.Error("the stopped marker survived the program exiting; F5 is now bound to a dead client")
	}
	if a.debug != nil {
		t.Error("the session outlived the terminated event")
	}
	if got := a.debugStatus(); got != "" {
		t.Errorf("status = %q, want empty once the session is over", got)
	}
}

// TestSyntheticTerminatedFromAdapterDeathEndsTheSession covers the specific
// path internal/dap uses when the debugger CRASHES rather than exiting: it
// posts a terminated event with no body. The UI must not sit in "stopped".
func TestSyntheticTerminatedFromAdapterDeathEndsTheSession(t *testing.T) {
	a, path := debugFixture(t)
	a.debug = &debugSession{adapter: "delve", running: true, bound: map[string][]boundBreakpoint{}}
	a.handleDebugStopped(&debugStoppedEvent{when: time.Now(), path: path, line: 5, reason: "breakpoint"})

	a.handleDAPEvent(&debugEvent{when: time.Now(), ev: dap.Event{Type: dap.TypeEvent, Event: dap.EventTerminated}})

	if a.debug != nil {
		t.Fatal("a synthetic terminated event did not end the session")
	}
	a.draw()
	if got := gutterRuneAt(t, a, 5); got == '▶' {
		t.Error("the marker survived the adapter dying")
	}
}

// TestStoppedMarkerNeverDestroysABreakpoint is the regression guard for the bug
// that shaped this file's design.
//
// The obvious implementation of "paint ▶ on the stopped line" is
// tab.SetMark(line, Mark{Kind: MarkStopped}). Tab.Marks is keyed by line, so
// that REPLACES any breakpoint on the same line — and in this stage the only
// way to stop is on a breakpoint. syncBreakpoints then sees no MarkBreakpoint
// there, drops it from the authoritative list, and persists the deletion; quit
// while stopped and the breakpoint is gone for good. Measured before the
// overlay existed: one syncBreakpoints tick took a.breakpoints from 1 to 0.
func TestStoppedMarkerNeverDestroysABreakpoint(t *testing.T) {
	a, path := debugFixture(t)
	tab := a.activeTabPtr()

	const line = 5
	tab.SetMark(line, editor.Mark{Kind: editor.MarkBreakpoint, Enabled: true, VerifiedLine: -1})
	a.syncBreakpoints()
	if len(a.breakpoints) != 1 {
		t.Fatalf("precondition: expected 1 breakpoint, got %d", len(a.breakpoints))
	}

	a.debug = &debugSession{adapter: "delve", running: true, bound: map[string][]boundBreakpoint{}}
	a.handleDebugStopped(&debugStoppedEvent{when: time.Now(), path: path, line: line, reason: "breakpoint"})
	a.draw()

	// The marker is visible…
	if got := gutterRuneAt(t, a, line); got != '▶' {
		t.Fatalf("gutter on the stopped line is %q, want '▶'", got)
	}
	// …and the breakpoint underneath it is untouched, through a sync.
	a.syncBreakpoints()
	if len(a.breakpoints) != 1 {
		t.Fatalf("the breakpoint was DESTROYED by the stopped marker: a.breakpoints = %v", a.breakpoints)
	}
	m, ok := tab.MarkAt(line)
	if !ok || m.Kind != editor.MarkBreakpoint {
		t.Fatalf("the mark on line %d is %+v (exists=%v), want a MarkBreakpoint", line, m, ok)
	}
}

// TestBoundBreakpointsPaintWhereTheAdapterActuallyStops covers trap 5's UI
// half. A breakpoint the adapter MOVED must show a ● at the line it really
// bound, and one it could not bind at all must show a hollow ○ — otherwise the
// dot sits somewhere execution never reaches and the debugger looks like it is
// lying.
func TestBoundBreakpointsPaintWhereTheAdapterActuallyStops(t *testing.T) {
	a, path := debugFixture(t)
	tab := a.activeTabPtr()

	// The user marked line 3 (a blank line) and line 4 (`func add…`).
	tab.SetMark(3, editor.Mark{Kind: editor.MarkBreakpoint, Enabled: true, VerifiedLine: -1})
	tab.SetMark(4, editor.Mark{Kind: editor.MarkBreakpoint, Enabled: true, VerifiedLine: -1})

	a.debug = &debugSession{adapter: "delve", running: true}
	a.handleDebugStarted(&debugStartedEvent{
		when: time.Now(), adapter: "delve", client: nil,
		bound: map[string][]boundBreakpoint{path: {
			{ID: 1, Requested: 3, Bound: 3, Verified: false, Message: "no statement here"},
			{ID: 2, Requested: 4, Bound: 5, Verified: true},
		}},
	})
	a.draw()

	if got := gutterRuneAt(t, a, 3); got != '○' {
		t.Errorf("unbindable breakpoint on line 3 drew %q, want a hollow '○'", got)
	}
	if got := gutterRuneAt(t, a, 5); got != '●' {
		t.Errorf("the moved breakpoint did not draw at its BOUND line 5; got %q", got)
	}

	// The verification is written back onto the mark, without disturbing Kind.
	m, _ := tab.MarkAt(4)
	if m.Kind != editor.MarkBreakpoint {
		t.Errorf("mark kind changed to %v; syncBreakpoints would drop this breakpoint", m.Kind)
	}
	if !m.Verified || m.VerifiedLine != 5 {
		t.Errorf("mark = %+v, want Verified with VerifiedLine 5", m)
	}
	unverified, _ := tab.MarkAt(3)
	if unverified.Verified || unverified.VerifiedLine != -1 {
		t.Errorf("unverified mark = %+v, want Verified=false and VerifiedLine=-1", unverified)
	}
}

// TestAdapterLineConversionIsExactlyOneRoundTrip pins the 1-based/0-based
// boundary directly. Trap 6 is that a fixture whose numbers already agree
// proves nothing, so this asserts the two helpers are inverses and that the
// adapter's first line maps to the buffer's line 0 — not line 1.
func TestAdapterLineConversionIsExactlyOneRoundTrip(t *testing.T) {
	if got := bufLineFromAdapter(1); got != 0 {
		t.Errorf("adapter line 1 became buffer line %d, want 0", got)
	}
	if got := adapterLineFromBuf(0); got != 1 {
		t.Errorf("buffer line 0 became adapter line %d, want 1", got)
	}
	for buf := 0; buf < 50; buf++ {
		if got := bufLineFromAdapter(adapterLineFromBuf(buf)); got != buf {
			t.Fatalf("round trip of buffer line %d produced %d", buf, got)
		}
	}
	// An adapter that answers 0 (some do, for "unknown") must not become -1 and
	// index out of the buffer.
	if got := bufLineFromAdapter(0); got != 0 {
		t.Errorf("adapter line 0 became %d, want a clamped 0", got)
	}
}

// TestEnabledBreakpointsSkipsDisabledOnes checks a breakpoint the user switched
// off is never armed in the debugger — the same rule the dlv export follows.
func TestEnabledBreakpointsSkipsDisabledOnes(t *testing.T) {
	a, path := debugFixture(t)
	tab := a.activeTabPtr()
	tab.SetMark(4, editor.Mark{Kind: editor.MarkBreakpoint, Enabled: true, VerifiedLine: -1})
	tab.SetMark(5, editor.Mark{Kind: editor.MarkBreakpoint, Enabled: false, VerifiedLine: -1})
	a.syncBreakpoints()

	got := a.enabledBreakpoints()
	if len(got) != 1 {
		t.Fatalf("enabledBreakpoints returned %d entries, want only the enabled one: %v", len(got), got)
	}
	if got[0].Line != 4 || got[0].Path != path {
		t.Errorf("got %+v, want the enabled breakpoint on line 4", got[0])
	}
}

// TestGroupBreakpointsByPath pins the bucketing setBreakpoints requires: the
// request is per-source and whole-file, so a session with breakpoints in two
// files must issue two calls, each complete.
func TestGroupBreakpointsByPath(t *testing.T) {
	got := groupBreakpointsByPath([]Breakpoint{
		{Path: "/a/main.go", Line: 1},
		{Path: "/b/util.go", Line: 9},
		{Path: "/a/main.go", Line: 4},
	})
	if len(got) != 2 {
		t.Fatalf("got %d files, want 2", len(got))
	}
	if len(got["/a/main.go"]) != 2 {
		t.Errorf("main.go got %d breakpoints, want both of them in ONE call", len(got["/a/main.go"]))
	}
	if len(got["/b/util.go"]) != 1 {
		t.Errorf("util.go got %d breakpoints, want 1", len(got["/b/util.go"]))
	}
}

// TestBoundFromAnswersMatchesPositionally covers the pairing rule: the
// adapter's answer array carries no key back to the request other than its
// index. It also pins that an UNVERIFIED answer — which has no line at all —
// leaves Bound on the requested line rather than collapsing to line 0.
func TestBoundFromAnswersMatchesPositionally(t *testing.T) {
	asked := []Breakpoint{{Line: 4}, {Line: 10}, {Line: 20}}
	answers := []dap.Breakpoint{
		{ID: 1, Verified: true, Line: 7}, // adapter moved it: 1-based 7 -> buffer 6
		{Verified: false, Message: "no statement"},
		{ID: 3, Verified: true, Line: 21}, // 1-based 21 -> buffer 20, unmoved
	}
	got := boundFromAnswers(asked, answers)
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3", len(got))
	}
	if got[0].Requested != 4 || got[0].Bound != 6 || !got[0].Verified {
		t.Errorf("first = %+v, want requested 4 bound 6 verified", got[0])
	}
	if got[1].Verified {
		t.Errorf("second = %+v, want unverified", got[1])
	}
	if got[1].Bound != 10 {
		t.Errorf("an unverified breakpoint's Bound collapsed to %d; it must stay on the "+
			"requested line 10 rather than line 0 of the file", got[1].Bound)
	}
	if got[2].Bound != 20 {
		t.Errorf("third = %+v, want bound 20", got[2])
	}

	// A short answer array (an adapter returning fewer entries than asked) must
	// not panic or mis-pair.
	short := boundFromAnswers(asked, []dap.Breakpoint{{Verified: true, Line: 5}})
	if len(short) != 3 || short[1].Verified || short[2].Verified {
		t.Errorf("short answer produced %+v", short)
	}
}

// TestBreakpointEventFlipsVerification covers the late-resolution path: a
// breakpoint can go from unverified to verified after setBreakpoints already
// answered, and the gutter must stop showing a hollow ○ once it does.
func TestBreakpointEventFlipsVerification(t *testing.T) {
	a, path := debugFixture(t)
	tab := a.activeTabPtr()
	tab.SetMark(4, editor.Mark{Kind: editor.MarkBreakpoint, Enabled: true, VerifiedLine: -1})

	a.debug = &debugSession{
		adapter: "delve", running: true,
		bound: map[string][]boundBreakpoint{path: {{ID: 7, Requested: 4, Bound: 4, Verified: false}}},
	}
	a.draw()
	if got := gutterRuneAt(t, a, 4); got != '○' {
		t.Fatalf("precondition: line 4 drew %q, want '○'", got)
	}

	body, _ := json.Marshal(dap.BreakpointEvent{
		Reason:     "changed",
		Breakpoint: dap.Breakpoint{ID: 7, Verified: true, Line: 5}, // 1-based 5 -> buffer 4
	})
	a.handleDAPEvent(&debugEvent{when: time.Now(), ev: dap.Event{
		Type: dap.TypeEvent, Event: dap.EventBreakpoint, Body: body,
	}})
	a.draw()

	if got := gutterRuneAt(t, a, 4); got == '○' {
		t.Error("the breakpoint still draws hollow after the adapter verified it")
	}
	m, _ := tab.MarkAt(4)
	if !m.Verified {
		t.Errorf("mark = %+v, want Verified after the breakpoint event", m)
	}
}

// TestContinuedEventClearsTheMarker checks the adapter resuming on its own is
// handled. Without it, ▶ stays painted under a program that is running again.
func TestContinuedEventClearsTheMarker(t *testing.T) {
	a, path := debugFixture(t)
	a.debug = &debugSession{adapter: "delve", running: true, bound: map[string][]boundBreakpoint{}}
	a.handleDebugStopped(&debugStoppedEvent{when: time.Now(), path: path, line: 5, reason: "breakpoint"})
	a.draw()
	if gutterRuneAt(t, a, 5) != '▶' {
		t.Fatal("precondition: no marker to clear")
	}

	a.handleDAPEvent(&debugEvent{when: time.Now(), ev: dap.Event{Type: dap.TypeEvent, Event: dap.EventContinued}})
	a.draw()

	if got := gutterRuneAt(t, a, 5); got == '▶' {
		t.Error("the marker survived a continued event")
	}
	if a.debug == nil || a.debug.stopped {
		t.Error("the session should still exist and be running, not stopped")
	}
}

// TestOutputEventIsRecordedNotPrinted pins trap 8's app half: the debuggee's
// output is captured as data. Anything that wrote it to the process's real
// stdout would land on top of the tcell screen.
func TestOutputEventIsRecordedNotPrinted(t *testing.T) {
	a, _ := debugFixture(t)
	a.debug = &debugSession{adapter: "delve", running: true, bound: map[string][]boundBreakpoint{}}

	body, _ := json.Marshal(dap.OutputEvent{Category: "stdout", Output: "hello from the debuggee\n"})
	a.handleDAPEvent(&debugEvent{when: time.Now(), ev: dap.Event{
		Type: dap.TypeEvent, Event: dap.EventOutput, Body: body,
	}})

	if len(a.debug.output) != 1 || a.debug.output[0] != "hello from the debuggee" {
		t.Fatalf("output = %v, want the program's line recorded with its newline trimmed", a.debug.output)
	}
	// And it surfaces when the program ends, so a result is not invisible.
	a.handleDAPTerminated(0, true)
	if !strings.Contains(a.statusMsg, "hello from the debuggee") {
		t.Errorf("exit message %q does not include the program's last output", a.statusMsg)
	}
}

// TestOutputIsBounded checks a program printing in a loop cannot grow the
// session without limit.
func TestOutputIsBounded(t *testing.T) {
	a, _ := debugFixture(t)
	a.debug = &debugSession{adapter: "delve", running: true, bound: map[string][]boundBreakpoint{}}
	for i := 0; i < maxDebugOutput*3; i++ {
		body, _ := json.Marshal(dap.OutputEvent{Category: "stdout", Output: "line\n"})
		a.handleDAPEvent(&debugEvent{when: time.Now(), ev: dap.Event{
			Type: dap.TypeEvent, Event: dap.EventOutput, Body: body,
		}})
	}
	if len(a.debug.output) > maxDebugOutput {
		t.Errorf("output grew to %d lines, over the %d cap", len(a.debug.output), maxDebugOutput)
	}
}

// TestDebugEventsAreIgnoredWithoutASession checks a late event from a session
// the user already stopped cannot resurrect state or panic.
func TestDebugEventsAreIgnoredWithoutASession(t *testing.T) {
	a, path := debugFixture(t)
	a.debug = nil

	for _, name := range []string{
		dap.EventStopped, dap.EventContinued, dap.EventOutput, dap.EventTerminated,
		dap.EventExited, dap.EventBreakpoint, dap.EventThread, dap.EventCapabilities,
		dap.EventInitialized, dap.EventProcess,
	} {
		a.handleDAPEvent(&debugEvent{when: time.Now(), ev: dap.Event{Type: dap.TypeEvent, Event: name}})
	}
	a.handleDebugStopped(&debugStoppedEvent{when: time.Now(), path: path, line: 5})

	if a.debug != nil {
		t.Fatal("an event with no session created one")
	}
	a.draw() // must not panic
}

// TestDebugStatusReportsEachState covers the status line's three states, since
// "is it running" is otherwise invisible.
func TestDebugStatusReportsEachState(t *testing.T) {
	a, path := debugFixture(t)

	if got := a.debugStatus(); got != "" {
		t.Errorf("no session should render an empty status, got %q", got)
	}

	a.debug = &debugSession{adapter: "delve", starting: true}
	if got := a.debugStatus(); !strings.Contains(got, "starting") {
		t.Errorf("starting status = %q", got)
	}

	a.debug = &debugSession{adapter: "delve", running: true}
	if got := a.debugStatus(); !strings.Contains(got, "running") {
		t.Errorf("running status = %q", got)
	}

	a.debug = &debugSession{adapter: "delve", running: true, bound: map[string][]boundBreakpoint{}}
	a.handleDebugStopped(&debugStoppedEvent{when: time.Now(), path: path, line: 5, reason: "breakpoint", frame: "main.add"})
	got := a.debugStatus()
	for _, want := range []string{"stopped", "breakpoint", "main.go:6", "main.add"} {
		if !strings.Contains(got, want) {
			t.Errorf("stopped status %q is missing %q", got, want)
		}
	}
}

// TestStatusBarShowsTheDebugSession is the CALL-SITE oracle for debugStatus.
//
// 🔴 CLAUDE.md's hardest-won rule: a green unit test proves the function works,
// not that anyone can reach it. This fork has shipped complete, tested,
// documented features whose every caller was a _test.go file — three times. So
// this renders the real status bar and reads the text back out of the screen.
func TestStatusBarShowsTheDebugSession(t *testing.T) {
	a, path := debugFixture(t)
	a.debug = &debugSession{adapter: "delve", running: true, bound: map[string][]boundBreakpoint{}}
	a.handleDebugStopped(&debugStoppedEvent{when: time.Now(), path: path, line: 5, reason: "breakpoint", frame: "main.add"})

	// handleDebugStopped flashes, and the flash owns the left-hand text while it
	// lasts. Expire it so the persistent status is what gets rendered.
	a.statusUntil = time.Now().Add(-time.Second)

	// paint() goes through draw() AND Show(): a SimulationScreen serves
	// GetContents from the front buffer, so reading without a Show sees a blank
	// screen and every assertion below would fail for the wrong reason.
	screen := paint(t, a, 120, 40)
	lines := strings.Split(screen, "\n")
	row := lines[len(lines)-2] // draw leaves a trailing blank line from the final \n
	if !strings.Contains(row, "debug: stopped") {
		t.Fatalf("the status bar never renders debugStatus().\nrow: %q", row)
	}
	if !strings.Contains(row, "main.go:6") {
		t.Errorf("status row %q does not name the stopped location", row)
	}
}

// TestDebugKeysAreReachable is the call-site oracle for the F-key branch: it
// drives real key events through handleKey and checks each one reached its
// action. A binding placed after a `return` would be invisible to any test that
// called the methods directly.
func TestDebugKeysAreReachable(t *testing.T) {
	a, _ := debugFixture(t)

	// F9 toggles a breakpoint at the cursor.
	a.activeTabPtr().MoveCursorTo(editor.Position{Line: 5, Col: 0}, false)
	a.handleKey(tcell.NewEventKey(tcell.KeyF9, 0, tcell.ModNone))
	if m, ok := a.activeTabPtr().MarkAt(5); !ok || m.Kind != editor.MarkBreakpoint {
		t.Fatalf("F9 did not set a breakpoint: mark=%+v ok=%v", m, ok)
	}
	a.handleKey(tcell.NewEventKey(tcell.KeyF9, 0, tcell.ModNone))
	if _, ok := a.activeTabPtr().MarkAt(5); ok {
		t.Error("a second F9 did not clear the breakpoint")
	}

	// F5 with no adapter installed must report something rather than doing
	// nothing silently. (It never spawns anything here: the guard runs first.)
	a.debug = &debugSession{adapter: "delve", running: true} // pretend one is up
	a.handleKey(tcell.NewEventKey(tcell.KeyF5, 0, tcell.ModNone))
	if a.statusMsg == "" {
		t.Error("F5 produced no feedback at all")
	}

	// F6 pause with a session that has no client must not panic.
	a.statusMsg = ""
	a.handleKey(tcell.NewEventKey(tcell.KeyF6, 0, tcell.ModNone))
	if a.statusMsg == "" {
		t.Error("F6 produced no feedback at all")
	}

	// The reserved stepping keys say so instead of appearing broken.
	for _, k := range []tcell.Key{tcell.KeyF10, tcell.KeyF11, tcell.KeyF12} {
		a.statusMsg = ""
		a.handleKey(tcell.NewEventKey(k, 0, tcell.ModNone))
		if !strings.Contains(a.statusMsg, "next stage") {
			t.Errorf("key %v said %q, want the reserved-for-stepping note", k, a.statusMsg)
		}
	}
}

// TestDebugKeysDoNotStealFromModals pins the placement rule: the F-key branch
// sits AFTER every modal guard, so a prompt that is up still owns the keyboard.
func TestDebugKeysDoNotStealFromModals(t *testing.T) {
	a, _ := debugFixture(t)
	a.openPrompt("Rename", "new name", "", func(*App, string) {})

	a.activeTabPtr().MoveCursorTo(editor.Position{Line: 5, Col: 0}, false)
	a.handleKey(tcell.NewEventKey(tcell.KeyF9, 0, tcell.ModNone))

	if _, ok := a.activeTabPtr().MarkAt(5); ok {
		t.Error("F9 set a breakpoint while a prompt modal was open")
	}
	if !a.promptOpen {
		t.Error("the prompt closed")
	}
}

// TestDebugMenuRowsAreReachable is the menu-side call-site oracle. Function
// keys are unreliable through a multiplexer, so the ≡ menu is the guaranteed
// path — and CLAUDE.md requires every action to be reachable there.
func TestDebugMenuRowsAreReachable(t *testing.T) {
	a, _ := debugFixture(t)
	items, _, _ := a.menuLayout()

	labels := make(map[string]menuItemDef, len(items))
	for _, it := range items {
		label := it.label
		if it.labelFor != nil {
			label = it.labelFor(a)
		}
		labels[label] = it
	}

	start, ok := labels["Start debugging"]
	if !ok {
		t.Fatalf("no 'Start debugging' row in the menu; got %v", keysOf(labels))
	}
	if start.action == nil {
		t.Error("the start row has no action")
	}
	if start.shortcut != "F5" {
		t.Errorf("start row advertises shortcut %q, want F5", start.shortcut)
	}
	if !start.enabled(a) {
		t.Error("start should be enabled with a Go file open")
	}

	stop, ok := labels["Stop debugging"]
	if !ok {
		t.Fatal("no 'Stop debugging' row in the menu")
	}
	if stop.enabled(a) {
		t.Error("stop should be disabled with no session")
	}

	// With a session up, the same row reads "Continue" while stopped.
	a.debug = &debugSession{adapter: "delve", running: true, stopped: true}
	if got := a.debugStartLabel(); got != "Continue" {
		t.Errorf("label while stopped = %q, want Continue", got)
	}
	if !stop.enabled(a) {
		t.Error("stop should be enabled with a session up")
	}
}

// keysOf lists a map's keys for a failure message.
func keysOf(m map[string]menuItemDef) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestLeaderTableStillRefusesReservedRunes guards the fork's binding contract
// while this stage adds keys: c / x / v stay unbound (the host terminal's
// clipboard owns them) and no Ctrl- binding was introduced.
func TestLeaderTableStillRefusesReservedRunes(t *testing.T) {
	for _, r := range []rune{'c', 'x', 'v'} {
		if leaderActionFor(r) != nil {
			t.Errorf("rune %q became bound; it is reserved for the terminal's own clipboard", r)
		}
	}
}

// TestMenuStartDebugGuardsBeforeSpawning checks the cheap refusals happen on
// the main goroutine and produce a message, without starting a process.
func TestMenuStartDebugGuardsBeforeSpawning(t *testing.T) {
	dir := t.TempDir()
	notGo := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(notGo, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := newTestApp(t, dir)
	a.openFile(notGo)

	a.menuStartDebug()
	if a.debug != nil {
		t.Fatal("a debug session was started for a .txt file")
	}
	if !strings.Contains(a.statusMsg, "debug") && !strings.Contains(a.statusMsg, "Go file") {
		t.Errorf("status %q does not explain why nothing happened", a.statusMsg)
	}

	// A second start while one is coming up must not spawn a second adapter.
	a.debug = &debugSession{adapter: "delve", starting: true}
	a.menuStartDebug()
	if !a.debug.starting {
		t.Error("the in-flight session was replaced")
	}
	if !strings.Contains(a.statusMsg, "already running") {
		t.Errorf("status %q does not say a session is already running", a.statusMsg)
	}
}

// TestMenuDebugStopClearsImmediately checks Stop is instant in the UI: waiting
// for the adapter to confirm would leave ▶ painted over a program the user just
// stopped, which reads as the command not working.
func TestMenuDebugStopClearsImmediately(t *testing.T) {
	a, path := debugFixture(t)
	a.debug = &debugSession{adapter: "delve", running: true, bound: map[string][]boundBreakpoint{}}
	a.handleDebugStopped(&debugStoppedEvent{when: time.Now(), path: path, line: 5, reason: "breakpoint"})

	a.menuDebugStop()

	if a.debug != nil {
		t.Fatal("Stop left the session in place")
	}
	a.draw()
	if got := gutterRuneAt(t, a, 5); got == '▶' {
		t.Error("the marker survived Stop")
	}
}

// TestStopDebugSessionIsSafeWithoutOne pins that quitting with no debug session
// is a no-op rather than a panic, since Run calls this unconditionally.
func TestStopDebugSessionIsSafeWithoutOne(t *testing.T) {
	a, _ := debugFixture(t)
	a.debug = nil
	a.stopDebugSession() // must not panic
	if a.debug != nil {
		t.Error("stopDebugSession created a session")
	}
}

// TestDebugGutterPaintsOnWrappedTabs pins the fix for a real gap: the debug
// overlay used to stand down entirely on a wrapped tab, because `line - ScrollY`
// is off by the number of continuation rows above the line and painting at the
// wrong row is worse than not painting. A wrapped tab therefore showed no
// stopped arrow at all, and only the status bar said where execution had
// stopped. Tab.GutterRowFor now answers for both geometries.
//
// 🔴 The expected row is DERIVED FROM RENDERED OUTPUT, not recomputed: the tab
// is drawn once with no debug session to find which row actually carries the
// line, then drawn again with one to assert the arrow landed there. Asserting
// against a second copy of the wrap arithmetic would pass whenever the helper
// and the test were wrong together — the exact failure that let ScreenPos ship
// off-by-one twice under three green tests.
func TestDebugGutterPaintsOnWrappedTabs(t *testing.T) {
	a, path := debugFixture(t)
	tab := a.activeTabPtr()

	// Lines long enough that wrapping actually produces continuation rows; without
	// them a wrapped tab and an unwrapped one agree and the test proves nothing.
	long := strings.Repeat("xy ", 40)
	for i := 0; i < 3; i++ {
		tab.Buffer.Lines[i] = "// " + long
	}
	tab.Wrap = true
	tab.StyleStale = true

	const target = 5 // zero-based; the "sum := a + b" line

	// Pass 1: no debug session, so nothing overlays the gutter. Find the row that
	// actually carries this line by looking for its rendered line number.
	a.draw()
	a.screen.Show()
	scr := a.screen.(tcell.SimulationScreen)
	wantRow := -1
	for y := 0; y < a.height; y++ {
		if strings.Contains(screenLine(scr, y), fmtLineNo(target+1)+" ") {
			wantRow = y
			break
		}
	}
	if wantRow < 0 {
		t.Fatalf("fixture never rendered line %d on a wrapped tab", target+1)
	}

	// Pass 2: stopped on that line. The arrow must land on the row pass 1 found.
	a.debug = &debugSession{adapter: "delve", running: true, bound: map[string][]boundBreakpoint{}}
	a.handleDebugStopped(&debugStoppedEvent{when: time.Now(), path: path, line: target, reason: "breakpoint"})
	tab.Wrap = true
	a.draw()
	a.screen.Show()

	ex, _, _, _ := a.editorRect()
	got, _, _, _ := scr.GetContent(ex, wantRow)
	if got != '▶' {
		t.Fatalf("wrapped tab: gutter cell at the line's own row %d is %q, want %q",
			wantRow, string(got), "▶")
	}
	if !strings.Contains(a.debugStatus(), "main.go:6") {
		t.Errorf("status %q lost the location on a wrapped tab", a.debugStatus())
	}
}

// fmtLineNo renders a line number the way the gutter does, so the test can find
// the row by reading the screen rather than by recomputing wrap geometry.
func fmtLineNo(n int) string { return strconv.Itoa(n) }

// requireDlvForApp is the anti-skip gate for the app-level live oracle, matching
// internal/dap/live_dlv_test.go: skip when delve is absent so a fresh clone
// stays green, fail when HERDR_REQUIRE_DAP=1 says it must be there.
func requireDlvForApp(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("dlv"); err != nil {
		msg := "dlv is not installed — `go install github.com/go-delve/delve/cmd/dlv@latest`"
		if os.Getenv("HERDR_REQUIRE_DAP") == "1" {
			t.Fatalf("HERDR_REQUIRE_DAP=1 but the live app oracle could not run: %s", msg)
		}
		t.Skip(msg)
	}
}

// pumpEvents drains the tcell queue into handleEvent until pred is satisfied or
// the deadline passes — a stand-in for Run's loop, which a test cannot use
// because Run blocks forever.
//
// This is what makes the oracle below an END-TO-END test rather than another
// unit test: every event travels the real path, from the adapter's read
// goroutine through a.post onto the tcell queue and into a.handleEvent.
func pumpEvents(t *testing.T, a *App, timeout time.Duration, pred func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return true
		}
		if a.screen.HasPendingEvent() {
			if ev := a.screen.PollEvent(); ev != nil {
				a.handleEvent(ev)
			}
			continue
		}
		time.Sleep(10 * time.Millisecond)
	}
	return pred()
}

// TestDebugLiveEndToEndStopsAndPaints is the whole definition of done for Lane
// B stage 2, driven through the REAL editor against a REAL delve:
//
//	press F5 → the program runs → it stops on your breakpoint →
//	the editor opens that file and paints ▶ on the line.
//
// 🔴 Why this exists on top of the dap package's live oracle and the simulated
// app tests: those two prove the protocol client works and that the painting
// code paints. Neither proves the WIRING between them — that F5 reaches
// menuStartDebug, that the launch config names a program delve can build, that
// events posted from a background goroutine are routed by handleEvent to the
// right handler, or that the breakpoint the user set is the line the program
// stops on. Every one of those is a place this stage could be complete and
// still not work, and this fork has shipped exactly that failure three times.
func TestDebugLiveEndToEndStopsAndPaints(t *testing.T) {
	requireDlvForApp(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module appfixture\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "main.go")
	src := "package main\n\nimport \"fmt\"\n\nfunc add(a, b int) int {\n\tsum := a + b\n\treturn sum\n}\n\nfunc main() {\n\tfmt.Println(add(2, 3))\n}\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	a := newTestApp(t, dir)
	a.openFile(path)
	t.Cleanup(a.stopDebugSession)

	// Buffer line 5 is `sum := a + b`. Verified by TEXT, not by number: a
	// hardcoded line that happens to match proves nothing about the 1-based /
	// 0-based conversion this whole path depends on.
	const bpLine = 5
	if got := a.activeTabPtr().LineText(bpLine); !strings.Contains(got, "sum := a + b") {
		t.Fatalf("fixture line %d is %q, not the line meant to be marked", bpLine, got)
	}

	// Set the breakpoint the way a user does: move the cursor, press F9.
	a.activeTabPtr().MoveCursorTo(editor.Position{Line: bpLine, Col: 0}, false)
	a.handleKey(tcell.NewEventKey(tcell.KeyF9, 0, tcell.ModNone))
	a.syncBreakpoints()
	if len(a.breakpoints) != 1 {
		t.Fatalf("F9 did not register a breakpoint: %v", a.breakpoints)
	}

	// Press F5.
	a.handleKey(tcell.NewEventKey(tcell.KeyF5, 0, tcell.ModNone))
	if a.debug == nil {
		t.Fatal("F5 did not start a debug session")
	}

	if !pumpEvents(t, a, 180*time.Second, func() bool {
		return a.debug != nil && a.debug.stopped && a.debug.path != ""
	}) {
		state := "no session"
		if a.debug != nil {
			state = fmt.Sprintf("starting=%v running=%v stopped=%v", a.debug.starting, a.debug.running, a.debug.stopped)
		}
		t.Fatalf("the program never stopped on the breakpoint (%s). last status: %q", state, a.statusMsg)
	}

	a.draw()

	if got := a.debug.line; got != bpLine {
		t.Errorf("stopped on 0-based line %d, want %d", got, bpLine)
	}
	if got := a.activeTabPtr().LineText(a.debug.line); !strings.Contains(got, "sum := a + b") {
		t.Errorf("stopped on a line whose text is %q, not the marked line", got)
	}
	if got := gutterRuneAt(t, a, bpLine); got != '▶' {
		t.Errorf("the gutter on the stopped line is %q, want '▶'", got)
	}
	if got := a.activeTabPtr().Cursor.Line; got != bpLine {
		t.Errorf("the cursor is on line %d, want the stopped line %d", got, bpLine)
	}
	if !strings.Contains(a.debugStatus(), "main.go:6") {
		t.Errorf("status %q does not report the 1-based stopped location", a.debugStatus())
	}
	t.Logf("F5 → stopped at %s · gutter shows ▶", a.debugStatus())

	// And F5 again continues, running the program to completion.
	a.handleKey(tcell.NewEventKey(tcell.KeyF5, 0, tcell.ModNone))
	if !pumpEvents(t, a, 60*time.Second, func() bool { return a.debug == nil }) {
		t.Fatalf("F5 did not resume the program to completion; session still %+v", a.debug)
	}
	a.draw()
	if got := gutterRuneAt(t, a, bpLine); got == '▶' {
		t.Error("the stopped marker survived the program running to completion")
	}
	t.Logf("F5 again → ran to completion, marker cleared, status: %q", a.statusMsg)
}
