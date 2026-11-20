// =============================================================================
// File: internal/app/refresh.go
// Author: Vonzelle Brown
// Created: 2026-11-19
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// refresh.go is the git half of the 10-second refresh, moved off the UI goroutine.
//
// refreshTreeNow used to run everything inline: `git ls-files` for the tree's ignore filter, the
// status/branch commands, a `git diff` plus a repo-wide `ls-files -u` for EVERY open tab, and then
// a second `git ls-files` for the finder. On a large repo that is hundreds of milliseconds of the
// event loop doing nothing but waiting on subprocesses, every 10 seconds, with no way to type.
//
// Now the UI goroutine only does the cheap local work (directory reload, mtime stats) and starts a
// job here. The job runs every git command in a goroutine and posts ONE refreshResultEvent, which
// installs the results on the main loop via the selfApplyingEvent seam — nothing the job computes
// touches UI state, per the project's rule on goroutine -> main-loop messaging.
//
// Two things are shared that used to be repeated: the repo-wide unmerged set is computed once per
// refresh (it is identical for every tab), and the single `ls-files` output feeds both the tree's
// ignore filter and the finder index.

package app

import (
	"os/exec"
	"sync/atomic"
	"time"

	"github.com/cloudmanic/spice-edit/internal/editor"
	"github.com/cloudmanic/spice-edit/internal/filetree"
	"github.com/cloudmanic/spice-edit/internal/finder"
)

// runGit runs `git -C dir args...` and returns its stdout. It is a variable so tests can count and
// block git invocations without a real repository; every git exec in this package goes through it.
var runGit = func(dir string, args ...string) ([]byte, error) {
	return exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
}

// refreshState is the overlap guard for the background git refresh. inFlight is atomic because the
// job goroutine clears it when it cannot even post its result; pending is touched only by the UI
// goroutine and records that a tick arrived mid-flight, so exactly one catch-up run follows.
type refreshState struct {
	inFlight atomic.Bool
	pending  bool
}

// refreshTabSnap pairs an open tab with the path it had when the job started. The job goroutine
// only ever reads the copied path (never the Tab), and apply re-checks both so a tab closed or
// renamed in the meantime is skipped rather than written to.
type refreshTabSnap struct {
	tab  *editor.Tab
	path string
}

// refreshResultEvent carries everything the background job computed, plus the snapshot it was
// computed against. It implements selfApplyingEvent: apply installs it on the main loop.
type refreshResultEvent struct {
	when time.Time

	tree     *filetree.Tree
	rootDir  string
	treeRoot string
	tabs     []refreshTabSnap

	status   gitStatus
	snap     *filetree.IgnoreSnapshot
	lsOK     bool
	paths    []string
	unmerged map[string]bool
	lines    []map[int]editor.GitLineChange
}

// When satisfies the tcell.Event interface.
func (e *refreshResultEvent) When() time.Time { return e.when }

// startGitRefresh starts the background git refresh, or, if one is already running, records that
// another is wanted so a slow repo cannot stack up jobs. Must run on the UI goroutine.
func (a *App) startGitRefresh() {
	if !a.refresh.inFlight.CompareAndSwap(false, true) {
		a.refresh.pending = true
		return
	}
	job := &refreshResultEvent{
		tree:    a.tree,
		rootDir: a.rootDir,
	}
	if a.tree != nil {
		job.treeRoot = a.tree.Root.Path
	}
	for _, t := range a.tabs {
		if tabHasGitState(t) {
			job.tabs = append(job.tabs, refreshTabSnap{tab: t, path: t.Path})
		}
	}
	wantIgnore := a.tree != nil && a.tree.IgnoringGitignore()
	wantIndex := a.tree != nil && a.finder != nil

	scr := a.screen
	if scr == nil {
		// No event queue to post to (a bare App): do the work inline so the refresh still happens.
		job.compute(wantIgnore, wantIndex)
		job.apply(a)
		return
	}
	done := a.trackJob()
	go func() {
		defer done()
		job.compute(wantIgnore, wantIndex)
		job.when = time.Now()
		if err := scr.PostEvent(job); err != nil {
			// Queue full or screen finalised: nobody will ever apply this, and leaving inFlight set
			// would wedge every later refresh.
			a.refresh.inFlight.Store(false)
		}
	}()
}

// compute runs every git command for one refresh and stores the results on e. It runs on the job
// goroutine and so must touch only e's own fields and the copied snapshot — never the App or a Tab.
func (e *refreshResultEvent) compute(wantIgnore, wantIndex bool) {
	if e.tree != nil {
		e.status = loadGitStatus(e.rootDir)
		if e.status.IsRepo && (wantIgnore || wantIndex) {
			// The same command the tree's ignore filter and the finder index each used to run on
			// their own, run once with the same working directory so the answers are identical.
			out, err := runGit(e.treeRoot, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
			if err == nil {
				e.lsOK = true
				if wantIgnore {
					e.snap = filetree.IgnoreSnapshotFromLsFiles(e.treeRoot, out)
				}
				if wantIndex {
					e.paths = finder.ParseGitLsFiles(out)
				}
			}
		}
	}
	if len(e.tabs) == 0 {
		return
	}
	e.unmerged = gitUnmergedPaths(e.rootDir) // once, not once per tab
	e.lines = make([]map[int]editor.GitLineChange, len(e.tabs))
	for i, ts := range e.tabs {
		e.lines[i] = loadGitLineChanges(e.rootDir, ts.path)
	}
}

// apply installs the job's results. It runs on the UI goroutine. Every write is guarded against
// the world having moved on while the job ran: the tree it was computed for, and each tab's
// identity and path.
func (e *refreshResultEvent) apply(a *App) {
	a.refresh.inFlight.Store(false)

	if e.tree != nil && e.tree == a.tree {
		if e.status.IsRepo {
			dirtyFiles := rebaseGitPaths(e.status.DirtyFiles, a.tree.Root.Path)
			a.tree.DirtyFiles = dirtyFiles
			a.tree.DirtyFolders = dirtyFolderSet(dirtyFiles, a.tree.Root.Path)
			a.gitBranch = e.status.Branch
		} else {
			a.tree.DirtyFiles = nil
			a.tree.DirtyFolders = nil
			a.gitBranch = ""
		}
		// The directory reload already ran against the previous ignore set; if the new one differs
		// (a file was added, a .gitignore edited) reload once more so the tree matches it.
		if a.tree.SwapIgnore(e.snap) {
			a.tree.ReloadDirs()
		}
	}

	for i, ts := range e.tabs {
		if i >= len(e.lines) || !a.isOpenTab(ts.tab) || ts.tab.Path != ts.path {
			continue
		}
		a.installTabGitState(ts.tab, e.lines[i], e.unmerged)
	}

	if e.tree != nil && e.tree == a.tree && a.finder != nil {
		if e.lsOK {
			a.finder.SetIndex(e.paths, true)
			if a.finderOpen {
				a.refreshFinderResults()
			}
		} else {
			// Not a git repo (or git failed): fall back to the finder's own walk. It keeps serving
			// the previous index while it rebuilds.
			a.invalidateFinder()
		}
	}

	if a.refresh.pending {
		a.refresh.pending = false
		a.startGitRefresh()
	}
}

// isOpenTab reports whether t is still one of the open tabs.
func (a *App) isOpenTab(t *editor.Tab) bool {
	for _, tab := range a.tabs {
		if tab == t {
			return true
		}
	}
	return false
}
