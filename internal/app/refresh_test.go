// =============================================================================
// File: internal/app/refresh_test.go
// Author: Vonzelle Brown
// Created: 2026-11-19
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/cloudmanic/spice-edit/internal/editor"
	"github.com/cloudmanic/spice-edit/internal/finder"
)

// fakeGit stands in for runGit: it records every invocation, answers from canned output keyed by
// the git subcommand, and can hold every call on a gate so a test can prove the UI goroutine does
// not wait for git.
type fakeGit struct {
	mu    sync.Mutex
	calls []string
	gate  chan struct{} // when non-nil, every call blocks until it is closed
	reply func(args []string) ([]byte, error)
}

// installFakeGit swaps runGit for a fake for the duration of the test.
func installFakeGit(t *testing.T, f *fakeGit) {
	t.Helper()
	old := runGit
	runGit = func(dir string, args ...string) ([]byte, error) {
		f.mu.Lock()
		f.calls = append(f.calls, strings.Join(args, " "))
		gate := f.gate
		f.mu.Unlock()
		if gate != nil {
			<-gate
		}
		if f.reply != nil {
			return f.reply(args)
		}
		return nil, os.ErrNotExist
	}
	t.Cleanup(func() { runGit = old })
}

// count returns how many recorded calls begin with the given argument prefix.
func (f *fakeGit) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// repoReply builds a fakeGit reply function for a healthy repo rooted at top.
func repoReply(top string, status string, unmerged string, ls string, diff string) func([]string) ([]byte, error) {
	return func(args []string) ([]byte, error) {
		switch {
		case args[0] == "rev-parse" && args[1] == "--show-toplevel":
			return []byte(top + "\n"), nil
		case args[0] == "status":
			return []byte(status), nil
		case args[0] == "symbolic-ref":
			return []byte("feature\n"), nil
		case args[0] == "ls-files" && args[1] == "-u":
			return []byte(unmerged), nil
		case args[0] == "ls-files":
			return []byte(ls), nil
		case args[0] == "diff":
			return []byte(diff), nil
		}
		return nil, os.ErrNotExist
	}
}

// pumpRefresh waits for the posted refreshResultEvent and dispatches it through handleEvent, the
// way the Run loop would, so the selfApplyingEvent seam is exercised rather than bypassed.
func pumpRefresh(t *testing.T, a *App) {
	t.Helper()
	got := make(chan tcell.Event, 1)
	go func() {
		for {
			ev := a.screen.PollEvent()
			if ev == nil {
				return
			}
			if _, ok := ev.(*refreshResultEvent); ok {
				got <- ev
				return
			}
		}
	}()
	select {
	case ev := <-got:
		a.handleEvent(ev)
	case <-time.After(5 * time.Second):
		t.Fatal("no refreshResultEvent was posted")
	}
}

