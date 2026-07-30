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

// TestWorkspaceEdits_BothWireShapes pins the decode. The spec offers a flat
// `changes` map AND a `documentChanges` array; servers pick per implementation
// (gopls prefers documentChanges), so handling only one makes rename silently
// do nothing against half of them, with no error to explain it.
func TestWorkspaceEdits_BothWireShapes(t *testing.T) {
	flat := []byte(`{"changes":{"file:///tmp/a.go":[{"range":{"start":{"line":1,"character":2},"end":{"line":1,"character":5}},"newText":"neo"}]}}`)
	got := workspaceEdits(flat)
	if len(got["/tmp/a.go"]) != 1 || got["/tmp/a.go"][0].NewText != "neo" {
		t.Fatalf("flat changes decoded to %+v", got)
	}

	docs := []byte(`{"documentChanges":[{"textDocument":{"uri":"file:///tmp/b.go","version":3},"edits":[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":3}},"newText":"x"}]}]}`)
	got = workspaceEdits(docs)
	if len(got["/tmp/b.go"]) != 1 || got["/tmp/b.go"][0].NewText != "x" {
		t.Fatalf("documentChanges decoded to %+v", got)
	}
}

// TestWorkspaceEdits_DescendingOrder is the property that makes applying them
// safe. Editing a line front-to-back shifts every later range; walking
// backwards keeps each remaining range valid with no offset bookkeeping.
func TestWorkspaceEdits_DescendingOrder(t *testing.T) {
	raw := []byte(`{"changes":{"file:///tmp/c.go":[
      {"range":{"start":{"line":1,"character":1},"end":{"line":1,"character":2}},"newText":"a"},
      {"range":{"start":{"line":5,"character":0},"end":{"line":5,"character":1}},"newText":"b"},
      {"range":{"start":{"line":1,"character":9},"end":{"line":1,"character":10}},"newText":"c"}]}}`)
	edits := workspaceEdits(raw)["/tmp/c.go"]
	if len(edits) != 3 {
		t.Fatalf("got %d edits", len(edits))
	}
	for i := 1; i < len(edits); i++ {
		prev, cur := edits[i-1].Range.Start, edits[i].Range.Start
		if prev.Line < cur.Line || (prev.Line == cur.Line && prev.Character < cur.Character) {
			t.Fatalf("edits are not descending: %+v", edits)
		}
	}
}

// TestWorkspaceEdits_Degrades pins that junk yields nothing rather than a panic
// or a half-decoded edit that would corrupt a file.
func TestWorkspaceEdits_Degrades(t *testing.T) {
	for _, in := range []string{"", "null", "{}", "not json", `{"changes":{}}`} {
		if got := workspaceEdits([]byte(in)); len(got) != 0 {
			t.Errorf("workspaceEdits(%q) = %+v, want empty", in, got)
		}
	}
}

// TestSymbols_BothWireShapes pins the decode. documentSymbol allows a nested
// DocumentSymbol[] AND a flat SymbolInformation[]; servers genuinely differ, and
// handling one leaves the outline empty against the other — which reads as "this
// language has no symbols" rather than as a missing branch.
func TestSymbols_BothWireShapes(t *testing.T) {
	nested := []byte(`[{"name":"Outer","kind":5,"selectionRange":{"start":{"line":4,"character":0},"end":{"line":4,"character":5}},
	   "children":[{"name":"inner","kind":6,"selectionRange":{"start":{"line":7,"character":2},"end":{"line":7,"character":7}}}]}]`)
	got := symbols(nested)
	if len(got) != 2 {
		t.Fatalf("nested decoded to %d symbols, want 2", len(got))
	}
	if got[0].Name != "Outer" || got[0].Line != 4 || got[0].Depth != 0 {
		t.Errorf("parent = %+v", got[0])
	}
	if got[1].Name != "inner" || got[1].Line != 7 || got[1].Depth != 1 {
		t.Errorf("child = %+v (depth must indent the outline)", got[1])
	}

	flat := []byte(`[{"name":"Legacy","kind":12,"location":{"range":{"start":{"line":9,"character":0},"end":{"line":9,"character":6}}}}]`)
	got = symbols(flat)
	if len(got) != 1 || got[0].Name != "Legacy" || got[0].Line != 9 {
		t.Fatalf("flat decoded to %+v", got)
	}
}

// TestSymbols_Degrades pins junk yields nothing rather than a phantom entry.
func TestSymbols_Degrades(t *testing.T) {
	for _, in := range []string{"", "null", "[]", "not json", `[{"name":""}]`} {
		if got := symbols([]byte(in)); len(got) != 0 {
			t.Errorf("symbols(%q) = %+v, want empty", in, got)
		}
	}
}

// TestSignatureText_BothLabelForms pins the parameter marker. A parameter label
// is EITHER a string or a [start,end] offset pair into the signature; assuming
// the string form silently drops the marker against servers using offsets.
func TestSignatureText_BothLabelForms(t *testing.T) {
	str := []byte(`{"signatures":[{"label":"func f(a int, b string)","parameters":[{"label":"a int"},{"label":"b string"}]}],"activeSignature":0,"activeParameter":1}`)
	if got := signatureText(str); got != "func f(a int, b string)   ← b string" {
		t.Errorf("string labels gave %q", got)
	}

	off := []byte(`{"signatures":[{"label":"func f(a int, b string)","parameters":[{"label":[7,12]},{"label":[14,22]}]}],"activeSignature":0,"activeParameter":0}`)
	got := signatureText(off)
	if got != "func f(a int, b string)   ← a int" {
		t.Errorf("offset labels gave %q", got)
	}
}

// TestSignatureText_SilentWhenNotInACall pins the common case: outside a call
// the server answers empty and the UI must stay quiet.
func TestSignatureText_SilentWhenNotInACall(t *testing.T) {
	for _, in := range []string{"", "null", `{"signatures":[]}`, `{"signatures":[{"label":""}]}`} {
		if got := signatureText([]byte(in)); got != "" {
			t.Errorf("signatureText(%q) = %q, want empty", in, got)
		}
	}
}

// TestCodeActions_DropsUnrunnable is the honesty guard. A server may answer with
// a Command rather than edits, and running one needs workspace/executeCommand
// which this editor does not implement — offering it would be a menu entry that
// silently does nothing.
func TestCodeActions_DropsUnrunnable(t *testing.T) {
	raw := []byte(`[
	  {"title":"Has edits","kind":"quickfix","edit":{"changes":{"file:///tmp/a.go":[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}},"newText":"x"}]}}},
	  {"title":"Command only","kind":"quickfix","command":{"command":"do.thing"}},
	  {"title":"Empty edit","kind":"quickfix","edit":{"changes":{}}}]`)
	got := codeActions(raw)
	if len(got) != 1 {
		t.Fatalf("kept %d actions, want only the runnable one: %+v", len(got), got)
	}
	if got[0].Title != "Has edits" {
		t.Errorf("kept the wrong action: %+v", got[0])
	}
}

// TestSymbolKindName covers the mapping and its blank fallback.
func TestSymbolKindName(t *testing.T) {
	if SymbolKindName(12) != "func" {
		t.Errorf("kind 12 = %q, want func", SymbolKindName(12))
	}
	if SymbolKindName(999) != "" {
		t.Errorf("unknown kind should be blank, got %q", SymbolKindName(999))
	}
}
