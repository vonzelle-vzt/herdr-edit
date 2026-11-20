// =============================================================================
// File: internal/app/lspsync.go
// Author: Vonzelle Brown
// Created: 2026-11-19
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// lspsync.go holds the bookkeeping behind maybeSyncLSP: remembering which
// buffer version each document was last sent at, and the one-shot trailing-edge
// wake that makes sure the LAST edit before the user pauses still reaches the
// server.
//
// maybeSyncLSP throttles to lspSyncInterval and is only ever invoked from the
// tail of Run's loop, i.e. after an event. Without a wake, an edit made inside
// the throttle window followed by silence would sit unsent until the next
// unrelated event (a mouse move, the 10s tree refresh) — diagnostics for the
// final keystrokes would lag by an arbitrary amount. The wake is just an event:
// it makes the loop turn once, and the loop's single maybeSyncLSP call does the
// work, so the "one polled call site" rule is untouched.
package app

import (
	"time"
)

// lspChangeObserver, when non-nil, sees every didChange maybeSyncLSP sends.
// It exists so tests can observe the sync without a language-server process.
var lspChangeObserver func(path, text string)

// lspWakeEvent is posted by the trailing-edge timer. Applying it only clears
// the "timer armed" marker; Run calls maybeSyncLSP after every event, which is
// what actually sends the pending change.
type lspWakeEvent struct{ when time.Time }

// When satisfies tcell.Event.
func (e *lspWakeEvent) When() time.Time { return e.when }

// apply runs on the UI goroutine and disarms the timer marker so a later
// throttled edit can arm a fresh one.
func (e *lspWakeEvent) apply(a *App) {
	a.lspWakeArmed = false
}

// lspChangePending reports whether the active document has edits the server has
// not been sent. It is a map lookup and an integer compare — no buffer walk.
func (a *App) lspChangePending() bool {
	tab := a.activeTabPtr()
	if tab == nil || tab.Path == "" || tab.IsImage() {
		return false
	}
	sent, ok := a.lspSentVersion[tab.Path]
	return !ok || sent != tab.Buffer.Version()
}

// scheduleLSPWake arms exactly one timer that posts an lspWakeEvent after d.
// Further throttled skips while it is armed do nothing, so a burst of typing
// costs one timer, not one per keystroke. The timer goroutine only posts an
// event; it never touches App state.
func (a *App) scheduleLSPWake(d time.Duration) {
	if a.screen == nil {
		return
	}
	if a.lspWakeArmed {
		return
	}
	a.lspWakeArmed = true
	scr := a.screen
	a.lspWakeTimer = time.AfterFunc(d, func() {
		_ = scr.PostEvent(&lspWakeEvent{when: time.Now()})
	})
}

// compile-time check that the wake rides the selfApplyingEvent seam.
var _ selfApplyingEvent = (*lspWakeEvent)(nil)
