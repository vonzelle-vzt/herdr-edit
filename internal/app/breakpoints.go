// =============================================================================
// File: internal/app/breakpoints.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// breakpoints.go owns the breakpoint MODEL: persistent, gutter-visible,
// edit-tracking marks, the authoritative list they flatten into, the export, and
// — since Lane B stage 4 — the condition and log message a mark can carry.
//
// It still owns no session. debug.go decides what reaches an adapter and when;
// everything here is about what the user has asked for, which is why a condition
// can be set with no debugger running at all and is exported as a `condition`
// line for driving dlv/pdb/gdb by hand.
//
// 🔴 The one exception is afterBreakpointEdit, and it is a single call in one
// place on purpose: setBreakpoints is WHOLE-FILE, so a mark that changes while a
// session is live has to re-send that file's complete set or the rest are
// cleared by the same request that was meant to refine one of them.
//
// Modeled on bookmarks.go: same shape, same doc-comment density, same
// "menu group + polled sync + one place to look" discipline the rest of
// this fork's small features follow.
//
// a.breakpoints is the AUTHORITATIVE list — it's what allBreakpoints, the
// export, and the persisted file all read. It is not the same thing as any
// one Tab's Marks: Marks only exists for tabs currently open, while
// a.breakpoints also carries breakpoints in files the user isn't looking at
// right now (loaded from disk at startup). syncBreakpoints reconciles the
// two, and it is the ONE place that happens — see its own doc comment for
// why that matters here specifically.
package app

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cloudmanic/spice-edit/internal/clipboard"
	"github.com/cloudmanic/spice-edit/internal/editor"
	"github.com/cloudmanic/spice-edit/internal/state"
)

// Breakpoint is one persistent breakpoint mark, flattened out of whatever
// Tab it lives on (or, for a file that isn't open, out of the persisted
// store). Condition and LogMessage are carried through from editor.Mark for
// the same reason they exist there: a later debug-adapter stage reads them,
// this one only stores and exports them.
type Breakpoint struct {
	Path       string
	Line       int // 0-based, matching editor.Tab.Marks.
	Enabled    bool
	Condition  string
	LogMessage string
}

// hasBreakpointableTab reports whether the active tab is real, editable
// text — not an image preview, not a synthetic view (the diff/problems/
// search-results tabs), and not an untitled scratch buffer with nowhere to
// export a path from.
func (a *App) hasBreakpointableTab() bool {
	t := a.activeTabPtr()
	return t != nil && t.Path != "" && !t.IsImage() && !t.Synthetic
}

// isBreakpointKind reports whether a mark kind is one the debugger sends to an
// adapter. Logpoints are breakpoints that print instead of stopping, so every
// path that collects, clears, verifies or exports a breakpoint has to accept
// both — a check that names only MarkBreakpoint silently drops every logpoint
// the user set, which reads as logpoints not working at all.
func isBreakpointKind(k editor.MarkKind) bool {
	return k == editor.MarkBreakpoint || k == editor.MarkLogpoint
}

// menuToggleBreakpoint sets or clears a breakpoint on the cursor's line.
// Bound to Esc 9 (mnemonic: F9, the universal toggle-breakpoint key in every
// GUI debugger this fork's users have used).
func (a *App) menuToggleBreakpoint() {
	a.closeMenu()
	tab := a.activeTabPtr()
	if !a.hasBreakpointableTab() {
		a.flash("Nothing here to set a breakpoint on")
		return
	}
	line := tab.Cursor.Line
	if m, ok := tab.MarkAt(line); ok && isBreakpointKind(m.Kind) {
		tab.ClearMark(line)
		a.afterBreakpointEdit()
		a.flash(fmt.Sprintf("Breakpoint cleared %s:%d", filepath.Base(tab.Path), line+1))
		return
	}
	tab.SetMark(line, editor.Mark{Kind: editor.MarkBreakpoint, Enabled: true, VerifiedLine: -1})
	a.afterBreakpointEdit()
	a.flash(fmt.Sprintf("Breakpoint set %s:%d — Esc 5 lists, Esc 9 toggles", filepath.Base(tab.Path), line+1))
}

