// =============================================================================
// File: internal/langconf/langconf.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// Package langconf carries per-language editing behaviour — comment markers,
// auto-closing pairs, surrounding pairs, brackets and folding markers — as a
// compiled-in table keyed by language id.
//
// The DATA is derived from the `language-configuration.json` files that ship
// with Microsoft's MIT-licensed VS Code repository; see NOTICE at the
// repository root and the header of data.go. None of Microsoft's code was
// copied — only the configuration data, transcribed by gen/main.go.
//
// 🔴 `notIn` IS RECORDED BUT NOT ENFORCED, and that limit is deliberate.
// VS Code suppresses several pairs when the cursor sits inside a string or a
// comment (Rust's `"`, Go's `'`, most of Python's 22 pairs). Honouring that
// needs a tokenizer with scope tracking at the cursor, and this editor does
// not have one:
//
//   - Chroma is used for *painting*, not parsing. `editor.HighlightVisible`
//     collapses every Chroma token into a `tcell.Style` and throws the token
//     type away, so `Tab.Styles` cannot answer "is this rune inside a string"
//     — only "what colour is it", and a theme is free to give two token types
//     the same colour.
//   - `Tab.Styles` is populated for the VIEWPORT only; every other row is nil.
//   - `Tab.StyleStale` is set by every mutation, so at the instant InsertRune
//     runs the grid still describes the buffer BEFORE this keystroke.
//
// So `Pair.NotIn` is preserved as data for a future caller that can honour it,
// and the pairs are offered unconditionally today. A quote auto-closing inside
// a string is a small annoyance; code that CLAIMS to suppress it and does not
// is how a wrong fix ships.
package langconf

import (
	"sort"
	"strings"

	"github.com/cloudmanic/spice-edit/internal/lsp"
)

//go:generate go run ./gen -out data.go

// Pair is one open/close pair from a language configuration.
//
// Open and Close are strings, not runes, because upstream pairs are not all
// single characters — Python opens `r"`, TypeScript opens `${`, and Python's
// block comment is `"""`. Callers that can only handle one rune per side use
// the rune tables below, which drop the multi-character pairs.
type Pair struct {
	// Open is the text that opens the pair.
	Open string
	// Close is the text that closes it.
	Close string
	// NotIn lists the syntactic scopes ("string", "comment") in which VS Code
	// suppresses this pair. RECORDED, NOT ENFORCED — see the package comment.
	NotIn []string
}

// Comments holds a language's comment markers. Any of the three may be empty:
// plenty of languages have line comments and no block form, and a few (JSON)
// officially have neither.
type Comments struct {
	// Line is the single-line comment marker, e.g. "//" or "#".
	Line string
	// BlockStart opens a block comment, e.g. "/*".
	BlockStart string
	// BlockEnd closes it, e.g. "*/". Python's is `"""`, the same as its start.
	BlockEnd string
}

// Folding holds the region markers a language uses for explicit folding.
// Both fields are regular-expression SOURCE as VS Code wrote it, deliberately
// left uncompiled: nothing in this editor folds yet, and compiling 70 regexes
// at init to serve no caller would be pure startup cost.
type Folding struct {
	// Start matches a line that opens a foldable region.
	Start string
	// End matches a line that closes one.
	End string
}

// Config is one language's editing behaviour.
type Config struct {
	// ID is the VS Code language id this config serves ("rust", "python").
	ID string
	// Comments are the language's comment markers.
	Comments Comments
	// Brackets are the bracket pairs used for matching and indentation.
	Brackets []Pair
	// AutoClosing are the pairs typed-opener auto-completes.
	AutoClosing []Pair
	// Surrounding are the pairs that wrap a selection instead of replacing it.
	Surrounding []Pair
	// Folding are the explicit region markers.
	Folding Folding
}

// runeTable is the single-rune projection of a Config, precomputed once at
// init so a keystroke costs map lookups and no allocation. Keystroke handling
// is the hottest path in the editor and it runs on the main loop.
type runeTable struct {
	// autoClose maps an opening rune to the rune to insert after it.
	autoClose map[rune]rune
	// closers is the set of closing runes, for the step-over rule.
	closers map[rune]bool
	// surround maps an opening rune to the closer used to wrap a selection.
	surround map[rune]rune
}

// runeTables is runeTable per language id, built from configs at init.
var runeTables = buildRuneTables()

// buildRuneTables projects every Config into its single-rune form.
//
// Pairs whose opener or closer is not exactly one rune are dropped, because
// the editor's auto-close path is driven by one typed rune: Python's `r"`,
// `f'` and friends and TypeScript's `${` and `/**` have no keystroke that
// could trigger them here. Dropping them is visible only as those pairs not
// firing, which is what happens today anyway.
func buildRuneTables() map[string]runeTable {
	out := make(map[string]runeTable, len(configs))
	for id, cfg := range configs {
		rt := runeTable{
			autoClose: singleRunePairs(cfg.AutoClosing),
			surround:  singleRunePairs(cfg.Surrounding),
		}
		rt.closers = make(map[rune]bool, len(rt.autoClose))
		for _, c := range rt.autoClose {
			rt.closers[c] = true
		}
		out[id] = rt
	}
	return out
}

