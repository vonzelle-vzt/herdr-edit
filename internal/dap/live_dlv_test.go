// =============================================================================
// File: internal/dap/live_dlv_test.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// live_dlv_test.go drives a REAL delve through a REAL debug session.
//
// 🔴 WHY THIS FILE HAD TO EXIST — the same reason live_gopls_test.go does.
// Every other test in this package feeds hand-written JSON to a decoder, or
// drives a fake adapter whose behaviour I chose. That proves the decoder
// handles the shape I handed it; it proves nothing about whether delve sends
// that shape, whether our requests are spelled the way it expects, or whether
// the handshake ordering works end to end. The whole definition of done for
// this stage — press F5, the program stops on your breakpoint — is a claim
// only a live adapter can substantiate.
//
// 🔴 THE ANTI-SKIP GATE. This skips when dlv is absent so a fresh clone stays
// green, and setting HERDR_REQUIRE_DAP=1 turns every skip into a failure. CI
// installs dlv and sets that variable. Without the gate a skipped test is
// indistinguishable from a passing one in the summary, which is exactly how
// the gopls oracle sat un-executed for a very long time.
//
//	go install github.com/go-delve/delve/cmd/dlv@latest
package dap

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// liveTimeout bounds the whole session. Generous because delve BUILDS the
// fixture module first: measured at 7.4s cold and 0.3s warm on a laptop, and a
// loaded CI runner is slower than both.
const liveTimeout = 180 * time.Second

// breakpointMarker tags the fixture line the debugger must stop on.
//
// 🔴 The oracle looks this marker up in the source at runtime instead of
// hardcoding a line number, and that is the point. Declaring linesStartAt1 and
// then comparing the adapter's answer against a constant proves nothing about
// the conversion: a fixture whose indices already agree with the buffer's
// passes whether the boundary is right or off by one. Asserting that the
// stopped line's TEXT is the marked line cannot pass by coincidence.
const breakpointMarker = "BREAKPOINT-TARGET"

// commentMarker tags a NON-executable line, used to prove the other half of the
// breakpoint contract: a breakpoint that cannot bind comes back unverified.
const commentMarker = "NOT-EXECUTABLE"

// stepTargetMarker tags the line a single `next` from breakpointMarker must
// land on — the statement immediately after it.
//
// 🔴 Same reasoning as breakpointMarker, and it is the whole reason a marker
// exists rather than an arithmetic assertion. "The stopped line is one greater
// than it was" passes for a step that went nowhere and then reported a line
// number off by one, and it passes for a fixture whose numbering happens to
// agree with the adapter's. Asserting that the TEXT of the new line is the line
// tagged as the step target cannot pass by coincidence.
const stepTargetMarker = "STEP-TARGET"

// localName and localValue are the local variable the variables oracle reads,
// and what it must be worth by the time execution reaches stepTargetMarker.
//
// The breakpoint for that oracle sits on the step-target line ON PURPOSE: a
// breakpoint on the assignment stops BEFORE it runs, so sum would be 0 there —
// a value that is also what a failed read, a wrong scope and an uninitialised
// struct all produce. 5 can only be the real answer.
const (
	localName  = "sum"
	localValue = "5"
)

// loopBodyMarker tags the body of a REAL loop — the line a conditional
// breakpoint must stop on exactly once out of ten iterations.
//
// 🔴 A loop is the only fixture shape that can tell a working condition from a
// dropped one. On a line that executes once, a breakpoint with a condition the
// adapter ignored and a breakpoint with a condition it honoured produce
// identical, passing behaviour — one stop, in the right place. The claim
// "conditions work" is only substantiated by a line the program reaches ten
// times and stops on once.
const loopBodyMarker = "LOOP-TARGET"

// loopCondition stops the loop on exactly one iteration, and loopConditionValue
// is what the loop variable must be worth when it does.
//
// The value is asserted through `evaluate` rather than inferred from the stop:
// an adapter that stopped on the first iteration for its own reasons would still
// have stopped exactly once, so "it stopped once" alone does not distinguish a
// condition that worked from a breakpoint that only ever fires once anyway.
const (
	loopVarName        = "i"
	loopCondition      = "i == 3"
	loopConditionValue = "3"
)

// stdoutSentinel is what the fixture PRINTS, and it is deliberately a string
// that cannot occur anywhere else in the session.
//
// 🔴 This constant exists because the first version of the output-event oracle
// was a false pass. It looked for a "5" in any stdout-category output event —
// and matched delve's own `Building /var/folders/…3039171955/001` progress
// message, which contains several. That message is emitted BEFORE the program
// is even built, so the test passed without the debuggee ever running. A
// sentinel only the fixture can produce is what makes the assertion mean what
// it claims.
const stdoutSentinel = "DAP-FIXTURE-PRINTED-THIS"

