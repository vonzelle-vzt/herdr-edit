// =============================================================================
// File: internal/editor/comment.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-05-14
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

package editor

import (
	"path/filepath"
	"strings"

	"github.com/cloudmanic/spice-edit/internal/langconf"
)

// lineCommentByExt maps common source file extensions to their single-line
// comment marker. Block-comment-only languages are intentionally omitted.
var lineCommentByExt = map[string]string{
	".adb":        "--",
	".ads":        "--",
	".bash":       "#",
	".c":          "//",
	".cc":         "//",
	".clj":        ";",
	".cljs":       ";",
	".cmake":      "#",
	".conf":       "#",
	".cpp":        "//",
	".cs":         "//",
	".csh":        "#",
	".cxx":        "//",
	".dart":       "//",
	".el":         ";",
	".elm":        "--",
	".erl":        "%",
	".ex":         "#",
	".exs":        "#",
	".env":        "#",
	".fish":       "#",
	".go":         "//",
	".h":          "//",
	".hpp":        "//",
	".hs":         "--",
	".ini":        ";",
	".java":       "//",
	".jl":         "#",
	".js":         "//",
	".jsx":        "//",
	".kt":         "//",
	".kts":        "//",
	".less":       "//",
	".lua":        "--",
	".mjs":        "//",
	".mk":         "#",
	".mm":         "//",
	".php":        "//",
	".pl":         "#",
	".pm":         "#",
	".ps1":        "#",
	".py":         "#",
	".r":          "#",
	".rb":         "#",
	".rs":         "//",
	".sass":       "//",
	".scala":      "//",
	".scss":       "//",
	".sh":         "#",
	".sql":        "--",
	".swift":      "//",
	".toml":       "#",
	".ts":         "//",
	".tsx":        "//",
	".vim":        "\"",
	".yaml":       "#",
	".yml":        "#",
	".zsh":        "#",
	".dockerfile": "#",
	".gitignore":  "#",
}

// LineCommentPrefix returns the single-line comment marker for path. The
// boolean is false for file types that do not have a safe line-comment syntax.
func LineCommentPrefix(path string) (string, bool) {
	base := strings.ToLower(filepath.Base(path))
	switch base {
	case "dockerfile", "containerfile", "makefile", "gnumakefile", "rakefile", "gemfile", "justfile":
		return "#", true
	case "cmakelists.txt":
		return "#", true
	}
	if prefix, ok := lineCommentByExt[base]; ok {
		return prefix, true
	}
	prefix, ok := lineCommentByExt[strings.ToLower(filepath.Ext(base))]
	return prefix, ok
}

// BlockCommentTokens returns the block-comment delimiters for path, e.g.
// "/*" and "*/" for Go and Rust, `"""` and `"""` for Python.
//
// The data comes from internal/langconf, i.e. from upstream's own language
// configurations, rather than from a second hand-written table beside
// lineCommentByExt above. lineCommentByExt is deliberately left alone: it is
// what `Esc /` has always used and this change must not move that behaviour.
func BlockCommentTokens(path string) (start, end string, ok bool) {
	return langconf.BlockComment(path)
}

// ToggleBlockComment wraps the selection (or the cursor line, when there is
// no selection) in the language's block-comment delimiters, or unwraps it
// when it is already wrapped. It returns ok=false for a file type with no
// block-comment form, which the caller should report the same way it reports
// an unsupported line comment.
//
// The wrap is EXACT — no padding spaces — so wrapping and unwrapping is an
// identity round trip. A "/* text */" form would have to guess whether to eat
// the spaces again on the way out, and would stop recognising blocks written
// by anything else.
//
// 🔴 NOT REACHABLE FROM THE UI YET. Every caller today is a _test.go file, so
// by this fork's own rule (CLAUDE.md, "grep for a call site before believing
// it works") block commenting is NOT a shipped feature — do not advertise it
// in README.md or FORK.md until it has a menu entry. Wiring it needs a `≡`
// menu item and a key binding in internal/app/app.go, next to where
// ToggleLineComment is dispatched for `Esc /`.
func (t *Tab) ToggleBlockComment() (changed bool, ok bool) {
	if t == nil || t.IsImage() || t.Buffer == nil {
		return false, false
	}
	open, closer, ok := BlockCommentTokens(t.Path)
	if !ok {
		return false, false
	}
	start, end := t.blockCommentRange()
	if start == end {
		return false, true
	}
	text := t.Buffer.Substring(start, end)
	if isBlockCommented(text, open, closer) {
		t.unwrapBlockComment(start, end, open, closer)
		return true, true
	}
	t.wrapBlockComment(start, end, open, closer)
	return true, true
}

