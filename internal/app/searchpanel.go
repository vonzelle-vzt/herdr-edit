// =============================================================================
// File: internal/app/searchpanel.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// searchpanel.go wires internal/search into the editor: Esc F (VS Code's
// Ctrl+Shift+F shifting Ctrl+F) prompts for a query, scans every file
// internal/finder already knows about, and shows the hits as a synthetic
// tab — the same mechanism the diff view and find-references already use,
// so the result list scrolls, closes and highlights like any other tab
// instead of needing a bespoke result pane.
//
// The prompt is reused (openPrompt, the same modal Esc g / rename use)
// rather than a second text-input widget — a second modal is a second copy
// of caret handling, horizontal scroll and Esc-to-cancel to keep in sync.
//
// The scan runs on a goroutine and reports back through a posted event, the
// established pattern every other background action in this fork uses
// (diagnostics, references, rename, code actions, blame). Calling
// search.Run inline would block Run's PollEvent loop for as long as the
// scan takes — fine for a small project, not fine for the monorepo this
// feature exists for.
//
// Row format: each hit renders as its own bare "path:line:col" line,
// followed by an indented line of context text. That split — rather than
// one combined "path:line:col: text" line — is not a style choice, it's
// forced by state.SplitLocation (which locationRefAt, Esc e's parser,
// calls directly on whatever line the cursor sits on): SplitLocation only
// strips trailing colon-separated INTEGER groups from the right, so any
// non-empty content after the column number — a space, a colon, the actual
// matched text — makes the last split segment non-numeric and the whole
// parse bails out with the original string untouched. handleReferences hit
// this exact constraint first (see refactor.go) and settled on the same
// bare "path:line:col" line for the same reason. Splitting hit and context
// across two lines keeps the reference line genuinely jumpable while still
// showing the match in context, without touching locationRefAt or
// state.SplitLocation — which sit outside this feature's file scope and
// are shared by every other "jump to a reference" caller in the editor.
package app

import (
	"fmt"
	"strings"
	"time"

	"github.com/cloudmanic/spice-edit/internal/search"
)

// searchResultsEvent delivers a completed workspace scan onto the tcell
// event queue. opts is carried alongside res so the header can describe
// exactly what was searched under, even though the scan may finish well
// after the user has moved on to something else.
type searchResultsEvent struct {
	res  search.Result
	opts search.Options
	when time.Time
}

// When satisfies the tcell.Event interface.
func (e *searchResultsEvent) When() time.Time { return e.when }

// menuSearchInFiles prompts for a query and kicks off a workspace-wide
// scan. Bound to Esc F and to a menu row. Options are inherited from the
// active tab's sticky find toggles (case-sensitive / whole-word / regex)
// rather than re-asked here — the goal is "search behaves like the find bar
// I already have open", not a second set of toggles to configure.
func (a *App) menuSearchInFiles() {
	a.closeMenu()
	if a.finder == nil {
		a.flash("Search in workspace isn't available in single-file mode")
		return
	}
	opts := a.stickyFindSearchOptions()
	hint := "across every indexed file — " + searchOptionsLabel(opts)
	a.openPrompt("Search in workspace", hint, "", func(app *App, query string) {
		app.startWorkspaceSearch(query, opts)
	})
}

// stickyFindSearchOptions reads the active tab's find toggles (if any) into
// a search.Options, leaving the zero value (case-insensitive plain text,
// matching Matches' own default) when there is no active tab to inherit
// from.
func (a *App) stickyFindSearchOptions() search.Options {
	var opts search.Options
	if tab := a.activeTabPtr(); tab != nil {
		opts.CaseSensitive = tab.FindCaseSensitive
		opts.WholeWord = tab.FindWholeWord
		opts.Regex = tab.FindRegex
	}
	return opts
}

// hasSearch is the menu/palette enablement predicate — identical to
// hasFinder's rule (a project index has to exist), kept as its own named
// predicate so the menu table reads in terms of the feature it gates.
func (a *App) hasSearch() bool {
	return a.finder != nil
}

// startWorkspaceSearch launches the scan on a goroutine and posts the
// result back to the main loop. An empty (post-trim) query is a silent
// no-op, matching openPrompt's own "empty submit is ignored" contract.
func (a *App) startWorkspaceSearch(query string, opts search.Options) {
	query = trimSpace(query)
	if query == "" {
		return
	}
	opts.Query = query
	root := a.rootDir
	files := a.finder.Paths()
	go func() {
		res := search.Run(root, files, opts)
		a.post(&searchResultsEvent{res: res, opts: opts, when: time.Now()})
	}()
	a.flash("Searching \"" + query + "\"…")
}

