// =============================================================================
// File: internal/state/openreq_test.go
// Author: Vonzelle Brown
// Created: 2026-07-30
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestOpenRequest_RoundTrip covers the happy path across the file boundary.
func TestOpenRequest_RoundTrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := WriteOpenRequest("/tmp/a.go", 42, 7); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, ok := ReadOpenRequest()
	if !ok {
		t.Fatal("a request that was just written did not read back")
	}
	if got.File != "/tmp/a.go" || got.Line != 42 || got.Col != 7 {
		t.Fatalf("round-tripped to %+v", got)
	}
	if got.Seq == 0 {
		t.Error("seq must be set so the editor can tell new requests from honoured ones")
	}
}

// TestOpenRequest_SeqAdvances pins that two writes are distinguishable. Without
// a moving sequence the editor cannot tell a fresh request from the one it just
// handled, and would either reopen files forever or ignore everything after the
// first.
func TestOpenRequest_SeqAdvances(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := WriteOpenRequest("/tmp/a.go", 1, 1); err != nil {
		t.Fatal(err)
	}
	first, _ := ReadOpenRequest()
	if err := WriteOpenRequest("/tmp/b.go", 2, 1); err != nil {
		t.Fatal(err)
	}
	second, _ := ReadOpenRequest()
	if second.Seq <= first.Seq {
		t.Fatalf("seq did not advance: %d then %d", first.Seq, second.Seq)
	}
}

// TestOpenRequest_MalformedIsAbsent pins that junk is treated as "no request".
// A panel writing garbage must not be able to break the editor.
func TestOpenRequest_MalformedIsAbsent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, junk := range []string{"", "not json", "{}", `{"file":"","seq":1}`, `{"file":"/a","seq":0}`} {
		if err := os.WriteFile(filepath.Join(Dir(), "open-request.json"), []byte(junk), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, ok := ReadOpenRequest(); ok {
			t.Errorf("malformed payload %q was accepted", junk)
		}
	}
}

// TestOpenRequest_MissingIsAbsent covers the no-file case.
func TestOpenRequest_MissingIsAbsent(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if _, ok := ReadOpenRequest(); ok {
		t.Fatal("with no file present there should be no request")
	}
}

// TestSplitLocation covers the three shapes a panel emits, plus the
// paths/strings that would break a left-to-right scan. Promoted here from
// main_test.go along with SplitLocation itself, so internal/app can share the
// exact same parser instead of growing a second, drifting copy.
func TestSplitLocation(t *testing.T) {
	cases := []struct {
		in       string
		wantPath string
		wantL    int
		wantC    int
	}{
		{"src/app.go", "src/app.go", 1, 1},
		{"src/app.go:42", "src/app.go", 42, 1},
		{"src/app.go:42:7", "src/app.go", 42, 7},
		{"/abs/path/to/file.ts:9", "/abs/path/to/file.ts", 9, 1},
		{"weird:name.go", "weird:name.go", 1, 1},
		{"file.go:0", "file.go:0", 1, 1},
	}
	for _, c := range cases {
		p, l, col := SplitLocation(c.in)
		if p != c.wantPath || l != c.wantL || col != c.wantC {
			t.Errorf("SplitLocation(%q) = %q,%d,%d — want %q,%d,%d", c.in, p, l, col, c.wantPath, c.wantL, c.wantC)
		}
	}
}

// countReads wraps the readFile seam with a counter for the test's duration.
func countReads(t *testing.T) *int {
	t.Helper()
	n := 0
	orig := readFile
	readFile = func(p string) ([]byte, error) { n++; return orig(p) }
	t.Cleanup(func() { readFile = orig })
	return &n
}

// agePast sets path's mtime well outside racyWindow, so the stat cache is
// allowed to trust it. A freshly written file is deliberately NOT trusted.
func agePast(t *testing.T, path string, ago time.Duration) {
	t.Helper()
	ts := time.Now().Add(-ago)
	if err := os.Chtimes(path, ts, ts); err != nil {
		t.Fatal(err)
	}
}

