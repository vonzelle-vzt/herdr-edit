// =============================================================================
// File: main_test.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-04-30
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudmanic/spice-edit/internal/state"
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
