// =============================================================================
// File: internal/app/debugview.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// debugview.go is Lane B stage 3: everything you do once the program has
// STOPPED. Stage 2 got you to a breakpoint; this is stepping (F10/F11/F12),
// the call stack, the goroutine list, variable inspection, and a console
// holding what the program printed.
//
// # Nothing here is a new widget
//
// The call stack, the thread list and the variables tree are all the COMMAND
// PALETTE with an override list (palette.go) — the same mechanism the outline,
// code actions and workspace symbols already go through. The debug console is
// an editor.NewSyntheticTab, the same mechanism as the diff view, search
// results and the problems list. That is not laziness: a debugger pane would be
// a second geometry path beside the editor's, and word wrap already taught this
// codebase what that costs (see CLAUDE.md). A picker inherits fuzzy search,
// keyboard selection and mouse clicks; a synthetic tab inherits scrolling,
// selection, search and the tab bar.
//
// 🔴 Pickers are opened ONLY through openPaletteWith, never by assigning
// a.paletteOverride. openPaletteWith returns early on an empty list without
// touching any state, so a caller that hands it nothing leaves whatever was
// there before — and a stale override turns the next Esc k into last week's
// list. Every entry point below therefore checks for emptiness itself and
// flashes instead.
//
// # Stepping is asynchronous
//
// next / stepIn / stepOut return the moment the adapter accepts them. Where the
// program ended up arrives later as a `stopped` event, so none of the three
// touches the cursor, the ▶ or the status bar. All of that happens on stage 2's
// existing route — fetchStack → debugStoppedEvent → handleDebugStopped — which
// is why a step needs no painting code of its own.
package app

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudmanic/spice-edit/internal/dap"
	"github.com/cloudmanic/spice-edit/internal/editor"
)

// maxDebugVariables caps how many variables are read from one reference.
//
// 🔴 This cap is LOAD-BEARING, not a second line of defence. A Go slice of a
// million elements answers `variables` with a million entries, and delve does
// NOT advertise supportsVariablePaging (measured, 1.27 — the live oracle logs
// it), so start/count are never even sent to it. Nothing between the adapter
// and this number bounds the answer.
//
// A silently capped list is a lie, so whenever it bites, the picker says so —
// see variablePickerTitle and TestVariablesTruncationIsStated.
const maxDebugVariables = 200

// debugValueWidth is how much of a variable's rendered value fits on a picker
// row before it is elided. A struct's Value can be several hundred characters;
// the row is about seventy.
const debugValueWidth = 40

// debugConsoleLabel names the console tab. It ends in .log so the synthetic
// tab's HighlightKey picks a lexer, exactly like the diff view's ".diff".
const debugConsoleLabel = "debug-console.log"

// debugVar is one variable, argument or field, as it will be shown.
//
// Ref is the adapter's variablesReference: non-zero means the value has
// children and Enter expands it, zero means it is a leaf. Value is already
// rendered by the adapter — display text, never something to parse.
type debugVar struct {
	Name  string
	Value string
	Type  string
	Ref   int
	Scope string // which scope it came from, for a multi-scope frame
}

// debugVarPage is one answer to a variables request, with an honest account of
// what was left out.
//
// total is how many the adapter said exist, or 0 when it did not say — which is
// what delve does (named/indexed both come back 0, measured). truncated is
// observed rather than inferred: fetchVariablePage asks for one MORE than the
// cap, so "there is more" is a fact about the response rather than a guess from
// a length that equals the cap by coincidence.
type debugVarPage struct {
	vars      []debugVar
	total     int
	truncated bool
}

// debugVarsEvent carries one page of variables from the fetch goroutine to the
// main loop, where the picker is opened and the cache is written.
type debugVarsEvent struct {
	when  time.Time
	title string
	ref   int // the reference this page came from; 0 for a whole-frame fetch
	page  debugVarPage
	err   string
}

func (e *debugVarsEvent) When() time.Time { return e.when }

// debugThreadsEvent carries the goroutine list back to the main loop.
type debugThreadsEvent struct {
	when    time.Time
	threads []dap.Thread
	err     string
}

func (e *debugThreadsEvent) When() time.Time { return e.when }

// -----------------------------------------------------------------------------
// Menu inventory
// -----------------------------------------------------------------------------