// blockCommentRange returns the range to wrap: the selection when there is
// one, otherwise the whole cursor line. An all-whitespace cursor line yields
// an empty range, which ToggleBlockComment treats as a no-op — commenting
// nothing would leave a bare `/**/` floating in the file.
func (t *Tab) blockCommentRange() (Position, Position) {
	if t.HasSelection() {
		start, end := PosOrdered(t.Anchor, t.Cursor)
		return t.Buffer.Clamp(start), t.Buffer.Clamp(end)
	}
	line := t.Buffer.Clamp(t.Cursor).Line
	if strings.TrimSpace(t.Buffer.Lines[line]) == "" {
		p := Position{Line: line, Col: 0}
		return p, p
	}
	return Position{Line: line, Col: 0},
		Position{Line: line, Col: len(t.Buffer.LineRunes(line))}
}

// isBlockCommented reports whether text is already a single block comment.
//
// The length guard is what makes Python safe: its open and close delimiters
// are both `"""`, so a bare `"""` satisfies both HasPrefix and HasSuffix while
// being one delimiter rather than a wrapped block. Without the guard,
// toggling it would delete six characters from a three-character selection.
func isBlockCommented(text, open, closer string) bool {
	if len(text) < len(open)+len(closer) {
		return false
	}
	return strings.HasPrefix(text, open) && strings.HasSuffix(text, closer)
}

// wrapBlockComment inserts the delimiters around start..end and leaves the
// original text selected between them, matching how surroundSelection behaves
// for brackets.
func (t *Tab) wrapBlockComment(start, end Position, open, closer string) {
	t.pushUndo(undoGroupStructural)
	// Closer first: it lands at end, a position the opener insert (at or
	// before start) cannot invalidate. The other order would need end
	// shifted by hand whenever both endpoints share a line.
	t.bufInsert(end, closer)
	afterOpen := t.bufInsert(start, open)
	newEnd := end
	if start.Line == end.Line {
		newEnd.Col += len([]rune(open))
	}
	t.Anchor = afterOpen
	t.Cursor = newEnd
	t.finishBlockComment()
}

// unwrapBlockComment removes the two delimiters from a range already known to
// be wrapped.
//
// It deletes the delimiters in place rather than replacing the whole range
// with its inner text. Both deletions stay within a single line, so no line
// entry disappears and Tab.Marks needs no renumbering — a
// delete-the-range-and-reinsert version would drop every breakpoint inside a
// multi-line block comment the moment the user un-commented it.
func (t *Tab) unwrapBlockComment(start, end Position, open, closer string) {
	openRunes := len([]rune(open))
	closeRunes := len([]rune(closer))

	t.pushUndo(undoGroupStructural)
	// Later delimiter first, so start's coordinates stay valid.
	t.bufDelete(Position{Line: end.Line, Col: end.Col - closeRunes}, end)
	t.bufDelete(start, Position{Line: start.Line, Col: start.Col + openRunes})

	newEnd := Position{Line: end.Line, Col: end.Col - closeRunes}
	if start.Line == end.Line {
		newEnd.Col -= openRunes
	}
	t.Anchor = start
	t.Cursor = newEnd
	t.finishBlockComment()
}

