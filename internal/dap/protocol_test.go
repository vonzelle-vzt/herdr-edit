// =============================================================================
// File: internal/dap/protocol_test.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package dap

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSuccessFalseSurvivesMarshal is the guard on the two-envelope design.
//
// It is RED against the obvious single-struct alternative: one envelope serving
// both requests and responses must tag Success with `omitempty` (a request has
// no success field), and `omitempty` on a bool DROPS false — so a refusal
// marshals with no "success" key at all and every reader treats it as a
// success. Asserting the literal bytes rather than a round-trip is deliberate:
// a round-trip passes either way, because an absent "success" decodes to false.
func TestSuccessFalseSurvivesMarshal(t *testing.T) {
	resp := Response{
		Seq: 12, Type: TypeResponse, RequestSeq: 7,
		Success: false, Command: "stackTrace", Message: "Unable to produce stack trace",
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"success":false`) {
		t.Fatalf("a refusal marshalled WITHOUT success:false — readers will treat it as success.\ngot: %s", raw)
	}

	// And the true case still says so, so the field is never simply absent.
	raw, err = json.Marshal(Response{Type: TypeResponse, RequestSeq: 1, Success: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"success":true`) {
		t.Fatalf("a success marshalled without success:true: %s", raw)
	}
}

// TestFailedResponseBodyDecodesToZero pins the second half of the same trap: a
// success:false response still carries a body, and decoding it as a result type
// yields that type's zero value. This is the exact payload delve sent for a bad
// stackTrace, and it demonstrates why call() must check Success BEFORE Body —
// read the other way round, an error is indistinguishable from a program with
// no stack.
func TestFailedResponseBodyDecodesToZero(t *testing.T) {
	wire := []byte(`{"seq":12,"type":"response","request_seq":7,"success":false,` +
		`"command":"stackTrace","message":"Unable to produce stack trace",` +
		`"body":{"error":{"id":2004,"format":"unknown goroutine 999999"}}}`)

	var resp Response
	if err := json.Unmarshal(wire, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Success {
		t.Fatal("decoded a refusal as a success")
	}

	var body stackTraceBody
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("the error body did not even fail to decode: %v", err)
	}
	if len(body.StackFrames) != 0 {
		t.Fatalf("expected the trap to produce zero frames, got %d", len(body.StackFrames))
	}

	var eb errorBody
	if err := json.Unmarshal(resp.Body, &eb); err != nil {
		t.Fatalf("errorBody: %v", err)
	}
	if eb.Error.Format != "unknown goroutine 999999" {
		t.Errorf("error format = %q, want the adapter's reason", eb.Error.Format)
	}
}

// TestUnverifiedBreakpointHasNoLine pins the shape delve actually sends when a
// breakpoint cannot be bound: verified false, a message, and NO line field at
// all. Line therefore decodes to 0, and drawing at it would put the marker on
// line 1 of the file. HasLine is what callers use instead.
func TestUnverifiedBreakpointHasNoLine(t *testing.T) {
	wire := []byte(`{"breakpoints":[` +
		`{"id":1,"verified":true,"source":{"name":"main.go"},"line":7},` +
		`{"verified":false,"message":"could not find statement at main.go:5, please use a line with a statement"}` +
		`]}`)
	var body setBreakpointsBody
	if err := json.Unmarshal(wire, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Breakpoints) != 2 {
		t.Fatalf("got %d breakpoints, want 2 (the answer is POSITIONAL with the request)", len(body.Breakpoints))
	}
	ok, bad := body.Breakpoints[0], body.Breakpoints[1]

	if !ok.Verified || ok.Line != 7 || !ok.HasLine() {
		t.Errorf("verified breakpoint decoded as %+v, want verified on line 7", ok)
	}
	if bad.Verified {
		t.Error("the unbindable breakpoint decoded as verified")
	}
	if bad.Line != 0 {
		t.Errorf("expected the missing line field to decode as 0, got %d", bad.Line)
	}
	if bad.HasLine() {
		t.Error("HasLine() said true for a breakpoint with no line — the marker would land on line 1")
	}
	if bad.Message == "" {
		t.Error("the adapter's reason was dropped; the user gets a hollow marker with no explanation")
	}
}

