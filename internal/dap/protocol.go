// =============================================================================
// File: internal/dap/protocol.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// Package dap is a minimal Debug Adapter Protocol client: enough to launch a
// program under a debugger, stop it on a breakpoint, and say where it stopped.
//
// Scope is deliberately narrow, for the same reason internal/lsp owns its own
// protocol types rather than importing one: the editor ships as a single static
// binary with exactly three dependencies, and google/go-dap would be a fourth.
// Everything here is stdlib.
//
// # Coordinates
//
// This package speaks the protocol's own numbering and nothing else. We declare
// linesStartAt1 and columnsStartAt1 in initialize, so every Line and Column
// crossing this package's boundary is ONE-based. Converting to the editor's
// zero-based buffer positions happens exactly once, in internal/app/debug.go,
// and never in here. A fixture whose indices already agree with the buffer's
// proves nothing about that boundary, so the live oracle asserts against the
// TEXT of the stopped line instead of its number.
package dap

import "encoding/json"

// Protocol message type discriminators, as they appear in the "type" field.
const (
	TypeRequest  = "request"
	TypeResponse = "response"
	TypeEvent    = "event"
)

// Reverse-request commands — requests the ADAPTER sends to US.
//
// 🔴 There is exactly one this client answers, and answering it is not
// optional: js-debug is a COORDINATOR that debugs nothing itself, so the
// session the user cares about only exists once startDebugging has been
// honoured. Every other reverse request is refused explicitly rather than
// ignored — an adapter blocks on a reverse request it sent, so silence is a
// hang, not a graceful decline. See Client.answerReverseRequest.
const (
	CommandStartDebugging = "startDebugging"
)

// Event names this client understands. Stage 2 handles every one of them:
// an event nobody reads is how a session hangs with nothing on screen to
// explain it.
const (
	EventInitialized  = "initialized"
	EventStopped      = "stopped"
	EventContinued    = "continued"
	EventOutput       = "output"
	EventTerminated   = "terminated"
	EventExited       = "exited"
	EventBreakpoint   = "breakpoint"
	EventThread       = "thread"
	EventCapabilities = "capabilities"
	EventProcess      = "process"
)

// Request is an outgoing request. It is a SEPARATE struct from Response on
// purpose, and this is the single most important shape decision in the file.
//
// 🔴 A single combined envelope has to tag Success with `omitempty`, because a
// request carries no success field — and `omitempty` on a bool DROPS false. A
// refusal would then marshal with no "success" key at all, which every adapter
// and every one of our own fakes reads as SUCCESS. The bug is invisible in a
// test that only ever asserts on successful traffic, so the two shapes are kept
// apart by construction rather than by remembering. TestSuccessFalseSurvivesMarshal
// is the guard.
type Request struct {
	Seq       int         `json:"seq"`
	Type      string      `json:"type"`
	Command   string      `json:"command"`
	Arguments interface{} `json:"arguments,omitempty"`
}

// Response is an answer to a Request.
//
// Success carries NO omitempty — see Request's comment. RequestSeq, not Seq, is
// what identifies the request being answered: on the first response of a
// session both fields are 1 (measured against delve 1.27), so a client that
// correlates on Seq demultiplexes correctly exactly once and then times out
// forever after, which reads as "the adapter is slow" rather than as a bug.
type Response struct {
	Seq        int             `json:"seq"`
	Type       string          `json:"type"`
	RequestSeq int             `json:"request_seq"`
	Success    bool            `json:"success"`
	Command    string          `json:"command,omitempty"`
	Message    string          `json:"message,omitempty"`
	Body       json.RawMessage `json:"body,omitempty"`
}

// Event is adapter-initiated traffic: the debuggee stopped, the program wrote
// to stdout, a breakpoint moved.
type Event struct {
	Seq   int             `json:"seq"`
	Type  string          `json:"type"`
	Event string          `json:"event"`
	Body  json.RawMessage `json:"body,omitempty"`
}