// debugMenuGroup is the action menu's Debug group, and the SINGLE definition of
// what the debugger can do.
//
// 🔴 It lives here rather than inline in builtinMenuGroups because it has two
// consumers: the ≡ menu (and therefore the command palette, which flattens the
// menu) and the Esc 5 debug picker. A second hand-written list in the picker
// would be a second thing to forget, and the symptom would be a command that
// exists in one surface and not the other — the same failure mode the image
// -paste oracle hit when it restated a list of panels instead of parsing one.
//
// Order is by how often a hand reaches for it mid-session, not alphabetically:
// start/step/stop first, then the three inspectors, then breakpoints.
func debugMenuGroup() []menuItemDef {
	return []menuItemDef{
		{shortcut: "F5", action: (*App).menuDebugStartOrContinue, enabled: (*App).canStartOrContinueDebug, labelFor: (*App).debugStartLabel},
		{label: "Step over", shortcut: "F10", action: (*App).menuDebugStepOver, enabled: (*App).hasStoppedDebugSession},
		{label: "Step into", shortcut: "F11", action: (*App).menuDebugStepIn, enabled: (*App).hasStoppedDebugSession},
		{label: "Step out", shortcut: "F12", action: (*App).menuDebugStepOut, enabled: (*App).hasStoppedDebugSession},
		{label: "Pause", shortcut: "F6", action: (*App).menuDebugPause, enabled: (*App).hasRunningDebugSession},
		{label: "Stop debugging", action: (*App).menuDebugStop, enabled: (*App).hasDebugSession},
		{label: "Call stack", action: (*App).menuDebugStack, enabled: (*App).hasStoppedDebugSession},
		{label: "Goroutines (threads)", action: (*App).menuDebugThreads, enabled: (*App).hasDebugSession},
		{label: "Variables", action: (*App).menuDebugVariables, enabled: (*App).hasStoppedDebugSession},
		{label: "Debug console", action: (*App).menuDebugConsole, enabled: (*App).hasDebugConsole},
		{label: "Toggle breakpoint", shortcut: "Esc 9 / F9", action: (*App).menuToggleBreakpoint, enabled: (*App).hasBreakpointableTab},
		{label: "Toggle breakpoint enabled", action: (*App).menuToggleBreakpointEnabled, enabled: (*App).hasBreakpointableTab},
		{label: "List breakpoints", action: (*App).menuListBreakpoints, enabled: (*App).hasBreakpoints},
		{label: "Clear breakpoints", action: (*App).menuClearBreakpoints, enabled: (*App).hasBreakpoints},
		{label: "Export breakpoints (dlv)", action: (*App).menuExportBreakpoints, enabled: (*App).hasBreakpoints},
	}
}

// menuDebugPicker offers every debug action as a picker. Bound to Esc 5.
//
// This is where the actions with no key of their own live. There is no leader
// rune left to give them: every lowercase letter is either bound or reserved
// for the terminal's own clipboard (c / x / v), Ctrl- is forbidden fork-wide,
// and a shifted F-key needs modifyOtherKeys and is unreliable through a
// multiplexer. One key onto a fuzzy-searchable list of all of them is better
// than eight keys nobody can remember.
//
// Esc 5 previously opened the breakpoint list directly; that is now a row in
// here, one keystroke further away and reachable by typing "br".
func (a *App) menuDebugPicker() {
	a.closeMenu()
	rows := debugMenuGroup()
	cmds := make([]paletteCommand, 0, len(rows))
	for _, it := range rows {
		label := it.label
		if it.labelFor != nil {
			label = it.labelFor(a)
		}
		if label == "" || it.action == nil {
			continue
		}
		enabled := it.enabled
		if enabled == nil {
			enabled = alwaysTrue
		}
		cmds = append(cmds, paletteCommand{
			label:    label,
			shortcut: it.shortcut,
			action:   it.action,
			enabled:  enabled,
		})
	}
	if len(cmds) == 0 {
		a.flash("No debug actions available")
		return
	}
	a.openPaletteWith(cmds, "Debug")
}

// -----------------------------------------------------------------------------
// Enablement predicates
// -----------------------------------------------------------------------------

// hasStoppedDebugSession gates everything that only makes sense at a stop:
// stepping, the call stack, and variables. A frame id and a variablesReference
// are both meaningless while the program is running.
func (a *App) hasStoppedDebugSession() bool {
	return a.debug != nil && a.debug.stopped
}

