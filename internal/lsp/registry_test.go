// =============================================================================
// File: internal/lsp/registry_test.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package lsp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// startFake starts a fake server in the given helper mode and registers it
// against the manager under name, the same bookkeeping clientFor does after a
// successful Start. Used to put a client directly into m.clients without
// going through the language-detection path, since WorkspaceSymbol has no
// file argument to detect a language from.
func startFake(t *testing.T, m *Manager, name, mode string) {
	t.Helper()
	argv := withHelper(t, mode)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := Start(ctx, name, argv, URI(t.TempDir()))
	if err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	t.Cleanup(c.Stop)
	m.mu.Lock()
	m.clients[name] = c
	m.mu.Unlock()
}

// TestManager_WorkspaceSymbolFansOutAndMerges pins the one thing this method
// exists to do: unlike every other Manager method, there is no file path to
// resolve a single client from, so it must ask every RUNNING server and merge
// what comes back. Two servers, each with one match, must yield two symbols.
func TestManager_WorkspaceSymbolFansOutAndMerges(t *testing.T) {
	m := NewManager(t.TempDir())
	t.Cleanup(m.Stop)
	startFake(t, m, "fake-a", "ok")
	startFake(t, m, "fake-b", "ok")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	syms, err := m.WorkspaceSymbol(ctx, "Handler")
	if err != nil {
		t.Fatalf("WorkspaceSymbol: %v", err)
	}
	if len(syms) != 2 {
		t.Fatalf("got %d symbols from 2 servers, want 2 (one each)", len(syms))
	}
}

// TestManager_WorkspaceSymbolToleratesOneServerFailing pins that a wedged or
// erroring server does not blank out an answer another server actually gave —
// the same partial-success spirit as CodeAction and the rest of this file.
func TestManager_WorkspaceSymbolToleratesOneServerFailing(t *testing.T) {
	m := NewManager(t.TempDir())
	t.Cleanup(m.Stop)
	startFake(t, m, "fake-good", "ok")
	startFake(t, m, "fake-bad", "workspace-symbol-error")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	syms, err := m.WorkspaceSymbol(ctx, "Handler")
	if err != nil {
		t.Fatalf("WorkspaceSymbol: %v", err)
	}
	if len(syms) != 1 {
		t.Fatalf("got %d symbols, want 1 from the surviving server", len(syms))
	}
}

// TestManager_WorkspaceSymbolNoRunningServersIsEmpty pins the other half:
// with nothing running, WorkspaceSymbol must return an empty, error-free
// result rather than panicking on a nil client or an empty clients map. The
// "tell the user to open a file" UX lives in the app layer (Running() is
// checked before the prompt even opens); this is the layer below that.
func TestManager_WorkspaceSymbolNoRunningServersIsEmpty(t *testing.T) {
	m := NewManager(t.TempDir())
	t.Cleanup(m.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	syms, err := m.WorkspaceSymbol(ctx, "Handler")
	if err != nil {
		t.Fatalf("WorkspaceSymbol: %v", err)
	}
	if len(syms) != 0 {
		t.Errorf("got %d symbols with nothing running, want 0", len(syms))
	}
}

// TestResolveFindsServersOutsidePATH pins the regression that made this
// editor's headline feature invisible in the environment it was built for. A
// herdr plugin pane is exec'd by a launchd-started server, so it runs with
// PATH=/usr/bin:/bin:/usr/sbin:/sbin — measured on a live pane. exec.LookPath
// therefore cannot see ~/go/bin, ~/.cargo/bin, /opt/homebrew/bin or any npm
// prefix, and clangd was the only one of the nine DefaultServers entries that
// resolved. Diagnostics were absent for every other language with nothing on
// screen to explain it.
func TestResolveFindsServersOutsidePATH(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	// A binary that exists in a tool dir but NOT on the minimal PATH.
	probe := filepath.Join(home, "go", "bin", "gopls")
	if fi, err := os.Stat(probe); err != nil || fi.IsDir() {
		t.Skip("gopls is not installed in ~/go/bin; nothing to prove here")
	}
	t.Setenv("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")

	if _, err := exec.LookPath("gopls"); err == nil {
		t.Fatal("precondition failed: gopls is on the minimal PATH, so this test proves nothing")
	}
	m := &Manager{root: t.TempDir()}
	s, ok := serverFor("go")
	if !ok {
		t.Fatal("no server configured for go")
	}
	got := m.resolve(s)
	if got == nil {
		t.Fatal("resolve found no gopls under the minimal PATH — every LSP feature is dark in a herdr pane")
	}
	if got[0] != probe {
		t.Fatalf("resolve returned %q, want %q", got[0], probe)
	}
}
