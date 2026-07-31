// =============================================================================
// File: internal/dap/client.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// client.go is a deliberate SIBLING of internal/lsp/client.go, not a shared
// transport abstraction.
//
// The two share a framing format (Content-Length, byte for byte) and nothing
// else. They talk to different processes over different transports about
// different things: LSP is stdio and request/response about a document, DAP is
// a SOCKET and is driven by events about a live process. CLAUDE.md's rule is to
// share code when two copies must AGREE — Tab.gutterMarker is shared because a
// breakpoint must look the same in both render paths — and to keep siblings
// when they merely resemble each other; wrap.go's renderWrapped next to Render
// is the fork's precedent for exactly that.
//
// 🔴 The concrete reason this is not a copy: lsp.Start's shape is WRONG here.
// It does call("initialize") and then treats the handshake as finished, because
// LSP has no event that can arrive before its own response. DAP does — see
// waitEvent and readLoop below. Copying the sibling verbatim reproduces that
// hang, so the resemblance is a trap, not a saving.
package dap

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SocketPlaceholder is substituted with the real socket path in an Adapter's
// Argv. Keeping the socket wiring in the registry's DATA rather than in this
// file is what lets a second adapter with a different flag spelling
// (debugpy's --connect, say) be one table entry instead of a code change.
const SocketPlaceholder = "{{sock}}"

// dialTimeout is how long we wait for the adapter to dial back in. Generous:
// delve dialed in at 26ms when measured, but a cold binary on a loaded machine
// is a different story, and the cost of being wrong here is a debugger that
// refuses to start.
const dialTimeout = 30 * time.Second

// maxStderrLines is how much adapter stderr is retained to explain a death.
// The last few lines are where the reason lives; keeping the whole stream would
// be an unbounded buffer attached to a process we do not control.
const maxStderrLines = 20

// writeTimeout bounds every write to the adapter.
//
// 🔴 Without it a wedged adapter — one that is alive but has stopped reading its
// socket — blocks us forever inside write(), which holds the client mutex. That
// is not a hypothetical: it was hit while proving the request_seq test RED, and
// the symptom was the EDITOR HANGING ON QUIT rather than anything to do with
// debugging. lsp.Stop's rule applies here word for word: a debugger that will
// not exit must never keep the editor from exiting. A write that times out
// leaves a half-framed message on a connection we are abandoning anyway.
const writeTimeout = 5 * time.Second

// disconnectTimeout is how long Stop waits for a polite shutdown before closing
// the socket underneath it.
const disconnectTimeout = 2 * time.Second

// Handlers are the callbacks a Client delivers adapter traffic through.
//
// 🔴 These are constructor PARAMETERS, not settable fields — and that is the
// difference between this and lsp.Client, which exposes OnDiagnostics/OnLog as
// fields you assign after Start returns.
//
// The DAP handshake makes that pattern unsafe. The `initialized` event may
// arrive before the `initialize` response (the spec permits either order), so
// the read loop can deliver an event before the caller has had a chance to
// assign a field. Taking the handlers up front makes "the handler was installed
// before the first byte was read" true by construction rather than by
// convention. Both are called from the read goroutine: the app must post a
// tcell event, never touch UI state here.
type Handlers struct {
	// OnEvent receives every adapter event, in arrival order.
	OnEvent func(Event)
	// OnLog receives adapter stderr and protocol-level complaints, wired to
	// the status line so a broken adapter says something.
	OnLog func(string)
}

// Client is one debug adapter, spoken to over a socket.
//
// The editor is single-threaded around a tcell event loop, so nothing here may
// block it: requests are issued from background goroutines and everything comes
// back as a posted event.
type Client struct {
	name   string
	cmd    *exec.Cmd
	exited chan error // the single cmd.Wait result; nil for StartConn
	stderr *lineRing  // retained adapter stderr; nil for StartConn
	conn   io.ReadWriteCloser
	ln     net.Listener // non-nil when we listened for the adapter to dial in
	sock   string       // unix socket path to unlink on Stop
	r      *bufio.Reader

	handlers Handlers

	mu      sync.Mutex
	nextSeq int
	pending map[int]chan Response // keyed by REQUEST_SEQ — see Response's doc
	caps    Capabilities
	closed  bool

	// seenEvents and waiters implement the "buffer events seen before anyone
	// waits" rule. See waitEvent.
	seenEvents map[string]bool
	waiters    map[string][]chan struct{}
}

