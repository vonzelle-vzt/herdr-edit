// =============================================================================
// File: internal/dap/live_jsdebug_test.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// live_jsdebug_test.go drives a REAL js-debug through a REAL debug session, and
// it exists to falsify a claim neither of the first two adapters could test: that
// a debug session is one connection to one adapter.
//
// 🔴 js-debug's root session DEBUGS NOTHING. It is a coordinator: it launches
// the program, then sends a `startDebugging` REVERSE REQUEST asking us to open a
// second session for the process it just started, and the debuggee lives only in
// that child. Everything below follows from that, and every one of them would
// have shipped as "a third adapter works" while the debugger silently never
// stopped:
//
//   - TRANSPORT. A THIRD shape. delve is a socket we listen on, debugpy speaks
//     stdio, js-debug is a TCP server we DIAL — and only the third can carry the
//     second connection a child session needs.
//   - REVERSE REQUESTS. The read loop used to skip them with a comment saying
//     nothing answers one. An adapter BLOCKS on a reverse request, so that
//     `continue` is a hang here rather than a no-op.
//   - WHICH CONNECTION HOLDS THE BREAKPOINTS. The root accepts setBreakpoints
//     and answers plausibly. Nothing binds. The program runs to completion and
//     the editor looks like it works.
//   - PROVISIONAL BREAKPOINTS. Every setBreakpoints answer is
//     verified:false / "breakpoint.provisionalBreakpoint", and it then HITS.
//     Read the way delve's answers are read, the gutter marks every working
//     breakpoint broken.
//
// 🔴 THE ANTI-SKIP GATE, identical to the other two live files'. These skip when
// no js-debug can be found so a fresh clone stays green, and HERDR_REQUIRE_DAP=1
// turns every skip into a failure. A skipped test reads as a pass in the summary.
//
//	curl -fsSL -o js-debug.tar.gz \
//	  https://github.com/microsoft/vscode-js-debug/releases/download/v1.117.0/js-debug-dap-v1.117.0.tar.gz
//	mkdir -p ~/.local/share/js-debug && tar -xzf js-debug.tar.gz -C ~/.local/share/js-debug
package dap

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// jsDebugTimeout bounds a whole js-debug session. Nothing is compiled, but the
// session is four processes deep — us, the adapter server, the launcher, and
// node itself — and it is brought up twice, once per connection.
const jsDebugTimeout = 90 * time.Second

// Markers in the JavaScript fixture. Their own constants, not shared with the Go
// or Python fixtures: one file's marker accidentally matching another's is how an
// oracle ends up asserting about the wrong program.
const (
	jsBreakpointMarker = "JS-BREAKPOINT-TARGET"
	jsLoopMarker       = "JS-LOOP-TARGET"
	jsStdoutSentinel   = "JSDEBUG-FIXTURE-PRINTED-THIS"
)

// jsLoopCondition stops the fixture's ten-iteration loop exactly once, and
// jsLoopValue is what the loop variable must be worth when it does.
const (
	jsLoopVarName   = "i"
	jsLoopCondition = "i === 3"
	jsLoopValue     = "3"
)

// requireJsDebug resolves a js-debug adapter or ends the test, and reports where
// it was found.
//
// The origin is logged rather than asserted, exactly as requireDebugpy does:
// which install answers depends on the machine, and pinning one would turn this
// into a test of the developer's node setup.
func requireJsDebug(t *testing.T, root string) *Command {
	t.Helper()
	cmd := LocateJsDebug(root)
	if cmd == nil {
		skipOrFatal(t, "no js-debug could be resolved — download "+
			"js-debug-dap-v1.117.0.tar.gz from github.com/microsoft/vscode-js-debug/releases "+
			"and extract it into ~/.local/share/js-debug/ (node is required too)")
		return nil
	}
	t.Logf("js-debug resolved from %s: argv=%v", cmd.Origin, cmd.Argv)
	return cmd
}

