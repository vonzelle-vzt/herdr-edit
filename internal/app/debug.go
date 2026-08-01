// =============================================================================
// File: internal/app/debug.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// debug.go is the debug SESSION: a real adapter behind the breakpoint marks
// breakpoints.go draws. Press F5, the program runs under delve (Go) or debugpy
// (Python), it stops on your breakpoint, the editor opens that file and paints ▶
// on the line.
//
// 🔴 It is also the one layer that knows what the adapter can DO. An adapter
// without supportsConditionalBreakpoints does not refuse a condition — it drops
// the field — so the breakpoint then fires every single time and the user
// concludes conditions are broken. sourceBreakpointsFor is where that is caught,
// because this is the only layer holding both the capabilities and a status line
// to complain on.
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
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/cloudmanic/spice-edit/internal/dap"
	"github.com/cloudmanic/spice-edit/internal/editor"
	"github.com/cloudmanic/spice-edit/internal/lsp"
	"github.com/cloudmanic/spice-edit/internal/state"
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

// debugRequestFloor is the sequence number below which a debug request is
// treated as already answered: the instant this PROCESS started.
//
// 🔴 This is the ONE deliberate difference from consumeOpenRequest and copying
// that function's zero would be a bug, not a style choice. lastOpenSeq starts at
// 0, so an open-request left on disk by a previous session is honoured once at
// startup — harmless, it opens a file. The same rule here would START A DEBUG
// SESSION nobody asked for: a `s` pressed in a panel last Tuesday, replayed by
// whichever editor happens to open next, compiling and running whatever program
// that editor's active tab belongs to.
//
// A package-level var rather than constructor state on purpose. The floor is the
// thing that must never be forgotten, and a field a constructor sets is a field
// the NEXT constructor forgets — the guard has to live where the comparison is.
// The constructors also seed lastDebugSeq from this same var, so the two cannot
// be different numbers.
var debugRequestFloor = time.Now().UnixNano()

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

	// Pending is an unverified answer from an adapter that BINDS LAZILY, which
	// means "not resolved yet", not "refused".
	//
	// 🔴 Without this the editor calls a working breakpoint broken. js-debug
	// answers every setBreakpoints with verified:false and
	// message:"breakpoint.provisionalBreakpoint", never sends a `breakpoint`
	// event to upgrade it, and then stops on it — all measured. Reading that
	// answer the way delve's is read paints a hollow ○ in the gutter and
	// announces "N breakpoint(s) could not be set on an executable line" about
	// breakpoints the program is about to hit. See Adapter.BreakpointsBindLazily.
	Pending bool
}

// WillBind reports whether the debugger is expected to stop here — either the
// adapter confirmed it, or it is an adapter that confirms nothing up front.
//
// 🔴 The gutter and the published panel payload BOTH go through this. Two
// readings of the same three fields would eventually disagree, and the symptom
// would be a breakpoint drawn hollow in the editor and solid in the panel beside
// it, with nothing to say which one is right.
func (b boundBreakpoint) WillBind() bool { return b.Verified || b.Pending }

