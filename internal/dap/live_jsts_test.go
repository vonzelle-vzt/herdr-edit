// =============================================================================
// File: internal/dap/live_jsts_test.go
// Author: Vonzelle Brown
// Created: 2026-08-07
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// live_jsts_test.go is the TypeScript oracle: it exists to prove (or refuse to
// let anyone claim) that a breakpoint set on a .ts SOURCE file binds through a
// real source map and stops on the .ts line — not on the compiled .js output
// that node actually runs.
//
// The fixture is a real tsc artifact, embedded rather than compiled at test
// time. fixture.ts was compiled ONCE during development with
//
//	npx tsc fixture.ts --sourceMap --removeComments --target es2020
//
// (tsc 7.0.2) and the exact .js and .js.map bytes are string constants below,
// so the oracle has no tsc dependency at runtime: a machine with node and
// js-debug but no TypeScript compiler still runs it. --removeComments matters:
// it keeps the breakpoint marker UNIQUE to the .ts file, so a stop asserted by
// line text cannot accidentally match the compiled output — the same
// one-file-one-marker rule the JS fixture states.
// TestJsTsEmbeddedFixtureIsConsistent re-checks the embedded artifacts agree
// with each other, so drift in one constant cannot silently unmoor the oracle.
//
// 🔴 The judge is the STOPPED FRAME'S PATH, never the setBreakpoints answer.
// js-debug answers verified:false ("breakpoint.provisionalBreakpoint") for
// breakpoints that then hit (measured, see live_jsdebug_test.go) — so verified
// is worthless in both directions here. The only evidence that source maps
// work is a stopped event whose top frame resolves to fixture.ts at the marker
// line. Stopping on fixture.js instead means the map was ignored, and the
// language must NOT be claimed.
//
// 🔴 THE ANTI-SKIP GATE, identical to the other live files'. These skip when no
// js-debug can be found so a fresh clone stays green, and HERDR_REQUIRE_DAP=1
// turns every skip into a failure. A skipped test reads as a pass in the summary.
package dap

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tsBreakpointMarker marks the .ts line the oracle arms. Its own constant, not
// shared with the Go, Python or JS fixtures: one file's marker accidentally
// matching another's is how an oracle ends up asserting about the wrong program.
const tsBreakpointMarker = "TS-BREAKPOINT-TARGET"

// tsFixtureSource is fixture.ts exactly as it was handed to tsc.
const tsFixtureSource = `// A fixture for the live TypeScript source-map oracle.

function greet(name: string): string {
  const message: string = 'hello, ' + name; // ` + tsBreakpointMarker + `
  return message;
}

console.log('TSDEBUG-FIXTURE-PRINTED-THIS', greet('sourcemaps'));
`

// tsFixtureCompiledJS is tsc's exact output for tsFixtureSource. Note the
// marker comment is GONE (--removeComments) and the sourceMappingURL trailer
// is what lets js-debug find the map next to the file node runs.
const tsFixtureCompiledJS = `"use strict";
function greet(name) {
    const message = 'hello, ' + name;
    return message;
}
console.log('TSDEBUG-FIXTURE-PRINTED-THIS', greet('sourcemaps'));
//# sourceMappingURL=fixture.js.map`

// tsFixtureSourceMap is the real map tsc emitted for that compile, verbatim.
const tsFixtureSourceMap = `{"version":3,"file":"fixture.js","sourceRoot":"","sources":["fixture.ts"],"names":[],"mappings":";AAEA,SAAS,KAAK,CAAC,IAAY;IACzB,MAAM,OAAO,GAAW,SAAS,GAAG,IAAI,CAAC;IACzC,OAAO,OAAO,CAAC;AACjB,CAAC;AAED,OAAO,CAAC,GAAG,CAAC,8BAA8B,EAAE,KAAK,CAAC,YAAY,CAAC,CAAC,CAAC"}`