// hasRunningDebugSession gates Pause: there must be a session and it must not
// already be stopped.
func (a *App) hasRunningDebugSession() bool {
	return a.debug != nil && !a.debug.stopped
}

// hasDebugConsole gates the console. Deliberately true AFTER the session ends —
// the output of a program that ran to completion is exactly what you want to
// read, and handleDAPTerminated keeps it in lastDebugOutput for that reason.
func (a *App) hasDebugConsole() bool {
	return a.debug != nil || len(a.lastDebugOutput) > 0
}

// -----------------------------------------------------------------------------
// Stepping
// -----------------------------------------------------------------------------

// beginStep runs the guards the three stepping commands share and hands back
// what the request needs.
//
// It also puts the session into the resuming state BEFORE the request is sent:
// the program is about to move, so the ▶ where it currently sits, the frame
// list and every cached variablesReference are already wrong. Waiting for the
// stopped event to clear them would leave a window in which the picker answers
// with the previous line's values.
func (a *App) beginStep(what string) (*dap.Client, int, bool) {
	a.closeMenu()

	// 🔴 Both refusals name the action. Three keys that all answer "no debug
	// session" are three keys nobody can tell apart — including a test, which
	// is how a stepping key wired to the wrong method would pass a call-site
	// oracle. The message is also better for the user: it says what you just
	// pressed, not merely what is missing.
	if a.debug == nil || a.debug.client == nil {
		a.flash(what + " needs a debug session — F5 starts one")
		return nil, 0, false
	}
	if !a.debug.stopped {
		a.flash(what + " needs the program to be stopped — F6 pauses it")
		return nil, 0, false
	}
	client, thread := a.debug.client, a.debug.threadID
	a.debug.stopped = false
	a.debug.path, a.debug.frame, a.debug.reason = "", "", ""
	a.debug.invalidateStop()
	return client, thread, true
}

// menuDebugStepOver runs the current line, treating any call on it as a single
// step. Bound to F10.
//
// 🔴 The response says only that delve accepted the request; the new location
// arrives as a `stopped` event and is painted by stage 2's handler. Reading a
// stack trace here would report the line we were already on.
func (a *App) menuDebugStepOver() {
	client, thread, ok := a.beginStep("Step over")
	if !ok {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), debugRequestTimeout)
		defer cancel()
		if err := client.Next(ctx, thread); err != nil {
			a.resumeFailed(client, thread, "step over", err)
		}
	}()
	a.flash("Step over…")
}

// menuDebugStepIn descends into the call on the current line. Bound to F11.
// Asynchronous — see menuDebugStepOver.
func (a *App) menuDebugStepIn() {
	client, thread, ok := a.beginStep("Step into")
	if !ok {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), debugRequestTimeout)
		defer cancel()
		if err := client.StepIn(ctx, thread); err != nil {
			a.resumeFailed(client, thread, "step into", err)
		}
	}()
	a.flash("Step into…")
}

// menuDebugStepOut runs to the return of the current function and stops in the
// caller. Bound to F12. Asynchronous — see menuDebugStepOver.
func (a *App) menuDebugStepOut() {
	client, thread, ok := a.beginStep("Step out")
	if !ok {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), debugRequestTimeout)
		defer cancel()
		if err := client.StepOut(ctx, thread); err != nil {
			a.resumeFailed(client, thread, "step out", err)
		}
	}()
	a.flash("Step out…")
}

// menuDebugPause interrupts a running program so it reports where it is. Bound
// to F6.
//
// Unlike the three above it does NOT pre-clear the stopped state: the program
// is already running, and pause either succeeds — producing a stopped event
// that fills everything in — or fails, leaving the session exactly as it was.
func (a *App) menuDebugPause() {
	a.closeMenu()
	if a.debug == nil || a.debug.client == nil {
		a.flash("No debug session — F5 starts one")
		return
	}
	if a.debug.stopped {
		a.flash("The program is already stopped")
		return
	}
	client, thread := a.debug.client, a.debug.threadID
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), debugRequestTimeout)
		defer cancel()
		if err := client.Pause(ctx, thread); err != nil {
			a.post(&debugLogEvent{when: time.Now(), msg: "pause: " + err.Error()})
		}
	}()
	a.flash("Pausing…")
}

// -----------------------------------------------------------------------------
// Call stack
// -----------------------------------------------------------------------------

