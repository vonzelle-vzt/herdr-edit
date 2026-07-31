// =============================================================================
// File: internal/app/debugview_test.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package app

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/cloudmanic/spice-edit/internal/dap"
)

// fakeAdapterClient returns a REAL dap.Client attached to a pipe nothing ever
// answers, for the tests that must get past a "is there a client?" guard
// without an adapter behind it.
//
// 🔴 It is a real client rather than a zero-valued &dap.Client{}, because the
// zero value has a nil pending map and any request against it panics inside the
// background goroutine — a crash in a place no test assertion is looking. The
// pipe's far end is closed on cleanup, which unblocks both the read loop and
// any request still waiting on a write.
func fakeAdapterClient(t *testing.T) *dap.Client {
	t.Helper()
	ours, theirs := net.Pipe()
	t.Cleanup(func() { _ = theirs.Close() })
	return dap.StartConn("test-adapter", ours, dap.Handlers{})
}

// stoppedFixture opens a Go file and puts the app into a stopped debug session
// with a two-frame stack, which is the state every inspector below needs.
//
// The client is a real one attached to a pipe nothing answers, so the "is there
// a session?" guards pass and the guards actually under test are the ones that
// fire. A nil client would short-circuit every one of them, and a test that
// asserted on the resulting message would be pinning down the wrong refusal.
func stoppedFixture(t *testing.T) (*App, string) {
	t.Helper()
	a, path := debugFixture(t)
	a.debug = &debugSession{
		adapter: "delve", running: true, stopped: true, threadID: 1,
		client: fakeAdapterClient(t),
		bound:  map[string][]boundBreakpoint{},
	}
	a.handleDebugStopped(&debugStoppedEvent{
		when: time.Now(), path: path, line: 5, frame: "main.add",
		reason: "breakpoint", threadID: 1,
		frames: []debugFrame{
			{ID: 1000, Name: "main.add", Path: path, Line: 5},
			{ID: 1001, Name: "main.main", Path: path, Line: 10},
		},
	})
	return a, path
}

// paletteLabels lists the labels currently offered by the open picker, in the
// order they would be shown.
func paletteLabels(a *App) []string {
	out := make([]string, 0, len(a.paletteResults))
	for _, r := range a.paletteResults {
		out = append(out, r.cmd.label)
	}
	return out
}

// runPaletteRow fires the first row whose label contains want, which is how a
// test drives a picker the way Enter does.
func runPaletteRow(t *testing.T, a *App, want string) {
	t.Helper()
	for i, r := range a.paletteResults {
		if strings.Contains(r.cmd.label, want) {
			a.paletteSelected = i
			a.runSelectedPaletteCommand()
			return
		}
	}
	t.Fatalf("no picker row containing %q; rows are %v", want, paletteLabels(a))
}

// TestSteppingRefusalsNameTheAction pins the guard messages apart.
//
// All three commands share beginStep, so a single "no debug session" message
// would make the three keys indistinguishable — and a call-site oracle could
// not then tell F11 wired to StepIn from F11 wired to Next. The refusal names
// the action for that reason as much as for the user's.
func TestSteppingRefusalsNameTheAction(t *testing.T) {
	a, _ := debugFixture(t)

	for _, tc := range []struct {
		name string
		fn   func()
		want string
	}{
		{"step over", a.menuDebugStepOver, "Step over"},
		{"step into", a.menuDebugStepIn, "Step into"},
		{"step out", a.menuDebugStepOut, "Step out"},
	} {
		a.debug = nil
		a.statusMsg = ""
		tc.fn()
		if !strings.Contains(a.statusMsg, tc.want) {
			t.Errorf("%s with no session said %q, want a message naming %q", tc.name, a.statusMsg, tc.want)
		}
		if !strings.Contains(a.statusMsg, "F5") {
			t.Errorf("%s said %q, which does not tell the user how to start one", tc.name, a.statusMsg)
		}
	}
}

// TestSteppingNeedsAStoppedProgram covers the other guard: a step aimed at a
// running program would be refused by the adapter after a round trip, and the
// user would see nothing until it timed out.
func TestSteppingNeedsAStoppedProgram(t *testing.T) {
	a, _ := stoppedFixture(t)
	a.debug.stopped = false

	a.statusMsg = ""
	a.menuDebugStepOver()
	if !strings.Contains(a.statusMsg, "stopped") {
		t.Errorf("stepping a running program said %q, want an explanation", a.statusMsg)
	}
	// And nothing was consumed: the session is untouched, so F6 still works.
	if a.debug == nil {
		t.Fatal("the refusal destroyed the session")
	}
}