// TestDefaultFiltersKeepsStopOnPanic checks we send back the exception filters
// the adapter says default to on. Sending an empty list instead silently turns
// off stop-on-panic, which looks like the debugger ignoring a crash.
func TestDefaultFiltersKeepsStopOnPanic(t *testing.T) {
	caps := Capabilities{ExceptionBreakpointFilters: []ExceptionBreakpointFilter{
		{Filter: "unrecovered-panic", Label: "Unrecovered Panics", Default: true},
		{Filter: "runtime-fatal-throw", Label: "Fatal Throws", Default: true},
		{Filter: "opt-in-thing", Label: "Off By Default", Default: false},
	}}
	got := caps.DefaultFilters()
	if len(got) != 2 || got[0] != "unrecovered-panic" || got[1] != "runtime-fatal-throw" {
		t.Fatalf("DefaultFilters() = %v, want the two default-on filters only", got)
	}
}

// TestCapabilitiesDecodeFromDelve decodes delve 1.27's real initialize body and
// pins the three facts stage 2 depends on: configurationDone is required,
// conditional breakpoints and logpoints exist, and terminate does NOT — which
// is why Disconnect carries terminateDebuggee.
func TestCapabilitiesDecodeFromDelve(t *testing.T) {
	wire := []byte(`{"supportsConfigurationDoneRequest":true,"supportsFunctionBreakpoints":true,` +
		`"supportsConditionalBreakpoints":true,"supportsHitConditionalBreakpoints":true,` +
		`"supportsEvaluateForHovers":true,"exceptionBreakpointFilters":[` +
		`{"filter":"unrecovered-panic","label":"Unrecovered Panics","default":true},` +
		`{"filter":"runtime-fatal-throw","label":"Fatal Throws","default":true}],` +
		`"supportsLogPoints":true,"supportsDisassembleRequest":true}`)
	var caps Capabilities
	if err := json.Unmarshal(wire, &caps); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !caps.SupportsConfigurationDoneRequest {
		t.Error("supportsConfigurationDoneRequest decoded false — configurationDone would be skipped and the session would hang")
	}
	if !caps.SupportsConditionalBreakpoints || !caps.SupportsLogPoints {
		t.Error("conditional breakpoints / logpoints decoded false against a body that declares both")
	}
	if caps.SupportsTerminateRequest {
		t.Error("supportsTerminateRequest decoded TRUE from a body that omits it")
	}
	if len(caps.ExceptionBreakpointFilters) != 2 {
		t.Errorf("got %d exception filters, want 2", len(caps.ExceptionBreakpointFilters))
	}
}

// TestStoppedEventDecode pins the body delve sends on a breakpoint hit — the
// event the whole stage exists to react to.
func TestStoppedEventDecode(t *testing.T) {
	wire := []byte(`{"reason":"breakpoint","threadId":1,"allThreadsStopped":true,"hitBreakpointIds":[1]}`)
	var ev StoppedEvent
	if err := json.Unmarshal(wire, &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev.Reason != "breakpoint" || ev.ThreadID != 1 || !ev.AllThreadsStopped {
		t.Fatalf("decoded %+v, want reason=breakpoint threadId=1 allThreadsStopped=true", ev)
	}
	if len(ev.HitBreakpointIDs) != 1 || ev.HitBreakpointIDs[0] != 1 {
		t.Errorf("hitBreakpointIds = %v, want [1]", ev.HitBreakpointIDs)
	}
}

// TestSourceBreakpointOmitsEmptyCondition checks an unconditional breakpoint
// goes out with no condition key. An empty-string condition is still a
// condition to an adapter, and one that never evaluates true is a breakpoint
// that silently never fires.
func TestSourceBreakpointOmitsEmptyCondition(t *testing.T) {
	raw, err := json.Marshal(SourceBreakpoint{Line: 7})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "condition") || strings.Contains(string(raw), "logMessage") {
		t.Fatalf("an unconditional breakpoint carried a condition/logMessage: %s", raw)
	}
	if !strings.Contains(string(raw), `"line":7`) {
		t.Fatalf("line missing from %s", raw)
	}
}

