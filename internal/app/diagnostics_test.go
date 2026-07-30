// =============================================================================
// File: internal/app/diagnostics_test.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-07-29
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/cloudmanic/spice-edit/internal/lsp"
)

// diag is a small constructor so the tests read as behaviour rather than as
// struct literals.
func diag(line, startCol, endCol int, sev lsp.Severity, msg string) lsp.Diagnostic {
	return lsp.Diagnostic{
		Range: lsp.Range{
			Start: lsp.Position{Line: line, Character: startCol},
			End:   lsp.Position{Line: line, Character: endCol},
		},
		Severity: sev,
		Message:  msg,
	}
}

// appWithFile builds an app with one file open, ready to receive diagnostics.
func appWithFile(t *testing.T, content string) (*App, string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	a := newTestApp(t, root)
	a.openFile(path)
	return a, path
}

// TestDiagnosticsReplaceRatherThanAccumulate pins the most important semantic in
// the protocol: a server publishes the COMPLETE list for a document every time,
// including an empty list once everything is fixed. Merging would leave repaired
// problems underlined forever.
func TestDiagnosticsReplaceRatherThanAccumulate(t *testing.T) {
	a, path := appWithFile(t, "package main\n\nfunc main() {}\n")
	uri := lsp.URI(path)

	a.handleDiagnostics(&diagnosticsEvent{when: time.Now(), uri: uri,
		diags: []lsp.Diagnostic{diag(0, 0, 4, lsp.SeverityError, "first")}})
	if got := len(a.diagnosticsFor(path)); got != 1 {
		t.Fatalf("expected 1 diagnostic, got %d", got)
	}

	a.handleDiagnostics(&diagnosticsEvent{when: time.Now(), uri: uri,
		diags: []lsp.Diagnostic{diag(1, 0, 4, lsp.SeverityWarning, "second")}})
	got := a.diagnosticsFor(path)
	if len(got) != 1 || got[0].Message != "second" {
		t.Fatalf("publish must replace, got %+v", got)
	}

	// The empty publish is how a server says "all clear".
	a.handleDiagnostics(&diagnosticsEvent{when: time.Now(), uri: uri, diags: nil})
	if got := a.diagnosticsFor(path); len(got) != 0 {
		t.Fatalf("an empty publish must clear the file, got %+v", got)
	}
}

// TestDiagnosticsIgnoreUnknownURI guards against a percent-encoding mismatch
// quietly attaching problems to a path nothing will ever look up.
func TestDiagnosticsIgnoreUnknownURI(t *testing.T) {
	a, _ := appWithFile(t, "package main\n")
	a.handleDiagnostics(&diagnosticsEvent{when: time.Now(), uri: "not-a-uri",
		diags: []lsp.Diagnostic{diag(0, 0, 1, lsp.SeverityError, "x")}})
	if len(a.diagnostics) != 0 {
		t.Fatalf("unparseable URI should be dropped, got %+v", a.diagnostics)
	}
}

// TestDiagnosticAtPrefersSeverity covers the status line: with a hint and an
// error on the same word, the error is what the user needs to read.
func TestDiagnosticAtPrefersSeverity(t *testing.T) {
	a, path := appWithFile(t, "package main\n")
	a.handleDiagnostics(&diagnosticsEvent{when: time.Now(), uri: lsp.URI(path), diags: []lsp.Diagnostic{
		diag(0, 0, 7, lsp.SeverityHint, "hint here"),
		diag(0, 0, 7, lsp.SeverityError, "real problem"),
	}})
	d := a.diagnosticAt(path, 0, 3)
	if d == nil || d.Message != "real problem" {
		t.Fatalf("expected the error to win, got %+v", d)
	}
	if d := a.diagnosticAt(path, 5, 0); d != nil {
		t.Fatalf("no diagnostic on that line, got %+v", d)
	}
}

// TestCoversHandlesZeroWidthRanges matters because servers really do emit them
// for "something is missing here", and an exclusive-End reading alone would
// match nothing at all.
func TestCoversHandlesZeroWidthRanges(t *testing.T) {
	zero := lsp.Diagnostic{Range: lsp.Range{
		Start: lsp.Position{Line: 3, Character: 8},
		End:   lsp.Position{Line: 3, Character: 8},
	}}
	if !covers(zero, 3, 8) {
		t.Error("a zero-width range must still match the cell it points at")
	}
	if covers(zero, 3, 9) {
		t.Error("zero-width range matched the wrong cell")
	}

	span := diag(2, 4, 9, lsp.SeverityError, "x")
	if !covers(span, 2, 4) || !covers(span, 2, 8) {
		t.Error("start and last-inside column must match")
	}
	if covers(span, 2, 9) {
		t.Error("End is exclusive")
	}
	if covers(span, 2, 3) {
		t.Error("before the start must not match")
	}
}

