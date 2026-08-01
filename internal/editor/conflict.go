// =============================================================================
// File: internal/editor/conflict.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// conflict.go models a git merge conflict as a REGION of the buffer, and
// resolves one by DELETING lines — never by rebuilding them.
//
// Two rules shape everything in this file, and both exist because getting
// either one wrong is invisible until it has already eaten someone's work.
//
// 🔴 **Git is the authority on WHETHER a file is conflicted; this scanner only
// says WHERE.** A run of `<<<<<<<` inside a string literal, a heredoc, a
// markdown code fence or a test fixture is byte-for-byte identical to a real
// marker, and NO test on the buffer can tell them apart — this very package's
// own conflict_test.go contains such bytes. So Tab.GitUnmerged gates
// RescanConflicts, and internal/app sets it from `git ls-files -u` and nothing
// else. In a clean repo a file full of markers scans to nil, by construction
// rather than by heuristic.
//
// 🔴 **Resolution is pure deletion.** The content the user keeps is already in
// the buffer, verbatim — so nothing is reconstructed and no whitespace, line
// ending, tab/space mix or encoding can be mangled on the way through. Every
// range goes through t.bufDelete (never t.Buffer.DeleteRange), so a breakpoint
// below the region tracks the edit instead of drifting;
// TestAllBufferMutationsGoThroughMarkWrappers reads this file's source and
// fails on a raw mutation.
//
// Tab.Conflicts is a RENDER CACHE. The scan is the source of truth for a
// mutation, which is why both resolve entry points rescan first and refuse
// when the cache no longer describes the buffer.
package editor

import "strings"

// conflictMarkerSize is the SHORTEST run of a marker rune git will write at
// column 0. `conflict-marker-size` is a gitattribute, so a repo may be
// configured to use a longer one — hence every test here is `>=`, never `==`.
// A shorter run is not a marker at all.
const conflictMarkerSize = 7

// conflictGlyph is the gutter glyph drawn on a conflict marker line.
//
// 🔴 U+259A is in the BLOCK ELEMENTS range, and that is why it was chosen.
// `▌` and `▁` (the git change bars) are the only evidence this fork has that a
// glyph renders SINGLE-WIDTH in Terminal.app, tmux and Ghostty alike. A rune
// from an ambiguous-width block renders two cells wide in some of them, which
// would shift the whole gutter — and every overlay that measures from it — one
// column to the right, on every line of every file.
const conflictGlyph = '▚'

// ConflictRegion is one `<<<<<<< / ======= / >>>>>>>` sequence, in 0-based
// buffer lines. All four fields index MARKER lines; the content lives strictly
// between them.
//
// Base is the `|||||||` line of git's diff3 / zdiff3 conflict style and is -1
// under the default `merge` style, where there is no common-ancestor section
// at all. Callers must branch on that rather than assuming a fixed layout: the
// style is a config setting, so both shapes turn up in real repositories.
//
// OursLabel / TheirsLabel are whatever git wrote after the marker — usually
// "HEAD" and a branch name, but a rebase writes commit subjects and a
// cherry-pick writes a SHA. They are display text only; nothing keys off them.
type ConflictRegion struct {
	Start int // the <<<<<<< line
	Base  int // the ||||||| line (diff3 style), or -1 under the merge style
	Sep   int // the ======= line
	End   int // the >>>>>>> line

	OursLabel   string
	TheirsLabel string
}

// ConflictChoice is which side of a conflict to keep. The three values are the
// only three resolutions that are pure deletions of the surrounding markers —
// anything else (an edited merge) is ordinary typing, which the editor already
// does.
type ConflictChoice int

const (
	// ConflictOurs keeps the section between <<<<<<< and ======= (or |||||||
	// under diff3) — the content already on the branch being merged INTO.
	ConflictOurs ConflictChoice = iota
	// ConflictTheirs keeps the section between ======= and >>>>>>>.
	ConflictTheirs
	// ConflictBoth keeps ours followed by theirs, dropping only the markers
	// and the common-ancestor section.
	ConflictBoth
)

