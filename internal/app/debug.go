// =============================================================================
// File: internal/app/debug.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// debug.go is Lane B stage 2: a real debug adapter behind the breakpoint marks
// stage 1 drew. Press F5, the program runs under delve, it stops on your
// breakpoint, the editor opens that file and paints ▶ on the line.
//
// What is here is the SESSION: starting it, continuing it, stopping it, knowing
// where it is, and painting that. What you do once it has stopped — stepping,
// the call stack, goroutines, variables, the console — is stage 3 and lives in
// debugview.go, which owns no session state and reaches the screen only through
// fetchStack → debugStoppedEvent → handleDebugStopped below.
//
// # Everything crosses the goroutine boundary as a posted event
//
// The adapter is a separate process whose answers arrive whenever they arrive,
// and internal/dap calls its handlers from a read goroutine. Nothing in this
// file may touch UI state from there: every callback posts a tcell event and
// the main loop handles it, exactly as diagnostics.go and blame.go already do.
// Calling inline would block Run's PollEvent on a debugger — and unlike gopls,
// a debugger is BUILDING YOUR PACKAGE, which took 7.4 seconds cold when
// measured. That is 7.4 seconds of frozen editor with no way to type.
//
// # The ONE place coordinates convert
//
// internal/dap speaks the protocol's 1-based lines throughout; the editor's
// buffer is 0-based. The conversion happens here and nowhere else — see
// bufLineFromAdapter / adapterLineFromBuf, which are the only two places either
// arithmetic appears.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/cloudmanic/spice-edit/internal/dap"
	"github.com/cloudmanic/spice-edit/internal/editor"
	"github.com/cloudmanic/spice-edit/internal/lsp"
)

// debugStartTimeout bounds bringing a session up. Generous because the adapter
// COMPILES the program first: 7.4s cold on a laptop, and a cold CI runner or a
// large module is slower again.
const debugStartTimeout = 180 * time.Second

// debugRequestTimeout bounds an in-session request (stack trace, continue).
// These are answered from an already-running debugger, so a slow one is wedged
// rather than busy.
const debugRequestTimeout = 20 * time.Second

// maxDebugOutput caps how much program output a session retains. The debuggee
// is a program we do not control and it may print in a loop.
const maxDebugOutput = 200

// maxDebugFrames caps how deep a stack trace is fetched. A runaway recursion
// has a stack of whatever depth the runtime allowed, and the picker is for
// navigating, not for reading a core dump.
const maxDebugFrames = 64

// boundBreakpoint is one breakpoint as the adapter actually bound it, kept so
// the gutter can tell the truth about where execution will really stop.
//
// 🔴 Both lines are 0-based buffer lines here — converted on the way in. The
// two differ whenever the adapter snapped a breakpoint forward to the next
// executable statement, and Verified is false when it could not bind it at all.
type boundBreakpoint struct {
	ID        int
	Requested int // where the user put the mark
	Bound     int // where the adapter will actually stop; == Requested when unmoved
	Verified  bool
	Message   string
}

// debugFrame is one stack frame in BUFFER coordinates.
//
// 🔴 Line is 0-based, converted on the way in by bufLineFromAdapter — this type
// exists so that a frame crossing into the view layer cannot still be carrying
// the adapter's 1-based number. internal/dap's StackFrame is the 1-based shape
// and stops at this file's boundary, which is the rule protocol.go's package
// comment states and the reason there are exactly two places the arithmetic
// appears.
type debugFrame struct {
	ID   int    // the adapter's frame id — what scopes() takes, NOT a thread id
	Name string // the function name, for the picker and the status bar
	Path string // absolute; "" when the adapter has no source for this frame
	Line int    // 0-based buffer line
}

