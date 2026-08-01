// =============================================================================
// File: internal/state/debugsession.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// debugsession.go publishes the DEBUG session the way state.go publishes the
// cursor: one small, debounced, best-effort JSON file that panels read and that
// nothing may write back through. It is a direct sibling of state.go and follows
// the same five rules — 150ms debounce, temp-file-plus-rename so a reader never
// sees half a payload, every failure swallowed, a nil receiver safe on every
// method, and a compare that ignores the timestamp so the debounce actually
// collapses anything.
//
// # The panel never speaks DAP
//
// The debug adapter client lives in this editor (internal/dap). What crosses
// this boundary is a SNAPSHOT of the session — is it running, where did it stop,
// what is on the stack, which breakpoints exist — plus, in debugreq.go, a way to
// ask for the next step. If a panel ever needs the protocol itself, the split
// was drawn in the wrong place.
//
// # Staleness has two halves and BOTH are required
//
//  1. Clean exit: the editor publishes state "idle" in the same shutdown block
//     that flushes active.json, so a panel opened after the editor quit sees
//     nothing rather than the last thing that was true.
//
//  2. SIGKILL: there is no shutdown block at all, so the last payload sits on
//     disk saying "stopped at main.go:42" forever. Readers therefore treat a
//     LIVE state (starting / running / stopped) whose timestamp is older than
//     StaleAfter as stale. Without this second half a crashed editor leaves a
//     panel confidently describing a program that has not existed for a week.
//
// StaleAfter travels IN the payload rather than being a number each reader
// hardcodes, so the editor and the panels cannot drift apart about it.
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The session states a payload may carry. idle and terminated are settled
// facts; the other three describe a session in flight and are the ones a reader
// ages out — see the header.
const (
	DebugStateIdle       = "idle"
	DebugStateStarting   = "starting"
	DebugStateRunning    = "running"
	DebugStateStopped    = "stopped"
	DebugStateTerminated = "terminated"
)

// DebugStaleAfter is how old a LIVE payload may be before a reader stops
// believing it. 60s is comfortably longer than the editor's slowest publish
// cadence (its 10s tree-refresh tick, which is what wakes the poll when nobody
// is typing) and short enough that a killed editor stops lying within a minute.
const DebugStaleAfter = 60 * time.Second

// maxSessionFrames caps the published stack. This file is a snapshot, not a
// log: a runaway recursion has whatever depth the runtime allowed, and a panel
// showing 4000 frames in a 40-row pane is not showing anything. The pre-cap
// depth is published as FrameTotal so a truncated list can say so rather than
// quietly claiming the stack was that shallow.
const maxSessionFrames = 20

// maxSessionBreakpoints caps the published breakpoint list for the same reason.
// Generous, because unlike frames the whole list is normally worth seeing.
const maxSessionBreakpoints = 200

// DebugFrame is one stack frame as published.
//
// 🔴 Line is 1-BASED on the wire, matching active.json and matching how every
// tool prints a location. The App hands this type 0-based buffer lines and
// DebugPublisher.Set converts — see toWire, the one place the +1 happens.
type DebugFrame struct {
	Name string `json:"name"`
	File string `json:"file"` // absolute; "" when the adapter had no source
	Line int    `json:"line"`
}

// DebugBreakpoint is one breakpoint as published. Verified is what the ADAPTER
// said, so a panel can distinguish "set" from "the debugger will actually stop
// here" — false whenever no session is running, which is honest: nothing has
// bound it.
//
// Line is 1-BASED on the wire, converted by toWire with everything else.
type DebugBreakpoint struct {
	File       string `json:"file"`
	Line       int    `json:"line"`
	Enabled    bool   `json:"enabled"`
	Verified   bool   `json:"verified"`
	Condition  string `json:"condition,omitempty"`
	LogMessage string `json:"logMessage,omitempty"`
}

// DebugSession is the published snapshot.
//
// 🔴 It has two coordinate spellings and the type cannot tell you which one you
// are holding: the App builds it with 0-BASED buffer lines and
// DebugPublisher.Set returns the 1-based wire form. Nothing else in the editor
// constructs one, which is what keeps that safe — see toWire.
type DebugSession struct {
	Adapter string `json:"adapter"` // "delve", "debugpy", "js-debug"; "" when idle

	// Config names what this session is running: the target the adapter was
	// pointed at, which is a package directory, a script, or a URL for a
	// browser session. The editor now reads .vscode/launch.json (see
	// internal/app/launchpicker.go), so this may come from a configuration the
	// user picked rather than from the file that was on screen — which is
	// exactly why a panel must show it rather than infer it from the active
	// file.
	Config string `json:"config"`

	State  string `json:"state"`  // one of the DebugState* constants
	Reason string `json:"reason"` // why it stopped: "breakpoint", "step", "exception"

	File     string `json:"file"` // where it stopped; "" unless State is stopped
	Line     int    `json:"line"` // 1-based on the wire; 0 when there is no location
	ThreadID int    `json:"threadId"`

	Frames     []DebugFrame `json:"frames"`
	FrameTotal int          `json:"frameTotal"` // depth BEFORE the cap

	Breakpoints     []DebugBreakpoint `json:"breakpoints"`
	BreakpointTotal int               `json:"breakpointTotal"` // count BEFORE the cap

	Root       string `json:"root"`       // project root, as active.json publishes it
	StaleAfter int    `json:"staleAfter"` // seconds; see the header
	TS         int64  `json:"ts"`         // unix milliseconds
}