// afterBreakpointEdit is the ONE place a breakpoint change is pushed to a live
// adapter.
//
// It exists for the reason syncBreakpoints is polled from a single site: there
// are now six actions that mutate a Mark, and a hand-placed resend after each
// one is one the seventh will forget. The symptom would be a breakpoint the
// editor shows and the debugger does not have, which is exactly the class of bug
// nothing on screen reports. A no-op when no session is running.
func (a *App) afterBreakpointEdit() {
	a.resendBreakpoints()
}

// breakpointMarkAtCursor returns the breakpoint or logpoint mark under the
// cursor, having already flashed the reason when there is not one.
//
// Shared by the three condition/logpoint actions so their guards cannot drift
// apart — and so each one refuses in the same words.
func (a *App) breakpointMarkAtCursor(what string) (*editor.Tab, int, editor.Mark, bool) {
	tab := a.activeTabPtr()
	if !a.hasBreakpointableTab() {
		a.flash("Nothing here to " + what)
		return nil, 0, editor.Mark{}, false
	}
	line := tab.Cursor.Line
	m, ok := tab.MarkAt(line)
	if !ok || !isBreakpointKind(m.Kind) {
		a.flash("No breakpoint on this line — Esc 9 sets one")
		return nil, 0, editor.Mark{}, false
	}
	return tab, line, m, true
}

// menuSetCondition asks for an expression the debugger must find true before
// stopping on the breakpoint under the cursor.
//
// 🔴 The capability is checked BEFORE the prompt opens, not after it is
// submitted. An adapter without supportsConditionalBreakpoints does not refuse a
// condition, it ignores the field — so the breakpoint would be accepted,
// reported verified, and then fire on every iteration of the loop the condition
// was written for. Asking the user to type an expression we already know will be
// thrown away is worse than refusing: it produces a debugger that appears to
// have taken the condition.
//
// The check only fires while a session is LIVE. With no adapter running there is
// nothing to ask, and refusing then would make a condition unsettable before F5
// — the moment you actually want to set one. pushBreakpoints runs the same gate
// on the way out, so a condition typed with no session and started against an
// adapter that cannot honour it is still reported rather than dropped in silence.
func (a *App) menuSetCondition() {
	a.closeMenu()
	if msg := a.adapterRefusal(false); msg != "" {
		a.flash(msg)
		return
	}
	_, _, m, ok := a.breakpointMarkAtCursor("set a condition on")
	if !ok {
		return
	}
	a.openPrompt("Breakpoint condition",
		"an expression the debugger must find true, e.g. i == 3",
		m.Condition, (*App).conditionSubmit)
}

// conditionSubmit stores the typed condition and pushes it to a live adapter.
func (a *App) conditionSubmit(expr string) {
	tab, line, m, ok := a.breakpointMarkAtCursor("set a condition on")
	if !ok {
		return
	}
	m.Condition = expr
	tab.SetMark(line, m)
	a.afterBreakpointEdit()
	a.flash(fmt.Sprintf("Condition on %s:%d — %s", filepath.Base(tab.Path), line+1, expr))
}

// menuSetLogpoint turns the breakpoint under the cursor into a logpoint: the
// adapter prints the message and keeps running instead of stopping.
//
// Same capability rule as menuSetCondition, and the same reason — an adapter
// without supportsLogPoints drops logMessage and the "logpoint" becomes an
// ordinary breakpoint that halts the program, which is the opposite of what was
// asked for.
func (a *App) menuSetLogpoint() {
	a.closeMenu()
	if msg := a.adapterRefusal(true); msg != "" {
		a.flash(msg)
		return
	}
	_, _, m, ok := a.breakpointMarkAtCursor("set a log message on")
	if !ok {
		return
	}
	a.openPrompt("Logpoint message",
		"printed instead of stopping; {expr} interpolates",
		m.LogMessage, (*App).logpointSubmit)
}

// logpointSubmit stores the message, flips the mark's kind so the gutter shows
// ◆ rather than ●, and pushes it to a live adapter.
func (a *App) logpointSubmit(msg string) {
	tab, line, m, ok := a.breakpointMarkAtCursor("set a log message on")
	if !ok {
		return
	}
	m.LogMessage = msg
	m.Kind = editor.MarkLogpoint
	tab.SetMark(line, m)
	a.afterBreakpointEdit()
	a.flash(fmt.Sprintf("Logpoint %s:%d — %s", filepath.Base(tab.Path), line+1, msg))
}