// requireDlv finds delve or ends the test. Under HERDR_REQUIRE_DAP=1 an absent
// dlv is a failure rather than a skip.
func requireDlv(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("dlv")
	if err != nil {
		skipOrFatal(t, "dlv is not installed — install it with "+
			"`go install github.com/go-delve/delve/cmd/dlv@latest` and make sure "+
			"$(go env GOPATH)/bin is on PATH")
	}
	return path
}

// skipOrFatal skips normally, but fails when HERDR_REQUIRE_DAP=1 is set. This
// is the whole anti-skip gate: CI sets the variable, so a live oracle that
// quietly stopped running turns the build red instead of staying green.
func skipOrFatal(t *testing.T, msg string) {
	t.Helper()
	if os.Getenv("HERDR_REQUIRE_DAP") == "1" {
		t.Fatalf("HERDR_REQUIRE_DAP=1 but the live DAP oracle could not run: %s", msg)
	}
	t.Skip(msg)
}

// dlvFixture writes a tiny real module and returns its root, main file, and the
// file's lines. A real debugger needs a real module: `dlv dap` in debug mode
// compiles the package before it can run anything.
func dlvFixture(t *testing.T) (root, file string, lines []string) {
	t.Helper()
	root = t.TempDir()

	write := func(name, body string) string {
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("go.mod", "module dapfixture\n\ngo 1.21\n")
	src := `package main

import "fmt"

// add returns the sum of a and b. ` + commentMarker + `
func add(a, b int) int {
	sum := a + b // ` + breakpointMarker + `
	return sum // ` + stepTargetMarker + `
}

// countTo sums 0..n-1 in a real loop, so a conditional breakpoint has ten
// chances to fire and must take exactly one of them.
func countTo(n int) int {
	total := 0
	for i := 0; i < n; i++ {
		total += i // ` + loopBodyMarker + `
	}
	return total
}

func main() {
	fmt.Println("` + stdoutSentinel + `", add(2, 3), countTo(10))
}
`
	file = write("main.go", src)
	return root, file, strings.Split(src, "\n")
}

// lineWithMarker returns the ONE-based line number carrying marker, which is
// the coordinate system the adapter speaks. It fails rather than returning zero
// when the marker is missing, so a fixture edit that loses it cannot quietly
// turn the oracle into a no-op.
func lineWithMarker(t *testing.T, lines []string, marker string) int {
	t.Helper()
	found := 0
	for i, l := range lines {
		if strings.Contains(l, marker) {
			if found != 0 {
				t.Fatalf("marker %q appears on both line %d and line %d; it must be unique", marker, found, i+1)
			}
			found = i + 1 // adapter coordinates are 1-based
		}
	}
	if found == 0 {
		t.Fatalf("marker %q is not in the fixture at all — the oracle would assert nothing", marker)
	}
	return found
}

// stoppedWaiter collects events and hands back the stopped one's decoded body,
// which WaitEvent alone cannot provide.
type stoppedWaiter struct {
	col     *collector
	stopped chan StoppedEvent
}

// newStoppedWaiter wires Handlers that both record everything and surface the
// first stopped event.
func newStoppedWaiter(t *testing.T) (*stoppedWaiter, Handlers) {
	t.Helper()
	w := &stoppedWaiter{col: &collector{}, stopped: make(chan StoppedEvent, 4)}
	base := w.col.handlers()
	return w, Handlers{
		OnEvent: func(e Event) {
			base.OnEvent(e)
			if e.Event == EventStopped {
				var se StoppedEvent
				if err := unmarshalBody(e.Body, &se); err == nil {
					select {
					case w.stopped <- se:
					default:
					}
				}
			}
		},
		OnLog: func(s string) {
			base.OnLog(s)
			t.Logf("adapter log: %s", s)
		},
	}
}

// unmarshalBody decodes an event body, tolerating an absent one.
func unmarshalBody(body []byte, v interface{}) error {
	if len(body) == 0 {
		return nil
	}
	return jsonUnmarshal(body, v)
}

// TestLiveDelveStopsAtBreakpoint is the definition of done for Lane B stage 2,
// expressed as an oracle: a real program, launched under a real debugger, stops
// on the line we marked.
//
// It walks the exact ordering a real session must follow —
//
//	initialize → launch SENT → `initialized` → setBreakpoints →
//	setExceptionBreakpoints → configurationDone → stopped
//
// — because every one of the traps in this package lives somewhere in that
// sequence, and any of them fails as a hang rather than as an error.
func TestLiveDelveStopsAtBreakpoint(t *testing.T) {
	dlv := requireDlv(t)
	t.Logf("using %s", dlv)

	root, file, lines := dlvFixture(t)
	targetLine := lineWithMarker(t, lines, breakpointMarker)
	commentLine := lineWithMarker(t, lines, commentMarker)
	t.Logf("fixture: breakpoint target on line %d, non-executable line %d", targetLine, commentLine)

	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()

	waiter, handlers := newStoppedWaiter(t)

	adapter, ok := AdapterFor("go")
	if !ok {
		t.Fatal("no adapter registered for go")
	}
	reg := NewRegistry(root)
	client, err := reg.Start(ctx, adapter, handlers)
	if err != nil {
		t.Fatalf("starting delve: %v", err)
	}
	defer client.Stop()

	// --- initialize -------------------------------------------------------
	caps, err := client.Initialize(ctx, adapter.AdapterID)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if !caps.SupportsConfigurationDoneRequest {
		t.Fatal("delve did not advertise supportsConfigurationDoneRequest; the session would hang without it")
	}

	// --- launch (SENT, not awaited) ---------------------------------------
	// 🔴 Blocking on the response here is the deadlock trap 4 describes.
	cfg := map[string]interface{}{}
	for k, v := range adapter.Launch {
		cfg[k] = v
	}
	cfg["program"] = root
	cfg["output"] = filepath.Join(root, "debug_bin")
	launchResp, err := client.Launch(cfg)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}

	// --- the initialized event -------------------------------------------
	if err := client.WaitEvent(ctx, EventInitialized); err != nil {
		t.Fatalf("never saw the initialized event: %v\nadapter stderr: %v", err, client.LastStderr())
	}

	// --- breakpoints ------------------------------------------------------
	// Both are sent in ONE call, because setBreakpoints is whole-file, and the
	// answer comes back positionally matched to this list.
	bps, err := client.SetBreakpoints(ctx, Source{Path: file}, []SourceBreakpoint{
		{Line: targetLine},
		{Line: commentLine},
	})
	if err != nil {
		t.Fatalf("setBreakpoints: %v", err)
	}
	if len(bps) != 2 {
		t.Fatalf("got %d breakpoint answers for 2 requests; the response is positional", len(bps))
	}

	if !bps[0].Verified {
		t.Fatalf("delve refused to bind a breakpoint on an executable line %d: %+v", targetLine, bps[0])
	}
	if !bps[0].HasLine() {
		t.Fatalf("verified breakpoint carries no line: %+v", bps[0])
	}
	t.Logf("breakpoint bound: requested line %d, delve bound line %d", targetLine, bps[0].Line)

	// The other half of the contract: a non-executable line comes back
	// unverified, with no line at all — which is why HasLine exists.
	if bps[1].Verified {
		t.Errorf("delve claimed to bind a breakpoint on a comment line: %+v", bps[1])
	}
	if bps[1].HasLine() {
		t.Errorf("an unverified breakpoint reported a usable line: %+v", bps[1])
	}
	if bps[1].Message == "" {
		t.Errorf("an unbindable breakpoint came back with no explanation: %+v", bps[1])
	}

	if err := client.SetExceptionBreakpoints(ctx, caps.DefaultFilters()); err != nil {
		t.Fatalf("setExceptionBreakpoints: %v", err)
	}
	if err := client.ConfigurationDone(ctx); err != nil {
		t.Fatalf("configurationDone: %v", err)
	}

	// --- the program actually stops ---------------------------------------
	var stopped StoppedEvent
	select {
	case stopped = <-waiter.stopped:
	case <-time.After(liveTimeout):
		t.Fatalf("the program never stopped. events seen: %v\nadapter stderr: %v",
			waiter.col.names(), client.LastStderr())
	}
	if stopped.Reason != "breakpoint" {
		t.Fatalf("stopped for reason %q, want breakpoint: %+v", stopped.Reason, stopped)
	}

	// --- where did it stop ------------------------------------------------
	frames, err := client.StackTrace(ctx, stopped.ThreadID, 20)
	if err != nil {
		t.Fatalf("stackTrace: %v", err)
	}
	if len(frames) == 0 {
		t.Fatal("stackTrace returned no frames for a stopped thread")
	}
	top := frames[0]

	// 🔴 The assertion that cannot pass by coincidence: the TEXT of the line
	// delve stopped on must be the line we marked. Comparing top.Line against
	// targetLine would pass even with the 1-based/0-based boundary inverted,
	// because the fixture's numbers would agree with themselves.
	if top.Line < 1 || top.Line > len(lines) {
		t.Fatalf("stopped on line %d, outside the fixture's %d lines", top.Line, len(lines))
	}
	stoppedText := lines[top.Line-1]
	if !strings.Contains(stoppedText, breakpointMarker) {
		t.Fatalf("stopped on line %d whose text is %q — that is not the marked line.\n"+
			"the marked line is %d: %q", top.Line, stoppedText, targetLine, lines[targetLine-1])
	}
	if filepath.Clean(top.Source.Path) != filepath.Clean(file) {
		t.Errorf("stopped in %q, want %q", top.Source.Path, file)
	}
	if top.Name == "" {
		t.Error("the top frame has no function name")
	}
	t.Logf("stopped in %s at %s:%d — %q", top.Name, filepath.Base(top.Source.Path), top.Line, strings.TrimSpace(stoppedText))

	// threads must answer while stopped; the app uses it as a fallback when a
	// stopped event carries no thread id.
	threads, err := client.Threads(ctx)
	if err != nil {
		t.Errorf("threads: %v", err)
	} else if len(threads) == 0 {
		t.Error("threads returned nothing for a stopped program")
	}

	// --- shut down --------------------------------------------------------
	if err := client.Disconnect(ctx, true); err != nil {
		t.Errorf("disconnect: %v", err)
	}
	select {
	case resp := <-launchResp:
		if !resp.Success {
			t.Errorf("the launch response eventually reported failure: %+v", resp)
		}
	case <-time.After(10 * time.Second):
		t.Error("the launch response never arrived at all")
	}
}