// menuDebugStack offers the stopped thread's frames as a picker, innermost
// first. Rows read "main.go:42  handleEvent".
//
// The frames are already in hand — fetchStack carried the whole stack with the
// stop — so this costs no adapter round trip and cannot show a stack belonging
// to a different moment than the ▶ on screen.
func (a *App) menuDebugStack() {
	a.closeMenu()
	if a.debug == nil {
		a.flash("No debug session — F5 starts one")
		return
	}
	if !a.debug.stopped {
		a.flash("The call stack is only available while the program is stopped")
		return
	}
	if len(a.debug.frames) == 0 {
		a.flash("The adapter reported no stack frames")
		return
	}
	cmds := make([]paletteCommand, 0, len(a.debug.frames))
	for i, f := range a.debug.frames {
		i, f := i, f
		where := "??"
		if f.Path != "" {
			where = fmt.Sprintf("%s:%d", filepath.Base(f.Path), f.Line+1)
		}
		shortcut := "#" + itoa(i)
		if i == a.debug.curFrame {
			shortcut += " ←"
		}
		cmds = append(cmds, paletteCommand{
			label:    where + "  " + f.Name,
			shortcut: shortcut,
			enabled:  alwaysTrue,
			action:   func(app *App) { app.selectFrame(i) },
		})
	}
	a.openPaletteWith(cmds, "Call stack")
}

// selectFrame moves the debugger's attention to one frame: the editor jumps to
// its line, the ▶ follows, and variables are re-scoped to it.
//
// 🔴 The variables cache is dropped even though the references are still valid
// within this stop. They are keyed by reference, so keeping them would be
// correct — but "correct because of an invariant three functions away" is how
// the perishable-handle bug gets reintroduced by someone adding a fourth caller.
// One extra round trip on a deliberate user gesture is not a cost worth
// defending against with that reasoning.
func (a *App) selectFrame(idx int) {
	if a.debug == nil || idx < 0 || idx >= len(a.debug.frames) {
		a.flash("That stack frame is no longer available")
		return
	}
	f := a.debug.frames[idx]
	a.debug.dropVariableCache() // the stop is still live — only the frame's references go
	a.debug.curFrame = idx
	a.debug.path, a.debug.line, a.debug.frame = f.Path, f.Line, f.Name

	if f.Path == "" {
		a.flash("Frame " + f.Name + " has no source available")
		return
	}
	tab := a.activeTabPtr()
	if tab == nil || tab.Path != f.Path {
		a.openFile(f.Path)
		tab = a.activeTabPtr()
	}
	if tab == nil {
		a.flash("Stopped in " + filepath.Base(f.Path) + ", which could not be opened")
		return
	}
	tab.MoveCursorTo(editor.Position{Line: f.Line, Col: 0}, false)
	a.flash(fmt.Sprintf("Frame #%d %s — %s:%d", idx, f.Name, filepath.Base(f.Path), f.Line+1))
}

// -----------------------------------------------------------------------------
// Threads (goroutines)
// -----------------------------------------------------------------------------

// menuDebugThreads asks the adapter which goroutines exist and offers them as a
// picker. The request runs on a goroutine like every other adapter call — a Go
// program can have thousands, and building that list on the main loop would
// freeze the editor for however long the debugger takes.
func (a *App) menuDebugThreads() {
	a.closeMenu()
	if a.debug == nil || a.debug.client == nil {
		a.flash("No debug session — F5 starts one")
		return
	}
	client := a.debug.client
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), debugRequestTimeout)
		defer cancel()
		th, err := client.Threads(ctx)
		ev := &debugThreadsEvent{when: time.Now(), threads: th}
		if err != nil {
			ev.err = err.Error()
		}
		a.post(ev)
	}()
	a.flash("Reading goroutines…")
}

// handleDebugThreads opens the goroutine picker.
func (a *App) handleDebugThreads(e *debugThreadsEvent) {
	if a.debug == nil {
		return // the session ended while the request was in flight
	}
	if e.err != "" {
		a.flash("Goroutines: " + e.err)
		return
	}
	if len(e.threads) == 0 {
		a.flash("The adapter reported no goroutines")
		return
	}
	cmds := make([]paletteCommand, 0, len(e.threads))
	for _, th := range e.threads {
		th := th
		name := th.Name
		if name == "" {
			name = "goroutine " + itoa(th.ID)
		}
		shortcut := "#" + itoa(th.ID)
		if th.ID == a.debug.threadID {
			shortcut += " ←"
		}
		cmds = append(cmds, paletteCommand{
			label:    name,
			shortcut: shortcut,
			enabled:  alwaysTrue,
			action:   func(app *App) { app.selectThread(th.ID) },
		})
	}
	a.openPaletteWith(cmds, "Goroutines")
}