// TestPauseIsRefusedWhileStopped is Pause's mirror: it only makes sense on a
// running program.
func TestPauseIsRefusedWhileStopped(t *testing.T) {
	a, _ := stoppedFixture(t)
	a.statusMsg = ""
	a.menuDebugPause()
	if !strings.Contains(a.statusMsg, "already stopped") {
		t.Errorf("pause while stopped said %q", a.statusMsg)
	}
}

// TestStackPickerNamesEachFrame pins the row format the brief specifies —
// "main.go:42  handleEvent" — and that the frames come from the stop rather
// than from an adapter round trip.
func TestStackPickerNamesEachFrame(t *testing.T) {
	a, _ := stoppedFixture(t)

	a.menuDebugStack()
	if !a.paletteOpen {
		t.Fatalf("the call stack picker did not open; status %q", a.statusMsg)
	}
	labels := paletteLabels(a)
	if len(labels) != 2 {
		t.Fatalf("picker has %d rows, want 2 frames: %v", len(labels), labels)
	}
	// 1-BASED in the label for a 0-based buffer line 5.
	if !strings.Contains(labels[0], "main.go:6") || !strings.Contains(labels[0], "main.add") {
		t.Errorf("top row = %q, want main.go:6 and the function name", labels[0])
	}
	if !strings.Contains(labels[1], "main.go:11") || !strings.Contains(labels[1], "main.main") {
		t.Errorf("caller row = %q, want main.go:11 and the function name", labels[1])
	}
}

// TestStackPickerIsRefusedWhileRunning covers the enablement rule: a frame id
// is meaningless while the program is running, so the picker must not open
// over a stale frame list.
func TestStackPickerIsRefusedWhileRunning(t *testing.T) {
	a, _ := stoppedFixture(t)
	a.debug.stopped = false

	a.menuDebugStack()
	if a.paletteOpen {
		t.Error("the call stack opened for a running program")
	}
	if !strings.Contains(a.statusMsg, "stopped") {
		t.Errorf("status = %q, want an explanation", a.statusMsg)
	}
}

// TestSelectFrameJumpsAndRescopes is the oracle for the brief's "jumps the
// editor AND re-scopes variables": picking the caller moves the cursor there,
// moves the ▶ with it, and makes the NEXT variables request read that frame.
//
// The ▶ assertion reads the rendered screen rather than a.debug.line, because
// the gutter overlay is the thing the user actually sees.
func TestSelectFrameJumpsAndRescopes(t *testing.T) {
	a, _ := stoppedFixture(t)

	a.menuDebugStack()
	runPaletteRow(t, a, "main.main")
	a.draw()

	if got := a.debug.curFrame; got != 1 {
		t.Errorf("curFrame = %d, want the caller's index 1", got)
	}
	f, ok := a.currentFrame()
	if !ok || f.ID != 1001 {
		t.Fatalf("currentFrame = %+v (ok=%v); variables would be read from the wrong frame", f, ok)
	}
	if got := a.activeTabPtr().Cursor.Line; got != 10 {
		t.Errorf("cursor is on 0-based line %d, want the caller's line 10", got)
	}
	if got := gutterRuneAt(t, a, 10); got != '▶' {
		t.Errorf("the marker did not follow the selected frame: gutter on line 10 is %q", got)
	}
	if got := gutterRuneAt(t, a, 5); got == '▶' {
		t.Error("the marker is painted on BOTH frames' lines")
	}
	if !strings.Contains(a.debugStatus(), "main.go:11") {
		t.Errorf("status %q does not follow the selected frame", a.debugStatus())
	}
}

// TestSelectFrameOutOfRangeIsSafe pins that a stale picker — one whose frames
// went away while it was open — refuses rather than indexing out of the slice.
func TestSelectFrameOutOfRangeIsSafe(t *testing.T) {
	a, _ := stoppedFixture(t)
	a.debug.frames = nil

	a.selectFrame(3) // must not panic
	if !strings.Contains(a.statusMsg, "no longer available") {
		t.Errorf("status = %q, want a refusal", a.statusMsg)
	}
}

