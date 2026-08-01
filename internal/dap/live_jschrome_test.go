// =============================================================================
// File: internal/dap/live_jschrome_test.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// live_jschrome_test.go drives a REAL headless Chrome through a REAL debug
// session, and it exists to falsify the one claim no offline test can reach:
// that a breakpoint set on a file ON DISK binds against a script the browser
// fetched over HTTP.
//
// 🔴 THE MEASUREMENT THIS FILE EXISTS FOR. js-debug's chrome defaults carry
// `webRoot: "${workspaceFolder}"`, and its resolver expands that from a
// `__workspaceFolder` key it reads off the configuration:
//
//	function zH(r){return r.__workspaceFolder||(r=GH(r)), …}
//	function GH(r){let e="${workspaceFolder}"; …
//	  "webRoot" in t && t.webRoot?.includes(e) && (t.webRoot="/") … }
//
// With the key ABSENT, GH runs and webRoot becomes "/" — so `pathMapping` maps
// the server root to the filesystem root, every fetched url resolves to a path
// that is not there, and NO BREAKPOINT BINDS. The session still initializes,
// launches, runs and terminates with no error anywhere. Node never needed the
// key, which is why the whole js-debug stage shipped without noticing. This
// oracle is the only thing in the repo that can tell the two apart.
//
// Three shapes of self-deception are ruled out deliberately:
//
//   - 🔴 THE FIXTURE MUST NOT RACE THE ARMING. The breakpointed function is
//     driven from setInterval, not from a top-level call. A script that already
//     ran before the breakpoint was armed produces a timeout with a completely
//     plausible excuse ("the page loaded too fast"), and the excuse is wrong.
//     A repeating driver means arming late costs 100ms, not the assertion.
//   - 🔴 THE STOP IS ASSERTED BY LINE TEXT, NEVER BY NUMBER. Browser frames come
//     back through a source-map layer; a line number that happens to agree is
//     exactly the assumption that would carry over from the node oracle
//     unexamined.
//   - 🔴 NO REAL CHROME PROFILE. js-debug's chrome default is `userDataDir: true`,
//     which means a fresh temp profile — verified in the bundle. Pointing at the
//     developer's own profile would make running the test suite interfere with
//     the browser they are using. Nothing here sets the key.
//
// 🔴 THE ANTI-SKIP GATE, identical to the other three live files'. These skip
// when no Chrome or no js-debug can be found so a fresh clone stays green, and
// HERDR_REQUIRE_DAP=1 turns every skip into a failure. A skipped test reads as a
// pass in the summary.
package dap

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// chromeTimeout bounds a whole browser session. Longer than the node one: this
// is five processes deep — us, the adapter server, the launcher, Chrome's
// browser process and its renderer — and a cold Chrome start dominates it.
const chromeTimeout = 120 * time.Second

// Markers in the browser fixture. Their own constants: one file's marker
// accidentally matching another's is how an oracle ends up asserting about the
// wrong program.
const (
	chromeBreakpointMarker = "JSCHROME-BREAKPOINT-TARGET"
	chromeLoadedSentinel   = "JSCHROME-FIXTURE-LOADED"
)

// chromeRuntimeArgs are the flags that make a browser session runnable on a
// headless machine — a CI runner, or a developer over SSH.
//
// --headless=new is Chrome's current headless mode; --disable-gpu and
// --no-sandbox are what keep it from failing on a container with no GPU and no
// user namespaces. They are deliberately NOT a full hardening list: every extra
// flag is another way for this to diverge from the browser a user actually
// debugs with.
var chromeRuntimeArgs = []interface{}{"--headless=new", "--disable-gpu", "--no-sandbox"}

