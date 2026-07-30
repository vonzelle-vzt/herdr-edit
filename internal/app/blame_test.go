// =============================================================================
// File: internal/app/blame_test.go
// Author: Vonzelle Brown
// Created: 2026-07-30
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package app

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestFormatBlamePorcelain_Commit renders a normal commit: author, a coarse
// relative age, and the subject.
func TestFormatBlamePorcelain_Commit(t *testing.T) {
	ts := time.Now().Add(-72 * time.Hour).Unix()
	out := fmt.Sprintf("abc123 1 1 1\nauthor Ada Lovelace\nauthor-time %d\nsummary Add the engine\n", ts)
	got := formatBlamePorcelain(out)
	for _, want := range []string{"Ada Lovelace", "Add the engine", "days ago"} {
		if !strings.Contains(got, want) {
			t.Errorf("blame line %q is missing %q", got, want)
		}
	}
}

// TestFormatBlamePorcelain_Uncommitted is the case a user hits constantly while
// editing. git reports the author as the literal "Not Committed Yet", whose
// commit fields are meaningless — echoing them would render a line dated 1970.
func TestFormatBlamePorcelain_Uncommitted(t *testing.T) {
	out := "0000000000000000000000000000000000000000 1 1 1\nauthor Not Committed Yet\nauthor-time 0\nsummary Version of x\n"
	got := formatBlamePorcelain(out)
	if !strings.Contains(got, "uncommitted") {
		t.Fatalf("uncommitted line rendered as %q", got)
	}
	if strings.Contains(got, "1970") || strings.Contains(got, "years ago") {
		t.Fatalf("uncommitted line leaked a meaningless timestamp: %q", got)
	}
}

// TestFormatBlamePorcelain_Degrades pins that unusable input yields an empty
// string rather than a half-rendered annotation. Blame is an ornament; a repo
// without history must show nothing, not an error the user cannot act on.
func TestFormatBlamePorcelain_Degrades(t *testing.T) {
	for _, in := range []string{"", "   ", "garbage with no fields", "abc123 1 1 1\n"} {
		if got := formatBlamePorcelain(in); got != "" {
			t.Errorf("formatBlamePorcelain(%q) = %q, want empty", in, got)
		}
	}
}

// TestFormatBlamePorcelain_NoSummary falls back to the author alone rather than
// rendering a dangling separator.
func TestFormatBlamePorcelain_NoSummary(t *testing.T) {
	got := formatBlamePorcelain("abc 1 1 1\nauthor Grace Hopper\n")
	if got != "Grace Hopper" {
		t.Fatalf("got %q, want the bare author", got)
	}
}

// TestHumanizeAge covers the buckets a blame annotation actually uses.
func TestHumanizeAge(t *testing.T) {
	cases := []struct {
		ago  time.Duration
		want string
	}{
		{30 * time.Second, "just now"},
		{20 * time.Minute, "minutes ago"},
		{5 * time.Hour, "hours ago"},
		{3 * 24 * time.Hour, "days ago"},
		{3 * 7 * 24 * time.Hour, "weeks ago"},
		{100 * 24 * time.Hour, "months ago"},
		{800 * 24 * time.Hour, "years ago"},
	}
	for _, c := range cases {
		got := humanizeAge(time.Now().Add(-c.ago))
		if !strings.Contains(got, c.want) {
			t.Errorf("humanizeAge(-%v) = %q, want it to contain %q", c.ago, got, c.want)
		}
	}
}

// TestBlame_ToggleAndReachable guards the reachability failure this fork has
// repeated three times, and the toggle's label flipping in place.
func TestBlame_ToggleAndReachable(t *testing.T) {
	if leaderActionFor('b') == nil {
		t.Fatal("Esc b is not bound — inline blame is unreachable")
	}
	a := newTestApp(t, t.TempDir())
	a.blameEnabled = true
	if got := a.inlineBlameLabel(); got != "Hide inline blame" {
		t.Errorf("label with blame on = %q", got)
	}
	a.menuToggleInlineBlame()
	if a.blameEnabled {
		t.Error("the toggle did not turn blame off")
	}
	if got := a.inlineBlameLabel(); got != "Show inline blame" {
		t.Errorf("label with blame off = %q", got)
	}
}

// TestBlame_NoLookupWhenDirty pins that a modified buffer is never blamed. Line
// numbers in a dirty buffer no longer match what git knows, so an answer would
// be attributed to the wrong line — confidently and invisibly.
func TestBlame_NoLookupWhenDirty(t *testing.T) {
	a := seedNavApp(t, "one\ntwo\nthree\n")
	tab := a.activeTabPtr()
	tab.Dirty = true
	a.blameEnabled = true
	a.maybeRequestBlame()
	if len(a.blameInflight) != 0 {
		t.Fatal("a dirty buffer should not trigger a blame lookup")
	}
}

// TestBlame_InvalidateOnSave checks the cache is dropped for the saved path
// only, since a save can shift every line after an edit.
func TestBlame_InvalidateOnSave(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.blameCache = map[blameKey]string{
		{path: "/a.go", line: 1}: "x",
		{path: "/a.go", line: 9}: "y",
		{path: "/b.go", line: 1}: "z",
	}
	a.invalidateBlame("/a.go")
	if len(a.blameCache) != 1 {
		t.Fatalf("cache has %d entries, want only the untouched file's", len(a.blameCache))
	}
	if a.blameCache[blameKey{path: "/b.go", line: 1}] != "z" {
		t.Error("invalidating one path dropped another path's entry")
	}
}
