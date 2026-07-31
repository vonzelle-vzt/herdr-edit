// =============================================================================
// File: internal/dap/client_test.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package dap

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// testTimeout bounds every fake-adapter exchange. These tests talk to an
// in-memory pipe, so anything approaching this is a hang, not slowness.
const testTimeout = 5 * time.Second

// fakeAdapter is the adapter half of a net.Pipe, with just enough framing to
// stand in for a real one. It exists so the ORDER of messages can be dictated
// exactly — which is the only way to test a hazard whose whole nature is that a
// real adapter may or may not exhibit it on any given run.
type fakeAdapter struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
	seq  int
}

// newFakePair wires a Client to a fakeAdapter over an in-memory pipe. The
// client goes through StartConn, which is the same code path StartCommand ends
// up in — a test that exercised a different one would prove nothing.
func newFakePair(t *testing.T, h Handlers) (*Client, *fakeAdapter) {
	t.Helper()
	clientEnd, adapterEnd := net.Pipe()
	_ = adapterEnd.SetDeadline(time.Now().Add(testTimeout))

	c := StartConn("fake", clientEnd, h)
	f := &fakeAdapter{t: t, conn: adapterEnd, r: bufio.NewReader(adapterEnd)}
	t.Cleanup(func() {
		adapterEnd.Close()
		clientEnd.Close()
	})
	return c, f
}

// stopFake shuts a test client down promptly.
//
// Closing the adapter end FIRST is the point: Stop sends a polite disconnect
// and waits disconnectTimeout for an answer, and a fake that is no longer
// reading would make every test pay that wait. With the peer closed the write
// fails at once and Stop takes the fast path — while still exercising the real
// Stop, which is what a test wants to cover.
func stopFake(c *Client, f *fakeAdapter) {
	f.conn.Close()
	c.Stop()
}

// readRequest reads one framed request from the client.
func (f *fakeAdapter) readRequest() Request {
	f.t.Helper()
	body, err := readMessage(f.r)
	if err != nil {
		f.t.Fatalf("fake adapter read: %v", err)
	}
	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		f.t.Fatalf("fake adapter decode: %v", err)
	}
	return req
}

// write frames and sends one message.
func (f *fakeAdapter) write(v interface{}) {
	f.t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		f.t.Fatalf("fake adapter marshal: %v", err)
	}
	if _, err := f.conn.Write([]byte("Content-Length: " + itoa(len(body)) + "\r\n\r\n")); err != nil {
		f.t.Fatalf("fake adapter write header: %v", err)
	}
	if _, err := f.conn.Write(body); err != nil {
		f.t.Fatalf("fake adapter write body: %v", err)
	}
}

// respond answers a request successfully with the given body.
func (f *fakeAdapter) respond(req Request, body interface{}) {
	f.t.Helper()
	f.seq++
	raw, err := json.Marshal(body)
	if err != nil {
		f.t.Fatalf("fake adapter body: %v", err)
	}
	f.write(Response{
		Seq: f.seq, Type: TypeResponse, RequestSeq: req.Seq,
		Success: true, Command: req.Command, Body: raw,
	})
}

// event sends an adapter event.
func (f *fakeAdapter) event(name string, body interface{}) {
	f.t.Helper()
	f.seq++
	var raw json.RawMessage
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			f.t.Fatalf("fake adapter event body: %v", err)
		}
		raw = b
	}
	f.write(Event{Seq: f.seq, Type: TypeEvent, Event: name, Body: raw})
}

// itoa keeps the fake's framing free of an fmt import, matching how small the
// real writer is.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// collector accumulates events delivered through Handlers.OnEvent from the read
// goroutine, so assertions can be made on the main one without a data race.
type collector struct {
	mu     sync.Mutex
	events []Event
	logs   []string
}

// handlers returns the Handlers wired to this collector.
func (c *collector) handlers() Handlers {
	return Handlers{
		OnEvent: func(e Event) {
			c.mu.Lock()
			c.events = append(c.events, e)
			c.mu.Unlock()
		},
		OnLog: func(s string) {
			c.mu.Lock()
			c.logs = append(c.logs, s)
			c.mu.Unlock()
		},
	}
}