// debugSession is one live debug session.
type debugSession struct {
	client  *dap.Client
	adapter string

	// starting is true between F5 and the adapter finishing configuration.
	// Distinct from running so a second F5 in that window says "starting…"
	// rather than launching a second debugger.
	starting bool
	running  bool

	stopped  bool
	threadID int
	path     string // absolute path of the file we are stopped in
	line     int    // 0-based buffer line we are stopped on
	frame    string // top frame's function name, for the status bar
	reason   string

	// bound maps an absolute path to what the adapter did with that file's
	// breakpoints. Read by the gutter overlay.
	bound map[string][]boundBreakpoint

	// frames is the stopped thread's stack, innermost first, and curFrame is
	// the one the user is looking at. Fetched once per stop by fetchStack, so
	// opening the call stack picker costs no adapter round trip.
	frames   []debugFrame
	curFrame int

	// varCache holds variable pages already fetched during THIS stop, keyed by
	// the adapter's variablesReference.
	//
	// 🔴 PERISHABLE, and this is the most dangerous state in the debugger. A
	// variablesReference is a handle the adapter allocates for one stop; once
	// the program runs again it may reuse the number for something else or
	// reject it. A cache that survived a step would therefore answer with
	// ANOTHER FRAME'S VALUES and no error at all — plausible, wrong, and
	// impossible for the user to detect, which is the worst failure a debugger
	// can have. invalidateStop is called on `continued` AND on `stopped`;
	// TestVariablesCacheDroppedOnStep is the guard.
	varCache map[int]debugVarPage

	output  []string
	lastErr string
}

// dropVariableCache invalidates every cached variables reference, leaving the
// frame list alone. Used when the stop is still valid but what the user is
// looking at within it has changed — selecting another frame.
func (s *debugSession) dropVariableCache() {
	s.varCache = nil
}

// invalidateStop throws away everything that belonged to the stop the program
// has just left: the variable references AND the frame ids, which are handles
// with exactly the same lifetime.
//
// Called from BOTH the continued and the stopped paths, which is not redundancy
// — a program can resume without us asking (continued) and can stop without
// having visibly resumed (a step, which produces stopped with no continued in
// between on some adapters). Handling only one leaves a window in which stale
// handles are served.
func (s *debugSession) invalidateStop() {
	s.dropVariableCache()
	s.frames = nil
	s.curFrame = 0
}

// resumeFailed reports a refused resume — a continue or any of the three steps
// — and puts the session back where it actually is. Runs on the requesting
// goroutine.
//
// 🔴 The recovery matters as much as the message. Every resume marks the
// session as running BEFORE the request goes out, because the program is about
// to move. When the adapter REFUSES, nothing else ever puts that back: the UI
// claims the program is running while it is still sitting on the same line —
// no ▶, no location in the status bar, the inspectors all disabled, and F5
// answering "a session is already running". The only way out would be Stop.
// Re-reading the stack restores the truth through the one route that paints it.
func (a *App) resumeFailed(client *dap.Client, thread int, what string, err error) {
	a.post(&debugLogEvent{when: time.Now(), msg: what + ": " + err.Error()})
	a.fetchStack(client, thread, what+" refused")
}

// debugEvent carries one adapter event onto the tcell queue.
type debugEvent struct {
	when time.Time
	ev   dap.Event
}

func (e *debugEvent) When() time.Time { return e.when }

// debugStartedEvent reports the outcome of bringing a session up, with the
// breakpoints the adapter bound along the way.
type debugStartedEvent struct {
	when    time.Time
	client  *dap.Client
	adapter string
	bound   map[string][]boundBreakpoint
	err     error
}

func (e *debugStartedEvent) When() time.Time { return e.when }

// debugStoppedEvent says where the program stopped, in BUFFER coordinates.
//
// It carries the resolved location rather than a thread id on purpose: fetching
// the stack is a request to the adapter, and doing that on the main loop would
// block the editor. By the time this event exists the answer is already in
// hand — which is also what makes the painting path testable without a live
// debugger.
type debugStoppedEvent struct {
	when     time.Time
	path     string
	line     int // 0-based
	frame    string
	reason   string
	threadID int

	// frames is the whole stack, already converted to buffer coordinates. It
	// travels with the stop rather than being fetched when the user asks for it
	// so that the call-stack picker opens instantly and, more importantly, so
	// the frame ids it hands to scopes() belong to the stop currently on screen
	// rather than to whatever the adapter is doing by then.
	frames []debugFrame
}

func (e *debugStoppedEvent) When() time.Time { return e.when }

// debugLogEvent surfaces an adapter complaint on the status line.
type debugLogEvent struct {
	when time.Time
	msg  string
}

func (e *debugLogEvent) When() time.Time { return e.when }

// bufLineFromAdapter converts a 1-based adapter line to a 0-based buffer line.
// One of exactly two places this arithmetic exists.
func bufLineFromAdapter(line int) int {
	l := line - 1
	if l < 0 {
		return 0
	}
	return l
}