// StartConn wraps an already-connected adapter stream and starts reading it.
// This is the seam the fake-adapter tests use, and it is the same code path
// StartCommand ends up in — a test that exercised a different one would prove
// nothing about the real thing.
func StartConn(name string, rw io.ReadWriteCloser, h Handlers) *Client {
	c := &Client{
		name:       name,
		conn:       rw,
		r:          bufio.NewReaderSize(rw, 64*1024),
		handlers:   h,
		pending:    make(map[int]chan Response),
		seenEvents: make(map[string]bool),
		waiters:    make(map[string][]chan struct{}),
	}
	go c.readLoop()
	return c
}

// StartCommand spawns an adapter and connects to it.
//
// 🔴 We LISTEN and let the adapter dial IN (delve's --client-addr), rather than
// letting it listen and polling for its socket to appear. `dlv dap` has no stdio
// mode at all — it is a socket server — so something has to establish the
// connection either way, and this direction has no readiness race to lose: the
// listener exists before the process does, so there is no window in which we can
// try to connect too early. The alternative (spawn, then poll for "DAP server
// listening at" on stdout, then dial) fails as "the adapter never started",
// which is indistinguishable from the adapter not being installed.
//
// dir is the working directory for the adapter, normally the project root.
func StartCommand(ctx context.Context, name string, argv []string, dir string, h Handlers) (*Client, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("%s: no command configured", name)
	}

	sock, err := shortSocketPath()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		os.Remove(sock)
		return nil, fmt.Errorf("%s: listen %s: %w", name, sock, err)
	}

	resolved := make([]string, len(argv))
	for i, a := range argv {
		resolved[i] = strings.ReplaceAll(a, SocketPlaceholder, sock)
	}

	cmd := exec.Command(resolved[0], resolved[1:]...)
	cmd.Dir = dir

	// 🔴 The debuggee must never inherit OUR stdio. `dlv dap` hands the program
	// it launches the adapter's own descriptors, so giving the adapter
	// os.Stdout would let a debugged program's fmt.Println scribble directly
	// over the tcell screen — with no way to repair it short of a redraw the
	// editor does not know it needs. The debuggee's real output reaches the user
	// as `output` EVENTS instead.
	//
	// Both streams are io.Writers rather than pipes on purpose: exec.Cmd then
	// owns the copying and cmd.Wait() waits for it, so the last few stderr lines
	// — exactly the ones that explain a death — cannot be lost to Wait closing a
	// pipe out from under a reader.
	ring := &lineRing{max: maxStderrLines}
	cmd.Stdout = io.Discard // dlv's own startup chatter; nothing acts on it
	cmd.Stderr = ring
	cmd.Stdin = nil // /dev/null, not our terminal

	if err := cmd.Start(); err != nil {
		ln.Close()
		os.RemoveAll(filepath.Dir(sock))
		return nil, fmt.Errorf("%s: %w", name, err)
	}

	// One waiter for the process, whose result both this function and Stop read.
	// Calling cmd.Wait twice is an error, so it happens in exactly one place.
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	abort := func() {
		_ = cmd.Process.Kill()
		<-exited
		ln.Close()
		os.RemoveAll(filepath.Dir(sock))
	}

	type acceptResult struct {
		conn net.Conn
		err  error
	}
	accepted := make(chan acceptResult, 1)
	go func() {
		conn, err := ln.Accept()
		accepted <- acceptResult{conn, err}
	}()

	deadline := time.NewTimer(dialTimeout)
	defer deadline.Stop()

	var conn net.Conn
	select {
	case res := <-accepted:
		if res.err != nil {
			abort()
			return nil, fmt.Errorf("%s: accept: %w", name, res.err)
		}
		conn = res.conn

	case err := <-exited:
		// The adapter died before dialing in. Waiting out the full dial timeout
		// here would report "did not connect within 30s" for something already
		// known to be over — and would hide the stderr that says WHY.
		ln.Close()
		os.RemoveAll(filepath.Dir(sock))
		return nil, fmt.Errorf("%s: adapter exited before connecting: %v%s", name, err, ring.suffix())

	case <-deadline.C:
		abort()
		return nil, fmt.Errorf("%s: adapter did not connect within %s%s", name, dialTimeout, ring.suffix())

	case <-ctx.Done():
		abort()
		return nil, ctx.Err()
	}

	c := &Client{
		name:       name,
		cmd:        cmd,
		exited:     exited,
		stderr:     ring,
		conn:       conn,
		ln:         ln,
		sock:       sock,
		r:          bufio.NewReaderSize(conn, 64*1024),
		handlers:   h,
		pending:    make(map[int]chan Response),
		seenEvents: make(map[string]bool),
		waiters:    make(map[string][]chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

// lineRing collects an adapter's stderr, keeping only the last max lines.
//
// Bounded on purpose: this is attached to a process we do not control, and a
// chatty adapter must not turn into unbounded memory. The last lines are the
// ones worth keeping — they are where the reason for a death lives.
type lineRing struct {
	mu      sync.Mutex
	partial []byte
	lines   []string
	max     int
}

// Write accumulates output, splitting it into lines as they complete.
func (l *lineRing) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.partial = append(l.partial, p...)
	for {
		i := bytes.IndexByte(l.partial, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimSpace(string(l.partial[:i]))
		l.partial = l.partial[i+1:]
		if line == "" {
			continue
		}
		l.lines = append(l.lines, line)
		if len(l.lines) > l.max {
			l.lines = l.lines[len(l.lines)-l.max:]
		}
	}
	return len(p), nil
}

// Lines returns the retained lines, oldest first, including any final line that
// never got a newline — which is exactly the shape a crash message takes.
func (l *lineRing) Lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := append([]string(nil), l.lines...)
	if rest := strings.TrimSpace(string(l.partial)); rest != "" {
		out = append(out, rest)
	}
	return out
}

// suffix renders the retained stderr for appending to an error message, or ""
// when the adapter said nothing.
func (l *lineRing) suffix() string {
	lines := l.Lines()
	if len(lines) == 0 {
		return ""
	}
	return " — " + strings.Join(lines, " | ")
}

// shortSocketPath builds a unix socket path that fits inside the platform's
// sockaddr_un limit.
//
// 🔴 macOS caps a unix socket path near 104 bytes, and the failure mode is not
// a path error — bind fails and the symptom is "the adapter never connected".
// A path derived from t.TempDir() blows the cap on its own: the per-test
// directory alone is ~97 characters before a filename is appended. So the name
// here is deliberately tiny, and the length is CHECKED rather than assumed.
func shortSocketPath() (string, error) {
	dir, err := os.MkdirTemp("", "hd")
	if err != nil {
		return "", err
	}
	sock := filepath.Join(dir, "d")
	if len(sock) > 100 {
		os.RemoveAll(dir)
		return "", fmt.Errorf("socket path %q is %d bytes, over the ~104 byte AF_UNIX limit", sock, len(sock))
	}
	return sock, nil
}

// Name is the adapter's configured name, used in status messages.
func (c *Client) Name() string { return c.name }

// Caps returns the capabilities the adapter reported at initialize.
func (c *Client) Caps() Capabilities {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.caps
}

// Initialize performs the handshake and records the adapter's capabilities.
//
// Note what this does NOT do, unlike its LSP sibling: it does not treat its own
// return as "the handshake is complete". The session is only configured once
// `initialized` has arrived and configurationDone has been sent, and the caller
// owns that sequence because only the caller knows what breakpoints to set in
// the middle of it.
func (c *Client) Initialize(ctx context.Context, adapterID string) (Capabilities, error) {
	raw, err := c.call(ctx, "initialize", initializeArgs{
		ClientID:        "herdr-edit",
		ClientName:      "herdr-edit",
		AdapterID:       adapterID,
		Locale:          "en-us",
		LinesStartAt1:   true,
		ColumnsStartAt1: true,
		PathFormat:      "path",
	})
	if err != nil {
		return Capabilities{}, err
	}
	var caps Capabilities
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &caps); err != nil {
			return Capabilities{}, fmt.Errorf("%s: initialize body: %w", c.name, err)
		}
	}
	c.mu.Lock()
	c.caps = caps
	c.mu.Unlock()
	return caps, nil
}

