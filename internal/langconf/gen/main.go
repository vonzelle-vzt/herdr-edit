// =============================================================================
// File: internal/langconf/gen/main.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// Command gen reads the `language-configuration.json` files that ship with a
// VS Code installation and writes internal/langconf/data.go.
//
// It is a GENERATOR rather than a hand-transcribed table on purpose. Seventy
// languages' worth of pairs, comment markers and folding regexes cannot be
// hand-copied accurately, cannot be re-derived after an upstream change, and
// cannot be audited — whereas a generated file plus
// TestGeneratedDataMatchesTheSource makes drift a test failure.
//
// 🔴 It takes DATA ONLY. None of VS Code's TypeScript is read or copied: it
// would be useless in a Go TUI and would add licence surface for nothing.
//
// 🔴 It reads only extensions whose package.json declares publisher "vscode"
// and license "MIT" — that is the set which lives in the github.com/microsoft/vscode
// repository itself. `ms-vscode.js-debug` is MIT too, but it comes from a
// DIFFERENT repository (microsoft/vscode-js-debug), and including it would
// make NOTICE's provenance claim false. That filter is why `.wat` is not
// covered.
//
// The JSONC stripper is internal/dap's exported character scanner, not a
// second copy: `language-configuration.json` is JSONC and a regex stripper
// breaks on a `//` inside a string, which is the exact failure that scanner
// was written to fix.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudmanic/spice-edit/internal/dap"
)

// noSourceExit is the exit code for "there is no VS Code here to read".
// Distinct from a generic failure so a caller — TestGeneratedDataMatchesTheSource
// above all — can tell "not installed, skip" from "installed and broken",
// instead of skipping on any error and quietly enforcing nothing.
const noSourceExit = 3

// main parses flags, resolves a VS Code installation and writes the table.
func main() {
	vscode := flag.String("vscode", "", "path to a VS Code Resources/app directory (default: auto-detect)")
	out := flag.String("out", "data.go", "file to write the generated table to")
	flag.Parse()

	dir := *vscode
	if dir == "" {
		found, ok := findVSCode()
		if !ok {
			fmt.Fprintf(os.Stderr, "langconf/gen: no VS Code installation found (looked in: %s)\n",
				strings.Join(candidateDirs(), ", "))
			os.Exit(noSourceExit)
		}
		dir = found
	}
	src, err := generate(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "langconf/gen: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, src, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "langconf/gen: write %s: %v\n", *out, err)
		os.Exit(1)
	}
}

// candidateDirs lists where a VS Code installation's Resources/app directory
// normally sits, per platform. Kept in one place so the generator and anything
// that asks it "where would you look" cannot disagree.
func candidateDirs() []string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return []string{
			"/Applications/Visual Studio Code.app/Contents/Resources/app",
			filepath.Join(home, "Applications/Visual Studio Code.app/Contents/Resources/app"),
			"/Applications/VSCodium.app/Contents/Resources/app",
		}
	case "windows":
		return []string{
			filepath.Join(os.Getenv("LOCALAPPDATA"), `Programs\Microsoft VS Code\resources\app`),
			`C:\Program Files\Microsoft VS Code\resources\app`,
		}
	default:
		return []string{
			"/usr/share/code/resources/app",
			"/usr/lib/code/resources/app",
			"/opt/visual-studio-code/resources/app",
			"/usr/share/codium/resources/app",
		}
	}
}

// findVSCode returns the first candidate directory that actually holds an
// extensions folder. Checking for the extensions folder rather than the
// directory itself matters: a stub or partially removed install would
// otherwise be accepted and produce an empty table.
func findVSCode() (string, bool) {
	for _, dir := range candidateDirs() {
		if st, err := os.Stat(filepath.Join(dir, "extensions")); err == nil && st.IsDir() {
			return dir, true
		}
	}
	return "", false
}

// rawPackage is the slice of an extension's package.json this generator reads.
type rawPackage struct {
	Publisher   string `json:"publisher"`
	License     string `json:"license"`
	Version     string `json:"version"`
	Contributes struct {
		Languages []struct {
			ID            string `json:"id"`
			Configuration string `json:"configuration"`
		} `json:"languages"`
	} `json:"contributes"`
}

// rawConfig is the slice of a language-configuration.json this generator
// reads. The pair lists stay as RawMessage because upstream writes each entry
// as EITHER a two-element array or an object with a notIn field, sometimes
// both within one file.
type rawConfig struct {
	Comments struct {
		// LineComment is RawMessage because it is not always a string:
		// makefile writes {"comment":"#","noIndent":true}. A plain string
		// field silently fails the whole file's decode, which is how this
		// was found — the generator refused to run at all.
		LineComment  json.RawMessage `json:"lineComment"`
		BlockComment []string        `json:"blockComment"`
	} `json:"comments"`
	Brackets         []json.RawMessage `json:"brackets"`
	AutoClosingPairs []json.RawMessage `json:"autoClosingPairs"`
	SurroundingPairs []json.RawMessage `json:"surroundingPairs"`
	Folding          struct {
		Markers struct {
			Start string `json:"start"`
			End   string `json:"end"`
		} `json:"markers"`
	} `json:"folding"`
}

