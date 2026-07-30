// =============================================================================
// File: internal/app/diffview_test.go
// Author: Vonzelle Brown
// Created: 2026-07-30
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// seedRepo builds a real git repo with one committed file, then modifies it, so
// there is a genuine diff to render.
func seedRepo(t *testing.T) (dir, file string) {
	t.Helper()
	dir = t.TempDir()
	file = filepath.Join(dir, "app.go")
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(file, []byte("package main\n\nfunc main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run("init", "-q")
	run("add", "-A")
	run("commit", "-qm", "init")
	if err := os.WriteFile(file, []byte("package main\n\nfunc main() { println(1) }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	return dir, file
}

// TestGitDiffFor_ProducesUnifiedDiff pins that a real modification yields real
// diff text against both baselines.
func TestGitDiffFor_ProducesUnifiedDiff(t *testing.T) {
	_, file := seedRepo(t)
	for _, base := range []diffBaseline{baselineHead, baselineMergeBase} {
		got, err := gitDiffFor(file, base)
		if err != nil {
			t.Fatalf("baseline %v: %v", base, err)
		}
		if !strings.Contains(got, "+func main() { println(1) }") {
			t.Errorf("baseline %v produced no added line:\n%s", base, got)
		}
		if !strings.Contains(got, "-func main() {}") {
			t.Errorf("baseline %v produced no removed line", base)
		}
	}
}

// TestGitDiffFor_NotARepo pins that a path outside git errors rather than
// returning an empty diff that would read as "no changes".
func TestGitDiffFor_NotARepo(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "x.txt")
	if err := os.WriteFile(f, []byte("hi\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := gitDiffFor(f, baselineHead); err == nil {
		t.Fatal("a non-repo path should report an error, not an empty diff")
	}
}

// TestOpenChanges_OpensASyntheticDiffTab is the feature end to end.
func TestOpenChanges_OpensASyntheticDiffTab(t *testing.T) {
	dir, file := seedRepo(t)
	a := newTestApp(t, dir)
	a.openFile(file)
	before := len(a.tabs)

	a.menuOpenChanges()

	if len(a.tabs) != before+1 {
		t.Fatalf("tab count %d -> %d, want one more", before, len(a.tabs))
	}
	tab := a.activeTabPtr()
	if !tab.Synthetic {
		t.Fatal("the diff tab is not synthetic — Save would write it to disk")
	}
	if !strings.HasSuffix(tab.Label, ".diff") {
		t.Errorf("label %q must end in .diff so Chroma highlights it", tab.Label)
	}
	if !strings.Contains(tab.Buffer.String(), "println(1)") {
		t.Error("the diff tab does not contain the change")
	}
}

// TestOpenChanges_FlipsBaselineInPlace pins that a second Esc o swaps the
// baseline rather than stacking another diff tab — otherwise reading a file
// twice leaves a trail of tabs behind.
func TestOpenChanges_FlipsBaselineInPlace(t *testing.T) {
	dir, file := seedRepo(t)
	a := newTestApp(t, dir)
	a.openFile(file)

	a.menuOpenChanges()
	afterFirst := len(a.tabs)
	firstBase := a.diffBaseline

	a.menuOpenChanges()
	if len(a.tabs) != afterFirst {
		t.Fatalf("flipping the baseline added a tab: %d -> %d", afterFirst, len(a.tabs))
	}
	if a.diffBaseline == firstBase {
		t.Fatal("the baseline did not flip")
	}
}

// TestSyntheticTab_RefusesToSave is the guard that makes a generated view safe.
// Without it Save() would write the diff text over something.
func TestSyntheticTab_RefusesToSave(t *testing.T) {
	dir, file := seedRepo(t)
	a := newTestApp(t, dir)
	a.openFile(file)
	a.menuOpenChanges()

	if err := a.activeTabPtr().Save(); err == nil {
		t.Fatal("a synthetic tab must refuse to save")
	}
}

// TestOpenChanges_Reachable guards the reachability failure this fork has hit
// three times.
func TestOpenChanges_Reachable(t *testing.T) {
	if leaderActionFor('o') == nil {
		t.Error("Esc o is not bound — the diff view is unreachable")
	}
	if leaderActionFor(' ') == nil {
		t.Error("Esc space is not bound — completion is unreachable")
	}
	if leaderActionFor('c') != nil {
		t.Error("c must stay unbound: CLAUDE.md reserves c/x/v for the terminal's own clipboard")
	}
}