// Launch SENDS the launch request and returns a channel carrying its eventual
// response. It deliberately does not wait.
//
// 🔴 The ordering the protocol requires is:
//
//	initialize → launch SENT → `initialized` event → setBreakpoints →
//	setExceptionBreakpoints → configurationDone → *then* the launch response
//
// An adapter is allowed to withhold the launch response until configurationDone
// arrives, so a client that blocks on it before sending breakpoints deadlocks —
// by design, not by accident. (Delve happens to answer earlier; debugpy does
// not. Writing this against delve alone and blocking would appear to work and
// then hang for the next adapter, which is the worst possible way to find out.)
// The channel is buffered so a caller that never reads it cannot wedge the read
// loop.
func (c *Client) Launch(cfg map[string]interface{}) (<-chan Response, error) {
	return c.send("launch", cfg)
}

// Attach is the same contract as Launch, for attaching to a running process.
func (c *Client) Attach(cfg map[string]interface{}) (<-chan Response, error) {
	return c.send("attach", cfg)
}

// WaitEvent blocks until the named event has been seen.
//
// 🔴 It returns immediately when the event ALREADY arrived. That is the whole
// point: `initialized` can be delivered before the response to `initialize`,
// which means the natural spelling — call("initialize") then wait for
// `initialized` — misses an event that already went past. configurationDone
// then never fires and the session hangs with nothing logged anywhere.
// TestInitializedEventBeforeInitializeResponse pins this.
func (c *Client) WaitEvent(ctx context.Context, name string) error {
	c.mu.Lock()
	if c.seenEvents[name] {
		c.mu.Unlock()
		return nil
	}
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("%s: connection closed before %s", c.name, name)
	}
	ch := make(chan struct{})
	c.waiters[name] = append(c.waiters[name], ch)
	c.mu.Unlock()

	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		c.removeWaiterLocked(name, ch)
		c.mu.Unlock()
		return fmt.Errorf("%s: timed out waiting for %s event: %w", c.name, name, ctx.Err())
	}
}