// singleRunePairs keeps only the pairs whose open and close sides are each
// exactly one rune, mapping opener to closer.
func singleRunePairs(pairs []Pair) map[rune]rune {
	m := make(map[rune]rune, len(pairs))
	for _, p := range pairs {
		o, ok := soleRune(p.Open)
		if !ok {
			continue
		}
		c, ok := soleRune(p.Close)
		if !ok {
			continue
		}
		// First entry wins: upstream lists the more specific pair first
		// (TypeScript's `${` before `{`), and both project to the same
		// opener only when they are genuinely the same single rune.
		if _, dup := m[o]; dup {
			continue
		}
		m[o] = c
	}
	return m
}

// soleRune reports the single rune in s, and false when s is empty or holds
// more than one rune.
func soleRune(s string) (rune, bool) {
	rs := []rune(s)
	if len(rs) != 1 {
		return 0, false
	}
	return rs[0], true
}

// ForLanguage returns the configuration for a VS Code language id. The
// boolean is false for a language the table does not cover, which callers
// must treat as "keep doing whatever you did before" rather than "this
// language has no pairs".
func ForLanguage(id string) (Config, bool) {
	cfg, ok := configs[id]
	return cfg, ok
}

// ForPath returns the configuration for a file path.
//
// The extension→language mapping is lsp.LanguageID's, reused rather than
// duplicated: a second copy would drift, and the symptom would be a file that
// the language server understands but the editor types differently, which
// nothing on screen would explain.
func ForPath(path string) (Config, bool) {
	return ForLanguage(LanguageOf(path))
}

// LanguageOf is the one extension→language mapping this package knows, kept
// as a thin named wrapper so every call site here reads as a deliberate reuse
// of internal/lsp rather than an incidental import.
func LanguageOf(path string) string {
	return lsp.LanguageID(path)
}

// AutoClosePairs returns the single-rune auto-closing pairs for path, opener
// to closer. The boolean is false when the language is not covered.
//
// 🔴 The returned map is the package's own and MUST NOT be mutated. It is
// shared by every tab of that language and returned on every keystroke, so
// copying it per call would allocate on the editor's hottest path.
func AutoClosePairs(path string) (map[rune]rune, bool) {
	rt, ok := runeTables[LanguageOf(path)]
	if !ok || len(rt.autoClose) == 0 {
		return nil, false
	}
	return rt.autoClose, true
}

// AutoCloseClosers returns the set of closing runes for path, used to decide
// whether typing a closer should step over an existing one. Same no-mutation
// contract as AutoClosePairs.
func AutoCloseClosers(path string) (map[rune]bool, bool) {
	rt, ok := runeTables[LanguageOf(path)]
	if !ok || len(rt.closers) == 0 {
		return nil, false
	}
	return rt.closers, true
}

// SurroundPairs returns the single-rune surrounding pairs for path, opener to
// closer. These are a SUPERSET of the auto-closing pairs in several languages
// — Rust surrounds with `<`/`>` and `"` but auto-closes neither `<` nor `'` —
// which is why they are a separate table rather than a reuse of the first.
// Same no-mutation contract as AutoClosePairs.
func SurroundPairs(path string) (map[rune]rune, bool) {
	rt, ok := runeTables[LanguageOf(path)]
	if !ok || len(rt.surround) == 0 {
		return nil, false
	}
	return rt.surround, true
}

// BlockComment returns the block-comment delimiters for path. The boolean is
// false when the language is unknown or has no block-comment form, which are
// deliberately the same answer to the caller: neither one can be commented.
func BlockComment(path string) (start, end string, ok bool) {
	cfg, found := ForPath(path)
	if !found || cfg.Comments.BlockStart == "" || cfg.Comments.BlockEnd == "" {
		return "", "", false
	}
	return cfg.Comments.BlockStart, cfg.Comments.BlockEnd, true
}

// LineComment returns the line-comment marker for path.
//
// Nothing in the editor calls this yet: internal/editor keeps its own
// lineCommentByExt map so the shipped `Esc /` behaviour is untouched by this
// change. It is exported because the data is here and a caller that wants the
// upstream answer should not be pushed into transcribing it a third time.
func LineComment(path string) (string, bool) {
	cfg, ok := ForPath(path)
	if !ok || cfg.Comments.Line == "" {
		return "", false
	}
	return cfg.Comments.Line, true
}

// Languages lists every language id in the table, sorted, so tests and any
// future "what do we support" surface do not have to range over a map.
func Languages() []string {
	ids := make([]string, 0, len(configs))
	for id := range configs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Covers reports whether path resolves to a language the table knows. Kept
// separate from ForPath so a caller only asking the question does not have to
// discard a whole Config to get an answer.
func Covers(path string) bool {
	_, ok := configs[LanguageOf(path)]
	return ok
}

// NotInScopes flattens the scopes named across a config's auto-closing pairs,
// sorted and de-duplicated. It exists so a test can assert what this package
// is choosing NOT to enforce — an unenforced restriction that nothing even
// names is the kind of thing that quietly becomes a false claim.
func NotInScopes(cfg Config) []string {
	seen := map[string]bool{}
	for _, p := range cfg.AutoClosing {
		for _, s := range p.NotIn {
			if s = strings.TrimSpace(s); s != "" {
				seen[s] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