// jsDebugFixture writes a small real Node program and returns its directory,
// file and lines.
func jsDebugFixture(t *testing.T) (root, file string, lines []string) {
	t.Helper()
	root = t.TempDir()
	src := `// A fixture for the live js-debug oracle.

function add(a, b) {
  const total = a + b; // ` + jsBreakpointMarker + `
  return total;
}

function countTo(n) {
  let total = 0;
  for (let i = 0; i < n; i++) {
    total += i; // ` + jsLoopMarker + `
  }
  return total;
}

console.log('` + jsStdoutSentinel + `', add(2, 3), countTo(10));
`
	file = filepath.Join(root, "fixture.js")
	if err := os.WriteFile(file, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, file, strings.Split(src, "\n")
}

// resolvePath renders a path the way both sides of a comparison can agree on.
//
// 🔴 filepath.Clean is NOT enough here, and this is the first adapter where that
// shows. On macOS t.TempDir() hands back /var/folders/... while js-debug reports
// the same file as /private/var/folders/... — /var is a symlink to /private/var,
// so the two strings differ, both are correct, and Clean leaves them differing.
// An oracle comparing the cleaned strings fails against an adapter that is right,
// which is the worst kind of red: it sends you looking for a bug in path
// handling that is not there.
func resolvePath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return filepath.Clean(p)
}

// jsDebugSession is a live js-debug brought all the way up: a root coordinator
// and the leaf child session the debuggee actually lives in.
type jsDebugSession struct {
	root   *Client
	leaf   *Client
	waiter *stoppedWaiter
	caps   Capabilities
	file   string
	lines  []string
	// answers is what the CHILD said about the breakpoints when they were armed.
	//
	// 🔴 Captured at arming time rather than re-read later, because js-debug's
	// ANSWER to a repeated setBreakpoints is not a description of the breakpoint
	// state. Measured: re-sending an identical set on a live session answers with
	// an EMPTY array while the breakpoint stays perfectly bound and stops the
	// program again. An oracle that re-asked in order to assert would therefore
	// read "no breakpoints" off a session that has one — and, sent before the
	// first bind, a re-send answers {"verified":false,"message":"Unbound
	// breakpoint"}. Neither is the state; both are answers to a question about
	// a change. See TestLiveJsDebugResendKeepsThemBound.
	answers []Breakpoint
}

