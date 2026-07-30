// =============================================================================
// File: internal/filetree/gitignore_test.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-07-29
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

package filetree

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// repo builds a throwaway git repository with the given files and .gitignore contents, so these
// tests exercise the real `git ls-files` path rather than a stand-in matcher — the whole point of
// shelling out to git is that it handles nested ignores and global excludes correctly, and a fake
// would not prove that.
func repo(t *testing.T, ignore string, files ...string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	for _, f := range files {
		full := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if ignore != "" {
		if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(ignore), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
	} {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return root
}

// names returns the visible child names of the tree root.
func names(tr *Tree) []string {
	out := make([]string, 0, len(tr.Root.Children))
	for _, c := range tr.Root.Children {
		out = append(out, c.Name)
	}
	return out
}

func has(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestIgnoredDirectoriesAreHidden is the headline behaviour: the tree used to list dist/, .next/
// and coverage/ because only the fuzzy finder consulted .gitignore. In a JS project that noise is
// most of what the user sees.
func TestIgnoredDirectoriesAreHidden(t *testing.T) {
	root := repo(t, "dist/\ncoverage/\n*.log\n",
		"src/main.ts", "dist/bundle.js", "coverage/lcov.info", "debug.log", "README.md")

	tr, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	got := names(tr)
	for _, gone := range []string{"dist", "coverage", "debug.log"} {
		if has(got, gone) {
			t.Errorf("%q should be hidden, tree shows %v", gone, got)
		}
	}
	for _, kept := range []string{"src", "README.md", ".gitignore"} {
		if !has(got, kept) {
			t.Errorf("%q should be visible, tree shows %v", kept, got)
		}
	}
}

// TestDirectoryVisibleOnlyIfItHoldsSomething pins the ancestor rule. git reports files, never
// directories, so a directory earns its place by containing something worth showing — which is
// also what makes a wholly-ignored directory vanish rather than appear empty.
func TestDirectoryVisibleOnlyIfItHoldsSomething(t *testing.T) {
	root := repo(t, "build/\n", "pkg/deep/nested/file.go", "build/out.o")
	tr, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if has(names(tr), "build") {
		t.Errorf("a directory with only ignored files must disappear, got %v", names(tr))
	}
	if !has(names(tr), "pkg") {
		t.Errorf("an ancestor of a visible file must be shown, got %v", names(tr))
	}
}

// TestNonRepoShowsEverything guards the degrade path. A directory that is not a git work tree must
// behave exactly as it did before this feature existed.
func TestNonRepoShowsEverything(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tr, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if tr.IgnoringGitignore() {
		t.Error("filter must stay inactive outside a git work tree")
	}
	if len(names(tr)) != 2 {
		t.Errorf("expected both files, got %v", names(tr))
	}
}

// TestRespectGitignoreOffRestoresOldBehaviour is what makes defaulting this ON safe: anyone who
// genuinely wants to browse an ignored directory has a documented way back.
func TestRespectGitignoreOffRestoresOldBehaviour(t *testing.T) {
	root := repo(t, "dist/\n", "src/a.ts", "dist/b.js")
	tr, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if has(names(tr), "dist") {
		t.Fatalf("precondition: dist should start hidden, got %v", names(tr))
	}

	tr.RespectGitignore(false)
	tr.Refresh()
	if !has(names(tr), "dist") {
		t.Errorf("turning the filter off must bring ignored paths back, got %v", names(tr))
	}
	if tr.IgnoringGitignore() {
		t.Error("IgnoringGitignore should report false once disabled")
	}
}

// TestRefreshPicksUpAnEditedGitignore matters because .gitignore is itself a file the user edits in
// this very editor. A set built once at startup would keep showing a directory they just ignored.
func TestRefreshPicksUpAnEditedGitignore(t *testing.T) {
	root := repo(t, "", "src/a.ts", "tmp/scratch.txt")
	tr, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if !has(names(tr), "tmp") {
		t.Fatalf("precondition: tmp should start visible, got %v", names(tr))
	}

	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("tmp/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr.Refresh()
	if has(names(tr), "tmp") {
		t.Errorf("Refresh must rebuild the ignore set, got %v", names(tr))
	}
}

// TestHardcodedHidesStillApply keeps the pre-existing list authoritative: .git is not listed in
// .gitignore, so the git filter alone would happily show it.
func TestHardcodedHidesStillApply(t *testing.T) {
	root := repo(t, "", "src/a.ts")
	tr, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if has(names(tr), ".git") {
		t.Errorf(".git must stay hidden regardless of the git filter, got %v", names(tr))
	}
}