// selectThread makes another goroutine the one every subsequent request is
// aimed at, and shows where it is.
//
// It goes back through fetchStack rather than moving the cursor itself, so the
// ▶, the cursor, the status bar and the frame list all update by the same route
// a real stop uses. Anything else would be a second place for the coordinate
// conversion to live.
func (a *App) selectThread(id int) {
	if a.debug == nil || a.debug.client == nil {
		a.flash("No debug session — F5 starts one")
		return
	}
	client := a.debug.client
	a.debug.threadID = id
	a.debug.invalidateStop()
	go a.fetchStack(client, id, "goroutine "+itoa(id))
	a.flash("Switching to goroutine " + itoa(id) + "…")
}

// -----------------------------------------------------------------------------
// Variables
// -----------------------------------------------------------------------------

// menuDebugVariables shows the selected frame's variables — one level, with
// expandable values opening a further picker on Enter.
func (a *App) menuDebugVariables() {
	a.closeMenu()
	if a.debug == nil || a.debug.client == nil {
		a.flash("No debug session — F5 starts one")
		return
	}
	if !a.debug.stopped {
		a.flash("Variables are only readable while the program is stopped")
		return
	}
	f, ok := a.currentFrame()
	if !ok {
		a.flash("No stack frame to read variables from")
		return
	}
	client := a.debug.client
	go a.loadFrameVariables(client, f.ID, "Variables — "+f.Name)
	a.flash("Reading variables…")
}

// currentFrame returns the frame the user has selected, defaulting to the
// innermost one.
func (a *App) currentFrame() (debugFrame, bool) {
	if a.debug == nil || len(a.debug.frames) == 0 {
		return debugFrame{}, false
	}
	i := a.debug.curFrame
	if i < 0 || i >= len(a.debug.frames) {
		i = 0
	}
	return a.debug.frames[i], true
}

// loadFrameVariables walks one frame's scopes and posts a single merged page.
// Runs on a goroutine.
//
// 🔴 frameID is a StackFrame.ID and not a thread id. Delve answers a scopes
// request for the wrong id with somebody else's variables rather than an error,
// so this is a bug that renders — which is why the live oracle asserts a value
// the program actually computed rather than merely that variables came back.
//
// Expensive scopes are NOT fetched. The protocol marks a scope expensive to say
// "do not walk this without being asked" (delve uses it for package globals),
// and eagerly reading one would put that cost on every stop. They appear as
// expandable rows instead, so the user can pay for it deliberately.
func (a *App) loadFrameVariables(client *dap.Client, frameID int, title string) {
	ctx, cancel := context.WithTimeout(context.Background(), debugRequestTimeout)
	defer cancel()

	scopes, err := client.Scopes(ctx, frameID)
	if err != nil {
		a.post(&debugVarsEvent{when: time.Now(), title: title, err: err.Error()})
		return
	}

	var page debugVarPage
	for _, sc := range scopes {
		if sc.VariablesReference == 0 {
			continue
		}
		if sc.Expensive {
			page.vars = append(page.vars, debugVar{
				Name:  sc.Name,
				Value: "(expensive — Enter to load)",
				Ref:   sc.VariablesReference,
				Scope: sc.Name,
			})
			continue
		}
		sub, err := fetchVariablePage(ctx, client, sc.VariablesReference, sc.Total(), sc.Name)
		if err != nil {
			a.post(&debugLogEvent{when: time.Now(), msg: "variables in " + sc.Name + ": " + err.Error()})
			continue
		}
		page.vars = append(page.vars, sub.vars...)
		page.total += sub.total
		page.truncated = page.truncated || sub.truncated
	}
	// A whole-frame page spans several references, so there is no single key to
	// cache it under; ref stays 0 and handleDebugVars skips the write.
	a.post(&debugVarsEvent{when: time.Now(), title: title, page: page})
}

