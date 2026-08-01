// =============================================================================
// File: internal/dap/registry_test.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package dap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestAdapterForGo checks the one adapter in the table answers for Go and for
// nothing else, so an unrelated file type never spawns a debugger.
func TestAdapterForGo(t *testing.T) {
	a, ok := AdapterFor("go")
	if !ok {
		t.Fatal("no adapter registered for go")
	}
	if a.Name != "delve" || a.AdapterID != "go" {
		t.Errorf("adapter = %+v, want delve/go", a)
	}
	if _, ok := AdapterFor("typescript"); ok {
		t.Error("an adapter answered for typescript; stage 2 ships delve only")
	}
}

// TestDelveArgvCarriesTheSocketPlaceholder pins the wiring that makes the
// adapter dial US. Without --client-addr, `dlv dap` listens instead and
// StartCommand's accept never fires — reported as "the adapter never
// connected", which reads like a missing binary.
func TestDelveArgvCarriesTheSocketPlaceholder(t *testing.T) {
	a, _ := AdapterFor("go")
	if len(a.Argv) == 0 || len(a.Argv[0]) == 0 {
		t.Fatal("delve has no argv")
	}
	joined := strings.Join(a.Argv[0], " ")
	if !strings.Contains(joined, "--client-addr") {
		t.Errorf("argv %q has no --client-addr; dlv would listen instead of dialing in", joined)
	}
	if !strings.Contains(joined, SocketPlaceholder) {
		t.Errorf("argv %q never mentions %s, so no socket path can be substituted", joined, SocketPlaceholder)
	}
	if !strings.Contains(joined, "unix:") {
		t.Errorf("argv %q lacks the unix: prefix delve requires for a socket path", joined)
	}
}

// TestResolvePrefersNodeModulesBin mirrors the lsp registry's rule: a
// repo-pinned adapter beats whatever is installed globally.
func TestResolvePrefersNodeModulesBin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable bit / shell script fixture is POSIX-only")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "node_modules", ".bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(bin, "some-adapter")
	if err := os.WriteFile(local, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry(root)
	got := r.Resolve(Adapter{Argv: [][]string{{"some-adapter", "--flag"}}})
	if got == nil || len(got.Argv) != 2 || got.Argv[0] != local || got.Argv[1] != "--flag" {
		t.Fatalf("Resolve = %+v, want the repo-local binary plus its args", got)
	}
}

// TestResolveReturnsNilWhenNothingIsInstalled checks the "not installed" path
// is a nil rather than a bogus argv, so Start can tell the two apart.
func TestResolveReturnsNilWhenNothingIsInstalled(t *testing.T) {
	r := NewRegistry(t.TempDir())
	if got := r.Resolve(Adapter{Argv: [][]string{{"definitely-not-a-real-binary-xyzzy"}}}); got != nil {
		t.Fatalf("Resolve = %+v, want nil for a binary that does not exist", got)
	}
}

// TestMissingBinaryIsStickyAndTyped pins the half of the rule that SHOULD be
// sticky: a binary that is not installed will not become installed while the
// editor runs, so re-running LookPath on every F5 is waste.
func TestMissingBinaryIsStickyAndTyped(t *testing.T) {
	r := NewRegistry(t.TempDir())
	a := Adapter{Name: "ghost", Argv: [][]string{{"definitely-not-a-real-binary-xyzzy"}}}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	_, err := r.Start(ctx, a, Handlers{})
	if err == nil {
		t.Fatal("Start succeeded for a binary that does not exist")
	}
	var notInstalled *NotInstalledError
	if !errors.As(err, &notInstalled) {
		t.Fatalf("error %v is not a *NotInstalledError; the app cannot tell "+
			"'install delve' apart from 'your code does not compile'", err)
	}
	if !r.Missing("ghost") {
		t.Error("a missing binary was not recorded; every F5 would re-run LookPath")
	}

	// The second attempt short-circuits and still reports the same typed error.
	if _, err := r.Start(ctx, a, Handlers{}); !errors.As(err, &notInstalled) {
		t.Errorf("second Start returned %v, want the same typed error", err)
	}
}

