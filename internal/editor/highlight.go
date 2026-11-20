// =============================================================================
// File: internal/editor/highlight.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-04-29
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

package editor

import (
	"strings"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/gdamore/tcell/v2"

	"github.com/cloudmanic/spice-edit/internal/theme"
)

// Highlight tokenises src using a Chroma lexer chosen by filename (falling
// back to content-based detection, then to a plain-text lexer) and returns a
// per-line slice of styles parallel to the buffer's lines: styles[i][j] is
// the style for rune j on line i.
//
// Returning a per-rune style grid keeps the renderer simple — it just looks
// up the style for each cell it draws — at the cost of some memory.
// For files small enough to comfortably review, that's a fine trade.
func Highlight(filename, src string, t theme.Theme) [][]tcell.Style {
	return highlightSource(filename, src, t)
}

// highlightLeadLines is how many rows above and below the viewport we
// tokenise but discard, so the lexer can re-enter state it couldn't see
// otherwise, like a multi-line comment or quoted string that spans the
// viewport. Without this lead, scrolling into the middle of a long comment
// colours the body as plain code because the lexer starts fresh at the
// viewport. The lead runs both above and below: chroma needs both
// delimiters of a multi-line block in range to colour the body, so a
// comment that opens above and closes below the viewport needs the opener
// reached by the upper lead and the closer reached by the lower lead.
// Bounded so keystroke cost still follows terminal height, not file size:
// a block longer than the combined lead is the rare case that still
// mis-colours.
const highlightLeadLines = 256

// HighlightVisible returns a style grid for the current viewport. Only
// visible rows are kept in the output so keystroke cost follows terminal
// height, not file size. To stay correct inside multi-line comments and
// strings that span the viewport, tokenisation starts a bounded lead above
// the top and ends a bounded lead below the bottom; those lead rows are
// styled then thrown away.
//
// This is the stateless form (tests, one-off callers). The renderer goes
// through Tab.refreshStyles, which layers a lexer cache and scroll reuse on
// the same primitives.
func HighlightVisible(filename string, lines []string, startLine, height int, t theme.Theme) [][]tcell.Style {
	styles := make([][]tcell.Style, len(lines))
	startLine, endLine, ok := clampViewport(lines, startLine, height)
	if !ok {
		return styles
	}
	leadStart, leadEnd := leadWindow(len(lines), startLine, endLine)
	src := strings.Join(lines[leadStart:leadEnd], "\n")
	lexer, _ := resolveLexer(filename, src)
	leadStyles := highlightWithLexer(lexer, src, t)
	fillViewport(styles, leadStyles, leadStart, startLine, endLine)
	return styles
}

// clampViewport normalises a requested viewport to the buffer: a negative
// start becomes 0 and the end is cut at the last line. ok is false when
// there is nothing to highlight (no height, or the start is past the end).
func clampViewport(lines []string, startLine, height int) (start, end int, ok bool) {
	if height <= 0 || startLine >= len(lines) {
		return startLine, startLine, false
	}
	if startLine < 0 {
		startLine = 0
	}
	end = startLine + height
	if end > len(lines) {
		end = len(lines)
	}
	return startLine, end, true
}

// leadWindow returns the half-open line range [leadStart, leadEnd) that is
// tokenised for the viewport [startLine, endLine): the viewport plus
// highlightLeadLines of context above and below, cut to the buffer.
func leadWindow(nLines, startLine, endLine int) (leadStart, leadEnd int) {
	leadStart = startLine - highlightLeadLines
	if leadStart < 0 {
		leadStart = 0
	}
	leadEnd = endLine + highlightLeadLines
	if leadEnd > nLines {
		leadEnd = nLines
	}
	return leadStart, leadEnd
}

// fillViewport points styles[i] at the lexed row for each viewport line i,
// where leadStyles[0] is buffer line leadStart. The rows are shared, never
// copied or mutated, so a retained lead grid can feed many viewports.
func fillViewport(styles, leadStyles [][]tcell.Style, leadStart, startLine, endLine int) {
	for i := startLine; i < endLine; i++ {
		idx := i - leadStart
		if idx < 0 || idx >= len(leadStyles) {
			continue
		}
		styles[i] = leadStyles[idx]
	}
}

