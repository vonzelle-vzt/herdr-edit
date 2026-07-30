// =============================================================================
// File: internal/lsp/protocol.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-07-29
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

// Package lsp is a minimal Language Server Protocol client: enough to get
// diagnostics, hover and go-to-definition out of the servers people already
// have installed, and no more.
//
// This is the one thing a terminal editor really cannot fake. Everything else
// VS Code users miss — a file tree, tabs, git colouring, format-on-save,
// search — is a shell command away. Squiggles under the actual mistake are
// not: they need a live process that has parsed your project.
//
// Scope is deliberately narrow. We speak just enough of the protocol to be a
// useful client and we own only the types we use, rather than pulling in a
// full protocol package — the editor ships as one static binary with no CGO
// and three dependencies, and that is worth protecting.
package lsp

import "encoding/json"

// Position is zero-based, as LSP defines it. The editor's own Position is also
// zero-based, so the two line up — but LSP counts a line's characters in UTF-16
// code units while the editor counts runes, which differ for anything outside
// the BMP. Conversion happens at the boundary in convert.go, never here.
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Range is a half-open span: End is exclusive.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Severity levels as defined by the protocol. The zero value is deliberately
// not a valid severity: servers are allowed to omit the field, and treating
// "unspecified" as Error would paint a file red over a hint.
type Severity int

const (
	SeverityUnset       Severity = 0
	SeverityError       Severity = 1
	SeverityWarning     Severity = 2
	SeverityInformation Severity = 3
	SeverityHint        Severity = 4
)

// String gives the short label used in the gutter and the problems list.
func (s Severity) String() string {
	switch s {
	case SeverityError:
		return "error"
	case SeverityWarning:
		return "warning"
	case SeverityInformation:
		return "info"
	case SeverityHint:
		return "hint"
	}
	return "diagnostic"
}

// Diagnostic is one problem reported against a document.
type Diagnostic struct {
	Range    Range           `json:"range"`
	Severity Severity        `json:"severity,omitempty"`
	Code     json.RawMessage `json:"code,omitempty"` // string OR number, per spec
	Source   string          `json:"source,omitempty"`
	Message  string          `json:"message"`
}

// CodeString renders Code for display. It is deliberately json.RawMessage
// above because the spec allows either a string or a number, and decoding into
// a concrete type makes every server that chose the other one fail to parse —
// which would silently drop the whole diagnostics payload, not just the code.
func (d Diagnostic) CodeString() string {
	if len(d.Code) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(d.Code, &s); err == nil {
		return s
	}
	var n json.Number
	if err := json.Unmarshal(d.Code, &n); err == nil {
		// json.Number keeps the literal text, so an integral code prints as "2304" rather than
		// the "2304.0" a float64 round-trip would produce.
		return n.String()
	}
	return string(d.Code)
}

// publishDiagnosticsParams is the server-initiated notification we care most
// about: it is how every diagnostic reaches us.
type publishDiagnosticsParams struct {
	URI         string       `json:"uri"`
	Version     *int         `json:"version,omitempty"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// MarkupContent is hover text, which servers send either as a plain string, as
// {kind,value}, or as an array of either. See hoverText for the unwrapping.
type MarkupContent struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// Location points at a place in a document — the answer to go-to-definition.
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// textDocumentIdentifier / versionedTextDocumentIdentifier name a document in
// requests. Version matters: a server that receives edits out of order will
// report diagnostics against text the user no longer has.
type textDocumentIdentifier struct {
	URI string `json:"uri"`
}

type versionedTextDocumentIdentifier struct {
	URI     string `json:"uri"`
	Version int    `json:"version"`
}

type textDocumentItem struct {
	URI        string `json:"uri"`
	LanguageID string `json:"languageId"`
	Version    int    `json:"version"`
	Text       string `json:"text"`
}

type didOpenParams struct {
	TextDocument textDocumentItem `json:"textDocument"`
}

type didCloseParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
}

// contentChange carries a whole-document replacement. The protocol also allows
// incremental ranges, but full-text sync is what every server must support and
// it removes an entire class of desync bug for a cost that does not matter at
// the file sizes this editor targets.
type contentChange struct {
	Text string `json:"text"`
}

type didChangeParams struct {
	TextDocument   versionedTextDocumentIdentifier `json:"textDocument"`
	ContentChanges []contentChange                 `json:"contentChanges"`
}

type didSaveParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
	Text         string                 `json:"text,omitempty"`
}

type positionParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
}
