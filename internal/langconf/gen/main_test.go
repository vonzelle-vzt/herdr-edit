// =============================================================================
// File: internal/langconf/gen/main_test.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// Tests for the language-table generator. They run against a FIXTURE tree in
// t.TempDir() rather than a real VS Code install, so they pin the generator's
// behaviour on machines that have no VS Code at all — the drift check in
// internal/langconf is the one that needs the real thing.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFixture builds a miniature VS Code installation tree and returns its
// root. Content is written verbatim, so a caller can hand it JSONC.
func writeFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return root
}

// TestGenerateReadsAFixtureTree is the end-to-end shape: a product manifest
// plus one built-in extension produce a formatted table carrying the version,
// the language id and its pairs.
func TestGenerateReadsAFixtureTree(t *testing.T) {
	root := writeFixture(t, map[string]string{
		"package.json": `{"name":"Code","version":"9.9.9"}`,
		"extensions/fake/package.json": `{"publisher":"vscode","license":"MIT","contributes":{
			"languages":[{"id":"fake","configuration":"./language-configuration.json"}]}}`,
		"extensions/fake/language-configuration.json": `{
			"comments": {"lineComment": "//", "blockComment": ["/*", "*/"]},
			"brackets": [["{","}"]],
			"autoClosingPairs": [["{","}"], {"open":"\"","close":"\"","notIn":["string"]}],
			"surroundingPairs": [["{","}"], ["<",">"]],
			"folding": {"markers": {"start": "^\\s*//\\s*#region", "end": "^\\s*//\\s*#endregion"}}
		}`,
	})
	src, err := generate(root)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	got := string(src)
	for _, want := range []string{
		`const SourceVersion = "9.9.9"`,
		`"fake": {`,
		`ID:       "fake"`,
		`Comments{Line: "//", BlockStart: "/*", BlockEnd: "*/"}`,
		`NotIn: []string{"string"}`,
		`{Open: "<", Close: ">"}`,
		`Folding{Start: "^\\s*//\\s*#region", End: "^\\s*//\\s*#endregion"}`,
		"Copyright (c) Microsoft Corporation",
		"DO NOT EDIT",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("generated source is missing %q\n---\n%s", want, got)
		}
	}
}

// TestGenerateOnlyTakesTheVSCodeRepositorysOwnExtensions is the provenance
// guard. NOTICE claims the data came from github.com/microsoft/vscode; an
// extension published by anyone else — even an MIT one from another Microsoft
// repository — would make that claim false, and nothing downstream could tell.
func TestGenerateOnlyTakesTheVSCodeRepositorysOwnExtensions(t *testing.T) {
	cfg := `{"comments":{"lineComment":"#"},"autoClosingPairs":[["(",")"]]}`
	manifest := func(publisher, license, id string) string {
		return `{"publisher":"` + publisher + `","license":"` + license + `","contributes":{
			"languages":[{"id":"` + id + `","configuration":"./language-configuration.json"}]}}`
	}
	root := writeFixture(t, map[string]string{
		"package.json":                                    `{"version":"1.0.0"}`,
		"extensions/builtin/package.json":                 manifest("vscode", "MIT", "keeper"),
		"extensions/builtin/language-configuration.json":  cfg,
		"extensions/foreign/package.json":                 manifest("ms-vscode", "MIT", "otherrepo"),
		"extensions/foreign/language-configuration.json":  cfg,
		"extensions/nonfree/package.json":                 manifest("vscode", "Proprietary", "nonfree"),
		"extensions/nonfree/language-configuration.json":  cfg,
		"extensions/unparsed/package.json":                `{ this is not json`,
		"extensions/unparsed/language-configuration.json": cfg,
	})
	src, err := generate(root)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	got := string(src)
	if !strings.Contains(got, `"keeper"`) {
		t.Error("the vscode-published MIT extension should be in the table")
	}
	for _, unwanted := range []string{`"otherrepo"`, `"nonfree"`} {
		if strings.Contains(got, unwanted) {
			t.Errorf("%s must not be in the table — it is not from microsoft/vscode", unwanted)
		}
	}
}

// TestGenerateUsesTheSharedJSONCScanner is why this generator does not strip
// comments with a regex. `language-configuration.json` is JSONC, and a `//`
// inside a string literal is exactly what a regex stripper truncates — here,
// inside a folding regex that legitimately matches a `//` comment. The failure
// would be silent: the JSON stops parsing and the language vanishes from the
// table.
func TestGenerateUsesTheSharedJSONCScanner(t *testing.T) {
	root := writeFixture(t, map[string]string{
		"package.json": `{"version":"1.0.0"}`,
		"extensions/tricky/package.json": `{"publisher":"vscode","license":"MIT","contributes":{
			"languages":[{"id":"tricky","configuration":"./language-configuration.json"}]}}`,
		"extensions/tricky/language-configuration.json": `{
			// a real line comment, which must go
			"comments": {"lineComment": "//"},
			"folding": {"markers": {"start": "^\\s*// #region", "end": "^\\s*// #endregion"}},
			"autoClosingPairs": [
				{"open": "\"", "close": "\"", "notIn": ["string"]}, /* trailing comma next */
			]
		}`,
	})
	src, err := generate(root)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	got := string(src)
	if !strings.Contains(got, `"tricky"`) {
		t.Fatalf("the language was dropped entirely:\n%s", got)
	}
	if !strings.Contains(got, `Start: "^\\s*// #region"`) {
		t.Errorf("the // inside the folding regex was eaten:\n%s", got)
	}
	if strings.Contains(got, "a real line comment") {
		t.Error("a genuine JSONC comment survived into the data")
	}
}

