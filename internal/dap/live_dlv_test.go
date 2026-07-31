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
	return sum
}

func main() {
	fmt.Println("` + stdoutSentinel + `", add(2, 3))
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