// ScanConflicts finds every well-formed conflict region in lines, in ascending
// order, never overlapping. It is pure — no Tab, no git, no I/O — so it is the
// unit the scanner's tests exercise directly.
//
// A single forward pass with one open region. Three shapes deserve calling
// out:
//
//   - A nested `<<<<<<<` DISCARDS the region already open rather than trying to
//     work out which `=======` belongs to which opener. Guessing is how a
//     resolver deletes the wrong half of a file; refusing to emit costs the
//     user one manual edit and nothing else.
//   - A `>>>>>>>` with no `=======` before it closes nothing and emits nothing:
//     without a separator there is no way to say where "ours" ends.
//   - A second `=======` inside an open region is ignored. git's own conflict
//     output never produces one, and treating a later separator as the real one
//     would move the boundary under the user.
func ScanConflicts(lines []string) []ConflictRegion {
	var out []ConflictRegion
	open := emptyConflictRegion()

	for i, line := range lines {
		marker, label, ok := conflictMarkerAt(line)
		if !ok {
			continue
		}
		switch marker {
		case '<':
			// Nested opener: drop whatever was open (see the doc comment) and
			// start again here.
			open = emptyConflictRegion()
			open.Start = i
			open.OursLabel = label
		case '|':
			if open.Start >= 0 && open.Base < 0 && open.Sep < 0 {
				open.Base = i
			}
		case '=':
			if open.Start >= 0 && open.Sep < 0 {
				open.Sep = i
			}
		case '>':
			if open.Start >= 0 && open.Sep >= 0 {
				open.End = i
				open.TheirsLabel = label
				out = append(out, open)
			}
			open = emptyConflictRegion()
		}
	}
	return out
}

// emptyConflictRegion is the "nothing open yet" value. -1 rather than 0 in
// every field, because line 0 is a perfectly legal marker position and a zero
// value would read as one.
func emptyConflictRegion() ConflictRegion {
	return ConflictRegion{Start: -1, Base: -1, Sep: -1, End: -1}
}

// conflictMarkerAt reports whether line begins with a run of at least
// conflictMarkerSize identical marker runes at COLUMN 0, and returns the run's
// rune plus whatever label follows it.
//
// Column 0 and a minimum run length are the whole test. Git writes markers
// flush left and never indents them, so an indented `=======` inside a
// docstring is not a marker; and a `>=` comparison rather than `==` is what
// keeps a repo with `conflict-marker-size` set to something other than 7
// working.
func conflictMarkerAt(line string) (marker rune, label string, ok bool) {
	if line == "" {
		return 0, "", false
	}
	runes := []rune(line)
	switch runes[0] {
	case '<', '|', '=', '>':
	default:
		return 0, "", false
	}
	n := 0
	for n < len(runes) && runes[n] == runes[0] {
		n++
	}
	if n < conflictMarkerSize {
		return 0, "", false
	}
	return runes[0], strings.TrimSpace(string(runes[n:])), true
}

// RescanConflicts refreshes this tab's ConflictRegion cache from the buffer.
//
// 🔴 GitUnmerged is the gate, and it is not optional: without it, opening this
// package's own conflict_test.go in a clean checkout would light up as a
// conflicted file, because the bytes are identical to a real one. internal/app
// sets GitUnmerged from `git ls-files -u` and nothing else.
func (t *Tab) RescanConflicts() {
	if !t.GitUnmerged || t.IsImage() {
		t.Conflicts = nil
		return
	}
	t.Conflicts = ScanConflicts(t.Buffer.Lines)
}

// ConflictAt returns the cached region containing line (marker lines
// included), its index in Tab.Conflicts, and whether one was found. The index
// is what the resolve methods take, so a caller can go from "where the cursor
// is" to "resolve this one" without re-deriving anything.
func (t *Tab) ConflictAt(line int) (ConflictRegion, int, bool) {
	for i, c := range t.Conflicts {
		if line < c.Start {
			break // regions are ascending and never overlap
		}
		if line <= c.End {
			return c, i, true
		}
	}
	return ConflictRegion{}, -1, false
}

// conflictMarkerLine reports whether line is one of a cached region's marker
// lines — the four (three under the merge style) rows gutterMarker paints the
// conflict glyph on. Body lines are deliberately excluded: the gutter says
// where a region STARTS and ENDS, and the body is already carrying the tint.
func (t *Tab) conflictMarkerLine(line int) bool {
	for _, c := range t.Conflicts {
		if line < c.Start {
			return false
		}
		if line == c.Start || line == c.Base || line == c.Sep || line == c.End {
			return true
		}
		if line <= c.End {
			return false // inside this region, but on a body line
		}
	}
	return false
}

// ResolveConflict rewrites the region at idx to keep only choice's side, as a
// single undo step, and reports whether it did anything.
//
// It rescans BEFORE acting and refuses when the fresh scan disagrees with the
// cached region the caller picked idx out of. Tab.Conflicts is a render cache;
// the buffer can be hand-edited between the paint that produced it and the
// keystroke that acts on it, and a resolver that trusts a stale region deletes
// lines that are no longer the ones it was pointed at. Refusing costs one
// redraw; the alternative costs the user their file.
func (t *Tab) ResolveConflict(idx int, choice ConflictChoice) bool {
	if t.IsImage() || idx < 0 || idx >= len(t.Conflicts) {
		return false
	}
	want := t.Conflicts[idx]
	t.RescanConflicts()
	if idx >= len(t.Conflicts) || t.Conflicts[idx] != want {
		return false
	}
	t.pushUndo(undoGroupStructural)
	t.resolveRegion(want, choice)
	t.afterResolve()
	return true
}

