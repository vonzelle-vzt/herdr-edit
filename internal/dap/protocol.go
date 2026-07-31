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
	// and leaking it.
	SupportsTerminateRequest bool `json:"supportsTerminateRequest"`

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

	// We do not implement variable paging, run-in-terminal, or progress
	// reporting, so we do not claim them. supportsRunInTerminalRequest in
	// particular makes an adapter send us a request it then BLOCKS on.
	SupportsVariableType         bool `json:"supportsVariableType"`
	SupportsRunInTerminalRequest bool `json:"supportsRunInTerminalRequest"`
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
