// =============================================================================
// File: internal/app/problems_test.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudmanic/spice-edit/internal/editor"
	"github.com/cloudmanic/spice-edit/internal/lsp"
)

// TestProblemRowsAreLocationRefs is the load-bearing oracle: every
// non-header row renderProblems prints must parse via locationRefAt, the
// same parser Esc e already uses, AND resolve back to the exact problem it
// represents — not merely "some string that happens to parse".
//
// 🔴 RED against a grouped-by-filename rendering (a reasonable-looking first
// draft): a filename heading like "a.go:" has no line/col and is correctly
// skipped by this test's own row filter, but the INDENTED data rows under it
// ("  5:3: [error] msg") still contain a colon-separated numeric pair, so
// locationRefAt happily parses "5" as the path and "3" as the line — a
// syntactically valid but semantically nonsense location. A test that only
// checked "did it parse" would pass on that; checking the resolved path
// against the real problem it should represent is what actually catches it.
func TestProblemRowsAreLocationRefs(t *testing.T) {
	probs := []problemRef{
		{Path: "/proj/a.go", Line: 4, Col: 2, Sev: lsp.SeverityError, Msg: "undefined: foo"},
		{Path: "/proj/a.go", Line: 9, Col: 0, Sev: lsp.SeverityWarning, Msg: "unused import"},
		{Path: "/proj/b.go", Line: 0, Col: 1, Sev: lsp.SeverityHint, Msg: "could be const"},
	}
	body := renderProblems("/proj", probs)

	// What a correctly-parsed row must resolve to, keyed by "path:line:col"
	// in the same 1-based form locationRefAt returns. renderProblems prints
	// paths relative to root (mirrored here) when possible, same as every
	// other jump list in this fork.
	want := make(map[string]bool, len(probs))
	for _, p := range probs {
		rel := p.Path
		if r, err := filepath.Rel("/proj", p.Path); err == nil && !strings.HasPrefix(r, "..") {
			rel = r
		}
		want[fmt.Sprintf("%s:%d:%d", rel, p.Line+1, p.Col+1)] = true
	}

	var dataRows int
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		// Header lines don't name a location and must be skipped by this
		// test the same way a real reader skips them — but every line at
		// or after the blank separator is a claim about being jumpable.
		if !strings.Contains(line, ":") || strings.HasPrefix(line, "Esc") || !strings.Contains(line, "[") {
			continue
		}
		path, line0, col0, ok := locationRefAt(line)
		if !ok {
			t.Fatalf("row %q did not parse as a location ref", line)
		}
		// A grouped rendering's indented "  5:3: [error] msg" rows DO parse
		// (path="5", line=3) — this is what actually distinguishes that from
		// a real "/proj/a.go:5:3: [error] msg" row: the resolved path must
		// be one of the files a problem was actually reported against.
		got := fmt.Sprintf("%s:%d:%d", path, line0, col0)
		if !want[got] {
			t.Fatalf("row %q resolved to %q, which names no real problem (got path=%q) — a grouped rendering would produce exactly this", line, got, path)
		}
		dataRows++
	}
	if dataRows != len(probs) {
		t.Fatalf("parsed %d jumpable rows, want %d", dataRows, len(probs))
	}
}

// TestRenderProblemsStatesScopeHonestly pins the header's wording: this list
// is what the language servers have reported, never a claim about the whole
// workspace, and it carries the count.
func TestRenderProblemsStatesScopeHonestly(t *testing.T) {
	probs := []problemRef{{Path: "/proj/a.go", Line: 0, Col: 0, Sev: lsp.SeverityError, Msg: "boom"}}
	body := renderProblems("/proj", probs)
	if !strings.Contains(body, "1 problem") {
		t.Fatalf("header should carry the count, got %q", body)
	}
	if !strings.Contains(body, "language servers have reported") {
		t.Fatalf("header should state the honest scope, got %q", body)
	}
	if strings.Contains(body, "in the workspace") {
		t.Fatalf("header must not claim workspace-wide coverage, got %q", body)
	}
}

