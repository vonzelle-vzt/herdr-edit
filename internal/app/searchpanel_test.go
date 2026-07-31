// =============================================================================
// File: internal/app/searchpanel_test.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/cloudmanic/spice-edit/internal/finder"
	"github.com/cloudmanic/spice-edit/internal/search"
)

// waitForSearchResults blocks until the goroutine started by
// startWorkspaceSearch has posted its result, then returns the event the
// real event loop would have received. Polls rather than blocking on
// PollEvent directly so a genuinely missing event fails the test instead of
// hanging it — the same deadline-loop shape internal/finder's own tests use
// for their background-build goroutine.
func waitForSearchResults(t *testing.T, a *App) *searchResultsEvent {
	t.Helper()
	scr := a.screen.(tcell.SimulationScreen)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if scr.HasPendingEvent() {
			if se, ok := scr.PollEvent().(*searchResultsEvent); ok {
				return se
			}
			continue
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("timed out waiting for searchResultsEvent")
	return nil
}

// seedSearchApp builds an app rooted at a real temp directory (so
// internal/finder has something to index) and rebuilds the finder
// synchronously so a.finder.Paths() is populated before the test runs.
func seedSearchApp(t *testing.T, files map[string]string) *App {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		mustWriteApp(t, dir, rel, content)
	}
	a := newTestApp(t, dir)
	a.finder = newReadyFinder(t, dir)
	return a
}

// newReadyFinder builds and synchronously rebuilds a finder.Finder rooted at
// dir, so the caller's very first Paths() call already sees the indexed
// files rather than racing the background build goroutine.
func newReadyFinder(t *testing.T, dir string) *finder.Finder {
	t.Helper()
	f := finder.New(dir)
	done := make(chan struct{})
	f.Rebuild(func() { close(done) })
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("finder rebuild never completed")
	}
	return f
}

// TestMenuSearchInFiles_UnavailableWithoutFinder pins the single-file-mode
// guard: with no finder wired in, the action must flash and never open the
// prompt (which would otherwise pop a search box with nothing to search).
func TestMenuSearchInFiles_UnavailableWithoutFinder(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.finder = nil
	a.menuSearchInFiles()
	if a.promptOpen {
		t.Fatal("prompt should not open when there is no finder")
	}
	if !strings.Contains(a.statusMsg, "single-file mode") {
		t.Fatalf("statusMsg = %q, want a single-file-mode explanation", a.statusMsg)
	}
}

// TestHasSearch_MirrorsFinderPresence pins the enablement predicate the menu
// row and command palette both rely on.
func TestHasSearch_MirrorsFinderPresence(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	a.finder = nil
	if a.hasSearch() {
		t.Fatal("hasSearch() should be false with no finder")
	}
}

// TestStickyFindSearchOptions_InheritsActiveTabToggles checks that the
// prompt's options come from the active tab's find state, not a fresh
// zero value, matching the brief: workspace search behaves like the find
// bar the user already has configured.
func TestStickyFindSearchOptions_InheritsActiveTabToggles(t *testing.T) {
	dir := t.TempDir()
	mustWriteApp(t, dir, "a.txt", "hi\n")
	a := newTestApp(t, dir)
	a.openFile(filepath.Join(dir, "a.txt"))
	tab := a.activeTabPtr()
	tab.SetFindOptions(true, true, false)

	got := a.stickyFindSearchOptions()
	if !got.CaseSensitive || !got.WholeWord || got.Regex {
		t.Fatalf("stickyFindSearchOptions() = %+v, want case-sensitive+whole-word, no regex", got)
	}
}

// TestStartWorkspaceSearch_EmptyQueryIsANoOp pins openPrompt's own contract
// ("empty submit is ignored") for the search entry point specifically —
// nothing should be flashed or scheduled for a blank query.
func TestStartWorkspaceSearch_EmptyQueryIsANoOp(t *testing.T) {
	a := seedSearchApp(t, map[string]string{"a.txt": "needle\n"})
	a.statusMsg = "untouched"
	a.startWorkspaceSearch("   ", search.Options{})
	if a.statusMsg != "untouched" {
		t.Fatalf("statusMsg = %q, want unchanged", a.statusMsg)
	}
}