// DebugPublisher coalesces session updates and writes at most one file per
// debounce window. It shares state.go's debounce constant rather than declaring
// a second one: two publishers writing the same directory on different cadences
// would be two different answers to the same question.
type DebugPublisher struct {
	path string

	mu      sync.Mutex
	latest  DebugSession
	pending bool
	timer   *time.Timer
}

// DebugSessionFile is the path the editor writes and panels read.
func DebugSessionFile() string {
	return filepath.Join(Dir(), "debug-session.json")
}

// NewDebugPublisher returns a publisher writing to Dir()/debug-session.json, or
// nil when there is no usable state directory. A nil *DebugPublisher is safe to
// call every method on, so callers never branch.
func NewDebugPublisher() *DebugPublisher {
	dir := Dir()
	if dir == "" {
		return nil
	}
	return &DebugPublisher{path: DebugSessionFile()}
}

// Set records the current session. Cheap and safe to call on every event —
// that is the intended use, because one polled call site cannot miss a stop the
// way hand-placed hooks at each adapter event would.
//
// s arrives in BUFFER coordinates and is converted here.
func (p *DebugPublisher) Set(s DebugSession) {
	if p == nil {
		return
	}
	next := s.toWire()

	p.mu.Lock()
	defer p.mu.Unlock()

	// Compare ignoring TS — toWire leaves it zero on both sides — or every call
	// would look like a change and defeat the debounce entirely.
	if next.sameAs(p.latest) {
		return
	}
	p.latest = next
	if p.pending {
		return // a write is already scheduled; it will pick up this newer value
	}
	p.pending = true
	p.timer = time.AfterFunc(debounce, p.flush)
}

// Flush writes any pending snapshot immediately. Called on shutdown, right
// after the editor sets the session idle, so the clean-exit half of the
// staleness contract does not sit inside the debounce window while the process
// exits underneath it.
func (p *DebugPublisher) Flush() {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.timer != nil {
		p.timer.Stop()
	}
	p.mu.Unlock()
	p.flush()
}

// flush writes the latest snapshot, swallowing every failure. A full disk or a
// read-only home must never disturb editing — same contract as Publisher.flush,
// and the reason neither returns an error.
func (p *DebugPublisher) flush() {
	p.mu.Lock()
	snap := p.latest
	p.pending = false
	p.mu.Unlock()

	snap.TS = time.Now().UnixMilli()
	blob, err := json.Marshal(snap)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p.path), 0o755); err != nil {
		return
	}
	// Temp file plus rename so a reader never observes a half-written payload.
	// Rename is atomic within a directory, which is why the temp file is a
	// SIBLING and not in /tmp.
	tmp, err := os.CreateTemp(filepath.Dir(p.path), ".debug-session-*.json")
	if err != nil {
		return
	}
	name := tmp.Name()
	if _, err := tmp.Write(blob); err != nil {
		tmp.Close()
		os.Remove(name)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return
	}
	if err := os.Rename(name, p.path); err != nil {
		os.Remove(name)
	}
}

// toWire converts a snapshot in buffer coordinates into the published form:
// every line +1, both lists capped, the true counts recorded BEFORE the cap,
// and the staleness window stamped on.
//
// 🔴 This is the ONE place the +1 happens. There are three line-bearing fields
// (the stop location, every frame, every breakpoint) and converting them at
// their call sites would be three places for the same off-by-one to be wrong in
// — which is exactly the mistake Publisher.Set avoids for the cursor.
func (s DebugSession) toWire() DebugSession {
	out := s
	out.StaleAfter = int(DebugStaleAfter / time.Second)
	if out.State == "" {
		out.State = DebugStateIdle
	}
	// No location means no line. Publishing 1 here would put a panel on the
	// first line of a file it is not stopped in.
	out.Line = 0
	if s.File != "" {
		out.Line = s.Line + 1
	}

	out.FrameTotal = len(s.Frames)
	frames := s.Frames
	if len(frames) > maxSessionFrames {
		frames = frames[:maxSessionFrames]
	}
	out.Frames = make([]DebugFrame, len(frames))
	for i, f := range frames {
		out.Frames[i] = DebugFrame{Name: f.Name, File: f.File, Line: f.Line + 1}
	}

	out.BreakpointTotal = len(s.Breakpoints)
	bps := s.Breakpoints
	if len(bps) > maxSessionBreakpoints {
		bps = bps[:maxSessionBreakpoints]
	}
	out.Breakpoints = make([]DebugBreakpoint, len(bps))
	for i, b := range bps {
		out.Breakpoints[i] = b
		out.Breakpoints[i].Line = b.Line + 1
	}

	out.TS = 0 // stamped by flush, and excluded from sameAs
	return out
}

// sameAs reports whether two wire snapshots carry the same content, ignoring
// the timestamp. Written out rather than reached with reflect.DeepEqual so a
// field added later has to be considered here — a comparison that silently
// covers new fields is fine, one that silently misses them publishes a change
// nobody sees.
func (s DebugSession) sameAs(o DebugSession) bool {
	if s.Adapter != o.Adapter || s.Config != o.Config || s.State != o.State ||
		s.Reason != o.Reason || s.File != o.File || s.Line != o.Line ||
		s.ThreadID != o.ThreadID || s.Root != o.Root || s.StaleAfter != o.StaleAfter ||
		s.FrameTotal != o.FrameTotal || s.BreakpointTotal != o.BreakpointTotal ||
		len(s.Frames) != len(o.Frames) || len(s.Breakpoints) != len(o.Breakpoints) {
		return false
	}
	for i := range s.Frames {
		if s.Frames[i] != o.Frames[i] {
			return false
		}
	}
	for i := range s.Breakpoints {
		if s.Breakpoints[i] != o.Breakpoints[i] {
			return false
		}
	}
	return true
}
