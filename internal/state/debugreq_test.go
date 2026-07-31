// =============================================================================
// File: internal/state/debugreq_test.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package state

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDebugRequest_RoundTrip covers the happy path across the file boundary,
// for the one action that carries a location as well as a verb.
func TestDebugRequest_RoundTrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := WriteDebugRequest(DebugActionToggleBreakpoint, "/proj/main.go", 42); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, ok := ReadDebugRequest()
	if !ok {
		t.Fatal("a request that was just written did not read back")
	}
	if got.Action != DebugActionToggleBreakpoint || got.File != "/proj/main.go" || got.Line != 42 {
		t.Fatalf("round-tripped to %+v", got)
	}
	if got.Seq == 0 {
		t.Error("seq must be set so the editor can tell new requests from honoured ones")
	}
}

// TestDebugRequest_SeqAdvances pins that two writes are distinguishable.
// Without a moving sequence the editor either replays the last request on every
// poll — stepping the program forever — or ignores everything after the first.
func TestDebugRequest_SeqAdvances(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := WriteDebugRequest(DebugActionNext, "", 0); err != nil {
		t.Fatal(err)
	}
	first, _ := ReadDebugRequest()
	if err := WriteDebugRequest(DebugActionNext, "", 0); err != nil {
		t.Fatal(err)
	}
	second, _ := ReadDebugRequest()
	if second.Seq <= first.Seq {
		t.Fatalf("seq did not advance: %d then %d", first.Seq, second.Seq)
	}
}

// TestDebugRequest_MissingIsAbsent covers the no-file case: a panel that has
// never been used is not an error condition.
func TestDebugRequest_MissingIsAbsent(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if _, ok := ReadDebugRequest(); ok {
		t.Fatal("with no file present there should be no request")
	}
}

// TestDebugRequest_MalformedIsAbsent pins that junk reads as "no request"
// rather than as an error the editor has to handle.
//
// The unknown-action case is the one that matters most: the editor switches on
// the verb, so an unrecognised one would either fall through silently (leaving
// the sequence unadvanced and the same junk re-read on every poll) or need a
// complaint path on the editor's hot loop. Rejecting it here means neither.
func TestDebugRequest_MalformedIsAbsent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	junk := []string{
		"",
		"not json",
		"{",
		"{}",
		`{"action":"start"}`,                     // no sequence
		`{"action":"start","seq":0}`,             // explicit zero sequence
		`{"action":"rm -rf /","seq":1}`,          // unknown action
		`{"action":"","seq":1}`,                  // empty action
		`{"action":"toggle-breakpoint","seq":1}`, // needs a file
		`{"action":"toggle-breakpoint","file":"","seq":1}`,
	}
	for _, j := range junk {
		if err := os.WriteFile(DebugRequestFile(), []byte(j), 0o644); err != nil {
			t.Fatal(err)
		}
		if got, ok := ReadDebugRequest(); ok {
			t.Errorf("malformed payload %q was accepted as %+v", j, got)
		}
	}
}

// TestDebugRequest_WriteRefusesJunk pins the other end of the same rule: the
// CLI refuses an action the editor would not honour, so a typo is reported to
// the person who made it instead of becoming a key that silently does nothing.
func TestDebugRequest_WriteRefusesJunk(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := WriteDebugRequest("contnue", "", 0); err == nil {
		t.Error("a misspelled action was accepted")
	}
	if err := WriteDebugRequest(DebugActionToggleBreakpoint, "", 0); err == nil {
		t.Error("toggle-breakpoint with no file was accepted, and has nothing to toggle")
	}
	if _, err := os.Stat(DebugRequestFile()); !os.IsNotExist(err) {
		t.Errorf("a refused request still wrote a file: %v", err)
	}
}

// TestDebugRequest_EveryActionIsAccepted walks the published action list and
// proves each one round-trips. It reads DebugActions() rather than restating
// the eight verbs, so an action added to the list without being wired up here
// cannot pass by being forgotten.
func TestDebugRequest_EveryActionIsAccepted(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, action := range DebugActions() {
		file := ""
		if action == DebugActionToggleBreakpoint {
			file = "/proj/main.go"
		}
		if err := WriteDebugRequest(action, file, 1); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		got, ok := ReadDebugRequest()
		if !ok || got.Action != action {
			t.Fatalf("%s did not round-trip (got %+v, ok=%v)", action, got, ok)
		}
		if !ValidDebugAction(action) {
			t.Errorf("%s is in DebugActions() but ValidDebugAction rejects it", action)
		}
	}
}

// TestDebugRequest_LeavesNoTempFiles pins the atomic-write discipline: the temp
// file is a sibling of the target so the rename is atomic, and it is always
// renamed away rather than accumulating in the state directory.
func TestDebugRequest_LeavesNoTempFiles(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for i := 0; i < 5; i++ {
		if err := WriteDebugRequest(DebugActionContinue, "", 0); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(Dir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(DebugRequestFile()) {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("state directory holds %v, want just debug-request.json", names)
	}
}
