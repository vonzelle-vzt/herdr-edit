// =============================================================================
// File: internal/app/blame_test.go
// Author: Vonzelle Brown
// Created: 2026-07-30
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package app

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

// TestFormatBlamePorcelain_Commit renders a normal commit: author, a coarse
// relative age, and the subject.
func TestFormatBlamePorcelain_Commit(t *testing.T) {
	ts := time.Now().Add(-72 * time.Hour).Unix()
	out := fmt.Sprintf("abc123 1 1 1\nauthor Ada Lovelace\nauthor-time %d\nsummary Add the engine\n", ts)
	got := formatBlamePorcelain(out)
	for _, want := range []string{"Ada Lovelace", "Add the engine", "days ago"} {
		if !strings.Contains(got, want) {
			t.Errorf("blame line %q is missing %q", got, want)
		}
	}
}

// TestFormatBlamePorcelain_Uncommitted is the case a user hits constantly while
// editing. git reports the author as the literal "Not Committed Yet", whose
// commit fields are meaningless — echoing them would render a line dated 1970.
func TestFormatBlamePorcelain_Uncommitted(t *testing.T) {
	out := "0000000000000000000000000000000000000000 1 1 1\nauthor Not Committed Yet\nauthor-time 0\nsummary Version of x\n"
	got := formatBlamePorcelain(out)
	if !strings.Contains(got, "uncommitted") {
		t.Fatalf("uncommitted line rendered as %q", got)
	}
	if strings.Contains(got, "1970") || strings.Contains(got, "years ago") {
		t.Fatalf("uncommitted line leaked a meaningless timestamp: %q", got)
	}
}

// TestFormatBlamePorcelain_Degrades pins that unusable input yields an empty
// string rather than a half-rendered annotation. Blame is an ornament; a repo
// without history must show nothing, not an error the user cannot act on.
func TestFormatBlamePorcelain_Degrades(t *testing.T) {
	for _, in := range []string{"", "   ", "garbage with no fields", "abc123 1 1 1\n"} {
		if got := formatBlamePorcelain(in); got != "" {
			t.Errorf("formatBlamePorcelain(%q) = %q, want empty", in, got)
		}
	}
}

// TestFormatBlamePorcelain_NoSummary falls back to the author alone rather than
// rendering a dangling separator.
func TestFormatBlamePorcelain_NoSummary(t *testing.T) {
	got := formatBlamePorcelain("abc 1 1 1\nauthor Grace Hopper\n")
	if got != "Grace Hopper" {
		t.Fatalf("got %q, want the bare author", got)
	}
}

// TestHumanizeAge covers the buckets a blame annotation actually uses.
func TestHumanizeAge(t *testing.T) {
	cases := []struct {
		ago  time.Duration
		want string
	}{
		{30 * time.Second, "just now"},
		{20 * time.Minute, "minutes ago"},
		{5 * time.Hour, "hours ago"},
		{3 * 24 * time.Hour, "days ago"},
		{3 * 7 * 24 * time.Hour, "weeks ago"},
		{100 * 24 * time.Hour, "months ago"},
		{800 * 24 * time.Hour, "years ago"},
	}
	for _, c := range cases {
		got := humanizeAge(time.Now().Add(-c.ago))
		if !strings.Contains(got, c.want) {
			t.Errorf("humanizeAge(-%v) = %q, want it to contain %q", c.ago, got, c.want)
		}
	}
}

// TestBlame_ToggleAndReachable guards the reachability failure this fork has
// repeated three times, and the toggle's label flipping in place.
func TestBlame_ToggleAndReachable(t *testing.T) {
	if leaderActionFor('b') == nil {
		t.Fatal("Esc b is not bound — inline blame is unreachable")
	}
	a := newTestApp(t, t.TempDir())
	a.blameEnabled = true
	if got := a.inlineBlameLabel(); got != "Hide inline blame" {
		t.Errorf("label with blame on = %q", got)
	}
	a.menuToggleInlineBlame()
	if a.blameEnabled {
		t.Error("the toggle did not turn blame off")
	}
	if got := a.inlineBlameLabel(); got != "Show inline blame" {
		t.Errorf("label with blame off = %q", got)
	}
}