// TestNextProblemWrapsToTheFirstFile is the pure arithmetic oracle for
// stepProblem, run with no screen and no App. RED against the naive "advance
// until the next entry is past the cursor" loop, which returns "not found"
// once the cursor sits after every problem and does nothing instead of
// cycling back to the first.
func TestNextProblemWrapsToTheFirstFile(t *testing.T) {
	probs := []problemRef{
		{Path: "/proj/a.go", Line: 0, Col: 0},
		{Path: "/proj/b.go", Line: 5, Col: 0},
	}
	// Cursor sits after the last problem in the flattened order.
	idx := nextProblemIndex(probs, "/proj/z.go", 99, 0, 1)
	if idx != 0 {
		t.Fatalf("next from past-the-end = %d, want 0 (wrap to first)", idx)
	}

	// And the reverse: cursor before the first problem, stepping backward
	// must wrap to the last.
	idx = nextProblemIndex(probs, "/proj/a.go", 0, 0, -1)
	if idx != len(probs)-1 {
		t.Fatalf("prev from before-the-start = %d, want %d (wrap to last)", idx, len(probs)-1)
	}
}

// TestNextProblemAdvancesWithinBounds is the non-wrapping half of the same
// arithmetic: a cursor sitting between two problems steps to the next one
// without needing to wrap at all.
func TestNextProblemAdvancesWithinBounds(t *testing.T) {
	probs := []problemRef{
		{Path: "/proj/a.go", Line: 0, Col: 0},
		{Path: "/proj/a.go", Line: 10, Col: 0},
		{Path: "/proj/b.go", Line: 0, Col: 0},
	}
	if idx := nextProblemIndex(probs, "/proj/a.go", 0, 0, 1); idx != 1 {
		t.Fatalf("next from problem 0 = %d, want 1", idx)
	}
	if idx := nextProblemIndex(probs, "/proj/b.go", 0, 0, -1); idx != 1 {
		t.Fatalf("prev from problem 2 = %d, want 1", idx)
	}
}

// TestProblemColumnIsRuneNotUTF16 is the column-resolution oracle. A line
// with an emoji before the diagnosed identifier makes the UTF-16 offset and
// the rune offset diverge; assigning the UTF-16 value straight into the
// rune-indexed Col field (the RED behaviour) lands one column too far right.
func TestProblemColumnIsRuneNotUTF16(t *testing.T) {
	// "// 🙂 " is 6 runes but the emoji costs 2 UTF-16 units, so the server's
	// UTF-16 column for "bad" (which starts at rune index 6) is 7, not 6.
	line := "// \U0001F642 bad"
	a, path := appWithFile(t, line+"\n")
	uri := lsp.URI(path)
	utf16Col := lsp.RuneColToUTF16(line, 6)
	if utf16Col == 6 {
		t.Fatalf("fixture doesn't actually diverge: utf16Col=%d", utf16Col)
	}
	a.handleDiagnostics(&diagnosticsEvent{uri: uri, diags: []lsp.Diagnostic{
		{Range: lsp.Range{Start: lsp.Position{Line: 0, Character: utf16Col}, End: lsp.Position{Line: 0, Character: utf16Col + 3}}, Message: "bad word"},
	}})

	probs := a.allProblems()
	if len(probs) != 1 {
		t.Fatalf("expected 1 problem, got %d", len(probs))
	}
	if probs[0].Col != 6 {
		t.Fatalf("Col = %d, want 6 (rune index of \"bad\"); a UTF-16 value here would read %d", probs[0].Col, utf16Col)
	}
}

// TestProblemColumnIsZeroWithoutAnOpenTab covers the other half of the
// column contract: with no tab open for the diagnosed path, there is no line
// text to convert against, and problemColumn must report 0 rather than
// fabricate a value.
func TestProblemColumnIsZeroWithoutAnOpenTab(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	d := lsp.Diagnostic{Range: lsp.Range{Start: lsp.Position{Line: 0, Character: 12}}}
	if got := a.problemColumn("/nowhere/ghost.go", d); got != 0 {
		t.Fatalf("problemColumn with no open tab = %d, want 0", got)
	}
}

// TestHasProblemsTracksDiagnostics pins the menu-enable predicate to the
// same invariant handleDiagnostics relies on: an empty publish deletes the
// map entry, so len(a.diagnostics) alone is enough to answer "is there
// anything to show".
func TestHasProblemsTracksDiagnostics(t *testing.T) {
	a, path := appWithFile(t, "package main\n")
	if a.hasProblems() {
		t.Fatal("fresh app should report no problems")
	}
	a.handleDiagnostics(&diagnosticsEvent{uri: lsp.URI(path), diags: []lsp.Diagnostic{
		{Range: lsp.Range{Start: lsp.Position{Line: 0, Character: 0}, End: lsp.Position{Line: 0, Character: 1}}, Message: "x"},
	}})
	if !a.hasProblems() {
		t.Fatal("after a publish, hasProblems should be true")
	}
	a.handleDiagnostics(&diagnosticsEvent{uri: lsp.URI(path), diags: nil})
	if a.hasProblems() {
		t.Fatal("an empty publish should clear hasProblems")
	}
}