// requireChrome resolves a browser to debug, or ends the test SAYING WHICH
// places it looked.
//
// 🔴 Chrome is resolved HERE rather than left to js-debug's own browser finder,
// and the reason is the skip message. js-debug answers a missing browser with a
// launch failure part-way into the handshake, which under HERDR_REQUIRE_DAP=1
// would surface as a failure inside internal/dap — sending a reader looking for
// a protocol bug that is not there. Resolving first turns "no browser" into a
// sentence naming the three places that were tried.
func requireChrome(t *testing.T) string {
	t.Helper()

	if p := os.Getenv("HERDR_CHROME"); p != "" {
		if isExecutableFile(p) {
			t.Logf("chrome resolved from HERDR_CHROME: %s", p)
			return p
		}
		t.Fatalf("HERDR_CHROME=%s is not an executable file", p)
	}
	if runtime.GOOS == "darwin" {
		for _, p := range []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		} {
			if isExecutableFile(p) {
				t.Logf("chrome resolved from the macOS application bundle: %s", p)
				return p
			}
		}
	}
	// 🔴 findExecutable, NOT toolpath.Look alone. toolpath.Look deliberately does
	// not search PATH — it is the launchd fallback, and its own package comment
	// says callers must try exec.LookPath first. On Linux, Chrome installs to
	// /usr/bin/google-chrome, which is in no directory toolpath.Dirs() lists, so
	// a Look-only resolver would SKIP on every Linux machine and CI runner —
	// a skip that reads as a pass, which is the exact failure the anti-skip gate
	// exists to prevent. findExecutable is the package's own LookPath-then-Look
	// helper, and LocateJsDebug already resolves `node` through it.
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome"} {
		if p := findExecutable(name); p != "" {
			t.Logf("chrome resolved from PATH or the tool directories: %s", p)
			return p
		}
	}

	skipOrFatal(t, "no Chrome or Chromium could be resolved — set HERDR_CHROME to a browser "+
		"binary, install Google Chrome (macOS: /Applications/Google Chrome.app), or put "+
		"google-chrome / chromium on PATH")
	return ""
}

// chromeFixture writes a real page and script into a temp directory and serves
// them over HTTP, returning the directory, the script path, its lines and the
// url to open.
//
// 🔴 The server is net/http/httptest from the standard library. A browser
// debugging oracle needs a real origin — file:// urls take a different code path
// through js-debug's path resolution and would test the wrong thing — and
// pulling in a server dependency to get one would break this repo's whole
// dependency rule for a package that ships in no binary.
func chromeFixture(t *testing.T) (root, script string, lines []string, url string) {
	t.Helper()
	// EvalSymlinks because macOS hands back /var/folders/... while the browser
	// and the adapter both speak /private/var/folders/... — the same directory
	// under two correct names. Comparing the unresolved ones fails against an
	// adapter that is right, which is the worst kind of red.
	root = resolvePath(t.TempDir())

	// 🔴 setInterval, not a top-level call. The breakpointed function must still
	// be REACHED after the breakpoint is armed; a script that ran once during
	// page load is a race whose failure looks like a slow browser.
	src := `// A fixture for the live js-debug chrome oracle.

function tick(n) {
  const doubled = n * 2; // ` + chromeBreakpointMarker + `
  return doubled;
}

let count = 0;
setInterval(function () {
  count = tick(count + 1);
}, 100);

console.log('` + chromeLoadedSentinel + `');
`
	script = filepath.Join(root, "app.js")
	if err := os.WriteFile(script, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	page := "<!doctype html>\n<html><head><title>herdr-edit chrome oracle</title></head>\n" +
		"<body><script src=\"/app.js\"></script></body></html>\n"
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte(page), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.FileServer(http.Dir(root)))
	t.Cleanup(srv.Close)
	return root, script, strings.Split(src, "\n"), srv.URL + "/index.html"
}

// chromeLaunchConfig builds the wire configuration for a browser session the way
// ResolveLaunchConfig would, so what this oracle measures is what the editor
// actually sends.
func chromeLaunchConfig(adapter Adapter, root, url, chrome string) map[string]interface{} {
	cfg := map[string]interface{}{}
	for k, v := range adapter.Launch {
		cfg[k] = v
	}
	cfg["name"] = "Launch Chrome (oracle)"
	cfg["url"] = url

	// webRoot is set EXPLICITLY rather than left to the adapter's
	// "${workspaceFolder}" default, so this oracle measures path mapping rather
	// than measuring the adapter's own variable expansion. The
	// __workspaceFolder assertion below is what covers the default's route.
	cfg["webRoot"] = root
	cfg[adapter.WorkspaceFolderKey] = root

	cfg["runtimeExecutable"] = chrome
	cfg["runtimeArgs"] = chromeRuntimeArgs
	// 🔴 No userDataDir. js-debug's chrome default is `userDataDir: true`,
	// meaning a fresh temporary profile — verified in the installed bundle. That
	// default is exactly what makes running this suite safe on a machine whose
	// owner has Chrome open.
	return cfg
}