// breakpointPolicy is what ONE adapter's setBreakpoints answers mean, gathered
// so the push path takes a single argument rather than accumulating one
// parameter per adapter quirk.
type breakpointPolicy struct {
	// adapter names the adapter in anything shown to the user.
	adapter string
	// caps decides which breakpoint fields may be sent at all.
	caps dap.Capabilities
	// lazyBind maps Adapter.BreakpointsBindLazily onto the answers.
	lazyBind bool
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

	// root is the COORDINATOR connection for an adapter that debugs through
	// child sessions, and nil for the two that do not.
	//
	// 🔴 It is held only so it can be stopped LAST. js-debug's root owns the
	// server process and the child connection is a second socket into it, so
	// killing the root first takes the leaf down mid-disconnect and the debuggee
	// outlives both — the exact process leak TerminateOrDisconnect exists to
	// prevent, arrived at from the other direction. Nothing in the UI ever talks
	// to it: every request goes to client, which IS the leaf.
	root *dap.Client

	// lazyBind mirrors Adapter.BreakpointsBindLazily for this session, so the
	// meaning of an unverified answer travels with the session that produced it
	// rather than being looked up again by whoever is drawing.
	lazyBind bool

	// config names what this session is running, for the panels that mirror it:
	// LaunchSpec.Target, i.e. the program, the url, or the configuration's name
	// when it has neither. That is a Go PACKAGE directory for delve, a script
	// for debugpy, and a URL for a browser session out of launch.json.
	// Recorded at launch rather than derived later, because a.activeTabPtr()
	// has moved on by the time anything asks — and because a launch.json
	// configuration need have nothing to do with the active tab at all.
	config string

	// caps is what THIS adapter said it could do at initialize, refreshed if it
	// revises itself with a `capabilities` event.
	//
	// 🔴 Kept on the session rather than read from client.Caps() at each use so
	// that a session's capabilities and its client cannot disagree — and, more
	// importantly, so the refusal path is reachable in a test without a live
	// adapter that lacks the capability. A gate whose refusing branch can only
	// be exercised by installing a different debugger is a gate nobody runs.
	caps dap.Capabilities

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

	// breakpointsDirty records a breakpoint edit made while the session was
	// still coming up.
	//
	// 🔴 The start path SNAPSHOTS the breakpoint list on the main goroutine at
	// F5 and sends it from a background one, and bringing an adapter up takes
	// seconds — 7.4 of them cold, because it compiles the program. A condition
	// typed inside that window would land on the Mark, find no client to push
	// to, and then never be sent at all: silently absent for the whole session,
	// on the one breakpoint the user went out of their way to refine.
	breakpointsDirty bool

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

// policy gathers how this session's adapter answers setBreakpoints. Computed on
// demand rather than stored, because a `capabilities` event can revise caps
// mid-session and a cached policy would keep refusing a condition the adapter
// has since said it supports.
func (s *debugSession) policy() breakpointPolicy {
	return breakpointPolicy{adapter: s.adapter, caps: s.caps, lazyBind: s.lazyBind}
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
	// client is the connection the event arrived on. A js-debug session is a
	// ROOT coordinator plus a LEAF that owns the program, and both post here, so
	// without this the two streams merge and a stop in the debuggee is
	// indistinguishable from anything the coordinator said.
	client *dap.Client
	ev     dap.Event
}

func (e *debugEvent) When() time.Time { return e.when }

// debugStartedEvent reports the outcome of bringing a session up, with the
// breakpoints the adapter bound along the way.
type debugStartedEvent struct {
	when time.Time
	// client is the LEAF: the connection the UI talks to. For an adapter with
	// child sessions that is the child, never the coordinator.
	client *dap.Client
	// root is the coordinator to shut down last, and nil when there is none.
	root    *dap.Client
	adapter string
	caps    dap.Capabilities
	bound   map[string][]boundBreakpoint
	notes   []string
	err     error
}

func (e *debugStartedEvent) When() time.Time { return e.when }

// debugRebindEvent carries the result of re-sending breakpoints to an adapter
// that is ALREADY running — what happens when a condition or a log message is
// edited mid-session.
//
// It is a separate event from debugStartedEvent because it must not touch the
// starting/running flags: the session is already up, and folding this into the
// start path would have a breakpoint edit report "delve running" over the top of
// a program that is currently stopped somewhere.
type debugRebindEvent struct {
	when  time.Time
	bound map[string][]boundBreakpoint
	notes []string
}

func (e *debugRebindEvent) When() time.Time { return e.when }

// debugEvalEvent carries an evaluate answer back to the main loop.
type debugEvalEvent struct {
	when time.Time
	expr string
	res  dap.EvaluateResult
	err  string
}

func (e *debugEvalEvent) When() time.Time { return e.when }

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
	when   time.Time
	client *dap.Client
	msg    string
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
// resume, or there is something to start.
//
// 🔴 The launch.json arm is what makes a config-only adapter reachable at all.
// Gating solely on hasDebuggableTab asks "does an adapter claim this file's
// language", which for a browser configuration is permanently no — so a user
// editing index.html beside a "Launch Chrome" entry would find F5 greyed out
// and the picker, its only route, unreachable.
func (a *App) canStartOrContinueDebug() bool {
	return a.hasDebugSession() || a.hasDebuggableTab() || a.hasLaunchConfigurations()
}

// debugStartLabel is the dynamic label for the F5 menu row, so one row reads
// correctly in all three states rather than three rows being mostly disabled.
//
// 🔴 It NAMES the remembered launch.json configuration, and that is not a
// nicety. A sticky choice with no visible indicator is a hidden mode: next
// week's F5 would run a different program from the file on screen, with nothing
// anywhere to explain it. Either the choice is visible or it should not persist.
func (a *App) debugStartLabel() string {
	if a.debug != nil && a.debug.stopped {
		return "Continue"
	}
	if a.debug != nil && (a.debug.running || a.debug.starting) {
		return "Debugging… (running)"
	}
	if name := a.rememberedLaunchName(); name != "" {
		return "Start debugging (" + name + ")"
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
//
// 🔴 It takes a resolved LaunchSpec rather than (adapter, program) so that F5 on
// a source file and a launch.json configuration are the SAME start path. Two
// copies of this sequence would be two copies of the coordinator handshake,
// adoptChildSession and the verbatim child configuration — and the copy is where
// breakpoints quietly stop binding while the session still looks healthy.
func (a *App) runDebugSession(spec dap.LaunchSpec, bps []Breakpoint) {
	adapter := spec.Adapter

	ctx, cancel := context.WithTimeout(context.Background(), debugStartTimeout)
	defer cancel()

	handlers := dap.Handlers{
		OnEvent: func(c *dap.Client, e dap.Event) {
			a.post(&debugEvent{when: time.Now(), client: c, ev: e})
		},
		OnLog: func(c *dap.Client, s string) {
			a.post(&debugLogEvent{when: time.Now(), client: c, msg: s})
		},
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
		// 🔴 The adapter's own stderr is appended here, and it is the whole
		// message on the stdio transport. A socket adapter that dies on startup
		// fails inside StartCommand, which already explains itself; a stdio one
		// cannot — there is no accept to fail — so the first thing to notice is
		// this request coming back "connection lost". Without the stderr the user
		// is told the connection dropped for something whose actual reason
		// (`No module named debugpy`) the adapter printed a moment earlier.
		client.Stop()
		fail(withAdapterStderr(err, client))
		return
	}

	// 🔴 launchOrAttach, not Launch. This call site sent `launch` unconditionally,
	// which is correct for every configuration the editor could previously
	// produce and WRONG for the first `"request": "attach"` a user writes: it
	// would START a second copy of the process they meant to attach to, and both
	// would then be running. The child path already knew this; the root did not.
	if _, err := launchOrAttach(client, spec.Request, spec.Args); err != nil {
		client.Stop()
		fail(err)
		return
	}

	if err := client.WaitEvent(ctx, dap.EventInitialized); err != nil {
		client.Stop()
		fail(withAdapterStderr(err, client))
		return
	}

	groups := groupBreakpointsByPath(bps)
	pol := breakpointPolicy{adapter: adapter.Name, caps: caps, lazyBind: adapter.BreakpointsBindLazily}

	if !adapter.UsesChildSessions {
		bound, notes, err := a.configureSession(ctx, client, pol, groups)
		if err != nil {
			client.Stop()
			fail(err)
			return
		}
		a.post(&debugStartedEvent{
			when: time.Now(), client: client, adapter: adapter.Name,
			caps: caps, bound: bound, notes: notes,
		})
		return
	}

	// 🔴 The coordinator is configured with NO BREAKPOINTS. This is the single
	// most likely way this ships broken while looking fine: js-debug's root
	// session accepts setBreakpoints and answers plausibly, nothing ever binds
	// there, and the program runs straight past every one of them. VS Code arms
	// each session individually for the same reason. The root still needs its
	// configurationDone — measured, it will not launch the program without one,
	// so startDebugging never arrives and F5 hangs until the start timeout.
	if _, _, err := a.configureSession(ctx, client, pol, nil); err != nil {
		client.Stop()
		fail(err)
		return
	}

	root := client
	leaf, leafCaps, err := a.adoptChildSession(ctx, adapter, root)
	if err != nil {
		root.Stop()
		fail(withAdapterStderr(err, root))
		return
	}

	leafPol := breakpointPolicy{adapter: adapter.Name, caps: leafCaps, lazyBind: adapter.BreakpointsBindLazily}
	bound, notes, err := a.configureSession(ctx, leaf, leafPol, groups)
	if err != nil {
		leaf.Stop()
		root.Stop()
		fail(err)
		return
	}

	a.post(&debugStartedEvent{
		when: time.Now(), client: leaf, root: root, adapter: adapter.Name,
		caps: leafCaps, bound: bound, notes: notes,
	})
}

// configureSession runs the half of the handshake that follows `initialized`:
// arm the breakpoints, select the exception filters, and say the configuration
// is complete.
//
// Shared by the root and the leaf on purpose. The child's sequence is not
// "similar to" the root's — it is the same sequence against a second
// connection, and writing it twice would leave two places for the
// configurationDone that must not be skipped.
//
// A nil groups map sends no breakpoints at all, which is what the coordinator
// gets.
func (a *App) configureSession(ctx context.Context, client *dap.Client, pol breakpointPolicy,
	groups map[string][]Breakpoint) (map[string][]boundBreakpoint, []string, error) {

	bound, notes := pushBreakpoints(ctx, client, pol, groups)

	if err := client.SetExceptionBreakpoints(ctx, pol.caps.DefaultFilters()); err != nil {
		a.post(&debugLogEvent{when: time.Now(), msg: "exception breakpoints: " + err.Error()})
	}
	if err := client.ConfigurationDone(ctx); err != nil {
		return nil, nil, err
	}
	return bound, notes, nil
}

// adoptChildSession waits for the coordinator to hand over a leaf session, dials
// it, and gets it as far as `initialized`.
//
// 🔴 The ordering trap is the same one debugpy proved real, on a second
// connection: launch is SENT and never awaited. Measured against js-debug, the
// child's launch response arrives only AFTER its configurationDone — 1989ms
// against a launch sent at 1932ms, with setBreakpoints and configurationDone in
// between — so a caller that waited here would hang having armed nothing.
//
// It returns before configuring the leaf because the caller owns what goes in
// the middle of that gap, which is the whole reason Launch hands back a channel.
func (a *App) adoptChildSession(ctx context.Context, adapter dap.Adapter, root *dap.Client) (*dap.Client, dap.Capabilities, error) {
	cs, err := root.AwaitChildSession(ctx)
	if err != nil {
		return nil, dap.Capabilities{}, err
	}

	handlers := dap.Handlers{
		OnEvent: func(c *dap.Client, e dap.Event) {
			a.post(&debugEvent{when: time.Now(), client: c, ev: e})
		},
		OnLog: func(c *dap.Client, s string) {
			a.post(&debugLogEvent{when: time.Now(), client: c, msg: s})
		},
	}
	leaf, err := root.DialChild(adapter.Name, handlers)
	if err != nil {
		return nil, dap.Capabilities{}, err
	}

	caps, err := leaf.Initialize(ctx, adapter.AdapterID)
	if err != nil {
		leaf.Stop()
		return nil, dap.Capabilities{}, err
	}

	// 🔴 The configuration goes over VERBATIM. It is not merged with
	// adapter.Launch and `program` is not re-added: the child's config carries a
	// __pendingTargetId naming the process the root ALREADY started, and adding
	// program back asks it to launch a second copy — so the user would step
	// through one process while another ran unobserved.
	if _, err := launchOrAttach(leaf, cs.Request, cs.Configuration); err != nil {
		leaf.Stop()
		return nil, dap.Capabilities{}, err
	}
	if err := leaf.WaitEvent(ctx, dap.EventInitialized); err != nil {
		leaf.Stop()
		return nil, dap.Capabilities{}, err
	}
	return leaf, caps, nil
}

// launchOrAttach sends a configuration the way it asked to be sent: `attach` to
// join a process that already exists, `launch` to start one.
//
// 🔴 BOTH start paths go through this, and only the child's did before. The root
// called client.Launch unconditionally, so a `"request": "attach"` configuration
// out of launch.json would have STARTED the program it was written to attach to
// — leaving the user stepping through a second copy while the one they cared
// about ran on untouched. Nothing in the protocol reports that as an error;
// `attach` and `launch` are both valid requests, so the adapter simply does what
// it was told.
func launchOrAttach(client *dap.Client, request string, cfg map[string]interface{}) (<-chan dap.Response, error) {
	if request == "attach" {
		return client.Attach(cfg)
	}
	return client.Launch(cfg)
}

// pushBreakpoints sends each file's COMPLETE breakpoint set and reports what the
// adapter made of it, along with anything the user needs told.
//
// 🔴 It takes a map keyed by path rather than a flat list, and a path mapped to
// an EMPTY list is meaningful: it clears that file. setBreakpoints has no
// incremental form, so re-sending only the files that still have breakpoints
// would leave the adapter armed on a file the user just emptied — a breakpoint
// that "cannot be deleted", which is indistinguishable from the delete key not
// working. resendBreakpoints is the caller that relies on it.
//
// A file the adapter refuses is not fatal: the others may still bind, and
// abandoning the run over one bad path throws away a working debug session.
func pushBreakpoints(ctx context.Context, client *dap.Client, pol breakpointPolicy,
	groups map[string][]Breakpoint) (map[string][]boundBreakpoint, []string) {

	bound := make(map[string][]boundBreakpoint, len(groups))
	var notes []string
	droppedConditions, droppedLogpoints := 0, 0

	for path, list := range groups {
		sbps, nCond, nLog := sourceBreakpointsFor(pol.caps, list)
		droppedConditions += nCond
		droppedLogpoints += nLog

		answers, err := client.SetBreakpoints(ctx, dap.Source{Path: path, Name: filepath.Base(path)}, sbps)
		if err != nil {
			notes = append(notes, "breakpoints in "+filepath.Base(path)+": "+err.Error())
			continue
		}
		bound[path] = boundFromAnswers(list, answers, pol.lazyBind)
	}
	// Counted across every file and reported ONCE. Per-file messages would say
	// the same sentence three times for a project with breakpoints in three
	// files, and the status bar shows one line.
	notes = append(capabilityRefusals(pol.adapter, droppedConditions, droppedLogpoints), notes...)
	return bound, notes
}

// sourceBreakpointsFor converts one file's complete breakpoint list into wire
// breakpoints, DROPPING any field this adapter cannot honour and counting each
// drop. Returns the wire list, the number of conditions dropped, and the number
// of log messages dropped.
//
// 🔴 Dropping rather than sending is the whole point, and the failure it
// prevents is silent in both directions. An adapter that never advertised
// supportsConditionalBreakpoints does not REFUSE a condition — it ignores the
// field — so the breakpoint is accepted, reported verified, and then fires on
// every iteration of the loop the user wrote `i == 3` for. Nothing on screen
// distinguishes that from a condition that is simply wrong, so the user
// concludes conditions are broken. Sending nothing and saying so out loud is the
// only honest version.
func sourceBreakpointsFor(caps dap.Capabilities, list []Breakpoint) (out []dap.SourceBreakpoint, droppedConditions, droppedLogpoints int) {
	// Never nil: internal/dap turns a nil into an empty array because null is
	// rejected, but an explicit empty slice keeps "clear this file" readable
	// here rather than two layers down.
	out = make([]dap.SourceBreakpoint, 0, len(list))
	for _, b := range list {
		sb := dap.SourceBreakpoint{Line: adapterLineFromBuf(b.Line)}
		if b.Condition != "" {
			if caps.SupportsConditionalBreakpoints {
				sb.Condition = b.Condition
			} else {
				droppedConditions++
			}
		}
		if b.LogMessage != "" {
			if caps.SupportsLogPoints {
				sb.LogMessage = b.LogMessage
			} else {
				droppedLogpoints++
			}
		}
		out = append(out, sb)
	}
	return out, droppedConditions, droppedLogpoints
}

// capabilityRefusals renders what was dropped, naming the adapter.
//
// The adapter's name is in the message on purpose: "conditions are not
// supported" reads as a limitation of this editor, which is the wrong thing to
// go and fix. "delve does not support log points" names the component that would
// have to change.
func capabilityRefusals(adapter string, droppedConditions, droppedLogpoints int) []string {
	var out []string
	if droppedConditions > 0 {
		out = append(out, fmt.Sprintf("%s does not support conditional breakpoints — %s ignored",
			adapter, plural(droppedConditions, "condition")))
	}
	if droppedLogpoints > 0 {
		out = append(out, fmt.Sprintf("%s does not support log points — %s ignored",
			adapter, plural(droppedLogpoints, "log message")))
	}
	return out
}

// withAdapterStderr attaches whatever the adapter printed to an error, because
// "the connection was lost" is the least actionable thing a debugger can say and
// the reason is almost always sitting in its stderr.
func withAdapterStderr(err error, client *dap.Client) error {
	lines := client.LastStderr()
	if len(lines) == 0 {
		return err
	}
	return fmt.Errorf("%w (adapter said: %s)", err, strings.Join(lines, " | "))
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

// resendBreakpoints pushes the CURRENT set to an adapter that is already
// running, and is what makes editing a condition mid-session take effect.
//
// 🔴 Every file that has EVER been bound in this session is included, even when
// it now has no breakpoints at all — see pushBreakpoints. And every file is sent
// whole: adding a condition to one breakpoint re-sends all of that file's, or
// the rest are cleared by the same call that was meant to refine one of them.
//
// A no-op with no session, so callers do not have to check.
func (a *App) resendBreakpoints() {
	if a.debug == nil {
		return
	}
	if a.debug.client == nil {
		// Still starting: the launch path already snapshotted the list, so this
		// edit would otherwise never reach the adapter. handleDebugStarted picks
		// the flag up once there is a client to push to.
		a.debug.breakpointsDirty = true
		return
	}
	// The mark changed a moment ago; a.breakpoints is only refreshed by the
	// polled sync in Run, and sending the pre-edit list would push the very
	// state the user just changed.
	a.syncBreakpoints()

	groups := groupBreakpointsByPath(a.enabledBreakpoints())
	for path := range a.debug.bound {
		if _, ok := groups[path]; !ok {
			groups[path] = nil // whole-file clear
		}
	}

	// 🔴 a.debug.client is the LEAF for a coordinating adapter, so a mid-session
	// edit lands on the connection that actually holds the breakpoints. Reaching
	// for the root here would arm the coordinator, which is the same bug the
	// start path guards against and would be much quieter: the session already
	// works, so only the breakpoint edited after F5 would fail to fire.
	client, pol := a.debug.client, a.debug.policy()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), debugRequestTimeout)
		defer cancel()
		bound, notes := pushBreakpoints(ctx, client, pol, groups)
		a.post(&debugRebindEvent{when: time.Now(), bound: bound, notes: notes})
	}()
}

// handleDebugRebind applies a mid-session breakpoint push.
func (a *App) handleDebugRebind(e *debugRebindEvent) {
	if a.debug == nil {
		return // the session ended while the request was in flight
	}
	if a.debug.bound == nil {
		a.debug.bound = map[string][]boundBreakpoint{}
	}
	for path, list := range e.bound {
		a.debug.bound[path] = list
	}
	a.applyBoundBreakpoints()
	if len(e.notes) > 0 {
		a.flash(strings.Join(e.notes, " · "))
	}
}

// boundFromAnswers pairs what we asked for with what the adapter did.
//
// 🔴 The answer array matches the request POSITIONALLY — it carries no key back
// to the request other than its index — so this walks both together. An
// unverified breakpoint has no line at all (HasLine is false), and its Bound
// stays at the requested line rather than becoming line 1 of the file.
// lazyBind marks an unverified answer as pending rather than refused, for the
// adapters that answer before they can know. See boundBreakpoint.Pending.
func boundFromAnswers(asked []Breakpoint, answers []dap.Breakpoint, lazyBind bool) []boundBreakpoint {
	out := make([]boundBreakpoint, 0, len(asked))
	for i, b := range asked {
		bb := boundBreakpoint{Requested: b.Line, Bound: b.Line}
		if i < len(answers) {
			ans := answers[i]
			bb.ID = ans.ID
			bb.Verified = ans.Verified
			bb.Message = ans.Message
			bb.Pending = lazyBind && !ans.Verified
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
	client, root := a.debug.client, a.debug.root
	a.debug = nil
	go stopDebugClients(client, root)
	a.flash("Debug session stopped")
}

// stopDebugClients shuts a session down leaf FIRST, coordinator LAST.
//
// 🔴 The order is the whole function. For js-debug the root owns the server
// process and the leaf is a second connection into it, so stopping the root
// first kills the server out from under the leaf's disconnect — and
// disconnect{terminateDebuggee:true} is the only thing that stops the debugged
// node process. It would be left running with nothing on screen to say so,
// which is exactly the leak TerminateOrDisconnect exists to prevent, reached
// from the other direction.
//
// Both may be nil: a session can be stopped before it ever had a client, and
// only a coordinating adapter has a root at all.
func stopDebugClients(leaf, root *dap.Client) {
	if leaf != nil {
		leaf.Stop()
	}
	if root != nil {
		root.Stop()
	}
}

// stopDebugSession tears the session down on quit. Called from Run alongside
// lsp.Stop, so a debugged process is never left running after the editor exits.
func (a *App) stopDebugSession() {
	if a.debug == nil {
		return
	}
	client, root := a.debug.client, a.debug.root
	a.debug = nil
	// Synchronous, unlike the other two teardown paths: this one runs on the way
	// out of Run, and a goroutine started here would be killed by the process
	// exiting before it had disconnected anything.
	stopDebugClients(client, root)
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
		go stopDebugClients(e.client, e.root)
		return
	}
	a.debug.client = e.client
	a.debug.root = e.root
	a.debug.starting = false
	a.debug.running = true
	a.debug.caps = e.caps
	a.debug.bound = e.bound
	a.applyBoundBreakpoints()

	// A breakpoint edited while the adapter was still compiling the program did
	// not reach it — the list was snapshotted at F5. Push the current one now.
	if a.debug.breakpointsDirty {
		a.debug.breakpointsDirty = false
		a.resendBreakpoints()
	}

	// A capability the adapter could not honour outranks the summary: it names
	// something the user asked for and did not get, which the count of
	// unverified breakpoints does not cover and would otherwise hide.
	if len(e.notes) > 0 {
		a.flash(strings.Join(e.notes, " · "))
		return
	}

	// Counted with WillBind, not with Verified: an adapter that binds lazily
	// answers every breakpoint unverified, so counting refusals by the raw flag
	// would announce that none of them could be set on an executable line —
	// about breakpoints the program is about to stop on.
	unverified := 0
	for _, list := range e.bound {
		for _, b := range list {
			if !b.WillBind() {
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
			if !exists || !isBreakpointKind(m.Kind) {
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
	// 🔴 Route on the connection the event arrived on, not just on its name.
	//
	// A js-debug session is a ROOT coordinator plus a LEAF that owns the program.
	// The root debugs nothing, so applying its stop would move ▶ for a connection
	// with no program state — a plausible location from the wrong process, which
	// is the worst thing a debugger can show. Program-state events are therefore
	// honoured only from the leaf.
	//
	// terminated/exited are honoured from EITHER, because teardown depends on
	// whichever connection ends first and which that is has not been measured.
	// This does not fix the two-leaf case — that is not fixable until there are
	// two leaves — it installs the discriminator that makes the fix possible, and
	// stops wrong-connection stops and output today.
	if e.client != nil && e.client != a.debug.client && e.client != a.debug.root {
		return // a connection this session does not own
	}
	if e.client != nil && a.debug.client != nil && e.client != a.debug.client {
		switch e.ev.Event {
		case dap.EventStopped, dap.EventContinued, dap.EventOutput, dap.EventBreakpoint:
			return // the coordinator has no program state to report
		}
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

	case dap.EventCapabilities:
		// internal/dap has already folded the revision into Caps(); this copies
		// it onto the session so the condition/logpoint gates read the CURRENT
		// answer. An adapter that gains supportsLogPoints mid-session and is
		// still refused by a stale copy is a feature switched off by a cache.
		if a.debug.client != nil {
			a.debug.caps = a.debug.client.Caps()
		}

	case dap.EventProcess, dap.EventInitialized:
		// Consumed by the start sequence (initialized) or purely informational
		// (process); nothing further to do on the UI side.
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
	// 🔴 Telemetry is the adapter talking to its VENDOR, not to the user, and it
	// is not small: js-debug emits several hundred-character JSON blobs
	// ("js-debug/launch", "js-debug/cdp/operation") around every launch and
	// every exit. Left in, they are the first and last thing in the debug
	// console and they push the program's own output out of a window capped at
	// maxDebugOutput lines. Filtered here rather than per-adapter because no
	// adapter's telemetry belongs in a console a user reads.
	if oe.Category == dap.OutputCategoryTelemetry {
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
	client, root := a.debug.client, a.debug.root

	// Keep the program's output alive past the session that produced it. The
	// debug console is the only place a run's output can be READ, and a console
	// that empties the instant the program exits is a console you can never use
	// on a program that ran to completion — which is most of them.
	a.lastDebugOutput = a.debug.output

	a.debug = nil // clears the ▶ by construction: drawDebugGutter reads this
	go stopDebugClients(client, root)

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
		// 🔴 Decompose returns (fg, bg, attr) IN THAT ORDER. Taking the first
		// return as the background painted every debug glyph on the previous
		// cell's FOREGROUND colour — a wrong-but-plausible block behind the dot,
		// which reads as a theme quirk rather than a bug.
		_, bg, _ := existing.Decompose()
		a.screen.SetContent(ex, ey+row, glyph, nil,
			tcell.StyleDefault.Background(bg).Foreground(color))
	}

	for _, b := range a.debug.bound[tab.Path] {
		switch {
		case !b.WillBind():
			// A hollow dot where the adapter refused to bind, so the user can
			// see the breakpoint will not be hit rather than wondering why.
			//
			// WillBind, not Verified: a pending answer from an adapter that
			// resolves breakpoints when the script loads is not a refusal, and
			// drawing ○ on one would mark every js-debug breakpoint broken.
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

// -----------------------------------------------------------------------------
// The Debug panel contract (Lane B stage 5)
//
// 🔴 The panel NEVER speaks DAP. Everything below moves two small JSON files:
// debug-session.json out (what the debugger is doing) and debug-request.json in
// (what to do next). The adapter client stays in this process — if a panel ever
// needed the protocol itself, the split was drawn in the wrong place.
//
// Both are driven from the SAME single polled call site in Run as publishActive
// and consumeOpenRequest, and for the same reason those are: a session changes
// from a dozen event handlers and a mark changes from six menu actions, so a set
// of hand-placed hooks would miss one and the symptom would be a stale panel
// rather than anything that looks like a bug.
// -----------------------------------------------------------------------------

// publishDebug mirrors the session out to debug-session.json. Cheap when nothing
// changed: the publisher compares ignoring its own timestamp and writes at most
// one file per debounce window.
func (a *App) publishDebug() {
	a.debugPub.Set(a.debugSnapshot())
}

// publishDebugIdle is the clean-exit half of the staleness contract: the editor
// says out loud that there is no session before the process goes away, so a
// panel opened afterwards shows nothing rather than the last stop.
//
// Flushed rather than merely Set, because the debounce window outlives the
// shutdown block it is called from.
func (a *App) publishDebugIdle() {
	a.debugPub.Set(state.DebugSession{State: state.DebugStateIdle, Root: a.rootDir})
	a.debugPub.Flush()
}

// debugSnapshot renders the current session as the published payload.
//
// 🔴 Every line here is a 0-BASED buffer line; state.DebugPublisher.Set converts
// the whole payload to the 1-based wire form in one place. Adding a +1 here
// would double-count, and it would do it on only the fields this function
// touched — the failure the single conversion point exists to prevent.
func (a *App) debugSnapshot() state.DebugSession {
	snap := state.DebugSession{State: state.DebugStateIdle, Root: a.rootDir}

	switch s := a.debug; {
	case s == nil:
		// No session. "terminated" rather than "idle" when the last run left
		// output behind, because that is the difference between "nothing has
		// happened here" and "your program ran and finished" — and the console
		// holding that output is still open to be read.
		if len(a.lastDebugOutput) > 0 {
			snap.State = state.DebugStateTerminated
		}
	default:
		snap.Adapter, snap.Config, snap.ThreadID = s.adapter, s.config, s.threadID
		switch {
		case s.starting:
			snap.State = state.DebugStateStarting
		case s.stopped:
			snap.State = state.DebugStateStopped
		default:
			snap.State = state.DebugStateRunning
		}
		if s.stopped {
			snap.Reason, snap.File, snap.Line = s.reason, s.path, s.line
			snap.Frames = make([]state.DebugFrame, 0, len(s.frames))
			for _, f := range s.frames {
				snap.Frames = append(snap.Frames, state.DebugFrame{
					Name: f.Name, File: f.Path, Line: f.Line,
				})
			}
		}
	}

	bps := a.allBreakpoints()
	snap.Breakpoints = make([]state.DebugBreakpoint, 0, len(bps))
	for _, b := range bps {
		snap.Breakpoints = append(snap.Breakpoints, state.DebugBreakpoint{
			File: b.Path, Line: b.Line, Enabled: b.Enabled,
			Verified:  a.breakpointVerified(b),
			Condition: b.Condition, LogMessage: b.LogMessage,
		})
	}
	return snap
}

// breakpointVerified reports whether the live adapter confirmed it can stop on
// this breakpoint.
//
// False with no session, which is honest rather than pessimistic: nothing has
// bound it, so nothing knows. A panel showing every breakpoint as verified
// before F5 would be claiming an answer the debugger has not given.
func (a *App) breakpointVerified(b Breakpoint) bool {
	if a.debug == nil {
		return false
	}
	for _, bb := range a.debug.bound[b.Path] {
		if bb.Requested == b.Line {
			// The SAME reading the gutter uses. A panel drawing a hollow dot
			// beside an editor drawing a solid one, for one breakpoint, is worse
			// than either answer alone — there would be nothing to say which is
			// right. See boundBreakpoint.WillBind.
			return bb.WillBind()
		}
	}
	return false
}

// consumeDebugRequest honours a pending request from the Debug panel. Polled
// from the SAME single call site as consumeOpenRequest, not a second one.
//
// 🔴 The floor is the whole guard, and it is where this deliberately differs
// from consumeOpenRequest — see debugRequestFloor. Recording the sequence BEFORE
// acting is the other half: an action that refuses (no debuggable file, no
// session to step) must still consume the request, or the same one is replayed
// on every poll for as long as the editor runs.
//
// The one thing this does NOT do is scope a session action to a project.
// debug-request.json is global, like open-request.json, and a verb such as
// "continue" carries no path to test — so with two editors open on two projects,
// a panel key reaches both. The file-bearing action IS scoped (toggleBreakpointAt
// applies withinRoot), which is the case where landing in the wrong editor would
// edit the wrong file.
func (a *App) consumeDebugRequest() {
	req, ok := state.ReadDebugRequest()
	if !ok || req.Seq <= a.lastDebugSeq || req.Seq <= debugRequestFloor {
		return
	}
	a.lastDebugSeq = req.Seq

	switch req.Action {
	case state.DebugActionStart:
		a.menuDebugStartOrContinue()
	case state.DebugActionContinue:
		a.menuDebugContinue()
	case state.DebugActionNext:
		a.menuDebugStepOver()
	case state.DebugActionStepIn:
		a.menuDebugStepIn()
	case state.DebugActionStepOut:
		a.menuDebugStepOut()
	case state.DebugActionPause:
		a.menuDebugPause()
	case state.DebugActionStop:
		a.menuDebugStop()
	case state.DebugActionToggleBreakpoint:
		// The wire is 1-based, like every other location this editor accepts.
		a.toggleBreakpointAt(req.File, bufLineFromAdapter(req.Line))
	}
}

// toggleBreakpointAt honours a panel's toggle against a file:line rather than
// the cursor: it opens the file if needed, moves the cursor there, and then goes
// through menuToggleBreakpoint.
//
// 🔴 Reusing menuToggleBreakpoint rather than writing the mark directly is the
// point. That function is the ONE place a breakpoint is created or cleared, and
// it is what calls afterBreakpointEdit — the single site that re-sends the
// file's whole set to a live adapter. A second toggle that set the mark itself
// would work perfectly in the gutter and silently never reach the debugger.
func (a *App) toggleBreakpointAt(file string, line int) {
	if file == "" {
		a.flash("Toggle breakpoint needs a file, optionally as file:line")
		return
	}
	if line < 0 {
		line = 0
	}
	if !a.withinRoot(file) {
		return // another editor's project — see consumeDebugRequest
	}
	if _, err := os.Stat(file); err != nil {
		a.flash("Cannot open " + filepath.Base(file))
		return
	}
	tab := a.activeTabPtr()
	if tab == nil || tab.Path != file {
		a.openFile(file)
		tab = a.activeTabPtr()
	}
	if tab == nil {
		a.flash("Could not open " + filepath.Base(file))
		return
	}
	tab.MoveCursorTo(editor.Position{Line: line, Col: 0}, false)
	a.menuToggleBreakpoint()
}

// withinRoot reports whether path lies inside the project this editor was
// opened on. Shared by consumeOpenRequest and toggleBreakpointAt because both
// request files are GLOBAL — one editor per project, one file for all of them —
// so each has to decide whether a request is addressed to it. Two copies of the
// rule would drift into two different answers to "is this mine".
func (a *App) withinRoot(path string) bool {
	if a.rootDir == "" {
		return true
	}
	rel, err := filepath.Rel(a.rootDir, path)
	return err == nil && !strings.HasPrefix(rel, "..")
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