// startJsDebugSession runs the full two-connection launch sequence and returns
// once the CHILD's configurationDone has been acknowledged.
//
// 🔴 This is the sequence, and every step of it was measured rather than read
// off the specification:
//
//	ROOT:  initialize -> launch SENT -> `initialized` -> configurationDone
//	       -> (only now) startDebugging arrives
//	CHILD: dial the SAME server -> initialize -> launch(the child's config,
//	       VERBATIM) -> `initialized` -> setBreakpoints -> configurationDone
//
// Two traps live in there. The root will not launch the program at all without
// its own configurationDone, so an implementation that waits for startDebugging
// first hangs forever having done nothing wrong-looking. And the breakpoints go
// on the CHILD: the root accepts them and nothing ever binds.
func startJsDebugSession(t *testing.T, ctx context.Context,
	want func(lines []string) []SourceBreakpoint) *jsDebugSession {
	t.Helper()

	root, file, lines := jsDebugFixture(t)
	requireJsDebug(t, root)

	adapter, ok := AdapterFor("javascript")
	if !ok {
		t.Fatal("no adapter registered for javascript")
	}
	if adapter.Transport != TransportServer {
		t.Fatalf("the javascript adapter is configured for transport %v; js-debug is a server we dial",
			adapter.Transport)
	}
	if !adapter.UsesChildSessions {
		t.Fatal("the javascript adapter does not declare UsesChildSessions; " +
			"without it the editor talks to the coordinator and nothing ever binds")
	}

	rootWaiter, rootHandlers := newStoppedWaiter(t)
	reg := NewRegistry(root)
	rootClient, err := reg.Start(ctx, adapter, rootHandlers)
	if err != nil {
		t.Fatalf("starting js-debug: %v", err)
	}
	t.Cleanup(rootClient.Stop)

	rootCaps, err := rootClient.Initialize(ctx, adapter.AdapterID)
	if err != nil {
		t.Fatalf("root initialize: %v (adapter stderr: %v)", err, rootClient.LastStderr())
	}
	t.Logf("js-debug capabilities: conditionalBreakpoints=%v logPoints=%v terminate=%v "+
		"configurationDone=%v variablePaging=%v",
		rootCaps.SupportsConditionalBreakpoints, rootCaps.SupportsLogPoints,
		rootCaps.SupportsTerminateRequest, rootCaps.SupportsConfigurationDoneRequest,
		rootCaps.SupportsVariablePaging)

	cfg := map[string]interface{}{}
	for k, v := range adapter.Launch {
		cfg[k] = v
	}
	// The FILE, not its directory — node runs a script. Same divergence debugpy
	// proved, and the opposite of delve.
	cfg["program"] = file
	if _, err := rootClient.Launch(cfg); err != nil {
		t.Fatalf("root launch: %v (adapter stderr: %v)", err, rootClient.LastStderr())
	}
	if err := rootClient.WaitEvent(ctx, EventInitialized); err != nil {
		t.Fatalf("root never sent initialized: %v (adapter stderr: %v)", err, rootClient.LastStderr())
	}
	// 🔴 No breakpoints on the coordinator, and configurationDone regardless:
	// without it the root never launches and startDebugging never comes.
	if err := rootClient.ConfigurationDone(ctx); err != nil {
		t.Fatalf("root configurationDone: %v", err)
	}

	cs, err := rootClient.AwaitChildSession(ctx)
	if err != nil {
		t.Fatalf("js-debug never asked for a child session: %v (adapter stderr: %v)\n"+
			"root events seen: %v", err, rootClient.LastStderr(), rootWaiter.col.names())
	}
	t.Logf("startDebugging: request=%q configuration=%v", cs.Request, cs.Configuration)
	if cs.Configuration == nil {
		t.Fatal("the child session carries no configuration; there would be nothing to launch it with")
	}

	waiter, handlers := newStoppedWaiter(t)
	leaf, err := rootClient.DialChild("js-debug", handlers)
	if err != nil {
		t.Fatalf("dialing the child session: %v", err)
	}
	t.Cleanup(leaf.Stop)

	caps, err := leaf.Initialize(ctx, adapter.AdapterID)
	if err != nil {
		t.Fatalf("child initialize: %v", err)
	}
	if _, err := leaf.Launch(cs.Configuration); err != nil {
		t.Fatalf("child launch: %v", err)
	}
	if err := leaf.WaitEvent(ctx, EventInitialized); err != nil {
		t.Fatalf("child never sent initialized: %v", err)
	}

	s := &jsDebugSession{root: rootClient, leaf: leaf, waiter: waiter, caps: caps, file: file, lines: lines}

	bps := want(lines)
	answers, err := leaf.SetBreakpoints(ctx, Source{Path: file, Name: filepath.Base(file)}, bps)
	if err != nil {
		t.Fatalf("child setBreakpoints: %v", err)
	}
	if len(answers) != len(bps) {
		t.Fatalf("got %d breakpoint answers for %d requests; the response is positional",
			len(answers), len(bps))
	}
	for i, ans := range answers {
		// 🔴 NOT asserted verified, and that is the finding rather than a
		// weakened assertion. Measured against 1.117.0, js-debug answers every
		// one of these {"verified":false,"message":"breakpoint.provisionalBreakpoint"}
		// and then stops on it. TestLiveJsDebugAnswersProvisionalAndStopsAnyway is
		// where that claim is pinned; here it is only logged, because insisting on
		// verified:true would fail against an adapter that works.
		t.Logf("breakpoint %d asked for line %d, answered %+v", i, bps[i].Line, ans)
	}
	s.answers = answers
	if err := leaf.SetExceptionBreakpoints(ctx, caps.DefaultFilters()); err != nil {
		t.Fatalf("child setExceptionBreakpoints: %v", err)
	}
	if err := leaf.ConfigurationDone(ctx); err != nil {
		t.Fatalf("child configurationDone: %v", err)
	}
	return s
}

