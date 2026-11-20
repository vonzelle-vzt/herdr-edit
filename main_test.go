// =============================================================================
// File: main_test.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-04-30
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudmanic/spice-edit/internal/state"
	"github.com/cloudmanic/spice-edit/internal/version"
)

// TestResolveArgs_NoArgsRootsCurrentDir keeps the no-arg path simple:
// "." as rootDir, no file to open, action = edit.
func TestResolveArgs_NoArgsRootsCurrentDir(t *testing.T) {
	got := resolveArgs(nil)
	if got.Action != actionEdit {
		t.Fatalf("action: got %q, want edit", got.Action)
	}
	if got.RootDir != "." {
		t.Fatalf("rootDir: got %q, want .", got.RootDir)
	}
	if got.OpenFile != "" {
		t.Fatalf("OpenFile should be empty, got %q", got.OpenFile)
	}
}

// TestResolveArgs_DirectoryArgUsesAsRoot pins the existing behaviour:
// passing a directory uses it as the editor's root.
func TestResolveArgs_DirectoryArgUsesAsRoot(t *testing.T) {
	dir := t.TempDir()
	got := resolveArgs([]string{dir})
	if got.Action != actionEdit {
		t.Fatalf("action: got %q", got.Action)
	}
	if got.RootDir != dir {
		t.Fatalf("rootDir: got %q, want %q", got.RootDir, dir)
	}
	if got.OpenFile != "" {
		t.Fatalf("OpenFile should be empty, got %q", got.OpenFile)
	}
}

