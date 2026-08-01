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
	// OnEvent receives every adapter event, in arrival order, along with the
	// client it arrived on.
	//
	// 🔴 The *Client argument is the whole point and it is a parameter rather
	// than something the caller closes over. A js-debug session is a ROOT
	// coordinator plus a LEAF that owns the program, and both are given the same
	// handlers — so without it, events from two connections merge into one
	// undifferentiated stream and the receiver cannot tell a stop in the debuggee
	// from anything the coordinator said. Closing over the client instead would
	// race: StartCommand runs `go c.readLoop()` BEFORE it returns, so the closure
	// could fire before the variable it reads has been assigned, and -race would
	// find it.
	OnEvent func(*Client, Event)
	// OnLog receives adapter stderr and protocol-level complaints, wired to
	// the status line so a broken adapter says something. Same client argument,
	// same reason.
	OnLog func(*Client, string)
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

	// dialAddr is the host:port of an adapter SERVER we dialed, and "" for the
	// two transports that cannot carry a second connection. DialChild is the
	// only reader; it is what makes a child session reachable at all.
	dialAddr string

	handlers Handlers

	mu      sync.Mutex
	nextSeq int
	pending map[int]chan Response // keyed by REQUEST_SEQ — see Response's doc
	caps    Capabilities
	closed  bool

	// childReq carries the FIRST startDebugging the adapter asks for. Buffered
	// so the read loop never blocks on a caller that is not waiting yet — the
	// request can and does arrive before AwaitChildSession is called.
	//
	// childClaimed is what makes a SECOND one a refusal rather than a queue. See
	// handleStartDebugging: silently accepting the second would leave the editor
	// debugging whichever process happened to arrive first, with the other one
	// running unobserved.
	childReq     chan ChildSession
	childClaimed bool

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
	c := newClient(name, rw, h)
	go c.readLoop()
	return c
}

// newClient builds the parts every transport shares, and exists so that there
// is ONE place the internal channels and maps are created.
//
// 🔴 There were four struct literals doing this by hand, which is three chances
// to forget a field. Forgetting childReq in particular is not a nil-map panic
// but something quieter: the read loop would block forever on a send to a nil
// channel the first time an adapter asked for a child session, and the symptom
// would be a debugger that stops answering with no error anywhere.
func newClient(name string, conn io.ReadWriteCloser, h Handlers) *Client {
	return &Client{
		name:       name,
		conn:       conn,
		r:          bufio.NewReaderSize(conn, 64*1024),
		handlers:   h,
		pending:    make(map[int]chan Response),
		childReq:   make(chan ChildSession, 1),
		seenEvents: make(map[string]bool),
		waiters:    make(map[string][]chan struct{}),
	}
}

// Transport is how a client reaches an adapter's protocol stream.
//
// 🔴 There are exactly two and neither is a superset of the other. `dlv dap` has
// NO stdio mode at all — it is a socket server — while debugpy's adapter speaks
// the protocol over its own stdin/stdout and only listens on a port when told
// to. An abstraction that assumed either shape would have had to be unpicked the
// moment a second adapter arrived, which is precisely why Client was built
// around an io.ReadWriteCloser: BELOW this one dispatch the two transports are
// the same code — same framing, same read loop, same Stop.
type Transport int

const (
	// TransportSocket makes US listen and the adapter dial IN. See
	// startSocketCommand for why that direction and not the other.
	TransportSocket Transport = iota

	// TransportStdio speaks the protocol over the adapter's own stdin/stdout.
	// The debuggee's output must NOT come back the same way — see
	// startStdioCommand.
	TransportStdio

	// TransportServer spawns a TCP SERVER and dials it — js-debug's shape, and
	// the third distinct answer to "how do I reach this adapter" in three
	// adapters.
	//
	// 🔴 It is not TransportSocket with a different address family. In
	// TransportSocket WE listen and the adapter dials in; here the ADAPTER
	// listens and we dial. That inversion is what makes a readiness race
	// possible again, and startServerCommand is shaped entirely around removing
	// it — see there.
	//
	// It is also the transport that can carry more than one connection, which is
	// the whole reason child sessions are reachable at all: DialChild opens a
	// SECOND connection to the same server. Neither of the other two transports
	// could express that — a stdio adapter has exactly one stdin.
	TransportServer
)