// jsTsFixture writes the three embedded artifacts — source, compiled output,
// map — into one directory, which is all the layout tsc's relative
// sourceMappingURL and the map's relative "sources" entry require.
func jsTsFixture(t *testing.T) (root, tsFile, jsFile string, tsLines []string) {
	t.Helper()
	root = t.TempDir()
	tsFile = filepath.Join(root, "fixture.ts")
	jsFile = filepath.Join(root, "fixture.js")
	for path, content := range map[string]string{
		tsFile:                                tsFixtureSource,
		jsFile:                                tsFixtureCompiledJS,
		filepath.Join(root, "fixture.js.map"): tsFixtureSourceMap,
	} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root, tsFile, jsFile, strings.Split(tsFixtureSource, "\n")
}

// TestJsTsEmbeddedFixtureIsConsistent guards the embedded artifacts against
// drifting apart. It needs no adapter, so it runs everywhere, including -short:
// an edit to one constant that forgets the others would otherwise surface only
// as a live-oracle failure that reads like a source-map bug in js-debug.
func TestJsTsEmbeddedFixtureIsConsistent(t *testing.T) {
	if !strings.HasSuffix(tsFixtureCompiledJS, "//# sourceMappingURL=fixture.js.map") {
		t.Error("the compiled fixture does not end with the sourceMappingURL trailer; " +
			"js-debug would never find the map")
	}
	if strings.Contains(tsFixtureCompiledJS, tsBreakpointMarker) {
		t.Errorf("the compiled fixture contains %q — the marker must be unique to the .ts "+
			"file (compile with --removeComments), or a stop on the WRONG file could pass "+
			"a by-text assertion", tsBreakpointMarker)
	}
	var m struct {
		Version  int      `json:"version"`
		File     string   `json:"file"`
		Sources  []string `json:"sources"`
		Mappings string   `json:"mappings"`
	}
	if err := json.Unmarshal([]byte(tsFixtureSourceMap), &m); err != nil {
		t.Fatalf("the embedded source map is not JSON: %v", err)
	}
	if m.Version != 3 {
		t.Errorf("source map version = %d, want 3", m.Version)
	}
	if m.File != "fixture.js" {
		t.Errorf("source map file = %q, want fixture.js", m.File)
	}
	if len(m.Sources) != 1 || m.Sources[0] != "fixture.ts" {
		t.Errorf("source map sources = %v, want [fixture.ts]", m.Sources)
	}
	if m.Mappings == "" {
		t.Error("the source map has no mappings; nothing could ever bind through it")
	}
	if lineWithMarker(t, strings.Split(tsFixtureSource, "\n"), tsBreakpointMarker) == 0 {
		t.Errorf("the .ts fixture has no %q line to arm", tsBreakpointMarker)
	}
}