// awaitStopped blocks for the next stopped event on the LEAF, reporting what was
// seen when none arrives.
func (s *jsDebugSession) awaitStopped(t *testing.T, what string) StoppedEvent {
	t.Helper()
	select {
	case ev := <-s.waiter.stopped:
		return ev
	case <-time.After(jsDebugTimeout):
		t.Fatalf("no stopped event for %s. child events seen: %v\nadapter stderr: %v",
			what, s.waiter.col.names(), s.root.LastStderr())
	}
	return StoppedEvent{}
}

// assertStoppedOn checks execution halted on the line carrying marker,
// identified by that line's TEXT.
//
// 🔴 By text, never by number — the rule both other live files state. It matters
// most here: this is the THIRD adapter, and js-debug is the first one whose
// stack frames come back through a source-map layer, so a line number that
// happened to agree with the other two is exactly the assumption that would
// carry over unexamined.
func (s *jsDebugSession) assertStoppedOn(t *testing.T, ctx context.Context, ev StoppedEvent, marker string) StackFrame {
	t.Helper()
	frames, err := s.leaf.StackTrace(ctx, ev.ThreadID, 20)
	if err != nil {
		t.Fatalf("stackTrace: %v", err)
	}
	if len(frames) == 0 {
		t.Fatal("stackTrace returned no frames for a stopped thread")
	}
	top := frames[0]
	if top.Line < 1 || top.Line > len(s.lines) {
		t.Fatalf("stopped on line %d, outside the fixture's %d lines", top.Line, len(s.lines))
	}
	text := s.lines[top.Line-1]
	if !strings.Contains(text, marker) {
		t.Fatalf("stopped on line %d whose text is %q — that is not the line marked %q",
			top.Line, strings.TrimSpace(text), marker)
	}
	return top
}

// countStoppedEvents counts how many times the program halted.
func (s *jsDebugSession) countStoppedEvents() int {
	n := 0
	for _, name := range s.waiter.col.names() {
		if name == EventStopped {
			n++
		}
	}
	return n
}