// menuClearBreakpointCondition strips the condition and log message from the
// mark under the cursor, turning a logpoint back into a plain breakpoint.
//
// It is a row of its own because openPrompt IGNORES an empty submit — deliberately,
// so a stray Enter cannot destroy a filename in the rename modal — which leaves
// no way to clear a condition through the prompt that set it. Without this, the
// only cure is deleting the breakpoint and setting it again.
func (a *App) menuClearBreakpointCondition() {
	a.closeMenu()
	tab, line, m, ok := a.breakpointMarkAtCursor("clear a condition on")
	if !ok {
		return
	}
	if m.Condition == "" && m.LogMessage == "" {
		a.flash("That breakpoint has no condition or log message")
		return
	}
	m.Condition, m.LogMessage = "", ""
	m.Kind = editor.MarkBreakpoint
	tab.SetMark(line, m)
	a.afterBreakpointEdit()
	a.flash(fmt.Sprintf("Cleared condition on %s:%d", filepath.Base(tab.Path), line+1))
}

// adapterRefusal returns the message to show when the LIVE adapter cannot
// honour a condition (logpoint false) or a log message (logpoint true), or ""
// when it can — or when there is no session to ask.
//
// The adapter is named in the message on purpose: "log points are not supported"
// reads as a limitation of this editor and sends the user to the wrong place.
func (a *App) adapterRefusal(logpoint bool) string {
	// 🔴 `starting` counts as "no session". Capabilities only exist once
	// initialize has been answered, and a zero Capabilities reads as "supports
	// nothing" — so gating on it during the seconds the adapter spends compiling
	// the program would refuse every condition with a message that is simply
	// false. resendBreakpoints marks the edit dirty instead, and
	// handleDebugStarted pushes it once the real answer is in.
	if a.debug == nil || a.debug.starting || a.debug.client == nil {
		return ""
	}
	if logpoint {
		if a.debug.caps.SupportsLogPoints {
			return ""
		}
		return a.debug.adapter + " does not support log points"
	}
	if a.debug.caps.SupportsConditionalBreakpoints {
		return ""
	}
	return a.debug.adapter + " does not support conditional breakpoints"
}

// hasConditionableBreakpoint gates the condition / logpoint menu rows: there
// must be a breakpoint under the cursor to put one on.
func (a *App) hasConditionableBreakpoint() bool {
	tab := a.activeTabPtr()
	if !a.hasBreakpointableTab() {
		return false
	}
	m, ok := tab.MarkAt(tab.Cursor.Line)
	return ok && isBreakpointKind(m.Kind)
}

// menuToggleBreakpointEnabled flips Enabled on the cursor line's breakpoint
// without removing it — the same "greyed out but still there" gesture VS
// Code and most debuggers offer for a breakpoint you want to keep but not
// hit right now.
func (a *App) menuToggleBreakpointEnabled() {
	a.closeMenu()
	tab := a.activeTabPtr()
	if !a.hasBreakpointableTab() {
		a.flash("Nothing here to toggle")
		return
	}
	line := tab.Cursor.Line
	m, ok := tab.MarkAt(line)
	if !ok || !isBreakpointKind(m.Kind) {
		a.flash("No breakpoint on this line")
		return
	}
	m.Enabled = !m.Enabled
	tab.SetMark(line, m)
	a.afterBreakpointEdit()
	state := "enabled"
	if !m.Enabled {
		state = "disabled"
	}
	a.flash(fmt.Sprintf("Breakpoint %s: %s:%d", state, filepath.Base(tab.Path), line+1))
}

// sortBreakpoints orders bps by path then line — the same "walk a file top
// to bottom instead of jumping around" rule bookmarks.go uses, and the order
// every consumer here (the picker, the export, the persisted file) wants.
func sortBreakpoints(bps []Breakpoint) {
	sort.SliceStable(bps, func(i, j int) bool {
		if bps[i].Path != bps[j].Path {
			return bps[i].Path < bps[j].Path
		}
		return bps[i].Line < bps[j].Line
	})
}

// allBreakpoints returns every known breakpoint, sorted by path then line.
// Reads the cache syncBreakpoints keeps current — see that function's doc
// comment for why this never has to walk a.tabs itself.
func (a *App) allBreakpoints() []Breakpoint {
	out := make([]Breakpoint, len(a.breakpoints))
	copy(out, a.breakpoints)
	sortBreakpoints(out)
	return out
}

// hasBreakpoints gates the menu rows that only make sense with breakpoints
// set (list / clear / export).
func (a *App) hasBreakpoints() bool {
	return len(a.breakpoints) > 0
}

