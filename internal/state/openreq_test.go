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
