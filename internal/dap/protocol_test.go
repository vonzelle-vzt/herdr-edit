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