// openTabs writes n files into the app's root and opens each as a tab.
func openTabs(t *testing.T, a *App, n int) []*editor.Tab {
	t.Helper()
	var tabs []*editor.Tab
	for i := 0; i < n; i++ {
		p := filepath.Join(a.rootDir, string(rune('a'+i))+".go")
		if err := os.WriteFile(p, []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		tab, err := editor.NewTab(p)
		if err != nil {
			t.Fatal(err)
		}
		a.tabs = append(a.tabs, tab)
		tabs = append(tabs, tab)
	}
	return tabs
}

// TestUnmergedIsComputedOncePerRefresh is the regression for the per-tab `ls-files -u`: the set is
// repo-wide and identical for every tab, yet it used to be re-run (with a rev-parse) once per open
// tab. It must be exactly one run whether there are three tabs or none, on both the synchronous
// path and the background refresh.
func TestUnmergedIsComputedOncePerRefresh(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	openTabs(t, a, 3)
	f := &fakeGit{}
	f.reply = repoReply(a.rootDir, "", "", "", "")
	installFakeGit(t, f)

	a.refreshGitLineChanges()
	if got := f.count("ls-files -u"); got != 1 {
		t.Fatalf("sync path, 3 tabs: ls-files -u ran %d times, want 1", got)
	}
	if got := f.count("diff"); got != 3 {
		t.Fatalf("sync path: per-tab diff ran %d times, want 3", got)
	}

	f2 := &fakeGit{}
	f2.reply = repoReply(a.rootDir, "", "", "", "")
	installFakeGit(t, f2)
	a.refreshTreeNow()
	pumpRefresh(t, a)
	if got := f2.count("ls-files -u"); got != 1 {
		t.Fatalf("background path, 3 tabs: ls-files -u ran %d times, want 1", got)
	}

	b := newTestApp(t, t.TempDir())
	f3 := &fakeGit{}
	f3.reply = repoReply(b.rootDir, "", "", "", "")
	installFakeGit(t, f3)
	b.refreshGitLineChanges()
	if got := f3.count("ls-files -u"); got != 0 {
		t.Fatalf("no tabs: ls-files -u ran %d times, want 0", got)
	}
}

// TestRefreshTreeNowDoesNotWaitOnGit proves the UI goroutine is not held by git: with every git
// call blocked on a gate, refreshTreeNow still returns promptly, and only after the gate opens does
// the result event arrive.
func TestRefreshTreeNowDoesNotWaitOnGit(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	openTabs(t, a, 2)
	gate := make(chan struct{})
	f := &fakeGit{gate: gate}
	f.reply = repoReply(a.rootDir, "", "", "", "")
	installFakeGit(t, f)

	done := make(chan struct{})
	go func() {
		a.refreshTreeNow()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		close(gate)
		t.Fatal("refreshTreeNow blocked on git")
	}
	close(gate)
	pumpRefresh(t, a)
	if a.refresh.inFlight.Load() {
		t.Fatal("inFlight should clear once the result is applied")
	}
}

// TestRefreshResultAppliesGitState checks the posted event installs everything on the main loop:
// dirty files and branch on the tree/app, line bars and the unmerged flag on the matching tab.
func TestRefreshResultAppliesGitState(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	tabs := openTabs(t, a, 2)
	status := " M a.go\n"
	unmerged := "100644 abc 1\tb.go\x00"
	diff := "@@ -0,0 +1 @@\n+x\n"
	f := &fakeGit{}
	f.reply = repoReply(a.rootDir, status, unmerged, "", diff)
	installFakeGit(t, f)

	a.refreshTreeNow()
	pumpRefresh(t, a)

	if _, ok := a.tree.DirtyFiles[filepath.Join(a.rootDir, "a.go")]; !ok {
		t.Fatalf("a.go should be dirty, got %v", a.tree.DirtyFiles)
	}
	if a.gitBranch != "feature" {
		t.Fatalf("gitBranch: got %q, want feature", a.gitBranch)
	}
	if tabs[0].GitUnmerged || !tabs[1].GitUnmerged {
		t.Fatalf("GitUnmerged: a=%v b=%v, want false/true", tabs[0].GitUnmerged, tabs[1].GitUnmerged)
	}
	if tabs[0].GitLines[0] != editor.GitLineAdded {
		t.Fatalf("a.go line bar: got %v", tabs[0].GitLines)
	}
}

// TestRefreshResultSkipsClosedAndRenamedTabs pins the staleness guards: a tab closed (or whose
// path changed) while the job ran must not be written to when the result lands.
func TestRefreshResultSkipsClosedAndRenamedTabs(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	tabs := openTabs(t, a, 2)
	gate := make(chan struct{})
	f := &fakeGit{gate: gate}
	f.reply = repoReply(a.rootDir, "", "", "", "@@ -0,0 +1 @@\n+x\n")
	installFakeGit(t, f)

	a.refreshTreeNow()
	a.tabs = a.tabs[1:] // close a.go while the job is running
	tabs[1].Path = filepath.Join(a.rootDir, "renamed.go")
	close(gate)
	pumpRefresh(t, a)

	if tabs[0].GitLines != nil {
		t.Fatal("a closed tab must not receive results")
	}
	if tabs[1].GitLines != nil {
		t.Fatal("a tab whose path changed must not receive results")
	}
}

// TestRefreshOverlapIsCoalesced pins the in-flight guard: ticks that arrive while a refresh is
// running do not stack jobs; they collapse into exactly one catch-up run after it lands.
func TestRefreshOverlapIsCoalesced(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	gate := make(chan struct{})
	f := &fakeGit{gate: gate}
	f.reply = repoReply(a.rootDir, "", "", "", "")
	installFakeGit(t, f)

	a.refreshTreeNow()
	a.refreshTreeNow()
	a.refreshTreeNow()
	close(gate)
	pumpRefresh(t, a) // first job; its apply starts the single catch-up
	pumpRefresh(t, a) // the catch-up
	if got := f.count("status"); got != 2 {
		t.Fatalf("status ran %d times for 3 ticks, want 2 (one job plus one coalesced catch-up)", got)
	}
}

// TestRefreshSharesLsFilesWithFinder pins the shared exec: with a finder attached the refresh runs
// `ls-files --cached` once and the finder is ready from that output without its own walk or exec.
func TestRefreshSharesLsFilesWithFinder(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.finder = finder.New(a.rootDir)
	a.tree.RespectGitignore(false)
	f := &fakeGit{}
	f.reply = repoReply(a.rootDir, "", "", "z.go\x00a.go\x00", "")
	installFakeGit(t, f)

	a.refreshTreeNow()
	pumpRefresh(t, a)

	if got := f.count("ls-files --cached"); got != 1 {
		t.Fatalf("ls-files --cached ran %d times, want 1", got)
	}
	state, total, viaGit := a.finder.Stats()
	if state != finder.StateReady || total != 2 || !viaGit {
		t.Fatalf("finder Stats: got (%v, %d, %v), want (Ready, 2, true)", state, total, viaGit)
	}
	if got := a.finder.Paths(); len(got) != 2 || got[0] != "a.go" {
		t.Fatalf("finder paths: got %v", got)
	}
}

// TestRefreshSwapsIgnoreSetAndRefilters is the end-to-end tree case against a real repo: a file
// created after startup is hidden by the old ignore set during the synchronous reload, and shows
// up once the background result swaps in the new set and re-filters.
func TestRefreshSwapsIgnoreSetAndRefilters(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "keep.go"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	a := newTestApp(t, dir)
	if !a.tree.IgnoringGitignore() {
		t.Fatal("precondition: filter should be active")
	}
	if err := os.WriteFile(filepath.Join(dir, "fresh.go"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	a.refreshTreeNow()
	if treeHas(a, "fresh.go") {
		t.Fatal("fresh.go must not appear before the background result lands")
	}
	pumpRefresh(t, a)
	if !treeHas(a, "fresh.go") {
		t.Fatal("fresh.go should appear after the ignore set is swapped in")
	}
}

// treeHas reports whether the tree root currently lists a child with this name.
func treeHas(a *App, name string) bool {
	for _, c := range a.tree.Root.Children {
		if c.Name == name {
			return true
		}
	}
	return false
}

// TestRefreshWithoutScreenAppliesInline covers a bare App with no event queue: the refresh must
// still complete (inline) rather than spawn a goroutine with nowhere to post.
func TestRefreshWithoutScreenAppliesInline(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	tabs := openTabs(t, a, 1)
	a.screen = nil
	f := &fakeGit{}
	f.reply = repoReply(a.rootDir, "", "100644 abc 1\ta.go\x00", "", "")
	installFakeGit(t, f)

	a.startGitRefresh()
	if !tabs[0].GitUnmerged {
		t.Fatal("inline refresh should have applied the unmerged flag")
	}
	if a.refresh.inFlight.Load() {
		t.Fatal("inFlight must be clear after an inline refresh")
	}
}
