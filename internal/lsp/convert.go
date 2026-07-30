// =============================================================================
// File: internal/lsp/convert.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-07-29
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

package lsp

import (
	"encoding/json"
	"net/url"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Everything in this file exists because LSP and the editor disagree about two
// things: how to name a file, and how to count along a line. Both disagreements
// are silent when they go wrong — a squiggle simply lands in the wrong place,
// or a file's diagnostics never match any open tab — so the conversions are
// isolated here and tested directly.

// URI turns an absolute path into a file:// URI.
//
// Path escaping is not optional: a repo under "~/my projects/" produces a URI
// the server will not match against the one it sends back, and the symptom is
// diagnostics that silently belong to no open document. Windows also needs the
// extra leading slash before its drive letter.
func URI(path string) string {
	p := filepath.ToSlash(path)
	if runtime.GOOS == "windows" && len(p) > 1 && p[1] == ':' {
		p = "/" + p
	}
	u := url.URL{Scheme: "file", Path: p}
	return u.String()
}

// PathFromURI is URI in reverse, for turning a definition result back into
// something the editor can open. A URI we cannot parse yields "", which callers
// treat as "no result" rather than opening a nonsense path.
func PathFromURI(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return ""
	}
	p := u.Path
	if runtime.GOOS == "windows" {
		p = strings.TrimPrefix(p, "/")
		p = filepath.FromSlash(p)
	}
	return p
}

// RuneColToUTF16 converts a rune index within a line to the UTF-16 code-unit
// offset the protocol expects.
//
// LSP measures a line's characters in UTF-16 code units; this editor stores and
// indexes runes. For ASCII the two are identical, which is exactly why getting
// it wrong survives testing — it only breaks once someone has an emoji or a CJK
// name above the cursor, and then every column on that line is off.
func RuneColToUTF16(line string, runeCol int) int {
	if runeCol <= 0 {
		return 0
	}
	units, seen := 0, 0
	for _, r := range line {
		if seen >= runeCol {
			break
		}
		units += utf16Len(r)
		seen++
	}
	if seen < runeCol {
		// Past the end of the line: clamp rather than extrapolate. A column that
		// does not exist is better reported as end-of-line than as a position the
		// server will reject.
		return units
	}
	return units
}

// UTF16ToRuneCol converts a protocol column back to a rune index.
func UTF16ToRuneCol(line string, utf16Col int) int {
	if utf16Col <= 0 {
		return 0
	}
	units, runes := 0, 0
	for _, r := range line {
		if units >= utf16Col {
			break
		}
		units += utf16Len(r)
		runes++
	}
	return runes
}

// utf16Len is how many UTF-16 code units a rune occupies: two for anything
// outside the Basic Multilingual Plane, one otherwise.
func utf16Len(r rune) int {
	if r > 0xFFFF {
		return len(utf16.Encode([]rune{r}))
	}
	return 1
}

// LanguageID maps a file extension to the identifier servers expect in
// didOpen. Servers use it to decide whether they care about a document at all,
// so a wrong or missing id shows up as "the server ignores my file".
func LanguageID(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".ts":
		return "typescript"
	case ".tsx":
		return "typescriptreact"
	case ".js", ".mjs", ".cjs":
		return "javascript"
	case ".jsx":
		return "javascriptreact"
	case ".go":
		return "go"
	case ".py":
		return "python"
	case ".rs":
		return "rust"
	case ".rb":
		return "ruby"
	case ".java":
		return "java"
	case ".c", ".h":
		return "c"
	case ".cc", ".cpp", ".cxx", ".hpp":
		return "cpp"
	case ".cs":
		return "csharp"
	case ".swift":
		return "swift"
	case ".css":
		return "css"
	case ".scss":
		return "scss"
	case ".less":
		return "less"
	case ".html", ".htm":
		return "html"
	case ".json":
		return "json"
	case ".jsonc":
		return "jsonc"
	case ".yaml", ".yml":
		return "yaml"
	case ".toml":
		return "toml"
	case ".md", ".markdown":
		return "markdown"
	case ".sh", ".bash", ".zsh":
		return "shellscript"
	case ".sql":
		return "sql"
	case ".vue":
		return "vue"
	case ".svelte":
		return "svelte"
	case ".lua":
		return "lua"
	case ".php":
		return "php"
	}
	return "plaintext"
}