// Command is one adapter resolved down to something that can be executed: what
// to run, with what extra environment, over which transport, and where it was
// found.
//
// It is a struct rather than a bare argv because the second adapter needed all
// four. Threading them as parameters would have put the "how do I reach this
// particular tool" knowledge into client.go, which is the one place in this
// package that must stay adapter-agnostic.
type Command struct {
	// Argv is the command line, already resolved to an absolute binary.
	Argv []string

	// Env holds extra KEY=VALUE entries APPENDED to the inherited environment.
	//
	// It exists because the MIT VS Code copy of debugpy is a library directory
	// rather than an installed distribution, so PYTHONPATH is the only thing
	// that makes `python -m debugpy.adapter` resolve to it. That is data about
	// one adapter, and it belongs in the registry rather than in a branch here.
	Env []string

	// Transport is how the protocol stream is reached.
	Transport Transport

	// Origin says WHERE this was found — a virtualenv, a VS Code extension
	// directory, PATH. Surfaced to the user, because "debugpy started" is a
	// materially different statement depending on which debugpy it was, and a
	// silently-preferred global install is how a project's pinned version gets
	// bypassed with nothing on screen to say so.
	Origin string
}

// StartCommand spawns an adapter and connects to it over its own transport.
//
// dir is the working directory for the adapter, normally the project root.
func StartCommand(ctx context.Context, name string, spec Command, dir string, h Handlers) (*Client, error) {
	if len(spec.Argv) == 0 {
		return nil, fmt.Errorf("%s: no command configured", name)
	}
	switch spec.Transport {
	case TransportStdio:
		return startStdioCommand(name, spec, dir, h)
	case TransportServer:
		return startServerCommand(ctx, name, spec, dir, h)
	default:
		return startSocketCommand(ctx, name, spec, dir, h)
	}
}

// newAdapterCmd builds the exec.Cmd both transports spawn, with the stderr ring
// attached and any extra environment folded in.
//
// 🔴 cmd.Env is left NIL when there is nothing to add, rather than being set to
// os.Environ(). The two are equivalent today, but an explicit copy is a snapshot
// taken at spawn time and a nil is "inherit", and only one of those keeps
// working if this ever moves off the main goroutine.
func newAdapterCmd(spec Command, dir string, ring *lineRing) *exec.Cmd {
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = dir
	if len(spec.Env) > 0 {
		cmd.Env = append(os.Environ(), spec.Env...)
	}
	cmd.Stderr = ring
	return cmd
}

// startStdioCommand spawns an adapter that speaks the protocol over its own
// stdin/stdout — debugpy's shape.
//
// 🔴 There is no accept to wait for here, so unlike the socket path an adapter
// that dies on startup (`No module named debugpy`) cannot be reported from this
// function. It surfaces one step later instead: the read loop hits EOF,
// handleDisconnect logs the retained stderr, and the in-flight initialize is
// failed rather than left hanging. That is why callers append LastStderr() to an
// initialize failure — without it the user is told "connection lost" for
// something whose actual reason the adapter printed.
//
// 🔴 The debuggee must never be handed this stdout: it IS the protocol stream,
// so a debugged program's print would be framed as garbage and desynchronise the
// session permanently. Adapters forward it as `output` events instead when asked
// to (debugpy's console:internalConsole, delve's outputMode:remote), which is a
// registry-level launch key rather than anything this function can enforce.
func startStdioCommand(name string, spec Command, dir string, h Handlers) (*Client, error) {
	ring := &lineRing{max: maxStderrLines}
	cmd := newAdapterCmd(spec, dir, ring)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("%s: stdin: %w", name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("%s: stdout: %w", name, err)
	}

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("%s: %w", name, err)
	}

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	conn := &stdioConn{in: stdin, out: stdout}
	c := newClient(name, conn, h)
	c.cmd, c.exited, c.stderr = cmd, exited, ring
	go c.readLoop()
	return c, nil
}