// removeWaiterLocked drops one waiter channel. Called when a wait gives up, so
// an abandoned waiter is not left in the map for the life of the session.
func (c *Client) removeWaiterLocked(name string, ch chan struct{}) {
	list := c.waiters[name]
	for i, w := range list {
		if w == ch {
			c.waiters[name] = append(list[:i], list[i+1:]...)
			return
		}
	}
}

// SetBreakpoints replaces the ENTIRE breakpoint set for one source file.
//
// 🔴 There is no incremental form of this request. Sending just the breakpoint
// the user added clears every other one in that file, and the symptom is
// breakpoints that "randomly stop working" — so callers must pass the complete
// list for the file every time.
//
// The returned slice matches bps POSITIONALLY, and its lines may differ from
// what was asked for. See Breakpoint's doc.
func (c *Client) SetBreakpoints(ctx context.Context, src Source, bps []SourceBreakpoint) ([]Breakpoint, error) {
	if bps == nil {
		bps = []SourceBreakpoint{} // null would be rejected; an empty set CLEARS the file
	}
	raw, err := c.call(ctx, "setBreakpoints", setBreakpointsArgs{Source: src, Breakpoints: bps})
	if err != nil {
		return nil, err
	}
	var body setBreakpointsBody
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, err
		}
	}
	return body.Breakpoints, nil
}