// TestStartWorkspaceSearch_OpensASyntheticTabWithHits is the feature end to
// end: search a real temp project, wait for the background scan, and check
// the synthetic tab lands with the expected hit.
func TestStartWorkspaceSearch_OpensASyntheticTabWithHits(t *testing.T) {
	a := seedSearchApp(t, map[string]string{
		"needle.go": "package p\n\nfunc findMe() {}\n",
		"other.go":  "package p\n",
	})
	before := len(a.tabs)

	a.startWorkspaceSearch("findMe", search.Options{})
	ev := waitForSearchResults(t, a)
	a.handleSearchResults(ev)

	if len(a.tabs) != before+1 {
		t.Fatalf("tab count %d -> %d, want one more", before, len(a.tabs))
	}
	tab := a.activeTabPtr()
	if !tab.Synthetic {
		t.Fatal("search results tab should be synthetic")
	}
	if !strings.Contains(tab.Buffer.String(), "needle.go:3:6") {
		t.Fatalf("results body missing the expected reference line:\n%s", tab.Buffer.String())
	}
}

// TestStartWorkspaceSearch_NoMatchesFlashesWithoutOpeningATab mirrors
// handleReferences' own "no references found" behaviour: a genuinely empty
// result should not leave an empty tab behind.
func TestStartWorkspaceSearch_NoMatchesFlashesWithoutOpeningATab(t *testing.T) {
	a := seedSearchApp(t, map[string]string{"a.go": "package a\n"})
	before := len(a.tabs)

	a.startWorkspaceSearch("nowhere-to-be-found", search.Options{})
	ev := waitForSearchResults(t, a)
	a.handleSearchResults(ev)

	if len(a.tabs) != before {
		t.Fatalf("tab count changed (%d -> %d) on a zero-hit search", before, len(a.tabs))
	}
	if !strings.Contains(a.statusMsg, "No matches") {
		t.Fatalf("statusMsg = %q, want a no-matches flash", a.statusMsg)
	}
}

// TestStartWorkspaceSearch_BadRegexFlashesAsAnErrorNotNoMatches pins the UI
// side of search.Result.Err's whole reason for existing: a broken pattern
// must read differently from a valid one that matched nothing.
func TestStartWorkspaceSearch_BadRegexFlashesAsAnErrorNotNoMatches(t *testing.T) {
	a := seedSearchApp(t, map[string]string{"a.go": "package a\n"})

	a.startWorkspaceSearch("(unclosed", search.Options{Regex: true})
	ev := waitForSearchResults(t, a)
	a.handleSearchResults(ev)

	if !strings.Contains(a.statusMsg, "Search error") {
		t.Fatalf("statusMsg = %q, want a distinct search-error flash", a.statusMsg)
	}
	if strings.Contains(a.statusMsg, "No matches") {
		t.Fatal("a bad regex must never be reported as \"no matches\"")
	}
}