// TestLiveDelveContinueRunsToCompletion covers the other half of F5: once
// stopped, continuing must actually resume the program and run it to the end.
// A session that stops but cannot resume is a debugger the user has to kill.
func TestLiveDelveContinueRunsToCompletion(t *testing.T) {
	requireDlv(t)

	root, file, lines := dlvFixture(t)
	targetLine := lineWithMarker(t, lines, breakpointMarker)

	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()

	waiter, handlers := newStoppedWaiter(t)
	adapter, _ := AdapterFor("go")
	reg := NewRegistry(root)
	client, err := reg.Start(ctx, adapter, handlers)
	if err != nil {
		t.Fatalf("starting delve: %v", err)
	}
	defer client.Stop()

	caps, err := client.Initialize(ctx, adapter.AdapterID)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	cfg := map[string]interface{}{}
	for k, v := range adapter.Launch {
		cfg[k] = v
	}
	cfg["program"] = root
	cfg["output"] = filepath.Join(root, "debug_bin")
	if _, err := client.Launch(cfg); err != nil {
		t.Fatalf("launch: %v", err)
	}
	if err := client.WaitEvent(ctx, EventInitialized); err != nil {
		t.Fatalf("initialized: %v", err)
	}
	if _, err := client.SetBreakpoints(ctx, Source{Path: file}, []SourceBreakpoint{{Line: targetLine}}); err != nil {
		t.Fatalf("setBreakpoints: %v", err)
	}
	if err := client.SetExceptionBreakpoints(ctx, caps.DefaultFilters()); err != nil {
		t.Fatalf("setExceptionBreakpoints: %v", err)
	}
	if err := client.ConfigurationDone(ctx); err != nil {
		t.Fatalf("configurationDone: %v", err)
	}

	var stopped StoppedEvent
	select {
	case stopped = <-waiter.stopped:
	case <-time.After(liveTimeout):
		t.Fatalf("never stopped; events: %v", waiter.col.names())
	}

	if err := client.Continue(ctx, stopped.ThreadID); err != nil {
		t.Fatalf("continue: %v", err)
	}

	// The program prints and exits, so the adapter terminates. Either
	// terminated or exited is a correct answer; the app handles both.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		for _, n := range waiter.col.names() {
			if n == EventTerminated || n == EventExited {
				t.Logf("program ran to completion; events: %v", waiter.col.names())
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("after continue the program never terminated; events: %v", waiter.col.names())
}

// TestLiveDelveOutputEventCarriesProgramStdout proves trap 8's premise: the
// debugged program's stdout comes back as an `output` EVENT rather than being
// written to our terminal. If it reached our real stdout instead, it would
// scribble over the tcell screen with nothing to repair it.
func TestLiveDelveOutputEventCarriesProgramStdout(t *testing.T) {
	requireDlv(t)

	root, _, _ := dlvFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()

	waiter, handlers := newStoppedWaiter(t)
	adapter, _ := AdapterFor("go")
	reg := NewRegistry(root)
	client, err := reg.Start(ctx, adapter, handlers)
	if err != nil {
		t.Fatalf("starting delve: %v", err)
	}
	defer client.Stop()

	caps, err := client.Initialize(ctx, adapter.AdapterID)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	cfg := map[string]interface{}{}
	for k, v := range adapter.Launch {
		cfg[k] = v
	}
	cfg["program"] = root
	cfg["output"] = filepath.Join(root, "debug_bin")
	if _, err := client.Launch(cfg); err != nil {
		t.Fatalf("launch: %v", err)
	}
	if err := client.WaitEvent(ctx, EventInitialized); err != nil {
		t.Fatalf("initialized: %v", err)
	}
	// No breakpoints at all: let it run straight through and print.
	if _, err := client.SetBreakpoints(ctx, Source{Path: filepath.Join(root, "main.go")}, nil); err != nil {
		t.Fatalf("setBreakpoints: %v", err)
	}
	if err := client.SetExceptionBreakpoints(ctx, caps.DefaultFilters()); err != nil {
		t.Fatalf("setExceptionBreakpoints: %v", err)
	}
	if err := client.ConfigurationDone(ctx); err != nil {
		t.Fatalf("configurationDone: %v", err)
	}

	// The fixture prints "<sentinel> 5". Matching on the sentinel — and on the
	// computed value with it — is what keeps this from matching delve's own
	// build chatter, which was the original false pass.
	want := stdoutSentinel + " 5"
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		waiter.col.mu.Lock()
		for _, e := range waiter.col.events {
			if e.Event != EventOutput {
				continue
			}
			var oe OutputEvent
			if err := unmarshalBody(e.Body, &oe); err != nil {
				continue
			}
			if strings.Contains(oe.Output, want) {
				category := oe.Category
				waiter.col.mu.Unlock()
				if category != "stdout" {
					t.Errorf("the program's output arrived under category %q, want stdout", category)
				}
				t.Logf("program stdout arrived as an output event: category=%q output=%q",
					category, strings.TrimSpace(oe.Output))
				return
			}
		}
		waiter.col.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the program's stdout (%q) never arrived as an output event; events seen: %v",
		want, waiter.col.names())
}