// handleSearchResults reacts to a completed scan: a compile error is
// flashed distinctly from "no matches" (see search.Result.Err's doc
// comment for why that distinction is load-bearing), a genuinely empty
// result is flashed rather than opening an empty tab, and otherwise the
// hits become a synthetic tab.
func (a *App) handleSearchResults(e *searchResultsEvent) {
	if e.res.Err != nil {
		a.flash("Search error: " + e.res.Err.Error())
		return
	}
	if len(e.res.Hits) == 0 {
		a.flash("No matches for \"" + e.opts.Query + "\"")
		return
	}
	label := "search: " + e.opts.Query + ".txt"
	a.tabs = append(a.tabs, editorSyntheticTab(label, renderSearchResults(e.res, e.opts)))
	a.activeTab = len(a.tabs) - 1
	a.flash("Esc e opens the hit under the cursor; Esc w closes the list")
}

// renderSearchResults builds the synthetic tab's full text: a header stating
// the query, the options in force and the counts (so a surprising result
// set has a visible explanation), then ONE line per hit,
// "path:line:col: matched text".
//
// One line per hit, not two: vertical space is the scarce resource in the
// 60-column pane beside an agent this editor targets, and a two-line form
// halves how many results fit on screen. locationRefAt parses the trailing
// ": text" suffix, so the row stays jumpable with Esc e.
//
// 🔴 Rows stay FLAT and self-describing rather than grouped under filename
// headers. A synthetic tab is a real editable buffer — Save() refuses, but
// nothing stops the user typing in it — so a row-index side table would go
// stale on the first keystroke and every jump after that would land silently
// on the wrong line. Whatever the user does to this buffer, the row still
// says where it points.
//
// Kept as a pure function of (Result, Options) — no App — so the layout can
// be pinned down in a test without spinning up a screen.
func renderSearchResults(res search.Result, opts search.Options) string {
	var b strings.Builder
	fmt.Fprintf(&b, "search: %s\n", opts.Query)
	fmt.Fprintf(&b, "options: %s\n", searchOptionsLabel(opts))
	fmt.Fprintf(&b, "%d hit(s) in %d file(s) — %d of %d file(s) scanned%s\n",
		len(res.Hits), countHitFiles(res.Hits), res.Scanned, res.Files, truncatedSuffix(res.Truncated))
	fmt.Fprintf(&b, "Esc e jumps to the hit under the cursor; Esc w closes this list\n\n")
	for _, h := range res.Hits {
		fmt.Fprintf(&b, "%s:%d:%d: %s\n", h.Path, h.Line+1, h.Col+1, strings.TrimSpace(h.Text))
	}
	return b.String()
}

// searchOptionsLabel renders the three sticky toggles as the same short
// vocabulary the find bar's own status text uses, so a user who knows the
// find bar recognises the header at a glance.
func searchOptionsLabel(opts search.Options) string {
	parts := make([]string, 0, 3)
	if opts.CaseSensitive {
		parts = append(parts, "case-sensitive")
	} else {
		parts = append(parts, "case-insensitive")
	}
	if opts.WholeWord {
		parts = append(parts, "whole word")
	}
	if opts.Regex {
		parts = append(parts, "regex")
	} else {
		parts = append(parts, "plain text")
	}
	return strings.Join(parts, ", ")
}

// countHitFiles returns the number of distinct files represented in hits —
// the header reports both "N hits" and "in M files" since a handful of hits
// clustered in one file reads very differently from the same count spread
// across a dozen.
func countHitFiles(hits []search.Hit) int {
	seen := make(map[string]bool, len(hits))
	for _, h := range hits {
		seen[h.Path] = true
	}
	return len(seen)
}

// truncatedSuffix appends a visible warning to the header's count line when
// the scan stopped early at MaxHits — silently showing a partial result as
// if it were complete would be the same "looks done, isn't" failure mode
// CLAUDE.md warns about elsewhere in this fork.
func truncatedSuffix(truncated bool) string {
	if truncated {
		return " (truncated — refine your query)"
	}
	return ""
}