// hoverText unwraps the three shapes a server may answer hover with: a bare
// string, a {kind,value} object, or an array mixing both. Guessing one shape
// and failing on the others would make hover appear broken against half the
// servers people actually run.
func hoverText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var wrapper struct {
		Contents json.RawMessage `json:"contents"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil || len(wrapper.Contents) == 0 {
		return ""
	}
	return markupToString(wrapper.Contents)
}

// markupToString renders any of the MarkupContent shapes to plain text.
func markupToString(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var mc MarkupContent
	if err := json.Unmarshal(raw, &mc); err == nil && mc.Value != "" {
		return strings.TrimSpace(mc.Value)
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err == nil {
		parts := make([]string, 0, len(list))
		for _, item := range list {
			if s := markupToString(item); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	}
	return ""
}

// locations unwraps a definition result, which may be a single Location, an
// array of Locations, or an array of LocationLinks (which name the target with
// different field names entirely).
func locations(raw json.RawMessage) []Location {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var one Location
	if err := json.Unmarshal(raw, &one); err == nil && one.URI != "" {
		return []Location{one}
	}
	var many []Location
	if err := json.Unmarshal(raw, &many); err == nil && len(many) > 0 && many[0].URI != "" {
		return many
	}
	var links []struct {
		TargetURI            string `json:"targetUri"`
		TargetSelectionRange Range  `json:"targetSelectionRange"`
		TargetRange          Range  `json:"targetRange"`
	}
	if err := json.Unmarshal(raw, &links); err == nil {
		out := make([]Location, 0, len(links))
		for _, l := range links {
			if l.TargetURI == "" {
				continue
			}
			// Prefer the selection range: it points at the symbol itself rather
			// than the whole declaration, so jumping lands on the name.
			rng := l.TargetSelectionRange
			if rng == (Range{}) {
				rng = l.TargetRange
			}
			out = append(out, Location{URI: l.TargetURI, Range: rng})
		}
		return out
	}
	return nil
}

// FirstLine is a small helper for the status bar: diagnostics are often
// multi-line and only the first line fits.
func FirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// Truncate shortens a string to n runes, appending an ellipsis. Rune-aware so
// it cannot split a multi-byte character and produce a broken glyph.
func Truncate(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	if n <= 1 {
		return string(runes[:n])
	}
	return string(runes[:n-1]) + "…"
}

// CompletionItem is one suggestion. Only the fields this editor can actually
// use are decoded: a label to show, the text to insert when it differs, a kind
// for the little type hint, and detail for the right-hand column.
type CompletionItem struct {
	Label      string
	InsertText string
	Detail     string
	Kind       int
	SortText   string
}

// Text returns what should actually be typed into the buffer. Servers often
// send a label decorated for display — "foo(…)" or "bar (deprecated)" — and a
// clean insertText beside it; inserting the label in that case puts punctuation
// into the user's code.
func (c CompletionItem) Text() string {
	if c.InsertText != "" {
		return c.InsertText
	}
	return c.Label
}

// completionItems unwraps a textDocument/completion result. The spec allows
// either a bare CompletionItem[] or a CompletionList{items:[...]}, and servers
// genuinely differ — decoding only one shape silently yields zero completions
// from half of them, which reads as "this language has no completion".
func completionItems(raw json.RawMessage) []CompletionItem {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	type wireItem struct {
		Label      string          `json:"label"`
		InsertText string          `json:"insertText"`
		Detail     string          `json:"detail"`
		Kind       int             `json:"kind"`
		SortText   string          `json:"sortText"`
		TextEdit   json.RawMessage `json:"textEdit"`
	}
	var items []wireItem
	if err := json.Unmarshal(raw, &items); err != nil {
		var list struct {
			Items []wireItem `json:"items"`
		}
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil
		}
		items = list.Items
	}
	out := make([]CompletionItem, 0, len(items))
	for _, it := range items {
		if it.Label == "" {
			continue
		}
		out = append(out, CompletionItem{
			Label:      it.Label,
			InsertText: it.InsertText,
			Detail:     it.Detail,
			Kind:       it.Kind,
			SortText:   it.SortText,
		})
	}
	return out
}

// CompletionKindName maps the spec's numeric CompletionItemKind onto a short
// label. Unknown kinds render blank rather than as a bare number, which would
// mean nothing to a reader.
func CompletionKindName(kind int) string {
	switch kind {
	case 1:
		return "text"
	case 2:
		return "method"
	case 3:
		return "func"
	case 4:
		return "ctor"
	case 5:
		return "field"
	case 6:
		return "var"
	case 7:
		return "class"
	case 8:
		return "iface"
	case 9:
		return "module"
	case 10:
		return "prop"
	case 12:
		return "value"
	case 13:
		return "enum"
	case 14:
		return "keyword"
	case 15:
		return "snippet"
	case 17:
		return "file"
	case 21:
		return "const"
	case 22:
		return "struct"
	case 25:
		return "type"
	default:
		return ""
	}
}

// workspaceEdits unwraps a textDocument/rename result into path -> edits.
//
// The spec offers TWO ways to say the same thing: a flat `changes` map, and a
// `documentChanges` array carrying versioned document identifiers. Servers pick
// per implementation — gopls prefers documentChanges, others send changes — so
// decoding only one yields "rename did nothing" against half of them, with no
// error to explain it.
//
// Edits are returned in DESCENDING position order per file. Applying them in
// document order would invalidate every later range as soon as the first edit
// changed the text length; walking backwards keeps every remaining range valid
// without re-deriving anything. Same reason ReplaceAll walks its matches
// last-to-first.
func workspaceEdits(raw json.RawMessage) map[string][]TextEdit {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	type wireEdit struct {
		Range   Range  `json:"range"`
		NewText string `json:"newText"`
	}
	var payload struct {
		Changes         map[string][]wireEdit `json:"changes"`
		DocumentChanges []struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
			Edits []wireEdit `json:"edits"`
		} `json:"documentChanges"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}

	out := map[string][]TextEdit{}
	add := func(uri string, edits []wireEdit) {
		path := PathFromURI(uri)
		if path == "" {
			return
		}
		for _, e := range edits {
			out[path] = append(out[path], TextEdit{Range: e.Range, NewText: e.NewText})
		}
	}
	for uri, edits := range payload.Changes {
		add(uri, edits)
	}
	for _, dc := range payload.DocumentChanges {
		add(dc.TextDocument.URI, dc.Edits)
	}

	for path := range out {
		edits := out[path]
		sort.SliceStable(edits, func(i, j int) bool {
			if edits[i].Range.Start.Line != edits[j].Range.Start.Line {
				return edits[i].Range.Start.Line > edits[j].Range.Start.Line
			}
			return edits[i].Range.Start.Character > edits[j].Range.Start.Character
		})
		out[path] = edits
	}
	return out
}
