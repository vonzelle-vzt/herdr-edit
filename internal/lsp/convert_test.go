// =============================================================================
// File: internal/lsp/convert_test.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-07-29
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

package lsp

import (
	"encoding/json"
	"testing"
)

// TestURIEscapesSpaces guards the failure that produces no error and no
// diagnostics: an unescaped path yields a URI the server never matches against
// the one it publishes, so every diagnostic silently belongs to no open tab.
func TestURIEscapesSpaces(t *testing.T) {
	got := URI("/Users/me/my projects/app.ts")
	want := "file:///Users/me/my%20projects/app.ts"
	if got != want {
		t.Fatalf("URI = %q, want %q", got, want)
	}
	if back := PathFromURI(got); back != "/Users/me/my projects/app.ts" {
		t.Fatalf("round trip = %q", back)
	}
}

// TestURIRoundTripsAwkwardPaths covers the rest of the characters that appear
// in real repository names.
func TestURIRoundTripsAwkwardPaths(t *testing.T) {
	for _, p := range []string{
		"/tmp/a b/c.go",
		"/tmp/über/main.rs",
		"/tmp/100%/x.ts",
		"/tmp/a#b/y.py",
		"/tmp/a?b/z.js",
		"/tmp/plain/ok.md",
	} {
		if back := PathFromURI(URI(p)); back != p {
			t.Errorf("round trip %q -> %q -> %q", p, URI(p), back)
		}
	}
}

// TestUTF16ColumnsForNonASCII is the subtle one. LSP counts UTF-16 code units
// and the editor counts runes; they agree for ASCII, so this only breaks once a
// line contains an emoji or CJK text — and then every column past it is wrong.
func TestUTF16ColumnsForNonASCII(t *testing.T) {
	cases := []struct {
		line    string
		runeCol int
		utf16   int
	}{
		{"hello", 3, 3}, // ASCII: identical
		{"héllo", 3, 3}, // BMP accent is still one unit
		{"日本語です", 3, 3}, // CJK is BMP: one unit each
		{"a😀b", 2, 3},   // emoji is a surrogate pair: two units
		{"😀😀x", 2, 4},   // two pairs
		{"", 0, 0},      // empty line
		{"abc", 99, 3},  // past the end clamps rather than extrapolating
	}
	for _, c := range cases {
		if got := RuneColToUTF16(c.line, c.runeCol); got != c.utf16 {
			t.Errorf("RuneColToUTF16(%q, %d) = %d, want %d", c.line, c.runeCol, got, c.utf16)
		}
	}
	// And back again, for the positions that actually exist.
	for _, c := range cases[:5] {
		if got := UTF16ToRuneCol(c.line, c.utf16); got != c.runeCol {
			t.Errorf("UTF16ToRuneCol(%q, %d) = %d, want %d", c.line, c.utf16, got, c.runeCol)
		}
	}
}