// adapterLineFromBuf converts a 0-based buffer line to a 1-based adapter line.
// The other of exactly two places.
func adapterLineFromBuf(line int) int { return line + 1 }

// hasDebuggableTab reports whether the active tab is something a registered
// adapter can debug.
func (a *App) hasDebuggableTab() bool {
	t := a.activeTabPtr()
	if t == nil || t.Path == "" || t.IsImage() || t.Synthetic {
		return false
	}
	_, ok := dap.AdapterFor(lsp.LanguageID(t.Path))
	return ok
}

// hasDebugSession reports whether a session exists, for menu enablement.
func (a *App) hasDebugSession() bool { return a.debug != nil }

// canStartOrContinueDebug gates the F5 row: either there is something to
// resume, or there is a file a debugger understands.
func (a *App) canStartOrContinueDebug() bool {
	return a.hasDebugSession() || a.hasDebuggableTab()
}

// debugStartLabel is the dynamic label for the F5 menu row, so one row reads
// correctly in all three states rather than three rows being mostly disabled.
func (a *App) debugStartLabel() string {
	if a.debug != nil && a.debug.stopped {
		return "Continue"
	}
	if a.debug != nil && (a.debug.running || a.debug.starting) {
		return "Debugging… (running)"
	}
	return "Start debugging"
}

// menuDebugStartOrContinue is what F5 actually does: start a session when there
// is none, resume when stopped. One key for both is the muscle memory from
// every GUI debugger.
func (a *App) menuDebugStartOrContinue() {
	if a.debug != nil && a.debug.stopped {
		a.menuDebugContinue()
		return
	}
	a.menuStartDebug()
}

// menuStartDebug launches the program under a debug adapter.
//
// Everything after the guards happens on a goroutine and comes back as a posted
// event — see this file's header.
func (a *App) menuStartDebug() {
	a.closeMenu()

	if a.debug != nil && (a.debug.starting || a.debug.running) {
		a.flash("A debug session is already running — Stop debugging first")
		return
	}
	tab := a.activeTabPtr()
	if !a.hasDebuggableTab() {
		a.flash("Nothing here to debug — open a Go file first")
		return
	}
	adapter, ok := dap.AdapterFor(lsp.LanguageID(tab.Path))
	if !ok {
		a.flash("No debug adapter for this file type")
		return
	}
	if a.dapReg == nil {
		a.dapReg = dap.NewRegistry(a.rootDir)
	}

	// Snapshot the breakpoints HERE, on the main goroutine. Reading
	// a.breakpoints from the background one would race the poll in Run that
	// keeps it current.
	bps := a.enabledBreakpoints()
	program := filepath.Dir(tab.Path)

	// A new run starts a clean console; the previous run's output belonged to a
	// different program state and mixing the two silently is worse than losing it.
	a.lastDebugOutput = nil

	a.debug = &debugSession{adapter: adapter.Name, starting: true, bound: map[string][]boundBreakpoint{}}
	go a.runDebugSession(adapter, program, bps)

	if len(bps) == 0 {
		a.flash("Starting " + adapter.Name + " — no breakpoints set, the program will run to completion")
		return
	}
	a.flash(fmt.Sprintf("Starting %s with %d breakpoint(s)…", adapter.Name, len(bps)))
}

// enabledBreakpoints returns the breakpoints worth sending to an adapter.
//
// Disabled ones are skipped: the user turned them off deliberately, and an
// adapter has no notion of a disabled breakpoint — sending one would arm
// something they switched off, which is the same reasoning the dlv export in
// breakpoints.go already follows.
func (a *App) enabledBreakpoints() []Breakpoint {
	all := a.allBreakpoints()
	out := make([]Breakpoint, 0, len(all))
	for _, b := range all {
		if b.Enabled {
			out = append(out, b)
		}
	}
	return out
}

