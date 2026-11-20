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
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// plantEditor writes a raw registry entry for pid, beating at beat (ms).
func plantEditor(t *testing.T, pid int, beat int64) string {
	t.Helper()
	if err := os.MkdirAll(EditorsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	blob, _ := json.Marshal(EditorInfo{PID: pid, Root: "/x", LastBeat: beat})
	if err := os.WriteFile(editorFile(pid), blob, 0o644); err != nil {
		t.Fatal(err)
	}
	return editorFile(pid)
}

// TestLiveEditorsNeedsAlivePidAndFreshBeat pins the two-part liveness test: a
// dead pid is not live, and neither is a LIVE pid whose beat is stale — which is
// exactly what a crashed editor's record looks like once the OS recycles its
// pid for an unrelated process. Reading must not delete anything.
func TestLiveEditorsNeedsAlivePidAndFreshBeat(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	now := time.Now().UnixMilli()
	dead := plantEditor(t, deadPID(t), now)
	stale := plantEditor(t, 1, now-2*EditorStaleAfter.Milliseconds()) // pid 1 is alive: a "reused" pid
	self := plantEditor(t, os.Getpid(), now)

	got := LiveEditors()
	if len(got) != 1 || got[0].PID != os.Getpid() {
		t.Fatalf("LiveEditors = %+v, want only this process", got)
	}
	for _, p := range []string{dead, stale, self} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("LiveEditors removed %s; reading must leave state untouched", p)
		}
	}
}

// TestLiveEditorsIgnoresUntrustworthyFiles pins that a corrupt record, an
// oversized one and a symlink are never counted — the symlink is not even
// followed, so a planted link cannot make the CLI read an arbitrary file.
func TestLiveEditorsIgnoresUntrustworthyFiles(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := os.MkdirAll(EditorsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := EditorsDir()
	_ = os.WriteFile(filepath.Join(dir, "junk.json"), []byte("{nope"), 0o644)
	big, _ := json.Marshal(EditorInfo{PID: os.Getpid(), Root: strings.Repeat("x", maxEditorRecord), LastBeat: time.Now().UnixMilli()})
	_ = os.WriteFile(filepath.Join(dir, "big.json"), big, 0o644)
	target := filepath.Join(t.TempDir(), "real.json")
	good, _ := json.Marshal(EditorInfo{PID: os.Getpid(), Root: "/x", LastBeat: time.Now().UnixMilli()})
	_ = os.WriteFile(target, good, 0o644)
	if err := os.Symlink(target, filepath.Join(dir, "link.json")); err != nil {
		t.Fatal(err)
	}
	if got := LiveEditors(); len(got) != 0 {
		t.Fatalf("LiveEditors counted untrustworthy entries: %+v", got)
	}
}

// TestRegisterEditorPrunesCrashResidue pins where cleanup happens: an editor
// starting up removes dead, stale and corrupt entries left by crashed ones, so
// residue stays bounded even though reads never delete.
func TestRegisterEditorPrunesCrashResidue(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dead := plantEditor(t, deadPID(t), time.Now().UnixMilli())
	stale := plantEditor(t, 1, 1)
	unregister, err := RegisterEditor("/root")
	if err != nil {
		t.Fatal(err)
	}
	defer unregister()
	for _, p := range []string{dead, stale} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived registration", p)
		}
	}
	fi, err := os.Stat(editorFile(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("registry entry mode %v, want owner-only", perm)
	}
}

// TestBeatEditorKeepsAnAgedEntryLive pins the heartbeat: once the clock passes
// EditorStaleAfter the entry stops counting, and a beat brings it back. Without
// it every editor left open for a minute would look dead to the CLI.
func TestBeatEditorKeepsAnAgedEntryLive(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	base := time.Now().UnixMilli()
	clock := base
	old := nowMillis
	nowMillis = func() int64 { return clock }
	defer func() { nowMillis = old }()

	unregister, err := RegisterEditor("/root")
	if err != nil {
		t.Fatal(err)
	}
	clock = base + 2*EditorStaleAfter.Milliseconds()
	if n := len(LiveEditors()); n != 0 {
		t.Fatalf("aged entry still live (%d)", n)
	}
	if err := BeatEditor(); err != nil {
		t.Fatal(err)
	}
	if n := len(LiveEditors()); n != 1 {
		t.Fatalf("beat did not revive the entry (%d live)", n)
	}
	unregister()
	unregister() // idempotent
	if err := BeatEditor(); err != nil {
		t.Fatalf("BeatEditor after unregister: %v", err)
	}
	if n := len(LiveEditors()); n != 0 {
		t.Fatalf("beat after unregister resurrected the entry (%d)", n)
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
