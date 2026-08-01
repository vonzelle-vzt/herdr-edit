// =============================================================================
// File: internal/editor/find.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-04-30
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

// find.go implements the editor's in-file search primitives plus find/
// replace. Matching operates on rune-decoded lines so multi-byte characters
// behave as one column each (consistent with how the cursor / selection
// already treat columns elsewhere). Three independent toggles are
// supported via FindOptions: case-sensitivity, whole-word, and regex (Go's
// stdlib regexp/RE2 — no backtracking, so a hostile pattern can't hang the
// editor). Replace and ReplaceAll build on top of Matches(): they never
// re-scan the buffer after an edit, so replacement text that happens to
// contain the search term can't be picked up as a new match mid-pass.

package editor

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// FindOptions selects the matching mode for Matches (and, through it,
// SetFindQuery / Replace / ReplaceAll). The zero value is the historical
// v1 behaviour: case-insensitive plain substring.
type FindOptions struct {
	CaseSensitive bool
	WholeWord     bool
	Regex         bool
}

// isWordRune reports whether r counts as part of a "word" for whole-word
// matching and for the auto-close-quote heuristic in tab.go. Letters,
// digits, and underscore match most languages' identifier rules closely
// enough for both purposes.
func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

// Match describes one find hit. Line and Col follow the same rune-indexed
// convention as Position; Width is the rune count of the query so the
// renderer can paint the right number of cells without re-running the
// matcher.
type Match struct {
	Line  int
	Col   int
	Width int
}

// FindAll returns every case-insensitive substring match of query inside
// buf, in document order. Kept as a thin wrapper around Matches for the
// callers (and tests) written before FindOptions existed — the zero-value
// FindOptions reproduces this exact behaviour, so it can never itself
// return an error.
func FindAll(buf *Buffer, query string) []Match {
	m, _ := Matches(buf, query, FindOptions{})
	return m
}

// Matches returns every match of query inside buf under opts, in document
// order. This is the preview API: the UI uses it to show a match count and
// to build the FindMatches list that Replace / ReplaceAll operate on. An
// empty query or nil buffer returns (nil, nil) — the caller is expected to
// clear its UI rather than show "0 of 0" results.
//
// The only error case is opts.Regex with an unparseable pattern; callers
// should surface it (e.g. in the find bar) rather than treating a bad
// regex as "no matches", which would look like a silent failure to type
// the query correctly.
func Matches(buf *Buffer, query string, opts FindOptions) ([]Match, error) {
	if query == "" || buf == nil {
		return nil, nil
	}
	if opts.Regex {
		return regexMatches(buf, query, opts)
	}
	return substringMatches(buf, query, opts), nil
}

// substringMatches implements the non-regex path: a rune-by-rune scan for
// query, honouring the case-sensitive and whole-word toggles. Matches do
// not overlap: after a hit the scanner advances past the matched run, so
// "aaaa" with query "aa" yields two matches at columns 0 and 2.
func substringMatches(buf *Buffer, query string, opts FindOptions) []Match {
	needle := []rune(query)
	if !opts.CaseSensitive {
		needle = []rune(strings.ToLower(query))
	}
	if len(needle) == 0 {
		return nil
	}
	var out []Match
	for lineIdx, raw := range buf.Lines {
		hay := []rune(raw)
		cmpHay := hay
		if !opts.CaseSensitive {
			cmpHay = []rune(strings.ToLower(raw))
		}
		col := 0
		for col+len(needle) <= len(cmpHay) {
			if runesEqual(cmpHay[col:col+len(needle)], needle) {
				if !opts.WholeWord || isWholeWordMatch(hay, col, len(needle)) {
					out = append(out, Match{Line: lineIdx, Col: col, Width: len(needle)})
				}
				col += len(needle)
				continue
			}
			col++
		}
	}
	return out
}

// isWholeWordMatch reports whether the match starting at col with the
// given rune width is bounded by non-word characters (or the start/end of
// the line) on both sides — the standard "whole word" definition used by
// every mainstream editor's search.
func isWholeWordMatch(hay []rune, col, width int) bool {
	if col > 0 && isWordRune(hay[col-1]) {
		return false
	}
	end := col + width
	if end < len(hay) && isWordRune(hay[end]) {
		return false
	}
	return true
}