// jsonUnmarshal is a tiny indirection so unmarshalBody reads cleanly; the
// import lives here rather than in the helper's signature.
func jsonUnmarshal(data []byte, v interface{}) error { return json.Unmarshal(data, v) }

// liveSession is a real delve, launched against the fixture and stopped on the
// requested marker, with everything the stage-3 oracles need to interrogate it.
type liveSession struct {
	client *Client
	waiter *stoppedWaiter
	caps   Capabilities
	file   string
	lines  []string

	// last is the most recent stopped event awaitStopped saw. Kept here rather
	// than returned and threaded around, because the thread id it carries is
	// needed by every request that follows a stop and losing track of it is how
	// a request ends up aimed at goroutine 0.
	last StoppedEvent
}

// startStoppedSession brings up a real delve, sets ONE breakpoint on the line
// carrying marker, and returns once the program has stopped on it.
//
// It exists because the launch sequence has five ordered steps and three of them
// fail as a hang rather than an error; the two stage-3 oracles below care about
// what happens AFTER the stop, and a fourth and fifth transcription of that
// sequence would be four and five chances to get it subtly wrong. The three
// stage-2 oracles above keep their own copies deliberately — they are testing
// the sequence itself, so sharing a helper with them would mean the thing under
// test and the test harness were the same code.
func startStoppedSession(t *testing.T, ctx context.Context, marker string) *liveSession {
	t.Helper()
	s := startConfiguredSession(t, ctx, func(file string, lines []string) []SourceBreakpoint {
		return []SourceBreakpoint{{Line: lineWithMarker(t, lines, marker)}}
	})
	stopped := s.awaitStopped(t, "the initial breakpoint")
	if stopped.Reason != "breakpoint" {
		t.Fatalf("first stop had reason %q, want breakpoint: %+v", stopped.Reason, stopped)
	}
	s.assertStoppedOn(t, ctx, stopped, marker)
	return s
}

