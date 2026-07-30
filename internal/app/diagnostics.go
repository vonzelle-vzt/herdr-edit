// =============================================================================
// File: internal/app/diagnostics.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-07-29
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

package app

import (
	"context"
	"sort"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/cloudmanic/spice-edit/internal/lsp"
)

// Language-server diagnostics, drawn as an overlay on top of the rendered tab.
//
// Drawing over Render rather than inside it is deliberate: the editor's render
// path is hot (it re-tokenises on scroll) and diagnostics arrive on their own
// schedule from a background process. Keeping them a separate pass means a
// server that publishes nothing costs literally nothing, and the two concerns
// never have to agree about anything except cell coordinates.

// diagnosticsEvent carries a published set from the LSP goroutine onto the
// tcell event queue. Background work must never mutate UI state directly —
// this is the same discipline the tree refresh and custom actions already use.
type diagnosticsEvent struct {
	when  time.Time
	uri   string
	diags []lsp.Diagnostic
}

func (e *diagnosticsEvent) When() time.Time { return e.when }

// lspLogEvent surfaces a server complaint on the status line. A misconfigured
// server that says nothing is the single most common way an LSP setup appears
// to "just not work", so these are worth a flash.
type lspLogEvent struct {
	when time.Time
	msg  string
}

func (e *lspLogEvent) When() time.Time { return e.when }

// startLSP brings up the language-server manager for this project.
//
// Called once at startup. It starts no servers by itself — the manager spawns
// one lazily on the first file of a matching language — so the cost here is a
// couple of allocations even in a project with no server installed.
func (a *App) startLSP() {
	if a.rootDir == "" {
		return
	}
	m := lsp.NewManager(a.rootDir)
	m.OnDiagnostics = func(uri string, diags []lsp.Diagnostic) {
		a.post(&diagnosticsEvent{when: time.Now(), uri: uri, diags: diags})
	}
	m.OnLog = func(msg string) {
		a.post(&lspLogEvent{when: time.Now(), msg: msg})
	}
	a.lsp = m
	a.diagnostics = make(map[string][]lsp.Diagnostic)
}

// post pushes an event onto the tcell queue, dropping it if the screen has gone
// away. PostEvent blocks when the queue is full, which would deadlock a server
// goroutine against a quitting editor.
func (a *App) post(ev tcell.Event) {
	if a.screen == nil {
		return
	}
	_ = a.screen.PostEvent(ev)
}

// handleDiagnostics stores a published set. Servers send the COMPLETE list for
// a document every time, including an empty list when everything is fixed, so
// this replaces rather than merges — merging would leave repaired problems on
// screen forever.
func (a *App) handleDiagnostics(e *diagnosticsEvent) {
	if a.diagnostics == nil {
		a.diagnostics = make(map[string][]lsp.Diagnostic)
	}
	path := lsp.PathFromURI(e.uri)
	if path == "" {
		return
	}
	if len(e.diags) == 0 {
		delete(a.diagnostics, path)
		return
	}
	// Sort by position so the gutter, the overlay and the status line all agree
	// about which diagnostic is "first" on a line.
	diags := append([]lsp.Diagnostic(nil), e.diags...)
	sort.SliceStable(diags, func(i, j int) bool {
		if diags[i].Range.Start.Line != diags[j].Range.Start.Line {
			return diags[i].Range.Start.Line < diags[j].Range.Start.Line
		}
		return diags[i].Range.Start.Character < diags[j].Range.Start.Character
	})
	a.diagnostics[path] = diags
}

// lspDidOpen / lspDidChange / lspDidSave / lspDidClose mirror the editor's
// document lifecycle to the server. Each is nil-safe so the whole feature can
// be absent without the call sites branching.
func (a *App) lspDidOpen(path, text string) {
	if a.lsp == nil || path == "" {
		return
	}
	go a.lsp.DidOpen(context.Background(), path, text)
}

func (a *App) lspDidChange(path, text string) {
	if a.lsp == nil || path == "" {
		return
	}
	a.lsp.DidChange(path, text)
}

func (a *App) lspDidSave(path, text string) {
	if a.lsp == nil || path == "" {
		return
	}
	a.lsp.DidSave(path, text)
}

func (a *App) lspDidClose(path string) {
	if a.lsp == nil || path == "" {
		return
	}
	a.lsp.DidClose(path)
	delete(a.diagnostics, path)
}

// lspSyncInterval is how often the active buffer is compared against what the
// server last saw. Diagnostics that lag a keystroke or two are fine; diagnostics
// that cost a full-buffer hash on every redraw are not, and a redraw happens on
// mouse movement.
const lspSyncInterval = 400 * time.Millisecond

// maybeSyncLSP sends the active buffer to the server when it has changed.
//
// Called from the event loop next to publishActive, for the same reason: the
// editor mutates text from many places, and one polled call site cannot miss an
// edit the way a set of hand-placed hooks would. The interval guard means the
// buffer is only walked while the user is actually typing, and the length check
// makes the common "nothing changed" case free.
func (a *App) maybeSyncLSP() {
	if a.lsp == nil {
		return
	}
	now := time.Now()
	if now.Sub(a.lspSyncedAt) < lspSyncInterval {
		return
	}
	a.lspSyncedAt = now

	tab := a.activeTabPtr()
	if tab == nil || tab.Path == "" || tab.IsImage() {
		return
	}
	text := tab.Buffer.String()
	if a.lspSentText == nil {
		a.lspSentText = make(map[string]string)
	}
	if a.lspSentText[tab.Path] == text {
		return
	}
	a.lspSentText[tab.Path] = text
	a.lspDidChange(tab.Path, text)
}