// stdioConn presents a child process's stdin/stdout pipes as the single
// io.ReadWriteCloser Client is built around, so the stdio adapter reuses the
// socket adapter's read loop, framing and shutdown verbatim.
type stdioConn struct {
	in  io.WriteCloser
	out io.ReadCloser

	once sync.Once
	err  error
}

// Read pulls from the adapter's stdout.
func (s *stdioConn) Read(p []byte) (int, error) { return s.out.Read(p) }

// Write pushes to the adapter's stdin.
func (s *stdioConn) Write(p []byte) (int, error) { return s.in.Write(p) }

// Close shuts both pipes, which is what makes an adapter blocked on reading its
// stdin see EOF and exit. Idempotent because Stop closes the connection and the
// process teardown may race with the read loop noticing.
func (s *stdioConn) Close() error {
	s.once.Do(func() {
		if err := s.in.Close(); err != nil {
			s.err = err
		}
		if err := s.out.Close(); err != nil && s.err == nil {
			s.err = err
		}
	})
	return s.err
}

// SetWriteDeadline forwards a deadline to the underlying pipe when it has one.
//
// 🔴 This is what keeps writeTimeout's guarantee on the stdio transport. Without
// it a wedged adapter — alive but no longer reading its stdin — blocks a write
// forever while holding the client mutex, and the symptom is the EDITOR HANGING
// ON QUIT rather than anything to do with debugging. os.Pipe descriptors are
// pollable, so this really does bound the write; the type assertion is there
// because exec.Cmd's pipes are wrapped, not because it is expected to fail.
func (s *stdioConn) SetWriteDeadline(t time.Time) error {
	if d, ok := s.in.(interface{ SetWriteDeadline(time.Time) error }); ok {
		return d.SetWriteDeadline(t)
	}
	return nil
}

// startSocketCommand spawns an adapter and waits for it to dial back in.
//
// 🔴 We LISTEN and let the adapter dial IN (delve's --client-addr), rather than
// letting it listen and polling for its socket to appear. `dlv dap` has no stdio
// mode at all — it is a socket server — so something has to establish the
// connection either way, and this direction has no readiness race to lose: the
// listener exists before the process does, so there is no window in which we can
// try to connect too early. The alternative (spawn, then poll for "DAP server
// listening at" on stdout, then dial) fails as "the adapter never started",
// which is indistinguishable from the adapter not being installed.
func startSocketCommand(ctx context.Context, name string, spec Command, dir string, h Handlers) (*Client, error) {
	argv := spec.Argv

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
	spec.Argv = resolved

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
	cmd := newAdapterCmd(spec, dir, ring)
	cmd.Stdout = io.Discard // dlv's own startup chatter; nothing acts on it
	cmd.Stdin = nil         // /dev/null, not our terminal

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

	c := newClient(name, conn, h)
	c.cmd, c.exited, c.stderr, c.ln, c.sock = cmd, exited, ring, ln, sock
	go c.readLoop()
	return c, nil
}

// serverListenPattern is what a dialable adapter server prints on stdout to say
// it is ready. Measured against js-debug 1.117.0:
//
//	Debug server listening at 127.0.0.1:61739
const serverListenPattern = "listening at "