// awaitFinished waits for the program to end, by either event that means it.
func (s *jsDebugSession) awaitFinished(t *testing.T) bool {
	t.Helper()
	deadline := time.Now().Add(jsDebugTimeout)
	for time.Now().Before(deadline) {
		for _, n := range s.waiter.col.names() {
			if n == EventTerminated || n == EventExited {
				return true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// TestLiveJsDebugStopsAtBreakpoint is the definition of done for the third
// adapter: a real JavaScript program, launched under a real js-debug over a
// dialed TCP connection, stopping on the line we marked — in a CHILD session the
// root asked us to open.
//
// It is the same claim the delve and debugpy oracles make, run against the first
// adapter whose session is not the connection it was launched on. Passing all
// three is what makes the layer between them an abstraction rather than a table.
func TestLiveJsDebugStopsAtBreakpoint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), jsDebugTimeout)
	defer cancel()

	s := startJsDebugSession(t, ctx, func(lines []string) []SourceBreakpoint {
		return []SourceBreakpoint{{Line: lineWithMarker(t, lines, jsBreakpointMarker)}}
	})

	stopped := s.awaitStopped(t, "the breakpoint")
	if stopped.Reason != "breakpoint" {
		t.Fatalf("stopped for reason %q, want breakpoint: %+v", stopped.Reason, stopped)
	}
	top := s.assertStoppedOn(t, ctx, stopped, jsBreakpointMarker)

	// 🔴 EvalSymlinks, not Clean. On macOS the fixture is written under
	// /var/folders/... and js-debug reports it as /private/var/folders/... — the
	// same directory through the /var symlink. Comparing the cleaned strings
	// fails on a path that is correct, which would read as the adapter losing
	// track of the source file.
	if got, want := resolvePath(top.Source.Path), resolvePath(s.file); got != want {
		t.Errorf("stopped in %q, want %q", got, want)
	}
	if top.Name == "" {
		t.Error("the top frame has no function name")
	}
	t.Logf("stopped in %s at %s:%d — %q",
		top.Name, filepath.Base(top.Source.Path), top.Line, strings.TrimSpace(s.lines[top.Line-1]))

	// The inspection chain end to end on the third adapter: a frame id turns into
	// an evaluate answered IN THAT FRAME. `a` is 2 only because this is add()'s
	// frame and the call passed 2.
	res, err := s.leaf.Evaluate(ctx, "a", top.ID, EvalContextWatch)
	if err != nil {
		t.Fatalf("evaluate(\"a\") in frame %d: %v", top.ID, err)
	}
	if res.Result != "2" {
		t.Errorf("evaluate(\"a\") = %q, want \"2\" — that is another frame's scope", res.Result)
	}
	t.Logf("evaluate(\"a\") = %q (type %q)", res.Result, res.Type)

	if err := s.leaf.Disconnect(ctx, true); err != nil {
		t.Errorf("disconnect: %v", err)
	}
}

// TestLiveJsDebugAnswersProvisionalAndStopsAnyway pins the measurement that
// decides how the gutter draws.
//
// 🔴 js-debug answers EVERY setBreakpoints with verified:false and
// message:"breakpoint.provisionalBreakpoint" — no line, no `breakpoint` event
// later to upgrade it — and then stops on the breakpoint. Read the way delve's
// answers are read, that marks every working js-debug breakpoint as one that
// "could not be set on an executable line", in the gutter and in the Debug panel
// at once. Adapter.BreakpointsBindLazily is what stops the editor saying it, and
// this is the test that fails if a future js-debug starts answering honestly and
// the flag becomes a lie in the other direction.
func TestLiveJsDebugAnswersProvisionalAndStopsAnyway(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), jsDebugTimeout)
	defer cancel()

	adapter, _ := AdapterFor("javascript")
	if !adapter.BreakpointsBindLazily {
		t.Fatal("the javascript adapter does not declare BreakpointsBindLazily; " +
			"every working breakpoint would be drawn as one the adapter refused")
	}

	s := startJsDebugSession(t, ctx, func(lines []string) []SourceBreakpoint {
		return []SourceBreakpoint{{Line: lineWithMarker(t, lines, jsBreakpointMarker)}}
	})
	if len(s.answers) != 1 {
		t.Fatalf("got %d answers for 1 breakpoint: %+v", len(s.answers), s.answers)
	}
	answered := s.answers
	t.Logf("js-debug answered the breakpoint %+v", answered[0])

	// THE point: whatever it answered, it stops.
	stopped := s.awaitStopped(t, "the provisionally-bound breakpoint")
	s.assertStoppedOn(t, ctx, stopped, jsBreakpointMarker)

	if answered[0].Verified {
		t.Log("NOTE: js-debug now reports verified:true. That is strictly better, and " +
			"Adapter.BreakpointsBindLazily is no longer load-bearing for this adapter — but " +
			"it is still correct, since a verified answer is never turned into a pending one.")
		return
	}
	t.Logf("CONFIRMED: js-debug answered verified=false message=%q and stopped on the "+
		"breakpoint anyway. Without Adapter.BreakpointsBindLazily the editor would draw this "+
		"working breakpoint hollow and announce it could not be set.", answered[0].Message)
}