// ReverseRequest is a request the ADAPTER sends to US, decoded far enough to
// answer it.
//
// Arguments stays raw because the only one this client acts on is
// startDebugging, and decoding an unknown reverse request's arguments into a
// concrete type would be inventing a shape in order to throw it away.
type ReverseRequest struct {
	Seq       int             `json:"seq"`
	Type      string          `json:"type"`
	Command   string          `json:"command"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// ChildSession is a leaf session a coordinating adapter has asked us to start.
//
// 🔴 Configuration is passed to launch/attach VERBATIM and must not be merged
// with our own launch keys. Measured against js-debug 1.117.0, the whole of it
// is:
//
//	{"type":"pwa-node","name":"fixture.js [96905]",
//	 "__pendingTargetId":"956ef4f0d34f26a8ac05ac1b"}
//
// The pending-target id is the ONLY thing that attaches this connection to the
// process the root already spawned. Re-adding `program` — the obvious "surely
// the child needs to know what to run" instinct — asks the child to launch a
// SECOND copy of the program, so the user would debug one process while another
// ran unobserved.
type ChildSession struct {
	// Request is "launch" or "attach", as the root asked for it.
	Request string `json:"request"`
	// Configuration is the child's launch configuration, sent unmodified.
	Configuration map[string]interface{} `json:"configuration"`
}

// errorBody is the shape an adapter puts in a failed response's body. Delve
// answers a bad stackTrace with
// {"error":{"id":2004,"format":"unknown goroutine 999999"}} — measured — so the
// human-readable reason lives here rather than in Response.Message alone.
//
// 🔴 A failed response still HAS a body, and it unmarshals into any result type
// as that type's zero value. Decoding it as a stackTrace yields zero frames,
// i.e. an error that reads as "the program has no stack". Success must be
// checked BEFORE Body is touched; call() is the one place that happens.
type errorBody struct {
	Error struct {
		ID     int    `json:"id"`
		Format string `json:"format"`
	} `json:"error"`
}

// Capabilities is the subset of the adapter's initialize response this client
// acts on. Fields we do not honour are deliberately absent: advertising or
// reading a capability we ignore invites traffic we then drop on the floor.
type Capabilities struct {
	// SupportsConfigurationDoneRequest is true for delve. When it is set, the
	// adapter WAITS for configurationDone before running the program, so
	// failing to send it hangs the session with no error anywhere.
	SupportsConfigurationDoneRequest  bool `json:"supportsConfigurationDoneRequest"`
	SupportsConditionalBreakpoints    bool `json:"supportsConditionalBreakpoints"`
	SupportsHitConditionalBreakpoints bool `json:"supportsHitConditionalBreakpoints"`
	SupportsLogPoints                 bool `json:"supportsLogPoints"`
	SupportsFunctionBreakpoints       bool `json:"supportsFunctionBreakpoints"`

	// SupportsTerminateRequest is ABSENT from delve's answer, so Disconnect
	// falls back to disconnect{terminateDebuggee:true}. Reading this before
	// choosing how to shut down is the difference between killing the debuggee
	// and leaking it. TerminateOrDisconnect is the one place that decides.
	SupportsTerminateRequest bool `json:"supportsTerminateRequest"`

	// SupportsVariablePaging decides whether a variables request may carry
	// start/count.
	//
	// 🔴 MEASURED: delve 1.27 answers this ABSENT, exactly like
	// supportsTerminateRequest — the live oracle logs
	// `variablePaging=false`. So against the one adapter this fork ships,
	// start/count are NEVER sent and the request always returns the entire
	// collection. That makes the CALLER's cap the only thing standing between a
	// []byte of a million elements and a million decoded, allocated picker rows;
	// it is load-bearing, not a belt-and-braces second line of defence. See
	// maxDebugVariables in internal/app.
	//
	// Sending the window anyway would be worse than useless: an adapter without
	// paging support IGNORES start/count silently rather than refusing, so the
	// client would believe it had bounded an answer it had not.
	SupportsVariablePaging bool `json:"supportsVariablePaging"`

	ExceptionBreakpointFilters []ExceptionBreakpointFilter `json:"exceptionBreakpointFilters"`
}

// ExceptionBreakpointFilter is one toggleable class of exception the adapter
// can break on. Delve offers unrecovered-panic and runtime-fatal-throw, both
// defaulting on.
type ExceptionBreakpointFilter struct {
	Filter  string `json:"filter"`
	Label   string `json:"label"`
	Default bool   `json:"default"`
}

// DefaultFilters returns the filters the adapter says should be enabled unless
// the user says otherwise. Sending these preserves "stop on panic", which is
// the behaviour a Go developer expects and which an empty filter list silently
// turns off.
func (c Capabilities) DefaultFilters() []string {
	out := make([]string, 0, len(c.ExceptionBreakpointFilters))
	for _, f := range c.ExceptionBreakpointFilters {
		if f.Default {
			out = append(out, f.Filter)
		}
	}
	return out
}

// Evaluate contexts, as the protocol spells them in an evaluate request's
// `context` field.
//
// 🔴 The choice is not cosmetic. EvalContextWatch is evaluated IN THE FRAME
// named by frameId, which is what makes "what is `i` right now" answer with the
// selected frame's `i` rather than a package-level one; EvalContextRepl is what
// an adapter expects when there is no frame to evaluate in. Sending repl with a
// frame id, or watch without one, is accepted by delve and answered — from the
// wrong scope, with no error. See Client.Evaluate.
const (
	EvalContextWatch = "watch"
	EvalContextRepl  = "repl"
)

// EvaluateResult is what an adapter made of an expression.
//
// Result is already RENDERED by the adapter — display text, exactly like
// Variable.Value, and never something to parse. A non-zero VariablesReference
// means the answer has children; nothing reads it yet, and it is carried so a
// future "expand this result" does not have to change the type.
type EvaluateResult struct {
	Result             string `json:"result"`
	Type               string `json:"type,omitempty"`
	VariablesReference int    `json:"variablesReference"`
}

// evaluateArgs asks the adapter to evaluate an expression.
//
// 🔴 FrameID is a POINTER, and it used to be an int with omitempty. That was a
// delve-shaped assumption written down as a fact — "frame 0 is not a valid frame
// id" — and js-debug falsified it: its innermost frame is literally
// {"id":0,"name":"global.add"}. omitempty then DROPPED the field, so an evaluate
// meant for the frame the user was looking at was answered globally, and the
// live oracle read `Uncaught ReferenceError: a is not defined` for a variable
// sitting in scope on the stopped line.
//
// That failure is loud here only because the fixture's variable happens not to
// exist globally. Evaluate a name that DOES exist at both scopes — a module
// constant shadowed by a local, the common case in real code — and the adapter
// answers the wrong one with no error at all.
//
// A pointer keeps both meanings distinct: nil is "evaluate outside any frame",
// and &0 is "evaluate in frame zero".
type evaluateArgs struct {
	Expression string `json:"expression"`
	FrameID    *int   `json:"frameId,omitempty"`
	Context    string `json:"context,omitempty"`
}

// Source names a file to the adapter. Path is absolute; adapters resolve
// relative paths against their own working directory, which is not ours.
type Source struct {
	Name string `json:"name,omitempty"`
	Path string `json:"path,omitempty"`
}

// SourceBreakpoint is one breakpoint we ASK for, in adapter (1-based)
// coordinates. Condition and LogMessage are sent only when non-empty: an
// adapter without supportsConditionalBreakpoints treats an empty-string
// condition as a condition, and one that never evaluates true is a breakpoint
// that silently never fires.
//
// 🔴 omitempty is only half the guard, and the quieter half is the CAPABILITY.
// An adapter that never advertised supportsConditionalBreakpoints DROPS the
// field rather than refusing the request, so the breakpoint is accepted,
// verified, and then fires on every single iteration — and the only visible
// evidence is a debugger that stops ten times when the user asked for one. The
// filtering happens in internal/app (sourceBreakpointsFor), which is the layer
// that has both the capabilities and a status line to complain on; this struct
// carries whatever it is handed.
type SourceBreakpoint struct {
	Line         int    `json:"line"`
	Column       int    `json:"column,omitempty"`
	Condition    string `json:"condition,omitempty"`
	HitCondition string `json:"hitCondition,omitempty"`
	LogMessage   string `json:"logMessage,omitempty"`
}

// Breakpoint is one breakpoint as the adapter ACTUALLY bound it, and the answer
// is not necessarily the question.
//
// 🔴 Verified breakpoints MOVE. setBreakpoints answers with an array matching
// what we sent POSITIONALLY, whose Line may have been snapped forward to the
// next executable statement. Measured against delve: asking for a comment line
// comes back {"verified":false,"message":"could not find statement at ...:5"}
// with NO line field at all — so Line is 0, not the line we asked about. Callers
// must therefore treat Line as meaningful only when Verified is true, which is
// what HasLine encodes; drawing at a zero would put the marker on line 1 of the
// file and make the debugger look like it is lying.
type Breakpoint struct {
	ID       int    `json:"id,omitempty"`
	Verified bool   `json:"verified"`
	Message  string `json:"message,omitempty"`
	Source   Source `json:"source"`
	Line     int    `json:"line,omitempty"`
	Column   int    `json:"column,omitempty"`
}

// HasLine reports whether this breakpoint carries a usable adapter line. An
// unverified breakpoint from delve has no line field, so its zero value must
// never be mistaken for line 1.
func (b Breakpoint) HasLine() bool { return b.Verified && b.Line > 0 }

// setBreakpointsArgs is the whole-file breakpoint set for one source.
//
// 🔴 setBreakpoints is NOT incremental. Every call REPLACES the complete set for
// that source, so sending only the breakpoint the user just added clears all the
// others. Client.SetBreakpoints takes the full list for a file for that reason.
type setBreakpointsArgs struct {
	Source      Source             `json:"source"`
	Breakpoints []SourceBreakpoint `json:"breakpoints"`
}

// setBreakpointsBody is the positional answer to the above.
type setBreakpointsBody struct {
	Breakpoints []Breakpoint `json:"breakpoints"`
}

// initializeArgs declares who we are and how we count.
//
// LinesStartAt1 and ColumnsStartAt1 are both true and both explicit. The
// protocol's default when they are omitted is true as well, but relying on a
// default for the one thing that decides whether the marker lands on the right
// line is how an off-by-one ships.
type initializeArgs struct {
	ClientID        string `json:"clientID"`
	ClientName      string `json:"clientName"`
	AdapterID       string `json:"adapterID"`
	Locale          string `json:"locale"`
	LinesStartAt1   bool   `json:"linesStartAt1"`
	ColumnsStartAt1 bool   `json:"columnsStartAt1"`
	PathFormat      string `json:"pathFormat"`

	// What we claim here is exactly what we honour, and no more.
	//
	// SupportsVariableType and SupportsVariablePaging became true in stage 3,
	// when a variables picker arrived that reads Variable.Type and sends
	// start/count. Claiming a capability we ignore invites traffic we then drop
	// on the floor; NOT claiming one we do use is the mirror mistake, and it is
	// the quieter of the two — an adapter is entitled to omit the `type`
	// attribute from a client that never said it could read one, so the picker
	// would simply show no types and look like the adapter did not know them.
	//
	// supportsRunInTerminalRequest stays false: it makes an adapter send US a
	// request it then BLOCKS on, and running the debuggee in a terminal is
	// something this editor genuinely cannot do — it owns the only one there is.
	//
	// SupportsStartDebuggingRequest is the opposite case and became true when
	// js-debug arrived, because we now really do answer it. Leaving it false
	// while implementing the handler would be the mirror mistake this comment
	// block already warns about, and the quieter one.
	//
	// 🔴 Declaring it false does NOT stop js-debug asking. Measured: with
	// supportsStartDebuggingRequest absent the root sent startDebugging anyway,
	// because its root session is a coordinator that has no other way to hand
	// over a debuggee. So the choice was never "opt in to child sessions or
	// don't" — it was "answer the request or hang".
	SupportsVariableType          bool `json:"supportsVariableType"`
	SupportsVariablePaging        bool `json:"supportsVariablePaging"`
	SupportsRunInTerminalRequest  bool `json:"supportsRunInTerminalRequest"`
	SupportsStartDebuggingRequest bool `json:"supportsStartDebuggingRequest"`
}

// setExceptionBreakpointsArgs toggles which exception classes stop the program.
// Filters is never nil on the wire: encoding/json renders a nil slice as null,
// and adapters that expect an array reject it.
type setExceptionBreakpointsArgs struct {
	Filters []string `json:"filters"`
}

// StoppedEvent says the program stopped and why. Reason is "breakpoint",
// "step", "pause", "exception", "entry"…; ThreadID names the goroutine to ask
// for a stack trace.
type StoppedEvent struct {
	Reason            string `json:"reason"`
	Description       string `json:"description,omitempty"`
	ThreadID          int    `json:"threadId"`
	AllThreadsStopped bool   `json:"allThreadsStopped"`
	Text              string `json:"text,omitempty"`
	HitBreakpointIDs  []int  `json:"hitBreakpointIds,omitempty"`
}

// ContinuedEvent says the program started running again without us asking —
// which happens when the adapter resumes on its own. Ignoring it leaves the
// stopped marker painted under a program that is no longer there.
type ContinuedEvent struct {
	ThreadID            int  `json:"threadId"`
	AllThreadsContinued bool `json:"allThreadsContinued"`
}

// OutputEvent is the debuggee's stdout/stderr, plus the adapter's own console
// chatter. This is the ONLY channel through which a debugged program's output
// may reach the user: writing it to our real stdout would scribble over the
// tcell screen.
type OutputEvent struct {
	Category string `json:"category,omitempty"`
	Output   string `json:"output"`
}

// OutputCategoryTelemetry is the category an adapter uses to report usage data
// to its own vendor. It is protocol traffic that happens to travel as output,
// and it is never something a user asked to read — see handleDAPOutput.
const OutputCategoryTelemetry = "telemetry"

// ExitedEvent carries the debuggee's exit code. Distinct from TerminatedEvent:
// the program can exit while the adapter stays up.
type ExitedEvent struct {
	ExitCode int `json:"exitCode"`
}

// ThreadEvent announces a goroutine starting or exiting.
type ThreadEvent struct {
	Reason   string `json:"reason"`
	ThreadID int    `json:"threadId"`
}

// BreakpointEvent is how a breakpoint's verified state or line changes AFTER
// setBreakpoints already answered. Handling it is what keeps a hollow ○ from
// staying hollow once the adapter binds it for real.
type BreakpointEvent struct {
	Reason     string     `json:"reason"`
	Breakpoint Breakpoint `json:"breakpoint"`
}

// CapabilitiesEvent lets an adapter announce capabilities it did not know about
// at initialize time.
type CapabilitiesEvent struct {
	Capabilities Capabilities `json:"capabilities"`
}

// StackFrame is one frame of a stopped thread's stack, in adapter coordinates.
//
// Column is 0 in every delve frame measured, even with columnsStartAt1
// declared — an adapter is allowed to not know the column. Callers must treat a
// zero column as "no column information", not as column 1.
type StackFrame struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Source Source `json:"source"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
}

