// =============================================================================
// File: internal/lsp/protocol_test.go
// Author: Vonzelle Brown
// Created: 2026-11-20
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package lsp

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCodeActionParams_WireShape smoke-tests the request spelling: servers
// match these JSON keys exactly, so a renamed tag fails silently as "no fixes".
func TestCodeActionParams_WireShape(t *testing.T) {
	b, err := json.Marshal(codeActionParams{
		TextDocument: textDocumentIdentifier{URI: "file:///a.go"},
		Range:        Range{Start: Position{Line: 1, Character: 2}, End: Position{Line: 1, Character: 3}},
		Context:      codeActionContext{Diagnostics: []Diagnostic{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"textDocument"`, `"range"`, `"context"`, `"diagnostics":[]`, `"uri":"file:///a.go"`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("wire form %s is missing %s", b, key)
		}
	}
}

// TestCodeAction_ZeroValueIsRunnable pins the Unavailable contract: an action
// is runnable exactly when Unavailable is empty, so the zero value of the field
// must mean "available" and nothing else may be consulted.
func TestCodeAction_ZeroValueIsRunnable(t *testing.T) {
	if (CodeAction{Title: "x"}).Unavailable != "" {
		t.Fatal("a fresh CodeAction must not read as unavailable")
	}
}

// TestDiagnostic_DataRoundTrips pins that a diagnostic's opaque `data` survives
// decode and re-encode, because codeAction's context must echo it back.
func TestDiagnostic_DataRoundTrips(t *testing.T) {
	var d Diagnostic
	if err := json.Unmarshal([]byte(`{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}},"message":"m","data":{"fix":42}}`), &d); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(d)
	if !strings.Contains(string(b), `"data":{"fix":42}`) {
		t.Fatalf("data was lost: %s", b)
	}
}