// startServerCommand spawns an adapter that LISTENS, waits for it to say where,
// and dials it.
//
// 🔴 The port is read from the adapter's own stdout rather than chosen by us,
// and that is what makes this transport race-free despite inverting
// TransportSocket's direction. The obvious implementation — pick a free port,
// pass it, then dial — loses twice: the port can be taken between our probe and
// the adapter's bind, and dialing before the listener exists fails as
// "connection refused", which is indistinguishable from the adapter not being
// installed. Passing port 0 and reading back the real one means the line we
// parse is itself the readiness signal: it cannot be printed before the socket
// can be accepted on.
//
// The stdout pipe stays drained afterwards. js-debug keeps writing to it, and a
// child process whose stdout pipe fills up blocks in write() — which would look
// like a debugger that hangs after a few minutes of use.
func startServerCommand(ctx context.Context, name string, spec Command, dir string, h Handlers) (*Client, error) {
	ring := &lineRing{max: maxStderrLines}
	cmd := newAdapterCmd(spec, dir, ring)
	cmd.Stdin = nil

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("%s: stdout: %w", name, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	abort := func() {
		_ = cmd.Process.Kill()
		<-exited
	}

	type addrResult struct {
		addr string
		err  error
	}
	announced := make(chan addrResult, 1)
	br := bufio.NewReader(stdout)
	go func() {
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				announced <- addrResult{err: fmt.Errorf("adapter stdout ended before it announced an address: %w", err)}
				return
			}
			if i := strings.Index(line, serverListenPattern); i >= 0 {
				announced <- addrResult{addr: strings.TrimSpace(line[i+len(serverListenPattern):])}
				break
			}
		}
		// Keep draining so the adapter never blocks writing to a full pipe.
		_, _ = io.Copy(io.Discard, br)
	}()

	var addr string
	select {
	case res := <-announced:
		if res.err != nil {
			abort()
			return nil, fmt.Errorf("%s: %v%s", name, res.err, ring.suffix())
		}
		addr = res.addr
	case err := <-exited:
		return nil, fmt.Errorf("%s: adapter exited before listening: %v%s", name, err, ring.suffix())
	case <-time.After(dialTimeout):
		abort()
		return nil, fmt.Errorf("%s: adapter did not announce an address within %s%s", name, dialTimeout, ring.suffix())
	case <-ctx.Done():
		abort()
		return nil, ctx.Err()
	}

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		abort()
		return nil, fmt.Errorf("%s: dial %s: %w%s", name, addr, err, ring.suffix())
	}

	c := newClient(name, conn, h)
	c.cmd, c.exited, c.stderr, c.dialAddr = cmd, exited, ring, addr
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
	raw, err := c.call(ctx, "initialize", initializeArgsForClient(adapterID))
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

// initializeArgsForClient is the capability declaration sent at initialize,
// kept next to Initialize so the two cannot drift.
func initializeArgsForClient(adapterID string) initializeArgs {
	return initializeArgs{
		ClientID:        "herdr-edit",
		ClientName:      "herdr-edit",
		AdapterID:       adapterID,
		Locale:          "en-us",
		LinesStartAt1:   true,
		ColumnsStartAt1: true,
		PathFormat:      "path",

		// Both are honoured: Variables reads Type, and sends start/count when
		// the adapter says it can page. See initializeArgs' comment.
		SupportsVariableType:   true,
		SupportsVariablePaging: true,

		// Honoured by readLoop → answerReverseRequest, and by the app's
		// AwaitChildSession/DialChild sequence. See initializeArgs.
		SupportsStartDebuggingRequest: true,
	}
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

// Next steps over the current line: a call on it runs to completion rather than
// being descended into. Bound to F10.
//
// 🔴 Stepping is ASYNCHRONOUS, and every one of these three is. The response
// says only that the adapter accepted the request; where the program ended up
// arrives later as a `stopped` event with reason "step". A caller that reads a
// stack trace off the back of this response gets the location it had BEFORE the
// step — plausible, wrong, and indistinguishable from a step that did nothing.
func (c *Client) Next(ctx context.Context, threadID int) error {
	_, err := c.call(ctx, "next", threadArgs{ThreadID: threadID})
	return err
}

// StepIn descends into the call on the current line, or behaves like Next when
// there is none. Bound to F11. Asynchronous — see Next.
func (c *Client) StepIn(ctx context.Context, threadID int) error {
	_, err := c.call(ctx, "stepIn", threadArgs{ThreadID: threadID})
	return err
}

// StepOut runs to the return of the current function and stops in the caller.
// Bound to F12. Asynchronous — see Next.
func (c *Client) StepOut(ctx context.Context, threadID int) error {
	_, err := c.call(ctx, "stepOut", threadArgs{ThreadID: threadID})
	return err
}

// Pause interrupts a running program so it reports where it is. Bound to F6.
func (c *Client) Pause(ctx context.Context, threadID int) error {
	_, err := c.call(ctx, "pause", threadArgs{ThreadID: threadID})
	return err
}

// Scopes lists the variable groups belonging to one stack FRAME.
//
// frameID comes from StackFrame.ID and is not a thread id: asking for scopes
// with a thread id is the classic spelling of this bug, and delve answers it
// with an unrelated frame's variables rather than an error — so the caller sees
// a plausible, wrong set of values. The live oracle walks
// stackTrace → scopes → variables end to end for exactly that reason.
func (c *Client) Scopes(ctx context.Context, frameID int) ([]Scope, error) {
	raw, err := c.call(ctx, "scopes", scopesArgs{FrameID: frameID})
	if err != nil {
		return nil, err
	}
	var body scopesBody
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, err
		}
	}
	return body.Scopes, nil
}