// runDebugSession brings a session up on a background goroutine.
//
// 🔴 The ORDER here is the whole function, and every step of it is a trap:
//
//	initialize → launch SENT (never awaited) → wait for `initialized` →
//	setBreakpoints → setExceptionBreakpoints → configurationDone
//
// Blocking on the launch response before sending breakpoints deadlocks against
// adapters that withhold it until configurationDone; skipping configurationDone
// leaves the program parked forever. Both fail as a hang with nothing logged.
func (a *App) runDebugSession(adapter dap.Adapter, program string, bps []Breakpoint) {
	ctx, cancel := context.WithTimeout(context.Background(), debugStartTimeout)
	defer cancel()

	handlers := dap.Handlers{
		OnEvent: func(e dap.Event) { a.post(&debugEvent{when: time.Now(), ev: e}) },
		OnLog:   func(s string) { a.post(&debugLogEvent{when: time.Now(), msg: s}) },
	}

	fail := func(err error) {
		a.post(&debugStartedEvent{when: time.Now(), adapter: adapter.Name, err: err})
	}

	client, err := a.dapReg.Start(ctx, adapter, handlers)
	if err != nil {
		fail(err)
		return
	}

	caps, err := client.Initialize(ctx, adapter.AdapterID)
	if err != nil {
		client.Stop()
		fail(err)
		return
	}

	cfg := make(map[string]interface{}, len(adapter.Launch)+1)
	for k, v := range adapter.Launch {
		cfg[k] = v
	}
	cfg["program"] = program
	if _, err := client.Launch(cfg); err != nil {
		client.Stop()
		fail(err)
		return
	}

	if err := client.WaitEvent(ctx, dap.EventInitialized); err != nil {
		client.Stop()
		fail(fmt.Errorf("%w (adapter said: %s)", err, strings.Join(client.LastStderr(), " | ")))
		return
	}

	bound := make(map[string][]boundBreakpoint)
	for path, list := range groupBreakpointsByPath(bps) {
		// 🔴 Whole-file, always: setBreakpoints REPLACES the set for a source,
		// so each file's complete list goes in one call.
		sbps := make([]dap.SourceBreakpoint, len(list))
		for i, b := range list {
			sbps[i] = dap.SourceBreakpoint{
				Line:       adapterLineFromBuf(b.Line),
				Condition:  b.Condition,
				LogMessage: b.LogMessage,
			}
		}
		answers, err := client.SetBreakpoints(ctx, dap.Source{Path: path, Name: filepath.Base(path)}, sbps)
		if err != nil {
			// A file the adapter will not accept is not fatal to the session —
			// the others may still bind, and stopping here would throw away a
			// working debug run over one bad path.
			a.post(&debugLogEvent{when: time.Now(), msg: "breakpoints in " + filepath.Base(path) + ": " + err.Error()})
			continue
		}
		bound[path] = boundFromAnswers(list, answers)
	}

	if err := client.SetExceptionBreakpoints(ctx, caps.DefaultFilters()); err != nil {
		a.post(&debugLogEvent{when: time.Now(), msg: "exception breakpoints: " + err.Error()})
	}
	if err := client.ConfigurationDone(ctx); err != nil {
		client.Stop()
		fail(err)
		return
	}

	a.post(&debugStartedEvent{when: time.Now(), client: client, adapter: adapter.Name, bound: bound})
}

// groupBreakpointsByPath buckets breakpoints per file, because setBreakpoints
// is scoped to one source and is whole-file within it.
func groupBreakpointsByPath(bps []Breakpoint) map[string][]Breakpoint {
	out := make(map[string][]Breakpoint)
	for _, b := range bps {
		out[b.Path] = append(out[b.Path], b)
	}
	return out
}

// boundFromAnswers pairs what we asked for with what the adapter did.
//
// 🔴 The answer array matches the request POSITIONALLY — it carries no key back
// to the request other than its index — so this walks both together. An
// unverified breakpoint has no line at all (HasLine is false), and its Bound
// stays at the requested line rather than becoming line 1 of the file.
func boundFromAnswers(asked []Breakpoint, answers []dap.Breakpoint) []boundBreakpoint {
	out := make([]boundBreakpoint, 0, len(asked))
	for i, b := range asked {
		bb := boundBreakpoint{Requested: b.Line, Bound: b.Line}
		if i < len(answers) {
			ans := answers[i]
			bb.ID = ans.ID
			bb.Verified = ans.Verified
			bb.Message = ans.Message
			if ans.HasLine() {
				bb.Bound = bufLineFromAdapter(ans.Line)
			}
		}
		out = append(out, bb)
	}
	return out
}