// SetExceptionBreakpoints selects which exception classes stop the program.
// Passing the adapter's own defaults (Capabilities.DefaultFilters) preserves
// stop-on-panic, which an empty list silently turns off.
func (c *Client) SetExceptionBreakpoints(ctx context.Context, filters []string) error {
	if filters == nil {
		filters = []string{}
	}
	_, err := c.call(ctx, "setExceptionBreakpoints", setExceptionBreakpointsArgs{Filters: filters})
	return err
}

// ConfigurationDone tells the adapter the breakpoint set is complete and it may
// run the program. Mandatory whenever supportsConfigurationDoneRequest is set —
// delve sets it, and skipping this leaves the program parked forever with no
// error to explain why.
func (c *Client) ConfigurationDone(ctx context.Context) error {
	_, err := c.call(ctx, "configurationDone", struct{}{})
	return err
}

// Threads lists the debuggee's threads (goroutines, for Go).
func (c *Client) Threads(ctx context.Context) ([]Thread, error) {
	raw, err := c.call(ctx, "threads", struct{}{})
	if err != nil {
		return nil, err
	}
	var body threadsBody
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, err
		}
	}
	return body.Threads, nil
}

// StackTrace returns a stopped thread's frames, innermost first. levels caps
// how many are fetched; 0 means "all".
func (c *Client) StackTrace(ctx context.Context, threadID, levels int) ([]StackFrame, error) {
	raw, err := c.call(ctx, "stackTrace", stackTraceArgs{ThreadID: threadID, Levels: levels})
	if err != nil {
		return nil, err
	}
	var body stackTraceBody
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, err
		}
	}
	return body.StackFrames, nil
}

// Continue resumes a stopped program.
func (c *Client) Continue(ctx context.Context, threadID int) error {
	_, err := c.call(ctx, "continue", threadArgs{ThreadID: threadID})
	return err
}

// Pause interrupts a running program so it reports where it is. Bound to F6.
func (c *Client) Pause(ctx context.Context, threadID int) error {
	_, err := c.call(ctx, "pause", threadArgs{ThreadID: threadID})
	return err
}

// Disconnect ends the session and, by default, kills the debuggee.
//
// 🔴 Delve does NOT advertise supportsTerminateRequest, so there is no polite
// `terminate` to send: disconnect{terminateDebuggee:true} is the only way to
// stop the debugged process. Getting this wrong leaks a live process every time
// the user stops debugging, which nothing on screen would report.
func (c *Client) Disconnect(ctx context.Context, terminateDebuggee bool) error {
	_, err := c.call(ctx, "disconnect", disconnectArgs{TerminateDebuggee: terminateDebuggee})
	return err
}

// Stop shuts the adapter down. Best effort throughout: a wedged debugger must
// never keep the editor from quitting, so we ask once and then kill.
func (c *Client) Stop() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()

	// The polite shutdown runs on its own goroutine and is given a deadline,
	// then we close the socket regardless. Closing unblocks a goroutine still
	// stuck in write(), so this bounds the wait without leaking anything.
	disconnected := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), disconnectTimeout)
		defer cancel()
		_ = c.Disconnect(ctx, true)
		close(disconnected)
	}()
	select {
	case <-disconnected:
	case <-time.After(disconnectTimeout):
	}

	if c.conn != nil {
		_ = c.conn.Close()
	}
	if c.ln != nil {
		_ = c.ln.Close()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		// exited is fed by the single cmd.Wait goroutine StartCommand started.
		select {
		case <-c.exited:
		case <-time.After(disconnectTimeout):
			_ = c.cmd.Process.Kill()
			<-c.exited
		}
	}
	if c.sock != "" {
		os.RemoveAll(filepath.Dir(c.sock))
	}
}

