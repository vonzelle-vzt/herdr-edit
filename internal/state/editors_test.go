// =============================================================================
// File: internal/state/editors_test.go
// Author: Vonzelle Brown
// Created: 2026-11-19
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package state

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/cloudmanic/spice-edit/internal/version"
)

// TestRegisterEditorListsAndUnregisters pins the happy path: a registered
// editor is listed with its fields, and unregistering (twice, harmlessly)
// removes it.
func TestRegisterEditorListsAndUnregisters(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if n := len(LiveEditors()); n != 0 {
		t.Fatalf("fresh state dir lists %d editors", n)
	}
	unreg, err := RegisterEditor("/proj")
	if err != nil {
		t.Fatal(err)
	}
	got := LiveEditors()
	if len(got) != 1 {
		t.Fatalf("LiveEditors = %+v, want one", got)
	}
	if got[0].PID != os.Getpid() || got[0].Root != "/proj" || got[0].Version != version.Version || got[0].StartedAt == 0 {
		t.Errorf("entry = %+v", got[0])
	}
	unreg()
	unreg()
	if n := len(LiveEditors()); n != 0 {
		t.Errorf("%d editors listed after unregister", n)
	}
}

// TestLiveEditorsPrunesDeadAndGarbage pins that a SIGKILLed editor (entry left
// behind, pid gone) and a corrupt file both disappear on read, and that a
// live foreign pid survives.
func TestLiveEditorsPrunesDeadAndGarbage(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := os.MkdirAll(EditorsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	plant := func(pid int) {
		blob, _ := json.Marshal(EditorInfo{PID: pid, Root: "/x"})
		if err := os.WriteFile(editorFile(pid), blob, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dead := deadPID(t)
	plant(dead)
	plant(1) // alive, not us
	if err := os.WriteFile(EditorsDir()+"/junk.json", []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := LiveEditors()
	if len(got) != 1 || got[0].PID != 1 {
		t.Fatalf("LiveEditors = %+v, want only pid 1", got)
	}
	if _, err := os.Stat(editorFile(dead)); !os.IsNotExist(err) {
		t.Error("dead editor's entry was not pruned")
	}
	if _, err := os.Stat(EditorsDir() + "/junk.json"); !os.IsNotExist(err) {
		t.Error("corrupt entry was not pruned")
	}
}

// TestWriteDebugStartCarriesConfig pins that the start request reaches disk
// with the location and the launch.json configuration name.
func TestWriteDebugStartCarriesConfig(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := WriteDebugStart("/proj/main.go", 12, "Launch API"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(DebugRequestFile())
	if err != nil {
		t.Fatal(err)
	}
	var req DebugRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	if req.Action != DebugActionStart || req.File != "/proj/main.go" || req.Line != 12 || req.Config != "Launch API" || req.Seq == 0 {
		t.Errorf("request = %+v", req)
	}
}