// syncBreakpoints reconciles every open tab's live Marks into a.breakpoints
// and persists the result. Called from the SAME single polled site in Run()
// as publishActive() / consumeOpenRequest(), for the same reason both of
// those are polled rather than hooked: SetMark/ClearMark can be called from
// several menu actions, and a hand-placed sync call after each one is easy
// to forget on the next one added. One site cannot miss a case.
//
// A tab's Marks is authoritative for ITS OWN path while that tab is open —
// so every breakpoint entry for an open path gets replaced wholesale by
// whatever that tab's Marks says right now (including "none", if the user
// cleared them all). Entries for paths with no open tab are left untouched;
// they came from disk at startup and nothing in this process can tell
// whether they're still accurate, so they're carried forward as-is rather
// than silently dropped.
func (a *App) syncBreakpoints() {
	openPaths := make(map[string]bool, len(a.tabs))
	var fromOpen []Breakpoint
	for _, tab := range a.tabs {
		if tab.Path == "" || tab.Synthetic || tab.IsImage() {
			continue
		}
		openPaths[tab.Path] = true
		for _, line := range tab.MarkLines() {
			m, _ := tab.MarkAt(line)
			if !isBreakpointKind(m.Kind) {
				continue
			}
			fromOpen = append(fromOpen, Breakpoint{
				Path: tab.Path, Line: line, Enabled: m.Enabled,
				Condition: m.Condition, LogMessage: m.LogMessage,
			})
		}
	}

	next := make([]Breakpoint, 0, len(a.breakpoints)+len(fromOpen))
	for _, b := range a.breakpoints {
		if !openPaths[b.Path] {
			next = append(next, b)
		}
	}
	next = append(next, fromOpen...)
	sortBreakpoints(next)

	if breakpointsEqual(a.breakpoints, next) {
		return // nothing changed — don't restart the persist debounce for no reason
	}
	a.breakpoints = next
	if a.bpStore != nil {
		a.bpStore.Set(a.rootDir, toPersistedBreakpoints(next))
	}
}

// breakpointsEqual reports whether two (already sorted) breakpoint slices
// carry the same content, so syncBreakpoints can skip a no-op persist write
// on every single poll tick.
func breakpointsEqual(a, b []Breakpoint) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// toPersistedBreakpoints converts to the state package's wire type.
func toPersistedBreakpoints(bps []Breakpoint) []state.PersistedBreakpoint {
	out := make([]state.PersistedBreakpoint, len(bps))
	for i, b := range bps {
		out[i] = state.PersistedBreakpoint{
			Path: b.Path, Line: b.Line, Enabled: b.Enabled,
			Condition: b.Condition, LogMessage: b.LogMessage,
		}
	}
	return out
}

// loadPersistedBreakpoints reads root's saved breakpoints back into the
// in-memory Breakpoint shape, for seeding a.breakpoints at startup. Marks
// only get created on the corresponding Tab once that file is actually
// opened (there being no Tab to hold a Mark for a file nobody's looking
// at) — a.breakpoints is what lets the picker and the export still see
// them in the meantime.
func loadPersistedBreakpoints(root string) []Breakpoint {
	saved := state.LoadBreakpoints(root)
	if len(saved) == 0 {
		return nil
	}
	out := make([]Breakpoint, len(saved))
	for i, b := range saved {
		out[i] = Breakpoint{
			Path: b.Path, Line: b.Line, Enabled: b.Enabled,
			Condition: b.Condition, LogMessage: b.LogMessage,
		}
	}
	sortBreakpoints(out)
	return out
}

// gotoBreakpoint opens b's file if needed and puts the cursor on its line —
// the same open-then-move sequence gotoBookmark uses.
func (a *App) gotoBreakpoint(b Breakpoint) {
	tab := a.activeTabPtr()
	if tab == nil || tab.Path != b.Path {
		a.openFile(b.Path)
		tab = a.activeTabPtr()
	}
	if tab == nil {
		a.flash("Could not open " + filepath.Base(b.Path))
		return
	}
	tab.MoveCursorTo(editor.Position{Line: b.Line, Col: 0}, false)
	a.flash(fmt.Sprintf("%s:%d", filepath.Base(b.Path), b.Line+1))
}