// TestBlame_NoLookupWhenDirty pins that a modified buffer is never blamed. Line
// numbers in a dirty buffer no longer match what git knows, so an answer would
// be attributed to the wrong line — confidently and invisibly.
func TestBlame_NoLookupWhenDirty(t *testing.T) {
	a := seedNavApp(t, "one\ntwo\nthree\n")
	tab := a.activeTabPtr()
	tab.Dirty = true
	a.blameEnabled = true
	a.maybeRequestBlame()
	if len(a.blameInflight) != 0 {
		t.Fatal("a dirty buffer should not trigger a blame lookup")
	}
}

// TestBlame_InvalidateOnSave checks the cache is dropped for the saved path
// only, since a save can shift every line after an edit.
func TestBlame_InvalidateOnSave(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.blameCache = map[blameKey]string{
		{path: "/a.go", line: 1}: "x",
		{path: "/a.go", line: 9}: "y",
		{path: "/b.go", line: 1}: "z",
	}
	a.invalidateBlame("/a.go")
	if len(a.blameCache) != 1 {
		t.Fatalf("cache has %d entries, want only the untouched file's", len(a.blameCache))
	}
	if a.blameCache[blameKey{path: "/b.go", line: 1}] != "z" {
		t.Error("invalidating one path dropped another path's entry")
	}
}

// blameHarness wires a scheduler to a fake runner and a captured event queue,
// so the tests count "git invocations" without a repo or a real screen.
type blameHarness struct {
	a      *App
	s      *blameScheduler
	events chan tcell.Event
	runs   atomic.Int32
	cur    atomic.Int32
	max    atomic.Int32
	gate   chan struct{} // nil means the runner returns immediately
}

// newBlameHarness builds an App with a short rest delay and a counting runner.
func newBlameHarness(t *testing.T, delay time.Duration) *blameHarness {
	t.Helper()
	h := &blameHarness{a: seedNavApp(t, "a\nb\nc\nd\ne\nf\ng\nh\ni\nj\n"), events: make(chan tcell.Event, 64)}
	h.a.blameEnabled = true
	h.s = h.a.blameSched()
	h.s.delay = delay
	h.s.post = func(ev tcell.Event) { h.events <- ev }
	h.s.run = func(path string, line int) string {
		h.runs.Add(1)
		n := h.cur.Add(1)
		for {
			m := h.max.Load()
			if n <= m || h.max.CompareAndSwap(m, n) {
				break
			}
		}
		if h.gate != nil {
			<-h.gate
		}
		h.cur.Add(-1)
		return fmt.Sprintf("blamed line %d", line)
	}
	return h
}

// next waits for one posted event.
func (h *blameHarness) next(t *testing.T) tcell.Event {
	t.Helper()
	select {
	case ev := <-h.events:
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("no event posted")
		return nil
	}
}

// TestBlame_RapidMovesRunNoGitUntilRest is the regression for the per-line
// process storm: crossing ten lines in a burst must start ZERO git runs, and
// after the cursor rests only the final line is blamed.
func TestBlame_RapidMovesRunNoGitUntilRest(t *testing.T) {
	h := newBlameHarness(t, 60*time.Millisecond)
	tab := h.a.activeTabPtr()
	for line := 0; line < 10; line++ {
		tab.Cursor.Line = line
		h.a.maybeRequestBlame()
	}
	if got := h.runs.Load(); got != 0 {
		t.Fatalf("%d git runs during a rapid move, want 0", got)
	}
	ev := h.next(t).(*blameRestEvent)
	ev.apply(h.a)
	if ev.key.line != 9 {
		t.Fatalf("rested on line %d, want the last (9)", ev.key.line)
	}
	be := h.next(t).(*blameEvent)
	h.a.handleBlame(be)
	if got := h.runs.Load(); got != 1 {
		t.Fatalf("%d git runs after rest, want exactly 1", got)
	}
	if h.a.blameCache[blameKey{path: tab.Path, line: 9}] == "" {
		t.Fatal("the rested line was not cached")
	}
}

