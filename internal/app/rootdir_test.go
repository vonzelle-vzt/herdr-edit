// =============================================================================
// File: internal/app/rootdir_test.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-07-29
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRootDirIsAbsolute pins down a bug that was invisible until the editor ran as a herdr pane.
//
// herdr launches a plugin pane with --cwd and NO argv argument, so main.go falls back to "." and
// the app stored that verbatim. Two things broke, one cosmetic and one not:
//
//   - the start page showed a project called "." instead of the directory name;
//   - active.json published root:"." — and every companion panel resolves that relative to ITS
//     OWN working directory, not the editor's, so a blame or markdown panel would look in the
//     wrong place entirely.
//
// Absolute is the only correct answer for a value that leaves the process.
func TestRootDirIsAbsolute(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// newTestApp mirrors New(), so assert on the same field New() populates.
	a := newTestApp(t, dir)
	if !filepath.IsAbs(a.rootDir) {
		t.Fatalf("rootDir must be absolute, got %q", a.rootDir)
	}
	if a.rootDir == "." {
		t.Fatal(`rootDir is "." — the exact value herdr passes`)
	}
}

// TestStartPageNamesTheProject is the user-visible half: the header must read as the project, not
// as a path fragment.
func TestStartPageNamesTheProject(t *testing.T) {
	dir := t.TempDir()
	a := newTestApp(t, dir)
	out := paint(t, a, 120, 40)

	want := filepath.Base(a.rootDir)
	if !strings.Contains(out, want) {
		t.Errorf("start page should name the project %q\n%s", want, out)
	}
	// The old bug rendered a lone "." on its own line as the title.
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "." {
			t.Error(`start page rendered "." as the project name`)
		}
	}
}

// TestAbsOrFallsBackRatherThanFailing covers the degrade path: an unresolvable path must still
// produce something usable rather than an empty string, which would read as "no project".
func TestAbsOrFallsBackRatherThanFailing(t *testing.T) {
	if got := absOr("relative/path"); !filepath.IsAbs(got) {
		t.Errorf("absOr should absolutise, got %q", got)
	}
	if got := absOr("/already/absolute"); got != "/already/absolute" {
		t.Errorf("absOr changed an absolute path: %q", got)
	}
	if absOr("") == "" {
		t.Error("absOr must not return an empty root")
	}
}