// stackTraceArgs asks for a thread's frames.
type stackTraceArgs struct {
	ThreadID   int `json:"threadId"`
	StartFrame int `json:"startFrame,omitempty"`
	Levels     int `json:"levels,omitempty"`
}

// stackTraceBody is the answer.
type stackTraceBody struct {
	StackFrames []StackFrame `json:"stackFrames"`
	TotalFrames int          `json:"totalFrames,omitempty"`
}

// Scope is one named group of variables belonging to a stack frame —
// "Arguments", "Locals", "Globals" for delve.
//
// 🔴 VariablesReference is a HANDLE, not an identifier: it is allocated by the
// adapter for one stop and is valid only until the program runs again. See
// Client.Variables for what that costs a caller who caches it.
//
// Expensive means "do not fetch this without being asked". Delve marks Globals
// expensive because a package's globals can be enormous, and a client that
// eagerly walked every scope on every stop would turn each step into that fetch.
type Scope struct {
	Name               string `json:"name"`
	VariablesReference int    `json:"variablesReference"`
	NamedVariables     int    `json:"namedVariables,omitempty"`
	IndexedVariables   int    `json:"indexedVariables,omitempty"`
	Expensive          bool   `json:"expensive"`
}

// Total is how many variables the adapter says this scope holds, or 0 when it
// did not say. Used to report an honest "showing the first N of M" rather than
// silently capping.
func (s Scope) Total() int { return s.NamedVariables + s.IndexedVariables }

