// =============================================================================
// File: internal/app/lifecycle.go
// Author: Vonzelle Brown
// Created: 2026-11-19
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// lifecycle.go owns what happens to the editor's background work when it quits.
//
// Run starts a surprising amount that outlives a single event: the 10s tree
// ticker, the auto-scroll ticker, three one-shot timers (LSP trailing-edge
// sync, blame rest, completion debounce), the git refresh job and the blame
// subprocess. Most are harmless at process exit, but "the process is about to
// exit" is not a contract a test, an embedder or a future in-process restart
// can lean on. shutdownBackground makes the contract explicit: every timer is
// stopped, every ticker is told to exit, and the two goroutines that wrap a
// subprocess are waited for — with a bound, because a wedged `git` must never
// keep the editor from quitting.

package app

import (
	"sync"
	"time"

	"github.com/cloudmanic/spice-edit/internal/state"
)

// registerEditor announces this editor to the liveness registry and returns
// the function that withdraws the announcement. A package-level seam so tests
// can observe registration without touching a real state directory.
var registerEditor = state.RegisterEditor

// beatEditor refreshes the registry entry; a seam for the same reason.
var beatEditor = state.BeatEditor

// jobDrainWait bounds how long shutdown waits for in-flight subprocess jobs
// (the git refresh and a blame lookup). They finish in milliseconds in
// practice; the bound exists for the day one does not.
const jobDrainWait = 750 * time.Millisecond

// lifecycle is the App's shutdown bookkeeping. Zero value is ready to use, so
// every test App that builds its struct by hand keeps working.
type lifecycle struct {
	// unregister withdraws the liveness-registry entry; nil until Run registers.
	unregister func()
	// requestWatchStop ends the request-file watcher (requestwake.go).
	requestWatchStop chan struct{}
	// jobs counts the goroutines that wrap a subprocess, so shutdown can wait
	// for them with a bound instead of abandoning them mid-flight.
	jobs sync.WaitGroup
}

// beginLifecycle registers the editor with the liveness registry. A failure is
// deliberately ignored: being absent from a peer's list must never stop the
// editor from running.
func (a *App) beginLifecycle() {
	unreg, err := registerEditor(a.rootDir)
	if err != nil || unreg == nil {
		return
	}
	a.life.unregister = unreg
}

// trackJob registers one subprocess-wrapping goroutine and returns the func it
// must call when done. Call it on the UI goroutine, before the `go` statement,
// so shutdown's Wait can never miss a job that is about to start.
func (a *App) trackJob() func() {
	a.life.jobs.Add(1)
	return a.life.jobs.Done
}

// waitJobs waits up to d for tracked jobs to finish and reports whether they
// did. The helper goroutine it spawns ends as soon as the jobs do.
func (a *App) waitJobs(d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		a.life.jobs.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// shutdownBackground stops everything the editor started in the background.
// It is idempotent — Run calls it on the way out and Close calls it again — and
// it must run on the UI goroutine, like every other App mutation.
func (a *App) shutdownBackground() {
	a.stopTreeRefresh()
	a.stopRequestWatch()
	a.stopAutoScroll()
	a.cancelCompletionRequest()
	if a.lspWakeTimer != nil {
		a.lspWakeTimer.Stop()
		a.lspWakeTimer = nil
	}
	if a.completionTimer != nil {
		a.completionTimer.Stop()
		a.completionTimer = nil
	}
	if a.blame != nil {
		a.blame.cancel()
	}
	if a.life.unregister != nil {
		unreg := a.life.unregister
		a.life.unregister = nil
		unreg()
	}
	a.waitJobs(jobDrainWait)
}