// TestLiveJsDebugResendKeepsThemBound pins what a MID-SESSION breakpoint edit
// does to js-debug, because the editor performs one every time F9 is pressed
// while a session is live.
//
// 🔴 internal/app's resendBreakpoints re-sends a file's COMPLETE set on every
// breakpoint edit, which is mandatory: setBreakpoints has no incremental form,
// so sending only the new one clears the rest. js-debug answers that re-send
// with an EMPTY ARRAY — no entry for the breakpoint that is still armed — and
// boundFromAnswers pairs answers POSITIONALLY, so the editor records zero
// information about a breakpoint that is working fine.
//
// That makes the answer untrustworthy as a description of state, which is why
// this test judges by BEHAVIOUR: continue, and see whether the program stops
// again. It reports whichever it observes rather than asserting one, because a
// user editing a condition mid-session on a Node program needs to know whether
// the rest of that file's breakpoints quietly stopped firing — and a silent
// version of either answer would leave that undocumented.
func TestLiveJsDebugResendKeepsThemBound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), jsDebugTimeout)
	defer cancel()

	s := startJsDebugSession(t, ctx, func(lines []string) []SourceBreakpoint {
		return []SourceBreakpoint{{Line: lineWithMarker(t, lines, jsLoopMarker)}}
	})

	first := s.awaitStopped(t, "the first loop iteration")
	s.assertStoppedOn(t, ctx, first, jsLoopMarker)

	// Exactly what resendBreakpoints sends: the same whole-file set again.
	line := lineWithMarker(t, s.lines, jsLoopMarker)
	answers, err := s.leaf.SetBreakpoints(ctx, Source{Path: s.file, Name: filepath.Base(s.file)},
		[]SourceBreakpoint{{Line: line}})
	if err != nil {
		t.Fatalf("re-sending the file's breakpoints: %v", err)
	}
	t.Logf("re-sent 1 breakpoint on a live session, got %d answer(s): %+v", len(answers), answers)

	if err := s.leaf.Continue(ctx, first.ThreadID); err != nil {
		t.Fatalf("continue: %v", err)
	}

	// The loop runs ten times, so a still-bound breakpoint stops again almost at
	// once. Waiting for the program to END is the negative case.
	select {
	case again := <-s.waiter.stopped:
		s.assertStoppedOn(t, ctx, again, jsLoopMarker)
		t.Logf("MEASURED: a mid-session re-send KEPT the breakpoint bound (stopped again, "+
			"reason %q) even though the re-send answered with %d entries. resendBreakpoints is "+
			"safe on this adapter; its ANSWER is not a description of state.", again.Reason, len(answers))
		return
	case <-time.After(15 * time.Second):
	}

	if !s.awaitFinished(t) {
		t.Fatalf("after the re-send the program neither stopped again nor finished; events: %v",
			s.waiter.col.names())
	}
	t.Fatalf("🔴 MEASURED CHANGE: re-sending a file's breakpoints to a LIVE js-debug session "+
		"UNBOUND them — the answer was %+v and the program then ran to completion past a "+
		"breakpoint that had just stopped it. When this test was written the re-send kept them "+
		"bound, so editing a breakpoint mid-session (F9, or a condition) now silently stops the "+
		"rest of that file's breakpoints firing for the remainder of the run on Node. Record it "+
		"in CLAUDE.md and decide whether resendBreakpoints should refuse on this adapter.", answers)
}

// TestLiveJsDebugConditionalBreakpointFiresOnce is the conditional-breakpoint
// oracle on the third adapter: a ten-iteration loop with `i === 3` stops exactly
// once, with i worth 3.
//
// Same shape and reasoning as the delve and debugpy versions. Running it a third
// time against a third independent implementation is what keeps "our client
// sends `condition`" from being mistaken for "conditional breakpoints work".
func TestLiveJsDebugConditionalBreakpointFiresOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), jsDebugTimeout)
	defer cancel()

	s := startJsDebugSession(t, ctx, func(lines []string) []SourceBreakpoint {
		return []SourceBreakpoint{{Line: lineWithMarker(t, lines, jsLoopMarker), Condition: jsLoopCondition}}
	})
	if !s.caps.SupportsConditionalBreakpoints {
		t.Fatal("js-debug did not advertise supportsConditionalBreakpoints; the gate would refuse this")
	}

	stopped := s.awaitStopped(t, "the conditional breakpoint")
	top := s.assertStoppedOn(t, ctx, stopped, jsLoopMarker)

	res, err := s.leaf.Evaluate(ctx, jsLoopVarName, top.ID, EvalContextWatch)
	if err != nil {
		t.Fatalf("evaluate(%q): %v", jsLoopVarName, err)
	}
	if res.Result != jsLoopValue {
		t.Fatalf("stopped with %s = %q, want %q — the condition %q was not honoured "+
			"(a dropped condition stops on the first iteration, where %s is 0)",
			jsLoopVarName, res.Result, jsLoopValue, jsLoopCondition, jsLoopVarName)
	}

	if err := s.leaf.Continue(ctx, stopped.ThreadID); err != nil {
		t.Fatalf("continue: %v", err)
	}
	if !s.awaitFinished(t) {
		t.Fatalf("after continue the program never finished; events: %v", s.waiter.col.names())
	}
	if got := s.countStoppedEvents(); got != 1 {
		t.Fatalf("the program stopped %d times on a loop of 10 with condition %q; want exactly 1. events: %v",
			got, jsLoopCondition, s.waiter.col.names())
	}
	t.Logf("condition %q on js-debug: 1 stop out of 10 iterations, %s == %s",
		jsLoopCondition, jsLoopVarName, res.Result)
}

