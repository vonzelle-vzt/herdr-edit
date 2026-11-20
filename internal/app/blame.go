// =============================================================================
// File: internal/app/blame.go
// Author: Vonzelle Brown
// Created: 2026-07-30
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// blame.go is inline git blame: who last touched the line the cursor is on,
// dimmed at the end of that line, the way GitLens does it.
//
// Only the CURSOR's line is blamed, never the viewport. `git blame` on a whole
// file is linear in history and would run on every scroll tick; one line is a
// single cheap call, and it is also the only line whose authorship you are
// actually asking about. The answer is cached per (path, line) and computed on
// a goroutine that delivers a posted event, the same shape diagnostics and the
// LSP requests use — calling git inline would block the event loop on a
// process, and a slow repo would freeze typing.
//
// 🔴 Diagnostics own the end of the line when both want it. An error on this
// line matters more than who wrote it, and two dim strings fighting for the
// same columns is worse than either alone.

package app

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
)

// blameKey identifies one blame lookup.
type blameKey struct {
	path string
	line int
}

// blameEvent carries a finished lookup back to the main loop.
type blameEvent struct {
	key  blameKey
	text string
	when time.Time
}

// When satisfies tcell.Event.
func (e *blameEvent) When() time.Time { return e.when }

// menuToggleInlineBlame flips inline blame. Bound to Esc b and a menu row.
func (a *App) menuToggleInlineBlame() {
	a.closeMenu()
	a.blameEnabled = !a.blameEnabled
	if a.blameEnabled {
		a.flash("Inline blame on")
	} else {
		a.flash("Inline blame off")
	}
}

// inlineBlameLabel is the menu's toggling label.
func (a *App) inlineBlameLabel() string {
	if a.blameEnabled {
		return "Hide inline blame"
	}
	return "Show inline blame"
}

// blameRestDelay is how long the cursor must sit on a line before it is
// blamed. Holding an arrow key crosses dozens of lines a second, and a git
// process per line is the exact load this delay exists to avoid.
const blameRestDelay = 250 * time.Millisecond

// blameScheduler is the debounce and single-flight state for inline blame.
//
// It lives beside the App rather than in it (a package-level registry keyed by
// *App) so this change does not touch App's field list; there is one App per
// process, so the registry holds one entry outside of tests. Every field is
// read and written on the UI goroutine only: the timer goroutine just posts a
// blameRestEvent carrying the generation it was armed under.
type blameScheduler struct {
	gen      uint64   // bumped whenever the wanted line changes; stale timers compare against it
	wanted   blameKey // the line the cursor is resting on (or about to rest on)
	hasWant  bool     // wanted is meaningful
	timer    *time.Timer
	inflight bool      // one git process is running
	pending  *blameKey // latest rested line that arrived while one was in flight

	// Seams, so tests count git invocations without a repo or real time.
	run   func(path string, line int) string
	delay time.Duration
	post  func(ev tcell.Event) // nil means App.post
}

// blameSched returns the App's scheduler, creating it with the real runner and
// delay on first use. Only the UI goroutine calls it, so no locking.
func (a *App) blameSched() *blameScheduler {
	if a.blame == nil {
		a.blame = &blameScheduler{run: runGitBlameLine, delay: blameRestDelay}
	}
	return a.blame
}

// cancel invalidates any armed timer so it can no longer act, and forgets the
// wanted line.
func (s *blameScheduler) cancel() {
	s.gen++
	s.hasWant = false
	s.pending = nil
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
}

// blameRestEvent says the cursor stayed on one line for the full delay. It
// carries the generation it was armed under so a timer that raced a later
// cursor move is dropped rather than blaming a line the user has left.
type blameRestEvent struct {
	gen  uint64
	key  blameKey
	when time.Time
}

// When satisfies tcell.Event.
func (e *blameRestEvent) When() time.Time { return e.when }