// TestBlame_StaleRestEventDropped pins the generation guard: a timer event that
// raced a later cursor move must not blame the line the user has left.
func TestBlame_StaleRestEventDropped(t *testing.T) {
	h := newBlameHarness(t, time.Hour)
	tab := h.a.activeTabPtr()
	tab.Cursor.Line = 2
	h.a.maybeRequestBlame()
	stale := &blameRestEvent{gen: h.s.gen, key: blameKey{path: tab.Path, line: 2}}
	tab.Cursor.Line = 3
	h.a.maybeRequestBlame()
	stale.apply(h.a)
	if h.runs.Load() != 0 || h.s.inflight {
		t.Fatal("a stale timer event started a blame")
	}
}

// TestBlame_RedrawDoesNotRearm pins that draw, which runs on every event
// including mouse motion, does not push the rest timer out forever.
func TestBlame_RedrawDoesNotRearm(t *testing.T) {
	h := newBlameHarness(t, 30*time.Millisecond)
	h.a.activeTabPtr().Cursor.Line = 4
	for i := 0; i < 5; i++ {
		h.a.maybeRequestBlame()
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := h.next(t).(*blameRestEvent); !ok {
		t.Fatal("repeated draws on one line starved the rest timer")
	}
}

// TestBlame_AtMostOneInFlight pins single-flight: a second rested line arriving
// while git runs is remembered and requested only on completion, so concurrency
// never exceeds one.
func TestBlame_AtMostOneInFlight(t *testing.T) {
	h := newBlameHarness(t, time.Millisecond)
	h.gate = make(chan struct{})
	tab := h.a.activeTabPtr()

	tab.Cursor.Line = 1
	h.a.maybeRequestBlame()
	h.next(t).(*blameRestEvent).apply(h.a) // starts line 1, blocks on the gate

	tab.Cursor.Line = 2
	h.a.maybeRequestBlame()
	h.next(t).(*blameRestEvent).apply(h.a) // must wait, not start
	time.Sleep(20 * time.Millisecond)
	if got := h.runs.Load(); got != 1 {
		t.Fatalf("%d runs while one was in flight, want 1", got)
	}

	h.gate <- struct{}{} // finish line 1
	h.a.handleBlame(h.next(t).(*blameEvent))
	h.gate <- struct{}{} // the remembered line 2 now runs
	h.a.handleBlame(h.next(t).(*blameEvent))

	if got := h.runs.Load(); got != 2 {
		t.Fatalf("%d runs total, want 2", got)
	}
	if got := h.max.Load(); got != 1 {
		t.Fatalf("max concurrency %d, want 1", got)
	}
	if h.a.blameCache[blameKey{path: tab.Path, line: 2}] == "" {
		t.Fatal("the remembered line was never blamed")
	}
}

// TestBlame_InvalidateRearms pins that dropping the cache after a save lets the
// same line be requested again instead of being treated as already pending.
func TestBlame_InvalidateRearms(t *testing.T) {
	h := newBlameHarness(t, time.Millisecond)
	tab := h.a.activeTabPtr()
	tab.Cursor.Line = 5
	h.a.maybeRequestBlame()
	h.next(t).(*blameRestEvent).apply(h.a)
	h.a.handleBlame(h.next(t).(*blameEvent))
	h.a.invalidateBlame(tab.Path)
	h.a.maybeRequestBlame()
	if _, ok := h.next(t).(*blameRestEvent); !ok {
		t.Fatal("invalidate did not allow a fresh request")
	}
}