// loadVariableRef expands one reference — a struct's fields, a slice's
// elements, an expensive scope. Runs on a goroutine.
func (a *App) loadVariableRef(client *dap.Client, ref int, scope, title string) {
	ctx, cancel := context.WithTimeout(context.Background(), debugRequestTimeout)
	defer cancel()

	page, err := fetchVariablePage(ctx, client, ref, 0, scope)
	if err != nil {
		a.post(&debugVarsEvent{when: time.Now(), title: title, ref: ref, err: err.Error()})
		return
	}
	a.post(&debugVarsEvent{when: time.Now(), title: title, ref: ref, page: page})
}

// fetchVariablePage reads one reference and caps it.
//
// 🔴 It asks for maxDebugVariables+1 so truncation is OBSERVED rather than
// inferred. A collection whose length happens to equal the cap would otherwise
// be reported as truncated when it is complete, and — worse — the "did the
// adapter honour our window?" question would have no answer at all. The extra
// entry is discarded; its only job is to prove there was a next one.
//
// total is what the CONTAINER said it holds, or 0 when it did not say. Delve
// says 0 (measured), so most truncation notices name a count of how many are
// shown rather than a fraction.
func fetchVariablePage(ctx context.Context, client *dap.Client, ref, total int, scope string) (debugVarPage, error) {
	raw, err := client.Variables(ctx, ref, 0, maxDebugVariables+1)
	if err != nil {
		return debugVarPage{}, err
	}
	page := debugVarPage{total: total}
	if len(raw) > maxDebugVariables {
		raw = raw[:maxDebugVariables]
		page.truncated = true
	}
	page.vars = make([]debugVar, 0, len(raw))
	for _, v := range raw {
		page.vars = append(page.vars, debugVar{
			Name:  v.Name,
			Value: v.Value,
			Type:  v.Type,
			Ref:   v.VariablesReference,
			Scope: scope,
		})
	}
	return page, nil
}

// handleDebugVars caches the page and opens it as a picker.
func (a *App) handleDebugVars(e *debugVarsEvent) {
	if a.debug == nil {
		return // the session ended while the request was in flight
	}
	if e.err != "" {
		a.flash("Variables: " + e.err)
		return
	}
	if len(e.page.vars) == 0 {
		a.flash("No variables in " + e.title)
		return
	}
	if e.ref != 0 {
		if a.debug.varCache == nil {
			a.debug.varCache = make(map[int]debugVarPage)
		}
		a.debug.varCache[e.ref] = e.page
	}
	a.openVariablePicker(e.title, e.page)
}

// openVariablePicker turns a page into palette rows.
func (a *App) openVariablePicker(title string, page debugVarPage) {
	cmds := variableCommands(title, page)
	if len(cmds) == 0 {
		a.flash("No variables in " + title)
		return
	}
	a.openPaletteWith(cmds, variablePickerTitle(title, page))
}

// variableCommands builds one row per variable, plus the truncation notice when
// there is one.
//
// Kept a plain function of (title, page) — no App — so the row text can be
// pinned down without a screen, the same reasoning renderSearchResults and
// renderProblems document for themselves.
func variableCommands(title string, page debugVarPage) []paletteCommand {
	cmds := make([]paletteCommand, 0, len(page.vars)+1)
	for _, v := range page.vars {
		v := v
		label := v.Name + " = " + truncateEllipsis(flattenValue(v.Value), debugValueWidth)
		if v.Ref != 0 {
			label = "▸ " + label // expandable: Enter opens its children
		}
		shortcut := v.Type
		if shortcut == "" {
			shortcut = v.Scope
		}
		cmds = append(cmds, paletteCommand{
			label:    label,
			shortcut: shortcut,
			enabled:  alwaysTrue,
			action:   func(app *App) { app.expandVariable(title, v) },
		})
	}
	if page.truncated {
		note := truncationNotice(page)
		cmds = append(cmds, paletteCommand{
			label:    note,
			enabled:  alwaysTrue,
			action:   func(app *App) { app.flash(note) },
			shortcut: "capped",
		})
	}
	return cmds
}

// truncationNotice is the row that admits what was left out.
//
// 🔴 A silently capped list is a lie: a user reading 200 of a slice's million
// elements and seeing no note has been told that slice has 200 elements. This
// is the same rule the workspace search's "truncated" suffix and the problems
// list's "not a full workspace scan" header already follow.
func truncationNotice(page debugVarPage) string {
	if page.total > len(page.vars) {
		return fmt.Sprintf("… showing the first %d of %d — the list was truncated",
			len(page.vars), page.total)
	}
	return fmt.Sprintf("… showing the first %d — the list was truncated", len(page.vars))
}

