// =============================================================================
// File: internal/editor/conflict_test.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// Tests for conflict.go. Every fixture here is TAB-INDENTED on purpose: every
// geometry and column bug this repo has shipped hid behind a fixture where a
// rune index and a screen column happened to coincide.
//
// Note that this file itself contains complete, well-formed conflict marker
// sequences — which is precisely why Tab.GitUnmerged exists. Opening this file
// in a clean checkout must show nothing;
// TestConflictsStayEmptyWhenGitSaysTheFileIsClean (internal/app) is where that
// claim is pinned against a real repo.
package editor

import (
	"strings"
	"testing"
)

// conflictFixtureLines is a three-region conflicted file: two under git's
// default `merge` style and one under `diff3`, so a resolver that assumes a
// fixed four-marker layout fails here rather than in someone's repository.
// Lines 27+ are filler, so a mark can sit well below the last region.
func conflictFixtureLines() []string {
	lines := []string{
		"package main",          // 0
		"",                      // 1
		"func one() int {",      // 2
		"<<<<<<< HEAD",          // 3
		"\treturn 1 // OURS",    // 4
		"=======",               // 5
		"\treturn 11 // THEIRS", // 6
		">>>>>>> feature",       // 7
		"}",                     // 8
		"",                      // 9
		"func two() int {",      // 10
		"<<<<<<< HEAD",          // 11
		"\treturn 2 // OURS",    // 12
		"||||||| 47eacf0",       // 13
		"\treturn 0 // BASE",    // 14
		"=======",               // 15
		"\treturn 22 // THEIRS", // 16
		">>>>>>> feature",       // 17
		"}",                     // 18
		"",                      // 19
		"func three() int {",    // 20
		"<<<<<<< HEAD",          // 21
		"\treturn 3 // OURS",    // 22
		"=======",               // 23
		"\treturn 33 // THEIRS", // 24
		">>>>>>> feature",       // 25
		"}",                     // 26
	}
	for i := 0; i < 23; i++ {
		lines = append(lines, "// filler "+string(rune('a'+i)))
	}
	return lines
}

// conflictFixtureTab builds a Tab over conflictFixtureLines with git's verdict
// already granted, since every resolve path is gated on it.
func conflictFixtureTab() *Tab {
	t := &Tab{Path: "main.go", Buffer: NewBuffer(strings.Join(conflictFixtureLines(), "\n"))}
	t.GitUnmerged = true
	t.RescanConflicts()
	return t
}

// wantOursLines is the byte-for-byte expected buffer after taking ours on
// every region — written out by hand rather than derived, so it cannot agree
// with the implementation by construction.
func wantOursLines() []string {
	lines := []string{
		"package main",
		"",
		"func one() int {",
		"\treturn 1 // OURS",
		"}",
		"",
		"func two() int {",
		"\treturn 2 // OURS",
		"}",
		"",
		"func three() int {",
		"\treturn 3 // OURS",
		"}",
	}
	for i := 0; i < 23; i++ {
		lines = append(lines, "// filler "+string(rune('a'+i)))
	}
	return lines
}

