// =============================================================================
// File: internal/state/debugreq.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// debugreq.go is the REVERSE of debugsession.go, and the exact shape openreq.go
// already established: debug-session.json flows editor -> panels ("here is what
// the debugger is doing"), debug-request.json flows panels -> editor ("do the
// next thing").
//
// That direction is what makes the Debug panel a debugger rather than a
// read-only mirror of one. The DAP client stays in the editor — the panel never
// speaks the protocol — but pressing `n` in the panel steps the program, because
// the request lands here and the editor honours it from its polled call site.
//
// The mechanism is deliberately identical to openreq.go: one small JSON file,
// temp-file plus rename so a reader never sees half a payload, a monotonic
// sequence so a request is honoured exactly once, and a malformed payload
// treated as ABSENT rather than as an error — a panel writing junk must not be
// able to break the editor.
//
// 🔴 There is ONE deliberate difference from openreq.go and it is the whole
// reason this file exists separately: consumeOpenRequest starts its high-water
// mark at 0, so a request left on disk by a previous session is honoured once at
// startup. For "open a file" that is harmless. For "start a debug session" it
// means LAUNCHING A PROCESS the user did not ask for, minutes or days later, on
// whatever project the editor happens to be opened on next. The editor floors
// the sequence at its own start time instead — see App.consumeDebugRequest.

package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// The actions a panel may ask for. They are the DAP verbs the editor already
// implements, spelled the way the protocol spells them so there is no
// translation table to get wrong — except toggle-breakpoint, which is not a DAP
// request at all but an edit to the editor's own marks.
const (
	DebugActionStart            = "start"
	DebugActionContinue         = "continue"
	DebugActionNext             = "next"
	DebugActionStepIn           = "stepIn"
	DebugActionStepOut          = "stepOut"
	DebugActionPause            = "pause"
	DebugActionStop             = "stop"
	DebugActionToggleBreakpoint = "toggle-breakpoint"
)

// debugActions is the single list every check reads: the CLI validates against
// it, ReadDebugRequest rejects against it, and the help text prints it. A second
// hand-written copy would drift the first time an action was added, and the
// symptom would be a key that works from the panel and not from the CLI.
var debugActions = []string{
	DebugActionStart, DebugActionContinue, DebugActionNext, DebugActionStepIn,
	DebugActionStepOut, DebugActionPause, DebugActionStop, DebugActionToggleBreakpoint,
}

// DebugActions returns the accepted actions, in the order the help lists them.
func DebugActions() []string {
	out := make([]string, len(debugActions))
	copy(out, debugActions)
	return out
}

// ValidDebugAction reports whether action is one the editor will honour.
func ValidDebugAction(action string) bool {
	for _, a := range debugActions {
		if a == action {
			return true
		}
	}
	return false
}

// DebugRequestFile is the path panels write and the editor polls.
func DebugRequestFile() string {
	return filepath.Join(Dir(), "debug-request.json")
}

// DebugRequest is one instruction to the editor's debugger.
//
// File and Line are only meaningful for toggle-breakpoint, and Line is 1-based
// on the wire — the same convention as OpenRequest and active.json, converted by
// the editor on the way in.
type DebugRequest struct {
	Action string `json:"action"`
	File   string `json:"file"`
	Line   int    `json:"line"`
	Seq    int64  `json:"seq"`
}

// WriteDebugRequest asks the editor to perform action. Used by the CLI that the
// Debug panel shells out to.
//
// It refuses an unknown action HERE rather than letting the editor discover it,
// so `herdr-edit --debug contnue` is a typo the user is told about instead of a
// key that silently does nothing.
func WriteDebugRequest(action, file string, line int) error {
	if !ValidDebugAction(action) {
		return errors.New("unknown debug action: " + action)
	}
	if action == DebugActionToggleBreakpoint && file == "" {
		return errors.New("toggle-breakpoint needs a file, optionally as file:line")
	}
	dir := Dir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	req := DebugRequest{Action: action, File: file, Line: line, Seq: time.Now().UnixNano()}
	payload, err := json.Marshal(req)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "debug-request-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, DebugRequestFile())
}

// ReadDebugRequest returns the pending request, or ok=false when there is none
// or it cannot be trusted.
//
// Missing, unparseable, sequence-less and unknown-action payloads all read as
// ABSENT rather than as an error. The editor polls this several times a second;
// a panel that wrote junk — or a half-finished file from some other tool — must
// cost nothing more than a poll that found nothing.
func ReadDebugRequest() (DebugRequest, bool) {
	raw, err := os.ReadFile(DebugRequestFile())
	if err != nil {
		return DebugRequest{}, false
	}
	var req DebugRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return DebugRequest{}, false
	}
	if req.Seq == 0 || !ValidDebugAction(req.Action) {
		return DebugRequest{}, false
	}
	if req.Action == DebugActionToggleBreakpoint && req.File == "" {
		return DebugRequest{}, false
	}
	return req, true
}