// pair mirrors langconf.Pair. Declared separately so the generator does not
// import the package it generates into, which would be a build cycle the first
// time data.go failed to compile.
type pair struct {
	Open  string
	Close string
	NotIn []string
}

// langConfig is one fully-resolved language, ready to emit.
type langConfig struct {
	ID          string
	LineComment string
	BlockStart  string
	BlockEnd    string
	Brackets    []pair
	AutoClosing []pair
	Surrounding []pair
	FoldStart   string
	FoldEnd     string
}

// generate reads every eligible language configuration under dir and returns
// formatted Go source for data.go.
func generate(dir string) ([]byte, error) {
	version, err := readVersion(dir)
	if err != nil {
		return nil, err
	}
	langs, err := collect(dir)
	if err != nil {
		return nil, err
	}
	if len(langs) == 0 {
		return nil, fmt.Errorf("no language configurations found under %s", dir)
	}
	return render(version, langs)
}

// readVersion reads the product version out of the installation's own
// package.json, so the generated file can say exactly which upstream release
// the data came from. Without it the provenance in NOTICE is unverifiable.
func readVersion(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return "", fmt.Errorf("read product package.json: %w", err)
	}
	var pkg rawPackage
	if err := json.Unmarshal(dap.StripJSONC(data), &pkg); err != nil {
		return "", fmt.Errorf("parse product package.json: %w", err)
	}
	if pkg.Version == "" {
		return "", fmt.Errorf("product package.json has no version")
	}
	return pkg.Version, nil
}