// TestLaunchFailureIsNotSticky is the half that must NOT be sticky, and the
// reason this package does not copy lsp.Manager's single `failed` map.
//
// The overwhelmingly common DAP failure is not a missing debugger — it is a
// program that does not compile, which the user fixes in thirty seconds. A
// sticky flag would leave F5 dead for the rest of the session with nothing on
// screen to say why. Here the binary EXISTS and exits immediately without ever
// dialing in, which is what a broken launch looks like from our side.
func TestLaunchFailureIsNotSticky(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is POSIX-only")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "exits-at-once")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho 'could not launch process' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry(dir)
	a := Adapter{Name: "flaky", Argv: [][]string{{fake}}}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	start := time.Now()
	_, err := r.Start(ctx, a, Handlers{})
	if err == nil {
		t.Fatal("Start succeeded against an adapter that exits immediately")
	}
	// An adapter that is already dead must be reported at once, not after the
	// full dial timeout — a 30-second wait for a known-dead process reads as a
	// hung editor.
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("took %s to notice the adapter had exited; dialTimeout is %s and should not have been waited out",
			elapsed, dialTimeout)
	}
	var notInstalled *NotInstalledError
	if errors.As(err, &notInstalled) {
		t.Errorf("a launch failure was reported as 'not installed': %v", err)
	}
	// 🔴 The assertion this test exists for.
	if r.Missing("flaky") {
		t.Fatal("a LAUNCH failure was recorded as a missing binary — F5 is now dead " +
			"for the rest of the session even after the user fixes their code")
	}
	// And the stderr that explains it survives into the error.
	if !strings.Contains(err.Error(), "could not launch process") {
		t.Errorf("error %q dropped the adapter's stderr, leaving the user nothing to act on", err)
	}
}

// TestStartCommandRejectsAnEmptyArgv covers the trivial guard, so a
// misconfigured table entry is an error rather than a panic.
func TestStartCommandRejectsAnEmptyArgv(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := StartCommand(ctx, "empty", Command{}, "", Handlers{}); err == nil {
		t.Fatal("StartCommand accepted an empty argv")
	}
}

// TestDescribeSaysSomethingEitherWay checks the status-bar summary is never
// blank: "F5 does nothing" must have a visible explanation.
func TestDescribeSaysSomethingEitherWay(t *testing.T) {
	r := NewRegistry(t.TempDir())
	if got := r.Describe(); got == "" {
		t.Fatal("Describe() is empty; the user gets no explanation at all")
	}
	if !strings.HasPrefix(r.Describe(), "dap:") {
		t.Errorf("Describe() = %q, want a dap: prefix matching lsp.Manager.Describe's shape", r.Describe())
	}
}

// TestNotInstalledErrorNamesTheBinary checks the message tells the user what to
// install rather than just that something is missing.
func TestNotInstalledErrorNamesTheBinary(t *testing.T) {
	e := &NotInstalledError{Name: "delve", Command: "dlv"}
	if !strings.Contains(e.Error(), "dlv") {
		t.Errorf("error %q does not name the binary to install", e)
	}
	bare := &NotInstalledError{Name: "delve"}
	if bare.Error() == "" {
		t.Error("an error with no command rendered as empty")
	}
}

