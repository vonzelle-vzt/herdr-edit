// =============================================================================
// File: internal/app/requestwake_test.go
// Author: Vonzelle Brown
// Created: 2026-11-19
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package app

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudmanic/spice-edit/internal/state"
	"github.com/gdamore/tcell/v2"
)

// TestRequestWatchWakesAnIdleEditor pins the latency fix: with nothing else
// happening, writing an open-request must post a wake event within a second.
// Before the watcher an idle editor only saw the request on the next 10s tick.
func TestRequestWatchWakesAnIdleEditor(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	old := requestWatchInterval
	requestWatchInterval = 20 * time.Millisecond
	defer func() { requestWatchInterval = old }()

	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatal(err)
	}
	defer scr.Fini()
	a := &App{screen: scr}
	a.startRequestWatch()
	defer a.stopRequestWatch()

	woke := make(chan struct{}, 1)
	go func() {
		for {
			ev := scr.PollEvent()
			if ev == nil {
				return
			}
			if _, ok := ev.(*requestWakeEvent); ok {
				woke <- struct{}{}
				return
			}
		}
	}()
	if err := state.WriteOpenRequest(filepath.Join(t.TempDir(), "x.go"), 1, 1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-woke:
	case <-time.After(time.Second):
		t.Fatal("no wake event within 1s of an open-request being written")
	}
}

// TestRequestWatchStopsOnShutdown pins that the watcher is part of the
// shutdown contract: shutdownBackground ends it, and calling stop twice is safe.
func TestRequestWatchStopsOnShutdown(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatal(err)
	}
	defer scr.Fini()
	a := &App{screen: scr}
	a.startRequestWatch()
	done := make(chan struct{})
	go func() { a.shutdownBackground(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdownBackground did not return with the watcher running")
	}
	if a.life.requestWatchStop != nil {
		t.Fatal("watcher stop channel still set after shutdown")
	}
	a.stopRequestWatch()
}