// send writes a request and hands back the channel its response will arrive on,
// without waiting. Used by Launch/Attach, whose responses must not be blocked
// on. Every other request goes through call.
func (c *Client) send(command string, args interface{}) (<-chan Response, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("%s: client closed", c.name)
	}
	c.nextSeq++
	seq := c.nextSeq
	ch := make(chan Response, 1)
	c.pending[seq] = ch
	c.mu.Unlock()

	if err := c.write(Request{Seq: seq, Type: TypeRequest, Command: command, Arguments: args}); err != nil {
		c.mu.Lock()
		delete(c.pending, seq)
		c.mu.Unlock()
		return nil, err
	}
	return ch, nil
}

// call issues a request and waits for its answer or the context deadline.
func (c *Client) call(ctx context.Context, command string, args interface{}) (json.RawMessage, error) {
	c.mu.Lock()
	if c.closed && command != "disconnect" {
		c.mu.Unlock()
		return nil, fmt.Errorf("%s: client closed", c.name)
	}
	c.nextSeq++
	seq := c.nextSeq
	ch := make(chan Response, 1)
	c.pending[seq] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, seq)
		c.mu.Unlock()
	}()

	if err := c.write(Request{Seq: seq, Type: TypeRequest, Command: command, Arguments: args}); err != nil {
		return nil, err
	}

	select {
	case resp := <-ch:
		// 🔴 Success is checked BEFORE Body is touched. A refusal still carries a
		// body, and unmarshalling it into a result type yields that type's zero
		// value — so reading the body first turns "unknown goroutine" into an
		// empty stack trace, and the user sees a debugger that stopped nowhere.
		if !resp.Success {
			return nil, c.responseError(command, resp)
		}
		return resp.Body, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("%s: %s: %w", c.name, command, ctx.Err())
	}
}

// responseError turns a refusal into the most informative error available.
// Delve puts the readable reason in body.error.format rather than in the
// top-level message, so both are consulted before falling back to the command
// name alone.
func (c *Client) responseError(command string, resp Response) error {
	var eb errorBody
	if len(resp.Body) > 0 {
		_ = json.Unmarshal(resp.Body, &eb)
	}
	switch {
	case eb.Error.Format != "":
		return fmt.Errorf("%s: %s: %s", c.name, command, eb.Error.Format)
	case resp.Message != "":
		return fmt.Errorf("%s: %s: %s", c.name, command, resp.Message)
	default:
		return fmt.Errorf("%s: %s failed", c.name, command)
	}
}

// write frames one message with the Content-Length header the protocol
// requires — byte-for-byte the same framing as LSP.
func (c *Client) write(req Request) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return fmt.Errorf("%s: no connection", c.name)
	}
	// Bound the write when the transport can be bounded — see writeTimeout.
	// net.Conn and net.Pipe both can; an io.ReadWriteCloser in a test may not,
	// and there is nothing to protect against in that case.
	if dc, ok := c.conn.(interface{ SetWriteDeadline(time.Time) error }); ok {
		_ = dc.SetWriteDeadline(time.Now().Add(writeTimeout))
	}
	if _, err := fmt.Fprintf(c.conn, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	_, err = c.conn.Write(body)
	return err
}

// readLoop demultiplexes everything the adapter sends: responses go to whoever
// is waiting on their REQUEST_SEQ, events are recorded and dispatched.
func (c *Client) readLoop() {
	for {
		body, err := readMessage(c.r)
		if err != nil {
			c.handleDisconnect(err)
			return
		}

		// Decoding into Response first is safe for every message type: it
		// ignores the fields it does not have, and Type is what decides.
		var resp Response
		if err := json.Unmarshal(body, &resp); err != nil {
			c.log("malformed message: " + err.Error())
			continue
		}

		if resp.Type == TypeEvent {
			var ev Event
			if err := json.Unmarshal(body, &ev); err != nil {
				c.log("malformed event: " + err.Error())
				continue
			}
			c.dispatchEvent(ev)
			continue
		}
		if resp.Type != TypeResponse {
			continue // reverse requests: nothing in stage 2 answers one
		}

		// 🔴 request_seq, NOT seq. Both are 1 on a session's first response
		// (measured against delve 1.27), so keying on Seq demultiplexes
		// correctly exactly once and then every later request times out.
		c.mu.Lock()
		ch, ok := c.pending[resp.RequestSeq]
		c.mu.Unlock()
		if ok {
			ch <- resp
		}
	}
}

