// =============================================================================
// File: internal/lsp/client.go
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
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client is one language server, spoken to over its stdin/stdout.
//
// The editor is single-threaded around a tcell event loop, so the rule here is
// that nothing in this package ever blocks that loop. Requests are issued from
// a background goroutine and answers arrive through a callback the app turns
// into a tcell event; a server that hangs, crashes, or was never installed
// costs the user nothing but the absence of squiggles.
type Client struct {
	name string
	cmd  *exec.Cmd

	stdin  io.WriteCloser
	stdout *bufio.Reader

	mu       sync.Mutex
	nextID   int
	pending  map[int]chan rpcResponse
	docs     map[string]int // uri -> version, so didChange is never sent out of order
	closed   bool
	initDone bool

	// OnDiagnostics is called from a background goroutine whenever the server
	// publishes. The app must not touch UI state here — it should post a tcell
	// event instead, which is the same discipline the tree refresh already uses.
	OnDiagnostics func(uri string, diags []Diagnostic)

	// OnLog receives server stderr and protocol-level complaints. Wired to the
	// status line so a misconfigured server says something instead of silently
	// doing nothing — the single most common way an LSP setup "does not work".
	OnLog func(string)
}

type rpcRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      *int        `json:"id,omitempty"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("lsp error %d: %s", e.Code, e.Message) }

// Start launches a server and completes the initialize handshake.
//
// The handshake is synchronous with a timeout because everything else depends
// on it, and a server that never answers initialize is broken in a way we want
// to report immediately rather than discover later through missing diagnostics.
func Start(ctx context.Context, name string, argv []string, rootURI string) (*Client, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("%s: no command configured", name)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}

	c := &Client{
		name:    name,
		cmd:     cmd,
		stdin:   stdin,
		stdout:  bufio.NewReaderSize(stdout, 64*1024),
		pending: make(map[int]chan rpcResponse),
		docs:    make(map[string]int),
	}

	go c.readLoop()
	go c.drainStderr(stderr)

	if err := c.initialize(ctx, rootURI); err != nil {
		c.Stop()
		return nil, err
	}
	return c, nil
}

// Name is the server's configured name, used in status messages.
func (c *Client) Name() string { return c.name }

// initialize performs the required handshake and declares what we support.
//
// We advertise only what we actually implement. Claiming a capability we do not
// honour makes servers send work we ignore, and in the case of dynamic
// registration makes them wait on a reply that never comes.
func (c *Client) initialize(ctx context.Context, rootURI string) error {
	params := map[string]interface{}{
		"processId": nil,
		"rootUri":   rootURI,
		"workspaceFolders": []map[string]string{
			{"uri": rootURI, "name": "root"},
		},
		"capabilities": map[string]interface{}{
			"textDocument": map[string]interface{}{
				"synchronization": map[string]interface{}{
					"didSave":  true,
					"willSave": false,
				},
				"publishDiagnostics": map[string]interface{}{
					"relatedInformation": false,
				},
				"hover": map[string]interface{}{
					"contentFormat": []string{"plaintext", "markdown"},
				},
				"definition": map[string]interface{}{},
			},
		},
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	if _, err := c.call(ctx, "initialize", params); err != nil {
		return fmt.Errorf("%s: initialize: %w", c.name, err)
	}
	if err := c.notify("initialized", map[string]interface{}{}); err != nil {
		return err
	}
	c.mu.Lock()
	c.initDone = true
	c.mu.Unlock()
	return nil
}

// Stop shuts the server down. Best-effort by design: a language server that
// will not exit must never keep the editor from quitting, so we ask politely
// once and then kill.
func (c *Client) Stop() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _ = c.call(ctx, "shutdown", nil)
	_ = c.notify("exit", nil)

	done := make(chan struct{})
	go func() { _ = c.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = c.cmd.Process.Kill()
	}
	_ = c.stdin.Close()
}

// DidOpen tells the server about a document. Diagnostics for a file generally
// do not arrive until it has been opened, so this is what actually starts the
// flow.
func (c *Client) DidOpen(uri, languageID, text string) error {
	c.mu.Lock()
	c.docs[uri] = 1
	c.mu.Unlock()
	return c.notify("textDocument/didOpen", didOpenParams{
		TextDocument: textDocumentItem{URI: uri, LanguageID: languageID, Version: 1, Text: text},
	})
}

// DidChange sends the full new text of a document.
//
// Version numbers are monotonic per document and assigned here rather than by
// the caller, because a caller that reuses or skips one makes the server
// discard the edit — and the symptom is stale squiggles, which reads as "the
// LSP is broken" rather than "we sent a bad version".
func (c *Client) DidChange(uri, text string) error {
	c.mu.Lock()
	v, ok := c.docs[uri]
	if !ok {
		c.mu.Unlock()
		return nil // never opened; nothing to update
	}
	v++
	c.docs[uri] = v
	c.mu.Unlock()

	return c.notify("textDocument/didChange", didChangeParams{
		TextDocument:   versionedTextDocumentIdentifier{URI: uri, Version: v},
		ContentChanges: []contentChange{{Text: text}},
	})
}

// DidSave notifies the server a document was written to disk. Some servers
// (eslint, and anything that shells out to a linter) only run their full
// analysis on save.
func (c *Client) DidSave(uri, text string) error {
	return c.notify("textDocument/didSave", didSaveParams{
		TextDocument: textDocumentIdentifier{URI: uri},
		Text:         text,
	})
}

// DidClose stops tracking a document.
func (c *Client) DidClose(uri string) error {
	c.mu.Lock()
	delete(c.docs, uri)
	c.mu.Unlock()
	return c.notify("textDocument/didClose", didCloseParams{
		TextDocument: textDocumentIdentifier{URI: uri},
	})
}

// Hover asks what is under the cursor. Returns "" when the server has nothing
// to say, which is an ordinary answer rather than an error.
func (c *Client) Hover(ctx context.Context, uri string, pos Position) (string, error) {
	raw, err := c.call(ctx, "textDocument/hover", positionParams{
		TextDocument: textDocumentIdentifier{URI: uri},
		Position:     pos,
	})
	if err != nil {
		return "", err
	}
	return hoverText(raw), nil
}

// Definition resolves go-to-definition. Servers may answer with a single
// Location, an array of them, or LocationLinks; all three shapes are unwrapped
// in locations().
func (c *Client) Definition(ctx context.Context, uri string, pos Position) ([]Location, error) {
	raw, err := c.call(ctx, "textDocument/definition", positionParams{
		TextDocument: textDocumentIdentifier{URI: uri},
		Position:     pos,
	})
	if err != nil {
		return nil, err
	}
	return locations(raw), nil
}

// call issues a request and waits for its response or the context deadline.
func (c *Client) call(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	c.mu.Lock()
	if c.closed && method != "shutdown" && method != "exit" {
		c.mu.Unlock()
		return nil, fmt.Errorf("%s: client closed", c.name)
	}
	c.nextID++
	id := c.nextID
	ch := make(chan rpcResponse, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if err := c.write(rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params}); err != nil {
		return nil, err
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-ctx.Done():
		// A slow server must not wedge a request forever. The pending entry is
		// removed by the defer, so a late reply is simply dropped.
		return nil, ctx.Err()
	}
}

// notify sends a request with no id, which the protocol defines as fire and
// forget — there will be no reply and none is expected.
func (c *Client) notify(method string, params interface{}) error {
	return c.write(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}

// write frames one message with the Content-Length header the protocol requires.
func (c *Client) write(req rpcRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := fmt.Fprintf(c.stdin, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	_, err = c.stdin.Write(body)
	return err
}

// readLoop demultiplexes everything the server sends: replies go to whoever is
// waiting, notifications are dispatched.
func (c *Client) readLoop() {
	for {
		body, err := readMessage(c.stdout)
		if err != nil {
			c.failPending(err)
			return
		}
		var resp rpcResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			c.log("malformed message: " + err.Error())
			continue
		}

		// A message carrying a method is server-initiated. One carrying only an
		// id is the answer to something we asked.
		if resp.Method != "" {
			c.dispatch(resp)
			continue
		}
		if resp.ID == nil {
			continue
		}
		c.mu.Lock()
		ch, ok := c.pending[*resp.ID]
		c.mu.Unlock()
		if ok {
			ch <- resp
		}
	}
}

// dispatch handles server-initiated traffic. Unknown notifications are ignored
// on purpose — servers send plenty we have no use for, and logging each one
// would turn the status line into noise.
func (c *Client) dispatch(resp rpcResponse) {
	switch resp.Method {
	case "textDocument/publishDiagnostics":
		var p publishDiagnosticsParams
		if err := json.Unmarshal(resp.Params, &p); err != nil {
			return
		}
		if c.OnDiagnostics != nil {
			c.OnDiagnostics(p.URI, p.Diagnostics)
		}
	case "window/showMessage", "window/logMessage":
		var p struct {
			Type    int    `json:"type"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(resp.Params, &p); err == nil && p.Type <= 2 && p.Message != "" {
			c.log(p.Message) // only Error(1) and Warning(2); Info and Log are chatter
		}
	case "window/workDoneProgress/create", "client/registerCapability",
		"client/unregisterCapability", "workspace/configuration":
		// These are REQUESTS, not notifications: the server blocks waiting for a
		// reply. Answering with an empty result keeps servers that use them from
		// stalling. workspace/configuration wants an array, one entry per item.
		if resp.ID != nil {
			result := interface{}(map[string]interface{}{})
			if resp.Method == "workspace/configuration" {
				result = []interface{}{map[string]interface{}{}}
			}
			_ = c.writeResult(*resp.ID, result)
		}
	}
}