// resolveLexer picks and coalesces the Chroma lexer for filename, falling
// back to content analysis of src and then to the plain-text lexer.
// byName reports that the lexer came from the file name alone, which is
// the only outcome that depends on nothing but the key and is therefore
// safe to cache; a content-sniffed lexer can change as the user types.
func resolveLexer(filename, src string) (lexer chroma.Lexer, byName bool) {
	lexer = lexers.Match(filename)
	byName = lexer != nil
	if lexer == nil {
		lexer = lexers.Analyse(src)
	}
	if lexer == nil {
		lexer = lexers.Fallback
	}
	// Coalesce merges adjacent same-type tokens; cheaper to scan in render.
	return chroma.Coalesce(lexer), byName
}

// highlightSource tokenises src and returns one style row per source line.
func highlightSource(filename, src string, t theme.Theme) [][]tcell.Style {
	lexer, _ := resolveLexer(filename, src)
	return highlightWithLexer(lexer, src, t)
}

// highlightWithLexer is highlightSource with the lexer already chosen, so a
// caller holding a cached lexer skips the Match / Analyse lookup entirely.
func highlightWithLexer(lexer chroma.Lexer, src string, t theme.Theme) [][]tcell.Style {
	base := tcell.StyleDefault.Background(t.BG).Foreground(t.Text)

	// Pre-allocate a styles grid sized to the source. We seed every cell
	// with the base style so untokenised runes still render readably.
	lines := strings.Split(src, "\n")
	styles := baseStyleGrid(lines, base)

	iter, err := lexer.Tokenise(nil, src)
	if err != nil {
		return styles
	}

	line, col := 0, 0
	for tok := iter(); tok != chroma.EOF; tok = iter() {
		st := styleForToken(tok.Type, t, base)
		for _, r := range tok.Value {
			if r == '\n' {
				line++
				col = 0
				continue
			}
			if line < len(styles) && col < len(styles[line]) {
				styles[line][col] = st
			}
			col++
		}
	}
	return styles
}

// refreshStyles brings t.Styles up to date for the viewport [ScrollY,
// ScrollY+h) and is the single highlight entry point for both render paths.
// It is a no-op when nothing changed. Otherwise it:
//
//   - resolves the lexer through a per-Tab cache keyed by HighlightKey(), so
//     lexers.Match (and its Analyse fallback) is not paid on every keystroke;
//   - on a pure scroll (content not stale, same key / theme / line count)
//     whose new viewport still sits well inside the previously lexed lead
//     window, reuses that lexed grid instead of tokenising again;
//   - reuses the outer Styles slice when its length still matches, rather
//     than allocating one slot per buffer line on every keystroke.
//
// An edit sets StyleStale, which always forces a fresh tokenise, so reuse
// can never show colours for text that has since changed.
func (t *Tab) refreshStyles(h int, th theme.Theme) {
	key := t.HighlightKey()
	if !t.StyleStale && t.Styles != nil && t.ScrollY == t.lastHighlightScrollY &&
		h == t.lastHighlightHeight && key == t.hlKey {
		return
	}
	lines := t.Buffer.Lines
	start, end, ok := clampViewport(lines, t.ScrollY, h)

	if ok && !t.canReuseLead(key, th, len(lines), start, end) {
		leadStart, leadEnd := leadWindow(len(lines), start, end)
		src := strings.Join(lines[leadStart:leadEnd], "\n")
		lexer := t.lexerFor(key, src)
		t.hlLead = highlightWithLexer(lexer, src, th)
		t.hlLeadStart, t.hlLeadEnd = leadStart, leadEnd
		t.hlKey, t.hlTheme, t.hlLines, t.hlValid = key, th, len(lines), true
		t.hlRuns++
	}

	styles := t.Styles
	if styles == nil || len(styles) != len(lines) {
		styles = make([][]tcell.Style, len(lines))
	} else {
		for i := t.hlFillStart; i < t.hlFillEnd && i < len(styles); i++ {
			styles[i] = nil
		}
	}
	t.hlFillStart, t.hlFillEnd = 0, 0
	if ok {
		fillViewport(styles, t.hlLead, t.hlLeadStart, start, end)
		t.hlFillStart, t.hlFillEnd = start, end
	}
	t.Styles = styles
	// hlKey is also what the early-out compares against; keep it current even
	// when there was nothing to lex (empty viewport).
	t.hlKey = key
	t.StyleStale = false
	t.lastHighlightScrollY = t.ScrollY
	t.lastHighlightHeight = h
}