// Variable is one variable, argument or field.
//
// Value is already rendered by the adapter — it is display text, not something
// to parse. A non-zero VariablesReference means the value has children (a
// struct, slice, map or pointer) and can be expanded by asking for that
// reference; zero means it is a leaf.
type Variable struct {
	Name               string `json:"name"`
	Value              string `json:"value"`
	Type               string `json:"type,omitempty"`
	EvaluateName       string `json:"evaluateName,omitempty"`
	VariablesReference int    `json:"variablesReference"`
	NamedVariables     int    `json:"namedVariables,omitempty"`
	IndexedVariables   int    `json:"indexedVariables,omitempty"`
}

// Total is how many children the adapter says this variable has, or 0 when it
// did not say — the same contract as Scope.Total.
func (v Variable) Total() int { return v.NamedVariables + v.IndexedVariables }

// scopesArgs asks for one frame's scopes.
type scopesArgs struct {
	FrameID int `json:"frameId"`
}

// scopesBody is the answer.
type scopesBody struct {
	Scopes []Scope `json:"scopes"`
}

// variablesArgs asks for the contents of one variables reference.
//
// Start and Count carry omitempty so that "no window" is expressed by their
// absence rather than by a zero the adapter would have to interpret — count:0
// is a legal request for zero variables in some readings of the spec, and the
// difference between "give me everything" and "give me nothing" is not
// something to leave to an adapter's interpretation.
type variablesArgs struct {
	VariablesReference int    `json:"variablesReference"`
	Filter             string `json:"filter,omitempty"`
	Start              int    `json:"start,omitempty"`
	Count              int    `json:"count,omitempty"`
}

// variablesBody is the answer.
type variablesBody struct {
	Variables []Variable `json:"variables"`
}

// terminateArgs asks the debuggee to end politely. Only ever sent to an adapter
// that advertised supportsTerminateRequest — delve does not. See
// Client.TerminateOrDisconnect.
type terminateArgs struct {
	Restart bool `json:"restart"`
}

// Thread is one goroutine.
type Thread struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// threadsBody is the answer to a threads request.
type threadsBody struct {
	Threads []Thread `json:"threads"`
}

// threadArgs names a single thread, for continue and pause.
type threadArgs struct {
	ThreadID int `json:"threadId"`
}

// disconnectArgs shuts the session down. TerminateDebuggee is the fallback for
// adapters (delve among them) that do not advertise supportsTerminateRequest;
// without it the debugged process outlives the editor.
type disconnectArgs struct {
	Restart           bool `json:"restart"`
	TerminateDebuggee bool `json:"terminateDebuggee"`
}