// writeResult answers a server-initiated request.
func (c *Client) writeResult(id int, result interface{}) error {
	body, err := json.Marshal(struct {
		JSONRPC string      `json:"jsonrpc"`
		ID      int         `json:"id"`
		Result  interface{} `json:"result"`
	}{"2.0", id, result})
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := fmt.Fprintf(c.stdin, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	_, err = c.stdin.Write(body)
	return err
}

// failPending unblocks every waiting caller when the connection dies, so a
// crashed server surfaces as an error instead of a hang.
func (c *Client) failPending(err error) {
	c.mu.Lock()
	pending := c.pending
	c.pending = make(map[int]chan rpcResponse)
	closed := c.closed
	c.mu.Unlock()

	for id, ch := range pending {
		ch <- rpcResponse{ID: &id, Error: &rpcError{Code: -1, Message: err.Error()}}
	}
	if !closed && err != io.EOF {
		c.log("connection lost: " + err.Error())
	}
}

// drainStderr keeps the server's stderr from filling its pipe buffer, which
// would block the server itself. Only the first few lines are surfaced: a
// server that is unhappy tends to be unhappy repeatedly.
func (c *Client) drainStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	shown := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if shown < 3 {
			c.log(line)
			shown++
		}
	}
}

func (c *Client) log(msg string) {
	if c.OnLog != nil {
		c.OnLog(c.name + ": " + msg)
	}
}

// readMessage reads one Content-Length framed message.
//
// Header parsing is lenient about everything except Content-Length, because
// that is the only field we need and servers differ on the rest. A missing or
// unparseable length is fatal to the stream: without it there is no way to know
// where the next message begins, so resyncing is not possible.
func readMessage(r *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break // end of headers
		}
		if idx := strings.IndexByte(line, ':'); idx > 0 {
			key := strings.ToLower(strings.TrimSpace(line[:idx]))
			if key == "content-length" {
				n, err := strconv.Atoi(strings.TrimSpace(line[idx+1:]))
				if err != nil {
					return nil, fmt.Errorf("bad Content-Length %q", line)
				}
				length = n
			}
		}
	}
	if length < 0 {
		return nil, fmt.Errorf("message without Content-Length")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}