// names returns the event names seen so far.
func (c *collector) names() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.events))
	for i, e := range c.events {
		out[i] = e.Event
	}
	return out
}

// waitForEventName polls until an event arrives or the deadline passes.
func (c *collector) waitForEventName(t *testing.T, name string) bool {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		for _, n := range c.names() {
			if n == name {
				return true
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// TestPendingKeysOnRequestSeqNotSeq is RED against correlating a response with
// its own Seq instead of RequestSeq.
//
// The two are BOTH 1 on a session's first response (measured against delve
// 1.27), so a Seq-keyed client demultiplexes correctly exactly once and then
// every later request times out — which reads as "the adapter is slow". Here
// the two fields deliberately differ AND the answers come back cross-wired, so
// a Seq-keyed client does not merely time out: it hands each caller the OTHER
// one's body, which is the quieter and worse failure.
func TestPendingKeysOnRequestSeqNotSeq(t *testing.T) {
	var col collector
	c, f := newFakePair(t, col.handlers())
	defer stopFake(c, f)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	type result struct {
		threads []Thread
		frames  []StackFrame
		err     error
	}
	threadsDone := make(chan result, 1)
	stackDone := make(chan result, 1)

	go func() {
		th, err := c.Threads(ctx)
		threadsDone <- result{threads: th, err: err}
	}()
	first := f.readRequest() // threads, seq 1

	go func() {
		fr, err := c.StackTrace(ctx, 1, 0)
		stackDone <- result{frames: fr, err: err}
	}()
	second := f.readRequest() // stackTrace, seq 2

	if first.Command != "threads" || second.Command != "stackTrace" {
		t.Fatalf("fake read %q then %q, want threads then stackTrace", first.Command, second.Command)
	}
	if first.Seq == second.Seq {
		t.Fatalf("both requests used seq %d; they must be distinct", first.Seq)
	}

	// Answer each request with a message whose OWN seq is the other request's
	// seq. Keyed on request_seq this is unambiguous; keyed on seq it is exactly
	// backwards.
	stackBody, _ := json.Marshal(stackTraceBody{StackFrames: []StackFrame{{ID: 1000, Name: "main.add", Line: 7}}})
	f.write(Response{
		Seq: first.Seq, Type: TypeResponse, RequestSeq: second.Seq,
		Success: true, Command: "stackTrace", Body: stackBody,
	})
	threadsBodyRaw, _ := json.Marshal(threadsBody{Threads: []Thread{{ID: 1, Name: "goroutine 1"}}})
	f.write(Response{
		Seq: second.Seq, Type: TypeResponse, RequestSeq: first.Seq,
		Success: true, Command: "threads", Body: threadsBodyRaw,
	})

	tr := <-threadsDone
	if tr.err != nil {
		t.Fatalf("threads: %v (a seq-keyed client never wakes this waiter)", tr.err)
	}
	if len(tr.threads) != 1 || tr.threads[0].Name != "goroutine 1" {
		t.Fatalf("threads got %+v — the waiter woke with the WRONG response body", tr.threads)
	}

	sr := <-stackDone
	if sr.err != nil {
		t.Fatalf("stackTrace: %v", sr.err)
	}
	if len(sr.frames) != 1 || sr.frames[0].Name != "main.add" {
		t.Fatalf("stackTrace got %+v — the waiter woke with the WRONG response body", sr.frames)
	}
}

// TestInitializedEventBeforeInitializeResponse is RED against copying
// lsp.Start's shape.
//
// The `initialized` event is allowed to arrive BEFORE the response to
// `initialize` — that is the spec, not a race, and it is what debugpy does. A
// client that calls initialize and only THEN starts waiting misses an event
// that already went past; configurationDone never fires and the session hangs
// with no error anywhere. LSP has no equivalent hazard, which is precisely why
// the resemblance between the two clients is dangerous.
//
// The assertion is not "WaitEvent returned" but "configurationDone actually
// reached the adapter", because that is the thing whose absence hangs a real
// session.
func TestInitializedEventBeforeInitializeResponse(t *testing.T) {
	var col collector
	c, f := newFakePair(t, col.handlers())
	defer stopFake(c, f)

	configurationDone := make(chan struct{})
	seqErr := make(chan error, 1)

	// The client half: the exact sequence internal/app performs.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		if _, err := c.Initialize(ctx, "go"); err != nil {
			seqErr <- err
			return
		}
		if _, err := c.Launch(map[string]interface{}{"mode": "debug"}); err != nil {
			seqErr <- err
			return
		}
		if err := c.WaitEvent(ctx, EventInitialized); err != nil {
			seqErr <- err
			return
		}
		if _, err := c.SetBreakpoints(ctx, Source{Path: "/x/main.go"}, []SourceBreakpoint{{Line: 7}}); err != nil {
			seqErr <- err
			return
		}
		if err := c.SetExceptionBreakpoints(ctx, []string{"unrecovered-panic"}); err != nil {
			seqErr <- err
			return
		}
		if err := c.ConfigurationDone(ctx); err != nil {
			seqErr <- err
			return
		}
		seqErr <- nil
	}()

	// The adapter half, ordered to spring the trap.
	go func() {
		initReq := f.readRequest()

		// 🔴 The event goes out FIRST, while the client is still blocked
		// waiting for the initialize response.
		f.event(EventInitialized, nil)
		f.respond(initReq, Capabilities{SupportsConfigurationDoneRequest: true})

		launchReq := f.readRequest()

		for {
			req := f.readRequest()
			switch req.Command {
			case "setBreakpoints":
				f.respond(req, setBreakpointsBody{Breakpoints: []Breakpoint{{ID: 1, Verified: true, Line: 7}}})
			case "setExceptionBreakpoints":
				f.respond(req, struct{}{})
			case "configurationDone":
				f.respond(req, struct{}{})
				// The launch response is deliberately withheld until here —
				// see TestLaunchDoesNotBlockOnItsResponse.
				f.respond(launchReq, struct{}{})
				close(configurationDone)
				return
			default:
				f.t.Errorf("unexpected request %q", req.Command)
				return
			}
		}
	}()

	select {
	case <-configurationDone:
	case <-time.After(testTimeout):
		t.Fatal("configurationDone never reached the adapter — the initialized event that arrived " +
			"before the initialize response was missed, and a real session would hang here with no error")
	}
	if err := <-seqErr; err != nil {
		t.Fatalf("configuration sequence: %v", err)
	}
}

// TestLaunchDoesNotBlockOnItsResponse is RED against a Launch that waits.
//
// The protocol lets an adapter withhold the launch response until
// configurationDone arrives, so blocking on it before sending breakpoints
// deadlocks by design. The fake here withholds it exactly that way; a blocking
// Launch never reaches setBreakpoints and the test times out.
func TestLaunchDoesNotBlockOnItsResponse(t *testing.T) {
	var col collector
	c, f := newFakePair(t, col.handlers())
	defer stopFake(c, f)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	reachedBreakpoints := make(chan struct{})

	go func() {
		initReq := f.readRequest()
		f.respond(initReq, Capabilities{SupportsConfigurationDoneRequest: true})
		launchReq := f.readRequest()
		f.event(EventInitialized, nil)

		bpReq := f.readRequest()
		close(reachedBreakpoints)
		f.respond(bpReq, setBreakpointsBody{Breakpoints: []Breakpoint{{ID: 1, Verified: true, Line: 7}}})
		f.respond(launchReq, struct{}{}) // only now
	}()

	if _, err := c.Initialize(ctx, "go"); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	launchResp, err := c.Launch(map[string]interface{}{"mode": "debug"})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if err := c.WaitEvent(ctx, EventInitialized); err != nil {
		t.Fatalf("wait initialized: %v", err)
	}
	if _, err := c.SetBreakpoints(ctx, Source{Path: "/x/main.go"}, []SourceBreakpoint{{Line: 7}}); err != nil {
		t.Fatalf("setBreakpoints: %v", err)
	}

	select {
	case <-reachedBreakpoints:
	case <-time.After(testTimeout):
		t.Fatal("setBreakpoints was never sent — Launch blocked on a response the adapter withholds until configurationDone")
	}
	select {
	case resp := <-launchResp:
		if !resp.Success {
			t.Errorf("launch response reported failure: %+v", resp)
		}
	case <-time.After(testTimeout):
		t.Fatal("the launch response never arrived on its channel")
	}
}

// TestSetBreakpointsSendsTheWholeFile pins that a call carries every breakpoint
// for the source. setBreakpoints REPLACES the file's set, so a caller that sent
// only the newly added one would silently clear the rest — and the symptom is
// breakpoints that "randomly stop working".
func TestSetBreakpointsSendsTheWholeFile(t *testing.T) {
	var col collector
	c, f := newFakePair(t, col.handlers())
	defer stopFake(c, f)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	got := make(chan setBreakpointsArgs, 1)
	go func() {
		req := f.readRequest()
		raw, _ := json.Marshal(req.Arguments)
		var args setBreakpointsArgs
		_ = json.Unmarshal(raw, &args)
		got <- args
		f.respond(req, setBreakpointsBody{Breakpoints: []Breakpoint{
			{ID: 1, Verified: true, Line: 7},
			{ID: 2, Verified: true, Line: 12},
			{ID: 3, Verified: true, Line: 20},
		}})
	}()

	want := []SourceBreakpoint{{Line: 7}, {Line: 12}, {Line: 20}}
	bps, err := c.SetBreakpoints(ctx, Source{Path: "/x/main.go"}, want)
	if err != nil {
		t.Fatalf("setBreakpoints: %v", err)
	}
	args := <-got
	if len(args.Breakpoints) != 3 {
		t.Fatalf("adapter received %d breakpoints, want all 3 — the request is whole-file", len(args.Breakpoints))
	}
	if args.Source.Path != "/x/main.go" {
		t.Errorf("source path = %q", args.Source.Path)
	}
	if len(bps) != 3 {
		t.Fatalf("got %d answers, want 3 (positional with the request)", len(bps))
	}
}

// TestSetBreakpointsNilMarshalsAsEmptyArray checks that clearing a file's
// breakpoints sends [] rather than null. A nil Go slice encodes as null, which
// adapters expecting an array reject — so "remove the last breakpoint" would
// error instead of clearing.
func TestSetBreakpointsNilMarshalsAsEmptyArray(t *testing.T) {
	var col collector
	c, f := newFakePair(t, col.handlers())
	defer stopFake(c, f)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	raw := make(chan []byte, 1)
	go func() {
		req := f.readRequest()
		b, _ := json.Marshal(req.Arguments)
		raw <- b
		f.respond(req, setBreakpointsBody{})
	}()

	if _, err := c.SetBreakpoints(ctx, Source{Path: "/x/main.go"}, nil); err != nil {
		t.Fatalf("setBreakpoints: %v", err)
	}
	body := string(<-raw)
	if !contains(body, `"breakpoints":[]`) {
		t.Fatalf("nil breakpoints did not marshal as an empty array: %s", body)
	}
}

// TestVerifiedBreakpointLineMayDiffer pins that the client returns the line the
// adapter BOUND, not the one we asked for. Reporting the requested line would
// paint the stopped marker somewhere the program never stops.
func TestVerifiedBreakpointLineMayDiffer(t *testing.T) {
	var col collector
	c, f := newFakePair(t, col.handlers())
	defer stopFake(c, f)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	go func() {
		req := f.readRequest()
		// Asked for line 5 (a comment); the adapter snapped it to 7.
		f.respond(req, setBreakpointsBody{Breakpoints: []Breakpoint{{ID: 1, Verified: true, Line: 7}}})
	}()

	bps, err := c.SetBreakpoints(ctx, Source{Path: "/x/main.go"}, []SourceBreakpoint{{Line: 5}})
	if err != nil {
		t.Fatalf("setBreakpoints: %v", err)
	}
	if len(bps) != 1 {
		t.Fatalf("got %d breakpoints, want 1", len(bps))
	}
	if bps[0].Line != 7 {
		t.Errorf("bound line = %d, want the adapter's 7 rather than the requested 5", bps[0].Line)
	}
	if !bps[0].HasLine() {
		t.Error("HasLine() false for a verified breakpoint with a line")
	}
}

// TestFailedResponseIsAnErrorNotAnEmptyResult is the client-level half of the
// success:false trap: a refusal must come back as an error carrying the
// adapter's reason, never as a successful empty stack trace.
func TestFailedResponseIsAnErrorNotAnEmptyResult(t *testing.T) {
	var col collector
	c, f := newFakePair(t, col.handlers())
	defer stopFake(c, f)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	go func() {
		req := f.readRequest()
		f.seq++
		f.write(Response{
			Seq: f.seq, Type: TypeResponse, RequestSeq: req.Seq, Success: false,
			Command: req.Command, Message: "Unable to produce stack trace",
			Body: json.RawMessage(`{"error":{"id":2004,"format":"unknown goroutine 999999"}}`),
		})
	}()

	frames, err := c.StackTrace(ctx, 999999, 0)
	if err == nil {
		t.Fatalf("a refused stackTrace returned no error and %d frames — an error is "+
			"indistinguishable from a program with no stack", len(frames))
	}
	if !contains(err.Error(), "unknown goroutine 999999") {
		t.Errorf("error %q does not carry the adapter's reason from body.error.format", err)
	}
}

// TestAdapterDeathPostsSyntheticTerminated covers the mid-session crash: on
// read-loop EOF the client must synthesise a terminated event and unblock every
// waiter. Without it the UI sits in "stopped" forever with F5 bound to a dead
// client, and nothing on screen says why.
func TestAdapterDeathPostsSyntheticTerminated(t *testing.T) {
	var col collector
	c, f := newFakePair(t, col.handlers())

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	callDone := make(chan error, 1)
	go func() {
		_, err := c.Threads(ctx)
		callDone <- err
	}()
	f.readRequest() // let the request land, then die without answering

	waitDone := make(chan error, 1)
	go func() { waitDone <- c.WaitEvent(ctx, EventStopped) }()
	time.Sleep(20 * time.Millisecond) // let the waiter register

	f.conn.Close()

	if !col.waitForEventName(t, EventTerminated) {
		t.Fatalf("no synthetic terminated event after the adapter died; saw %v", col.names())
	}
	select {
	case err := <-callDone:
		if err == nil {
			t.Error("an in-flight request returned success after the adapter died")
		}
	case <-time.After(testTimeout):
		t.Fatal("an in-flight request hung forever after the adapter died")
	}
	select {
	case <-waitDone:
	case <-time.After(testTimeout):
		t.Fatal("a WaitEvent caller hung forever after the adapter died")
	}
}

// TestEventsAreDeliveredInOrder checks that every event type stage 2 cares
// about reaches OnEvent. An event nobody handles is how a session hangs with
// nothing to explain it, so the list here is the full stage-2 set.
func TestEventsAreDeliveredInOrder(t *testing.T) {
	var col collector
	c, f := newFakePair(t, col.handlers())
	defer stopFake(c, f)

	want := []string{
		EventInitialized, EventProcess, EventOutput, EventThread,
		EventBreakpoint, EventStopped, EventContinued, EventExited, EventTerminated,
	}
	go func() {
		for _, name := range want {
			f.event(name, nil)
		}
	}()

	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) && len(col.names()) < len(want) {
		time.Sleep(5 * time.Millisecond)
	}
	got := col.names()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %q, want %q (order must be preserved)", i, got[i], want[i])
		}
	}
}