// dispatchEvent records that an event was seen, wakes anyone waiting on it, and
// forwards it. Recording happens BEFORE forwarding so a handler that turns
// around and calls WaitEvent for the same name cannot deadlock against itself.
func (c *Client) dispatchEvent(ev Event) {
	c.mu.Lock()
	c.seenEvents[ev.Event] = true
	waiters := c.waiters[ev.Event]
	delete(c.waiters, ev.Event)
	// The capabilities event is the adapter revising what it told us at
	// initialize; folding it in here keeps Caps() authoritative.
	if ev.Event == EventCapabilities && len(ev.Body) > 0 {
		var ce CapabilitiesEvent
		if err := json.Unmarshal(ev.Body, &ce); err == nil {
			c.caps = ce.Capabilities
		}
	}
	c.mu.Unlock()

	for _, w := range waiters {
		close(w)
	}
	if c.handlers.OnEvent != nil {
		c.handlers.OnEvent(ev)
	}
}

// handleDisconnect is what runs when the adapter's stream ends — whether it
// exited cleanly after `terminated` or died mid-session.
//
// 🔴 A synthetic `terminated` event is posted unconditionally. Without it, an
// adapter that crashes leaves the UI sitting in "stopped": the ▶ stays painted,
// the status bar still claims a live session, and F5 is bound to a dead client.
// The last stderr lines go out too, because "the debugger vanished" with no
// reason attached is the least actionable failure a user can be handed.
func (c *Client) handleDisconnect(err error) {
	c.mu.Lock()
	pending := c.pending
	c.pending = make(map[int]chan Response)
	waiters := c.waiters
	c.waiters = make(map[string][]chan struct{})
	alreadyTerminated := c.seenEvents[EventTerminated]
	c.seenEvents[EventTerminated] = true
	closed := c.closed
	c.mu.Unlock()
	stderr := c.LastStderr()

	// Unblock every waiting caller so a dead adapter surfaces as an error
	// rather than as a request that never returns.
	for seq, ch := range pending {
		ch <- Response{
			Type: TypeResponse, RequestSeq: seq, Success: false,
			Message: "adapter connection lost: " + err.Error(),
		}
	}
	for _, list := range waiters {
		for _, w := range list {
			close(w)
		}
	}

	if closed {
		return // we asked for this; Stop() already told the app
	}
	if !errors.Is(err, io.EOF) {
		c.log("connection lost: " + err.Error())
	}
	if len(stderr) > 0 && !alreadyTerminated {
		c.log("adapter said: " + strings.Join(stderr, " | "))
	}
	if !alreadyTerminated && c.handlers.OnEvent != nil {
		c.handlers.OnEvent(Event{Type: TypeEvent, Event: EventTerminated})
	}
}

// LastStderr returns the retained adapter stderr, oldest first. Empty for a
// client built with StartConn, which owns no process.
func (c *Client) LastStderr() []string {
	if c.stderr == nil {
		return nil
	}
	return c.stderr.Lines()
}

// log forwards a message to the app's status line, prefixed with the adapter
// name so a multi-adapter future stays legible.
func (c *Client) log(msg string) {
	if c.handlers.OnLog != nil {
		c.handlers.OnLog(c.name + ": " + msg)
	}
}

// readMessage reads one Content-Length framed message.
//
// The framing is identical to LSP's, and this is a genuine second copy of
// lsp.readMessage rather than a shared helper — see this file's header for why
// the two packages stay siblings. A missing or unparseable length is fatal to
// the stream: without it there is no way to know where the next message begins.
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