// finishBlockComment applies the bookkeeping both block-comment paths share:
// clamp the selection back inside the buffer and mark the tab dirty, its
// styles stale and its cursor moved.
func (t *Tab) finishBlockComment() {
	t.Anchor = t.Buffer.Clamp(t.Anchor)
	t.Cursor = t.Buffer.Clamp(t.Cursor)
	t.Dirty = true
	t.StyleStale = true
	t.cursorMoved = true
}

// ToggleLineComment comments or uncomments the selected lines. It returns
// ok=false when the active file type has no known line-comment marker.
func (t *Tab) ToggleLineComment() (changed bool, ok bool) {
	if t == nil || t.IsImage() || t.Buffer == nil {
		return false, false
	}
	prefix, ok := LineCommentPrefix(t.Path)
	if !ok {
		return false, false
	}
	start, end := t.commentLineRange()
	if !hasNonBlankLine(t.Buffer.Lines, start, end) {
		return false, true
	}
	uncomment := t.linesAreCommented(start, end, prefix)

	t.pushUndo(undoGroupStructural)
	for i := start; i <= end; i++ {
		line := t.Buffer.Lines[i]
		if strings.TrimSpace(line) == "" {
			continue
		}
		if uncomment {
			t.Buffer.Lines[i] = uncommentLine(line, prefix)
			continue
		}
		t.Buffer.Lines[i] = commentLine(line, prefix)
	}
	t.Cursor = t.Buffer.Clamp(t.Cursor)
	t.Anchor = t.Buffer.Clamp(t.Anchor)
	t.Dirty = true
	t.StyleStale = true
	t.cursorMoved = true
	return true, true
}

// commentLineRange returns the inclusive line range touched by the current
// selection, or the cursor line when there is no selection.
func (t *Tab) commentLineRange() (int, int) {
	if !t.HasSelection() {
		line := t.Buffer.Clamp(t.Cursor).Line
		return line, line
	}
	start, end := PosOrdered(t.Anchor, t.Cursor)
	start = t.Buffer.Clamp(start)
	end = t.Buffer.Clamp(end)
	if end.Col == 0 && end.Line > start.Line {
		end.Line--
	}
	return start.Line, end.Line
}

// linesAreCommented reports whether every non-blank line in the range already
// starts with prefix, either at column zero or after indentation.
func (t *Tab) linesAreCommented(start, end int, prefix string) bool {
	for i := start; i <= end; i++ {
		line := t.Buffer.Lines[i]
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !lineHasCommentPrefix(line, prefix) {
			return false
		}
	}
	return true
}

// hasNonBlankLine reports whether any line in the inclusive range has content.
func hasNonBlankLine(lines []string, start, end int) bool {
	for i := start; i <= end; i++ {
		if strings.TrimSpace(lines[i]) != "" {
			return true
		}
	}
	return false
}

// commentLine inserts prefix at column zero, leaving the line's existing
// indentation untouched after the marker.
func commentLine(line, prefix string) string {
	return prefix + " " + line
}

// uncommentLine removes prefix, plus one following space if present, from
// column zero or from after indentation for lines toggled by older builds.
func uncommentLine(line, prefix string) string {
	if strings.HasPrefix(line, prefix) {
		rest := strings.TrimPrefix(line, prefix)
		rest = strings.TrimPrefix(rest, " ")
		return rest
	}
	indent, rest := splitIndent(line)
	rest = strings.TrimPrefix(rest, prefix)
	rest = strings.TrimPrefix(rest, " ")
	return indent + rest
}

// lineHasCommentPrefix reports whether line starts with prefix at column zero
// or after indentation.
func lineHasCommentPrefix(line, prefix string) bool {
	if strings.HasPrefix(line, prefix) {
		return true
	}
	_, rest := splitIndent(line)
	return strings.HasPrefix(rest, prefix)
}

// splitIndent separates leading horizontal whitespace from the rest of a line.
func splitIndent(line string) (string, string) {
	i := 0
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	return line[:i], line[i:]
}
