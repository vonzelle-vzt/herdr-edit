// =============================================================================
// File: internal/lsp/client_test.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-07-29
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// The client is tested against a real subprocess speaking real LSP framing,
// using the standard Go trick of re-executing the test binary in a helper mode.
// A hand-rolled in-process fake would skip exactly the parts most likely to be
// wrong — Content-Length framing, the pipe plumbing, and the handshake — and
// those are the parts that decide whether any of this works against tsserver.

const helperEnv = "SPICEEDIT_LSP_FAKE_SERVER"

// fakeServerArgv re-runs this test binary in helper mode.
func fakeServerArgv(mode string) []string {
	return []string{os.Args[0], "-test.run=TestHelperProcess", "--", mode}
}

func withHelper(t *testing.T, mode string) []string {
	t.Helper()
	t.Setenv(helperEnv, mode)
	return fakeServerArgv(mode)
}

// TestHelperProcess is not a real test. It is the fake language server, and it
// only does anything when the environment marker is set.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv(helperEnv)
	if mode == "" {
		return
	}
	runFakeServer(mode)
	os.Exit(0)
}

// runFakeServer speaks just enough LSP to exercise the client.
func runFakeServer(mode string) {
	in := bufio.NewReaderSize(os.Stdin, 64*1024)
	write := func(v interface{}) {
		body, _ := json.Marshal(v)
		fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(body), body)
	}

	for {
		body, err := readMessage(in)
		if err != nil {
			return
		}
		var req struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(body, &req) != nil {
			continue
		}

		switch req.Method {
		case "initialize":
			if mode == "no-initialize" {
				continue // deliberately never answer, to exercise the timeout
			}
			write(map[string]interface{}{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]interface{}{"capabilities": map[string]interface{}{}},
			})

		case "textDocument/didOpen":
			// Publish one error against whatever was opened, the way a real
			// server does once it has parsed the document.
			var p didOpenParams
			_ = json.Unmarshal(req.Params, &p)
			write(map[string]interface{}{
				"jsonrpc": "2.0",
				"method":  "textDocument/publishDiagnostics",
				"params": publishDiagnosticsParams{
					URI: p.TextDocument.URI,
					Diagnostics: []Diagnostic{{
						Range:    Range{Start: Position{Line: 2, Character: 4}, End: Position{Line: 2, Character: 9}},
						Severity: SeverityError,
						Code:     json.RawMessage(`2304`),
						Source:   "fake",
						Message:  "Cannot find name 'oops'.",
					}},
				},
			})

		case "textDocument/hover":
			write(map[string]interface{}{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]interface{}{
					"contents": map[string]string{"kind": "markdown", "value": "const x: number"},
				},
			})

		case "textDocument/definition":
			write(map[string]interface{}{
				"jsonrpc": "2.0", "id": req.ID,
				"result": []map[string]interface{}{{
					"uri":   "file:///defined/here.ts",
					"range": map[string]interface{}{"start": map[string]int{"line": 7, "character": 1}, "end": map[string]int{"line": 7, "character": 5}},
				}},
			})

		case "workspace/symbol":
			if mode == "workspace-symbol-error" {
				write(map[string]interface{}{
					"jsonrpc": "2.0", "id": req.ID,
					"error": map[string]interface{}{"code": -32601, "message": "boom"},
				})
				continue
			}
			write(map[string]interface{}{
				"jsonrpc": "2.0", "id": req.ID,
				"result": []map[string]interface{}{{
					"name": "FakeHandler",
					"kind": 12,
					"location": map[string]interface{}{
						"uri":   "file:///proj/handler.ts",
						"range": map[string]interface{}{"start": map[string]int{"line": 3, "character": 0}, "end": map[string]int{"line": 3, "character": 11}},
					},
				}},
			})

		case "shutdown":
			write(map[string]interface{}{"jsonrpc": "2.0", "id": req.ID, "result": nil})
		case "exit":
			return
		}
	}
}

