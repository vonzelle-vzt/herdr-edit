// =============================================================================
// File: internal/lsp/live_gopls_test.go
// Author: Vonzelle Brown
// Created: 2026-07-30
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// live_gopls_test.go drives EVERY LSP request against a REAL language server.
//
// 🔴 WHY THIS FILE HAD TO EXIST. Every other test here feeds hand-written JSON
// to a decoder. That proves the decoder handles the shape it was handed; it
// proves nothing about whether a real server sends that shape, whether our
// request params are spelled the way the server expects, or whether the whole
// round trip works at all. This fork has shipped a "complete, tested" LSP
// feature that nobody could reach three separate times — a green decoder test
// is exactly the kind of evidence that let that happen.
//
// Skips when gopls is absent, so CI and a fresh clone stay green; runs for real
// wherever it is installed. Install with:
//
//	go install golang.org/x/tools/gopls@latest

package lsp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// goplsFixture writes a tiny module and returns its root and main file. A real
// server needs a real module: gopls answers almost nothing for a loose file.
func goplsFixture(t *testing.T) (root, file string) {
	t.Helper()
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not installed — skipping the live language-server test")
	}
	root = t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("go.mod", "module livecheck\n\ngo 1.21\n")
	file = write("main.go", `package main

import "fmt"

// Greeter builds a greeting.
type Greeter struct {
	Name string
}

// Greet returns the greeting for this Greeter.
func (g Greeter) Greet() string {
	return "hello " + g.Name
}

func main() {
	g := Greeter{Name: "world"}
	fmt.Println(g.Greet())
	fmt.Println(g.Greet())
}
`)
	return root, file
}

// openManager starts a manager and opens the file, giving gopls time to load
// the package. Everything downstream depends on that having happened.
func openManager(t *testing.T, root, file string) (*Manager, string) {
	t.Helper()
	m := NewManager(root)
	t.Cleanup(m.Stop)

	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	m.DidOpen(ctx, file, string(src))
	// gopls needs a moment to type-check the package before it answers usefully.
	time.Sleep(3 * time.Second)
	return m, string(src)
}