// regexMatches implements the regex path using Go's stdlib regexp (RE2),
// which never backtracks — so unlike PCRE-style engines, a hostile pattern
// can't hang the editor on catastrophic backtracking. Byte offsets from
// FindAllStringIndex are converted to rune columns to keep Match's
// rune-indexed contract intact for multi-byte lines.
func regexMatches(buf *Buffer, pattern string, opts FindOptions) ([]Match, error) {
	pat := pattern
	if opts.WholeWord {
		pat = `\b(?:` + pat + `)\b`
	}
	if !opts.CaseSensitive {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return nil, fmt.Errorf("invalid pattern: %w", err)
	}
	var out []Match
	for lineIdx, line := range buf.Lines {
		for _, pair := range re.FindAllStringIndex(line, -1) {
			if pair[0] == pair[1] {
				// A zero-width match (e.g. "a*" against "b") isn't
				// something the UI can usefully jump to or replace, and
				// looping FindNext over it would never advance.
				continue
			}
			col := utf8.RuneCountInString(line[:pair[0]])
			width := utf8.RuneCountInString(line[pair[0]:pair[1]])
			out = append(out, Match{Line: lineIdx, Col: col, Width: width})
		}
	}
	return out, nil
}

// runesEqual returns true when two equal-length rune slices match
// element-for-element. Inlined so the hot inner loop of FindAll doesn't
// pay for a generic slices.Equal call (which exists in 1.21+ but pulls in
// the slices package).
func runesEqual(a, b []rune) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// FirstMatchAtOrAfter returns the index into matches of the first hit at
// or after cursor, or 0 when cursor sits past the last match (we wrap
// around to the top — that's what the user expects after typing a query
// at the bottom of a file).
//
// Returns -1 when matches is empty so callers can short-circuit without
// re-checking the length.
func FirstMatchAtOrAfter(matches []Match, cursor Position) int {
	if len(matches) == 0 {
		return -1
	}
	for i, m := range matches {
		if m.Line > cursor.Line || (m.Line == cursor.Line && m.Col >= cursor.Col) {
			return i
		}
	}
	// Cursor is past every match — wrap to the top.
	return 0
}

// MatchPosition returns the cursor-friendly Position at the start of m.
// Trivial helper, but it keeps callers from constructing Position literals
// by hand (which loses the "rune-indexed" intent at the call site).
func MatchPosition(m Match) Position {
	return Position{Line: m.Line, Col: m.Col}
}

// MatchEndPosition returns the position one past the end of m — useful
// when the caller wants to set a selection that covers the match.
func MatchEndPosition(m Match) Position {
	return Position{Line: m.Line, Col: m.Col + m.Width}
}

// SetFindQuery installs a new search query on the tab, recomputes the
// match list against the current buffer under the tab's current
// FindOptions, and points FindIndex at the first match at or after the
// cursor (so the user lands on the nearest hit, not always the first hit
// in the file). An empty query clears all find state — symmetrical with
// closing the bar via Esc.
//
// The cursor is left where it is; SetFindQuery only updates state. It is
// the caller's job to call FocusCurrentMatch when they want the cursor
// to actually move (which is what happens on the first non-empty query
// and on every Enter / Shift-Enter press).
//
// When Regex is enabled and the pattern doesn't compile, FindErr is set
// and FindMatches is cleared — the caller should show FindErr instead of
// a "0 of 0" count, which would look like the query legitimately had no
// hits rather than being broken.
func (t *Tab) SetFindQuery(query string) {
	t.FindQuery = query
	if query == "" {
		t.FindMatches = nil
		t.FindIndex = -1
		t.FindErr = nil
		return
	}
	matches, err := Matches(t.Buffer, query, t.findOptions())
	t.FindErr = err
	if err != nil {
		t.FindMatches = nil
		t.FindIndex = -1
		return
	}
	t.FindMatches = matches
	t.FindIndex = FirstMatchAtOrAfter(t.FindMatches, t.Cursor)
}

// findOptions bundles the tab's toggle fields into a FindOptions for
// Matches. A tiny helper, but it keeps SetFindQuery / SetFindOptions from
// duplicating the field-by-field construction.
func (t *Tab) findOptions() FindOptions {
	return FindOptions{
		CaseSensitive: t.FindCaseSensitive,
		WholeWord:     t.FindWholeWord,
		Regex:         t.FindRegex,
	}
}

// SetFindOptions updates the case-sensitive / whole-word / regex toggles
// and immediately recomputes matches against the current query, exactly
// as if the user had retyped it — so flipping a toggle in the UI takes
// effect right away instead of waiting for the next keystroke.
func (t *Tab) SetFindOptions(caseSensitive, wholeWord, regex bool) {
	t.FindCaseSensitive = caseSensitive
	t.FindWholeWord = wholeWord
	t.FindRegex = regex
	t.SetFindQuery(t.FindQuery)
}

// FocusCurrentMatch moves the cursor (and anchor — we don't want a
// dangling selection from an earlier action) to the start of the
// currently-pointed match. No-op when FindIndex is out of range, so
// callers don't have to re-check it themselves.
func (t *Tab) FocusCurrentMatch() {
	if t.FindIndex < 0 || t.FindIndex >= len(t.FindMatches) {
		return
	}
	m := t.FindMatches[t.FindIndex]
	t.Cursor = MatchPosition(m)
	t.Anchor = t.Cursor
	t.cursorMoved = true
}