// TestThreadPickerSwitchesTheTargetGoroutine covers the goroutine list: the
// rows name each thread, and picking one re-aims every later request at it.
func TestThreadPickerSwitchesTheTargetGoroutine(t *testing.T) {
	a, _ := stoppedFixture(t)

	a.handleDebugThreads(&debugThreadsEvent{when: time.Now(), threads: []dap.Thread{
		{ID: 1, Name: "* [Go 1] main.main"},
		{ID: 17, Name: "[Go 17] runtime.gopark"},
	}})
	if !a.paletteOpen {
		t.Fatalf("the goroutine picker did not open; status %q", a.statusMsg)
	}
	labels := paletteLabels(a)
	if len(labels) != 2 {
		t.Fatalf("picker has %d rows, want 2: %v", len(labels), labels)
	}

	// Selecting the second one re-aims the session at it. The stack refresh it
	// kicks off runs on a goroutine against a pipe nothing answers, so what is
	// asserted here is the state change every LATER request depends on.
	runPaletteRow(t, a, "Go 17")
	if got := a.debug.threadID; got != 17 {
		t.Errorf("threadID = %d after picking goroutine 17", got)
	}
	// Switching goroutine invalidates the previous one's references.
	if a.debug.varCache != nil {
		t.Errorf("varCache survived a goroutine switch: %v", a.debug.varCache)
	}
}

// TestThreadPickerReportsAnAdapterError pins that a refused threads request
// says so rather than opening an empty picker.
func TestThreadPickerReportsAnAdapterError(t *testing.T) {
	a, _ := stoppedFixture(t)
	a.handleDebugThreads(&debugThreadsEvent{when: time.Now(), err: "unknown goroutine"})
	if a.paletteOpen {
		t.Error("a failed threads request opened a picker anyway")
	}
	if !strings.Contains(a.statusMsg, "unknown goroutine") {
		t.Errorf("status = %q, want the adapter's reason", a.statusMsg)
	}
}

// TestVariablePickerShowsNameValueAndExpandability pins the row format: a leaf
// reads "name = value", an expandable value is marked, and a multi-line struct
// value is flattened onto the single line a picker row actually is.
func TestVariablePickerShowsNameValueAndExpandability(t *testing.T) {
	a, _ := stoppedFixture(t)

	a.handleDebugVars(&debugVarsEvent{when: time.Now(), title: "Variables — main.add", page: debugVarPage{
		vars: []debugVar{
			{Name: "sum", Value: "5", Type: "int", Scope: "Locals"},
			{Name: "cfg", Value: "main.Config {\n\tName: \"x\",\n}", Type: "main.Config", Ref: 1002, Scope: "Locals"},
		},
	}})
	if !a.paletteOpen {
		t.Fatalf("the variables picker did not open; status %q", a.statusMsg)
	}
	labels := paletteLabels(a)
	if len(labels) != 2 {
		t.Fatalf("picker has %d rows, want 2: %v", len(labels), labels)
	}
	if labels[0] != "sum = 5" {
		t.Errorf("leaf row = %q, want %q", labels[0], "sum = 5")
	}
	if !strings.HasPrefix(labels[1], "▸ ") {
		t.Errorf("expandable row = %q, want it marked as expandable", labels[1])
	}
	if strings.ContainsAny(labels[1], "\n\t") {
		t.Errorf("row %q still contains a newline or tab; it would draw over the rest of the screen", labels[1])
	}
}

// TestVariablesTruncationIsStated is the required oracle for the paging cap.
//
// 🔴 A silently capped list is a lie: shown 200 entries of a slice with 5000 and
// told nothing, the user has been told the slice has 200. The notice is
// asserted in TWO places on purpose — in a row, and in the picker's TITLE,
// which is read back off the RENDERED SCREEN. Only the title is reliably
// visible: the palette shows ten rows at a time and filters as you type, so a
// note at position 201 is a note nobody ever sees.
func TestVariablesTruncationIsStated(t *testing.T) {
	a, _ := stoppedFixture(t)

	over := make([]debugVar, 0, maxDebugVariables)
	for i := 0; i < maxDebugVariables; i++ {
		over = append(over, debugVar{Name: "elem" + itoa(i), Value: itoa(i), Type: "int", Scope: "Locals"})
	}
	a.handleDebugVars(&debugVarsEvent{when: time.Now(), title: "Variables — main.add", ref: 1000, page: debugVarPage{
		vars:      over,
		total:     5000,
		truncated: true,
	}})
	if !a.paletteOpen {
		t.Fatalf("the variables picker did not open; status %q", a.statusMsg)
	}

	// The title says so, and it is what is on screen.
	if !strings.Contains(a.paletteTitle, "truncated") {
		t.Errorf("picker title = %q, which does not admit the list was capped", a.paletteTitle)
	}
	if !strings.Contains(a.paletteTitle, itoa(maxDebugVariables)) || !strings.Contains(a.paletteTitle, "5000") {
		t.Errorf("picker title = %q, want it to name how many of how many are shown", a.paletteTitle)
	}
	screen := paint(t, a, 120, 40)
	if !strings.Contains(screen, "truncated") {
		t.Fatalf("nothing on the rendered screen says the list was truncated.\n%s", screen)
	}

	// And a row says so too, for anyone who scrolls to the end.
	labels := paletteLabels(a)
	last := labels[len(labels)-1]
	if !strings.Contains(last, "truncated") {
		t.Errorf("last row = %q, want the truncation notice", last)
	}
	if got, want := len(labels), maxDebugVariables+1; got != want {
		t.Errorf("picker has %d rows, want %d variables plus the notice", got, want)
	}
}