// TestScopeAndVariableDecodeDelvesRealShapes pins the JSON delve actually
// sends, captured from the live oracle's log rather than invented:
//
//	scope "Locals": ref=1000 named=0 indexed=0 expensive=false
//	Locals.sum = 5 (int)
//
// 🔴 named and indexed BOTH being zero is the shape that matters. Total() must
// answer 0 for "the adapter did not say" rather than being read as "this scope
// is empty", because a truncation notice that names a denominator of 0 is worse
// than one that names none.
func TestScopeAndVariableDecodeDelvesRealShapes(t *testing.T) {
	var body scopesBody
	if err := json.Unmarshal([]byte(
		`{"scopes":[{"name":"Locals","variablesReference":1000,"namedVariables":0,"indexedVariables":0,"expensive":false}]}`,
	), &body); err != nil {
		t.Fatalf("scopes body: %v", err)
	}
	if len(body.Scopes) != 1 {
		t.Fatalf("got %d scopes", len(body.Scopes))
	}
	sc := body.Scopes[0]
	if sc.Name != "Locals" || sc.VariablesReference != 1000 || sc.Expensive {
		t.Errorf("scope = %+v", sc)
	}
	if sc.Total() != 0 {
		t.Errorf("Total() = %d for a scope that reported no counts, want 0 meaning unknown", sc.Total())
	}

	var vars variablesBody
	if err := json.Unmarshal([]byte(
		`{"variables":[{"name":"sum","value":"5","type":"int","evaluateName":"sum","variablesReference":0},`+
			`{"name":"cfg","value":"main.Config {Name: \"x\"}","type":"main.Config","variablesReference":1002,"namedVariables":2}]}`,
	), &vars); err != nil {
		t.Fatalf("variables body: %v", err)
	}
	if len(vars.Variables) != 2 {
		t.Fatalf("got %d variables", len(vars.Variables))
	}
	leaf, expandable := vars.Variables[0], vars.Variables[1]
	if leaf.Value != "5" || leaf.Type != "int" {
		t.Errorf("leaf = %+v", leaf)
	}
	if leaf.VariablesReference != 0 {
		t.Errorf("an int reported children: %+v", leaf)
	}
	if expandable.VariablesReference != 1002 || expandable.Total() != 2 {
		t.Errorf("expandable = %+v, want ref 1002 and Total 2", expandable)
	}
}