// menuDebugContinue resumes a stopped program.
func (a *App) menuDebugContinue() {
	a.closeMenu()
	if a.debug == nil || a.debug.client == nil {
		a.flash("No debug session — F5 starts one")
		return
	}
	if !a.debug.stopped {
		a.flash("The program is already running")
		return
	}
	client, thread := a.debug.client, a.debug.threadID
	a.debug.stopped = false
	a.debug.path, a.debug.frame, a.debug.reason = "", "", ""
	a.debug.invalidateStop()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), debugRequestTimeout)
		defer cancel()
		if err := client.Continue(ctx, thread); err != nil {
			a.resumeFailed(client, thread, "continue", err)
		}
	}()
	a.flash("Continuing…")
}

// menuDebugStop ends the session and kills the debuggee.
//
// The session is cleared from the UI immediately rather than waiting for the
// adapter to confirm: shutting a debugger down can take seconds, and leaving ▶
// painted over a program the user just stopped reads as the command not working.
func (a *App) menuDebugStop() {
	a.closeMenu()
	if a.debug == nil {
		a.flash("No debug session to stop")
		return
	}
	// Keep the output, exactly as handleDAPTerminated does: a run the user
	// ended by hand is still a run whose output they may want to read.
	a.lastDebugOutput = a.debug.output
	client := a.debug.client
	a.debug = nil
	if client != nil {
		go client.Stop()
	}
	a.flash("Debug session stopped")
}

// stopDebugSession tears the session down on quit. Called from Run alongside
// lsp.Stop, so a debugged process is never left running after the editor exits.
func (a *App) stopDebugSession() {
	if a.debug == nil {
		return
	}
	client := a.debug.client
	a.debug = nil
	if client != nil {
		client.Stop()
	}
}

// handleDebugStarted records the outcome of a start attempt.
func (a *App) handleDebugStarted(e *debugStartedEvent) {
	if e.err != nil {
		a.debug = nil
		a.flash("Debug: " + e.err.Error())
		return
	}
	if a.debug == nil {
		// The user stopped the session while it was still coming up. Shut the
		// adapter down rather than adopting a session nobody asked for any more.
		go e.client.Stop()
		return
	}
	a.debug.client = e.client
	a.debug.starting = false
	a.debug.running = true
	a.debug.bound = e.bound
	a.applyBoundBreakpoints()

	unverified := 0
	for _, list := range e.bound {
		for _, b := range list {
			if !b.Verified {
				unverified++
			}
		}
	}
	if unverified > 0 {
		a.flash(fmt.Sprintf("%s running — %d breakpoint(s) could not be set on an executable line",
			e.adapter, unverified))
		return
	}
	a.flash(e.adapter + " running")
}

// applyBoundBreakpoints copies the adapter's verification back onto the open
// tabs' marks.
//
// 🔴 It writes ONLY Verified and VerifiedLine, never Kind. Overwriting a
// MarkBreakpoint's Kind would make syncBreakpoints (breakpoints.go) stop seeing
// it as a breakpoint at all — it collects only MarkBreakpoint/MarkLogpoint — so
// the breakpoint would be dropped from the authoritative list AND from the
// persisted file. That is measured behaviour, not a worry: see
// TestStoppedMarkerNeverDestroysABreakpoint.
func (a *App) applyBoundBreakpoints() {
	if a.debug == nil {
		return
	}
	for _, tab := range a.tabs {
		list, ok := a.debug.bound[tab.Path]
		if !ok {
			continue
		}
		for _, b := range list {
			m, exists := tab.MarkAt(b.Requested)
			if !exists || m.Kind != editor.MarkBreakpoint {
				continue
			}
			m.Verified = b.Verified
			m.VerifiedLine = -1
			if b.Verified {
				m.VerifiedLine = b.Bound
			}
			tab.SetMark(b.Requested, m)
		}
	}
}