// TestUntruncatedListSaysNothingExtra is the other half: the notice must not
// appear when nothing was left out, or it becomes noise nobody reads.
func TestUntruncatedListSaysNothingExtra(t *testing.T) {
	a, _ := stoppedFixture(t)
	a.handleDebugVars(&debugVarsEvent{when: time.Now(), title: "Variables — main.add", page: debugVarPage{
		vars: []debugVar{{Name: "sum", Value: "5", Type: "int"}},
	}})
	if strings.Contains(a.paletteTitle, "truncated") {
		t.Errorf("title = %q claims truncation for a complete list", a.paletteTitle)
	}
	if len(a.paletteResults) != 1 {
		t.Errorf("picker has %d rows, want just the one variable: %v", len(a.paletteResults), paletteLabels(a))
	}
}

// TestExpandingAVariableUsesTheCacheAndRefusesWhenRunning covers both halves of
// the perishable-reference rule at the picker level: a cached page is reused
// within one stop, and an expansion is refused outright once the program is
// running, because the reference behind it is dead.
func TestExpandingAVariableUsesTheCacheAndRefusesWhenRunning(t *testing.T) {
	a, _ := stoppedFixture(t)
	a.debug.varCache = map[int]debugVarPage{
		1002: {vars: []debugVar{{Name: "Name", Value: `"x"`, Type: "string"}}},
	}

	// The fixture's client is attached to a pipe nothing answers, so a cache hit
	// that reached the adapter would hang rather than quietly pass.
	a.expandVariable("Variables — main.add", debugVar{Name: "cfg", Ref: 1002})
	if !a.paletteOpen {
		t.Fatalf("expanding a cached variable did not open a picker; status %q", a.statusMsg)
	}
	if got := paletteLabels(a); len(got) != 1 || !strings.Contains(got[0], "Name") {
		t.Errorf("cached expansion showed %v", got)
	}
	if !strings.Contains(a.paletteTitle, "cfg") {
		t.Errorf("expansion title = %q, want it to name the variable", a.paletteTitle)
	}

	// 🔴 Once running, the same reference must NOT be served from the cache:
	// the adapter may have reused that number for something else entirely.
	a.closePalette()
	a.debug.stopped = false
	a.expandVariable("Variables — main.add", debugVar{Name: "cfg", Ref: 1002})
	if a.paletteOpen {
		t.Error("a variables reference was expanded while the program was running")
	}
	if !strings.Contains(a.statusMsg, "running") {
		t.Errorf("status = %q, want an explanation", a.statusMsg)
	}
}

// TestExpandingALeafShowsTheWholeValue pins that Enter on a row whose value was
// elided to fit still does something — that row is exactly the one you press
// Enter on, and nothing happening reads as the picker being broken.
func TestExpandingALeafShowsTheWholeValue(t *testing.T) {
	a, _ := stoppedFixture(t)
	long := strings.Repeat("abcdefghij", 12)
	a.expandVariable("Variables", debugVar{Name: "s", Value: long})
	if !strings.Contains(a.statusMsg, long[:60]) {
		t.Errorf("status = %q, want the full value of the leaf", a.statusMsg)
	}
}