// TestDecodePairAcceptsBothUpstreamSpellings pins the union type: upstream
// writes a pair as either a two-element array or an object with notIn, and
// mixes the two inside one file.
func TestDecodePairAcceptsBothUpstreamSpellings(t *testing.T) {
	cases := []struct {
		raw   string
		want  pair
		valid bool
	}{
		{`["{","}"]`, pair{Open: "{", Close: "}"}, true},
		{`{"open":"'","close":"'","notIn":["string","comment"]}`,
			pair{Open: "'", Close: "'", NotIn: []string{"string", "comment"}}, true},
		{`{"open":"${","close":"}"}`, pair{Open: "${", Close: "}"}, true},
		{`["$",""]`, pair{Open: "$", Close: ""}, true}, // TypeScript really has this
		{`["{"]`, pair{}, false},
		{`[]`, pair{}, false},
		{`{"close":"}"}`, pair{}, false},
		{`"nonsense"`, pair{}, false},
	}
	for _, c := range cases {
		got, ok := decodePair([]byte(c.raw))
		if ok != c.valid {
			t.Errorf("%s: ok = %v, want %v", c.raw, ok, c.valid)
			continue
		}
		if !ok {
			continue
		}
		if got.Open != c.want.Open || got.Close != c.want.Close || len(got.NotIn) != len(c.want.NotIn) {
			t.Errorf("%s: got %+v, want %+v", c.raw, got, c.want)
		}
	}
}

// TestDecodeLineCommentHandlesTheMakefileObjectForm covers the one shape in
// the real data that is not a string. makefile writes
// {"comment":"#","noIndent":true}; a string-typed field failed the whole
// file's decode and stopped the generator dead, which is how this was found.
func TestDecodeLineCommentHandlesTheMakefileObjectForm(t *testing.T) {
	cases := map[string]string{
		`"//"`:                            "//",
		`{"comment":"#","noIndent":true}`: "#",
		`{"noIndent":true}`:               "",
		`null`:                            "",
		`12`:                              "",
	}
	for raw, want := range cases {
		if got := decodeLineComment([]byte(raw)); got != want {
			t.Errorf("%s: got %q, want %q", raw, got, want)
		}
	}
	if got := decodeLineComment(nil); got != "" {
		t.Errorf("missing field: got %q, want empty", got)
	}
}

// TestGenerateIsDeterministic is what makes TestGeneratedDataMatchesTheSource
// meaningful: two runs over the same tree must be byte-identical, or the drift
// check would fail on map iteration order rather than on real drift.
func TestGenerateIsDeterministic(t *testing.T) {
	cfg := `{"comments":{"lineComment":"#"},"autoClosingPairs":[["(",")"],["[","]"]]}`
	files := map[string]string{"package.json": `{"version":"2.0.0"}`}
	for _, id := range []string{"zeta", "alpha", "mu", "beta"} {
		files["extensions/"+id+"/package.json"] = `{"publisher":"vscode","license":"MIT","contributes":{
			"languages":[{"id":"` + id + `","configuration":"./language-configuration.json"}]}}`
		files["extensions/"+id+"/language-configuration.json"] = cfg
	}
	root := writeFixture(t, files)

	first, err := generate(root)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := generate(root)
		if err != nil {
			t.Fatalf("generate (run %d): %v", i, err)
		}
		if string(again) != string(first) {
			t.Fatalf("run %d differs from the first — the output is not deterministic", i)
		}
	}
	// Sorted by language id, not by directory-walk order.
	idx := func(s string) int { return strings.Index(string(first), `"`+s+`": {`) }
	if !(idx("alpha") < idx("beta") && idx("beta") < idx("mu") && idx("mu") < idx("zeta")) {
		t.Error("languages are not emitted in sorted order")
	}
}

// TestGenerateFailsLoudlyOnAnEmptyTree makes sure a wrong -vscode path is an
// error rather than a silently empty table — an empty table would compile,
// pass every type check, and switch per-language behaviour off everywhere.
func TestGenerateFailsLoudlyOnAnEmptyTree(t *testing.T) {
	root := writeFixture(t, map[string]string{"package.json": `{"version":"1.0.0"}`})
	if _, err := generate(root); err == nil {
		t.Fatal("generate should fail when it finds no language configurations")
	}
	missing := writeFixture(t, map[string]string{"unrelated.txt": "x"})
	if _, err := generate(missing); err == nil {
		t.Fatal("generate should fail when there is no product package.json")
	}
}

// TestFindVSCodeRequiresAnExtensionsDirectory pins the detection rule: a
// directory that exists but holds no extensions is not an installation. A
// looser check would accept a leftover empty folder and produce an empty
// table.
func TestFindVSCodeRequiresAnExtensionsDirectory(t *testing.T) {
	dirs := candidateDirs()
	if len(dirs) == 0 {
		t.Fatal("no candidate directories for this platform")
	}
	for _, d := range dirs {
		if !filepath.IsAbs(d) {
			t.Errorf("candidate %q should be absolute", d)
		}
	}
	// findVSCode consults real paths, so only assert the contract it must
	// hold either way: what it returns, if anything, has an extensions dir.
	if dir, ok := findVSCode(); ok {
		st, err := os.Stat(filepath.Join(dir, "extensions"))
		if err != nil || !st.IsDir() {
			t.Errorf("findVSCode returned %q with no extensions directory", dir)
		}
	}
}