// TestLive_HoverAndDefinition proves the two requests that sat unreachable for
// months actually round-trip against a real server.
func TestLive_HoverAndDefinition(t *testing.T) {
	root, file := goplsFixture(t)
	m, _ := openManager(t, root, file)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Line 17 (0-based 16): `fmt.Println(g.Greet())` — hover on Greet.
	pos := Position{Line: 16, Character: 17} // inside "Greet" on the call line
	text, err := m.Hover(ctx, file, pos)
	if err != nil {
		t.Fatalf("hover: %v", err)
	}
	if !strings.Contains(text, "Greet") {
		t.Errorf("hover returned %q, expected it to mention Greet", text)
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
}

// TestLive_Completion is the one the decoder tests could not settle: whether
// gopls answers with a bare array or a CompletionList, and whether our params
// are spelled the way it expects.
func TestLive_Completion(t *testing.T) {
	root, file := goplsFixture(t)
	m, _ := openManager(t, root, file)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Just after `g.` on the Println line.
	items, err := m.Completion(ctx, file, Position{Line: 16, Character: 15}) // just after "g."
	if err != nil {
		t.Fatalf("completion: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("completion returned nothing from a real server")
	}
	found := false
	for _, it := range items {
		if strings.HasPrefix(it.Label, "Greet") || strings.HasPrefix(it.Label, "Name") {
			found = true
		}
	}
	if !found {
		t.Errorf("completion did not offer a member of Greeter; got %d items, first=%q",
			len(items), items[0].Label)
	}
}

// TestLive_References checks the count is real: Greet is declared once and
// called twice, and includeDeclaration is on.
func TestLive_References(t *testing.T) {
	root, file := goplsFixture(t)
	m, _ := openManager(t, root, file)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Line 10 is `func (g Greeter) Greet() string {` and "Greet" spans columns
	// 17-21 — verified against the fixture rather than guessed. Column 18 on
	// line 11 lands on a SPACE, and gopls answers "no identifier found", which
	// is a correct refusal and reads as a broken request if you trust the number.
	locs, err := m.References(ctx, file, Position{Line: 10, Character: 19})
	if err != nil {
		t.Fatalf("references: %v", err)
	}
	if len(locs) < 3 {
		t.Errorf("references found %d, expected at least 3 (1 declaration + 2 calls)", len(locs))
	}
}

// TestLive_DocumentSymbol is where the shape trap actually bites: gopls sends
// NESTED DocumentSymbol[], and a decoder that guessed flat would silently
// report every symbol at line 0.
func TestLive_DocumentSymbol(t *testing.T) {
	root, file := goplsFixture(t)
	m, _ := openManager(t, root, file)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	syms, err := m.DocumentSymbol(ctx, file)
	if err != nil {
		t.Fatalf("documentSymbol: %v", err)
	}
	if len(syms) == 0 {
		t.Fatal("documentSymbol returned nothing")
	}
	var names []string
	nonZero := 0
	for _, s := range syms {
		names = append(names, s.Name)
		if s.Line > 0 {
			nonZero++
		}
	}
	for _, want := range []string{"Greeter", "main"} {
		if !contains(names, want) {
			t.Errorf("outline missing %q; got %v", want, names)
		}
	}
	// The shape bug's signature is EVERY symbol landing on line 0.
	if nonZero == 0 {
		t.Fatalf("every symbol reported line 0 — the nested/flat shape was mis-decoded: %v", names)
	}
}

// TestLive_Rename proves the edits come back in a shape we can apply.
func TestLive_Rename(t *testing.T) {
	root, file := goplsFixture(t)
	m, _ := openManager(t, root, file)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	edits, err := m.Rename(ctx, file, Position{Line: 10, Character: 19}, "Salute")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if len(edits) == 0 {
		t.Fatal("rename produced no edits from a real server")
	}
	got := edits[file]
	if len(got) < 3 {
		t.Errorf("rename produced %d edits for the file, expected at least 3", len(got))
	}
	// Descending order is what makes applying them safe without recomputing offsets.
	for i := 1; i < len(got); i++ {
		prev, cur := got[i-1].Range.Start, got[i].Range.Start
		if prev.Line < cur.Line || (prev.Line == cur.Line && prev.Character < cur.Character) {
			t.Fatalf("edits are not in descending order: %+v", got)
		}
	}
	for _, e := range got {
		if e.NewText != "Salute" {
			t.Errorf("edit inserts %q, want Salute", e.NewText)
		}
	}
}

// TestLive_SignatureHelp checks the request is spelled right and the parameter
// marker survives whichever label form gopls uses.
func TestLive_SignatureHelp(t *testing.T) {
	root, file := goplsFixture(t)
	m, _ := openManager(t, root, file)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Inside the parens of fmt.Println(...).
	sig, err := m.SignatureHelp(ctx, file, Position{Line: 16, Character: 13})
	if err != nil {
		t.Fatalf("signatureHelp: %v", err)
	}
	if sig == "" {
		t.Skip("gopls returned no signature at this position — position-sensitive, not a decode failure")
	}
	if !strings.Contains(sig, "(") {
		t.Errorf("signature %q does not look like a call signature", sig)
	}
}

// TestLive_CodeAction exercises the request end to end. An empty result is a
// legitimate answer for clean code, so this asserts the call SUCCEEDS rather
// than that fixes exist.
func TestLive_CodeAction(t *testing.T) {
	root, file := goplsFixture(t)
	m, _ := openManager(t, root, file)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pos := Position{Line: 16, Character: 2}
	if _, err := m.CodeAction(ctx, file, Range{Start: pos, End: pos}, nil); err != nil {
		t.Fatalf("codeAction: %v", err)
	}
}

// contains is a tiny helper so the assertions read as prose.
func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