// startConfiguredSession runs the five-step launch sequence against a real
// delve with whatever breakpoints bps builds, and returns once configurationDone
// has been acknowledged — i.e. the moment the program is allowed to run.
//
// It does NOT wait for a stop, because not every oracle wants one immediately:
// the conditional-breakpoint oracle has to be able to observe how MANY stops
// there are, which a helper that consumed the first would make impossible.
func startConfiguredSession(t *testing.T, ctx context.Context,
	bps func(file string, lines []string) []SourceBreakpoint) *liveSession {
	t.Helper()

	root, file, lines := dlvFixture(t)

	waiter, handlers := newStoppedWaiter(t)
	adapter, ok := AdapterFor("go")
	if !ok {
		t.Fatal("no adapter registered for go")
	}
	reg := NewRegistry(root)
	client, err := reg.Start(ctx, adapter, handlers)
	if err != nil {
		t.Fatalf("starting delve: %v", err)
	}
	t.Cleanup(client.Stop)

	caps, err := client.Initialize(ctx, adapter.AdapterID)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	// Measured facts, recorded in the run output rather than asserted: delve
	// 1.27 answers supportsTerminateRequest ABSENT (so TerminateOrDisconnect
	// must fall back to disconnect{terminateDebuggee:true}) and answers
	// supportsVariablePaging ABSENT too (so start/count are never sent and
	// maxDebugVariables is the only cap there is). Both are read by production
	// code, so seeing what a real adapter actually says is the point of this file.
	t.Logf("delve capabilities: terminate=%v variablePaging=%v variableType(request)=%v configurationDone=%v",
		caps.SupportsTerminateRequest, caps.SupportsVariablePaging,
		initializeArgsForClient(adapter.AdapterID).SupportsVariableType,
		caps.SupportsConfigurationDoneRequest)

	cfg := map[string]interface{}{}
	for k, v := range adapter.Launch {
		cfg[k] = v
	}
	cfg["program"] = root
	cfg["output"] = filepath.Join(root, "debug_bin")
	if _, err := client.Launch(cfg); err != nil {
		t.Fatalf("launch: %v", err)
	}
	if err := client.WaitEvent(ctx, EventInitialized); err != nil {
		t.Fatalf("never saw the initialized event: %v (adapter stderr: %v)", err, client.LastStderr())
	}
	want := bps(file, lines)
	answers, err := client.SetBreakpoints(ctx, Source{Path: file}, want)
	if err != nil {
		t.Fatalf("setBreakpoints: %v", err)
	}
	if len(answers) != len(want) {
		t.Fatalf("got %d breakpoint answers for %d requests; the response is positional",
			len(answers), len(want))
	}
	for i, ans := range answers {
		if !ans.Verified {
			t.Fatalf("delve would not bind breakpoint %+v: %+v", want[i], ans)
		}
	}
	if err := client.SetExceptionBreakpoints(ctx, caps.DefaultFilters()); err != nil {
		t.Fatalf("setExceptionBreakpoints: %v", err)
	}
	if err := client.ConfigurationDone(ctx); err != nil {
		t.Fatalf("configurationDone: %v", err)
	}
	return &liveSession{client: client, waiter: waiter, caps: caps, file: file, lines: lines}
}

