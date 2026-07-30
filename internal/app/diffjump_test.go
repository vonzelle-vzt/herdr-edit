// =============================================================================
// File: internal/app/diffjump_test.go
// Author: Vonzelle Brown
// Created: 2026-07-30
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package app

import (
	"strings"
	"testing"
)

// sampleDiff has TWO hunks and, crucially, deletions — the case that separates
// a correct mapping from counting rows.
var sampleDiff = strings.Split(`diff --git a/app.go b/app.go
index 1111111..2222222 100644
--- a/app.go
+++ b/app.go
@@ -10,6 +10,7 @@ func main() {
 context one
-removed line
+added line
+another added
 context two
 context three
@@ -40,3 +41,4 @@ func other() {
 tail context
+tail added
 tail end`, "\n")

// TestDiffLineTarget_HonoursDeletions is the whole point. A deleted line exists
// only in the OLD file and must not advance the new-file counter; counting rows
// instead drifts by one for every deletion above the cursor, which is invisible
// on a toy diff and wrong by dozens on a real one.
func TestDiffLineTarget_HonoursDeletions(t *testing.T) {
	cases := []struct {
		row  int
		want int
		what string
	}{
		{4, 10, "the hunk header points at its first line"},
		{5, 10, "first context line"},
		{6, 11, "a DELETED line maps to where it was removed from"},
		{7, 11, "first added line"},
		{8, 12, "second added line"},
		{9, 13, "context after the additions"},
		{10, 14, "last context of hunk one"},
		{11, 41, "the second hunk header restarts from its own start"},
		{12, 41, "context in hunk two"},
		{13, 42, "addition in hunk two"},
	}
	for _, c := range cases {
		got, ok := diffLineTarget(sampleDiff, c.row)
		if !ok {
			t.Errorf("row %d (%s): reported no source line", c.row, c.what)
			continue
		}
		if got != c.want {
			t.Errorf("row %d (%s): mapped to line %d, want %d", c.row, c.what, got, c.want)
		}
	}
}

// TestDiffLineTarget_RejectsPreamble pins that the header rows name no source
// line, so jumping from them says so instead of landing on line 1.
func TestDiffLineTarget_RejectsPreamble(t *testing.T) {
	for _, row := range []int{0, 1, 2, 3} {
		if _, ok := diffLineTarget(sampleDiff, row); ok {
			t.Errorf("preamble row %d claimed a source line", row)
		}
	}
}

// TestDiffLineTarget_OutOfRange covers the boundaries.
func TestDiffLineTarget_OutOfRange(t *testing.T) {
	for _, row := range []int{-1, len(sampleDiff), len(sampleDiff) + 10} {
		if _, ok := diffLineTarget(sampleDiff, row); ok {
			t.Errorf("row %d out of range claimed a source line", row)
		}
	}
}

// TestParseHunkNewStart covers the header shapes git actually emits, including
// the count-less form that a naive comma split would mis-read.
func TestParseHunkNewStart(t *testing.T) {
	cases := map[string]int{
		"@@ -10,6 +10,7 @@":              10,
		"@@ -1 +1 @@":                    1,
		"@@ -40,3 +41,4 @@ func other()": 41,
		"@@ -0,0 +1,25 @@":               1,
	}
	for header, want := range cases {
		got, ok := parseHunkNewStart(header)
		if !ok || got != want {
			t.Errorf("parseHunkNewStart(%q) = %d,%v — want %d", header, got, ok, want)
		}
	}
	for _, bad := range []string{"@@ no plus @@", "", "context line", "@@ -1,2 +x @@"} {
		if _, ok := parseHunkNewStart(bad); ok {
			t.Errorf("parseHunkNewStart(%q) should have failed", bad)
		}
	}
}

// TestDiffJump_Reachable guards this fork's signature failure and the guard that
// keeps the action honest outside a diff.
func TestDiffJump_Reachable(t *testing.T) {
	if leaderActionFor('e') == nil {
		t.Error("Esc e is not bound — jumping from a diff is unreachable")
	}
	a := newTestApp(t, t.TempDir())
	if a.hasDiffTab() {
		t.Error("hasDiffTab is true with no diff open")
	}
}

// TestDiffJump_EndToEnd opens a real diff and jumps from it.
func TestDiffJump_EndToEnd(t *testing.T) {
	dir, file := seedRepo(t)
	a := newTestApp(t, dir)
	a.openFile(file)
	a.menuOpenChanges()
	if !a.hasDiffTab() {
		t.Fatal("no diff tab opened")
	}
	diff := a.activeTabPtr()
	// Find a row the mapping accepts, then jump from it.
	row := -1
	for i := range diff.Buffer.Lines {
		if _, ok := diffLineTarget(diff.Buffer.Lines, i); ok {
			row = i
			break
		}
	}
	if row < 0 {
		t.Fatal("no mappable row in the diff")
	}
	diff.MoveCursorTo(posAtLine(row), false)
	a.menuJumpToDiffSource()

	tab := a.activeTabPtr()
	if tab == nil || tab.Path != file {
		t.Fatalf("jump did not land in the source file (got %v)", tab)
	}
	if tab.Synthetic {
		t.Fatal("still on the diff tab after jumping")
	}
}