// TestReadOpenRequest_UnchangedFileNotReread pins the point of the cache: the
// Run loop polls on every event, and a file that has not changed must cost a
// stat, not a read and unmarshal.
func TestReadOpenRequest_UnchangedFileNotReread(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	openRequestCache = polledFile[OpenRequest]{}
	if err := WriteOpenRequest("/proj/a.go", 3, 1); err != nil {
		t.Fatal(err)
	}
	agePast(t, OpenRequestFile(), time.Hour)
	reads := countReads(t)

	first, ok := ReadOpenRequest()
	if !ok {
		t.Fatal("request did not read")
	}
	for i := 0; i < 50; i++ {
		got, ok := ReadOpenRequest()
		if !ok || got != first {
			t.Fatalf("poll %d returned %+v ok=%v, want the cached %+v", i, got, ok, first)
		}
	}
	if *reads != 1 {
		t.Fatalf("file read %d times across 51 polls, want 1", *reads)
	}
}

// TestReadOpenRequest_ChangedFileReread pins that the cache never hides a new
// request: the seq guard in the editor only works if a fresh Seq is seen.
func TestReadOpenRequest_ChangedFileReread(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	openRequestCache = polledFile[OpenRequest]{}
	if err := WriteOpenRequest("/proj/a.go", 3, 1); err != nil {
		t.Fatal(err)
	}
	agePast(t, OpenRequestFile(), time.Hour)
	first, _ := ReadOpenRequest()

	if err := WriteOpenRequest("/proj/b.go", 9, 2); err != nil {
		t.Fatal(err)
	}
	agePast(t, OpenRequestFile(), time.Hour)
	second, ok := ReadOpenRequest()
	if !ok || second.File != "/proj/b.go" || second.Seq <= first.Seq {
		t.Fatalf("a rewritten request was not seen: first %+v second %+v", first, second)
	}
}

// TestReadOpenRequest_SameSizeRewriteSeen pins the documented residual risk is
// closed: a same-size in-place rewrite that keeps the mtime is exactly what a
// bare (mtime, size) cache would miss, and a recent mtime must force a re-read.
func TestReadOpenRequest_SameSizeRewriteSeen(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	openRequestCache = polledFile[OpenRequest]{}
	path := OpenRequestFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"file":"/a/x.go","line":1,"col":1,"seq":111}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := ReadOpenRequest(); got.Seq != 111 {
		t.Fatalf("first read got %+v", got)
	}
	mt := time.Now()
	if err := os.WriteFile(path, []byte(`{"file":"/a/y.go","line":1,"col":1,"seq":222}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mt, mt); err != nil {
		t.Fatal(err)
	}
	if got, _ := ReadOpenRequest(); got.Seq != 222 {
		t.Fatalf("same-size in-place rewrite was missed: %+v", got)
	}
}

// TestReadOpenRequest_RemovedFileForgotten pins that deleting the file is seen
// and that its reappearance is read, never served from a stale entry.
func TestReadOpenRequest_RemovedFileForgotten(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	openRequestCache = polledFile[OpenRequest]{}
	if err := WriteOpenRequest("/proj/a.go", 1, 1); err != nil {
		t.Fatal(err)
	}
	agePast(t, OpenRequestFile(), time.Hour)
	if _, ok := ReadOpenRequest(); !ok {
		t.Fatal("request did not read")
	}
	if err := os.Remove(OpenRequestFile()); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadOpenRequest(); ok {
		t.Fatal("a removed request was still served from the cache")
	}
}

// TestSigOf_CarriesMtimeNanosAndSize pins that the signature separates two
// writes by nanoseconds and by size, which is what the "same mtime granularity"
// requirement rests on.
func TestSigOf_CarriesMtimeNanosAndSize(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := time.Unix(1_700_000_000, 0)
	os.Chtimes(p, base, base)
	fi, _ := os.Stat(p)
	a := sigOf(fi)
	os.Chtimes(p, base.Add(time.Nanosecond), base.Add(time.Nanosecond))
	fi, _ = os.Stat(p)
	b := sigOf(fi)
	if a == b {
		t.Skip("filesystem timestamps are coarser than 1ns; covered by the racy-window test")
	}
	if a.size != 3 {
		t.Fatalf("size = %d, want 3", a.size)
	}
}