// apply runs on the UI goroutine: if the cursor is still where the timer was
// armed, ask for the blame.
func (e *blameRestEvent) apply(a *App) {
	s := a.blameSched()
	if e.gen != s.gen {
		return
	}
	s.timer = nil
	tab := a.activeTabPtr()
	if !a.blameEnabled || tab == nil || tab.Dirty || tab.Path != e.key.path || tab.Cursor.Line != e.key.line {
		// The world moved on between arming and firing. Forget the line so the
		// next draw re-arms rather than waiting on a timer that is gone.
		s.hasWant = false
		return
	}
	a.startBlame(e.key)
}

// maybeRequestBlame arms a rest timer for the cursor's line when it is not
// cached. Polled from the draw path like publishActive and maybeSyncLSP — one
// call site cannot miss a case, whereas hooks scattered across every cursor
// mutation would. It never starts git itself: only a line the cursor has
// rested on for blameRestDelay is blamed, so holding an arrow key costs
// nothing. Draw runs on every event, including bare mouse motion, so it must
// not re-arm the timer for a line it is already waiting on.
func (a *App) maybeRequestBlame() {
	if !a.blameEnabled {
		return
	}
	tab := a.activeTabPtr()
	if tab == nil || tab.IsImage() || tab.Path == "" || tab.Dirty {
		// A dirty buffer's line numbers no longer match what git knows, so a
		// blame answer would be attributed to the wrong line. Silence is right.
		return
	}
	key := blameKey{path: tab.Path, line: tab.Cursor.Line}
	if a.blameCache == nil {
		a.blameCache = make(map[blameKey]string)
		a.blameInflight = make(map[blameKey]bool)
	}
	s := a.blameSched()
	if _, ok := a.blameCache[key]; ok {
		// Answered already; cancel any timer armed for a line we left.
		if !s.hasWant || s.wanted != key {
			s.cancel()
			s.wanted, s.hasWant = key, true
		}
		return
	}
	if s.hasWant && s.wanted == key {
		return // already waiting on, or running, this line
	}
	s.cancel()
	s.wanted, s.hasWant = key, true
	gen := s.gen
	post := s.post
	if post == nil {
		post = a.post
	}
	s.timer = time.AfterFunc(s.delay, func() {
		post(&blameRestEvent{gen: gen, key: key, when: time.Now()})
	})
}

// startBlame runs git for key unless a lookup is already in flight, in which
// case it remembers key and handleBlame requests it on completion. At most one
// git process exists at a time.
func (a *App) startBlame(key blameKey) {
	s := a.blameSched()
	if _, ok := a.blameCache[key]; ok {
		return
	}
	if s.inflight {
		k := key
		s.pending = &k
		return
	}
	s.inflight = true
	if a.blameInflight == nil {
		a.blameInflight = make(map[blameKey]bool)
	}
	a.blameInflight[key] = true
	run := s.run
	post := s.post
	if post == nil {
		post = a.post
	}
	done := a.trackJob()
	go func() {
		defer done()
		text := run(key.path, key.line)
		post(&blameEvent{key: key, text: text, when: time.Now()})
	}()
}

// handleBlame stores a finished lookup, then requests the line the cursor
// rested on while it ran, if that is still where the cursor is.
func (a *App) handleBlame(e *blameEvent) {
	if a.blameCache == nil {
		a.blameCache = make(map[blameKey]string)
	}
	if a.blameInflight != nil {
		delete(a.blameInflight, e.key)
	}
	s := a.blameSched()
	s.inflight = false
	a.blameCache[e.key] = e.text
	if p := s.pending; p != nil {
		s.pending = nil
		if s.hasWant && s.wanted == *p {
			a.startBlame(*p)
		}
	}
}