// menuListBreakpoints opens the command palette as a picker over every
// breakpoint, jumping to whichever one is chosen. Bound to Esc 5 (mnemonic:
// F5, the universal "start/resume debugging" key — the closest thing this
// no-adapter stage has to it).
func (a *App) menuListBreakpoints() {
	a.closeMenu()
	bps := a.allBreakpoints()
	if len(bps) == 0 {
		a.flash("No breakpoints — Esc 9 sets one at the cursor")
		return
	}
	cmds := make([]paletteCommand, 0, len(bps))
	for _, b := range bps {
		b := b // capture
		label := fmt.Sprintf("%s:%d", a.relBreakpointPath(b.Path), b.Line+1)
		if !b.Enabled {
			label += "  (disabled)"
		}
		cmds = append(cmds, paletteCommand{
			label:   label,
			action:  func(app *App) { app.gotoBreakpoint(b) },
			enabled: alwaysTrue,
		})
	}
	a.openPaletteWith(cmds, "Breakpoints")
}

// relBreakpointPath renders path relative to the project root when
// possible, matching how every other jump list in this fork (references,
// search results, problems) prints locations.
func (a *App) relBreakpointPath(path string) string {
	if r, err := filepath.Rel(a.rootDir, path); err == nil && !strings.HasPrefix(r, "..") {
		return r
	}
	return path
}

// menuClearBreakpoints drops every breakpoint — from every open tab's Marks
// and from the authoritative list — and persists the (now empty) result.
func (a *App) menuClearBreakpoints() {
	a.closeMenu()
	n := len(a.breakpoints)
	for _, tab := range a.tabs {
		for _, line := range tab.MarkLines() {
			if m, ok := tab.MarkAt(line); ok && isBreakpointKind(m.Kind) {
				tab.ClearMark(line)
			}
		}
	}
	a.breakpoints = nil
	if a.bpStore != nil {
		a.bpStore.Set(a.rootDir, nil)
	}
	a.afterBreakpointEdit()
	a.flash(fmt.Sprintf("Cleared %d breakpoint(s)", n))
}

// menuExportBreakpoints copies every ENABLED breakpoint to the system
// clipboard as `break file:line` lines ready to paste into dlv, pdb or gdb.
// Disabled breakpoints are skipped — a script that re-armed something the
// user deliberately turned off would be a surprise, not a convenience.
func (a *App) menuExportBreakpoints() {
	a.closeMenu()
	bps := a.allBreakpoints()
	script := exportBreakpointScript(bps, "dlv")
	if script == "" {
		a.flash("No enabled breakpoints to export")
		return
	}
	if err := clipboard.CopyToSystem(script); err != nil {
		a.flash("Could not copy to clipboard: " + err.Error())
		return
	}
	a.flash("Copied breakpoints to clipboard as dlv commands")
}

// exportBreakpointScript renders bps as debugger commands for the given
// flavour ("dlv", "pdb" or "gdb"). Disabled breakpoints are always skipped.
//
// All three flavours accept bare `break file:line`, which is what an
// unconditional breakpoint renders as regardless of flavour. They diverge once
// a Condition is set, and as of this stage conditions are real rather than a
// field nothing writes — so the branching below is now exercised instead of
// being a reservation.
//
// 🔴 dlv is the one that cannot say it in a single command: `break` takes no
// inline condition, so a named breakpoint plus a `condition` line is the only
// spelling that works. Emitting a bare `break` for a conditional breakpoint —
// which is what this did while conditions were always empty — would paste a
// breakpoint that stops EVERY time into a debugger the user is about to trust.
func exportBreakpointScript(bps []Breakpoint, flavour string) string {
	var b strings.Builder
	n := 0
	for _, bp := range bps {
		if !bp.Enabled {
			continue
		}
		n++
		loc := fmt.Sprintf("%s:%d", bp.Path, bp.Line+1)
		switch {
		case bp.Condition == "":
			fmt.Fprintf(&b, "break %s\n", loc)
		case flavour == "pdb":
			fmt.Fprintf(&b, "break %s, %s\n", loc, bp.Condition)
		case flavour == "gdb":
			fmt.Fprintf(&b, "break %s if %s\n", loc, bp.Condition)
		default: // dlv: name it, then condition it by name.
			name := fmt.Sprintf("bp%d", n)
			fmt.Fprintf(&b, "break %s %s\ncondition %s %s\n", name, loc, name, bp.Condition)
		}
	}
	return b.String()
}