// TestSearchResultsTabRendersJumpableRows is the format-vs-jump weld test:
// it renders a canned result into a synthetic tab, draws the app onto a
// SimulationScreen, confirms the "path:line:col" reference line is actually
// painted on screen (not just present in the buffer), and confirms
// locationRefAt — Esc e's own parser — can parse that exact line. A row
// format that looked nicer but broke jumping (e.g. gluing the context text
// onto the same line as the reference) would fail step two even if step one
// passed.
func TestSearchResultsTabRendersJumpableRows(t *testing.T) {
	a := newTestApp(t, t.TempDir())

	res := search.Result{
		Hits: []search.Hit{
			{Path: "internal/foo.go", Line: 4, Col: 2, Width: 4, Text: "  find this line"},
		},
		Files:   1,
		Scanned: 1,
	}
	opts := search.Options{Query: "find"}
	body := renderSearchResults(res, opts)

	a.tabs = append(a.tabs, editorSyntheticTab("search: find.txt", body))
	a.activeTab = len(a.tabs) - 1

	a.draw()
	a.screen.Show() // SimulationScreen serves GetContents from the front buffer.

	const wantRef = "internal/foo.go:5:3"

	// (a) the reference line is actually painted on screen.
	scr := a.screen.(tcell.SimulationScreen)
	onScreen := false
	for y := 0; y < a.height; y++ {
		if strings.Contains(screenLine(scr, y), wantRef) {
			onScreen = true
			break
		}
	}
	if !onScreen {
		t.Fatalf("reference line %q was not painted on screen", wantRef)
	}

	// (b) the same line, taken from the buffer the way menuGoToLocation reads
	// it, parses cleanly through locationRefAt.
	tab := a.activeTabPtr()
	lineIdx := -1
	for i := 0; i < tab.Buffer.LineCount(); i++ {
		if strings.HasPrefix(strings.TrimSpace(tab.LineText(i)), wantRef) {
			lineIdx = i
			break
		}
	}
	if lineIdx < 0 {
		t.Fatalf("reference line %q not found in the buffer", wantRef)
	}

	// The row carries the matched text on the SAME line as the location. That
	// is the property that halves the vertical space a result set costs, and
	// it is only safe because locationRefAt strips the trailing ": text" — so
	// assert both halves here rather than trusting one.
	if !strings.Contains(tab.LineText(lineIdx), "find this line") {
		t.Fatalf("row %q does not carry the matched text", tab.LineText(lineIdx))
	}
	path, line, col, ok := locationRefAt(tab.LineText(lineIdx))
	if !ok {
		t.Fatalf("locationRefAt could not parse %q", wantRef)
	}
	if path != "internal/foo.go" || line != 5 || col != 3 {
		t.Fatalf("locationRefAt(%q) = %q,%d,%d, want internal/foo.go,5,3", wantRef, path, line, col)
	}
}

// TestRenderSearchResults_HeaderStatesQueryOptionsAndCounts pins the header
// content the brief calls for: query, the options in force, and the counts,
// so a surprising result set has a visible explanation.
func TestRenderSearchResults_HeaderStatesQueryOptionsAndCounts(t *testing.T) {
	res := search.Result{
		Hits: []search.Hit{
			{Path: "a.go", Line: 0, Col: 0, Width: 3, Text: "foo"},
			{Path: "a.go", Line: 1, Col: 0, Width: 3, Text: "foo"},
			{Path: "b.go", Line: 0, Col: 0, Width: 3, Text: "foo"},
		},
		Files:   5,
		Scanned: 5,
	}
	opts := search.Options{Query: "foo", CaseSensitive: true}
	body := renderSearchResults(res, opts)

	for _, want := range []string{
		"search: foo",
		"case-sensitive",
		"3 hit(s) in 2 file(s)",
		"5 of 5 file(s) scanned",
		"Esc e",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("header missing %q:\n%s", want, body)
		}
	}
}

// TestRenderSearchResults_TruncatedNotesItInTheHeader pins that a scan
// which stopped early at MaxHits says so, rather than presenting a partial
// result as if it were complete.
func TestRenderSearchResults_TruncatedNotesItInTheHeader(t *testing.T) {
	res := search.Result{
		Hits:      []search.Hit{{Path: "a.go", Line: 0, Col: 0, Width: 1, Text: "a"}},
		Files:     10,
		Scanned:   10,
		Truncated: true,
	}
	body := renderSearchResults(res, search.Options{Query: "a"})
	if !strings.Contains(body, "truncated") {
		t.Fatalf("header should note truncation:\n%s", body)
	}
}

// TestLeaderF_ResolvesToSearchInFiles pins the Esc-F binding directly,
// separate from the "every binding resolves" sweep in leader_test.go, so a
// future rebind of 'F' shows up as this test's failure message rather than
// a generic one.
func TestLeaderF_ResolvesToSearchInFiles(t *testing.T) {
	if leaderActionFor('F') == nil {
		t.Fatal("Esc F should be bound to search in files")
	}
	// Lowercase 'f' must be untouched — it's still the in-file find bar.
	if leaderActionFor('f') == nil {
		t.Fatal("Esc f (find in file) should still resolve")
	}
}

// mustWriteApp is the internal/app-local twin of internal/finder and
// internal/search's own mustWrite test helpers — kept separate rather than
// exported across packages for a helper this small.
func mustWriteApp(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatalf("mkdir for %s: %v", full, err)
	}
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
}
