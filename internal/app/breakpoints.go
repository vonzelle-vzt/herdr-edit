// =============================================================================
// File: internal/app/breakpoints.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// breakpoints.go is Lane B stage 1: persistent, gutter-visible, edit-tracking
// breakpoint marks. There is NO debug adapter behind this — no DAP, no
// delve, no process launching. A breakpoint here is a place you mark by
// hand, that survives edits above it (internal/editor/marks.go does the
// line-tracking), and that you export as `break file:line` lines to paste
// into dlv, pdb or gdb yourself.
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
	if m, ok := tab.MarkAt(line); ok && m.Kind == editor.MarkBreakpoint {
		tab.ClearMark(line)
		a.flash(fmt.Sprintf("Breakpoint cleared %s:%d", filepath.Base(tab.Path), line+1))
		return
	}
	tab.SetMark(line, editor.Mark{Kind: editor.MarkBreakpoint, Enabled: true, VerifiedLine: -1})
	a.flash(fmt.Sprintf("Breakpoint set %s:%d — Esc 5 lists, Esc 9 toggles", filepath.Base(tab.Path), line+1))
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
	if !ok || m.Kind != editor.MarkBreakpoint {
		a.flash("No breakpoint on this line")
		return
	}
	m.Enabled = !m.Enabled
	tab.SetMark(line, m)
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
			if m.Kind != editor.MarkBreakpoint && m.Kind != editor.MarkLogpoint {
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
			if m, ok := tab.MarkAt(line); ok && (m.Kind == editor.MarkBreakpoint || m.Kind == editor.MarkLogpoint) {
				tab.ClearMark(line)
			}
		}
	}
	a.breakpoints = nil
	if a.bpStore != nil {
		a.bpStore.Set(a.rootDir, nil)
	}
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
// unconditional breakpoint renders as regardless of flavour; they diverge
// only once a Condition is set (reserved for a later stage — Condition is
// always "" today, so in practice every flavour currently produces
// identical output). The flavour parameter and the branching below exist
// now so that divergence is a one-line change later, not a rewrite of every
// call site.
func exportBreakpointScript(bps []Breakpoint, flavour string) string {
	var b strings.Builder
	for _, bp := range bps {
		if !bp.Enabled {
			continue
		}
		loc := fmt.Sprintf("%s:%d", bp.Path, bp.Line+1)
		switch {
		case bp.Condition == "":
			fmt.Fprintf(&b, "break %s\n", loc)
		case flavour == "pdb":
			fmt.Fprintf(&b, "break %s, %s\n", loc, bp.Condition)
		case flavour == "gdb":
			fmt.Fprintf(&b, "break %s if %s\n", loc, bp.Condition)
		default: // "dlv" has no inline condition syntax on `break` itself.
			fmt.Fprintf(&b, "break %s\n", loc)
		}
	}
	return b.String()
}