// TestDebugConsoleIsASyntheticTab pins the console's shape: a synthetic tab (so
// Save cannot write it anywhere), holding the program's output.
func TestDebugConsoleIsASyntheticTab(t *testing.T) {
	a, _ := stoppedFixture(t)
	a.debug.output = []string{"first line", "DAP-FIXTURE-PRINTED-THIS 5"}

	before := len(a.tabs)
	a.menuDebugConsole()
	if len(a.tabs) != before+1 {
		t.Fatalf("tab count %d -> %d, want one console tab added", before, len(a.tabs))
	}
	tab := a.activeTabPtr()
	if !tab.Synthetic {
		t.Error("the console is not a synthetic tab; Save would write it to disk")
	}
	if tab.Path != "" {
		t.Errorf("the console tab has Path %q; a synthetic tab must have none", tab.Path)
	}
	body := tab.Buffer.String()
	for _, want := range []string{"DAP-FIXTURE-PRINTED-THIS 5", "first line", "snapshot"} {
		if !strings.Contains(body, want) {
			t.Errorf("console body is missing %q:\n%s", want, body)
		}
	}

	// Re-opening refreshes in place rather than stacking a second console.
	a.debug.output = append(a.debug.output, "later line")
	a.menuDebugConsole()
	if len(a.tabs) != before+1 {
		t.Errorf("re-opening stacked a second console tab (%d tabs)", len(a.tabs))
	}
	if !strings.Contains(a.activeTabPtr().Buffer.String(), "later line") {
		t.Error("re-opening did not refresh the console")
	}
}

// TestDebugConsoleOutlivesTheSession is the reason lastDebugOutput exists: the
// output of a program that RAN TO COMPLETION is the output you most want to
// read, and handleDAPTerminated drops the session that held it.
func TestDebugConsoleOutlivesTheSession(t *testing.T) {
	a, _ := stoppedFixture(t)
	a.debug.output = []string{"answer: 5"}

	a.handleDAPTerminated(0, true)
	if a.debug != nil {
		t.Fatal("precondition: the session should be gone")
	}
	if !a.hasDebugConsole() {
		t.Fatal("the console became unavailable the moment the program exited")
	}

	a.menuDebugConsole()
	body := a.activeTabPtr().Buffer.String()
	if !strings.Contains(body, "answer: 5") {
		t.Errorf("the finished run's output was lost:\n%s", body)
	}

	// The same must hold for a run the user ended by hand. Stop takes a
	// different exit from the session than a program exiting, and only
	// archiving on one of them loses the output of whichever the user picked.
	b, _ := stoppedFixture(t)
	b.debug.output = []string{"partial: 3"}
	b.menuDebugStop()
	if !b.hasDebugConsole() {
		t.Fatal("stopping by hand threw the run's output away")
	}
	b.menuDebugConsole()
	if got := b.activeTabPtr().Buffer.String(); !strings.Contains(got, "partial: 3") {
		t.Errorf("a manually stopped run's output was lost:\n%s", got)
	}
}

// TestDebugConsoleRefusesWithNothingToShow pins the empty case: no session and
// no previous run means there is nothing to open, and a blank tab would be
// worse than a message.
func TestDebugConsoleRefusesWithNothingToShow(t *testing.T) {
	a, _ := debugFixture(t)
	before := len(a.tabs)
	a.menuDebugConsole()
	if len(a.tabs) != before {
		t.Error("an empty console tab was opened anyway")
	}
	if a.statusMsg == "" {
		t.Error("nothing explained why no console appeared")
	}
}

// TestRenderDebugConsoleStatesItsScope pins the header. The console is a
// snapshot and a capped one; presenting a truncated, frozen view as if it were
// the whole live output is the same failure this fork already refuses in the
// search results and the problems list.
func TestRenderDebugConsoleStatesItsScope(t *testing.T) {
	got := renderDebugConsole("debug: delve running", []string{"a", "b"})
	for _, want := range []string{"debug: delve running", "snapshot", "2 lines", itoa(maxDebugOutput)} {
		if !strings.Contains(got, want) {
			t.Errorf("console header is missing %q:\n%s", want, got)
		}
	}
	empty := renderDebugConsole("", nil)
	if !strings.Contains(empty, "has not printed anything") {
		t.Errorf("an empty console does not say so:\n%s", empty)
	}
	if !strings.Contains(empty, "last run") {
		t.Errorf("a console with no session does not say whose output it is:\n%s", empty)
	}
}

