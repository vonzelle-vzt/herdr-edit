// =============================================================================
// File: internal/app/requestwake.go
// Author: Vonzelle Brown
// Created: 2026-11-19
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// requestwake.go makes an IDLE editor answer --open-at and --debug promptly.
//
// consumeOpenRequest and consumeDebugRequest run once per event, from the one
// polled call site in Run. That is right for correctness, but an editor nobody
// is touching only gets an event from the 10s tree tick — so a request from
// vzt-orca-flow or a herdr panel sat for up to ten seconds, measured at 2.9s on
// one try, which reads as "the button did nothing". This watcher stats the two
// request files and, when either changes, posts an event whose only job is to
// turn the loop once. The consumers and their seq guards are untouched; the
// watcher never reads a request or touches App state.
package app

import (
	"os"
	"time"

	"github.com/cloudmanic/spice-edit/internal/state"
)

// requestWatchInterval is how often the watcher stats the request files. Two
// stats every 150ms is noise next to a terminal redraw, and well under the
// ~250ms at which a click starts to feel unanswered.
var requestWatchInterval = 150 * time.Millisecond

// requestWakeEvent turns the event loop once so the polled consumers run.
type requestWakeEvent struct{ when time.Time }

// When satisfies tcell.Event.
func (e *requestWakeEvent) When() time.Time { return e.when }

// apply does nothing: Run calls the request consumers after every event, and
// that is the whole point of posting one.
func (e *requestWakeEvent) apply(a *App) {}

// fileStamp is what the watcher compares: a rewrite changes mtime or size, and
// every writer renames a temp file into place, so the mtime moves on each one.
type fileStamp struct {
	mod  time.Time
	size int64
	ok   bool
}

// stampOf stats path; a missing file is the zero stamp.
func stampOf(path string) fileStamp {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{mod: fi.ModTime(), size: fi.Size(), ok: true}
}

// startRequestWatch launches the watcher. It is stopped by stopRequestWatch,
// which shutdownBackground calls, so it cannot outlive the editor.
func (a *App) startRequestWatch() {
	if a.screen == nil || a.life.requestWatchStop != nil {
		return
	}
	stop := make(chan struct{})
	a.life.requestWatchStop = stop
	scr := a.screen
	paths := []string{state.OpenRequestFile(), state.DebugRequestFile()}
	last := make([]fileStamp, len(paths))
	for i, p := range paths {
		last[i] = stampOf(p)
	}
	a.life.jobs.Add(1)
	go func() {
		defer a.life.jobs.Done()
		t := time.NewTicker(requestWatchInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
			}
			changed := false
			for i, p := range paths {
				if s := stampOf(p); s != last[i] {
					last[i] = s
					changed = true
				}
			}
			if changed {
				_ = scr.PostEvent(&requestWakeEvent{when: time.Now()})
			}
		}
	}()
}

// stopRequestWatch ends the watcher. Safe to call more than once.
func (a *App) stopRequestWatch() {
	if a.life.requestWatchStop != nil {
		close(a.life.requestWatchStop)
		a.life.requestWatchStop = nil
	}
}

// compile-time check that the wake rides the selfApplyingEvent seam.
var _ selfApplyingEvent = (*requestWakeEvent)(nil)