// diagnosticsFor returns the diagnostics for a tab, newest publish wins.
func (a *App) diagnosticsFor(path string) []lsp.Diagnostic {
	if a.diagnostics == nil || path == "" {
		return nil
	}
	return a.diagnostics[path]
}

// diagnosticAt finds the diagnostic covering a buffer position, preferring the
// most severe one — with an error and a hint on the same word, the error is
// what the user needs to read.
func (a *App) diagnosticAt(path string, line, col int) *lsp.Diagnostic {
	var best *lsp.Diagnostic
	for i := range a.diagnosticsFor(path) {
		d := &a.diagnosticsFor(path)[i]
		if !covers(*d, line, col) {
			continue
		}
		if best == nil || severityRank(d.Severity) < severityRank(best.Severity) {
			best = d
		}
	}
	return best
}

// covers reports whether a diagnostic's range contains a position. End is
// exclusive per the protocol, but a zero-width range (which servers do emit,
// for "something is missing here") must still match the cell it points at.
func covers(d lsp.Diagnostic, line, col int) bool {
	s, e := d.Range.Start, d.Range.End
	if line < s.Line || line > e.Line {
		return false
	}
	if s == e {
		return line == s.Line && col == s.Character
	}
	if line == s.Line && col < s.Character {
		return false
	}
	if line == e.Line && col >= e.Character {
		return false
	}
	return true
}

// severityRank orders severities so the lowest number is the most urgent,
// matching the protocol's own numbering. An unset severity sorts last: servers
// may omit it, and treating that as an error would paint a file red over a hint.
func severityRank(s lsp.Severity) int {
	if s == lsp.SeverityUnset {
		return 99
	}
	return int(s)
}

// diagnosticColor maps severity to a theme colour, reusing the palette rather
// than hardcoding, so a restyle cannot leave the overlay behind.
func (a *App) diagnosticColor(s lsp.Severity) tcell.Color {
	switch s {
	case lsp.SeverityError:
		return a.theme.Error
	case lsp.SeverityWarning:
		return a.theme.GitModified // amber in this palette
	case lsp.SeverityInformation:
		return a.theme.Accent
	}
	return a.theme.Muted
}

// drawDiagnostics underlines every visible diagnostic on the active tab.
//
// Underline is used rather than a background wash because the editor already
// spends background colour on the selection and the current line, and stacking
// them makes both unreadable. Terminals that cannot render curly underlines
// fall back to a straight one automatically.
func (a *App) drawDiagnostics() {
	tab := a.activeTabPtr()
	if tab == nil || tab.IsImage() {
		return
	}
	diags := a.diagnosticsFor(tab.Path)
	if len(diags) == 0 {
		return
	}
	ex, ey, ew, eh := a.editorRect()

	for _, d := range diags {
		style := tcell.StyleDefault.
			Foreground(a.diagnosticColor(d.Severity)).
			Underline(true)

		for line := d.Range.Start.Line; line <= d.Range.End.Line; line++ {
			text := tab.LineText(line)
			// The server speaks UTF-16; the buffer is rune-indexed. Converting
			// per line is what keeps a squiggle under the right characters on a
			// line containing an emoji or CJK text.
			startCol := 0
			if line == d.Range.Start.Line {
				startCol = lsp.UTF16ToRuneCol(text, d.Range.Start.Character)
			}
			endCol := tab.LineRuneLen(line)
			if line == d.Range.End.Line {
				endCol = lsp.UTF16ToRuneCol(text, d.Range.End.Character)
			}
			// A zero-width range still deserves a visible mark, or "expected a
			// semicolon here" would render as nothing at all.
			if endCol <= startCol {
				endCol = startCol + 1
			}

			for col := startCol; col < endCol; col++ {
				dx, dy, ok := tab.ScreenPos(line, col, ew, eh)
				if !ok {
					continue
				}
				sx, sy := ex+dx, ey+dy
				// Repaint the existing rune with the diagnostic style so the
				// syntax highlighting underneath survives; only the underline
				// and its colour are ours.
				mainc, combc, existing, _ := a.screen.GetContent(sx, sy)
				bg, _, _ := existing.Decompose()
				_ = bg
				a.screen.SetContent(sx, sy, mainc, combc, style)
			}
		}
	}
}

// diagnosticStatus is the one-line summary for the status bar: what is wrong
// under the cursor, or a count for the file. This is the half of "Error Lens"
// that does not need inline text rendering, and it is where the message
// actually becomes readable.
func (a *App) diagnosticStatus() string {
	tab := a.activeTabPtr()
	if tab == nil {
		return ""
	}
	if d := a.diagnosticAt(tab.Path, tab.Cursor.Line, tab.Cursor.Col); d != nil {
		msg := lsp.FirstLine(d.Message)
		if code := d.CodeString(); code != "" {
			msg += " [" + code + "]"
		}
		return d.Severity.String() + ": " + msg
	}
	var errs, warns int
	for _, d := range a.diagnosticsFor(tab.Path) {
		switch d.Severity {
		case lsp.SeverityError:
			errs++
		case lsp.SeverityWarning:
			warns++
		}
	}
	switch {
	case errs > 0 && warns > 0:
		return plural(errs, "error") + ", " + plural(warns, "warning")
	case errs > 0:
		return plural(errs, "error")
	case warns > 0:
		return plural(warns, "warning")
	}
	return ""
}

// plural renders "1 error" / "2 errors" without a format-string dance.
func plural(n int, word string) string {
	s := itoa(n) + " " + word
	if n != 1 {
		s += "s"
	}
	return s
}