// TestDiagnosticCodeAcceptsStringOrNumber pins a real interoperability trap:
// the spec allows either, and decoding into a concrete type makes every server
// that picked the other one fail to parse — dropping the WHOLE diagnostics
// payload, not just the code field.
func TestDiagnosticCodeAcceptsStringOrNumber(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}},"code":2304,"message":"m"}`, "2304"},
		{`{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}},"code":"no-unused-vars","message":"m"}`, "no-unused-vars"},
		{`{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}},"message":"m"}`, ""},
	} {
		var d Diagnostic
		if err := json.Unmarshal([]byte(tc.body), &d); err != nil {
			t.Fatalf("unmarshal %s: %v", tc.body, err)
		}
		if got := d.CodeString(); got != tc.want {
			t.Errorf("CodeString() = %q, want %q", got, tc.want)
		}
	}
}

// TestHoverAcceptsEveryShape covers the three encodings servers actually use.
// Guessing one and failing on the rest makes hover look broken against half the
// servers people run.
func TestHoverAcceptsEveryShape(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"contents":"plain text"}`, "plain text"},
		{`{"contents":{"kind":"markdown","value":"**bold**"}}`, "**bold**"},
		{`{"contents":["one","two"]}`, "one\ntwo"},
		{`{"contents":[{"kind":"plaintext","value":"a"},"b"]}`, "a\nb"},
		{`null`, ""},
		{`{}`, ""},
	} {
		if got := hoverText(json.RawMessage(tc.body)); got != tc.want {
			t.Errorf("hoverText(%s) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

// TestDefinitionAcceptsEveryShape does the same for go-to-definition, which may
// answer with a Location, an array of them, or LocationLinks whose fields are
// named differently entirely.
func TestDefinitionAcceptsEveryShape(t *testing.T) {
	single := `{"uri":"file:///a.go","range":{"start":{"line":1,"character":2},"end":{"line":1,"character":5}}}`
	if got := locations(json.RawMessage(single)); len(got) != 1 || got[0].Range.Start.Line != 1 {
		t.Errorf("single Location: %+v", got)
	}
	array := `[` + single + `]`
	if got := locations(json.RawMessage(array)); len(got) != 1 {
		t.Errorf("array: %+v", got)
	}
	links := `[{"targetUri":"file:///b.go",
	            "targetRange":{"start":{"line":9,"character":0},"end":{"line":12,"character":0}},
	            "targetSelectionRange":{"start":{"line":9,"character":5},"end":{"line":9,"character":8}}}]`
	got := locations(json.RawMessage(links))
	if len(got) != 1 || got[0].URI != "file:///b.go" {
		t.Fatalf("links: %+v", got)
	}
	// Selection range wins so the jump lands on the symbol, not the whole decl.
	if got[0].Range.Start.Character != 5 {
		t.Errorf("expected targetSelectionRange, got %+v", got[0].Range)
	}
	if l := locations(json.RawMessage(`null`)); l != nil {
		t.Errorf("null should yield nothing, got %+v", l)
	}
}

// TestLanguageIDCoversTheStack keeps didOpen honest: a wrong or missing id is
// how a server ends up ignoring a file entirely.
func TestLanguageIDCoversTheStack(t *testing.T) {
	for path, want := range map[string]string{
		"a.ts": "typescript", "a.tsx": "typescriptreact", "a.js": "javascript",
		"a.go": "go", "a.py": "python", "a.rs": "rust", "a.yml": "yaml",
		"a.json": "json", "a.css": "css", "a.sh": "shellscript",
		"a.unknown": "plaintext", "noext": "plaintext",
	} {
		if got := LanguageID(path); got != want {
			t.Errorf("LanguageID(%q) = %q, want %q", path, got, want)
		}
	}
}

// TestTruncateIsRuneAware makes sure shortening a message for the status bar
// cannot split a multi-byte character into a broken glyph.
func TestTruncateIsRuneAware(t *testing.T) {
	if got := Truncate("日本語です", 3); got != "日本…" {
		t.Errorf("Truncate = %q", got)
	}
	if got := Truncate("short", 99); got != "short" {
		t.Errorf("no-op Truncate = %q", got)
	}
}

// TestFirstLine keeps multi-line diagnostics from wrecking a one-line status bar.
func TestFirstLine(t *testing.T) {
	if got := FirstLine("first\nsecond\nthird"); got != "first" {
		t.Errorf("FirstLine = %q", got)
	}
}

// TestSeverityString covers the deliberate choice that an omitted severity is
// NOT an error: servers may leave it out, and defaulting to Error would paint a
// file red over a hint.
func TestSeverityString(t *testing.T) {
	if SeverityUnset.String() == SeverityError.String() {
		t.Error("an unspecified severity must not read as an error")
	}
	if SeverityWarning.String() != "warning" {
		t.Errorf("got %q", SeverityWarning.String())
	}
}

// TestCompletionItems_BothWireShapes pins the decode. The spec allows a bare
// CompletionItem[] OR a CompletionList{items:[...]}, and servers genuinely
// differ — handling only one silently yields zero completions from half of
// them, which the user reads as "this language has no completion".
func TestCompletionItems_BothWireShapes(t *testing.T) {
	arr := []byte(`[{"label":"alpha","kind":3},{"label":"beta"}]`)
	if got := completionItems(arr); len(got) != 2 || got[0].Label != "alpha" {
		t.Fatalf("bare array decoded to %+v", got)
	}
	list := []byte(`{"isIncomplete":false,"items":[{"label":"gamma","insertText":"gamma()"}]}`)
	got := completionItems(list)
	if len(got) != 1 || got[0].Label != "gamma" || got[0].InsertText != "gamma()" {
		t.Fatalf("CompletionList decoded to %+v", got)
	}
}

// TestCompletionItems_Degrades pins that junk yields nothing rather than a
// panic or a phantom entry.
func TestCompletionItems_Degrades(t *testing.T) {
	for _, in := range []string{"", "null", "{}", `{"items":null}`, `[{"label":""}]`, "not json"} {
		if got := completionItems([]byte(in)); len(got) != 0 {
			t.Errorf("completionItems(%q) = %+v, want empty", in, got)
		}
	}
}

// TestCompletionKindName covers the mapping and its unknown-kind fallback: a
// bare number would mean nothing to a reader, so unknown renders blank.
func TestCompletionKindName(t *testing.T) {
	if CompletionKindName(3) != "func" {
		t.Errorf("kind 3 = %q, want func", CompletionKindName(3))
	}
	if CompletionKindName(999) != "" {
		t.Errorf("an unknown kind should render blank, got %q", CompletionKindName(999))
	}
}