// TestResolveArgs_FileArgRootsParent is the regression test for the
// "spiceedit main.go" bug: a file argument should root the editor at
// the file's parent and seed an OpenFile so the user's tab is ready.
func TestResolveArgs_FileArgRootsParent(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "main.go")
	if err := os.WriteFile(target, []byte("package main"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got := resolveArgs([]string{target})
	if got.Action != actionEdit {
		t.Fatalf("action: got %q", got.Action)
	}
	if got.RootDir != dir {
		t.Fatalf("rootDir: got %q, want %q", got.RootDir, dir)
	}
	if got.OpenFile != target {
		t.Fatalf("OpenFile: got %q, want %q", got.OpenFile, target)
	}
}

// TestResolveArgs_BarefilenameRootsCwd covers the common "spiceedit
// foo.go" form where the path has no directory component. The
// filepath.Dir of "foo.go" is "." — without the empty-string guard
// we'd hand the editor an empty rootDir and filetree.New would fail.
func TestResolveArgs_BarefilenameRootsCwd(t *testing.T) {
	// Use a real bare filename in a temp cwd so the stat path covers
	// the existing-file branch.
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	if err := os.WriteFile("bare.txt", []byte("x"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got := resolveArgs([]string{"bare.txt"})
	if got.RootDir != "." {
		t.Fatalf("rootDir: got %q, want .", got.RootDir)
	}
	if got.OpenFile != "bare.txt" {
		t.Fatalf("OpenFile: got %q, want bare.txt", got.OpenFile)
	}
}

// TestResolveArgs_MissingFileTreatsAsNew mirrors `vim foo.go` on a
// non-existent path: open the editor at the parent dir with the file
// queued for editing — first save creates it.
func TestResolveArgs_MissingFileTreatsAsNew(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "new.go")

	got := resolveArgs([]string{target})
	if got.Err != nil {
		t.Fatalf("missing file should not be an error, got %v", got.Err)
	}
	if got.RootDir != dir {
		t.Fatalf("rootDir: got %q, want %q", got.RootDir, dir)
	}
	if got.OpenFile != target {
		t.Fatalf("OpenFile: got %q, want %q", got.OpenFile, target)
	}
}

// TestResolveArgs_VersionFlag covers every flavour of --version we
// accept. Failing here would mean a user typing `--version` lands in
// the editor instead of seeing a printed version.
func TestResolveArgs_VersionFlag(t *testing.T) {
	for _, flag := range []string{"--version", "-v", "-V", "version"} {
		got := resolveArgs([]string{flag})
		if got.Action != actionVersion {
			t.Errorf("flag %q: action = %q, want version", flag, got.Action)
		}
	}
}

// TestResolveArgs_HelpFlag is the equivalent for --help. Like version,
// the multi-spelling list keeps the CLI forgiving.
func TestResolveArgs_HelpFlag(t *testing.T) {
	for _, flag := range []string{"--help", "-h", "help"} {
		got := resolveArgs([]string{flag})
		if got.Action != actionHelp {
			t.Errorf("flag %q: action = %q, want help", flag, got.Action)
		}
	}
}

// TestResolveArgs_OpenAt pins the flag and its error case.
func TestResolveArgs_OpenAt(t *testing.T) {
	res := resolveArgs([]string{"--open-at", "src/a.go:12"})
	if res.Action != actionOpenAt || res.OpenFile != "src/a.go:12" {
		t.Fatalf("got %+v", res)
	}
	if got := resolveArgs([]string{"--open-at"}); got.Err == nil {
		t.Error("--open-at with no argument should be an error, not a silent no-op")
	}
}

// TestResolveArgs_Debug pins the flag the Debug panel drives the editor with.
//
// Every key in that panel becomes one of these invocations, so the parse has to
// be exact in three places: the verb is accepted, an unknown verb is REFUSED
// here rather than becoming a key that silently does nothing, and
// toggle-breakpoint refuses without a location because it has nothing to toggle.
func TestResolveArgs_Debug(t *testing.T) {
	for _, action := range state.DebugActions() {
		args := []string{"--debug", action}
		if action == state.DebugActionToggleBreakpoint {
			args = append(args, "src/a.go:12")
		}
		if action == state.DebugActionTest {
			args = append(args, "/x/t.py::TestK::test_a[p1]")
		}
		got := resolveArgs(args)
		if got.Err != nil {
			t.Errorf("--debug %s: unexpected error %v", action, got.Err)
			continue
		}
		if got.Action != actionDebug || got.DebugAction != action {
			t.Errorf("--debug %s resolved to %+v", action, got)
		}
	}

	if got := resolveArgs([]string{"--debug"}); got.Err == nil {
		t.Error("--debug with no action should be an error, not a silent no-op")
	}
	if got := resolveArgs([]string{"--debug", "contnue"}); got.Err == nil {
		t.Error("a misspelled action should be reported, not written for the editor to ignore")
	}
	if got := resolveArgs([]string{"--debug", "toggle-breakpoint"}); got.Err == nil {
		t.Error("toggle-breakpoint with no location should be an error")
	}
}

// TestResolveArgsDebugTestSplitsOnlyTheFileBoundary preserves nested pytest nodes.
func TestResolveArgsDebugTestSplitsOnlyTheFileBoundary(t *testing.T) {
	got := resolveArgs([]string{"--debug", "test", "/x/t.py::TestK::test_a[p1]"})
	if got.Err != nil {
		t.Fatal(got.Err)
	}
	if got.OpenFile != "/x/t.py" || got.DebugNode != "TestK::test_a[p1]" {
		t.Fatalf("resolved to %+v", got)
	}
	if got := resolveArgs([]string{"--debug", "test", "/x/t.py"}); got.Err == nil {
		t.Fatal("test target without :: was accepted")
	}
}

// TestResolveArgs_DebugCarriesTheLocation pins that the optional location
// survives the parse untouched. It is split by state.SplitLocation in main —
// the SAME parser --open-at uses — so this only has to prove the string is
// carried, not re-implement the split.
func TestResolveArgs_DebugCarriesTheLocation(t *testing.T) {
	got := resolveArgs([]string{"--debug", state.DebugActionToggleBreakpoint, "/proj/main.go:42"})
	if got.OpenFile != "/proj/main.go:42" {
		t.Fatalf("location = %q, want it carried through verbatim", got.OpenFile)
	}
	// A verb that takes no location must not acquire one by accident.
	if plain := resolveArgs([]string{"--debug", state.DebugActionContinue}); plain.OpenFile != "" {
		t.Fatalf("continue picked up a location %q", plain.OpenFile)
	}
}

// TestHelpNamesEveryDebugAction guards the gap between a CLI that accepts a
// verb and a user who can find out it exists. The help text is the only place
// the actions are listed for a human, and a verb added to state.DebugActions()
// without a mention here is a feature nobody can discover — the same
// "implementing the request is the easy half" trap CLAUDE.md records for the
// LSP features that shipped with no call site.
//
// It reads the ACTUAL printed output rather than the source literal, so a help
// block that stops printing would fail too.
func TestHelpNamesEveryDebugAction(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	printHelp()
	os.Stdout = saved
	w.Close()

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	help := string(out)
	for _, action := range state.DebugActions() {
		if !strings.Contains(help, action) {
			t.Errorf("--help does not mention the %q action", action)
		}
	}
}

// TestHelpNamesTheRealBinary pins that usage text says herdr-edit, not the
// upstream binary name, and documents the new surface: --capabilities, start
// FILE / --config, and exit code 3.
func TestHelpNamesTheRealBinary(t *testing.T) {
	help := captureStdout(t, printHelp)
	if strings.Contains(help, "spiceedit") || strings.Contains(help, "SpiceEdit") {
		t.Errorf("--help still names the upstream binary:\n%s", help)
	}
	for _, want := range []string{"herdr-edit --capabilities", "--config NAME", "start [FILE[:LINE]]", "Exit codes", "3  the request was written"} {
		if !strings.Contains(help, want) {
			t.Errorf("--help does not mention %q", want)
		}
	}
}

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	fn()
	os.Stdout = saved
	w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// TestResolveArgs_DebugStart pins start's optional location and --config, in
// either order, and its refusals.
func TestResolveArgs_DebugStart(t *testing.T) {
	got := resolveArgs([]string{"--debug", "start", "src/a.go:12", "--config", "Launch API"})
	if got.Err != nil || got.OpenFile != "src/a.go:12" || got.DebugConfig != "Launch API" {
		t.Fatalf("got %+v", got)
	}
	got = resolveArgs([]string{"--debug", "start", "--config=Web", "a.go"})
	if got.Err != nil || got.OpenFile != "a.go" || got.DebugConfig != "Web" {
		t.Fatalf("got %+v", got)
	}
	got = resolveArgs([]string{"--debug", "start", "--config", "Only"})
	if got.Err != nil || got.OpenFile != "" || got.DebugConfig != "Only" {
		t.Fatalf("config with no file: %+v", got)
	}
	if got := resolveArgs([]string{"--debug", "start"}); got.Err != nil || got.DebugConfig != "" {
		t.Errorf("bare start changed: %+v", got)
	}
	for _, bad := range [][]string{
		{"--debug", "start", "--config"},
		{"--debug", "start", "--config="},
		{"--debug", "start", "--bogus"},
		{"--debug", "start", "a.go", "b.go"},
	} {
		if got := resolveArgs(bad); got.Err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

// TestResolveArgs_Capabilities pins the flag.
func TestResolveArgs_Capabilities(t *testing.T) {
	if got := resolveArgs([]string{"--capabilities"}); got.Action != actionCapabilities {
		t.Fatalf("got %+v", got)
	}
}

// TestCapabilitiesDocument pins the JSON contract: verbs are DERIVED from
// state.DebugActions, the booleans and schema versions are present, and
// liveEditors counts the registry.
func TestCapabilitiesDocument(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var buf strings.Builder
	if err := printCapabilities(&buf); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(buf.String()), &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, buf.String())
	}
	if doc["version"] != version.Version {
		t.Errorf("version = %v", doc["version"])
	}
	verbs, _ := doc["verbs"].([]any)
	want := state.DebugActions()
	if len(verbs) != len(want) {
		t.Fatalf("verbs = %v, want %v", verbs, want)
	}
	for i := range want {
		if verbs[i] != want[i] {
			t.Errorf("verb %d = %v, want %s", i, verbs[i], want[i])
		}
	}
	for _, k := range []string{"openAt", "startFile", "startConfig"} {
		if doc[k] != true {
			t.Errorf("%s = %v, want true", k, doc[k])
		}
	}
	if doc["capabilitiesSchema"] != float64(1) {
		t.Errorf("capabilitiesSchema = %v", doc["capabilitiesSchema"])
	}
	if doc["debugSessionSchema"] != float64(1) || doc["vizSchema"] != float64(1) {
		t.Errorf("schemas = %v / %v", doc["debugSessionSchema"], doc["vizSchema"])
	}
	if doc["liveEditors"] != float64(0) {
		t.Errorf("liveEditors = %v, want 0", doc["liveEditors"])
	}
	unreg, err := state.RegisterEditor("/p")
	if err != nil {
		t.Fatal(err)
	}
	defer unreg()
	if got := currentCapabilities().LiveEditors; got != 1 {
		t.Errorf("liveEditors = %d after registering, want 1", got)
	}
}

// TestHandoffStatusIsTruthful pins the exit-3 contract: no registered editor
// means exit 3 with the stderr explanation, a registered one means 0 and
// silence.
func TestHandoffStatusIsTruthful(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var errw strings.Builder
	if got := handoffStatus(&errw); got != exitNoEditor || exitNoEditor != 3 {
		t.Fatalf("no editor: status %d, want 3", got)
	}
	if !strings.Contains(errw.String(), "request queued, but no running editor is registered") ||
		!strings.Contains(errw.String(), "the next editor started on this machine picks it up") {
		t.Errorf("stderr = %q", errw.String())
	}
	unreg, err := state.RegisterEditor("/p")
	if err != nil {
		t.Fatal(err)
	}
	defer unreg()
	errw.Reset()
	if got := handoffStatus(&errw); got != 0 || errw.Len() != 0 {
		t.Errorf("with an editor: status %d, stderr %q", got, errw.String())
	}
}

// TestCapabilitiesTouchesNoState pins that probing a build is side-effect free:
// a doctor or an IDE calls --capabilities to decide whether to offer "Open in
// Herdr", and that probe must not create the state directory, prune the
// registry, or need any editor to be running.
func TestCapabilitiesTouchesNoState(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	if err := printCapabilities(io.Discard); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("--capabilities created %d entries under the state dir", len(entries))
	}
}

// TestHandoffExitZeroMeansQueuedNotOpened pins the semantics callers rely on:
// with a live editor registered that never consumes anything, the handoff
// still exits 0 and the request is left QUEUED on disk. Exit 0 is "queued while
// an editor was registered", never "opened" — there is no consumption ack.
func TestHandoffExitZeroMeansQueuedNotOpened(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	unreg, err := state.RegisterEditor(t.TempDir()) // registered, never polls
	if err != nil {
		t.Fatal(err)
	}
	defer unreg()
	target := filepath.Join(t.TempDir(), "x.go")
	if err := state.WriteOpenRequest(target, 3, 1); err != nil {
		t.Fatal(err)
	}
	if got := handoffStatus(io.Discard); got != 0 {
		t.Fatalf("status %d with a live editor registered, want 0", got)
	}
	req, ok := state.ReadOpenRequest()
	if !ok || req.File != target || req.Line != 3 {
		t.Fatalf("request not left queued: ok=%v %+v", ok, req)
	}
}
