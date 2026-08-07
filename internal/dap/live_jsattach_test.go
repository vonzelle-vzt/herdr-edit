// =============================================================================
// File: internal/dap/live_jsattach_test.go
// Author: Vonzelle Brown
// Created: 2026-08-07
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// live_jsattach_test.go drives a REAL js-debug through a REAL ATTACH session,
// and it exists to answer the one question the launch oracles cannot: when the
// process ALREADY EXISTS — started by hand with --inspect-brk, the config
// carrying nothing but a port — does js-debug's root session still coordinate
// through the `startDebugging` reverse request, and do breakpoints still belong
// to the child it asks for?
//
// The launch oracles (live_jsdebug_test.go, live_jschrome_test.go) prove the
// coordinator-plus-child shape when js-debug SPAWNS the debuggee. Attach is the
// other half of the editor's F5 story — a launch.json row
// {"type":"node","request":"attach","port":N} — and nothing in the spec
// promises the two halves share a shape. An implementation that assumed they
// do, in either direction, would ship a debugger whose attach mode looks
// healthy and binds nothing; the measurement below is the only evidence either
// way in this repo.
//
// The debuggee is OURS: this test starts `node --inspect-brk=127.0.0.1:<port>`
// itself, which parks the process before its first statement until a debugger
// attaches and resumes it. That removes the arming race by construction — the
// breakpointed function cannot have run before the breakpoint exists — and it
// means the first stopped event may be the entry pause on line 1 rather than
// the breakpoint, which the assertion loop below handles by resuming.
//
// 🔴 HONESTY RULES, inherited from the launch oracles and load-bearing here:
//
//   - Binding is judged ONLY by a stopped event whose top frame lands on the
//     resolved fixture path and the marker's line — filepath.EvalSymlinks on
//     BOTH sides, because macOS writes the fixture under /var/folders/... and
//     js-debug reports it as /private/var/folders/... Never by the answer to
//     setBreakpoints: js-debug answers verified:false
//     ("breakpoint.provisionalBreakpoint") and then stops anyway.
//   - The stop is asserted by LINE TEXT, never by number — the frames come back
//     through a source-map layer.
//   - The child's configuration is sent VERBATIM. Merging our attach keys back
//     in would re-add `port`, and the pending-target id is the only thing tying
//     the connection to the process we already own.
//
// 🔴 THE ANTI-SKIP GATE, identical to the other live files'. These skip when no
// js-debug or no node can be found so a fresh clone stays green, and
// HERDR_REQUIRE_DAP=1 turns every skip into a failure. A skipped test reads as
// a pass in the summary.
package dap

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// jsAttachTimeout bounds the whole attach session. Same depth as the launch
// oracle — us, the adapter server, and node — minus the launcher, but the
// session is still brought up twice, once per connection.
const jsAttachTimeout = 90 * time.Second

// Markers in the attach fixture. Their own constants, not shared with the other
// fixtures: one file's marker accidentally matching another's is how an oracle
// ends up asserting about the wrong program.
const (
	jsAttachBreakpointMarker = "JSATTACH-BREAKPOINT-TARGET"
	jsAttachStdoutSentinel   = "JSATTACH-FIXTURE-PRINTED-THIS"
)

// jsAttachFixture writes a small real Node program and returns its directory,
// file and lines.
//
// The breakpointed function is called from the LAST line, after the entry
// pause --inspect-brk parks the process at — so the marker line is reached only
// after this test resumes execution, and a stop there can only mean the
// breakpoint bound.
func jsAttachFixture(t *testing.T) (root, file string, lines []string) {
	t.Helper()
	root = t.TempDir()
	src := `// A fixture for the live js-debug ATTACH oracle.

function add(a, b) {
  const total = a + b; // ` + jsAttachBreakpointMarker + `
  return total;
}

console.log('` + jsAttachStdoutSentinel + `', add(2, 3));
`
	file = filepath.Join(root, "fixture.js")
	if err := os.WriteFile(file, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, file, strings.Split(src, "\n")
}

// jsAttachFreePort asks the kernel for a free loopback port and releases it for
// the debuggee to take.
//
// The gap between closing the listener and node binding the port is a real
// race, but losing it turns into a loud startup failure in the debuggee's
// output — never a silently wrong measurement.
func jsAttachFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding a free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatalf("releasing the probe listener: %v", err)
	}
	return port
}