// TestDiagnosticStatusSummarises checks the fallback the status bar shows when
// the cursor is not sitting on a problem.
func TestDiagnosticStatusSummarises(t *testing.T) {
	a, path := appWithFile(t, "package main\n\nfunc main() {}\n")
	a.handleDiagnostics(&diagnosticsEvent{when: time.Now(), uri: lsp.URI(path), diags: []lsp.Diagnostic{
		diag(2, 0, 4, lsp.SeverityError, "boom"),
		diag(2, 6, 9, lsp.SeverityError, "bang"),
		diag(1, 0, 1, lsp.SeverityWarning, "meh"),
	}})

	tab := a.activeTabPtr()
	tab.Cursor.Line, tab.Cursor.Col = 0, 0 // not on any diagnostic
	if got := a.diagnosticStatus(); got != "2 errors, 1 warning" {
		t.Errorf("summary = %q", got)
	}

	tab.Cursor.Line, tab.Cursor.Col = 2, 1 // now on one
	if got := a.diagnosticStatus(); !strings.Contains(got, "boom") || !strings.HasPrefix(got, "error:") {
		t.Errorf("specific = %q", got)
	}
}

// TestDiagnosticStatusIncludesCode surfaces things like TS2304, which is what a
// user actually searches for.
func TestDiagnosticStatusIncludesCode(t *testing.T) {
	a, path := appWithFile(t, "package main\n")
	d := diag(0, 0, 7, lsp.SeverityError, "Cannot find name")
	d.Code = json.RawMessage(`2304`)
	a.handleDiagnostics(&diagnosticsEvent{when: time.Now(), uri: lsp.URI(path),
		diags: []lsp.Diagnostic{d}})
	a.activeTabPtr().Cursor.Line, a.activeTabPtr().Cursor.Col = 0, 1
	if got := a.diagnosticStatus(); !strings.Contains(got, "[2304]") {
		t.Errorf("status should carry the code, got %q", got)
	}
}

// TestOverlayUnderlinesTheRightCells is the one that proves a squiggle lands
// where the server said. It reads the rendered screen rather than trusting the
// arithmetic, because the whole class of bug here is off-by-a-gutter.
func TestOverlayUnderlinesTheRightCells(t *testing.T) {
	a, path := appWithFile(t, "package main\n\nfunc oops() {}\n")
	a.handleDiagnostics(&diagnosticsEvent{when: time.Now(), uri: lsp.URI(path),
		diags: []lsp.Diagnostic{diag(2, 5, 9, lsp.SeverityError, "undefined: oops")}})

	scr := a.screen.(tcell.SimulationScreen)
	scr.SetSize(120, 40)
	a.width, a.height = 120, 40
	a.draw()
	scr.Show()

	tab := a.activeTabPtr()
	ex, ey, _, _ := a.editorRect()
	gut := tab.GutterWidth()
	cells, w, _ := scr.GetContents()

	underlined := func(col int) bool {
		x := ex + gut + col
		y := ey + 2 // buffer line 2, no scroll
		_, _, attr := cells[y*w+x].Style.Decompose()
		return attr&tcell.AttrUnderline != 0
	}
	for col := 5; col < 9; col++ {
		if !underlined(col) {
			t.Errorf("column %d should be underlined", col)
		}
	}
	if underlined(4) || underlined(9) {
		t.Error("underline leaked outside the reported range")
	}
}

// TestOverlaySkipsScrolledOffLines guards the coordinate guard: without it a
// diagnostic above the viewport would be drawn over the tab bar.
func TestOverlaySkipsScrolledOffLines(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 200; i++ {
		b.WriteString("line of code here\n")
	}
	a, path := appWithFile(t, b.String())
	a.handleDiagnostics(&diagnosticsEvent{when: time.Now(), uri: lsp.URI(path),
		diags: []lsp.Diagnostic{diag(0, 0, 4, lsp.SeverityError, "way up top")}})

	tab := a.activeTabPtr()
	tab.ScrollY = 150 // the diagnostic is now far above the viewport

	scr := a.screen.(tcell.SimulationScreen)
	scr.SetSize(120, 40)
	a.width, a.height = 120, 40
	a.draw() // must not panic or paint outside the editor rect
	scr.Show()

	// The tab bar (row 0) must be untouched by the overlay.
	cells, w, _ := scr.GetContents()
	for x := 0; x < w; x++ {
		if _, _, attr := cells[0*w+x].Style.Decompose(); attr&tcell.AttrUnderline != 0 {
			t.Fatalf("overlay painted onto the tab bar at x=%d", x)
		}
	}
}

// TestLSPCallsAreNilSafe keeps the whole feature optional: with no server
// available every hook must be a no-op rather than a crash.
func TestLSPCallsAreNilSafe(t *testing.T) {
	a, path := appWithFile(t, "package main\n")
	a.lsp = nil
	a.lspDidOpen(path, "x")
	a.lspDidChange(path, "x")
	a.lspDidSave(path, "x")
	a.lspDidClose(path)
	a.maybeSyncLSP()
	if got := a.diagnosticStatus(); got != "" {
		t.Errorf("no server should mean no status, got %q", got)
	}
}
