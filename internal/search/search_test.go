// =============================================================================
// File: internal/search/search_test.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package search

import (
	"os"
	"path/filepath"
	"testing"
)

// mustWrite creates a file (and any parent directories) with the given
// content, failing the test on any error. Mirrors internal/finder's own test
// helper so fixtures read the same way across both packages.
func mustWrite(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", full, err)
	}
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
}

// TestRunFindsPlainTextMatch is the happy path: one file, one match, every
// Hit field populated the way the doc comment promises.
func TestRunFindsPlainTextMatch(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "greet.go", "package greet\n\nfunc Hello() string {\n\treturn \"hi\"\n}\n")

	res := Run(dir, []string{"greet.go"}, Options{Query: "Hello"})
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("hits = %d, want 1: %+v", len(res.Hits), res.Hits)
	}
	h := res.Hits[0]
	if h.Path != "greet.go" || h.Line != 2 || h.Col != 5 || h.Width != 5 {
		t.Fatalf("hit = %+v, want {greet.go 2 5 5 ...}", h)
	}
	if h.Text != "func Hello() string {" {
		t.Fatalf("hit.Text = %q, want the full source line", h.Text)
	}
	if res.Scanned != 1 {
		t.Fatalf("scanned = %d, want 1", res.Scanned)
	}
}

// TestRunReportsRuneColumnsOnMultibyteLines pins the whole reason this
// package delegates to editor.Matches instead of a byte-oriented scan: a
// multi-byte rune before the match must not shift the reported column.
//
// The fixture line is "// 🚀 findBar here". Counted in RUNES, "findBar"
// starts at index 5 (/ / space rocket space). A strings.Index-based
// implementation would report a BYTE offset instead — 🚀 is 4 bytes in
// UTF-8, 1 rune — landing the column 3 cells too far right and putting any
// jump-to-match on the wrong character. Confirmed RED against exactly that
// implementation before writing the real one (see the builder's report).
func TestRunReportsRuneColumnsOnMultibyteLines(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "rocket.go", "// 🚀 findBar here\n")

	res := Run(dir, []string{"rocket.go"}, Options{Query: "findBar"})
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("hits = %d, want 1: %+v", len(res.Hits), res.Hits)
	}
	const wantCol = 5
	if res.Hits[0].Col != wantCol {
		t.Fatalf("Col = %d, want %d (rune index, not byte offset)", res.Hits[0].Col, wantCol)
	}
}

// TestRunSurfacesABadRegexAsAnError pins Result.Err as a field distinct from
// an empty Hits slice. An implementation that swallows the compile error
// (e.g. `matches, _ := editor.Matches(...)`) would report zero hits — which
// is indistinguishable from a valid pattern that legitimately matched
// nothing. Confirmed RED against exactly that swallowed-error shape before
// writing the real one.
func TestRunSurfacesABadRegexAsAnError(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "a.go", "package a\n")

	res := Run(dir, []string{"a.go"}, Options{Query: "(unclosed", Regex: true})
	if res.Err == nil {
		t.Fatal("bad regex: Err is nil, want a compile error")
	}
	if len(res.Hits) != 0 {
		t.Fatalf("bad regex: Hits = %v, want empty", res.Hits)
	}
}

// TestRunSkipsBinaryFiles seeds a NUL-containing file alongside a real text
// match and checks the binary file contributes neither a hit nor a scanned
// count, while the text file is still found normally.
func TestRunSkipsBinaryFiles(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "readme.txt", "needle in the haystack\n")
	binPath := filepath.Join(dir, "blob.bin")
	if err := os.WriteFile(binPath, []byte("needle\x00\x01\x02binary"), 0644); err != nil {
		t.Fatalf("write binary fixture: %v", err)
	}

	res := Run(dir, []string{"readme.txt", "blob.bin"}, Options{Query: "needle"})
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("hits = %d, want 1 (binary file must be skipped): %+v", len(res.Hits), res.Hits)
	}
	if res.Hits[0].Path != "readme.txt" {
		t.Fatalf("hit path = %q, want readme.txt", res.Hits[0].Path)
	}
	if res.Scanned != 1 {
		t.Fatalf("scanned = %d, want 1 (blob.bin must not count as scanned)", res.Scanned)
	}
	if res.Files != 2 {
		t.Fatalf("files = %d, want 2 (the candidate count, before skipping)", res.Files)
	}
}

// TestRunSkipsFilesOverMaxFileBytes checks the size guard independently of
// the binary guard: a plain-text file that's simply too large must be
// skipped without ever being read into memory.
func TestRunSkipsFilesOverMaxFileBytes(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "huge.txt", "needle plus padding\n")
	mustWrite(t, dir, "small.txt", "needle\n")

	res := Run(dir, []string{"huge.txt", "small.txt"}, Options{Query: "needle", MaxFileBytes: 10})
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if len(res.Hits) != 1 || res.Hits[0].Path != "small.txt" {
		t.Fatalf("hits = %+v, want exactly small.txt", res.Hits)
	}
}

// TestRunTruncatesAtMaxHits pins the early-stop contract: once MaxHits is
// reached, Run stops scanning and reports Truncated rather than silently
// returning a partial result that looks complete.
func TestRunTruncatesAtMaxHits(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "a.txt", "needle needle needle\n")
	mustWrite(t, dir, "b.txt", "needle needle needle\n")

	res := Run(dir, []string{"a.txt", "b.txt"}, Options{Query: "needle", MaxHits: 3})
	if !res.Truncated {
		t.Fatal("expected Truncated=true when MaxHits is reached")
	}
	if len(res.Hits) != 3 {
		t.Fatalf("hits = %d, want exactly MaxHits (3)", len(res.Hits))
	}
}

// TestRunEmptyQueryIsANoOp mirrors editor.Matches' own contract: an empty
// query returns a zero Result rather than treating "" as "match everything"
// (which would make every line of every file a hit).
func TestRunEmptyQueryIsANoOp(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "a.txt", "anything\n")

	res := Run(dir, []string{"a.txt"}, Options{Query: ""})
	if res.Err != nil || len(res.Hits) != 0 || res.Scanned != 0 {
		t.Fatalf("empty query: got %+v, want a no-op zero result", res)
	}
}

// TestRunSkipsMissingFiles ensures a stale path (present in the file list
// but no longer on disk — e.g. deleted between indexing and scanning)
// doesn't abort the whole scan.
func TestRunSkipsMissingFiles(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "here.txt", "needle\n")

	res := Run(dir, []string{"gone.txt", "here.txt"}, Options{Query: "needle"})
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if len(res.Hits) != 1 || res.Hits[0].Path != "here.txt" {
		t.Fatalf("hits = %+v, want exactly here.txt", res.Hits)
	}
}
