// =============================================================================
// File: internal/finder/index_test.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-04-30
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

package finder

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
)

// TestBuildIndex_FallbackWalk pins the non-git path: a plain
// directory tree (no .git, no .gitignore) gets walked and every
// regular file shows up, sorted, with hardcoded ignores honoured.
func TestBuildIndex_FallbackWalk(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "main.go", "package main")
	mustWrite(t, dir, "internal/app/app.go", "package app")
	mustWrite(t, dir, "node_modules/react/index.js", "// vendored")
	mustWrite(t, dir, ".DS_Store", "junk")

	paths, viaGit, err := BuildIndex(dir)
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	if viaGit {
		t.Fatal("expected fallback path for non-git tempdir, got git")
	}
	want := []string{"internal/app/app.go", "main.go"}
	if !sliceEqual(paths, want) {
		t.Fatalf("paths: got %v, want %v", paths, want)
	}
}

// TestBuildIndex_FallbackHonoursGitignore is the .gitignore-in-
// fallback contract: even without git installed, a project root
// .gitignore should still mask out files. Without this the user
// would get build artefacts and secret env files spamming results
// in any non-git workspace.
func TestBuildIndex_FallbackHonoursGitignore(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "main.go", "package main")
	mustWrite(t, dir, "secret.env", "KEY=...")
	mustWrite(t, dir, "build/output.bin", "x")
	mustWrite(t, dir, ".gitignore", "*.env\nbuild/\n")

	paths, _, err := BuildIndex(dir)
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	want := []string{".gitignore", "main.go"}
	if !sliceEqual(paths, want) {
		t.Fatalf("paths: got %v, want %v", paths, want)
	}
}

// TestBuildIndex_GitFastPath confirms the fast path runs when the
// directory is a real git repo. We `git init` the tempdir, drop
// in two tracked-ish files, and assert the index reports `viaGit`
// true and contains both entries. If git is missing on the host
// the test skips — CI with git installed always exercises this.
func TestBuildIndex_GitFastPath(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	mustWrite(t, dir, "main.go", "package main")
	mustWrite(t, dir, "README.md", "# hi")
	mustWrite(t, dir, ".gitignore", "ignored.txt\n")
	mustWrite(t, dir, "ignored.txt", "should not appear")

	paths, viaGit, err := BuildIndex(dir)
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	if !viaGit {
		t.Fatal("expected git fast path for git repo, got fallback")
	}
	want := []string{".gitignore", "README.md", "main.go"}
	if !sliceEqual(paths, want) {
		t.Fatalf("paths: got %v, want %v", paths, want)
	}
}

// TestBuildIndex_EmptyRootRejected is a small contract guard:
// passing "" should return a useful error, not silently scan the
// caller's CWD. Otherwise a bug in the caller could blast every
// file under their home directory into the finder.
func TestBuildIndex_EmptyRootRejected(t *testing.T) {
	if _, _, err := BuildIndex(""); err == nil {
		t.Fatal("expected error for empty rootDir")
	}
}

// mustWrite is a tiny test helper that creates parent directories
// and writes content to a file under root. Pulled out so each test
// reads as the scenario it's pinning down rather than mkdir+write
// boilerplate.
func mustWrite(t *testing.T, root, rel, body string) {
	t.Helper()
	abs := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(abs, []byte(body), 0644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// sliceEqual checks two string slices for deep equality. Pulled
// into the test file so the assertion in each test reads as the
// rule it's pinning down ("got these paths, want these paths")
// instead of a reflect.DeepEqual call.
func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ac := append([]string(nil), a...)
	bc := append([]string(nil), b...)
	sort.Strings(ac)
	sort.Strings(bc)
	for i := range ac {
		if ac[i] != bc[i] {
			return false
		}
	}
	return true
}

// TestGitIndexDeduplicatesUnmergedPaths pins a bug that only appears during a
// merge — which is exactly when the conflict features are in use.
//
// 🔴 `git ls-files --cached` emits an UNMERGED path once PER STAGE: three times
// for an ordinary conflict (base, ours, theirs). So the fuzzy finder listed the
// same file three times and workspace search scanned it three times, reporting
// 39 hits in a file that has 13. It reads as a search bug and is really the
// shape of git's output.
func TestGitIndexDeduplicatesUnmergedPaths(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed; this asserts on real git output")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+root)
		_ = cmd.Run() // the merge is EXPECTED to fail
	}
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@example.com")
	run("config", "user.name", "T")
	write("base\n")
	run("add", "a.txt")
	run("commit", "-qm", "base")
	run("checkout", "-qb", "feature")
	write("theirs\n")
	run("commit", "-qam", "theirs")
	run("checkout", "-q", "main")
	write("ours\n")
	run("commit", "-qam", "ours")
	run("merge", "feature")

	paths, err := buildIndexGit(root)
	if err != nil {
		t.Fatalf("buildIndexGit: %v", err)
	}
	seen := map[string]int{}
	for _, p := range paths {
		seen[p]++
	}
	if n := seen["a.txt"]; n != 1 {
		t.Fatalf("the conflicted path appears %d time(s) in the index, want 1 — "+
			"every search over it would report %dx the real hit count", n, n)
	}
}