// invalidateBlame drops the cache for a path. Called after a save, because the
// line the cursor sits on may now be attributed to a different commit.
func (a *App) invalidateBlame(path string) {
	for k := range a.blameCache {
		if k.path == path {
			delete(a.blameCache, k)
		}
	}
	// Forget the wanted line too, or the next draw would see "already waiting
	// on this line" and never re-request the answer just thrown away.
	if s := a.blameSched(); s.hasWant && s.wanted.path == path {
		s.cancel()
	}
}

// runGitBlameLine shells out for a single line and formats it. Returns "" when
// the file is untracked, the repo is absent, or git fails for any reason —
// blame is an ornament, and a repo without history must simply show nothing
// rather than an error the user cannot act on.
func runGitBlameLine(path string, line int) string {
	dir := filepath.Dir(path)
	spec := strconv.Itoa(line+1) + "," + strconv.Itoa(line+1)
	cmd := exec.Command("git", "blame", "-L", spec, "--porcelain", "--", filepath.Base(path))
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return formatBlamePorcelain(string(out))
}

// formatBlamePorcelain turns `git blame --porcelain` output into the one-line
// summary shown at the end of the line.
//
// Pure and exported to the package so every shape can be tested without a repo:
// an uncommitted line, a normal commit, and a truncated payload each have a
// distinct correct answer, and the uncommitted case is the one users hit
// constantly while editing.
func formatBlamePorcelain(out string) string {
	if strings.TrimSpace(out) == "" {
		return ""
	}
	var author, summary string
	var when int64
	for _, ln := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(ln, "author "):
			author = strings.TrimSpace(strings.TrimPrefix(ln, "author "))
		case strings.HasPrefix(ln, "author-time "):
			when, _ = strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(ln, "author-time ")), 10, 64)
		case strings.HasPrefix(ln, "summary "):
			summary = strings.TrimSpace(strings.TrimPrefix(ln, "summary "))
		}
	}
	if author == "" {
		return ""
	}
	// git reports a line you have not committed as this exact author name; the
	// commit fields are meaningless there, so say the useful thing instead.
	if author == "Not Committed Yet" {
		return "You • uncommitted"
	}
	if summary == "" {
		return author
	}
	if when == 0 {
		return author + " • " + summary
	}
	return author + ", " + humanizeAge(time.Unix(when, 0)) + " • " + summary
}

// humanizeAge renders a commit time the way a blame annotation should read —
// coarse and relative. Nobody reading a blame line needs a timestamp.
func humanizeAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	case d < 14*24*time.Hour:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%d weeks ago", int(d.Hours()/24/7))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%d months ago", int(d.Hours()/24/30))
	default:
		return fmt.Sprintf("%d years ago", int(d.Hours()/24/365))
	}
}

// drawInlineBlame paints the cursor line's blame at the end of that line.
// Runs in the overlay pass after Tab.Render, next to the diagnostics overlay,
// and defers to it: a line carrying a diagnostic shows the diagnostic.
func (a *App) drawInlineBlame(ex, ey, ew, eh int) {
	if !a.blameEnabled {
		return
	}
	tab := a.activeTabPtr()
	if tab == nil || tab.IsImage() || tab.Wrap || tab.Dirty {
		return
	}
	line := tab.Cursor.Line
	text := a.blameCache[blameKey{path: tab.Path, line: line}]
	if text == "" {
		return
	}
	// Diagnostics win the end of this line.
	for _, d := range a.diagnosticsFor(tab.Path) {
		if d.Range.Start.Line == line {
			return
		}
	}
	endCol := tab.LineRuneLen(line)
	dx, dy, ok := tab.ScreenPos(line, endCol, ew, eh)
	if !ok {
		return
	}
	startX := dx + inlineMessageGap
	avail := ew - startX
	if avail < inlineMessageMinWidth {
		return
	}
	out := truncateEllipsis(text, avail)
	style := tcell.StyleDefault.Foreground(a.theme.Muted).Dim(true)
	sx, sy := ex+startX, ey+dy
	for i, r := range []rune(out) {
		a.screen.SetContent(sx+i, sy, r, nil, style)
	}
}