// handleDAPEvent routes one adapter event.
//
// Every event internal/dap can deliver is handled here. An event that falls
// through silently is how a session ends up looking alive when it is not —
// which is why terminated and exited are as important as stopped.
func (a *App) handleDAPEvent(e *debugEvent) {
	if a.debug == nil {
		return // a stale event from a session the user already stopped
	}
	switch e.ev.Event {
	case dap.EventStopped:
		a.handleDAPStopped(e.ev)

	case dap.EventContinued:
		// The adapter resumed on its own. Clearing the marker here is what
		// keeps ▶ from sitting under a program that is no longer there — and
		// dropping the variables cache is what keeps the next expansion from
		// answering with values read before the program moved.
		a.debug.stopped = false
		a.debug.path, a.debug.frame, a.debug.reason = "", "", ""
		a.debug.invalidateStop()

	case dap.EventOutput:
		a.handleDAPOutput(e.ev)

	case dap.EventTerminated:
		a.handleDAPTerminated(0, false)

	case dap.EventExited:
		var ex dap.ExitedEvent
		_ = decodeBody(e.ev.Body, &ex)
		a.handleDAPTerminated(ex.ExitCode, true)

	case dap.EventBreakpoint:
		a.handleDAPBreakpointChanged(e.ev)

	case dap.EventThread:
		// Goroutines start and exit constantly in a Go program; nothing in
		// stage 2 shows a thread list, so this is recorded and not surfaced.
		// It is handled explicitly rather than by omission so the next stage
		// has an obvious place to hang a thread picker.
		var te dap.ThreadEvent
		_ = decodeBody(e.ev.Body, &te)
		if a.debug.threadID == 0 && te.Reason == "started" {
			a.debug.threadID = te.ThreadID
		}

	case dap.EventCapabilities, dap.EventProcess, dap.EventInitialized:
		// Consumed by internal/dap itself (capabilities) or by the start
		// sequence (initialized); nothing further to do on the UI side.
	}
}

// handleDAPStopped reacts to the program stopping.
//
// It does NOT fetch the stack here: that is a request to the adapter, and doing
// it on the main loop would block the editor for the whole timeout on a wedged
// debugger. The goroutine posts a debugStoppedEvent once it has an answer.
func (a *App) handleDAPStopped(ev dap.Event) {
	// 🔴 FIRST, before any early return. The program ran to get here, so every
	// variablesReference and frame id we hold died on the way — including when
	// the event body is unreadable or there is no client to ask. An
	// invalidation skipped on an error path is a stale handle served on the
	// next expand.
	a.debug.invalidateStop()

	var se dap.StoppedEvent
	if err := decodeBody(ev.Body, &se); err != nil {
		a.flash("Debug: could not read the stopped event")
		return
	}
	a.debug.stopped = true
	a.debug.reason = se.Reason
	if se.ThreadID != 0 {
		a.debug.threadID = se.ThreadID
	}
	client, thread := a.debug.client, a.debug.threadID
	if client == nil {
		return
	}
	go a.fetchStack(client, thread, se.Reason)
}

// fetchStack reads a stopped thread's stack and posts it as a
// debugStoppedEvent. Runs on a goroutine — never call it inline.
//
// 🔴 This is the ONE route by which a new location reaches the UI, and keeping
// it single is the whole reason stepping needed no painting code of its own.
// The ▶ overlay, the cursor jump, the status bar and the frame list are all
// downstream of handleDebugStopped; a step, a stop and a thread switch differ
// only in the reason they pass. A second path that "just moved the cursor"
// would be a second place for the coordinate conversion to be wrong, and the
// two would disagree only in the cases nobody tests.
func (a *App) fetchStack(client *dap.Client, thread int, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), debugRequestTimeout)
	defer cancel()

	frames, err := client.StackTrace(ctx, thread, maxDebugFrames)
	if err != nil || len(frames) == 0 {
		msg := "stopped, but the stack trace was unavailable"
		if err != nil {
			msg = "stopped: " + err.Error()
		}
		a.post(&debugLogEvent{when: time.Now(), msg: msg})
		return
	}

	conv := make([]debugFrame, 0, len(frames))
	for _, f := range frames {
		conv = append(conv, debugFrame{
			ID:   f.ID,
			Name: f.Name,
			Path: f.Source.Path,
			Line: bufLineFromAdapter(f.Line), // 🔴 the one conversion
		})
	}
	top := conv[0]
	a.post(&debugStoppedEvent{
		when:     time.Now(),
		path:     top.Path,
		line:     top.Line,
		frame:    top.Name,
		reason:   reason,
		threadID: thread,
		frames:   conv,
	})
}