// TestHandshakeAndDiagnostics is the end-to-end path that matters: start a
// server, open a document, and receive a diagnostic through the callback.
func TestHandshakeAndDiagnostics(t *testing.T) {
	argv := withHelper(t, "ok")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	c, err := Start(ctx, "fake", argv, URI(t.TempDir()))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer c.Stop()

	got := make(chan []Diagnostic, 1)
	c.OnDiagnostics = func(uri string, d []Diagnostic) { got <- d }

	if err := c.DidOpen(URI("/proj/app.ts"), "typescript", "let x = oops\n"); err != nil {
		t.Fatal(err)
	}

	select {
	case diags := <-got:
		if len(diags) != 1 {
			t.Fatalf("expected 1 diagnostic, got %d", len(diags))
		}
		d := diags[0]
		if d.Severity != SeverityError || d.Range.Start.Line != 2 || d.CodeString() != "2304" {
			t.Fatalf("unexpected diagnostic %+v", d)
		}
		if !strings.Contains(d.Message, "Cannot find name") {
			t.Fatalf("message = %q", d.Message)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no diagnostics published")
	}
}

// TestVersionsAreMonotonic guards a bug whose only symptom is stale squiggles:
// a server that receives a repeated or out-of-order version discards the edit.
func TestVersionsAreMonotonic(t *testing.T) {
	argv := withHelper(t, "ok")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := Start(ctx, "fake", argv, URI(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Stop()

	uri := URI("/proj/a.ts")
	_ = c.DidOpen(uri, "typescript", "one")
	for i := 0; i < 5; i++ {
		if err := c.DidChange(uri, "text"); err != nil {
			t.Fatal(err)
		}
	}
	c.mu.Lock()
	v := c.docs[uri]
	c.mu.Unlock()
	if v != 6 { // 1 from didOpen + 5 changes
		t.Fatalf("version = %d, want 6", v)
	}

	// An unopened document must not invent a version.
	if err := c.DidChange(URI("/never/opened.ts"), "x"); err != nil {
		t.Fatalf("DidChange on unopened doc should be a no-op, got %v", err)
	}
}

// TestHoverAndDefinitionRoundTrip exercises the request/response path rather
// than just the decoders.
func TestHoverAndDefinitionRoundTrip(t *testing.T) {
	argv := withHelper(t, "ok")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := Start(ctx, "fake", argv, URI(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Stop()

	text, err := c.Hover(ctx, URI("/proj/a.ts"), Position{Line: 1, Character: 1})
	if err != nil || text != "const x: number" {
		t.Fatalf("Hover = %q, %v", text, err)
	}

	locs, err := c.Definition(ctx, URI("/proj/a.ts"), Position{Line: 1, Character: 1})
	if err != nil || len(locs) != 1 {
		t.Fatalf("Definition = %+v, %v", locs, err)
	}
	if p := PathFromURI(locs[0].URI); p != "/defined/here.ts" {
		t.Fatalf("definition path = %q", p)
	}
}

// TestClient_WorkspaceSymbol exercises the request/response path for
// workspace/symbol, which — unlike documentSymbol — is a project-wide search
// with no file argument at all.
func TestClient_WorkspaceSymbol(t *testing.T) {
	argv := withHelper(t, "ok")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := Start(ctx, "fake", argv, URI(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Stop()

	syms, err := c.WorkspaceSymbol(ctx, "Handler")
	if err != nil {
		t.Fatalf("WorkspaceSymbol: %v", err)
	}
	if len(syms) != 1 {
		t.Fatalf("got %d symbols, want 1", len(syms))
	}
	if syms[0].Name != "FakeHandler" || syms[0].URI != "file:///proj/handler.ts" || syms[0].Line != 3 {
		t.Errorf("symbol = %+v", syms[0])
	}
}

// TestMissingBinaryFailsFast is the ordinary case for most users: a server that
// is simply not installed must return an error promptly, never hang the editor.
func TestMissingBinaryFailsFast(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err := Start(ctx, "nope", []string{"definitely-not-a-real-language-server-xyz"}, "file:///tmp")
	if err == nil {
		t.Fatal("expected an error for a missing binary")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("took %v; a missing binary must fail fast", time.Since(start))
	}
}

// TestUnresponsiveServerTimesOut covers the other half: a server that starts but
// never answers initialize must not wedge startup forever.
func TestUnresponsiveServerTimesOut(t *testing.T) {
	argv := withHelper(t, "no-initialize")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := Start(ctx, "silent", argv, "file:///tmp")
	if err == nil {
		t.Fatal("expected initialize to time out")
	}
}

// TestReadMessageFraming pins the wire format directly, including the header
// case-insensitivity servers vary on and the unrecoverable missing-length case.
func TestReadMessageFraming(t *testing.T) {
	body := `{"jsonrpc":"2.0"}`
	in := bufio.NewReader(strings.NewReader(
		"Content-Length: " + fmt.Sprint(len(body)) + "\r\nContent-Type: application/vscode-jsonrpc\r\n\r\n" + body))
	got, err := readMessage(in)
	if err != nil || string(got) != body {
		t.Fatalf("readMessage = %q, %v", got, err)
	}

	lower := bufio.NewReader(strings.NewReader("content-length: 2\r\n\r\n{}"))
	if got, err := readMessage(lower); err != nil || string(got) != "{}" {
		t.Fatalf("lowercase header: %q, %v", got, err)
	}

	// Without a length there is no way to find the next message, so this must be
	// an error rather than a silent resync attempt.
	if _, err := readMessage(bufio.NewReader(strings.NewReader("X: 1\r\n\r\n{}"))); err == nil {
		t.Fatal("expected an error when Content-Length is absent")
	}
}

// TestStopIsIdempotent matters because quit paths call it more than once.
func TestStopIsIdempotent(t *testing.T) {
	argv := withHelper(t, "ok")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := Start(ctx, "fake", argv, URI(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	c.Stop()
	c.Stop() // must not panic or block
}