// TestLiveJsTsBreakpointBindsThroughSourceMap is the definition of done for the
// TypeScript language claim: node launches the compiled fixture.js under a real
// js-debug, the breakpoint is set on fixture.ts — the file node never touches —
// and the program stops with its top frame ON the .ts path at the marker line.
//
// The session is driven through the `javascript` adapter row on purpose, not a
// `typescript` one: the oracle must be runnable BEFORE the registry claims the
// language, or the claim could never be gated on it.
func TestLiveJsTsBreakpointBindsThroughSourceMap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), jsDebugTimeout)
	defer cancel()

	root, tsFile, jsFile, tsLines := jsTsFixture(t)
	requireJsDebug(t, root)

	adapter, ok := AdapterFor("javascript")
	if !ok {
		t.Fatal("no adapter registered for javascript")
	}

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

	cfg := map[string]interface{}{}
	for k, v := range adapter.Launch {
		cfg[k] = v
	}
	// The COMPILED file: node runs JavaScript, and proving the map means asking
	// to stop somewhere node has never heard of. The type stays the adapter's
	// `pwa-node`: a user's launch.json may say `node`, but that alias is
	// resolved by VS Code's extension layer, not by the standalone
	// dapDebugServer — measured, it answers `Error: Unknown config` and
	// terminates without ever starting a child session.
	cfg["program"] = jsFile
	// What launchselect.go sets through Adapter.WorkspaceFolderKey on a real F5.
	// js-debug's outFiles / resolveSourceMapLocations defaults hang off it, so a
	// session without it exercises a config no user session runs.
	cfg[adapter.WorkspaceFolderKey] = root
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

	// 🔴 The breakpoint goes on the CHILD, and on the .ts PATH — a file that is
	// not the program and that node never loads. Only the source map can turn
	// this into a binding.
	markerLine := lineWithMarker(t, tsLines, tsBreakpointMarker)
	answers, err := leaf.SetBreakpoints(ctx,
		Source{Path: tsFile, Name: filepath.Base(tsFile)},
		[]SourceBreakpoint{{Line: markerLine}})
	if err != nil {
		t.Fatalf("child setBreakpoints on the .ts source: %v", err)
	}
	// Logged, never judged: js-debug answers verified:false for breakpoints
	// that then hit, so the answer proves nothing in either direction.
	for i, ans := range answers {
		t.Logf("breakpoint %d asked for %s:%d, answered %+v", i, filepath.Base(tsFile), markerLine, ans)
	}
	if err := leaf.SetExceptionBreakpoints(ctx, caps.DefaultFilters()); err != nil {
		t.Fatalf("child setExceptionBreakpoints: %v", err)
	}
	if err := leaf.ConfigurationDone(ctx); err != nil {
		t.Fatalf("child configurationDone: %v", err)
	}

	var stopped StoppedEvent
	select {
	case stopped = <-waiter.stopped:
	case <-ctx.Done():
		t.Fatalf("no stopped event for the source-mapped breakpoint — the program ran past "+
			"it, which is what an ignored source map looks like. child events seen: %v\n"+
			"adapter stderr: %v", waiter.col.names(), rootClient.LastStderr())
	}
	if stopped.Reason != "breakpoint" {
		t.Fatalf("stopped for reason %q, want breakpoint: %+v", stopped.Reason, stopped)
	}

	frames, err := leaf.StackTrace(ctx, stopped.ThreadID, 20)
	if err != nil {
		t.Fatalf("stackTrace: %v", err)
	}
	if len(frames) == 0 {
		t.Fatal("stackTrace returned no frames for a stopped thread")
	}
	top := frames[0]

	// THE oracle. EvalSymlinks on both sides (macOS reports /var/folders as
	// /private/var/folders), and the .ts path or nothing: a stop on the .js is
	// a working BREAKPOINT but a failed SOURCE MAP, and claiming typescript on
	// it would ship a debugger that steps through compiled output.
	got := resolvePath(top.Source.Path)
	if want := resolvePath(jsFile); got == want {
		t.Fatalf("stopped on the COMPILED output %q, not the .ts source — the breakpoint "+
			"bound but the source map was not applied to the reported frame. TypeScript "+
			"is NOT proven; do not add it to the registry", got)
	}
	if want := resolvePath(tsFile); got != want {
		t.Fatalf("stopped in %q, want the .ts source %q", got, want)
	}
	if top.Line < 1 || top.Line > len(tsLines) {
		t.Fatalf("stopped on line %d, outside the .ts fixture's %d lines", top.Line, len(tsLines))
	}
	if text := tsLines[top.Line-1]; !strings.Contains(text, tsBreakpointMarker) {
		t.Fatalf("stopped on .ts line %d whose text is %q — that is not the line marked %q",
			top.Line, strings.TrimSpace(text), tsBreakpointMarker)
	}

	// The frame is real, not just plausibly named: evaluate in it. Logged
	// rather than asserted — string rendering is the adapter's business, the
	// frame's PATH above is the oracle.
	if res, err := leaf.Evaluate(ctx, "name", top.ID, EvalContextWatch); err == nil {
		t.Logf("evaluate(\"name\") in the stopped .ts frame = %q (type %q)", res.Result, res.Type)
	}
	t.Logf("PROVEN: breakpoint set on %s:%d stopped with the top frame on the resolved .ts "+
		"path at that line (frame %q) — the source map bound, TypeScript is claimable",
		filepath.Base(tsFile), markerLine, top.Name)

	if err := leaf.Disconnect(ctx, true); err != nil {
		t.Errorf("disconnect: %v", err)
	}
}