// TestScanConflictsReadsBothConflictStyles pins the scanner against the two
// layouts git actually writes. diff3 is not exotic — it is one `git config`
// away and many people turn it on — and a scanner that only understands the
// default style reports the ||||||| section as content someone chose.
func TestScanConflictsReadsBothConflictStyles(t *testing.T) {
	got := ScanConflicts(conflictFixtureLines())
	want := []ConflictRegion{
		{Start: 3, Base: -1, Sep: 5, End: 7, OursLabel: "HEAD", TheirsLabel: "feature"},
		{Start: 11, Base: 13, Sep: 15, End: 17, OursLabel: "HEAD", TheirsLabel: "feature"},
		{Start: 21, Base: -1, Sep: 23, End: 25, OursLabel: "HEAD", TheirsLabel: "feature"},
	}
	if len(got) != len(want) {
		t.Fatalf("ScanConflicts found %d regions, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("region %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestScanConflictsRegionsAscendAndNeverOverlap is the invariant every caller
// leans on: the gutter breaks out of its loop on the first region past the
// line it is asking about, and resolveRegion walks regions backwards assuming
// an earlier one cannot contain a later one.
func TestScanConflictsRegionsAscendAndNeverOverlap(t *testing.T) {
	got := ScanConflicts(conflictFixtureLines())
	for i, c := range got {
		if !(c.Start < c.Sep && c.Sep < c.End) {
			t.Errorf("region %d is not ordered: %+v", i, c)
		}
		if c.Base >= 0 && !(c.Start < c.Base && c.Base < c.Sep) {
			t.Errorf("region %d's base marker is out of place: %+v", i, c)
		}
		if i > 0 && got[i-1].End >= c.Start {
			t.Errorf("region %d overlaps region %d: %+v vs %+v", i-1, i, got[i-1], c)
		}
	}
}

// TestNestedOpenerDiscardsTheOuterRegion pins the deliberate refusal. With two
// openers before any separator there is no way to know which ======= closes
// which, and a resolver that guesses deletes the wrong half of a file. Losing
// the outer region costs one manual edit; guessing costs the user their work.
func TestNestedOpenerDiscardsTheOuterRegion(t *testing.T) {
	lines := []string{
		"a",
		"<<<<<<< OUTER",
		"\touter ours",
		"<<<<<<< INNER",
		"\tinner ours",
		"=======",
		"\tinner theirs",
		">>>>>>> inner-branch",
		"z",
	}
	got := ScanConflicts(lines)
	if len(got) != 1 {
		t.Fatalf("got %d regions, want exactly the inner one: %+v", len(got), got)
	}
	want := ConflictRegion{Start: 3, Base: -1, Sep: 5, End: 7, OursLabel: "INNER", TheirsLabel: "inner-branch"}
	if got[0] != want {
		t.Errorf("kept %+v, want the INNER region %+v", got[0], want)
	}
}

// TestMalformedSequencesEmitNothing covers the three shapes that must produce
// no region at all. Emitting one for any of them would let a resolution delete
// lines that were never a conflict.
func TestMalformedSequencesEmitNothing(t *testing.T) {
	cases := map[string][]string{
		"no separator":  {"<<<<<<< HEAD", "ours", ">>>>>>> feature"},
		"no terminator": {"<<<<<<< HEAD", "ours", "=======", "theirs"},
		"no opener":     {"ours", "=======", "theirs", ">>>>>>> feature"},
		"markers only":  {"=======", "======="},
	}
	for name, lines := range cases {
		if got := ScanConflicts(lines); len(got) != 0 {
			t.Errorf("%s: got %+v, want no regions", name, got)
		}
	}
}

// TestMarkerNeedsSevenRunesAtColumnZero pins both halves of the marker test.
// `conflict-marker-size` is a gitattribute, so a longer run is still a marker
// (>= not ==); and git writes markers flush left, so an indented `=======`
// inside a docstring or a markdown setext heading is not one.
func TestMarkerNeedsSevenRunesAtColumnZero(t *testing.T) {
	// A repo with conflict-marker-size set to 10.
	long := []string{
		"<<<<<<<<<< HEAD",
		"\tours",
		"==========",
		"\ttheirs",
		">>>>>>>>>> feature",
	}
	if got := ScanConflicts(long); len(got) != 1 {
		t.Errorf("a 10-rune marker run was not recognised: %+v", got)
	}

	// Six runes is not a marker, and neither is an indented seven.
	for name, lines := range map[string][]string{
		"six runes": {"<<<<<< HEAD", "\tours", "======", "\ttheirs", ">>>>>> feature"},
		"indented":  {"\t<<<<<<< HEAD", "\tours", "\t=======", "\ttheirs", "\t>>>>>>> feature"},
	} {
		if got := ScanConflicts(lines); len(got) != 0 {
			t.Errorf("%s was treated as a conflict: %+v", name, got)
		}
	}
}

// TestRescanConflictsNeedsGitsVerdict is the buffer-side half of the defence
// this whole feature rests on. The bytes of a marker inside a string literal
// are identical to a real one, so the scanner is only ever allowed to run on a
// path git listed as unmerged.
func TestRescanConflictsNeedsGitsVerdict(t *testing.T) {
	tab := &Tab{Path: "main.go", Buffer: NewBuffer(strings.Join(conflictFixtureLines(), "\n"))}
	tab.RescanConflicts()
	if len(tab.Conflicts) != 0 {
		t.Fatalf("scanned %d regions without git saying the file is unmerged: %+v",
			len(tab.Conflicts), tab.Conflicts)
	}
	// And the bytes really are detectable — this is a gate, not a broken scanner.
	if n := len(ScanConflicts(tab.Buffer.Lines)); n != 3 {
		t.Fatalf("the same bytes scan to %d regions directly, want 3 — the fixture is wrong, not the gate", n)
	}
	tab.GitUnmerged = true
	tab.RescanConflicts()
	if len(tab.Conflicts) != 3 {
		t.Errorf("with git's verdict granted, got %d regions, want 3", len(tab.Conflicts))
	}
}

// TestConflictAtCoversMarkersAndBodyButNotTheGaps pins what "the conflict the
// cursor is in" means: the whole region including both marker lines, and
// nothing between regions.
func TestConflictAtCoversMarkersAndBodyButNotTheGaps(t *testing.T) {
	tab := conflictFixtureTab()
	for _, line := range []int{3, 4, 5, 6, 7} {
		c, idx, ok := tab.ConflictAt(line)
		if !ok || idx != 0 || c.Start != 3 {
			t.Errorf("line %d: got (%+v, %d, %v), want the first region", line, c, idx, ok)
		}
	}
	for _, line := range []int{0, 2, 8, 9, 10, 18, 26, 40} {
		if _, _, ok := tab.ConflictAt(line); ok {
			t.Errorf("line %d is outside every region but ConflictAt claimed one", line)
		}
	}
}

// TestTakeOursIsExactAndUndoesInOneStep is the load-bearing oracle for the
// whole resolution path, and its LAST assertion is the one that matters.
//
// 🔴 Resolution is pure deletion, so the buffer must come out byte-for-byte
// identical to the kept content — no re-indent, no whitespace normalisation,
// nothing reconstructed. And every deleted range must go through bufDelete: a
// resolver built on Buffer.DeleteRange passes assertions 1 through 3 exactly
// as this one does, and silently drifts every breakpoint below the edit,
// because Marks looks perfectly fine right up until the user goes to run the
// program. That is why the breakpoint sits twenty lines BELOW the last region
// — far enough that it can only be right by tracking, never by luck — and why
// it is asserted by TEXT rather than by line number.
func TestTakeOursIsExactAndUndoesInOneStep(t *testing.T) {
	tab := conflictFixtureTab()
	before := tab.Buffer.String()

	const markLine = 45 // twenty lines below the last region's >>>>>>> (line 25)
	markText := tab.LineText(markLine)
	if markText == "" || strings.Contains(markText, "<") {
		t.Fatalf("fixture drifted: line %d is %q, expected filler text", markLine, markText)
	}
	tab.SetMark(markLine, Mark{Kind: MarkBreakpoint, Enabled: true})

	// (1) It resolves every region in the file.
	if n := tab.ResolveAllConflicts(ConflictOurs); n != 3 {
		t.Fatalf("ResolveAllConflicts took %d regions, want 3", n)
	}

	// (2) Byte for byte, exactly the kept content.
	want := strings.Join(wantOursLines(), "\n")
	if got := tab.Buffer.String(); got != want {
		t.Fatalf("buffer after take-ours is not exact.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if len(tab.Conflicts) != 0 {
		t.Errorf("%d regions still cached after resolving them all", len(tab.Conflicts))
	}

	// Capture where the breakpoint ended up BEFORE undoing — undo restores the
	// mark map too, so asserting after it would test the snapshot, not the edit.
	marks := tab.MarkLines()
	if len(marks) != 1 {
		t.Fatalf("got %d marks after resolving, want the one breakpoint: %v", len(marks), marks)
	}
	movedTo, movedText := marks[0], tab.LineText(marks[0])

	// (3) ONE undo restores the file byte for byte. The whole file is a single
	// structural step; N undos to reverse one action would be its own bug.
	tab.Undo()
	if got := tab.Buffer.String(); got != before {
		t.Fatalf("one undo did not restore the buffer.\n--- got ---\n%s\n--- want ---\n%s", got, before)
	}

	// (4) The breakpoint still sits on the line whose TEXT it started on.
	if movedText != markText {
		t.Errorf("breakpoint drifted: it started on %q (line %d) and ended on line %d, which reads %q",
			markText, markLine, movedTo, movedText)
	}
}

// TestTakeTheirsAndTakeBothAreExactDeletions covers the other two choices, and
// in particular that diff3's common-ancestor section is discarded by all of
// them — it is context git printed, never content anyone chose to keep.
func TestTakeTheirsAndTakeBothAreExactDeletions(t *testing.T) {
	theirs := []string{
		"package main", "", "func one() int {", "\treturn 11 // THEIRS", "}", "",
		"func two() int {", "\treturn 22 // THEIRS", "}", "",
		"func three() int {", "\treturn 33 // THEIRS", "}",
	}
	both := []string{
		"package main", "", "func one() int {", "\treturn 1 // OURS", "\treturn 11 // THEIRS", "}", "",
		"func two() int {", "\treturn 2 // OURS", "\treturn 22 // THEIRS", "}", "",
		"func three() int {", "\treturn 3 // OURS", "\treturn 33 // THEIRS", "}",
	}
	filler := conflictFixtureLines()[27:]

	for _, tc := range []struct {
		name   string
		choice ConflictChoice
		head   []string
	}{
		{"theirs", ConflictTheirs, theirs},
		{"both", ConflictBoth, both},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tab := conflictFixtureTab()
			if n := tab.ResolveAllConflicts(tc.choice); n != 3 {
				t.Fatalf("resolved %d regions, want 3", n)
			}
			want := strings.Join(append(append([]string{}, tc.head...), filler...), "\n")
			if got := tab.Buffer.String(); got != want {
				t.Fatalf("not exact.\n--- got ---\n%s\n--- want ---\n%s", got, want)
			}
			if strings.Contains(tab.Buffer.String(), "BASE") {
				t.Error("the diff3 common-ancestor section survived; it is context, not a choice")
			}
		})
	}
}

// TestResolveRefusesWhenTheRegionWentStale pins the cache discipline.
// Tab.Conflicts is a RENDER cache: the buffer can be edited between the paint
// that filled it and the keystroke that acts on it. A resolver that trusts a
// stale region deletes lines that are no longer the ones it was pointed at —
// so it rescans, notices the disagreement, and refuses.
func TestResolveRefusesWhenTheRegionWentStale(t *testing.T) {
	tab := conflictFixtureTab()

	// The user deletes a line ABOVE the first region, by hand, and the cache
	// is not refreshed — exactly what happens between two redraws.
	tab.bufDelete(Position{Line: 1, Col: 0}, Position{Line: 2, Col: 0})
	before := tab.Buffer.String()
	stale := tab.Conflicts[0]

	if tab.ResolveConflict(0, ConflictOurs) {
		t.Fatal("resolved against a stale region instead of refusing")
	}
	if got := tab.Buffer.String(); got != before {
		t.Fatalf("a refused resolution still edited the buffer.\n--- got ---\n%s", got)
	}
	// The refusal must be about staleness, not about there being no conflict:
	// the region is still there, one line higher.
	if len(tab.Conflicts) != 3 {
		t.Fatalf("the rescan lost the regions entirely: %+v", tab.Conflicts)
	}
	if tab.Conflicts[0] == stale {
		t.Fatal("fixture drifted: the region did not actually move, so nothing was stale")
	}

	// And with a fresh cache the very same call goes through.
	if !tab.ResolveConflict(0, ConflictOurs) {
		t.Fatal("refused even after the cache was refreshed by the failed attempt")
	}
	if strings.Contains(tab.Buffer.String(), "// THEIRS\n\tre") {
		t.Error("the first region was not the one resolved")
	}
}

// TestResolveAtEndOfFileLeavesNoStrayLine is the tail edge case.
// Buffer.DeleteRange CLAMPS a position past the end back onto the last line,
// so deleting forwards to {LineCount, 0} collapses to nothing and leaves the
// emptied row behind — a blank line appended to the file every time the
// closing >>>>>>> happens to be its last line, which for a conflict at the
// bottom of a file is always.
func TestResolveAtEndOfFileLeavesNoStrayLine(t *testing.T) {
	for _, tc := range []struct {
		name   string
		choice ConflictChoice
		want   []string
	}{
		{"ours", ConflictOurs, []string{"head", "\tours"}},
		{"theirs", ConflictTheirs, []string{"head", "\ttheirs"}},
		{"both", ConflictBoth, []string{"head", "\tours", "\ttheirs"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// No trailing newline, so the >>>>>>> really is the final line.
			src := strings.Join([]string{
				"head", "<<<<<<< HEAD", "\tours", "=======", "\ttheirs", ">>>>>>> feature",
			}, "\n")
			tab := &Tab{Path: "main.go", Buffer: NewBuffer(src)}
			tab.GitUnmerged = true
			tab.RescanConflicts()

			if n := tab.ResolveAllConflicts(tc.choice); n != 1 {
				t.Fatalf("resolved %d regions, want 1", n)
			}
			want := strings.Join(tc.want, "\n")
			if got := tab.Buffer.String(); got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}

// TestResolvingAConflictThatIsTheWholeFileKeepsOneLine covers the degenerate
// buffer: nothing above the <<<<<<< and nothing below the >>>>>>>. Every
// Buffer is required to have at least one line, so the result is a single
// empty line rather than a slice with none.
func TestResolvingAConflictThatIsTheWholeFileKeepsOneLine(t *testing.T) {
	tab := &Tab{Path: "main.go", Buffer: NewBuffer("<<<<<<< HEAD\n=======\n>>>>>>> feature")}
	tab.GitUnmerged = true
	tab.RescanConflicts()

	if n := tab.ResolveAllConflicts(ConflictOurs); n != 1 {
		t.Fatalf("resolved %d regions, want 1", n)
	}
	if got := tab.Buffer.Lines; len(got) != 1 || got[0] != "" {
		t.Fatalf("got %q, want exactly one empty line", got)
	}
}

// TestResolveConflictHandlesOneRegionAtATime pins the single-region entry
// point: resolving the middle conflict must leave its neighbours untouched,
// which is what makes "take ours here, take theirs there" possible at all.
func TestResolveConflictHandlesOneRegionAtATime(t *testing.T) {
	tab := conflictFixtureTab()
	if !tab.ResolveConflict(1, ConflictTheirs) {
		t.Fatal("ResolveConflict refused a fresh region")
	}
	if len(tab.Conflicts) != 2 {
		t.Fatalf("got %d regions left, want 2", len(tab.Conflicts))
	}
	body := tab.Buffer.String()
	if !strings.Contains(body, "\treturn 22 // THEIRS") {
		t.Error("the chosen side of the resolved region is missing")
	}
	if strings.Contains(body, "// BASE") || strings.Contains(body, "\treturn 2 // OURS") {
		t.Error("the discarded sides of the resolved region survived")
	}
	for _, keep := range []string{"\treturn 1 // OURS", "\treturn 11 // THEIRS", "\treturn 3 // OURS", "\treturn 33 // THEIRS"} {
		if !strings.Contains(body, keep) {
			t.Errorf("resolving region 1 disturbed its neighbours: %q is gone", keep)
		}
	}
}

// TestResolveConflictRefusesOutOfRangeAndImageTabs covers the cheap guards.
// An out-of-range index is the shape a stale menu row arrives in, and an image
// tab has no buffer to delete lines from.
func TestResolveConflictRefusesOutOfRangeAndImageTabs(t *testing.T) {
	tab := conflictFixtureTab()
	for _, idx := range []int{-1, 3, 99} {
		if tab.ResolveConflict(idx, ConflictOurs) {
			t.Errorf("ResolveConflict(%d) claimed success", idx)
		}
	}
	img := &Tab{Path: "x.png", Mode: imageMode, Buffer: NewBuffer("<<<<<<< a\n=======\n>>>>>>> b")}
	img.GitUnmerged = true
	img.RescanConflicts()
	if len(img.Conflicts) != 0 {
		t.Error("an image tab was scanned for conflicts")
	}
	if img.ResolveAllConflicts(ConflictOurs) != 0 {
		t.Error("an image tab was resolved")
	}
}

// TestUndoRestoresTheRegionCacheWithTheMarkers pins a defect the resolution
// path introduced into undo. The snapshot carries Lines, Cursor, Anchor and
// Marks — not Conflicts — so undoing a resolution used to put every marker
// back into the buffer while the region cache stayed EMPTY: no gutter glyphs,
// no body tint, and the whole conflict menu group gone, until the next
// ten-second git tick happened to rebuild it. The cache is derived state and
// applySnapshot has to re-derive it.
func TestUndoRestoresTheRegionCacheWithTheMarkers(t *testing.T) {
	tab := conflictFixtureTab()
	if len(tab.Conflicts) != 3 {
		t.Fatalf("fixture has %d regions, want 3", len(tab.Conflicts))
	}
	if n := tab.ResolveAllConflicts(ConflictOurs); n != 3 {
		t.Fatalf("resolved %d regions, want 3", n)
	}
	tab.Undo()

	if !strings.Contains(tab.Buffer.String(), "<<<<<<<") {
		t.Fatal("undo did not restore the markers, so this tests nothing")
	}
	if len(tab.Conflicts) != 3 {
		t.Fatalf("the markers are back in the buffer but the region cache holds %d regions, want 3 — "+
			"the gutter, the tint and the whole conflict menu would be invisible", len(tab.Conflicts))
	}
	// And redo takes them away again, so the cache tracks in both directions.
	tab.Redo()
	if len(tab.Conflicts) != 0 {
		t.Errorf("after redo the cache still holds %d regions", len(tab.Conflicts))
	}
}