// TestAdapterForPython pins the second row of the table, which is the whole
// point of this stage: the abstraction has to answer for something other than Go.
func TestAdapterForPython(t *testing.T) {
	a, ok := AdapterFor("python")
	if !ok {
		t.Fatal("no adapter registered for python")
	}
	if a.Name != "debugpy" || a.AdapterID != "python" {
		t.Errorf("adapter = %s/%s, want debugpy/python", a.Name, a.AdapterID)
	}
	if a.Transport != TransportStdio {
		t.Errorf("debugpy transport = %v, want TransportStdio — `python -m debugpy.adapter` "+
			"only listens on a port when given --port", a.Transport)
	}
	if a.ProgramIsDir {
		t.Error("debugpy's ProgramIsDir is true; debugpy runs a FILE, not a package directory")
	}
	if a.Locate == nil {
		t.Error("debugpy has no Locate hook, so Resolve would look for a `debugpy` binary on PATH")
	}
	if got := a.Launch["console"]; got != "internalConsole" {
		t.Errorf("debugpy console = %v, want internalConsole — any other mode hands the "+
			"debuggee our stdout, which on this transport IS the protocol stream", got)
	}

	// And the first row must not have moved.
	g, _ := AdapterFor("go")
	if g.Transport != TransportSocket || !g.ProgramIsDir {
		t.Errorf("delve = transport %v ProgramIsDir %v, want socket/true", g.Transport, g.ProgramIsDir)
	}
}