// TestDebugPickerIsDerivedFromTheMenuGroup is the anti-drift oracle for Esc 5.
//
// 🔴 The picker and the ≡ menu must offer the same actions, and the only way to
// guarantee that is for both to read ONE list. An oracle that restated the
// expected labels would police nothing — it would drift with whichever copy the
// author remembered. This derives the expectation from debugMenuGroup() and
// checks the picker reproduces it exactly.
func TestDebugPickerIsDerivedFromTheMenuGroup(t *testing.T) {
	a, _ := stoppedFixture(t)

	a.menuDebugPicker()
	if !a.paletteOpen {
		t.Fatalf("Esc 5 did not open the debug picker; status %q", a.statusMsg)
	}
	if a.paletteTitle != "Debug" {
		t.Errorf("picker title = %q, want Debug", a.paletteTitle)
	}

	want := make(map[string]bool)
	for _, it := range debugMenuGroup() {
		label := it.label
		if it.labelFor != nil {
			label = it.labelFor(a)
		}
		if label != "" && it.action != nil {
			want[label] = true
		}
	}
	got := make(map[string]bool)
	for _, l := range paletteLabels(a) {
		got[l] = true
	}
	for label := range want {
		if !got[label] {
			t.Errorf("the debug picker is missing %q, which is in the menu group", label)
		}
	}
	for label := range got {
		if !want[label] {
			t.Errorf("the debug picker offers %q, which is not in the menu group", label)
		}
	}
	// The picker must not list itself, or picking a row reopens the picker.
	if got["Debug actions"] {
		t.Error("the debug picker lists itself")
	}
}

// TestDebugMenuRowsExistForEveryStage3Action is the CALL-SITE oracle CLAUDE.md
// demands: an action that is not in the ≡ menu is unreachable on a terminal
// that swallows function keys, and this fork has shipped complete, tested,
// unreachable features three times. Function keys are a bonus path, never the
// path.
func TestDebugMenuRowsExistForEveryStage3Action(t *testing.T) {
	a, _ := stoppedFixture(t)
	items, _, _ := a.menuLayout()

	labels := make(map[string]menuItemDef, len(items))
	for _, it := range items {
		label := it.label
		if it.labelFor != nil {
			label = it.labelFor(a)
		}
		labels[label] = it
	}
	for _, tc := range []struct{ label, shortcut string }{
		{"Step over", "F10"},
		{"Step into", "F11"},
		{"Step out", "F12"},
		{"Pause", "F6"},
		{"Call stack", ""},
		{"Goroutines (threads)", ""},
		{"Variables", ""},
		{"Debug console", ""},
		{"Debug actions", "Esc 5"},
	} {
		it, ok := labels[tc.label]
		if !ok {
			t.Errorf("no %q row in the action menu", tc.label)
			continue
		}
		if it.action == nil {
			t.Errorf("the %q row has no action", tc.label)
		}
		if it.shortcut != tc.shortcut {
			t.Errorf("%q advertises shortcut %q, want %q", tc.label, it.shortcut, tc.shortcut)
		}
	}

	// Enablement: the inspectors are on while stopped, off once running.
	for _, label := range []string{"Step over", "Call stack", "Variables"} {
		if !labels[label].enabled(a) {
			t.Errorf("%q is disabled while stopped", label)
		}
	}
	a.debug.stopped = false
	items, _, _ = a.menuLayout()
	for _, it := range items {
		if it.label == "Step over" && it.enabled(a) {
			t.Error("Step over is enabled while the program is running")
		}
		if it.label == "Pause" && !it.enabled(a) {
			t.Error("Pause is disabled while the program is running")
		}
	}
}

// TestEscFiveOpensTheDebugPicker drives the real key sequence, because a leader
// entry that resolves is not the same as a leader entry that fires.
func TestEscFiveOpensTheDebugPicker(t *testing.T) {
	a, _ := stoppedFixture(t)
	a.handleKey(keyEv(tcell.KeyEsc, 0))
	a.handleKey(keyEv(tcell.KeyRune, '5'))
	if !a.paletteOpen || a.paletteTitle != "Debug" {
		t.Fatalf("Esc 5 left paletteOpen=%v title=%q", a.paletteOpen, a.paletteTitle)
	}
}

