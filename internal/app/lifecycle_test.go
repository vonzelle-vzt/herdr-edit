// =============================================================================
// File: internal/app/lifecycle_test.go
// Author: Vonzelle Brown
// Created: 2026-11-19
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package app

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/cloudmanic/spice-edit/internal/spiceconfig"
)

// quitEvent flips a.quit on the UI goroutine, which is how a test ends Run
// without synthesising the key sequence for the menu's Quit row.
type quitEvent struct{ when time.Time }

// When satisfies tcell.Event.
func (e *quitEvent) When() time.Time { return e.when }

// apply ends the Run loop.
func (e *quitEvent) apply(a *App) { a.quit = true }

// waitGoroutines polls until the goroutine count is within tol of base or d
// passes, and returns the last count. Polling, not sleeping: the answer is
// usually immediate and a fixed sleep would be either slow or flaky.
func waitGoroutines(base, tol int, d time.Duration) int {
	deadline := time.Now().Add(d)
	n := runtime.NumGoroutine()
	for n > base+tol && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		n = runtime.NumGoroutine()
	}
	return n
}

// TestRunAndCloseLeaveNoGoroutinesBehind is the shutdown contract end to end:
// arm every ticker, timer and subprocess job the editor can have in flight,
// quit, Close, and the goroutine count must return to where it started.
func TestRunAndCloseLeaveNoGoroutinesBehind(t *testing.T) {
	base := runtime.NumGoroutine()

	root := t.TempDir()
	file := filepath.Join(root, "a.txt")
	if err := os.WriteFile(file, []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	origGit := runGit
	runGit = func(dir string, args ...string) ([]byte, error) { return nil, errors.New("no git in test") }
	defer func() { runGit = origGit }()

	var registered, unregistered atomic.Int32
	origReg := registerEditor
	registerEditor = func(string) (func(), error) {
		registered.Add(1)
		return func() { unregistered.Add(1) }, nil
	}
	defer func() { registerEditor = origReg }()

	// Inline suggestions ON, against a provider that holds every request open
	// until its context is cancelled: the debouncer's goroutine is parked
	// inside a call when quit arrives, and only shutdown can release it.
	inlineFake := newFakeInlineProvider("", true)
	stubInline(t, spiceconfig.Inline{Enabled: true}, inlineFake)

	a := newTestApp(t, root)
	a.openFile(file)

	// Tickers.
	a.startTreeRefresh()
	a.startAutoScroll(1)
	// One-shot timers, armed far in the future so only a Stop can disarm them.
	a.scheduleLSPWake(time.Hour)
	a.completionTimer = time.AfterFunc(time.Hour, func() {})
	s := a.blameSched()
	s.delay = time.Hour
	s.run = func(string, int) string { return "someone" }
	s.timer = time.AfterFunc(time.Hour, func() {})
	// Subprocess-wrapping jobs.
	a.startBlame(blameKey{path: file, line: 0})
	a.startGitRefresh()

	runDone := make(chan error, 1)
	go func() { runDone <- a.Run() }()
	_ = a.screen.PostEvent(&tcell.EventKey{})
	// A typed rune triggers an inline request; quit only once it is in flight.
	_ = a.screen.PostEvent(tcell.NewEventKey(tcell.KeyRune, 'x', tcell.ModNone))
	waitSignal(t, inlineFake.started, 5*time.Second, "the inline request to be in flight")
	if err := a.screen.PostEvent(&quitEvent{when: time.Now()}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after quit")
	}
	a.Close()

	if registered.Load() != 1 || unregistered.Load() != 1 {
		t.Fatalf("registered=%d unregistered=%d, want 1 and 1", registered.Load(), unregistered.Load())
	}
	waitSignal(t, inlineFake.cancelled, 2*time.Second, "shutdown to cancel the in-flight inline request")
	if n := waitGoroutines(base, 2, 2*time.Second); n > base+2 {
		buf := make([]byte, 1<<16)
		t.Fatalf("goroutines after Close = %d, baseline %d\n%s", n, base, buf[:runtime.Stack(buf, true)])
	}
}

// TestShutdownStopsEveryTimer pins the unit behind the e2e test: after
// shutdownBackground no one-shot timer is left armed, so none can fire a stale
// event into a screen that is gone.
func TestShutdownStopsEveryTimer(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.scheduleLSPWake(time.Hour)
	a.completionTimer = time.AfterFunc(time.Hour, func() {})
	s := a.blameSched()
	s.timer = time.AfterFunc(time.Hour, func() {})
	lsp, comp, bl := a.lspWakeTimer, a.completionTimer, s.timer

	a.shutdownBackground()

	if lsp == nil || comp == nil || bl == nil {
		t.Fatal("timers were not armed by the test setup")
	}
	if lsp.Stop() || comp.Stop() || bl.Stop() {
		t.Fatal("a timer was still armed after shutdownBackground (Stop reported it was active)")
	}
	if a.lspWakeTimer != nil || a.completionTimer != nil || s.timer != nil {
		t.Fatal("timer references were not cleared")
	}
	a.shutdownBackground() // idempotent: a second call must not panic
}

// TestRegisterEditorFailureDoesNotStopRun pins that a registry error is
// ignored and a nil unregister is tolerated: being absent from a peer's list
// must never stop the editor from running or quitting.
func TestRegisterEditorFailureDoesNotStopRun(t *testing.T) {
	stubInline(t, spiceconfig.Inline{}, nil) // beginLifecycle must not read the real config.json
	orig := registerEditor
	defer func() { registerEditor = orig }()

	for name, fn := range map[string]func(string) (func(), error){
		"error":   func(string) (func(), error) { return nil, errors.New("no registry") },
		"nil fn":  func(string) (func(), error) { return nil, nil },
		"both":    func(string) (func(), error) { return func() {}, errors.New("half") },
		"default": orig,
	} {
		registerEditor = fn
		t.Run(name, func(t *testing.T) {
			a := newTestApp(t, t.TempDir())
			a.beginLifecycle()
			a.shutdownBackground() // must not panic whatever came back
		})
	}
}

// TestBeginLifecycleStartsInlineEvenWhenRegistryFails pins the ordering inside
// beginLifecycle: the registry's failure path returns early, and inline
// suggestions enabled in config.json must still start — and, with them off,
// must not.
func TestBeginLifecycleStartsInlineEvenWhenRegistryFails(t *testing.T) {
	orig := registerEditor
	registerEditor = func(string) (func(), error) { return nil, errors.New("no registry") }
	defer func() { registerEditor = orig }()

	stubInline(t, spiceconfig.Inline{Enabled: true}, newFakeInlineProvider("x", false))
	a := newTestApp(t, t.TempDir())
	a.beginLifecycle()
	if a.inline.deb == nil {
		t.Fatal("a registry failure skipped starting enabled inline suggestions")
	}
	a.shutdownBackground()
	if a.inline.deb != nil {
		t.Fatal("shutdownBackground left inline suggestions running")
	}

	stubInline(t, spiceconfig.Inline{}, newFakeInlineProvider("x", false))
	b := newTestApp(t, t.TempDir())
	b.beginLifecycle()
	defer b.shutdownBackground()
	if b.inline.deb != nil {
		t.Fatal("beginLifecycle started inline suggestions the config leaves off")
	}
}

// TestShutdownWaitIsBoundedWhenAJobHangs pins the "bounded wait" half: a git
// subprocess that never returns must delay quitting by at most jobDrainWait.
func TestShutdownWaitIsBoundedWhenAJobHangs(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	block := make(chan struct{})
	done := a.trackJob()
	go func() { <-block; done() }()

	start := time.Now()
	a.shutdownBackground()
	elapsed := time.Since(start)
	close(block)

	if elapsed < jobDrainWait/2 {
		t.Fatalf("shutdown returned in %v without waiting for the in-flight job", elapsed)
	}
	if elapsed > jobDrainWait+time.Second {
		t.Fatalf("shutdown took %v, want it bounded near %v", elapsed, jobDrainWait)
	}
}

// TestShutdownWaitsForFinishingJobs pins the other side: a job that finishes
// promptly is waited for, so Close returning means the goroutine is gone.
func TestShutdownWaitsForFinishingJobs(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	var finished atomic.Bool
	done := a.trackJob()
	go func() { time.Sleep(50 * time.Millisecond); finished.Store(true); done() }()

	a.shutdownBackground()
	if !finished.Load() {
		t.Fatal("shutdownBackground returned before a short job finished")
	}
}

// TestDefaultRegisterEditorIsANoOp pins the seam's contract for the build that
// has no registry wired: it succeeds and hands back a callable withdraw.
func TestDefaultRegisterEditorIsANoOp(t *testing.T) {
	unreg, err := registerEditor("/x")
	if err != nil || unreg == nil {
		t.Fatalf("default registerEditor: err=%v nil-func=%v, want a func and no error", err, unreg == nil)
	}
	unreg()
}

// TestTreeRefreshTickBeatsTheRegistry pins the heartbeat's one call site: the
// 10s tick. Without it a registered editor ages past state.EditorStaleAfter in
// a minute and every handoff starts reporting "no running editor".
func TestTreeRefreshTickBeatsTheRegistry(t *testing.T) {
	beats := 0
	old := beatEditor
	beatEditor = func() error { beats++; return nil }
	defer func() { beatEditor = old }()

	a := newTestApp(t, t.TempDir())
	a.handleEvent(&treeRefreshEvent{})
	if beats != 1 {
		t.Fatalf("tick beat the registry %d times, want 1", beats)
	}
}