// awaitStopped blocks for the next stopped event, failing with what the adapter
// did say when none arrives.
func (s *liveSession) awaitStopped(t *testing.T, what string) StoppedEvent {
	t.Helper()
	select {
	case ev := <-s.waiter.stopped:
		s.last = ev
		return ev
	case <-time.After(liveTimeout):
		t.Fatalf("no stopped event for %s. events seen: %v\nadapter stderr: %v",
			what, s.waiter.col.names(), s.client.LastStderr())
	}
	return StoppedEvent{}
}

// topFrame reads the innermost frame of a stopped thread.
func (s *liveSession) topFrame(t *testing.T, ctx context.Context, ev StoppedEvent) StackFrame {
	t.Helper()
	frames, err := s.client.StackTrace(ctx, ev.ThreadID, 20)
	if err != nil {
		t.Fatalf("stackTrace: %v", err)
	}
	if len(frames) == 0 {
		t.Fatal("stackTrace returned no frames for a stopped thread")
	}
	return frames[0]
}

// assertStoppedOn checks execution is halted on the line carrying marker,
// identified by that line's TEXT.
//
// 🔴 By text, never by number. The whole 1-based/0-based boundary this package
// declares in initialize is invisible to an assertion that compares the
// adapter's line against a number computed from the same fixture — both sides
// would be wrong together. Reading the source line the adapter names and
// checking the marker is IN it is an assertion that cannot agree with a bug.
func (s *liveSession) assertStoppedOn(t *testing.T, ctx context.Context, ev StoppedEvent, marker string) StackFrame {
	t.Helper()
	top := s.topFrame(t, ctx, ev)
	if top.Line < 1 || top.Line > len(s.lines) {
		t.Fatalf("stopped on line %d, outside the fixture's %d lines", top.Line, len(s.lines))
	}
	text := s.lines[top.Line-1]
	if !strings.Contains(text, marker) {
		t.Fatalf("stopped on line %d whose text is %q — that is not the line marked %q.\n"+
			"the marked line is %d: %q",
			top.Line, strings.TrimSpace(text), marker,
			lineWithMarker(t, s.lines, marker), strings.TrimSpace(s.lines[lineWithMarker(t, s.lines, marker)-1]))
	}
	return top
}

// TestLiveDelveStepOverAdvancesOneLine is the definition of done for stage 3's
// stepping half: a real program, stopped on a real breakpoint, moves to the
// NEXT EXECUTABLE STATEMENT when `next` is sent — and reports it as a `stopped`
// event rather than in the step response.
//
// 🔴 The new location is identified by the TEXT of the line, not by its number.
// A fixture whose numbers already agree with the adapter's proves nothing about
// the conversion at either end, and "line+1" is satisfied by a step that never
// happened next to an off-by-one. See assertStoppedOn.
func TestLiveDelveStepOverAdvancesOneLine(t *testing.T) {
	dlv := requireDlv(t)
	t.Logf("using %s", dlv)

	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()

	s := startStoppedSession(t, ctx, breakpointMarker)

	// The two markers must genuinely be different lines, or the oracle proves
	// nothing about advancing at all.
	from := lineWithMarker(t, s.lines, breakpointMarker)
	to := lineWithMarker(t, s.lines, stepTargetMarker)
	if from == to {
		t.Fatalf("the breakpoint and step markers are both on line %d; a step could not be observed", from)
	}

	// 🔴 next() returns as soon as delve ACCEPTS the request. Reading the stack
	// here would report where we already were.
	if err := s.client.Next(ctx, s.last.ThreadID); err != nil {
		t.Fatalf("next: %v", err)
	}

	stopped := s.awaitStopped(t, "the step")
	if stopped.Reason != "step" {
		t.Errorf("stopped after next with reason %q, want %q", stopped.Reason, "step")
	}
	top := s.assertStoppedOn(t, ctx, stopped, stepTargetMarker)
	t.Logf("next: %q -> %q (delve lines %d -> %d, frame %s)",
		strings.TrimSpace(s.lines[from-1]), strings.TrimSpace(s.lines[to-1]), from, top.Line, top.Name)

	if filepath.Clean(top.Source.Path) != filepath.Clean(s.file) {
		t.Errorf("stepped into %q, want to stay in %q", top.Source.Path, s.file)
	}
}

