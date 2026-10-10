// =============================================================================
// File: internal/lsp/live_pyright_test.go
// Author: Vonzelle Brown
// Created: 2026-10-10
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// live_pyright_test.go drives the Python language server for real: the
// diagnostics, completion, hover, definition, references, signature help and
// outline the editor advertises for .py files.
//
// 🔴 WHY A SECOND LIVE ORACLE. live_gopls_test.go proves the round trip against
// gopls only. Python goes through a different server (basedpyright or pyright,
// see DefaultServers), which answers with its own payload shapes and positions.
// "Python intelligence works" was a claim backed by decoder tests alone until
// this file existed.
//
// Skips when neither server is on PATH, so CI and a fresh clone stay green.
// HERDR_REQUIRE_LSP=1 turns that skip into a failure, the same anti-skip gate
// as HERDR_REQUIRE_DAP in internal/dap: a skipped test reads as a pass.
//
//	npm install -g basedpyright

package lsp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// pyFixture is the module under test. The line numbers in the tests below are
// 0-based and were counted against this text, not guessed.
//
//	line 9:  def greet(self, punctuation: str) -> str:   "greet" spans cols 8-12
//	line 14: print(g.greet("!"))                          "g." ends at col 8
//	line 16: broken: int = "not an int"                   the deliberate type error
const pyFixture = `"""Fixture for the live pyright oracle."""


class Greeter:
    """Builds a greeting."""

    def __init__(self, name: str) -> None:
        self.name = name

    def greet(self, punctuation: str) -> str:
        return "hello " + self.name + punctuation


g = Greeter("world")
print(g.greet("!"))
print(g.greet("?"))
broken: int = "not an int"
`

// pyrightManager writes the fixture, opens it, and waits for the server's
// first diagnostics push, which is the only signal that analysis has finished.
// A slept constant here would be a guess about this machine's speed.
func pyrightManager(t *testing.T) (*Manager, string, func() []Diagnostic) {
	t.Helper()
	found := false
	for _, bin := range []string{"basedpyright-langserver", "pyright-langserver"} {
		if _, err := exec.LookPath(bin); err == nil {
			found = true
		}
	}
	if !found {
		if os.Getenv("HERDR_REQUIRE_LSP") == "1" {
			t.Fatal("HERDR_REQUIRE_LSP=1 but neither basedpyright-langserver nor pyright-langserver is on PATH")
		}
		t.Skip("no Python language server installed — skipping the live pyright test")
	}

	root := t.TempDir()
	file := filepath.Join(root, "greeter.py")
	if err := os.WriteFile(file, []byte(pyFixture), 0644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var diags []Diagnostic
	got := make(chan struct{}, 1)
	m := NewManager(root)
	m.OnDiagnostics = func(uri string, d []Diagnostic) {
		if PathFromURI(uri) != file {
			return
		}
		mu.Lock()
		diags = d
		mu.Unlock()
		select {
		case got <- struct{}{}:
		default:
		}
	}
	t.Cleanup(m.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	m.DidOpen(ctx, file, pyFixture)
	select {
	case <-got:
	case <-time.After(60 * time.Second):
		t.Fatalf("no publishDiagnostics for %s within 60s (running: %v)", file, m.Running())
	}
	snapshot := func() []Diagnostic {
		mu.Lock()
		defer mu.Unlock()
		return append([]Diagnostic(nil), diags...)
	}
	return m, file, snapshot
}

// TestLivePyright_Diagnostics is the squiggle: the deliberate str-into-int
// assignment must come back as an error on its own line.
func TestLivePyright_Diagnostics(t *testing.T) {
	_, _, diags := pyrightManager(t)
	deadline := time.Now().Add(30 * time.Second)
	for {
		for _, d := range diags() {
			if d.Range.Start.Line == 16 && d.Severity == SeverityError {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no error diagnostic on line 16; got %+v", diags())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestLivePyright_Completion proves completion after "g." offers Greeter's
// members, in whichever of the two response shapes the server chose.
func TestLivePyright_Completion(t *testing.T) {
	m, file, _ := pyrightManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	items, err := m.Completion(ctx, file, Position{Line: 14, Character: 8})
	if err != nil {
		t.Fatalf("completion: %v", err)
	}
	var labels []string
	for _, it := range items {
		labels = append(labels, it.Label)
	}
	if !contains(labels, "greet") || !contains(labels, "name") {
		t.Fatalf("completion after g. did not offer greet and name; got %d items: %v", len(labels), labels)
	}
}

// TestLivePyright_HoverAndDefinition hovers the call and jumps to the def.
func TestLivePyright_HoverAndDefinition(t *testing.T) {
	m, file, _ := pyrightManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pos := Position{Line: 14, Character: 10} // inside "greet" on the call line
	text, err := m.Hover(ctx, file, pos)
	if err != nil {
		t.Fatalf("hover: %v", err)
	}
	if !strings.Contains(text, "greet") {
		t.Errorf("hover returned %q, expected it to mention greet", text)
	}

	locs, err := m.Definition(ctx, file, pos)
	if err != nil {
		t.Fatalf("definition: %v", err)
	}
	if len(locs) == 0 {
		t.Fatal("definition returned no locations")
	}
	if got := PathFromURI(locs[0].URI); got != file {
		t.Errorf("definition pointed at %q, want %q", got, file)
	}
	if locs[0].Range.Start.Line != 9 {
		t.Errorf("definition landed on line %d, want 9 (def greet)", locs[0].Range.Start.Line)
	}
}

// TestLivePyright_References: greet is declared once and called twice.
func TestLivePyright_References(t *testing.T) {
	m, file, _ := pyrightManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	locs, err := m.References(ctx, file, Position{Line: 9, Character: 10})
	if err != nil {
		t.Fatalf("references: %v", err)
	}
	if len(locs) < 3 {
		t.Errorf("references found %d, expected at least 3 (1 declaration + 2 calls)", len(locs))
	}
}

// TestLivePyright_SignatureHelp checks the parameter list survives decoding.
func TestLivePyright_SignatureHelp(t *testing.T) {
	m, file, _ := pyrightManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sig, err := m.SignatureHelp(ctx, file, Position{Line: 14, Character: 14}) // just inside greet(
	if err != nil {
		t.Fatalf("signatureHelp: %v", err)
	}
	if !strings.Contains(sig, "punctuation") {
		t.Errorf("signature %q does not name the punctuation parameter", sig)
	}
}

// TestLivePyright_DocumentSymbol guards the flat-vs-nested shape trap: every
// symbol at line 0 means the payload was mis-decoded.
func TestLivePyright_DocumentSymbol(t *testing.T) {
	m, file, _ := pyrightManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	syms, err := m.DocumentSymbol(ctx, file)
	if err != nil {
		t.Fatalf("documentSymbol: %v", err)
	}
	var names []string
	for _, s := range syms {
		names = append(names, s.Name)
		if s.Name == "Greeter" && s.Line != 3 {
			t.Errorf("Greeter reported at line %d, want 3", s.Line)
		}
	}
	if !contains(names, "Greeter") {
		t.Fatalf("outline missing Greeter; got %v", names)
	}
}
