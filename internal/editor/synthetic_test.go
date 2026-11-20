// =============================================================================
// File: internal/editor/synthetic_test.go
// Author: Vonzelle Brown
// Created: 2026-11-20
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package editor

import "testing"

// TestSynthetic_TabIsGeneratedAndKeyedByLabel pins that a synthetic tab carries
// its content and label with no path, and that the highlighter is keyed by the
// label rather than a (nonexistent) path.
func TestSynthetic_TabIsGeneratedAndKeyedByLabel(t *testing.T) {
	tab := NewSyntheticTab("changes.diff", "a\nb")
	if !tab.Synthetic || tab.Path != "" {
		t.Fatalf("synthetic=%v path=%q, want synthetic with no path", tab.Synthetic, tab.Path)
	}
	if tab.Buffer.LineCount() != 2 {
		t.Errorf("lines = %d, want 2", tab.Buffer.LineCount())
	}
	if got := tab.HighlightKey(); got != "changes.diff" {
		t.Errorf("HighlightKey = %q, want the label", got)
	}
}

// TestSynthetic_BookmarksNeverApply pins that a synthetic tab draws no bookmark
// glyph: it has no path, so the app never feeds it a set.
func TestSynthetic_BookmarksNeverApply(t *testing.T) {
	tab := NewSyntheticTab("x.diff", "a")
	if tab.Bookmarks != nil {
		t.Errorf("fresh synthetic tab has bookmarks: %v", tab.Bookmarks)
	}
}