// variablePickerTitle names the picker and, when the list was capped, says so
// in the one place that is always on screen.
//
// The notice is in the TITLE as well as in a row because the palette shows ten
// rows at a time and filters them as you type: a note sitting at position 201
// is a note nobody will ever see. The title survives both.
func variablePickerTitle(base string, page debugVarPage) string {
	if !page.truncated {
		return base
	}
	if page.total > len(page.vars) {
		return fmt.Sprintf("%s — %d of %d (truncated)", base, len(page.vars), page.total)
	}
	return fmt.Sprintf("%s — first %d (truncated)", base, len(page.vars))
}

// expandVariable opens a variable's children, or shows a leaf's full value.
//
// A leaf still does something on Enter deliberately: a picker row whose value
// was elided to fit is exactly the row you press Enter on, and having nothing
// happen reads as the picker being broken.
func (a *App) expandVariable(parentTitle string, v debugVar) {
	if v.Ref == 0 {
		a.flash(v.Name + " = " + flattenValue(v.Value))
		return
	}
	if a.debug == nil || a.debug.client == nil {
		a.flash("The debug session ended")
		return
	}
	if !a.debug.stopped {
		// 🔴 The reference died when the program resumed. Asking anyway would
		// return whatever the adapter has since put behind that number.
		a.flash("The program is running — variables are only readable at a stop")
		return
	}
	title := parentTitle + " › " + v.Name
	if page, ok := a.debug.varCache[v.Ref]; ok {
		a.openVariablePicker(title, page)
		return
	}
	client := a.debug.client
	go a.loadVariableRef(client, v.Ref, v.Scope, title)
	a.flash("Reading " + v.Name + "…")
}

// flattenValue puts a multi-line rendered value onto one line. A struct's Value
// arrives with newlines in it, and a picker row is one line — pasting the raw
// text in would draw the tail of it over whatever is beside the modal.
func flattenValue(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	return strings.Join(strings.Fields(s), " ")
}

// -----------------------------------------------------------------------------
// Debug console
// -----------------------------------------------------------------------------

// menuDebugConsole opens what the debugged program printed as a synthetic tab.
//
// It is a SNAPSHOT, refreshed by re-running the command, and says so in its own
// header. A live-updating console would need the render path to know about a
// buffer that changes underneath it, which is a second geometry problem for a
// feature whose whole job is to be read after the fact.
func (a *App) menuDebugConsole() {
	a.closeMenu()
	if !a.hasDebugConsole() {
		a.flash("No debug output yet — F5 runs the program")
		return
	}
	body := renderDebugConsole(a.debugStatus(), a.debugConsoleLines())

	// Re-opening replaces the console in place rather than stacking a second
	// one, the same reuse showDiff does when flipping a baseline.
	tab := a.activeTabPtr()
	if tab != nil && tab.Synthetic && tab.Label == debugConsoleLabel {
		a.tabs[a.activeTab] = editorSyntheticTab(debugConsoleLabel, body)
	} else {
		a.tabs = append(a.tabs, editorSyntheticTab(debugConsoleLabel, body))
		a.activeTab = len(a.tabs) - 1
	}
	a.flash("Debug console — run it again to refresh; Esc w closes it")
}

// debugConsoleLines returns the live session's output, or the last finished
// run's when there is no session.
func (a *App) debugConsoleLines() []string {
	if a.debug != nil {
		return a.debug.output
	}
	return a.lastDebugOutput
}

// renderDebugConsole lays the console out: a status line, a note about what
// this view is, then the program's output verbatim.
//
// A plain function of (status, lines) so the layout is testable without a
// session or a screen.
func renderDebugConsole(status string, lines []string) string {
	var b strings.Builder
	if status == "" {
		status = "no debug session — this is the last run's output"
	}
	fmt.Fprintf(&b, "%s\n", status)
	fmt.Fprintf(&b, "%s — a snapshot, capped at the last %d lines; re-open to refresh\n\n",
		plural(len(lines), "line"), maxDebugOutput)
	if len(lines) == 0 {
		b.WriteString("(the program has not printed anything)\n")
		return b.String()
	}
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return b.String()
}