// TestEmptyPickerNeverLeavesAStaleOverride is the guard for the palette hazard
// the whole file's header calls out.
//
// 🔴 openPaletteWith returns EARLY and untouched when handed an empty list — it
// does not close modals, does not set paletteOpen, and above all does not clear
// paletteOverride. So a caller that hands it nothing while another picker is
// open leaves the old list in place, and the next Esc k shows last week's
// commands instead of the command palette. Every entry point must therefore
// check emptiness itself and flash. This drives the refusal paths with a picker
// already open and asserts the override is gone.
func TestEmptyPickerNeverLeavesAStaleOverride(t *testing.T) {
	refusals := []struct {
		name  string
		setup func(*App)
		fire  func(*App)
	}{
		{"call stack with no frames",
			func(a *App) { a.debug.frames = nil },
			func(a *App) { a.menuDebugStack() }},
		{"variables with no frame",
			func(a *App) { a.debug.frames = nil },
			func(a *App) { a.menuDebugVariables() }},
		{"variables answering with nothing",
			func(a *App) {},
			func(a *App) {
				a.handleDebugVars(&debugVarsEvent{when: time.Now(), title: "Variables", page: debugVarPage{}})
			}},
		{"threads with no session",
			func(a *App) { a.debug = nil },
			func(a *App) { a.menuDebugThreads() }},
		{"threads answering with nothing",
			func(a *App) {},
			func(a *App) { a.handleDebugThreads(&debugThreadsEvent{when: time.Now()}) }},
		{"console with nothing to show",
			func(a *App) { a.debug = nil; a.lastDebugOutput = nil },
			func(a *App) { a.menuDebugConsole() }},
	}

	for _, r := range refusals {
		a, _ := stoppedFixture(t)

		// Put a picker up and close it, so a surviving override has something
		// to be. closePalette clears it; the refusal must not put one back.
		a.menuDebugStack()
		if !a.paletteOpen {
			t.Fatalf("%s: precondition failed, no picker was open", r.name)
		}
		a.closePalette()

		r.setup(a)
		a.statusMsg = ""
		r.fire(a)

		if a.paletteOverride != nil {
			t.Errorf("%s left a stale paletteOverride of %d commands; the next Esc k would show it",
				r.name, len(a.paletteOverride))
		}
		if a.paletteOpen {
			t.Errorf("%s opened a picker anyway", r.name)
		}
		if a.statusMsg == "" {
			t.Errorf("%s refused silently", r.name)
		}
	}
}

// TestHandleDebugVarsIgnoresAStaleAnswer pins that a variables answer arriving
// after the user stopped the session cannot panic or resurrect a picker over a
// debugger that is gone.
func TestHandleDebugVarsIgnoresAStaleAnswer(t *testing.T) {
	a, _ := stoppedFixture(t)
	a.debug = nil
	a.handleDebugVars(&debugVarsEvent{when: time.Now(), title: "Variables", page: debugVarPage{
		vars: []debugVar{{Name: "sum", Value: "5"}},
	}})
	if a.paletteOpen {
		t.Error("a late variables answer opened a picker with no session")
	}
	a.handleDebugThreads(&debugThreadsEvent{when: time.Now(), threads: []dap.Thread{{ID: 1}}})
	if a.paletteOpen {
		t.Error("a late threads answer opened a picker with no session")
	}
}

// TestFlattenValuePutsAStructOnOneLine covers the helper directly, including
// the runs of whitespace delve's rendering leaves behind.
func TestFlattenValuePutsAStructOnOneLine(t *testing.T) {
	got := flattenValue("main.Config {\n\tName: \"x\",\n\tPort: 8080,\n}")
	if strings.ContainsAny(got, "\n\t") {
		t.Errorf("flattenValue left a newline or tab in %q", got)
	}
	if strings.Contains(got, "  ") {
		t.Errorf("flattenValue left a run of spaces in %q", got)
	}
	if !strings.Contains(got, "Port: 8080,") {
		t.Errorf("flattenValue lost content: %q", got)
	}
}

// TestTruncationNoticeHandlesAnUnknownTotal covers the shape delve actually
// produces: namedVariables and indexedVariables both come back 0 (measured), so
// most notices cannot name a denominator and must not invent one.
func TestTruncationNoticeHandlesAnUnknownTotal(t *testing.T) {
	unknown := debugVarPage{vars: make([]debugVar, maxDebugVariables), truncated: true}
	got := truncationNotice(unknown)
	if strings.Contains(got, " of 0") {
		t.Errorf("notice %q invented a total of 0", got)
	}
	if !strings.Contains(got, itoa(maxDebugVariables)) || !strings.Contains(got, "truncated") {
		t.Errorf("notice = %q, want the count shown and the word truncated", got)
	}
	if title := variablePickerTitle("Variables", unknown); !strings.Contains(title, "truncated") ||
		strings.Contains(title, " of 0") {
		t.Errorf("title = %q", title)
	}
}