// ResolveAllConflicts resolves every region in the file the same way, as ONE
// undo step for the whole file, and returns how many it resolved.
//
// Regions are walked LAST TO FIRST: an edit only shifts the positions that
// come after it, so working backwards keeps every remaining region in the
// freshly-scanned slice valid without re-scanning between edits. Same
// reasoning as ReplaceAll's backwards walk, for the same reason.
func (t *Tab) ResolveAllConflicts(choice ConflictChoice) int {
	if t.IsImage() {
		return 0
	}
	t.RescanConflicts()
	regions := t.Conflicts
	if len(regions) == 0 {
		return 0
	}
	t.pushUndo(undoGroupStructural)
	for i := len(regions) - 1; i >= 0; i-- {
		t.resolveRegion(regions[i], choice)
	}
	t.afterResolve()
	return len(regions)
}

// resolveRegion deletes the line ranges that choice discards, in DESCENDING
// order within the region. Descending is what makes this safe without any
// position bookkeeping: an edit only shifts the lines after it, so a range
// resolved earlier in the loop can never move a range still to come.
//
// Note oursEnd: under diff3 the "ours" section ends at the ||||||| line, not
// at the =======, and the common-ancestor section between them is discarded by
// all three choices — it is context git printed to help, never content anyone
// chose to keep.
func (t *Tab) resolveRegion(c ConflictRegion, choice ConflictChoice) {
	oursEnd := c.Sep
	if c.Base >= 0 {
		oursEnd = c.Base
	}
	switch choice {
	case ConflictTheirs:
		t.deleteLines(c.End, c.End)
		t.deleteLines(c.Start, c.Sep)
	case ConflictBoth:
		t.deleteLines(c.End, c.End)
		t.deleteLines(oursEnd, c.Sep)
		t.deleteLines(c.Start, c.Start)
	default: // ConflictOurs
		t.deleteLines(oursEnd, c.End)
		t.deleteLines(c.Start, c.Start)
	}
}

// deleteLines removes buffer lines [lo, hi] INCLUSIVE, through bufDelete.
//
// 🔴 It eats the newline that ENDS line lo-1 rather than the one that ends hi,
// and that choice is load-bearing twice over:
//
//  1. Deleting forwards to {hi+1, 0} is wrong when hi is the LAST line —
//     Buffer.DeleteRange clamps a position past the end back onto the end of
//     the final line, so the range collapses and the emptied row is left
//     behind. That is the stray blank line a `>>>>>>>` at end-of-file used to
//     produce.
//  2. bufDelete drops marks on (lo, hi] and keeps the mark on lo. Deleting
//     backwards shifts that window onto exactly [lo, hi] — the lines that
//     actually disappear — so a breakpoint on the line immediately AFTER the
//     block moves up with its text instead of being deleted along with it.
//
// A block starting at line 0 has no preceding newline to eat and falls back to
// the forward form; the one mark that can be misattributed there sits on line
// 0 itself, which is bufDelete's own documented semantics rather than
// something this function can fix.
func (t *Tab) deleteLines(lo, hi int) {
	n := t.Buffer.LineCount()
	if lo < 0 || lo >= n || hi < lo {
		return
	}
	if hi >= n {
		hi = n - 1
	}
	if lo > 0 {
		t.bufDelete(
			Position{Line: lo - 1, Col: t.LineRuneLen(lo - 1)},
			Position{Line: hi, Col: t.LineRuneLen(hi)},
		)
		return
	}
	if hi+1 < n {
		t.bufDelete(Position{Line: 0, Col: 0}, Position{Line: hi + 1, Col: 0})
		return
	}
	// The block is the whole buffer. Leave the single empty line every Buffer
	// is required to have rather than deleting past the end.
	t.bufDelete(Position{Line: 0, Col: 0}, Position{Line: hi, Col: t.LineRuneLen(hi)})
}

// afterResolve settles the tab once a resolution's deletions are done: clamp
// the cursor (the lines it sat on may be gone), drop any selection, mark the
// buffer dirty and its styles stale, and refresh the region cache so the
// gutter and the tint agree with the new buffer on the very next paint.
func (t *Tab) afterResolve() {
	t.Cursor = t.Buffer.Clamp(t.Cursor)
	t.Anchor = t.Cursor
	t.Dirty = true
	t.StyleStale = true
	t.cursorMoved = true
	t.RescanConflicts()
}