// Variables reads the contents of one variables reference — a scope's, or an
// expandable variable's.
//
// count caps how many are asked for, and is sent ONLY when the adapter
// advertised supportsVariablePaging. That gate is not politeness: an adapter
// without paging support ignores start/count silently instead of refusing, so a
// client that always sent them would believe it had bounded the answer while
// receiving the whole of a million-element slice.
//
// 🔴 Delve does NOT advertise it (measured, 1.27), so on the adapter this fork
// actually ships the window is never sent and this always returns everything.
// Callers MUST cap what comes back — see internal/app's maxDebugVariables.
//
// 🔴 ref is PERISHABLE. It is valid until the program next runs, and after a
// continue or a step the adapter may reuse the number for something else or
// reject it outright. Caching a variables tree across a step therefore shows
// another frame's values with no error at all, which is the worst failure a
// debugger has. internal/app drops every cached reference on `continued` and on
// `stopped`.
func (c *Client) Variables(ctx context.Context, ref, start, count int) ([]Variable, error) {
	args := variablesArgs{VariablesReference: ref}
	if c.Caps().SupportsVariablePaging {
		args.Start = start
		args.Count = count
	}
	raw, err := c.call(ctx, "variables", args)
	if err != nil {
		return nil, err
	}
	var body variablesBody
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, err
		}
	}
	return body.Variables, nil
}

// Evaluate asks the adapter what an expression is worth.
//
// 🔴 frameID and evalContext travel together and getting the pair wrong is a
// bug that RENDERS. With EvalContextWatch and a frame id the expression is
// evaluated in that frame, which is the only way "what is `i` here" can mean
// the `i` the user is looking at; with EvalContextRepl and no frame it is
// evaluated globally. Passing a frame id with repl, or watch with none, is
// something both delve and debugpy accept and answer — from another scope,
// with no error to say so. Pass 0 for frameID when there is no stop.
func (c *Client) Evaluate(ctx context.Context, expr string, frameID int, evalContext string) (EvaluateResult, error) {
	args := evaluateArgs{Expression: expr, Context: evalContext}
	// 🔴 The frame is sent whenever there IS one, and "frameID != 0" is not that
	// test — js-debug numbers its innermost frame 0. EvalContextWatch means "in
	// the frame the user is looking at", so it always carries the id, including
	// zero; EvalContextRepl with no frame is the one case where the field must be
	// absent. See evaluateArgs.
	if frameID != 0 || evalContext == EvalContextWatch {
		id := frameID
		args.FrameID = &id
	}
	raw, err := c.call(ctx, "evaluate", args)
	if err != nil {
		return EvaluateResult{}, err
	}
	var body EvaluateResult
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			return EvaluateResult{}, err
		}
	}
	return body, nil
}

