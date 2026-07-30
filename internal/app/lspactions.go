// =============================================================================
// File: internal/app/lspactions.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-07-30
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

// lspactions.go wires hover and go-to-definition to the UI.
//
// Both requests, the Manager methods in front of them, and the `hover` and `definition` client
// capabilities we advertise in initialize were all already implemented and tested — and nothing
// called them. An audit of every LSP method in the package found the two most-used features after
// diagnostics sitting complete and unreachable, so this file is the missing keypress, not new
// protocol work.
//
// Requests run on a goroutine and deliver their answer as a posted event, the same shape
// diagnostics use. Calling them inline would block Run's PollEvent loop on a language server: gopls
// answers a hover in milliseconds, but a server that is indexing, wedged, or dead would freeze the
// editor for the whole timeout with no way to type.

package app

import (
	"context"
	"strings"
	"time"

	"github.com/cloudmanic/spice-edit/internal/editor"
	"github.com/cloudmanic/spice-edit/internal/lsp"
)

// lspRequestTimeout bounds a hover or definition round trip. Generous enough for a cold gopls on a
// large module, short enough that a wedged server resolves to a status message rather than silence.
const lspRequestTimeout = 5 * time.Second

// lspHoverEvent carries hover text from the request goroutine onto the main loop.
type lspHoverEvent struct {
	when time.Time
	text string
}

// When satisfies tcell.Event.
func (e *lspHoverEvent) When() time.Time { return e.when }

// lspJumpEvent carries go-to-definition results onto the main loop.
type lspJumpEvent struct {
	when time.Time
	locs []lsp.Location
}

// When satisfies tcell.Event.
func (e *lspJumpEvent) When() time.Time { return e.when }

// lspCursorPos converts the active tab's cursor into an LSP position, or reports false when there is
// nothing to ask about.
//
// 🔴 The column conversion is the whole reason this helper exists. LSP counts columns in UTF-16 code
// units; the buffer is rune indexed. They agree for ASCII, so getting this wrong survives every test
// until a line contains an emoji or CJK text and every hover on that line silently targets the wrong
// character. Convert at the boundary, never in the middle.
func (a *App) lspCursorPos() (path string, pos lsp.Position, ok bool) {
	tab := a.activeTabPtr()
	if tab == nil || tab.Path == "" || a.lsp == nil {
		return "", lsp.Position{}, false
	}
	line := tab.LineText(tab.Cursor.Line)
	return tab.Path, lsp.Position{
		Line:      tab.Cursor.Line,
		Character: lsp.RuneColToUTF16(line, tab.Cursor.Col),
	}, true
}

// menuHover asks the language server what is under the cursor. Bound to Esc h.
func (a *App) menuHover() {
	a.closeMenu()
	path, pos, ok := a.lspCursorPos()
	if !ok {
		a.flash("No language server for this file")
		return
	}
	m := a.lsp
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), lspRequestTimeout)
		defer cancel()
		text, err := m.Hover(ctx, path, pos)
		if err != nil {
			text = ""
		}
		a.post(&lspHoverEvent{when: time.Now(), text: text})
	}()
}

// menuGoToDefinition jumps to the definition of the symbol under the cursor. Bound to Esc d.
func (a *App) menuGoToDefinition() {
	a.closeMenu()
	path, pos, ok := a.lspCursorPos()
	if !ok {
		a.flash("No language server for this file")
		return
	}
	m := a.lsp
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), lspRequestTimeout)
		defer cancel()
		locs, err := m.Definition(ctx, path, pos)
		if err != nil {
			locs = nil
		}
		a.post(&lspJumpEvent{when: time.Now(), locs: locs})
	}()
}

// handleLSPHover shows hover text in the status bar.
//
// One line, deliberately. Hover payloads are markdown and often several lines of signature plus
// docs, and this editor has no popup surface to put that in yet — a status flash is the honest
// version of the feature rather than a reason to withhold it. The first non-empty, non-fence line is
// the signature in every server we have looked at, which is the part people actually want.
func (a *App) handleLSPHover(e *lspHoverEvent) {
	line := firstMeaningfulLine(e.text)
	if line == "" {
		a.flash("No hover information here")
		return
	}
	a.flash(line)
}

// firstMeaningfulLine picks the most useful single line out of a markdown hover payload: the first
// line that is not blank and not a code-fence marker.
func firstMeaningfulLine(s string) string {
	for _, raw := range strings.Split(s, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "```") {
			continue
		}
		return line
	}
	return ""
}

// handleLSPJump opens the first definition result and puts the cursor on it.
//
// MoveCursorTo rather than assigning Cursor directly: it is the exported mutator that sets the
// cursorMoved flag, and without that flag Render will not call EnsureVisible, so the jump would move
// the cursor to a line the viewport never scrolls to — the definition would be "opened" off screen.
func (a *App) handleLSPJump(e *lspJumpEvent) {
	if len(e.locs) == 0 {
		a.flash("No definition found")
		return
	}
	loc := e.locs[0]
	path := lsp.PathFromURI(loc.URI)
	if path == "" {
		a.flash("Definition has no local path")
		return
	}

	if tab := a.activeTabPtr(); tab == nil || tab.Path != path {
		a.openFile(path)
	}
	tab := a.activeTabPtr()
	if tab == nil {
		a.flash("Could not open " + path)
		return
	}

	// Back through the UTF-16 boundary, in the other direction this time.
	target := loc.Range.Start
	col := lsp.UTF16ToRuneCol(tab.LineText(target.Line), target.Character)
	tab.MoveCursorTo(editor.Position{Line: target.Line, Col: col}, false)
}

// menuToggleWrap flips word wrap for the active tab. Bound to Esc z, mirroring the alt+Z most editors
// use for this, and per-tab rather than global because whether you want wrap depends on the file:
// prose and JSON yes, a wide table or aligned columns no.
//
// Wrap changes the geometry, not the content, but StyleStale is set anyway: the highlighter caches per
// viewport, and cursorMoved forces the scroll anchor to be recomputed for the new row layout so the
// cursor does not end up off screen the instant the mode flips.
func (a *App) menuToggleWrap() {
	a.closeMenu()
	tab := a.activeTabPtr()
	if tab == nil {
		a.flash("No file open")
		return
	}
	tab.Wrap = !tab.Wrap
	tab.ScrollSub = 0
	tab.StyleStale = true
	tab.MoveCursorTo(tab.Cursor, false) // sets cursorMoved, so the viewport re-anchors
	if tab.Wrap {
		a.flash("Word wrap: on")
	} else {
		a.flash("Word wrap: off")
	}
}