// TestARefusedStepPutsTheSessionBack pins the recovery path.
//
// 🔴 beginStep moves the session into the resuming state before the request
// goes out, because the program is about to run. If the adapter REFUSES the
// step, nothing ever puts it back: the UI claims the program is running while
// it is still sitting on the same line, the ▶ is gone, stepping and the
// inspectors are all disabled, and F5 answers "a session is already running".
// The only way out would be Stop. stepFailed re-reads the stack for that
// reason, so this drives a real refusal through a fake adapter and watches the
// session come back.
func TestARefusedStepPutsTheSessionBack(t *testing.T) {
	a, path := debugFixture(t)

	// A fake adapter that refuses the step and then answers the recovery
	// stackTrace, which is exactly the sequence stepFailed produces.
	clientEnd, adapterEnd := net.Pipe()
	t.Cleanup(func() { _ = adapterEnd.Close() })
	client := dap.StartConn("refusing-adapter", clientEnd, dap.Handlers{
		OnEvent: func(e dap.Event) { a.post(&debugEvent{when: time.Now(), ev: e}) },
	})

	a.debug = &debugSession{
		adapter: "delve", running: true, stopped: true, threadID: 1,
		client: client, bound: map[string][]boundBreakpoint{},
		frames: []debugFrame{{ID: 1000, Name: "main.add", Path: path, Line: 5}},
	}

	go func() {
		f := &fakeReplier{t: t, conn: adapterEnd, r: bufio.NewReader(adapterEnd)}
		f.refuse("next", "cannot single step while the program is running")
		f.answerStackTrace(path, 6, "main.add")
	}()

	a.menuDebugStepOver()
	if a.debug.stopped {
		t.Fatal("precondition: beginStep should have marked the session as resuming")
	}

	if !pumpEvents(t, a, 10*time.Second, func() bool { return a.debug != nil && a.debug.stopped }) {
		t.Fatalf("a refused step left the session claiming to be running forever; status %q", a.statusMsg)
	}
	if got := a.debug.line; got != 5 {
		t.Errorf("after the refusal the session is on 0-based line %d, want the line it never left (5)", got)
	}
	if len(a.debug.frames) == 0 {
		t.Error("the frame list was not restored; the call stack and variables stay disabled")
	}
	a.draw()
	if got := gutterRuneAt(t, a, 5); got != '▶' {
		t.Errorf("the ▶ did not come back: gutter is %q", got)
	}
}

// fakeReplier answers a couple of requests over a raw pipe, for the refusal
// test above. It is deliberately not internal/dap's fakeAdapter: that one lives
// in the other package and this needs only two canned answers.
type fakeReplier struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
	seq  int
}

// next reads one framed request and returns its seq and command.
func (f *fakeReplier) next() (int, string) {
	f.t.Helper()
	var length int
	for {
		line, err := f.r.ReadString('\n')
		if err != nil {
			return 0, ""
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if strings.HasPrefix(strings.ToLower(line), "content-length:") {
			length, _ = strconv.Atoi(strings.TrimSpace(line[len("content-length:"):]))
		}
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(f.r, body); err != nil {
		return 0, ""
	}
	var req struct {
		Seq     int    `json:"seq"`
		Command string `json:"command"`
	}
	_ = json.Unmarshal(body, &req)
	return req.Seq, req.Command
}

// send frames one JSON message back to the client.
func (f *fakeReplier) send(v interface{}) {
	body, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = f.conn.Write([]byte(fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))))
	_, _ = f.conn.Write(body)
}

// refuse answers the next request of the named command with success:false.
func (f *fakeReplier) refuse(command, reason string) {
	seq, got := f.next()
	if got != command {
		f.t.Errorf("fake adapter expected %q, got %q", command, got)
	}
	f.seq++
	f.send(map[string]interface{}{
		"seq": f.seq, "type": "response", "request_seq": seq,
		"success": false, "command": command, "message": reason,
	})
}

// answerStackTrace answers the next stackTrace with one frame, in the
// adapter's own 1-BASED coordinates.
func (f *fakeReplier) answerStackTrace(path string, line int, name string) {
	seq, got := f.next()
	if got != "stackTrace" {
		f.t.Errorf("fake adapter expected stackTrace, got %q", got)
		return
	}
	f.seq++
	f.send(map[string]interface{}{
		"seq": f.seq, "type": "response", "request_seq": seq,
		"success": true, "command": "stackTrace",
		"body": map[string]interface{}{
			"stackFrames": []map[string]interface{}{
				{"id": 1000, "name": name, "line": line, "column": 0,
					"source": map[string]interface{}{"path": path, "name": "main.go"}},
			},
			"totalFrames": 1,
		},
	})
}
