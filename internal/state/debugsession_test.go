// =============================================================================
// File: internal/state/debugsession_test.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// readSession unmarshals whatever is on disk right now, failing the test if it
// is not there or not parseable. Reading the FILE rather than the publisher's
// own field is the point: this contract exists to be read from another process,
// so an assertion against in-memory state would prove nothing about it.
func readSession(t *testing.T) DebugSession {
	t.Helper()
	raw, err := os.ReadFile(DebugSessionFile())
	if err != nil {
		t.Fatalf("read published session: %v", err)
	}
	var got DebugSession
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("published session is not JSON: %v (%s)", err, raw)
	}
	return got
}

// TestDebugSessionSurvivesAMissingFile covers the absence cases on both sides
// of the boundary: a publisher that has published nothing leaves no file (a
// reader arriving early finds nothing rather than an empty or half-formed
// payload), and every method is safe on a nil publisher, which is what the App
// holds when there is no usable state directory.
//
// Absence must never be an error here. This file is best-effort by contract —
// the editor must keep working on a read-only home — so a missing file is a
// normal state, not a failure to report.
func TestDebugSessionSurvivesAMissingFile(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	if _, err := os.Stat(DebugSessionFile()); !os.IsNotExist(err) {
		t.Fatalf("nothing has been published yet, so there must be no file: %v", err)
	}

	// A nil publisher is what NewDebugPublisher returns with no state directory,
	// and the App calls straight through it without branching.
	var nilPub *DebugPublisher
	nilPub.Set(DebugSession{State: DebugStateRunning})
	nilPub.Flush()

	// A real publisher whose directory does not exist yet must create it rather
	// than fail — the editor may be the first thing to write there.
	p := NewDebugPublisher()
	if p == nil {
		t.Fatal("NewDebugPublisher returned nil with XDG_STATE_HOME set")
	}
	p.Set(DebugSession{State: DebugStateIdle, Root: "/tmp/proj"})
	p.Flush()
	if got := readSession(t); got.State != DebugStateIdle || got.Root != "/tmp/proj" {
		t.Fatalf("published %+v, want an idle session rooted at /tmp/proj", got)
	}
}

// TestDebugSessionSurvivesAnUnwritableDirectory pins the other half of
// best-effort: a state directory that cannot be written must be swallowed, not
// returned. A full disk disturbing editing is the failure this contract exists
// to avoid, and Set/Flush have no error to return by design.
func TestDebugSessionSurvivesAnUnwritableDirectory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	p := NewDebugPublisher()

	// A regular FILE where the state directory belongs: MkdirAll then fails for
	// every write, on every platform, without needing a permission trick that
	// root would defeat.
	if err := os.WriteFile(filepath.Join(root, "spiceedit"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	p.Set(DebugSession{State: DebugStateStopped, File: "/a/b.go", Line: 4})
	p.Flush() // must not panic and must not block
}

// TestDebugSessionWireLinesAreOneBased is the coordinate contract. The App
// hands this package 0-based buffer lines; active.json and every tool that
// prints a location are 1-based, and all three line-bearing fields have to
// agree — a stop location that is right while a frame is off by one is worse
// than both being wrong, because only one of them looks broken.
func TestDebugSessionWireLinesAreOneBased(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	p := NewDebugPublisher()
	p.Set(DebugSession{
		State: DebugStateStopped,
		File:  "/proj/main.go",
		Line:  41, // 0-based buffer line 41 is what a human calls line 42
		Frames: []DebugFrame{
			{Name: "main.add", File: "/proj/main.go", Line: 41},
			{Name: "main.main", File: "/proj/main.go", Line: 9},
		},
		Breakpoints: []DebugBreakpoint{{File: "/proj/main.go", Line: 41, Enabled: true}},
	})
	p.Flush()

	got := readSession(t)
	if got.Line != 42 {
		t.Errorf("stop line = %d, want 42 for 0-based buffer line 41", got.Line)
	}
	if len(got.Frames) != 2 || got.Frames[0].Line != 42 || got.Frames[1].Line != 10 {
		t.Errorf("frame lines = %+v, want 42 and 10", got.Frames)
	}
	if len(got.Breakpoints) != 1 || got.Breakpoints[0].Line != 42 {
		t.Errorf("breakpoint lines = %+v, want 42", got.Breakpoints)
	}
	if got.StaleAfter != int(DebugStaleAfter/time.Second) {
		t.Errorf("staleAfter = %d, want %d — readers age a payload out with the editor's own number",
			got.StaleAfter, int(DebugStaleAfter/time.Second))
	}
	if got.TS == 0 {
		t.Error("ts is zero, so a reader cannot tell a fresh payload from a dead editor's last one")
	}
}

// TestDebugSessionIdleCarriesNoLocation pins that an idle payload cannot claim
// a line. Publishing 1 for "no location" would put a panel on the first line of
// a file the program is not stopped in, which reads as a wrong answer rather
// than as no answer.
func TestDebugSessionIdleCarriesNoLocation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	p := NewDebugPublisher()
	p.Set(DebugSession{State: DebugStateIdle, Line: 41})
	p.Flush()
	if got := readSession(t); got.Line != 0 || got.File != "" {
		t.Fatalf("idle payload carries %s:%d, want no location at all", got.File, got.Line)
	}
}