// TestCapabilitiesEventUpdatesCaps checks the late-capabilities path: an
// adapter may revise what it told us at initialize, and Caps() has to stay
// authoritative or Disconnect picks the wrong shutdown.
func TestCapabilitiesEventUpdatesCaps(t *testing.T) {
	var col collector
	c, f := newFakePair(t, col.handlers())
	defer stopFake(c, f)

	if c.Caps().SupportsTerminateRequest {
		t.Fatal("fresh client already claims terminate support")
	}
	go f.event(EventCapabilities, CapabilitiesEvent{
		Capabilities: Capabilities{SupportsTerminateRequest: true},
	})

	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		if c.Caps().SupportsTerminateRequest {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("a capabilities event did not update Caps()")
}

// TestWaitEventReturnsForAnAlreadySeenEvent isolates the buffering rule from
// the full handshake: an event that arrived before anyone asked must still
// satisfy a later wait.
func TestWaitEventReturnsForAnAlreadySeenEvent(t *testing.T) {
	var col collector
	c, f := newFakePair(t, col.handlers())
	defer stopFake(c, f)

	go f.event(EventInitialized, nil)
	if !col.waitForEventName(t, EventInitialized) {
		t.Fatal("the event never arrived at all")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitEvent(ctx, EventInitialized); err != nil {
		t.Fatalf("WaitEvent for an already-seen event: %v — a real session hangs here", err)
	}
}

// TestWaitEventTimesOutForAnEventThatNeverComes checks the other direction: the
// buffering must not make every wait succeed.
func TestWaitEventTimesOutForAnEventThatNeverComes(t *testing.T) {
	var col collector
	c, f := newFakePair(t, col.handlers())
	defer stopFake(c, f)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := c.WaitEvent(ctx, EventStopped); err == nil {
		t.Fatal("WaitEvent returned success for an event that was never sent")
	}
}

// TestShortSocketPathFitsTheAFUnixLimit guards the trap that surfaces as "the
// adapter never started" rather than as a path error. A path derived from
// t.TempDir() is ~97 characters before a filename is even appended, which is
// why StartCommand builds its own.
func TestShortSocketPathFitsTheAFUnixLimit(t *testing.T) {
	sock, err := shortSocketPath()
	if err != nil {
		t.Fatalf("shortSocketPath: %v", err)
	}
	defer os.RemoveAll(filepath.Dir(sock))

	if len(sock) > 100 {
		t.Fatalf("socket path %q is %d bytes — over the ~104 byte AF_UNIX cap", sock, len(sock))
	}
	// Prove it actually binds, which is the property that matters. A length
	// assertion alone would pass on a platform with a different limit.
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("could not bind %q (%d bytes): %v", sock, len(sock), err)
	}
	ln.Close()

	// And demonstrate the trap is real: a t.TempDir()-derived path is longer.
	tooLong := t.TempDir() + "/dap.sock"
	t.Logf("t.TempDir() socket path would be %d bytes: %s", len(tooLong), tooLong)
}

// TestSteppingRequestsAreSpelledCorrectly pins the three step commands' wire
// spelling and their argument.
//
// 🔴 The names are camelCase and the protocol has no tolerance for a miss:
// "stepin" or "step_in" comes back as an unknown-command error, and the app
// surfaces that on a status line the user has probably stopped reading. Worse,
// each of them takes a threadId and NOT a frameId, and an adapter handed a
// frame id where a thread id belongs steps some other goroutine.
func TestSteppingRequestsAreSpelledCorrectly(t *testing.T) {
	c, f := newFakePair(t, Handlers{})
	defer stopFake(c, f)

	for _, tc := range []struct {
		command string
		call    func(context.Context) error
	}{
		{"next", func(ctx context.Context) error { return c.Next(ctx, 17) }},
		{"stepIn", func(ctx context.Context) error { return c.StepIn(ctx, 17) }},
		{"stepOut", func(ctx context.Context) error { return c.StepOut(ctx, 17) }},
		{"pause", func(ctx context.Context) error { return c.Pause(ctx, 17) }},
	} {
		done := make(chan error, 1)
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		go func() { done <- tc.call(ctx) }()

		req := f.readRequest()
		if req.Command != tc.command {
			t.Errorf("request command = %q, want %q", req.Command, tc.command)
		}
		args, _ := json.Marshal(req.Arguments)
		var got threadArgs
		if err := json.Unmarshal(args, &got); err != nil {
			t.Fatalf("%s arguments: %v (raw %s)", tc.command, err, args)
		}
		if got.ThreadID != 17 {
			t.Errorf("%s carried threadId %d, want 17 (raw %s)", tc.command, got.ThreadID, args)
		}
		f.respond(req, struct{}{})
		if err := <-done; err != nil {
			t.Errorf("%s: %v", tc.command, err)
		}
		cancel()
	}
}

// TestScopesAndVariablesCarryTheRightHandles pins that scopes takes a frameId
// and variables takes a variablesReference — two integers that are freely
// interchangeable as far as the wire is concerned, and whose confusion produces
// a plausible wrong answer rather than an error.
func TestScopesAndVariablesCarryTheRightHandles(t *testing.T) {
	c, f := newFakePair(t, Handlers{})
	defer stopFake(c, f)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	type result struct {
		scopes []Scope
		vars   []Variable
		err    error
	}
	done := make(chan result, 1)
	go func() {
		sc, err := c.Scopes(ctx, 1004)
		if err != nil {
			done <- result{err: err}
			return
		}
		v, err := c.Variables(ctx, sc[0].VariablesReference, 0, 200)
		done <- result{scopes: sc, vars: v, err: err}
	}()

	req := f.readRequest()
	if req.Command != "scopes" {
		t.Fatalf("first request = %q, want scopes", req.Command)
	}
	raw, _ := json.Marshal(req.Arguments)
	if !contains(string(raw), `"frameId":1004`) {
		t.Errorf("scopes arguments = %s, want frameId 1004", raw)
	}
	f.respond(req, scopesBody{Scopes: []Scope{{Name: "Locals", VariablesReference: 1000}}})

	req = f.readRequest()
	if req.Command != "variables" {
		t.Fatalf("second request = %q, want variables", req.Command)
	}
	raw, _ = json.Marshal(req.Arguments)
	if !contains(string(raw), `"variablesReference":1000`) {
		t.Errorf("variables arguments = %s, want the SCOPE's reference 1000, not the frame id", raw)
	}
	f.respond(req, variablesBody{Variables: []Variable{{Name: "sum", Value: "5", Type: "int"}}})

	res := <-done
	if res.err != nil {
		t.Fatalf("chain: %v", res.err)
	}
	if len(res.vars) != 1 || res.vars[0].Name != "sum" || res.vars[0].Value != "5" {
		t.Errorf("variables = %+v", res.vars)
	}
}

// TestVariablePagingIsOnlySentWhenTheAdapterSupportsIt is the gate's oracle,
// driven from BOTH sides.
//
// 🔴 Sending start/count to an adapter without supportsVariablePaging is not a
// harmless extra: it IGNORES them rather than refusing, so the client would
// believe it had bounded an answer it had not — and delve is exactly that
// adapter (measured: it omits the capability). A test that only checked the
// supported case would leave the dangerous half unmeasured.
func TestVariablePagingIsOnlySentWhenTheAdapterSupportsIt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		paging  bool
		wantKey bool
	}{
		{"adapter without paging (delve)", false, false},
		{"adapter with paging", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, f := newFakePair(t, Handlers{})
			defer stopFake(c, f)

			c.mu.Lock()
			c.caps = Capabilities{SupportsVariablePaging: tc.paging}
			c.mu.Unlock()

			ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := c.Variables(ctx, 1000, 0, 200)
				done <- err
			}()

			req := f.readRequest()
			raw, _ := json.Marshal(req.Arguments)
			if got := contains(string(raw), `"count":200`); got != tc.wantKey {
				t.Errorf("arguments %s carried count=%v, want %v", raw, got, tc.wantKey)
			}
			f.respond(req, variablesBody{})
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestStopFallsBackToDisconnectWithoutTerminateSupport is the guard for the
// measured delve behaviour: supportsTerminateRequest is ABSENT from its
// capabilities, so a stop wired straight to `terminate` would come back as an
// unknown command — the editor would drop the session, the debugged process
// would keep running, and nothing on screen would say so.
func TestStopFallsBackToDisconnectWithoutTerminateSupport(t *testing.T) {
	c, f := newFakePair(t, Handlers{})

	// No capabilities at all: exactly what delve reports for this one.
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.TerminateOrDisconnect(ctx) }()

	req := f.readRequest()
	if req.Command != "disconnect" {
		t.Fatalf("request = %q, want disconnect — terminate is not supported here", req.Command)
	}
	raw, _ := json.Marshal(req.Arguments)
	if !contains(string(raw), `"terminateDebuggee":true`) {
		t.Errorf("disconnect arguments = %s, want terminateDebuggee true or the debuggee outlives us", raw)
	}
	f.respond(req, struct{}{})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	stopFake(c, f)
}

