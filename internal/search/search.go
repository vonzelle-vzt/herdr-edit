// =============================================================================
// File: internal/search/search.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// Package search implements whole-workspace text search: given a project
// root and the file list from internal/finder's index, scan every file for a
// query and report every hit.
//
// Deliberately NOT a wrapper around ripgrep. Three reasons, each a documented
// failure mode elsewhere in this stack:
//
//   - PATH. herdr execs plugin panes and actions under launchd's minimal
//     PATH, where a bare `rg` never resolves (and is often a shell function
//     to begin with). A feature that works in a developer's terminal and
//     silently does nothing in a herdr pane is worse than not shipping it.
//   - Three coordinate systems. `rg --column` reports a 1-indexed BYTE
//     offset; this editor's buffer is rune-indexed; LSP is UTF-16. They
//     agree on pure ASCII, which is exactly how a column bug survives
//     testing and only shows up on a line with an emoji.
//   - Two regex dialects. rg is Rust-regex; the in-file find bar (and this
//     package) use Go's stdlib RE2. A query that behaves one way in Find and
//     another way in workspace search is a bug users would rightly report.
//
// So this package delegates the actual matching, per file, to
// internal/editor's Matches — the exact function the in-file find bar uses.
// One matcher, one regex dialect, one coordinate system (runes), identical
// semantics to Esc f's case/whole-word/regex toggles.
package search

import (
	"os"
	"path/filepath"

	"github.com/cloudmanic/spice-edit/internal/editor"
)

const (
	// defaultMaxHits caps a scan that never specifies its own limit. High
	// enough that realistic queries never hit it, low enough that a query
	// like "e" against a large repo can't allocate without bound.
	defaultMaxHits = 500

	// defaultMaxFileBytes skips anything larger than this when the caller
	// doesn't specify. 2MB comfortably covers real source files while
	// keeping a single huge generated file (a bundled JS build, a vendored
	// data dump) from dominating scan time.
	defaultMaxFileBytes = 2 * 1024 * 1024

	// binarySniffLen is how many leading bytes of a file are inspected for a
	// NUL byte before deciding it's binary. Matches the convention used by
	// git and most text tools — a NUL in the first few KB is conclusive; a
	// full-file scan just to classify wastes the time we're trying to save.
	binarySniffLen = 8192
)

// Hit is one match found in one file.
type Hit struct {
	// Path is project-relative, forward-slash separated — the same form
	// internal/finder hands out, so callers never have to convert.
	Path string
	// Line is the zero-based line number the match starts on.
	Line int
	// Col is the zero-based RUNE index the match starts at. There is no
	// byte offset anywhere in this package, on purpose — see the package
	// doc comment on why rg's byte columns are a trap here.
	Col int
	// Width is the rune width of the match, so the renderer can highlight
	// or report it without re-running the matcher.
	Width int
	// Text is the whole line the match was found on, so the renderer never
	// has to re-open the file to show context.
	Text string
}

// Options selects the query and matching mode for Run, plus the two scan
// limits a caller may want to override for a very large workspace.
type Options struct {
	// Query is the search text (or regex pattern, when Regex is set). An
	// empty Query makes Run a no-op — it returns a zero Result rather than
	// treating "" as "match everything", matching Matches' own contract.
	Query string
	// CaseSensitive, WholeWord and Regex mirror internal/editor.FindOptions
	// exactly — Run builds one from these and hands it straight to Matches,
	// so workspace search behaves identically to the in-file find bar's own
	// toggles under the same names.
	CaseSensitive bool
	WholeWord     bool
	Regex         bool
	// MaxHits caps how many hits Run collects before stopping early and
	// setting Result.Truncated. Zero or negative uses defaultMaxHits.
	MaxHits int
	// MaxFileBytes skips any file larger than this without reading it.
	// Zero or negative uses defaultMaxFileBytes.
	MaxFileBytes int64
}

// Result is everything Run learned from one scan.
type Result struct {
	// Hits is every match found, in file-list order and then in-file
	// document order. Never partially populated on error — see Err.
	Hits []Hit
	// Files is the number of candidate paths Run was given.
	Files int
	// Scanned is how many of those files were actually opened and matched
	// against — i.e. Files minus whatever was skipped as binary or
	// oversized.
	Scanned int
	// Truncated is true when MaxHits was reached before every file was
	// scanned; the caller should say so rather than presenting a partial
	// result as if it were complete.
	Truncated bool
	// Err is set ONLY for a genuine failure — currently just an unparseable
	// regex — and is a distinct field from an empty Hits so the two can
	// never be confused. This fork has shipped that exact confusion once
	// already (Tab.FindErr, set by the matcher and read by nobody, so a bad
	// pattern reported "no results" instead of an error): see CLAUDE.md's
	// "THE CALLER IS THE FEATURE" note. When Err != nil, Hits is always
	// nil — a caller checking Err first can never be misled by leftover
	// hits from before the failure.
	Err error
}

// Run scans every file in files (project-relative paths, resolved against
// root) for opts.Query and returns every hit, in file order.
//
// A bad regex is validated ONCE up front, against a throwaway one-line
// buffer, rather than discovered partway through the scan. regexp.Compile
// either succeeds or fails independently of any file's content, so
// re-discovering the same compile error on every file would just burn the
// time a user is waiting on — and it would also mean Result.Hits could be
// partially populated when Err is set, which is exactly the "was that no
// results, or did it break?" ambiguity Result.Err exists to prevent.
func Run(root string, files []string, opts Options) Result {
	res := Result{Files: len(files)}
	if opts.Query == "" {
		return res
	}

	maxHits := opts.MaxHits
	if maxHits <= 0 {
		maxHits = defaultMaxHits
	}
	maxBytes := opts.MaxFileBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxFileBytes
	}

	findOpts := editor.FindOptions{
		CaseSensitive: opts.CaseSensitive,
		WholeWord:     opts.WholeWord,
		Regex:         opts.Regex,
	}

	if _, err := editor.Matches(editor.NewBuffer(""), opts.Query, findOpts); err != nil {
		res.Err = err
		return res
	}

	for _, rel := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Stat(abs)
		if err != nil || info.IsDir() {
			continue
		}
		if info.Size() > maxBytes {
			continue
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		if looksBinary(data) {
			continue
		}
		res.Scanned++

		buf := editor.NewBuffer(string(data))
		matches, err := editor.Matches(buf, opts.Query, findOpts)
		if err != nil {
			// Defensive only: the up-front validation above already proved
			// the pattern compiles, and regexp compilation doesn't depend
			// on the text it's later run against. Kept rather than
			// asserted-away so a future change to Matches can't silently
			// reintroduce the "bad pattern reads as no results" bug this
			// field exists to prevent.
			res.Hits = nil
			res.Err = err
			return res
		}
		for _, m := range matches {
			if len(res.Hits) >= maxHits {
				res.Truncated = true
				break
			}
			res.Hits = append(res.Hits, Hit{
				Path:  rel,
				Line:  m.Line,
				Col:   m.Col,
				Width: m.Width,
				Text:  buf.Lines[m.Line],
			})
		}
		if len(res.Hits) >= maxHits {
			res.Truncated = true
			break
		}
	}
	return res
}

// looksBinary reports whether the leading bytes of data contain a NUL byte
// — the same heuristic git and most text tools use to classify a file as
// binary without decoding its whole contents.
func looksBinary(data []byte) bool {
	n := len(data)
	if n > binarySniffLen {
		n = binarySniffLen
	}
	for i := 0; i < n; i++ {
		if data[i] == 0 {
			return true
		}
	}
	return false
}