// writeFakeExtension builds a VS Code extension directory with the given
// declared licence and a bundled debugpy, returning its path.
func writeFakeExtension(t *testing.T, root, name, license string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	libs := filepath.Join(dir, "bundled", "libs", "debugpy")
	if err := os.MkdirAll(libs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(libs, "__init__.py"), []byte("\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := "{\"name\":\"debugpy\"}"
	if license != "" {
		body = "{\"name\":\"debugpy\",\"license\":" + strconvQuote(license) + "}"
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// strconvQuote JSON-quotes a string without pulling encoding/json into the test
// for one field.
func strconvQuote(s string) string { return "\"" + strings.ReplaceAll(s, "\"", "\\\"") + "\"" }

// TestVSCodeExtensionLookupRefusesNonOSILicences is the licence gate, and it is
// a real constraint rather than hygiene.
//
// 🔴 A VS Code extensions directory is full of code this MIT repo may not ship
// against. ms-python.debugpy is MIT and fair to use; ms-python.vscode-pylance
// declares "SEE LICENSE IN LICENSE.txt" and that licence restricts use to
// Microsoft products. So an unrecognised licence — including a MISSING one —
// must be DENY, not unknown-so-probably-fine: treating it as permissive defeats
// the check in exactly the case it exists for, and the failure is silent, legal,
// and shipped in a binary.
//
// The positive control is in the same test on purpose. "Nothing resolved" passes
// for a lookup that is simply broken, so the MIT copy alongside proves the
// refusal is about the licence and not about the search.
func TestVSCodeExtensionLookupRefusesNonOSILicences(t *testing.T) {
	root := t.TempDir()

	denied := map[string]string{
		"ms-python.debugpy-1.0.0-seelicense":  "SEE LICENSE IN LICENSE.txt",
		"ms-python.debugpy-1.0.1-missing":     "", // no license field at all
		"ms-python.debugpy-1.0.2-proprietary": "LicenseRef-Microsoft",
	}
	for name, lic := range denied {
		dir := writeFakeExtension(t, root, name, lic)
		if vscodeExtensionIsOSILicensed(dir) {
			t.Errorf("licence %q was accepted; only OSI identifiers may be run from an "+
				"extension directory", lic)
		}
		if got := findDebugpyInVSCodeExtensions([]string{root}); got != "" {
			t.Fatalf("findDebugpyInVSCodeExtensions resolved %q out of a %q-licensed extension",
				got, lic)
		}
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	}

	// The positive control: an MIT copy in the same place IS resolved, so the
	// three refusals above are about the licence and not about a search that
	// never finds anything.
	mit := writeFakeExtension(t, root, "ms-python.debugpy-2.0.0-mit", "MIT")
	got := findDebugpyInVSCodeExtensions([]string{root})
	want := filepath.Join(mit, "bundled", "libs")
	if got != want {
		t.Fatalf("findDebugpyInVSCodeExtensions = %q, want the MIT copy at %q", got, want)
	}

	// And a denied copy sitting NEXT to the MIT one must not win it, whichever
	// way the version sort happens to order them.
	writeFakeExtension(t, root, "ms-python.debugpy-9.9.9-seelicense", "SEE LICENSE IN LICENSE.txt")
	if got := findDebugpyInVSCodeExtensions([]string{root}); got != want {
		t.Fatalf("a later-versioned, non-OSI copy won: got %q, want %q", got, want)
	}
}

// TestVSCodeExtensionLookupIgnoresOtherExtensions checks the search is by NAME
// as well as by licence: an MIT extension that is not ms-python.debugpy must
// never be mined for a `bundled/libs/debugpy`, or the gate becomes "scan every
// extension for anything importable".
func TestVSCodeExtensionLookupIgnoresOtherExtensions(t *testing.T) {
	root := t.TempDir()
	writeFakeExtension(t, root, "some.other-extension-1.0.0", "MIT")
	if got := findDebugpyInVSCodeExtensions([]string{root}); got != "" {
		t.Fatalf("resolved %q out of an unrelated extension", got)
	}
}

// TestVSCodeExtensionLicenseReadsThePackageManifest pins where the answer comes
// from: package.json's `license` field, not a LICENSE file's contents.
//
// Reading LICENSE.txt would be the obvious "be helpful" move and is exactly
// wrong — "SEE LICENSE IN LICENSE.txt" is a manifest pointing AT a file whose
// text this code cannot adjudicate, and a licence a program cannot name is one a
// human has to look at.
func TestVSCodeExtensionLicenseReadsThePackageManifest(t *testing.T) {
	root := t.TempDir()
	dir := writeFakeExtension(t, root, "ms-python.debugpy-1.2.3", "MIT")
	if got := vscodeExtensionLicense(dir); got != "MIT" {
		t.Errorf("license = %q, want MIT", got)
	}
	// A LICENSE.txt saying MIT does NOT rescue a manifest that does not.
	if err := os.WriteFile(filepath.Join(dir, "LICENSE.txt"), []byte("MIT License\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"),
		[]byte(`{"license":"SEE LICENSE IN LICENSE.txt"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if vscodeExtensionIsOSILicensed(dir) {
		t.Error("an MIT-looking LICENSE.txt overrode a non-OSI manifest field")
	}
	if got := vscodeExtensionLicense(filepath.Join(root, "nope")); got != "" {
		t.Errorf("license of a missing directory = %q, want empty", got)
	}
}

// TestLocateDebugpyPrefersTheProjectVirtualenv pins the resolution ORDER at its
// most load-bearing point.
//
// 🔴 A debugger running under a different interpreter from the code sees a
// different set of installed packages, so a global debugpy silently winning over
// the project's own is a debug session about the wrong environment — and nothing
// on screen says which one it picked, which is why Command.Origin exists.
//
// The fake venv here is a real executable that answers `import debugpy`
// successfully, because that is the actual gate: pythonCanImportDebugpy runs the
// interpreter rather than guessing from a directory layout.
func TestLocateDebugpyPrefersTheProjectVirtualenv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the shell-script interpreter stub is POSIX-only")
	}
	root := t.TempDir()
	bin := filepath.Join(root, ".venv", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	python := filepath.Join(bin, "python")
	if err := os.WriteFile(python, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := LocateDebugpy(root)
	if cmd == nil {
		t.Fatal("LocateDebugpy found nothing with a project virtualenv that can import debugpy")
	}
	if len(cmd.Argv) < 3 || cmd.Argv[0] != python {
		t.Fatalf("argv = %v, want the project interpreter %q first", cmd.Argv, python)
	}
	if cmd.Argv[1] != "-m" || cmd.Argv[2] != "debugpy.adapter" {
		t.Errorf("argv = %v, want `-m debugpy.adapter`", cmd.Argv)
	}
	if len(cmd.Env) != 0 {
		t.Errorf("env = %v, want none: an installed debugpy needs no PYTHONPATH", cmd.Env)
	}
	if !strings.Contains(cmd.Origin, python) {
		t.Errorf("origin = %q, does not name where it came from", cmd.Origin)
	}
}

// TestLocateDebugpyNeverRunsAnInterpreterThatCannotImportIt is the other half of
// the order, and it caught the assumption it was written to check.
//
// A virtualenv whose interpreter cannot import debugpy must never be chosen ON
// ITS OWN: the adapter would exit immediately with `No module named debugpy`,
// and on the stdio transport there is no accept to fail, so the symptom is
// "connection lost" rather than "not installed". But it is entirely correct for
// that same interpreter to be chosen WITH the extension copy on PYTHONPATH —
// that combination is the best available answer, because the debuggee still runs
// under the python the project expects. So the invariant is not "never use this
// interpreter", it is "never use it without supplying debugpy".
func TestLocateDebugpyNeverRunsAnInterpreterThatCannotImportIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the shell-script interpreter stub is POSIX-only")
	}
	root := t.TempDir()
	bin := filepath.Join(root, ".venv", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	python := filepath.Join(bin, "python")
	if err := os.WriteFile(python, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := LocateDebugpy(root)
	if cmd == nil {
		return // nothing else on this machine either; nothing to assert
	}
	if cmd.Argv[0] != python {
		return // it fell through to a system interpreter, which is also correct
	}
	if len(cmd.Env) == 0 {
		t.Fatalf("chose %q with no PYTHONPATH, but it cannot import debugpy: %+v", python, cmd)
	}
	if !strings.HasPrefix(cmd.Env[0], "PYTHONPATH=") {
		t.Errorf("env = %v, want a PYTHONPATH supplying debugpy", cmd.Env)
	}
}

// TestNotInstalledErrorCarriesAHint checks the message for an adapter that is a
// LIBRARY rather than a binary. "debugpy not found on PATH" would send the user
// looking for a command that does not exist even when debugpy is installed
// correctly, so the typed error carries the real instruction.
func TestNotInstalledErrorCarriesAHint(t *testing.T) {
	e := &NotInstalledError{Name: "debugpy", Hint: "run `pip install debugpy`"}
	got := e.Error()
	if !strings.Contains(got, "debugpy is not installed") || !strings.Contains(got, "pip install debugpy") {
		t.Errorf("error = %q, want the name and the install hint", got)
	}
	if strings.Contains(got, "PATH") {
		t.Errorf("error = %q mentions PATH for a library-shaped adapter", got)
	}
	// The delve shape still reads as it did.
	if got := (&NotInstalledError{Name: "delve", Command: "dlv"}).Error(); !strings.Contains(got, "dlv not found on PATH") {
		t.Errorf("error = %q, want the binary-not-on-PATH wording", got)
	}
}

// -----------------------------------------------------------------------------
// js-debug resolution
// -----------------------------------------------------------------------------

// writeJsDebugServer creates a fake dapDebugServer.js at rel inside dir, so a
// resolution test asserts on the FILE the adapter actually needs rather than on
// a directory that happens to exist.
func writeJsDebugServer(t *testing.T, dir, rel string) string {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("// fake\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return full
}

// TestAdapterForJavaScript pins the third adapter's table entry, including the
// two flags that have no other visible effect until a session is live.
//
// 🔴 UsesChildSessions and BreakpointsBindLazily are DATA, and a session with
// either one wrong looks healthy. Without the first the editor talks to a
// coordinator that debugs nothing, so the program runs past every breakpoint;
// without the second every working breakpoint is drawn as one the adapter
// refused. Neither can be discovered at runtime, so both are asserted here.
func TestAdapterForJavaScript(t *testing.T) {
	a, ok := AdapterFor("javascript")
	if !ok {
		t.Fatal("no adapter registered for javascript")
	}
	if a.Name != "js-debug" {
		t.Fatalf("javascript resolves to %q, want js-debug", a.Name)
	}
	if a.Transport != TransportServer {
		t.Errorf("transport = %v, want TransportServer — js-debug is a server we dial, and only "+
			"that transport can carry the second connection a child session needs", a.Transport)
	}
	if !a.UsesChildSessions {
		t.Error("UsesChildSessions is false; the editor would arm the coordinator and the " +
			"program would run past every breakpoint")
	}
	if !a.BreakpointsBindLazily {
		t.Error("BreakpointsBindLazily is false; js-debug answers every setBreakpoints " +
			"unverified, so every working breakpoint would be drawn as a refused one")
	}
	if a.ProgramIsDir {
		t.Error("ProgramIsDir is true; node runs a FILE, not a directory")
	}
	if a.AdapterID != "pwa-node" {
		t.Errorf("adapterID = %q, want pwa-node", a.AdapterID)
	}
	if a.Launch["console"] != "internalConsole" {
		t.Errorf("launch console = %v, want internalConsole or the debuggee's output never "+
			"comes back as events", a.Launch["console"])
	}
	// The install hint has to say how to GET it: js-debug is a tarball, not a
	// binary on PATH, so "js-debug not found" would be unactionable on its own.
	if !strings.Contains(a.InstallHint, "vscode-js-debug") {
		t.Errorf("install hint %q does not name where to download the adapter", a.InstallHint)
	}
}

// TestJsDebugLanguagesAreOnlyWhatIsProven pins the language claim to what a live
// oracle actually drives.
//
// 🔴 A row in a table is a claim, and this fork has shipped three features whose
// only caller was a test. `pwa-node` will happily be handed a .ts file and node
// 24 can even strip the types — but nothing here proves a TypeScript breakpoint
// binds through a source map, so claiming typescript would advertise a feature
// no oracle covers. Add the language when an oracle covers it, not before.
func TestJsDebugLanguagesAreOnlyWhatIsProven(t *testing.T) {
	a, _ := AdapterFor("javascript")
	for _, lang := range a.Languages {
		switch lang {
		case "javascript", "javascriptreact":
		default:
			t.Errorf("js-debug claims language %q, which no live oracle exercises", lang)
		}
	}
	if _, ok := AdapterFor("typescript"); ok {
		t.Error("typescript resolves to a debug adapter, but no oracle proves a TypeScript " +
			"breakpoint binds through a source map — see this test's comment before adding it")
	}
}

// TestLocateJsDebugPrefersTheProjectCopy is the same project-first rule
// node_modules/.bin and the debugpy virtualenv follow: a repo that pins its own
// debugger means the pinned one.
func TestLocateJsDebugPrefersTheProjectCopy(t *testing.T) {
	if findExecutable("node") == "" {
		t.Skip("no node on this machine; js-debug cannot be resolved at all")
	}
	root := t.TempDir()
	home := t.TempDir()
	t.Setenv("XDG_DATA_HOME", home)
	writeJsDebugServer(t, home, filepath.Join("js-debug", "js-debug", "src", "dapDebugServer.js"))

	// With only the user install present, that is what answers.
	cmd := LocateJsDebug(root)
	if cmd == nil {
		t.Fatal("a user install was not found")
	}
	if !strings.Contains(cmd.Origin, "XDG_DATA_HOME") {
		t.Errorf("origin = %q, want the user install", cmd.Origin)
	}

	// Add a project copy and it must win.
	local := writeJsDebugServer(t, root, filepath.Join("node_modules", "@vscode", "js-debug", "src", "dapDebugServer.js"))
	cmd = LocateJsDebug(root)
	if cmd == nil {
		t.Fatal("a project copy was not found")
	}
	if len(cmd.Argv) < 2 || cmd.Argv[1] != local {
		t.Fatalf("argv = %v, want the project's own %s — a global copy silently beating a "+
			"pinned one is how a project's version gets bypassed with nothing on screen",
			cmd.Argv, local)
	}
}

// TestLocateJsDebugAcceptsBothTarballLayouts covers the two shapes the same
// release arrives in: extracted into ~/.local/share/js-debug (which keeps the
// tarball's own js-debug/ wrapper) and moved up a level. Probing for the FILE is
// what makes both work without this code having to be right about which the user
// chose.
func TestLocateJsDebugAcceptsBothTarballLayouts(t *testing.T) {
	if findExecutable("node") == "" {
		t.Skip("no node on this machine; js-debug cannot be resolved at all")
	}
	for _, rel := range []string{
		filepath.Join("js-debug", "js-debug", "src", "dapDebugServer.js"),
		filepath.Join("js-debug", "src", "dapDebugServer.js"),
	} {
		t.Run(rel, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("XDG_DATA_HOME", home)
			want := writeJsDebugServer(t, home, rel)
			cmd := LocateJsDebug(t.TempDir())
			if cmd == nil {
				t.Fatalf("layout %s was not found", rel)
			}
			if len(cmd.Argv) < 2 || cmd.Argv[1] != want {
				t.Fatalf("argv = %v, want to run %s", cmd.Argv, want)
			}
		})
	}
}

// TestLocateJsDebugArgvAsksForAnEphemeralPort pins the readiness contract.
//
// 🔴 Port 0 is what makes TransportServer race-free. The adapter picks a free
// port and PRINTS it, and that printed line is itself the readiness signal —
// it cannot appear before the socket can be accepted on. Choosing a port here
// instead reintroduces both halves of the race the transport removes: the port
// can be taken between the probe and the bind, and dialing early fails as
// "connection refused", which is indistinguishable from a missing adapter.
func TestLocateJsDebugArgvAsksForAnEphemeralPort(t *testing.T) {
	if findExecutable("node") == "" {
		t.Skip("no node on this machine; js-debug cannot be resolved at all")
	}
	home := t.TempDir()
	t.Setenv("XDG_DATA_HOME", home)
	writeJsDebugServer(t, home, filepath.Join("js-debug", "src", "dapDebugServer.js"))

	cmd := LocateJsDebug(t.TempDir())
	if cmd == nil {
		t.Fatal("js-debug was not resolved")
	}
	if len(cmd.Argv) != 4 {
		t.Fatalf("argv = %v, want node, the server, a port and a host", cmd.Argv)
	}
	if cmd.Argv[2] != "0" {
		t.Errorf("argv asks for port %q, want \"0\" — a chosen port can be taken between the "+
			"choice and the bind", cmd.Argv[2])
	}
	if cmd.Argv[3] != "127.0.0.1" {
		t.Errorf("argv listens on %q; the adapter must not be reachable off this machine", cmd.Argv[3])
	}
}

// TestJsDebugServerInRefusesTheVSCodeShape is the negative case, and it is
// asserted on the probe rather than on LocateJsDebug so it is HERMETIC.
//
// 🔴 Driving LocateJsDebug here would be a test of the developer's machine:
// it also searches ~/.local/share/js-debug, so a real install makes it answer
// and no amount of XDG_DATA_HOME juggling hides that. Faking HOME instead would
// make it return nil for the WRONG reason — no node — which is a vacuous pass.
//
// The shape being refused is the real one. ms-vscode.js-debug is MIT and would
// pass the licence allow-list, and its directory looks exactly right, but its
// src/ holds extension.js and no dapDebugServer.js anywhere (verified against
// VS Code 1.131.0) because that build is hosted by the extension host.
// Resolving on the directory would find it, run nothing, and report a missing
// adapter as a launch failure.
func TestJsDebugServerInRefusesTheVSCodeShape(t *testing.T) {
	dir := t.TempDir()
	writeJsDebugServer(t, dir, filepath.Join("src", "extension.js"))
	writeJsDebugServer(t, dir, filepath.Join("src", "bootloader.js"))
	if got := jsDebugServerIn(dir); got != "" {
		t.Fatalf("accepted %q out of a directory with no dapDebugServer.js", got)
	}

	// The positive control, or an implementation that never accepts anything
	// would pass the half above.
	want := writeJsDebugServer(t, dir, filepath.Join("src", "dapDebugServer.js"))
	if got := jsDebugServerIn(dir); got != want {
		t.Fatalf("jsDebugServerIn = %q, want %q", got, want)
	}

	// A DIRECTORY named like the server is not the server.
	other := t.TempDir()
	if err := os.MkdirAll(filepath.Join(other, "src", "dapDebugServer.js"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := jsDebugServerIn(other); got != "" {
		t.Fatalf("accepted the directory %q as the adapter server", got)
	}
}