// TestTerminateIsUsedWhenAdvertised covers the other branch, and it is why
// isShutdownCommand exempts terminate as well as disconnect.
//
// 🔴 Stop marks the client closed BEFORE asking it to end the debuggee. With
// only disconnect exempt from that guard, terminate would be refused by our own
// code, silently fall through to disconnect, and appear to work — leaving the
// polite path permanently unreachable while still looking tested.
func TestTerminateIsUsedWhenAdvertised(t *testing.T) {
	c, f := newFakePair(t, Handlers{})

	c.mu.Lock()
	c.caps = Capabilities{SupportsTerminateRequest: true}
	c.closed = true // as Stop leaves it
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.TerminateOrDisconnect(ctx) }()

	req := f.readRequest()
	if req.Command != "terminate" {
		t.Fatalf("request = %q, want terminate for an adapter that advertises it", req.Command)
	}
	f.respond(req, struct{}{})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	stopFake(c, f)
}

// TestTerminateRefusalStillKillsTheDebuggee pins the fall-through: an adapter
// may advertise terminate and still refuse a particular one, and "we asked
// nicely and it said no" must not be the end of the story when a live process
// is at stake.
func TestTerminateRefusalStillKillsTheDebuggee(t *testing.T) {
	c, f := newFakePair(t, Handlers{})

	c.mu.Lock()
	c.caps = Capabilities{SupportsTerminateRequest: true}
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.TerminateOrDisconnect(ctx) }()

	req := f.readRequest()
	if req.Command != "terminate" {
		t.Fatalf("first request = %q, want terminate", req.Command)
	}
	f.seq++
	f.write(Response{
		Seq: f.seq, Type: TypeResponse, RequestSeq: req.Seq,
		Success: false, Command: "terminate", Message: "not right now",
	})

	req = f.readRequest()
	if req.Command != "disconnect" {
		t.Fatalf("after a refused terminate the request was %q, want disconnect", req.Command)
	}
	f.respond(req, struct{}{})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	stopFake(c, f)
}

// TestInitializeClaimsOnlyWhatWeHonour pins the capability declaration.
//
// Claiming supportsRunInTerminalRequest would make an adapter send US a request
// it then blocks on, and nothing here answers a reverse request. The other two
// are claimed because stage 3 genuinely reads them — and NOT claiming
// supportsVariableType would entitle an adapter to omit the type attribute, so
// the variables picker would show no types and look like the adapter did not
// know them.
func TestInitializeClaimsOnlyWhatWeHonour(t *testing.T) {
	args := initializeArgsForClient("go")
	if !args.SupportsVariableType {
		t.Error("supportsVariableType is not claimed, but the variables picker reads Variable.Type")
	}
	if !args.SupportsVariablePaging {
		t.Error("supportsVariablePaging is not claimed, but Variables sends start/count")
	}
	if args.SupportsRunInTerminalRequest {
		t.Error("supportsRunInTerminalRequest is claimed; nothing here answers a reverse request")
	}
	if !args.LinesStartAt1 || !args.ColumnsStartAt1 {
		t.Error("the 1-based declaration this package's whole coordinate contract rests on is missing")
	}
}

// contains is a tiny substring helper so assertions read as prose.
func contains(hay, needle string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
