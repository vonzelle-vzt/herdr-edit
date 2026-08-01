// =============================================================================
// File: internal/editor/marks.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// marks.go gives every Tab a set of gutter marks — breakpoints today,
// logpoints and an adapter's "stopped here" line reserved for later stages —
// keyed by buffer line and kept correct as the buffer edits out from under
// them.
//
// This stage has NO debug adapter. A Mark is a persistent, edit-tracking
// annotation the user places by hand; MarkBreakpoint exists so it can be
// exported as `break file:line` and pasted into dlv/pdb/gdb, and
// MarkStopped/Verified exist only as fields a future adapter stage will
// start writing — nothing in this package ever sets them.
//
// The line-tracking is the actual point of this file. Marks are keyed by a
// plain buffer line number, and any edit that inserts or removes whole lines
// has to renumber every mark below it, or a breakpoint silently drifts onto
// the wrong line the first time the user edits above it. bufInsert /
// bufDelete (tab.go) are the only two places lines actually move, so they
// are the only two callers of shiftMarks / dropMarksIn —
// TestAllBufferMutationsGoThroughMarkWrappers pins that down.
package editor

import (
	"sort"

	"github.com/gdamore/tcell/v2"

	"github.com/cloudmanic/spice-edit/internal/theme"
)

// MarkKind identifies what a gutter mark represents.
type MarkKind int

const (
	MarkNone MarkKind = iota
	MarkBreakpoint
	MarkLogpoint
	MarkStopped
)

// Mark is one gutter annotation. Condition, LogMessage, Verified and
// VerifiedLine are carried for a later debug-adapter stage: this stage never
// reads Condition/LogMessage and never sets Verified true, but the fields
// exist now so that stage isn't a breaking change to this type.
type Mark struct {
	Kind    MarkKind
	Enabled bool
	// Condition and LogMessage reach the adapter as SourceBreakpoint.condition /
	// .logMessage. 🔴 internal/app gates BOTH on the adapter's capabilities and
	// refuses loudly when they are missing: an adapter without
	// supportsConditionalBreakpoints drops the field SILENTLY, and the breakpoint
	// then fires every single time, from which the only reasonable conclusion is
	// that conditions are broken.
	Condition  string
	LogMessage string

	// Verified and VerifiedLine are the adapter's confirmation that a breakpoint
	// actually landed on a real, executable line. The adapter may snap it FORWARD
	// to the next executable statement, so VerifiedLine is where it will really
	// stop and is what the gutter draws -- otherwise the stopped arrow lands where
	// no breakpoint dot is and the debugger looks like it is lying.
	Verified     bool
	VerifiedLine int
}

// SetMark places or replaces the mark at line (0-based). The map is
// allocated lazily so a freshly-created Tab doesn't need special-case init.
func (t *Tab) SetMark(line int, m Mark) {
	if t.Marks == nil {
		t.Marks = make(map[int]Mark)
	}
	t.Marks[line] = m
}

// ClearMark removes any mark at line. A no-op if none is set.
func (t *Tab) ClearMark(line int) {
	delete(t.Marks, line)
}

// MarkAt returns the mark at line, if any.
func (t *Tab) MarkAt(line int) (Mark, bool) {
	m, ok := t.Marks[line]
	return m, ok
}

// MarkLines returns every marked line, sorted ascending — the order the
// gutter, the export list, and any future "next breakpoint" navigation all
// want.
func (t *Tab) MarkLines() []int {
	if len(t.Marks) == 0 {
		return nil
	}
	lines := make([]int, 0, len(t.Marks))
	for l := range t.Marks {
		lines = append(lines, l)
	}
	sort.Ints(lines)
	return lines
}

// shiftMarks renumbers every mark at or after fromLine by delta. Called by
// bufInsert / bufDelete after a buffer edit inserts or removes whole lines,
// so a breakpoint below the edit keeps pointing at the same source line
// instead of drifting onto whatever text now occupies its old line number.
func (t *Tab) shiftMarks(fromLine, delta int) {
	if delta == 0 || len(t.Marks) == 0 {
		return
	}
	// Rebuild rather than mutate keys in place: renumbering upward while
	// iterating the same map could revisit an entry we just moved.
	shifted := make(map[int]Mark, len(t.Marks))
	for line, m := range t.Marks {
		if line >= fromLine {
			line += delta
		}
		shifted[line] = m
	}
	t.Marks = shifted
}