// Terminate asks the adapter to end the debuggee politely, giving it a chance
// to run its own shutdown. Only meaningful when the adapter advertised
// supportsTerminateRequest; use TerminateOrDisconnect rather than calling this
// directly.
func (c *Client) Terminate(ctx context.Context) error {
	_, err := c.call(ctx, "terminate", terminateArgs{})
	return err
}

// TerminateOrDisconnect stops the debugged process by whichever route this
// adapter actually supports, and is the ONLY place that decision is made.
//
// 🔴 supportsTerminateRequest is ABSENT from delve's capabilities — measured
// against 1.27. Sending `terminate` to an adapter that never claimed it comes
// back as an unknown-command error, so a stop button wired straight to it does
// NOTHING on delve while reporting nothing: the editor drops the session, the
// debugged process keeps running, and the only evidence is a stray dlv in the
// process table. The fallback is disconnect{terminateDebuggee:true}, which is
// what delve requires and what Stop has always used.
//
// The terminate branch also falls through on failure rather than reporting it.
// An adapter is allowed to advertise the capability and still refuse a
// particular terminate, and "we asked nicely and it said no" must not be the
// end of the story when a live process is at stake.
func (c *Client) TerminateOrDisconnect(ctx context.Context) error {
	if c.Caps().SupportsTerminateRequest {
		if err := c.Terminate(ctx); err == nil {
			return nil
		}
	}
	return c.Disconnect(ctx, true)
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
		// Whichever route this adapter supports — on delve that is still
		// disconnect{terminateDebuggee:true}. See TerminateOrDisconnect.
		_ = c.TerminateOrDisconnect(ctx)
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
			// 🔴 The wait after the KILL is bounded too, and an unbounded one
			// here hangs the EDITOR. cmd.Stderr is a lineRing rather than a
			// *os.File, so exec.Cmd makes a pipe and a copying goroutine for it,
			// and cmd.Wait does not return until that goroutine sees EOF. EOF
			// needs every holder of the write end to close it — and an adapter's
			// GRANDCHILDREN inherit it. js-debug spawns the debuggee, which
			// spawns node; killing the adapter leaves node holding that pipe, so
			// Wait blocks forever on a process we have already killed.
			//
			// Observed as a 45-second test timeout with cmd.Wait parked in
			// awaitGoroutines. It matters far beyond a test: stopDebugSession
			// runs SYNCHRONOUSLY on the way out of Run, so this is the editor
			// refusing to quit — the exact failure writeTimeout exists to
			// prevent, arrived at from a different direction.
			//
			// Abandoning the wait leaks a goroutine and a pipe until the process
			// exits, which is the cheaper of the two: the adapter is dead, and a
			// debugger that will not exit must never keep the editor from
			// exiting.
			select {
			case <-c.exited:
			case <-time.After(disconnectTimeout):
			}
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

// isShutdownCommand names the requests that are still legal AFTER Stop has
// marked the client closed.
//
// 🔴 Both belong here, not just disconnect. Stop sets closed and then asks
// TerminateOrDisconnect to end the debuggee; with only disconnect exempt, an
// adapter that DOES advertise supportsTerminateRequest would have its terminate
// refused by our own guard, fall through to disconnect, and appear to work —
// hiding the fact that the polite path had never once run. A guard that makes a
// branch unreachable is worse than no guard, because the branch still looks
// tested.
func isShutdownCommand(command string) bool {
	return command == "disconnect" || command == "terminate"
}

// call issues a request and waits for its answer or the context deadline.
func (c *Client) call(ctx context.Context, command string, args interface{}) (json.RawMessage, error) {
	c.mu.Lock()
	if c.closed && !isShutdownCommand(command) {
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
//
// It takes an interface rather than a Request because outgoing traffic is no
// longer only requests: a reverse request has to be ANSWERED, and an answer is
// a Response. One framing path for both is the point — two would be two places
// for the deadline and the nil-connection check to drift.
func (c *Client) write(msg interface{}) error {
	body, err := json.Marshal(msg)
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
		if resp.Type == TypeRequest {
			var rr ReverseRequest
			if err := json.Unmarshal(body, &rr); err != nil {
				c.log("malformed reverse request: " + err.Error())
				continue
			}
			c.handleReverseRequest(rr)
			continue
		}
		if resp.Type != TypeResponse {
			continue
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

// handleReverseRequest answers a request the ADAPTER sent us.
//
// 🔴 EVERY reverse request is answered, including the ones we refuse. An
// adapter blocks on a reverse request until it gets a response, so ignoring one
// is not a graceful decline — it is a session that stops making progress with
// nothing logged. That is what the old `continue` in readLoop did, and it was
// correct only for as long as we declared no capability that could produce one.
func (c *Client) handleReverseRequest(rr ReverseRequest) {
	if rr.Command == CommandStartDebugging {
		c.handleStartDebugging(rr)
		return
	}
	c.answerReverseRequest(rr, false, "herdr-edit does not implement the "+rr.Command+" request")
	c.log("declined the adapter's " + rr.Command + " request")
}

// handleStartDebugging accepts the FIRST child session an adapter asks for and
// refuses every later one out loud.
//
// 🔴 The refusal is the designed behaviour, not a limitation being papered
// over. This client tracks ONE active leaf session, so a second startDebugging
// — a browser next to a server, a worker thread, a forked process — has nowhere
// to go. The two ways to get that wrong are both silent: dropping it leaves the
// adapter blocked forever, and accepting it would REPLACE the session the user
// is looking at, so they would step through one process while the one they set
// a breakpoint in ran to completion unobserved. Refusing and saying which
// configuration was declined is the only version where what is on screen and
// what is running agree.
func (c *Client) handleStartDebugging(rr ReverseRequest) {
	var cs ChildSession
	if len(rr.Arguments) > 0 {
		if err := json.Unmarshal(rr.Arguments, &cs); err != nil {
			c.answerReverseRequest(rr, false, "unreadable startDebugging arguments")
			c.log("startDebugging: unreadable arguments: " + err.Error())
			return
		}
	}
	if cs.Request == "" {
		cs.Request = "launch" // the protocol's default, and what js-debug means
	}

	c.mu.Lock()
	first := !c.childClaimed
	if first {
		c.childClaimed = true
	}
	c.mu.Unlock()

	if !first {
		c.answerReverseRequest(rr, false,
			"herdr-edit debugs one session at a time; this child session was declined")
		c.log("declined a second debug session (" + describeChild(cs) + ") — " +
			"only one is debugged at a time, and the first one is still active")
		return
	}

	c.answerReverseRequest(rr, true, "")
	select {
	case c.childReq <- cs:
	default:
		// Unreachable while childClaimed gates entry, but a send to a full
		// buffered channel would block the read loop and take the whole session
		// down, so it is a default rather than a comment saying it cannot happen.
		c.log("startDebugging arrived twice past the claim guard; ignoring the second")
	}
}

// describeChild names a child session in a way a user can act on. The `name`
// js-debug puts in the configuration is the readable one ("fixture.js [96905]");
// the type is the fallback for an adapter that does not set one.
func describeChild(cs ChildSession) string {
	if cs.Configuration != nil {
		if n, ok := cs.Configuration["name"].(string); ok && n != "" {
			return n
		}
		if t, ok := cs.Configuration["type"].(string); ok && t != "" {
			return t
		}
	}
	return cs.Request
}

// answerReverseRequest writes the response an adapter is blocked on.
//
// A write failure is logged rather than returned: this runs on the read loop,
// there is no caller to hand an error to, and a connection too broken to answer
// on is about to surface as a read error anyway.
func (c *Client) answerReverseRequest(rr ReverseRequest, success bool, message string) {
	c.mu.Lock()
	c.nextSeq++
	seq := c.nextSeq
	c.mu.Unlock()

	if err := c.write(Response{
		Seq: seq, Type: TypeResponse, RequestSeq: rr.Seq,
		Success: success, Command: rr.Command, Message: message,
	}); err != nil {
		c.log("could not answer the adapter's " + rr.Command + " request: " + err.Error())
	}
}

// AwaitChildSession blocks until the adapter asks us to start a leaf session.
//
// 🔴 It may return IMMEDIATELY, and that is required rather than an
// optimisation: measured against js-debug, startDebugging arrives ~80ms after
// the launch response, which is well before the caller finishes the root's
// configuration sequence. The request is buffered by handleStartDebugging for
// exactly the same reason WaitEvent buffers `initialized` — a handshake step
// that can be overtaken by the thing it waits for is a hang.
func (c *Client) AwaitChildSession(ctx context.Context) (ChildSession, error) {
	select {
	case cs := <-c.childReq:
		return cs, nil
	case <-ctx.Done():
		return ChildSession{}, fmt.Errorf("%s: no child debug session was started: %w", c.name, ctx.Err())
	}
}

// DialChild opens a SECOND connection to the same adapter server, for the leaf
// session AwaitChildSession described.
//
// 🔴 Only TransportServer can do this. A stdio adapter has one stdin and a
// dialed-in socket adapter has one accepted connection, so dialAddr being empty
// is a programming error rather than a runtime condition — an adapter table
// entry that asked for child sessions on the wrong transport.
//
// The returned client owns no process: the root spawned the server and the root
// is what must outlive it. See Stop.
func (c *Client) DialChild(name string, h Handlers) (*Client, error) {
	if c.dialAddr == "" {
		return nil, fmt.Errorf("%s: this adapter's transport cannot carry a child session", c.name)
	}
	conn, err := net.Dial("tcp", c.dialAddr)
	if err != nil {
		return nil, fmt.Errorf("%s: dialing the child session at %s: %w", name, c.dialAddr, err)
	}
	child := newClient(name, conn, h)
	// 🔴 A LEAF has no session to give away, and saying so is the fix for a real
	// hang. newClient leaves childClaimed false, so a leaf that received its own
	// startDebugging answered success:true, buffered the request, and dropped it —
	// nobody calls AwaitChildSession on a leaf. The adapter then believes we
	// opened a session we never opened, and the debuggee's worker waits for a
	// debugger that never attaches. "One leaf session, enforced out loud" was
	// enforced only on the root; this extends it to the leaf, where a page with a
	// web worker makes it reachable.
	child.childClaimed = true
	go child.readLoop()
	return child, nil
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
		c.handlers.OnEvent(c, ev)
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
	//
	// 🔴 The send is NON-BLOCKING, and a plain send here deadlocks the read
	// goroutine for the rest of the process. Every pending channel is buffered
	// for one, and `call` removes its entry once answered — but `send`, which is
	// what Launch and Attach use, deliberately does NOT: the caller may read the
	// launch response long afterwards, or (as internal/app does) never. So by
	// the time a session ends, `pending` still holds the launch entry with its
	// one slot ALREADY FULL, and a blocking send stops right here — before the
	// waiters are closed and before the synthetic `terminated` event that tells
	// the UI the adapter is gone.
	//
	// The symptom is not a crash: the read goroutine simply never returns, so an
	// adapter that dies mid-session leaves the editor painting ▶ over a program
	// that no longer exists, with F5 bound to a dead client. Observed as a
	// 600-second test timeout with the goroutine parked on this line.
	//
	// A full buffer means the caller's answer is already waiting for it, so
	// there is nothing to deliver and nothing to report.
	for seq, ch := range pending {
		select {
		case ch <- Response{
			Type: TypeResponse, RequestSeq: seq, Success: false,
			Message: "adapter connection lost: " + err.Error(),
		}:
		default:
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
		c.handlers.OnEvent(c, Event{Type: TypeEvent, Event: EventTerminated})
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
		c.handlers.OnLog(c, c.name+": "+msg)
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