// TestLiveDelveVariablesReadsALocal is the oracle that proves the whole
// inspection CHAIN rather than its transport: stackTrace gives a frame id,
// scopes turns that into a variables reference, and variables turns that into a
// value the program actually computed.
//
// 🔴 Every link here is one where a wrong id decodes cleanly into the right
// SHAPE. Passing a thread id where a frame id belongs, or a frame id where a
// variablesReference belongs, produces variables — just somebody else's. Only
// asserting a value the program computed (sum == 5, which requires the
// assignment to have RUN) can tell the difference, which is why the breakpoint
// for this oracle is on the line after the assignment rather than on it.
func TestLiveDelveVariablesReadsALocal(t *testing.T) {
	requireDlv(t)

	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()

	s := startStoppedSession(t, ctx, stepTargetMarker)
	top := s.topFrame(t, ctx, s.last)
	t.Logf("stopped in frame %d (%s) at %s:%d", top.ID, top.Name, filepath.Base(top.Source.Path), top.Line)

	scopes, err := s.client.Scopes(ctx, top.ID)
	if err != nil {
		t.Fatalf("scopes(frame %d): %v", top.ID, err)
	}
	if len(scopes) == 0 {
		t.Fatal("a stopped frame reported no scopes at all")
	}
	for _, sc := range scopes {
		t.Logf("scope %q: ref=%d named=%d indexed=%d expensive=%v",
			sc.Name, sc.VariablesReference, sc.NamedVariables, sc.IndexedVariables, sc.Expensive)
	}

	// Walk every non-expensive scope looking for the local. Which scope a Go
	// local lands in is delve's business, not ours, so hardcoding "Locals" would
	// make this oracle a test of delve's naming rather than of our chain.
	var found *Variable
	var seen []string
	for _, sc := range scopes {
		if sc.Expensive || sc.VariablesReference == 0 {
			continue
		}
		vars, err := s.client.Variables(ctx, sc.VariablesReference, 0, 200)
		if err != nil {
			t.Fatalf("variables(scope %q, ref %d): %v", sc.Name, sc.VariablesReference, err)
		}
		for i := range vars {
			seen = append(seen, fmt.Sprintf("%s.%s=%s", sc.Name, vars[i].Name, vars[i].Value))
			if vars[i].Name == localName {
				found = &vars[i]
			}
		}
	}
	if found == nil {
		t.Fatalf("no variable named %q in any scope of the stopped frame; saw %v", localName, seen)
	}
	t.Logf("variables: %v", seen)

	if found.Value != localValue {
		t.Fatalf("%s = %q, want %q — the assignment on the previous line had not run, "+
			"or these are another frame's variables", localName, found.Value, localValue)
	}
	if found.Type == "" {
		t.Errorf("%s came back with no type; supportsVariableType is declared at initialize", localName)
	}
	if found.VariablesReference != 0 {
		t.Errorf("%s is an int and reported children (ref %d)", localName, found.VariablesReference)
	}
}

// countStopped counts how many `stopped` events the session has seen. Read off
// the collector, which records everything the read goroutine delivered, rather
// than off the waiter channel — a channel a test drains is a channel whose depth
// no longer answers "how many times did this happen".
func (s *liveSession) countStopped() int {
	n := 0
	for _, name := range s.waiter.col.names() {
		if name == EventStopped {
			n++
		}
	}
	return n
}