// dropMarksIn removes every mark on a line in [lo, hi) — the half-open range
// of buffer lines a delete is about to erase entirely. Called by bufDelete
// BEFORE shiftMarks, so a mark inside the deleted span dies instead of being
// renumbered onto whatever survives the edit.
func (t *Tab) dropMarksIn(lo, hi int) {
	for line := range t.Marks {
		if line >= lo && line < hi {
			delete(t.Marks, line)
		}
	}
}

// copyMarks deep-copies a marks map so a caller holding onto one (chiefly
// undo.go's snapshots) never aliases the live map — otherwise the next edit's
// shiftMarks/dropMarksIn would silently rewrite history, the same hazard
// applySnapshot's line-copying already guards against for Buffer.Lines.
func copyMarks(m map[int]Mark) map[int]Mark {
	if len(m) == 0 {
		return nil
	}
	out := make(map[int]Mark, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// markGlyph returns the gutter glyph for m.Kind. A disabled breakpoint stays
// visible as a hollow dot rather than disappearing — the same way VS Code
// shows you it's there but inert, instead of making it look cleared.
func markGlyph(m Mark) rune {
	switch m.Kind {
	case MarkBreakpoint:
		if !m.Enabled {
			return '○'
		}
		return '●'
	case MarkLogpoint:
		return '◆'
	case MarkStopped:
		return '▶'
	default:
		return ' '
	}
}

// markColor returns the gutter color for m.Kind.
func markColor(th theme.Theme, m Mark) tcell.Color {
	switch m.Kind {
	case MarkLogpoint:
		return th.Accent
	case MarkStopped:
		return th.GitAdded
	default: // MarkBreakpoint
		if !m.Enabled {
			return th.Muted
		}
		return th.Error
	}
}

// gutterMarker resolves what belongs in the leftmost gutter cell for a
// buffer line: a mark's glyph when one is set, a conflict marker line's glyph
// next, the git bar's glyph otherwise, or nothing at all.
//
// The priority is mark > conflict > git bar, and the first two are that way
// round deliberately. A breakpoint is something the user DELIBERATELY placed
// and expects to see; a conflict is already shouting from the body of the file
// — the `<<<<<<<` text is literally on screen and the region is tinted — so
// losing its gutter cell on one line out of four costs nothing, while losing a
// breakpoint dot reads as the breakpoint having been cleared.
//
// Shared by Render (tab.go) and renderWrapped (wrap.go) on purpose. The two
// render paths already diverge in their innermost loop — wrap.go's own doc
// comment explains why — and a priority rule resolved twice is exactly the
// kind of drift this fork has shipped before: patch one copy, and the other
// silently falls back to git-bar-only behaviour with no error anywhere. It is
// also why a wrapped tab gets the conflict glyph for free, on each region's
// first segment row, with no second painter to keep in step.
func (t *Tab) gutterMarker(th theme.Theme, lineIdx int) (r rune, color tcell.Color, ok bool) {
	if m, mok := t.Marks[lineIdx]; mok && m.Kind != MarkNone {
		return markGlyph(m), markColor(th, m), true
	}
	if t.conflictMarkerLine(lineIdx) {
		// Error, matching filetree's gitChangeColor for GitChangeConflict: an
		// unresolved conflict BLOCKS work and must not read as an ordinary edit.
		return conflictGlyph, th.Error, true
	}
	if marker, gok := t.GitLines[lineIdx]; gok && marker != GitLineNone {
		return gitLineMarkerRune(marker), gitLineMarkerColor(th, marker), true
	}
	return 0, 0, false
}

// clampMarks drops marks that fall outside the buffer and clears the adapter's
// verification state on the ones that remain. Called after a Reload, where the
// content changed underneath us without going through bufInsert/bufDelete:
// mark tracking observes OUR edits, so an external rewrite is invisible to it
// and can leave a mark past the end of a file that shrank. Verified/VerifiedLine
// describe where an adapter actually bound the breakpoint in the OLD content,
// so they are meaningless against new bytes and must not be carried forward.
func (t *Tab) clampMarks() {
	if len(t.Marks) == 0 {
		return
	}
	n := t.Buffer.LineCount()
	for line, m := range t.Marks {
		if line < 0 || line >= n {
			delete(t.Marks, line)
			continue
		}
		m.Verified = false
		m.VerifiedLine = -1
		t.Marks[line] = m
	}
}
