// =============================================================================
// File: internal/app/dochighlight.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// dochighlight.go tints every VISIBLE occurrence of the identifier under the
// cursor — ambient, ownerless of any key, drawn on every frame the way VS
// Code has always done it. There is no leader binding here on purpose: the
// whole point is that it needs none, and its absence for even a few minutes
// is what people notice about a terminal editor.
//
// It draws between tab.Render and drawDiagnostics in app.go's draw(): a
// squiggle under an actual mistake must always win the eye over a tint that's
// just saying "this word again", so diagnostics paint on top, last.
//
// Matching goes through editor.Matches with the SAME FindOptions shape the
// find bar and internal/search already use (CaseSensitive, WholeWord) — one
// matcher, so this can never disagree with what Esc f would find on the
// identical text.
package app

import (
	"github.com/cloudmanic/spice-edit/internal/editor"
)

// minHighlightSymbolRunes is the shortest identifier worth tinting. Below
// this, common short tokens (i, x, id two letters like "ok") would light up
// half the screen and the feature would read as noise rather than signal.
const minHighlightSymbolRunes = 2

// visibleOccurrences finds every whole-word, case-sensitive occurrence of sym
// within tab's currently visible rows [top, top+height). It matches against a
// buffer built ONLY from those rows, via editor.Matches — the exact function
// every other "find" surface in this fork already calls — so document
// highlighting can never find something the find bar wouldn't.
//
// Returned Match.Line is relative to the visible window (0 == top), not the
// tab's absolute buffer line; callers add top back before calling ScreenPos.
// Keeping it window-relative is what makes this cheap to call on every
// redraw: the sub-buffer built here is only ever `height` lines long,
// regardless of how large the real file is.
func (a *App) visibleOccurrences(tab *editor.Tab, sym string, top, height int) []editor.Match {
	if tab == nil || sym == "" || height <= 0 {
		return nil
	}
	lineCount := len(tab.Buffer.Lines)
	if top < 0 {
		top = 0
	}
	if top >= lineCount {
		return nil
	}
	end := top + height
	if end > lineCount {
		end = lineCount
	}
	sub := &editor.Buffer{Lines: append([]string(nil), tab.Buffer.Lines[top:end]...)}
	matches, err := editor.Matches(sub, sym, editor.FindOptions{CaseSensitive: true, WholeWord: true})
	if err != nil {
		return nil
	}
	return matches
}

// drawDocumentHighlights repaints the background of every visible occurrence
// of the identifier under the cursor, leaving the syntax foreground colour
// untouched — same discipline drawDiagnostics uses for its underline.
//
// Suppressed when: the tab is wrapped (ScreenPos's contract doesn't hold for
// wrapped rows — see drawDiagnosticsInline for the identical guard), there's
// an active selection or an open find query (both already own the highlight
// background; a third tint on top would just be noise), the tab is synthetic
// or an image (no real identifiers to highlight), or the symbol under the
// cursor is shorter than minHighlightSymbolRunes.
func (a *App) drawDocumentHighlights() {
	tab := a.activeTabPtr()
	if tab == nil || tab.IsImage() || tab.Synthetic {
		return
	}
	if tab.Wrap || tab.HasSelection() || tab.FindQuery != "" {
		return
	}
	sym := a.symbolAtCursor()
	if len([]rune(sym)) < minHighlightSymbolRunes {
		return
	}

	ex, ey, ew, eh := a.editorRect()
	lineCount := len(tab.Buffer.Lines)

	// 🔴 Memoized on (path, sym, ScrollY, height, lineCount). draw() clears
	// and repaints the WHOLE screen on every event this app handles,
	// including bare mouse motion — an uncached full editor.Matches scan on
	// every one of those is a stall the user reads as the editor hanging,
	// not as a feature working correctly.
	if a.docHighlightPath != tab.Path || a.docHighlightSym != sym ||
		a.docHighlightScrollY != tab.ScrollY || a.docHighlightHeight != eh ||
		a.docHighlightLineCount != lineCount {
		a.docHighlightMatches = a.visibleOccurrences(tab, sym, tab.ScrollY, eh)
		a.docHighlightPath = tab.Path
		a.docHighlightSym = sym
		a.docHighlightScrollY = tab.ScrollY
		a.docHighlightHeight = eh
		a.docHighlightLineCount = lineCount
	}

	for _, m := range a.docHighlightMatches {
		line := tab.ScrollY + m.Line
		for col := m.Col; col < m.Col+m.Width; col++ {
			// 🔴 MUST go through ScreenPos, no exceptions. It is the only
			// arithmetic in this fork that accounts for both the gutter and a
			// hard tab occupying more than one screen cell (LineVisualCol) —
			// the same computation Render itself uses to place the cursor.
			// `dx = tab.GutterWidth() + col` is the historical bug this repo
			// shipped: correct on unindented fixtures, wrong on every
			// tab-indented line, which in a Go project is most of them.
			dx, dy, ok := tab.ScreenPos(line, col, ew, eh)
			if !ok {
				continue
			}
			sx, sy := ex+dx, ey+dy
			// Repaint the existing cell's background only, so the syntax
			// foreground colour underneath survives — same discipline
			// drawDiagnostics uses for its underline.
			mainc, combc, existing, _ := a.screen.GetContent(sx, sy)
			a.screen.SetContent(sx, sy, mainc, combc, existing.Background(a.theme.FindMatch))
		}
	}
}