// handleDebugStopped opens the file the program stopped in and puts the cursor
// on the line. The ▶ itself is painted by drawDebugGutter, from the state this
// records.
func (a *App) handleDebugStopped(e *debugStoppedEvent) {
	if a.debug == nil {
		return
	}
	a.debug.stopped = true
	a.debug.path = e.path
	a.debug.line = e.line
	a.debug.frame = e.frame
	a.debug.reason = e.reason
	a.debug.frames = e.frames
	a.debug.curFrame = 0 // a new stop always lands on the innermost frame
	if e.threadID != 0 {
		a.debug.threadID = e.threadID
	}

	if e.path == "" {
		a.flash("Stopped in a file with no source available")
		return
	}
	tab := a.activeTabPtr()
	if tab == nil || tab.Path != e.path {
		a.openFile(e.path)
		tab = a.activeTabPtr()
	}
	if tab == nil {
		a.flash("Stopped in " + filepath.Base(e.path) + ", which could not be opened")
		return
	}
	tab.MoveCursorTo(editor.Position{Line: e.line, Col: 0}, false)
	a.flash(fmt.Sprintf("Stopped (%s) %s:%d", e.reason, filepath.Base(e.path), e.line+1))
}

// handleDAPOutput records the debugged program's output.
//
// 🔴 This is the ONLY channel through which a debuggee's stdout may reach the
// user. The adapter is spawned with its stdio pointed away from the terminal
// (internal/dap), because a debugged program's fmt.Println landing on our real
// stdout would scribble straight over the tcell screen.
func (a *App) handleDAPOutput(ev dap.Event) {
	var oe dap.OutputEvent
	if err := decodeBody(ev.Body, &oe); err != nil {
		return
	}
	text := strings.TrimRight(oe.Output, "\r\n")
	if text == "" {
		return
	}
	a.debug.output = append(a.debug.output, text)
	if len(a.debug.output) > maxDebugOutput {
		a.debug.output = a.debug.output[len(a.debug.output)-maxDebugOutput:]
	}
}

// handleDAPTerminated ends the session.
//
// Reached both by a real terminated/exited event and by the synthetic one
// internal/dap posts when the adapter dies mid-session — which is the case that
// matters, because otherwise the UI sits in "stopped" with F5 bound to a dead
// client and nothing on screen to say why.
func (a *App) handleDAPTerminated(exitCode int, haveCode bool) {
	if a.debug == nil {
		return
	}
	last := a.lastProgramOutput()
	client := a.debug.client

	// Keep the program's output alive past the session that produced it. The
	// debug console is the only place a run's output can be READ, and a console
	// that empties the instant the program exits is a console you can never use
	// on a program that ran to completion — which is most of them.
	a.lastDebugOutput = a.debug.output

	a.debug = nil // clears the ▶ by construction: drawDebugGutter reads this
	if client != nil {
		go client.Stop()
	}

	msg := "Debug session ended"
	if haveCode {
		msg = fmt.Sprintf("Program exited (%d)", exitCode)
	}
	if last != "" {
		msg += " · " + last
	}
	a.flash(msg)
}

// lastProgramOutput returns the final line the program printed, so a run that
// produced a result does not end with that result invisible. A full output
// panel is a later stage; one line covers the common case of a program that
// prints an answer.
func (a *App) lastProgramOutput() string {
	if a.debug == nil {
		return ""
	}
	for i := len(a.debug.output) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(a.debug.output[i]); s != "" {
			return truncateEllipsis(s, 60)
		}
	}
	return ""
}

// handleDAPBreakpointChanged applies a late change to a breakpoint's binding.
//
// Adapters resolve breakpoints lazily — a breakpoint in a package that is not
// loaded yet can go from unverified to verified once it is. Without this the
// gutter keeps showing a hollow ○ for something that now works.
func (a *App) handleDAPBreakpointChanged(ev dap.Event) {
	var be dap.BreakpointEvent
	if err := decodeBody(ev.Body, &be); err != nil || be.Breakpoint.ID == 0 {
		return
	}
	for path, list := range a.debug.bound {
		for i := range list {
			if list[i].ID != be.Breakpoint.ID {
				continue
			}
			list[i].Verified = be.Breakpoint.Verified
			list[i].Message = be.Breakpoint.Message
			if be.Breakpoint.HasLine() {
				list[i].Bound = bufLineFromAdapter(be.Breakpoint.Line)
			}
			a.debug.bound[path] = list
			a.applyBoundBreakpoints()
			return
		}
	}
}