// startInspectBrkDebuggee starts the fixture under node's own inspector,
// paused at its first statement, and returns once the inspector's HTTP
// endpoint answers — the signal that the port is really accepting debuggers,
// which node reaching main is not.
//
// The process is killed in t.Cleanup, failure paths included: the cleanup is
// registered the moment the process exists, before anything else can Fatal.
func startInspectBrkDebuggee(t *testing.T, file string) int {
	t.Helper()

	// node resolved the way LocateJsDebug resolves it, so the debuggee and the
	// adapter cannot disagree about which node this machine has.
	node := findExecutable("node")
	if node == "" {
		skipOrFatal(t, "no node could be resolved — the attach oracle needs a live "+
			"debuggee to attach to")
	}

	port := jsAttachFreePort(t)
	cmd := exec.Command(node, fmt.Sprintf("--inspect-brk=127.0.0.1:%d", port), file)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the debuggee: %v", err)
	}
	t.Cleanup(func() {
		// Kill before Wait, and read the buffer only after Wait: os/exec copies
		// the process's output into it from goroutines that Wait joins, so
		// reading earlier is a data race under -race.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Logf("debuggee output: %q", output.String())
	})

	// A bounded poll, never an unbounded wait: an inspector that never comes up
	// must fail this test, not hang it. (macOS has no `timeout` command, which
	// is why the bound lives here in Go rather than in any wrapper script.)
	deadline := time.Now().Add(15 * time.Second)
	url := fmt.Sprintf("http://127.0.0.1:%d/json", port)
	for {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return port
			}
		}
		if time.Now().After(deadline) {
			// The debuggee's output is NOT quoted here — it is still being
			// written. The cleanup above logs it after Wait.
			t.Fatalf("the inspector endpoint %s never answered; the debuggee "+
				"either lost the port race or failed to start", url)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestLiveJsAttachStopsAtBreakpoint is the definition of done for attach: a
// node process this test started with --inspect-brk, an attach config carrying
// only the port, and the debuggee stopping on the line we marked — in whichever
// session js-debug puts the debuggee, which is the thing being measured.
//
// MEASURED (js-debug 1.117.0, node v24): the attach root behaves exactly like
// the launch root. startDebugging arrived after the root's configurationDone,
// carrying request:"attach" and {__pendingTargetId, name:"Remote Process [0]",
// type:pwa-node}; the child's setBreakpoints answered verified:false
// ("breakpoint.provisionalBreakpoint"); the --inspect-brk entry pause surfaced
// as a stopped event with reason "pause" on the fixture's first executable
// STATEMENT (the console.log line — the hoisted function declaration is not
// where V8 parks); after one continue the debuggee stopped on the marker line,
// reason "breakpoint". So the coordinator-plus-child shape is attach's shape
// too, and Adapter.UsesChildSessions routing breakpoints to the leaf is as
// load-bearing here as it is for launch.
func TestLiveJsAttachStopsAtBreakpoint(t *testing.T) {
	root, file, lines := jsAttachFixture(t)
	requireJsDebug(t, root)
	port := startInspectBrkDebuggee(t, file)

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

	ctx, cancel := context.WithTimeout(context.Background(), jsAttachTimeout)
	defer cancel()

	// ---- ROOT: the coordinator ------------------------------------------------
	rootWaiter, rootHandlers := newStoppedWaiter(t)
	reg := NewRegistry(root)
	rootClient, err := reg.Start(ctx, adapter, rootHandlers)
	if err != nil {
		t.Fatalf("starting js-debug: %v", err)
	}
	t.Cleanup(rootClient.Stop)

	if _, err := rootClient.Initialize(ctx, adapter.AdapterID); err != nil {
		t.Fatalf("root initialize: %v (adapter stderr: %v)", err, rootClient.LastStderr())
	}

	// The wire shape of a launch.json {"type":"node","request":"attach","port":N}
	// row: `node` is the alias VS Code registers, `pwa-node` the canonical name
	// the standalone server dispatches on — the same alias-to-canonical rule
	// TestChromeWireConfigIsCompleteOffline pins for the browser row.
	cfg := map[string]interface{}{
		"type":    "pwa-node",
		"request": "attach",
		"name":    "Attach (oracle)",
		"address": "127.0.0.1",
		"port":    port,
	}
	t.Logf("attach config: %v", cfg)

	if _, err := rootClient.Attach(cfg); err != nil {
		t.Fatalf("root attach: %v (adapter stderr: %v)", err, rootClient.LastStderr())
	}
	if err := rootClient.WaitEvent(ctx, EventInitialized); err != nil {
		t.Fatalf("root never sent initialized: %v (adapter stderr: %v)", err, rootClient.LastStderr())
	}
	// 🔴 No breakpoints on the coordinator, and configurationDone regardless:
	// same as launch — without it the root never proceeds and startDebugging
	// never comes.
	if err := rootClient.ConfigurationDone(ctx); err != nil {
		t.Fatalf("root configurationDone: %v", err)
	}

	// ---- THE MEASUREMENT: does an attach root coordinate at all? --------------
	// Bounded well under the session timeout so a root that never asks for a
	// child produces a diagnosis, not a 90-second hang blamed on nothing.
	childCtx, childCancel := context.WithTimeout(ctx, 30*time.Second)
	defer childCancel()
	cs, err := rootClient.AwaitChildSession(childCtx)
	if err != nil {
		t.Fatalf("js-debug's ATTACH root never sent startDebugging: %v (adapter stderr: %v)\n"+
			"root events seen: %v\n"+
			"If this reproduces, the attach shape has diverged from launch — record the wire "+
			"behaviour in ATTACH-FINDINGS.md and in CLAUDE.md's js-debug section before touching "+
			"any routing code.", err, rootClient.LastStderr(), rootWaiter.col.names())
	}
	t.Logf("startDebugging: request=%q configuration=%v", cs.Request, cs.Configuration)
	if cs.Configuration == nil {
		t.Fatal("the child session carries no configuration; there would be nothing to start it with")
	}

	// ---- CHILD: where the debuggee lives --------------------------------------
	waiter, handlers := newStoppedWaiter(t)
	leaf, err := rootClient.DialChild("js-debug (attach)", handlers)
	if err != nil {
		t.Fatalf("dialing the child session: %v", err)
	}
	t.Cleanup(leaf.Stop)

	caps, err := leaf.Initialize(ctx, adapter.AdapterID)
	if err != nil {
		t.Fatalf("child initialize: %v", err)
	}
	// VERBATIM, and by the request IT names — launchOrAttachChild sends `attach`
	// when startDebugging asked for an attach, which is what it asks for here.
	if _, err := launchOrAttachChild(leaf, cs); err != nil {
		t.Fatalf("child %s: %v", cs.Request, err)
	}
	if err := leaf.WaitEvent(ctx, EventInitialized); err != nil {
		t.Fatalf("child never sent initialized: %v", err)
	}

	// ---- Arm the breakpoint on the CHILD --------------------------------------
	line := lineWithMarker(t, lines, jsAttachBreakpointMarker)
	answers, err := leaf.SetBreakpoints(ctx,
		Source{Path: file, Name: filepath.Base(file)},
		[]SourceBreakpoint{{Line: line}})
	if err != nil {
		t.Fatalf("child setBreakpoints: %v", err)
	}
	// Logged, not asserted verified: js-debug answers provisional and stops
	// anyway. See TestLiveJsDebugAnswersProvisionalAndStopsAnyway.
	for i, ans := range answers {
		t.Logf("breakpoint %d asked for line %d, answered %+v", i, line, ans)
	}
	if err := leaf.SetExceptionBreakpoints(ctx, caps.DefaultFilters()); err != nil {
		t.Fatalf("child setExceptionBreakpoints: %v", err)
	}
	if err := leaf.ConfigurationDone(ctx); err != nil {
		t.Fatalf("child configurationDone: %v", err)
	}

	// ---- The assertion --------------------------------------------------------
	// --inspect-brk parks the debuggee before its first statement, so the first
	// stopped event is expected to be that entry pause, not the breakpoint.
	// Resume past anything that is not the marker line, boundedly: two stops are
	// the measured shape (reason "pause" on the first executable statement, then
	// "breakpoint" on the marker), three is headroom, and a debugger that needs
	// more than that is not binding.
	for attempt := 0; attempt < 3; attempt++ {
		var ev StoppedEvent
		select {
		case ev = <-waiter.stopped:
		case <-time.After(jsAttachTimeout):
			t.Fatalf("no stopped event arrived (attempt %d). child events seen: %v\nadapter stderr: %v",
				attempt+1, waiter.col.names(), rootClient.LastStderr())
		}

		frames, err := leaf.StackTrace(ctx, ev.ThreadID, 20)
		if err != nil {
			t.Fatalf("stackTrace: %v", err)
		}
		if len(frames) == 0 {
			t.Fatal("stackTrace returned no frames for a stopped thread")
		}
		top := frames[0]

		// 🔴 EvalSymlinks on both sides, marker by line TEXT. This is the whole
		// judgment: a stop that resolves to the fixture file on the marked line.
		onMarker := resolvePath(top.Source.Path) == resolvePath(file) &&
			top.Line >= 1 && top.Line <= len(lines) &&
			strings.Contains(lines[top.Line-1], jsAttachBreakpointMarker)
		if !onMarker {
			t.Logf("stopped (reason %q) at %s:%d before the breakpoint — resuming",
				ev.Reason, top.Source.Path, top.Line)
			if err := leaf.Continue(ctx, ev.ThreadID); err != nil {
				t.Fatalf("continue past the stop at %s:%d: %v", top.Source.Path, top.Line, err)
			}
			continue
		}

		if ev.Reason != "breakpoint" {
			t.Errorf("stopped on the marker line for reason %q, want breakpoint: %+v", ev.Reason, ev)
		}
		t.Logf("ATTACH stopped in %s at %s:%d — %q (reason %q)",
			top.Name, filepath.Base(top.Source.Path), top.Line,
			strings.TrimSpace(lines[top.Line-1]), ev.Reason)

		// Detach WITHOUT terminating: the debuggee is this test's process to
		// kill, and t.Cleanup does. A launch session would pass true here.
		if err := leaf.Disconnect(ctx, false); err != nil {
			t.Logf("disconnect: %v", err)
		}
		return
	}
	t.Fatalf("the debuggee never stopped on the line marked %q — the attach-session breakpoint "+
		"did not bind. child events seen: %v\nadapter stderr: %v",
		jsAttachBreakpointMarker, waiter.col.names(), rootClient.LastStderr())
}