// TestVariablesArgsOmitAnAbsentWindow checks the paging window is expressed by
// ABSENCE rather than by a zero.
//
// count:0 is a legal request for zero variables in some readings of the spec,
// so "give me everything" and "give me nothing" must not marshal to the same
// bytes. The difference is not something to leave to an adapter's judgement.
func TestVariablesArgsOmitAnAbsentWindow(t *testing.T) {
	raw, err := json.Marshal(variablesArgs{VariablesReference: 1000})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "count") || strings.Contains(string(raw), "start") {
		t.Fatalf("an unwindowed request carried start/count: %s", raw)
	}

	raw, err = json.Marshal(variablesArgs{VariablesReference: 1000, Count: 200})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"count":200`) {
		t.Fatalf("a windowed request lost its count: %s", raw)
	}
}

// TestCapabilitiesReadDelvesRealAnswer pins what delve 1.27 actually reports —
// measured, and logged by the live oracle on every run.
//
// 🔴 Both absences are load-bearing. No supportsTerminateRequest means
// TerminateOrDisconnect MUST fall back to disconnect{terminateDebuggee:true} or
// the debuggee outlives the editor; no supportsVariablePaging means start/count
// are never sent and the caller's cap is the only bound on a million-element
// slice.
func TestCapabilitiesReadDelvesRealAnswer(t *testing.T) {
	var caps Capabilities
	if err := json.Unmarshal([]byte(
		`{"supportsConfigurationDoneRequest":true,"supportsConditionalBreakpoints":true,`+
			`"supportsLogPoints":true,"supportsFunctionBreakpoints":true,`+
			`"exceptionBreakpointFilters":[{"filter":"call-stack-error","label":"Call Stack Errors","default":true}]}`,
	), &caps); err != nil {
		t.Fatalf("capabilities: %v", err)
	}
	if !caps.SupportsConfigurationDoneRequest {
		t.Error("configurationDone support was lost; the session would hang")
	}
	if caps.SupportsTerminateRequest {
		t.Error("terminate support was decoded from an answer that does not mention it")
	}
	if caps.SupportsVariablePaging {
		t.Error("variable paging was decoded from an answer that does not mention it")
	}
	if got := caps.DefaultFilters(); len(got) != 1 || got[0] != "call-stack-error" {
		t.Errorf("DefaultFilters = %v", got)
	}
}

// TestSourceBreakpointOmitsEmptyConditionAndLogMessage pins the wire shape of
// the two fields this stage made real.
//
// 🔴 Both carry omitempty, and it is not tidiness. An adapter reads an
// empty-string condition as a CONDITION — one that never evaluates true — so a
// breakpoint sent with `"condition": ""` silently never fires, which is
// indistinguishable from a breakpoint the adapter refused to bind. Same for
// logMessage: an empty one turns a breakpoint into a logpoint that prints
// nothing and, crucially, no longer stops.
func TestSourceBreakpointOmitsEmptyConditionAndLogMessage(t *testing.T) {
	raw, err := json.Marshal(SourceBreakpoint{Line: 12})
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	for _, key := range []string{"condition", "logMessage", "hitCondition", "column"} {
		if strings.Contains(got, key) {
			t.Errorf("a plain breakpoint marshalled as %s and carries %q", got, key)
		}
	}
	if !strings.Contains(got, `"line":12`) {
		t.Errorf("marshalled as %s, want the line", got)
	}

	raw, err = json.Marshal(SourceBreakpoint{Line: 12, Condition: "i == 3", LogMessage: "i is {i}"})
	if err != nil {
		t.Fatal(err)
	}
	got = string(raw)
	if !strings.Contains(got, `"condition":"i == 3"`) {
		t.Errorf("marshalled as %s, want the condition on the wire", got)
	}
	if !strings.Contains(got, `"logMessage":"i is {i}"`) {
		t.Errorf("marshalled as %s, want the log message on the wire", got)
	}
}

// TestEvaluateContextsAreTheProtocolSpellings guards the two constants against a
// rename or a typo, which would be invisible: an adapter given an unknown
// context does not refuse, it falls back to its own default and answers from
// whatever scope that implies.
func TestEvaluateContextsAreTheProtocolSpellings(t *testing.T) {
	if EvalContextWatch != "watch" {
		t.Errorf("EvalContextWatch = %q, want watch", EvalContextWatch)
	}
	if EvalContextRepl != "repl" {
		t.Errorf("EvalContextRepl = %q, want repl", EvalContextRepl)
	}
}

// TestChildSessionDecodesTheRealStartDebuggingArguments pins the reverse
// request's shape against bytes copied off the wire from js-debug 1.117.0.
//
// 🔴 The configuration is carried as a map and forwarded VERBATIM, and
// __pendingTargetId is why. It is the only thing tying the child connection to
// the process the root already spawned; everything else in there is cosmetic.
// The tempting "surely the child needs to know what to run" instinct — merging
// our own launch keys back in, re-adding `program` — asks the child to launch a
// SECOND copy of the program, so the user would step through one process while
// another ran unobserved. A struct with named fields would quietly drop the one
// key that matters, since it is not in any published schema.
func TestChildSessionDecodesTheRealStartDebuggingArguments(t *testing.T) {
	const wire = `{"request":"launch","configuration":{"type":"pwa-node",` +
		`"name":"fixture.js [96905]","__pendingTargetId":"956ef4f0d34f26a8ac05ac1b"}}`

	var cs ChildSession
	if err := json.Unmarshal([]byte(wire), &cs); err != nil {
		t.Fatalf("decoding js-debug's startDebugging arguments: %v", err)
	}
	if cs.Request != "launch" {
		t.Errorf("request = %q, want launch", cs.Request)
	}
	if got := cs.Configuration["__pendingTargetId"]; got != "956ef4f0d34f26a8ac05ac1b" {
		t.Fatalf("__pendingTargetId = %v; without it the child attaches to nothing", got)
	}
	if got := cs.Configuration["type"]; got != "pwa-node" {
		t.Errorf("type = %v, want pwa-node", got)
	}

	// Re-marshalling must round-trip every key, because the configuration is
	// sent back out as the launch arguments unchanged.
	out, err := json.Marshal(cs.Configuration)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]interface{}
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if len(back) != 3 {
		t.Errorf("the configuration lost keys on the way back out: %v", back)
	}
	if _, ok := back["program"]; ok {
		t.Error("the child configuration carries `program`; that would launch a SECOND copy " +
			"of the debuggee alongside the one the root already started")
	}
}

// TestReverseRequestKeepsArgumentsRaw pins the one decision in ReverseRequest.
//
// Arguments stays json.RawMessage because the only reverse request this client
// acts on is startDebugging. Decoding an unknown one into a concrete type would
// mean inventing a shape in order to throw it away — and the answer to an
// unknown reverse request is a refusal, which needs nothing from its body.
func TestReverseRequestKeepsArgumentsRaw(t *testing.T) {
	const wire = `{"seq":8,"type":"request","command":"runInTerminal",` +
		`"arguments":{"kind":"integrated","args":["node","app.js"]}}`

	var rr ReverseRequest
	if err := json.Unmarshal([]byte(wire), &rr); err != nil {
		t.Fatal(err)
	}
	if rr.Seq != 8 {
		t.Errorf("seq = %d, want 8 — the answer must name the request the adapter is blocked on", rr.Seq)
	}
	if rr.Type != TypeRequest {
		t.Errorf("type = %q, want %q", rr.Type, TypeRequest)
	}
	if rr.Command != "runInTerminal" {
		t.Errorf("command = %q", rr.Command)
	}
	if len(rr.Arguments) == 0 {
		t.Error("arguments were dropped")
	}
}

// TestChildSessionToleratesAnAbsentConfiguration is the failure path: an adapter
// that asks for a child session without saying what to run.
//
// A nil map is what an absent configuration must decode to, so the caller can
// tell "nothing to launch" from "launch with no options" — the second would be
// sent as `{}` and answered by the adapter with something unhelpful.
func TestChildSessionToleratesAnAbsentConfiguration(t *testing.T) {
	var cs ChildSession
	if err := json.Unmarshal([]byte(`{"request":"attach"}`), &cs); err != nil {
		t.Fatal(err)
	}
	if cs.Request != "attach" {
		t.Errorf("request = %q, want attach", cs.Request)
	}
	if cs.Configuration != nil {
		t.Errorf("configuration = %v, want nil for an absent one", cs.Configuration)
	}
}

// TestEvaluateArgsDistinguishFrameZeroFromNoFrame is the type-level guard for
// the assumption js-debug falsified.
//
// 🔴 FrameID was an int with omitempty, on the stated grounds that frame 0 is
// not a valid frame id. That is true of delve and FALSE of js-debug, whose
// innermost frame is {"id":0} — so omitempty dropped the field and the
// expression was evaluated globally instead of in the frame on screen. A
// pointer is what keeps "frame zero" and "no frame" from marshalling
// identically.
func TestEvaluateArgsDistinguishFrameZeroFromNoFrame(t *testing.T) {
	zero := 0
	withFrame, err := json.Marshal(evaluateArgs{Expression: "a", FrameID: &zero, Context: EvalContextWatch})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(string(withFrame), `"frameId":0`) {
		t.Fatalf("frame zero marshalled as %s — it was dropped, so the expression is answered "+
			"from another scope with no error", withFrame)
	}

	noFrame, err := json.Marshal(evaluateArgs{Expression: "a", Context: EvalContextRepl})
	if err != nil {
		t.Fatal(err)
	}
	if contains(string(noFrame), `"frameId"`) {
		t.Fatalf("no-frame marshalled as %s — `\"frameId\":0` is a question ABOUT frame zero, "+
			"not the absence of a question", noFrame)
	}
}