// canReuseLead reports whether the retained lead grid can serve the
// viewport [start, end) without re-tokenising. It demands the same lexer
// key, theme and line count, no pending edit, and that the viewport sit at
// least half a lead inside the lexed window (unless that edge is the file's
// own edge), so a reused viewport still has real context above and below.
func (t *Tab) canReuseLead(key string, th theme.Theme, nLines, start, end int) bool {
	if t.StyleStale || !t.hlValid || key != t.hlKey || th != t.hlTheme || nLines != t.hlLines {
		return false
	}
	margin := highlightLeadLines / 2
	if t.hlLeadStart > 0 && start < t.hlLeadStart+margin {
		return false
	}
	if t.hlLeadEnd < nLines && end > t.hlLeadEnd-margin {
		return false
	}
	return start >= t.hlLeadStart && end <= t.hlLeadEnd
}

// lexerFor returns the coalesced lexer for key, from the per-Tab cache when
// the key is unchanged. Only name-matched lexers are cached; a lexer chosen
// by sniffing src (an untitled buffer) is recomputed each time because its
// answer can change as the content does.
func (t *Tab) lexerFor(key, src string) chroma.Lexer {
	if t.hlLexer != nil && t.hlLexerKey == key {
		return t.hlLexer
	}
	lexer, byName := resolveLexer(key, src)
	if byName {
		t.hlLexer, t.hlLexerKey = lexer, key
	} else {
		t.hlLexer, t.hlLexerKey = nil, ""
	}
	return lexer
}

// baseStyleGrid returns a correctly shaped grid pre-filled with base.
func baseStyleGrid(lines []string, base tcell.Style) [][]tcell.Style {
	styles := make([][]tcell.Style, len(lines))
	for i, ln := range lines {
		runes := []rune(ln)
		row := make([]tcell.Style, len(runes))
		for j := range row {
			row[j] = base
		}
		styles[i] = row
	}
	return styles
}

// styleForToken maps a Chroma token type to a tcell.Style using the active
// theme. We match by category first (Keyword, LiteralString, etc.) so the
// mapping stays tight across the dozens of language-specific subtypes.
func styleForToken(tt chroma.TokenType, t theme.Theme, base tcell.Style) tcell.Style {
	switch tt.Category() {
	case chroma.Keyword:
		return base.Foreground(t.SynKeyword)
	case chroma.LiteralString:
		return base.Foreground(t.SynString)
	case chroma.LiteralNumber:
		return base.Foreground(t.SynNumber)
	case chroma.Comment:
		return base.Foreground(t.SynComment).Italic(true)
	case chroma.Operator:
		return base.Foreground(t.SynOperator)
	case chroma.Punctuation:
		return base.Foreground(t.SynPunct)
	case chroma.Literal:
		return base.Foreground(t.SynConstant)
	case chroma.Name:
		switch tt {
		case chroma.NameFunction, chroma.NameFunctionMagic:
			return base.Foreground(t.SynFunction)
		case chroma.NameClass, chroma.NameNamespace:
			return base.Foreground(t.SynType)
		case chroma.NameBuiltin, chroma.NameBuiltinPseudo:
			return base.Foreground(t.SynBuiltin)
		case chroma.NameConstant:
			return base.Foreground(t.SynConstant)
		case chroma.NameVariable, chroma.NameVariableInstance,
			chroma.NameVariableClass, chroma.NameVariableGlobal,
			chroma.NameVariableAnonymous:
			return base.Foreground(t.SynVariable)
		case chroma.NameTag:
			return base.Foreground(t.SynType)
		case chroma.NameAttribute:
			return base.Foreground(t.SynVariable)
		}
		return base
	}
	return base
}
