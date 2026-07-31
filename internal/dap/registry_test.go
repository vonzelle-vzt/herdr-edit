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
	if len(got) != 2 || got[0] != local || got[1] != "--flag" {
		t.Fatalf("Resolve = %v, want the repo-local binary plus its args", got)
	}
}

// TestResolveReturnsNilWhenNothingIsInstalled checks the "not installed" path
// is a nil rather than a bogus argv, so Start can tell the two apart.
func TestResolveReturnsNilWhenNothingIsInstalled(t *testing.T) {
	r := NewRegistry(t.TempDir())
	if got := r.Resolve(Adapter{Argv: [][]string{{"definitely-not-a-real-binary-xyzzy"}}}); got != nil {
		t.Fatalf("Resolve = %v, want nil for a binary that does not exist", got)
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
	if _, err := StartCommand(ctx, "empty", nil, "", Handlers{}); err == nil {
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