// TestLiveJsDebugProgramOutputArrivesAsEvents proves the console decision on the
// third adapter, and proves it on the CHILD connection.
//
// 🔴 The program's stdout comes back on the leaf, not on the coordinator that
// launched it. An implementation that kept listening to the root for output
// would show the user js-debug's telemetry and none of their own console.log —
// which is the shape of "the debug console is empty and nothing explains it".
func TestLiveJsDebugProgramOutputArrivesAsEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), jsDebugTimeout)
	defer cancel()

	// No breakpoints: let it run straight through and print.
	s := startJsDebugSession(t, ctx, func([]string) []SourceBreakpoint { return nil })

	want := jsStdoutSentinel + " 5 45" // add(2,3) and countTo(10)
	deadline := time.Now().Add(jsDebugTimeout)
	for time.Now().Before(deadline) {
		s.waiter.col.mu.Lock()
		for _, e := range s.waiter.col.events {
			if e.Event != EventOutput {
				continue
			}
			var oe OutputEvent
			if err := unmarshalBody(e.Body, &oe); err != nil {
				continue
			}
			if strings.Contains(oe.Output, want) {
				category := oe.Category
				s.waiter.col.mu.Unlock()
				t.Logf("program stdout arrived as an output event on the CHILD: category=%q output=%q",
					category, strings.TrimSpace(oe.Output))
				if category == OutputCategoryTelemetry {
					t.Errorf("the program's own output was categorised as telemetry, which the "+
						"console filters out: %q", oe.Output)
				}
				if s.countStoppedEvents() != 0 {
					t.Errorf("the program stopped %d time(s) with no breakpoints set",
						s.countStoppedEvents())
				}
				return
			}
		}
		s.waiter.col.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the program's stdout (%q) never arrived as an output event on the child; "+
		"child events seen: %v\nadapter stderr: %v", want, s.waiter.col.names(), s.root.LastStderr())
}

// TestLiveJsDebugReportsNoTerminateSupport is the measured half of
// TestJsDebugFallsBackToDisconnectWhenTerminateUnsupported.
//
// 🔴 That test proves the client falls back correctly when an adapter says it
// cannot terminate. This one proves js-debug is such an adapter, which is the
// half a fake cannot establish: if js-debug started advertising the capability,
// the fallback would still be right and the fake would still be green while the
// reason for the fallback had quietly evaporated. Same shape as delve's absent
// supportsTerminateRequest, measured rather than assumed.
func TestLiveJsDebugReportsNoTerminateSupport(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), jsDebugTimeout)
	defer cancel()

	root, _, _ := jsDebugFixture(t)
	requireJsDebug(t, root)

	adapter, _ := AdapterFor("javascript")
	var col collector
	reg := NewRegistry(root)
	client, err := reg.Start(ctx, adapter, col.handlers())
	if err != nil {
		t.Fatalf("starting js-debug: %v", err)
	}
	defer client.Stop()

	caps, err := client.Initialize(ctx, adapter.AdapterID)
	if err != nil {
		t.Fatalf("initialize: %v (adapter stderr: %v)", err, client.LastStderr())
	}
	if caps.SupportsTerminateRequest {
		t.Fatalf("js-debug now advertises supportsTerminateRequest. The fallback in " +
			"TerminateOrDisconnect is still correct, but the REASON recorded for it in " +
			"registry.go and CLAUDE.md no longer describes this adapter — update the claim.")
	}
	if !caps.SupportsConfigurationDoneRequest {
		t.Error("js-debug did not advertise supportsConfigurationDoneRequest; the root would " +
			"launch without waiting and startDebugging would race the breakpoint push")
	}
	t.Logf("js-debug: supportsTerminateRequest=%v -> disconnect{terminateDebuggee:true} is the "+
		"only route that stops the debuggee", caps.SupportsTerminateRequest)
}
