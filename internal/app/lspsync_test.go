// =============================================================================
// File: internal/app/lspsync_test.go
// Author: Vonzelle Brown
// Created: 2026-11-19
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package app

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/cloudmanic/spice-edit/internal/editor"
	"github.com/cloudmanic/spice-edit/internal/lsp"
)

// syncRecorder installs lspChangeObserver for one test and records every
// didChange text maybeSyncLSP sends.
type syncRecorder struct {
	texts []string
	at    []time.Time
}

// recordLSPChanges wires a recorder and restores the observer on cleanup.
func recordLSPChanges(t *testing.T) *syncRecorder {
	t.Helper()
	r := &syncRecorder{}
	lspChangeObserver = func(_, text string) {
		r.texts = append(r.texts, text)
		r.at = append(r.at, time.Now())
	}
	t.Cleanup(func() { lspChangeObserver = nil })
	return r
}

// lspSyncApp is an app with one open file and a (never-started) LSP manager,
// which is all maybeSyncLSP needs to run its logic.
func lspSyncApp(t *testing.T) (*App, *editor.Tab) {
	t.Helper()
	a, path := appWithFile(t, "package main\n")
	a.lsp = lsp.NewManager(filepath.Dir(path))
	return a, a.activeTabPtr()
}

// pumpLoop emulates Run's loop body for d: it takes events off the screen's
// queue, dispatches them through handleEvent and calls maybeSyncLSP after each,
// exactly like the real loop. It returns the number of lspWakeEvents seen.
func pumpLoop(a *App, d time.Duration) int {
	evs := make(chan tcell.Event, 16)
	go func() {
		for {
			ev := a.screen.PollEvent()
			if ev == nil {
				return
			}
			evs <- ev
		}
	}()
	wakes := 0
	deadline := time.After(d)
	for {
		select {
		case ev := <-evs:
			if _, ok := ev.(*lspWakeEvent); ok {
				wakes++
			}
			a.handleEvent(ev)
			a.maybeSyncLSP()
		case <-deadline:
			return wakes
		}
	}
}

// TestMaybeSyncLSPSendsTrailingEditWithoutFurtherEvents is the RED test for the
// missing trailing edge: an edit that lands inside the 400ms throttle window,
// followed by silence, used to sit unsent until some unrelated event happened
// to arrive. Now the throttled skip arms a wake and the loop syncs it unprompted.
func TestMaybeSyncLSPSendsTrailingEditWithoutFurtherEvents(t *testing.T) {
	a, tab := lspSyncApp(t)
	rec := recordLSPChanges(t)

	tab.Buffer.InsertString(editor.Position{Line: 0, Col: 0}, "// one\n")
	a.maybeSyncLSP() // outside the window: sent now
	if len(rec.texts) != 1 {
		t.Fatalf("first edit should sync immediately, got %d sends", len(rec.texts))
	}

	tab.Buffer.InsertString(editor.Position{Line: 0, Col: 0}, "// two\n")
	a.maybeSyncLSP() // inside the window: throttled
	if len(rec.texts) != 1 {
		t.Fatalf("second edit must be throttled, got %d sends", len(rec.texts))
	}

	// No further user events: only the trailing-edge wake can deliver it.
	start := time.Now()
	pumpLoop(a, 900*time.Millisecond)
	if len(rec.texts) != 2 {
		t.Fatalf("trailing edit never synced (sends=%d)", len(rec.texts))
	}
	if want := tab.Buffer.String(); rec.texts[1] != want {
		t.Errorf("synced text = %q, want %q", rec.texts[1], want)
	}
	if d := rec.at[1].Sub(start); d > 600*time.Millisecond {
		t.Errorf("trailing edit synced after %v, want ~400ms", d)
	}
}

// TestThrottledBurstArmsExactlyOneWake pins that a flurry of throttled calls
// costs one timer and one event, not one per keystroke.
func TestThrottledBurstArmsExactlyOneWake(t *testing.T) {
	a, tab := lspSyncApp(t)
	recordLSPChanges(t)

	tab.Buffer.InsertString(editor.Position{}, "a")
	a.maybeSyncLSP()
	for i := 0; i < 20; i++ {
		tab.Buffer.InsertString(editor.Position{}, "b")
		a.maybeSyncLSP()
	}
	if wakes := pumpLoop(a, 900*time.Millisecond); wakes != 1 {
		t.Fatalf("expected exactly 1 wake event, got %d", wakes)
	}
}

// TestThrottledSkipWithNothingPendingArmsNoWake pins that the timer is only
// armed when there is a change to deliver; an idle editor must stay idle.
func TestThrottledSkipWithNothingPendingArmsNoWake(t *testing.T) {
	a, tab := lspSyncApp(t)
	recordLSPChanges(t)

	tab.Buffer.InsertString(editor.Position{}, "a")
	a.maybeSyncLSP() // sent
	a.maybeSyncLSP() // throttled, nothing new
	if a.lspWakeArmed {
		t.Fatal("a wake was armed with no pending change")
	}
}

// TestMaybeSyncLSPUsesVersionNotText pins the change test: a same-length edit
// is still sent (the version moved), and an unchanged buffer is not resent.
func TestMaybeSyncLSPUsesVersionNotText(t *testing.T) {
	a, tab := lspSyncApp(t)
	rec := recordLSPChanges(t)

	a.maybeSyncLSP()
	n := len(rec.texts)
	if n != 1 {
		t.Fatalf("first sync of an opened buffer should send once, got %d", n)
	}

	a.lspSyncedAt = time.Time{}
	a.maybeSyncLSP()
	if len(rec.texts) != n {
		t.Fatal("unchanged buffer was resent")
	}

	// Replace one rune with another: identical length, different text.
	tab.Buffer.DeleteRange(editor.Position{Line: 0, Col: 0}, editor.Position{Line: 0, Col: 1})
	tab.Buffer.InsertString(editor.Position{Line: 0, Col: 0}, "P")
	a.lspSyncedAt = time.Time{}
	a.maybeSyncLSP()
	if len(rec.texts) != n+1 {
		t.Fatal("same-length edit was not sent")
	}
}

// TestLSPWakeEventDisarmsTheTimerMarker pins that applying the wake clears the
// armed marker so the next throttled edit can schedule again.
func TestLSPWakeEventDisarmsTheTimerMarker(t *testing.T) {
	a := &App{lspWakeArmed: true}
	a.handleEvent(&lspWakeEvent{when: time.Now()})
	if a.lspWakeArmed {
		t.Fatal("wake event left the marker armed")
	}
}