// TestLiveDelveConditionalBreakpointFiresOnce is the definition of done for
// conditional breakpoints: a breakpoint inside a ten-iteration loop, carrying
// `i == 3`, stops the program EXACTLY ONCE, on the iteration where i is 3.
//
// 🔴 Both halves of that sentence are load-bearing and neither is sufficient
// alone. "Stopped exactly once" without the value passes for a condition that
// was dropped on the floor and a breakpoint that happened to be hit once;
// "i == 3 at the stop" without the count passes for a condition that was ignored
// and simply stopped on the first of ten iterations that also… no, on the FIRST
// iteration i is 0 — which is precisely why the value is asserted through
// `evaluate`, in the stopped frame. A dropped condition stops with i == 0 and
// then nine more times; an honoured one stops once with i == 3.
//
// RED against a client that does not put `condition` on the wire: the run then
// stops ten times and the first stop reports i == 0.
func TestLiveDelveConditionalBreakpointFiresOnce(t *testing.T) {
	dlv := requireDlv(t)
	t.Logf("using %s", dlv)

	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()

	var loopLine int
	s := startConfiguredSession(t, ctx, func(file string, lines []string) []SourceBreakpoint {
		loopLine = lineWithMarker(t, lines, loopBodyMarker)
		return []SourceBreakpoint{{Line: loopLine, Condition: loopCondition}}
	})

	// The capability this whole feature is gated on, read from a real adapter
	// rather than assumed. Recorded rather than merely asserted, because what
	// delve actually answers is the thing internal/app branches on.
	t.Logf("delve capabilities: conditionalBreakpoints=%v logPoints=%v hitConditional=%v",
		s.caps.SupportsConditionalBreakpoints, s.caps.SupportsLogPoints,
		s.caps.SupportsHitConditionalBreakpoints)
	if !s.caps.SupportsConditionalBreakpoints {
		t.Fatal("delve did not advertise supportsConditionalBreakpoints; the gate would refuse this")
	}

	stopped := s.awaitStopped(t, "the conditional breakpoint")
	if stopped.Reason != "breakpoint" {
		t.Fatalf("stopped for reason %q, want breakpoint: %+v", stopped.Reason, stopped)
	}
	top := s.assertStoppedOn(t, ctx, stopped, loopBodyMarker)

	// 🔴 The value, in the STOPPED FRAME. context "watch" plus the frame id is
	// what makes this the loop's i rather than some other scope's.
	res, err := s.client.Evaluate(ctx, loopVarName, top.ID, EvalContextWatch)
	if err != nil {
		t.Fatalf("evaluate(%q) in frame %d: %v", loopVarName, top.ID, err)
	}
	t.Logf("evaluate(%q) = %q (type %q) in frame %d (%s)", loopVarName, res.Result, res.Type, top.ID, top.Name)
	if res.Result != loopConditionValue {
		t.Fatalf("stopped with %s = %q, want %q — the condition %q was not honoured "+
			"(a dropped condition stops on the first iteration, where %s is 0)",
			loopVarName, res.Result, loopConditionValue, loopCondition, loopVarName)
	}

	// --- and it must not stop again ---------------------------------------
	if err := s.client.Continue(ctx, stopped.ThreadID); err != nil {
		t.Fatalf("continue: %v", err)
	}
	deadline := time.Now().Add(90 * time.Second)
	done := false
	for time.Now().Before(deadline) {
		for _, n := range s.waiter.col.names() {
			if n == EventTerminated || n == EventExited {
				done = true
			}
		}
		if done {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !done {
		t.Fatalf("after continue the program never finished; events: %v", s.waiter.col.names())
	}
	if got := s.countStopped(); got != 1 {
		t.Fatalf("the program stopped %d times on a loop of 10 with condition %q; want exactly 1. events: %v",
			got, loopCondition, s.waiter.col.names())
	}
	t.Logf("condition %q: 1 stop out of 10 iterations, %s == %s", loopCondition, loopVarName, res.Result)
}

// TestLiveDelveLogPointDoesNotStop is the other half of the capability pair:
// delve advertises supportsLogPoints, and a breakpoint carrying logMessage must
// therefore run STRAIGHT THROUGH the loop without halting it.
//
// 🔴 The assertion is "zero stops and the program finished", not "some output
// appeared". Which category a logpoint's text arrives under, and whether it
// arrives at all, is adapter policy; whether the program was halted is the
// contract — and a logMessage that the adapter treated as an ordinary breakpoint
// would stop ten times, which is the failure this pins.
func TestLiveDelveLogPointDoesNotStop(t *testing.T) {
	requireDlv(t)

	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()

	s := startConfiguredSession(t, ctx, func(file string, lines []string) []SourceBreakpoint {
		return []SourceBreakpoint{{
			Line:       lineWithMarker(t, lines, loopBodyMarker),
			LogMessage: "i is {i}",
		}}
	})
	if !s.caps.SupportsLogPoints {
		t.Fatal("delve did not advertise supportsLogPoints; the gate would refuse this")
	}

	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		for _, n := range s.waiter.col.names() {
			if n == EventTerminated || n == EventExited {
				if got := s.countStopped(); got != 0 {
					t.Fatalf("a logpoint stopped the program %d time(s); a log point must not halt it. events: %v",
						got, s.waiter.col.names())
				}
				t.Logf("logpoint ran the loop to completion without stopping; events: %v", s.waiter.col.names())
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the program never finished with a logpoint set; events: %v, stops: %d",
		s.waiter.col.names(), s.countStopped())
}