// collect walks the built-in extensions and returns their language configs,
// sorted by language id so the output is byte-stable across runs.
func collect(dir string) ([]langConfig, error) {
	manifests, err := filepath.Glob(filepath.Join(dir, "extensions", "*", "package.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(manifests)

	seen := map[string]bool{}
	var out []langConfig
	for _, manifest := range manifests {
		data, err := os.ReadFile(manifest)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", manifest, err)
		}
		var pkg rawPackage
		if err := json.Unmarshal(dap.StripJSONC(data), &pkg); err != nil {
			// A built-in whose manifest we cannot parse is not a reason to
			// abort the whole table; skip it and keep going.
			continue
		}
		if pkg.Publisher != "vscode" || pkg.License != "MIT" {
			continue
		}
		extDir := filepath.Dir(manifest)
		for _, lang := range pkg.Contributes.Languages {
			if lang.ID == "" || lang.Configuration == "" || seen[lang.ID] {
				continue
			}
			cfg, err := loadConfig(filepath.Join(extDir, filepath.FromSlash(lang.Configuration)))
			if err != nil {
				return nil, err
			}
			cfg.ID = lang.ID
			seen[lang.ID] = true
			out = append(out, cfg)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// loadConfig reads and decodes one language-configuration.json.
func loadConfig(path string) (langConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return langConfig{}, fmt.Errorf("read %s: %w", path, err)
	}
	var raw rawConfig
	if err := json.Unmarshal(dap.StripJSONC(data), &raw); err != nil {
		return langConfig{}, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg := langConfig{
		LineComment: decodeLineComment(raw.Comments.LineComment),
		Brackets:    decodePairs(raw.Brackets),
		AutoClosing: decodePairs(raw.AutoClosingPairs),
		Surrounding: decodePairs(raw.SurroundingPairs),
		FoldStart:   raw.Folding.Markers.Start,
		FoldEnd:     raw.Folding.Markers.End,
	}
	if len(raw.Comments.BlockComment) >= 2 {
		cfg.BlockStart = raw.Comments.BlockComment[0]
		cfg.BlockEnd = raw.Comments.BlockComment[1]
	}
	return cfg, nil
}

// decodeLineComment reads the line-comment marker, which upstream writes as a
// plain string almost everywhere and as {"comment":"#","noIndent":true} for
// makefile. The noIndent hint is dropped: nothing here indents a comment
// marker, so recording a flag no code reads would be a claim we do not honour.
func decodeLineComment(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var obj struct {
		Comment string `json:"comment"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		return obj.Comment
	}
	return ""
}

// decodePairs turns a list of upstream pair entries into pairs, tolerating
// both spellings. An entry that yields no opener is dropped rather than
// emitted as an empty pair, which would look like a real pair to the consumer.
func decodePairs(raws []json.RawMessage) []pair {
	out := make([]pair, 0, len(raws))
	for _, raw := range raws {
		p, ok := decodePair(raw)
		if !ok {
			continue
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// decodePair decodes one entry, which upstream writes either as ["{", "}"] or
// as {"open":"{","close":"}","notIn":["string"]}.
//
// A pair with an empty close side is KEPT (TypeScript really does list
// ["$", ""]): dropping it here would make the table disagree with its source
// for no gain, and the single-rune projection in langconf.go discards it
// anyway.
func decodePair(raw json.RawMessage) (pair, bool) {
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		if len(arr) < 2 || arr[0] == "" {
			return pair{}, false
		}
		return pair{Open: arr[0], Close: arr[1]}, true
	}
	var obj struct {
		Open  string   `json:"open"`
		Close string   `json:"close"`
		NotIn []string `json:"notIn"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil || obj.Open == "" {
		return pair{}, false
	}
	return pair{Open: obj.Open, Close: obj.Close, NotIn: obj.NotIn}, true
}

// render writes the Go source for data.go and gofmts it.
//
// The header carries MICROSOFT's copyright, not this fork's: the data below it
// is theirs, and MIT requires the notice travel with it. Every other new file
// in this repository carries the fork's header; this one is the exception, and
// that is the point.
func render(version string, langs []langConfig) ([]byte, error) {
	var b strings.Builder
	fmt.Fprintf(&b, `// Code generated by internal/langconf/gen; DO NOT EDIT.
//
// Regenerate with:  go generate ./internal/langconf
//
// -----------------------------------------------------------------------------
// The DATA below is derived from the language-configuration.json files of the
// built-in language extensions in github.com/microsoft/vscode, which is
// licensed MIT. Only data was taken; no VS Code source code is reproduced here.
//
//	Copyright (c) Microsoft Corporation. All rights reserved.
//	Licensed under the MIT License.
//
// Extracted from Visual Studio Code %s. See NOTICE at the repository root
// for the full attribution and the OSS-repository / binary-distribution
// distinction.
// -----------------------------------------------------------------------------

package langconf

// SourceVersion is the Visual Studio Code release the table below was
// extracted from, recorded so the provenance in NOTICE can be checked rather
// than taken on trust.
const SourceVersion = %s

// configs is the compiled-in language table, keyed by VS Code language id.
// %d languages.
var configs = map[string]Config{
`, version, strconv.Quote(version), len(langs))

	for _, lang := range langs {
		fmt.Fprintf(&b, "%s: {\n", strconv.Quote(lang.ID))
		fmt.Fprintf(&b, "ID: %s,\n", strconv.Quote(lang.ID))
		if lang.LineComment != "" || lang.BlockStart != "" || lang.BlockEnd != "" {
			b.WriteString("Comments: Comments{")
			var parts []string
			if lang.LineComment != "" {
				parts = append(parts, "Line: "+strconv.Quote(lang.LineComment))
			}
			if lang.BlockStart != "" {
				parts = append(parts, "BlockStart: "+strconv.Quote(lang.BlockStart))
			}
			if lang.BlockEnd != "" {
				parts = append(parts, "BlockEnd: "+strconv.Quote(lang.BlockEnd))
			}
			b.WriteString(strings.Join(parts, ", "))
			b.WriteString("},\n")
		}
		writePairs(&b, "Brackets", lang.Brackets)
		writePairs(&b, "AutoClosing", lang.AutoClosing)
		writePairs(&b, "Surrounding", lang.Surrounding)
		if lang.FoldStart != "" || lang.FoldEnd != "" {
			fmt.Fprintf(&b, "Folding: Folding{Start: %s, End: %s},\n",
				strconv.Quote(lang.FoldStart), strconv.Quote(lang.FoldEnd))
		}
		b.WriteString("},\n")
	}
	b.WriteString("}\n")

	src, err := format.Source([]byte(b.String()))
	if err != nil {
		return nil, fmt.Errorf("gofmt generated source: %w", err)
	}
	return src, nil
}

// writePairs emits one Pair slice field, or nothing at all when it is empty.
func writePairs(b *strings.Builder, field string, pairs []pair) {
	if len(pairs) == 0 {
		return
	}
	fmt.Fprintf(b, "%s: []Pair{\n", field)
	for _, p := range pairs {
		fmt.Fprintf(b, "{Open: %s, Close: %s", strconv.Quote(p.Open), strconv.Quote(p.Close))
		if len(p.NotIn) > 0 {
			quoted := make([]string, len(p.NotIn))
			for i, s := range p.NotIn {
				quoted[i] = strconv.Quote(s)
			}
			fmt.Fprintf(b, ", NotIn: []string{%s}", strings.Join(quoted, ", "))
		}
		b.WriteString("},\n")
	}
	b.WriteString("},\n")
}