// FindNext advances FindIndex by one (wrapping at the end) and moves
// the cursor onto the new match. No-op when there are no matches. Used
// by Enter inside the find bar and by the Esc-g "again" leader.
func (t *Tab) FindNext() {
	if len(t.FindMatches) == 0 {
		return
	}
	t.FindIndex = (t.FindIndex + 1) % len(t.FindMatches)
	t.FocusCurrentMatch()
}

// FindPrev moves FindIndex backwards by one (wrapping at the start) and
// moves the cursor onto the new match. Used by Shift-Enter inside the
// find bar.
func (t *Tab) FindPrev() {
	if len(t.FindMatches) == 0 {
		return
	}
	t.FindIndex--
	if t.FindIndex < 0 {
		t.FindIndex = len(t.FindMatches) - 1
	}
	t.FocusCurrentMatch()
}

// matchAtRune returns the index into FindMatches of the match that
// covers (line, col), or -1 when none does. The renderer calls this once
// per visible cell, so the implementation has to stay cheap — a linear
// scan is fine for realistic file sizes (hundreds of matches at most),
// and avoids the overhead of building a per-line index. If this ever
// shows up in a profile, switch to a sorted-by-line bsearch.
func (t *Tab) matchAtRune(line, col int) int {
	for i, m := range t.FindMatches {
		if m.Line != line {
			continue
		}
		if col >= m.Col && col < m.Col+m.Width {
			return i
		}
	}
	return -1
}

// ClearFind drops every piece of find state. The app calls this when the
// buffer has been edited enough that the cached match list is stale and
// can't safely be re-used; the user will re-type their query.
func (t *Tab) ClearFind() {
	t.FindQuery = ""
	t.FindMatches = nil
	t.FindIndex = -1
	t.FindErr = nil
}

// Replace substitutes the text of the currently-focused match
// (FindMatches[FindIndex]) with replacement, moves the cursor past the
// inserted text, and re-derives the match list against the now-mutated
// buffer — which naturally lands FindIndex on the next match at or after
// the cursor, giving the "replace and advance" behaviour the caller
// wants without any manual index bookkeeping. Recorded as one undo step.
// Returns false (no-op) when there is no current match, e.g. an empty
// query or a stale FindIndex.
func (t *Tab) Replace(replacement string) bool {
	if t.IsImage() || t.FindIndex < 0 || t.FindIndex >= len(t.FindMatches) {
		return false
	}
	m := t.FindMatches[t.FindIndex]
	t.pushUndo(undoGroupStructural)
	start := MatchPosition(m)
	end := MatchEndPosition(m)
	// bufDelete / bufInsert, not the raw buffer methods. This shipped calling
	// Buffer.DeleteRange + Buffer.InsertString directly, which is the one
	// documented way to drift every mark below the edit: replacing a
	// multi-line match, or replacing with text containing a newline, moves
	// lines without renumbering a single breakpoint.
	t.bufDelete(start, end)
	pos := t.bufInsert(start, replacement)
	t.Cursor = pos
	t.Anchor = pos
	t.Dirty = true
	t.StyleStale = true
	t.cursorMoved = true
	t.SetFindQuery(t.FindQuery)
	return true
}

// ReplaceAll substitutes every match currently in FindMatches with
// replacement as a single undo step, so one Undo call restores the whole
// file rather than requiring N. Matches are applied last-to-first: since
// an edit can only shift positions that come *after* it in the buffer,
// walking backwards means every remaining match position in the snapshot
// stays valid without having to re-run Matches (and re-run it against a
// buffer that now contains replacement text) between edits — which is
// also what guarantees replacement text containing the search term can
// never be picked up as a new match mid-pass.
//
// The cursor is left at the end of the first (topmost) replacement.
// Returns the number of replacements made.
func (t *Tab) ReplaceAll(replacement string) int {
	if t.IsImage() || len(t.FindMatches) == 0 {
		return 0
	}
	t.pushUndo(undoGroupStructural)
	n := len(t.FindMatches)
	var cursorAfter Position
	for i := n - 1; i >= 0; i-- {
		m := t.FindMatches[i]
		start := MatchPosition(m)
		end := MatchEndPosition(m)
		// Same mark-tracking reason as Replace above: this is the fork's one
		// existing "rewrite N regions as one undo step", and it was the
		// counter-example rather than the template until this line changed.
		t.bufDelete(start, end)
		pos := t.bufInsert(start, replacement)
		if i == 0 {
			cursorAfter = pos
		}
	}
	t.Cursor = t.Buffer.Clamp(cursorAfter)
	t.Anchor = t.Cursor
	t.Dirty = true
	t.StyleStale = true
	t.cursorMoved = true
	t.SetFindQuery(t.FindQuery)
	return n
}