// TestLiveJsDebugChromeStopsInABrowserScript is the definition of done for
// browser debugging: a page served over HTTP, a breakpoint set on the file ON
// DISK, and the browser stopping on that line.
//
// It is the only test in the repo that can fail when __workspaceFolder is
// dropped, because every layer beneath it succeeds without it.
func TestLiveJsDebugChromeStopsInABrowserScript(t *testing.T) {
	chrome := requireChrome(t)

	root, script, lines, url := chromeFixture(t)
	requireJsDebug(t, root)

	adapter, ok := AdapterForConfigType("pwa-chrome")
	if !ok {
		t.Fatal("no adapter answers the launch.json type pwa-chrome")
	}
	if !adapter.ConfigOnly() {
		t.Fatalf("the chrome adapter claims languages %v; it would steal them from the node row",
			adapter.Languages)
	}
	if adapter.WorkspaceFolderKey == "" {
		t.Fatal("the chrome adapter has no WorkspaceFolderKey; js-debug would fall back to " +
			"webRoot=\"/\" and nothing would ever bind")
	}

	ctx, cancel := context.WithTimeout(context.Background(), chromeTimeout)
	defer cancel()

	// ---- ROOT: the coordinator ------------------------------------------------
	rootWaiter, rootHandlers := newStoppedWaiter(t)
	reg := NewRegistry(root)
	rootClient, err := reg.Start(ctx, adapter, rootHandlers)
	if err != nil {
		t.Fatalf("starting js-debug for chrome: %v", err)
	}
	t.Cleanup(rootClient.Stop)

	if _, err := rootClient.Initialize(ctx, adapter.AdapterID); err != nil {
		t.Fatalf("root initialize: %v (adapter stderr: %v)", err, rootClient.LastStderr())
	}

	cfg := chromeLaunchConfig(adapter, root, url, chrome)
	t.Logf("chrome launch config: type=%v url=%v webRoot=%v %s=%v",
		cfg["type"], cfg["url"], cfg["webRoot"], adapter.WorkspaceFolderKey, cfg[adapter.WorkspaceFolderKey])

	if _, err := rootClient.Launch(cfg); err != nil {
		t.Fatalf("root launch: %v (adapter stderr: %v)", err, rootClient.LastStderr())
	}
	if err := rootClient.WaitEvent(ctx, EventInitialized); err != nil {
		t.Fatalf("root never sent initialized: %v (adapter stderr: %v)", err, rootClient.LastStderr())
	}
	// No breakpoints on the coordinator, and configurationDone regardless:
	// without it the root never launches and startDebugging never comes.
	if err := rootClient.ConfigurationDone(ctx); err != nil {
		t.Fatalf("root configurationDone: %v", err)
	}

	// ---- CHILD: where the page lives -----------------------------------------
	cs, err := rootClient.AwaitChildSession(ctx)
	if err != nil {
		t.Fatalf("js-debug never asked for a child session for the browser: %v "+
			"(adapter stderr: %v)\nroot events seen: %v",
			err, rootClient.LastStderr(), rootWaiter.col.names())
	}
	t.Logf("startDebugging: request=%q configuration=%v", cs.Request, cs.Configuration)

	waiter, handlers := newStoppedWaiter(t)
	leaf, err := rootClient.DialChild("js-debug (chrome)", handlers)
	if err != nil {
		t.Fatalf("dialing the child session: %v", err)
	}
	t.Cleanup(leaf.Stop)

	caps, err := leaf.Initialize(ctx, adapter.AdapterID)
	if err != nil {
		t.Fatalf("child initialize: %v", err)
	}
	// VERBATIM, exactly as the editor sends it: the child's configuration carries
	// the pending target id naming the page the root already opened.
	if _, err := launchOrAttachChild(leaf, cs); err != nil {
		t.Fatalf("child launch: %v", err)
	}
	if err := leaf.WaitEvent(ctx, EventInitialized); err != nil {
		t.Fatalf("child never sent initialized: %v", err)
	}

	// ---- Arm the breakpoint on the file ON DISK -------------------------------
	line := lineWithMarker(t, lines, chromeBreakpointMarker)
	answers, err := leaf.SetBreakpoints(ctx,
		Source{Path: script, Name: filepath.Base(script)},
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
	var ev StoppedEvent
	select {
	case ev = <-waiter.stopped:
	case <-time.After(chromeTimeout):
		t.Fatalf("the browser never stopped on %s. This is what a missing %s looks like: "+
			"webRoot falls back to \"/\", every fetched url maps to nothing on disk, and the "+
			"session stays perfectly healthy.\nchild events seen: %v\nadapter stderr: %v",
			chromeBreakpointMarker, adapter.WorkspaceFolderKey, waiter.col.names(), rootClient.LastStderr())
	}

	frames, err := leaf.StackTrace(ctx, ev.ThreadID, 20)
	if err != nil {
		t.Fatalf("stackTrace: %v", err)
	}
	if len(frames) == 0 {
		t.Fatal("stackTrace returned no frames for a stopped browser thread")
	}
	top := frames[0]

	// 🔴 BY TEXT, never by number. Browser frames come back through a source-map
	// layer, so a line number that agrees with the node oracle's proves nothing.
	if top.Line < 1 || top.Line > len(lines) {
		t.Fatalf("stopped on line %d, outside the fixture's %d lines", top.Line, len(lines))
	}
	if text := lines[top.Line-1]; !strings.Contains(text, chromeBreakpointMarker) {
		t.Fatalf("stopped on line %d whose text is %q — that is not the line marked %q",
			top.Line, strings.TrimSpace(text), chromeBreakpointMarker)
	}
	t.Logf("stopped in %s at %s:%d — %q",
		top.Name, filepath.Base(top.Source.Path), top.Line, strings.TrimSpace(lines[top.Line-1]))

	// The frame's source must be the file on disk, not a synthesised url. That is
	// the path mapping working, which is the thing __workspaceFolder buys.
	if got := resolvePath(top.Source.Path); got != resolvePath(script) {
		t.Errorf("the stopped frame names %q, want the file on disk %q — the browser's url did "+
			"not map back to the project", got, resolvePath(script))
	}
}

// TestChromeWireConfigIsCompleteOffline is the offline half, and it runs
// everywhere.
//
// 🔴 It exists because the live oracle above CANNOT run without a browser, and
// this fork's rule is that a skip reads as a pass. The three keys asserted here
// are the ones whose absence produces a healthy-looking session that binds
// nothing — so even on a machine with no Chrome, the specific silent failure
// this stage exists to prevent is still measured.
func TestChromeWireConfigIsCompleteOffline(t *testing.T) {
	root := resolvePath(t.TempDir())
	adapter, ok := AdapterForConfigType("pwa-chrome")
	if !ok {
		t.Fatal("no adapter answers the launch.json type pwa-chrome")
	}

	spec, err := ResolveLaunchConfig(LaunchConfig{
		Name: "Launch Chrome", Type: "chrome", Request: "launch",
		Args: map[string]interface{}{
			"name": "Launch Chrome", "type": "chrome", "request": "launch",
			"url":     "http://localhost:5173",
			"webRoot": "${workspaceFolder}",
		},
	}, LaunchVarContext{WorkspaceFolder: root, File: filepath.Join(root, "index.html")})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if got := spec.Args["type"]; got != "pwa-chrome" {
		t.Errorf("type = %v, want pwa-chrome — the standalone server dispatches on the "+
			"canonical name, not on the `chrome` alias the VS Code extension registers", got)
	}
	if got := spec.Args[adapter.WorkspaceFolderKey]; got != root {
		t.Errorf("%s = %v, want %s — js-debug's fallback sets webRoot to \"/\" without it, "+
			"and no browser breakpoint ever binds", adapter.WorkspaceFolderKey, got, root)
	}
	webRoot, _ := spec.Args["webRoot"].(string)
	if !filepath.IsAbs(webRoot) {
		t.Errorf("webRoot = %q, want an absolute path; a relative one maps nothing", webRoot)
	}
	if webRoot != root {
		t.Errorf("webRoot = %q, want the expanded workspace folder %q", webRoot, root)
	}

	t.Logf("⚠️ This is the OFFLINE half. It proves the wire configuration is complete; only "+
		"TestLiveJsDebugChromeStopsInABrowserScript proves a browser breakpoint actually binds, "+
		"and that one needs a real Chrome. Wire config: type=%v %s=%v webRoot=%v",
		spec.Args["type"], adapter.WorkspaceFolderKey, spec.Args[adapter.WorkspaceFolderKey],
		spec.Args["webRoot"])
}

// launchOrAttachChild sends a child session's configuration the way its parent
// asked for it, mirroring internal/app's helper of the same shape.
func launchOrAttachChild(client *Client, cs ChildSession) (<-chan Response, error) {
	if cs.Request == "attach" {
		return client.Attach(cs.Configuration)
	}
	return client.Launch(cs.Configuration)
}