// decodeBody unmarshals an event body, treating an absent one as a zero value
// rather than an error — several events legitimately carry none.
func decodeBody(body json.RawMessage, v interface{}) error {
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, v)
}

// drawDebugGutter paints the debugger's view of the gutter ON TOP of the
// rendered tab: ▶ where execution is stopped, ○ where a breakpoint could not be
// bound, ● where the adapter moved one.
//
// 🔴 This is an OVERLAY rather than an editor.Mark, and that is a deliberate
// departure worth explaining. Writing Mark{Kind: MarkStopped} onto the stopped
// line is the obvious implementation and it DESTROYS DATA: Tab.Marks is keyed
// by line, so the stopped mark replaces any breakpoint on that same line — and
// stopping on a breakpoint is the only thing that happens in this stage.
// syncBreakpoints (breakpoints.go) then sees no MarkBreakpoint there, drops it
// from the authoritative list, and persists the deletion. Measured: one
// syncBreakpoints tick took a.breakpoints from one entry to zero.
//
// Drawing over the top instead leaves Tab.Marks untouched, which is exactly the
// discipline drawDiagnostics already follows for the same reason — the render
// path and the overlay never have to agree about anything but cell coordinates.
//
// Wrapped tabs are skipped, matching drawDiagnosticsInline: mapping a buffer
// line to a wrapped screen row needs internal/editor's unexported segment
// arithmetic, and a marker painted on the wrong row is worse than none. The
// cursor still moves to the stopped line and the status bar still names it.
func (a *App) drawDebugGutter() {
	if a.debug == nil {
		return
	}
	tab := a.activeTabPtr()
	if tab == nil || tab.IsImage() || tab.Synthetic || tab.Path == "" {
		return
	}
	ex, ey, ew, eh := a.editorRect()

	// The gutter marker is painted at the editor rect's leftmost column, which
	// is where Tab.Render puts it (tab.go: scr.SetContent(x, cy, markerR, …)).
	//
	// Tab.GutterRowFor resolves the row for BOTH geometries. This used to bail
	// out on tab.Wrap entirely, because `line - ScrollY` is off by the number of
	// continuation rows above the line and painting in the wrong place is worse
	// than not painting — so a wrapped tab showed no breakpoint dots and no
	// stopped arrow, with only the status bar saying where execution had
	// stopped. Skipping was the right call while the arithmetic lived here; the
	// fix was to ask the geometry that owns wrapping.
	paint := func(line int, glyph rune, color tcell.Color) {
		row, ok := tab.GutterRowFor(line, ew, eh)
		if !ok {
			return // scrolled out of view, or not a line in this buffer
		}
		_, _, existing, _ := a.screen.GetContent(ex, ey+row)
		bg, _, _ := existing.Decompose()
		a.screen.SetContent(ex, ey+row, glyph, nil,
			tcell.StyleDefault.Background(bg).Foreground(color))
	}

	for _, b := range a.debug.bound[tab.Path] {
		switch {
		case !b.Verified:
			// A hollow dot where the adapter refused to bind, so the user can
			// see the breakpoint will not be hit rather than wondering why.
			paint(b.Requested, '○', a.theme.Muted)
		case b.Bound != b.Requested:
			// The adapter snapped it forward to the next real statement. Show
			// where it will ACTUALLY stop, or ▶ later lands where no ● is.
			paint(b.Bound, '●', a.theme.Error)
		}
	}

	// Painted last so the stopped line wins the cell over any breakpoint glyph.
	if a.debug.stopped && a.debug.path == tab.Path {
		paint(a.debug.line, '▶', a.theme.GitAdded)
	}
}

// debugStatus is the one-line status-bar summary of the session — the sibling
// of diagnosticStatus in diagnostics.go, and the answer to "is it running, and
// where am I".
func (a *App) debugStatus() string {
	if a.debug == nil {
		return ""
	}
	if a.debug.starting {
		return "debug: starting " + a.debug.adapter + "…"
	}
	if a.debug.stopped {
		where := "debug: stopped"
		if a.debug.reason != "" {
			where = "debug: stopped (" + a.debug.reason + ")"
		}
		if a.debug.path != "" {
			where += " " + filepath.Base(a.debug.path) + ":" + itoa(a.debug.line+1)
		}
		if a.debug.frame != "" {
			where += " in " + a.debug.frame
		}
		return where
	}
	return "debug: " + a.debug.adapter + " running"
}
