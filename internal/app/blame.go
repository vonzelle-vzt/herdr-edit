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

// maybeRequestBlame kicks a lookup for the cursor's line if one is not cached
// and not already running. Polled from the draw path like publishActive and
// maybeSyncLSP — one call site cannot miss a case, whereas hooks scattered
// across every cursor mutation would.
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
	if _, ok := a.blameCache[key]; ok {
		return
	}
	if a.blameInflight[key] {
		return
	}
	a.blameInflight[key] = true
	go func() {
		text := runGitBlameLine(key.path, key.line)
		a.post(&blameEvent{key: key, text: text, when: time.Now()})
	}()
}

// handleBlame stores a finished lookup.
func (a *App) handleBlame(e *blameEvent) {
	if a.blameCache == nil {
		a.blameCache = make(map[blameKey]string)
	}
	if a.blameInflight != nil {
		delete(a.blameInflight, e.key)
	}
	a.blameCache[e.key] = e.text
}

// invalidateBlame drops the cache for a path. Called after a save, because the
// line the cursor sits on may now be attributed to a different commit.
func (a *App) invalidateBlame(path string) {
	for k := range a.blameCache {
		if k.path == path {
			delete(a.blameCache, k)
		}
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