// TestMenuProblemsOpensAJumpableList exercises the full path end to end:
// publish a diagnostic, open the list, and confirm Esc e (menuGoToLocation)
// can actually jump from a row in it — proving the list is wired to the
// existing jump mechanism rather than a second one.
func TestMenuProblemsOpensAJumpableList(t *testing.T) {
	a, path := appWithFile(t, "package main\n\nfunc oops() {}\n")
	a.handleDiagnostics(&diagnosticsEvent{uri: lsp.URI(path), diags: []lsp.Diagnostic{
		{Range: lsp.Range{Start: lsp.Position{Line: 2, Character: 5}, End: lsp.Position{Line: 2, Character: 9}}, Message: "undefined: oops"},
	}})

	before := len(a.tabs)
	a.menuProblems()
	if len(a.tabs) != before+1 {
		t.Fatalf("menuProblems should open a new tab, got %d tabs (had %d)", len(a.tabs), before)
	}
	list := a.activeTabPtr()
	if !list.Synthetic {
		t.Fatal("problems list should be a synthetic tab")
	}
	// Land the cursor on the data row (skip the two header lines and the
	// blank separator).
	list.Cursor = editor.Position{Line: 3, Col: 0}

	a.menuGoToLocation()
	got := a.activeTabPtr()
	if got == nil || got.Path != path {
		t.Fatalf("Esc e should have opened %q, got %+v", path, got)
	}
	if got.Cursor.Line != 2 {
		t.Fatalf("cursor line = %d, want 2", got.Cursor.Line)
	}
}

// TestMenuNextProblemFlashesWhenEmpty covers the no-problems case: pressing
// next/prev with nothing published must flash rather than panic or silently
// no-op with no feedback at all.
func TestMenuNextProblemFlashesWhenEmpty(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.menuNextProblem()
	if !strings.Contains(a.statusMsg, "No problems") {
		t.Fatalf("statusMsg = %q, want a no-problems flash", a.statusMsg)
	}
	a.menuPrevProblem()
	if !strings.Contains(a.statusMsg, "No problems") {
		t.Fatalf("statusMsg = %q, want a no-problems flash", a.statusMsg)
	}
}

// TestMenuNextProblemJumpsAcrossFiles proves stepProblem actually drives the
// cursor across files, not just within one buffer: starting from the top of
// a.go (which sorts before both problems in the flattened path order —
// a.go's own problem is still ahead of the cursor), next lands on a.go's
// problem, and a second next crosses into b.go.
func TestMenuNextProblemJumpsAcrossFiles(t *testing.T) {
	dir := t.TempDir()
	a := newTestApp(t, dir)
	pathA := writeTestFile(t, dir, "a.go", "package main\n\nfunc a() {}\n")
	pathB := writeTestFile(t, dir, "b.go", "package main\n\nfunc b() {}\n")
	a.openFile(pathA)
	a.openFile(pathB)

	a.handleDiagnostics(&diagnosticsEvent{uri: lsp.URI(pathA), diags: []lsp.Diagnostic{
		{Range: lsp.Range{Start: lsp.Position{Line: 2, Character: 5}, End: lsp.Position{Line: 2, Character: 6}}, Message: "in a"},
	}})
	a.handleDiagnostics(&diagnosticsEvent{uri: lsp.URI(pathB), diags: []lsp.Diagnostic{
		{Range: lsp.Range{Start: lsp.Position{Line: 2, Character: 5}, End: lsp.Position{Line: 2, Character: 6}}, Message: "in b"},
	}})

	a.openFile(pathA)
	a.activeTabPtr().Cursor = editor.Position{Line: 0, Col: 0}
	a.menuNextProblem()
	if got := a.activeTabPtr(); got == nil || got.Path != pathA {
		t.Fatalf("expected next to land on %q, got %+v", pathA, got)
	}

	a.menuNextProblem()
	if got := a.activeTabPtr(); got == nil || got.Path != pathB {
		t.Fatalf("expected next to land on %q, got %+v", pathB, got)
	}
}

// writeTestFile is a small helper for tests needing more than one seeded
// file in the same project.
func writeTestFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("seed %s: %v", name, err)
	}
	return path
}