// TestDebugSessionComparesIgnoringTimestamp is what makes the debounce work at
// all. Set is called on every editor event, including bare mouse motion; if an
// unchanged session looked like a change because its timestamp had moved, the
// publisher would rewrite the file thousands of times during an ordinary scroll.
//
// Asserted from the FILE (mtime and ts), never from the publisher's own fields:
// reading those without the lock is a data race against the debounce timer, and
// the thing worth pinning is the syscall that did or did not happen.
//
// Its meaning narrowed when the heartbeat arrived: unchanged content does not
// rewrite PER Set CALL, but a live session is re-stamped on the heartbeat
// timer — TestDebugSessionHeartbeats covers that. The interval is pinned long
// here so only the per-call behaviour is under test.
func TestDebugSessionComparesIgnoringTimestamp(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	p := NewDebugPublisher()
	p.hbInterval = time.Hour
	defer p.Close()
	snap := DebugSession{State: DebugStateRunning, Adapter: "delve", Root: "/proj"}

	p.Set(snap)
	p.Flush()
	first := readSession(t)
	st, err := os.Stat(DebugSessionFile())
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 50; i++ {
		p.Set(snap) // byte-identical session, over and over
	}
	time.Sleep(2 * debounce)

	st2, err := os.Stat(DebugSessionFile())
	if err != nil {
		t.Fatal(err)
	}
	if !st2.ModTime().Equal(st.ModTime()) {
		t.Error("an unchanged session triggered a rewrite — the debounce is defeated")
	}
	if again := readSession(t); again.TS != first.TS {
		t.Error("timestamp changed without a session change")
	}

	// The other half: a real change must still land, and without a Flush — the
	// debounce timer is what publishes while the editor keeps running.
	snap.State = DebugStateStopped
	snap.File, snap.Line = "/proj/main.go", 4
	p.Set(snap)
	deadline := time.Now().Add(time.Second)
	for {
		if got := readSession(t); got.State == DebugStateStopped && got.Line == 5 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a real state change never reached disk: %+v", readSession(t))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestDebugSessionCapsTheSnapshot pins that a runaway stack is truncated AND
// that the truncation is admitted. A capped list with no count is a lie: a
// reader shown 20 frames and told nothing has been told the stack is 20 deep.
func TestDebugSessionCapsTheSnapshot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	p := NewDebugPublisher()

	deep := make([]DebugFrame, maxSessionFrames*3)
	for i := range deep {
		deep[i] = DebugFrame{Name: "recurse", File: "/proj/main.go", Line: i}
	}
	many := make([]DebugBreakpoint, maxSessionBreakpoints+7)
	for i := range many {
		many[i] = DebugBreakpoint{File: "/proj/main.go", Line: i, Enabled: true}
	}
	p.Set(DebugSession{State: DebugStateStopped, File: "/proj/main.go", Frames: deep, Breakpoints: many})
	p.Flush()

	got := readSession(t)
	if len(got.Frames) != maxSessionFrames {
		t.Errorf("published %d frames, want the cap of %d", len(got.Frames), maxSessionFrames)
	}
	if got.FrameTotal != len(deep) {
		t.Errorf("frameTotal = %d, want the pre-cap depth %d", got.FrameTotal, len(deep))
	}
	if len(got.Breakpoints) != maxSessionBreakpoints {
		t.Errorf("published %d breakpoints, want the cap of %d", len(got.Breakpoints), maxSessionBreakpoints)
	}
	if got.BreakpointTotal != len(many) {
		t.Errorf("breakpointTotal = %d, want the pre-cap count %d", got.BreakpointTotal, len(many))
	}
}

// TestDebugSessionSetDoesNotMutateTheCaller pins that converting to the wire
// form leaves the App's own snapshot alone. Set takes a value, but the slices
// inside it are shared — writing +1 through them would corrupt the App's frame
// list, and it would do it silently and cumulatively, one line per publish.
func TestDebugSessionSetDoesNotMutateTheCaller(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	p := NewDebugPublisher()
	frames := []DebugFrame{{Name: "main.add", File: "/proj/main.go", Line: 5}}
	bps := []DebugBreakpoint{{File: "/proj/main.go", Line: 5, Enabled: true}}

	p.Set(DebugSession{State: DebugStateStopped, File: "/proj/main.go", Line: 5, Frames: frames, Breakpoints: bps})
	p.Flush()
	p.Set(DebugSession{State: DebugStateStopped, File: "/proj/main.go", Line: 5, Frames: frames, Breakpoints: bps})
	p.Flush()

	if frames[0].Line != 5 || bps[0].Line != 5 {
		t.Fatalf("publishing rewrote the caller's lines to frame=%d bp=%d — they must stay 0-based",
			frames[0].Line, bps[0].Line)
	}
	if got := readSession(t); got.Line != 6 || got.Frames[0].Line != 6 {
		t.Fatalf("second publish produced %d/%d, want a stable 6 — the conversion is not idempotent",
			got.Line, got.Frames[0].Line)
	}
}

// TestDebugSessionLeavesNoTempFiles pins the atomic-write discipline: the temp
// file is a sibling (so rename is atomic within the directory) and it is always
// renamed or removed. A state directory slowly filling with .debug-session-*
// files is the visible symptom of a write path that gave up halfway.
func TestDebugSessionLeavesNoTempFiles(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	p := NewDebugPublisher()
	for i := 0; i < 5; i++ {
		p.Set(DebugSession{State: DebugStateRunning, Adapter: "delve", ThreadID: i})
		p.Flush()
	}
	entries, err := os.ReadDir(Dir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".debug-session-") {
			t.Errorf("temp file %s survived a completed write", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Errorf("state directory holds %d entries, want just debug-session.json", len(entries))
	}
}

// waitFor polls cond until it holds or two seconds pass, failing with msg.
func waitFor(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// peekSession is readSession without the failure: a zero session when the file
// is not there yet, for polling until the debounce timer publishes.
func peekSession() DebugSession {
	var got DebugSession
	if raw, err := os.ReadFile(DebugSessionFile()); err == nil {
		_ = json.Unmarshal(raw, &got)
	}
	return got
}

// heartbeatArmed reports whether the publisher currently holds a heartbeat
// timer, under the lock.
func heartbeatArmed(p *DebugPublisher) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hb != nil
}

// TestDebugSessionHeartbeats pins the staleness fix: a session paused at a
// breakpoint changes nothing, yet its ts must keep moving or a reader's
// StaleAfter check declares a healthy session dead. Once idle, the file must
// stop being rewritten — an idle heartbeat would be noise.
func TestDebugSessionHeartbeats(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	p := NewDebugPublisher()
	p.hbInterval = 20 * time.Millisecond
	defer p.Close()

	stopped := DebugSession{State: DebugStateStopped, File: "/proj/main.go", Line: 4}
	p.Set(stopped)
	waitFor(t, "stopped session never reached disk", func() bool { return peekSession().State == DebugStateStopped })
	first := readSession(t).TS
	// Content is unchanged throughout; only the heartbeat can move ts.
	waitFor(t, "ts never advanced for an unchanged stopped session", func() bool {
		p.Set(stopped)
		return readSession(t).TS > first
	})

	p.Set(DebugSession{State: DebugStateIdle})
	waitFor(t, "idle never reached disk", func() bool { return peekSession().State == DebugStateIdle })
	time.Sleep(60 * time.Millisecond) // let any in-flight beat land
	idleTS := readSession(t).TS
	time.Sleep(120 * time.Millisecond)
	if got := readSession(t).TS; got != idleTS {
		t.Errorf("idle session was heartbeated: ts %d -> %d", idleTS, got)
	}
	if heartbeatArmed(p) {
		t.Error("heartbeat timer still armed while idle")
	}
}

// TestDebugSessionCloseStopsHeartbeat pins that Close leaves no timer running:
// the editor exiting must not leave a goroutine re-writing the file.
func TestDebugSessionCloseStopsHeartbeat(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	p := NewDebugPublisher()
	p.hbInterval = 15 * time.Millisecond
	p.Set(DebugSession{State: DebugStateRunning})
	waitFor(t, "never published", func() bool { return peekSession().State == DebugStateRunning })
	p.Close()
	time.Sleep(40 * time.Millisecond)
	ts := readSession(t).TS
	time.Sleep(80 * time.Millisecond)
	if got := readSession(t).TS; got != ts {
		t.Errorf("heartbeat survived Close: ts %d -> %d", ts, got)
	}
	p.Set(DebugSession{State: DebugStateStopped}) // must be a no-op, not a re-arm
	if heartbeatArmed(p) {
		t.Error("Set after Close re-armed the heartbeat")
	}
}

// TestDebugSessionStampsPID pins that the writer's pid is on the wire.
func TestDebugSessionStampsPID(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	p := NewDebugPublisher()
	defer p.Close()
	p.Set(DebugSession{State: DebugStateStopped})
	p.Flush()
	if got := readSession(t).PID; got != os.Getpid() {
		t.Errorf("pid = %d, want %d", got, os.Getpid())
	}
}

// writeForeign plants a session file as though another editor (pid) wrote it.
func writeForeign(t *testing.T, state string, pid int) {
	t.Helper()
	blob, _ := json.Marshal(DebugSession{State: state, Adapter: "delve", File: "/other/x.go", Line: 9, PID: pid, TS: 1})
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(DebugSessionFile(), blob, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestIdleDoesNotClobberAnotherEditorsLiveSession is the ownership guard: a
// second editor starting up (or quitting) publishes idle, and that must not
// erase the stopped session of a different, running editor.
func TestIdleDoesNotClobberAnotherEditorsLiveSession(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	// pid 1 is alive and is never us.
	writeForeign(t, DebugStateStopped, 1)
	p := NewDebugPublisher()
	defer p.Close()
	p.Set(DebugSession{State: DebugStateIdle})
	p.Flush()
	got := readSession(t)
	if got.State != DebugStateStopped || got.PID != 1 || got.File != "/other/x.go" {
		t.Fatalf("idle write clobbered a live foreign session: %+v", got)
	}

	// The editor that is actually debugging always wins.
	p.Set(DebugSession{State: DebugStateRunning})
	p.Flush()
	if got := readSession(t); got.State != DebugStateRunning || got.PID != os.Getpid() {
		t.Fatalf("a live write lost to a foreign session: %+v", got)
	}
}

// TestIdleOverwritesDeadOrUnownedSessions pins every case in which an idle
// write is allowed: dead owner, own pid, already idle, pid-less legacy file,
// unparseable file, and no file at all.
func TestIdleOverwritesDeadOrUnownedSessions(t *testing.T) {
	garbage := func(t *testing.T) {
		writeForeign(t, DebugStateStopped, 1)
		if err := os.WriteFile(DebugSessionFile(), []byte("{nope"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]func(t *testing.T){
		"dead pid": func(t *testing.T) { writeForeign(t, DebugStateStopped, deadPID(t)) },
		"own pid":  func(t *testing.T) { writeForeign(t, DebugStateStopped, os.Getpid()) },
		"idle":     func(t *testing.T) { writeForeign(t, DebugStateIdle, 1) },
		"no pid":   func(t *testing.T) { writeForeign(t, DebugStateStopped, 0) },
		"garbage":  garbage,
		"no file":  func(t *testing.T) {},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			setup(t)
			p := NewDebugPublisher()
			defer p.Close()
			p.Set(DebugSession{State: DebugStateIdle})
			p.Flush()
			if got := readSession(t); got.State != DebugStateIdle || got.PID != os.Getpid() {
				t.Fatalf("idle write was refused: %+v", got)
			}
		})
	}
}
