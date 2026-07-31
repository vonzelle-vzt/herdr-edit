// =============================================================================
// File: internal/lsp/registry_test.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package lsp

import (
	"context"
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
