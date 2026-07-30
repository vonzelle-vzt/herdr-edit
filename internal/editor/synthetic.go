// =============================================================================
// File: internal/editor/synthetic.go
// Author: Vonzelle Brown
// Created: 2026-07-30
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// synthetic.go adds tabs whose content is GENERATED rather than read from disk
// — currently the diff view.
//
// The alternative was writing the diff to a temp file and opening that, which
// is what several editors do. It is worse in the way that matters here: the tab
// then has a real Path, so Save() would happily write the user's edits into a
// file in /tmp, and the tree would show a project file that does not exist.
// A synthetic tab carries a Label instead of a Path, refuses to save, and
// highlights by that label's extension.

package editor

// NewSyntheticTab builds a read-only tab from generated content. label is what
// the tab bar shows AND what picks the syntax lexer, so ending it in ".diff"
// gets diff highlighting for free.
func NewSyntheticTab(label, content string) *Tab {
	t := &Tab{
		Buffer:     NewBuffer(content),
		Synthetic:  true,
		Label:      label,
		FindIndex:  -1,
		IndentUnit: "\t",
	}
	t.StyleStale = true
	return t
}

// HighlightKey is the name the syntax highlighter should match a lexer against:
// a synthetic tab's label, otherwise the file's path.
func (t *Tab) HighlightKey() string {
	if t.Synthetic {
		return t.Label
	}
	return t.Path
}
