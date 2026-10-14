// =============================================================================
// File: internal/dap/evaluate_test.go
// Author: Vonzelle Brown
// Created: 2026-10-14
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package dap

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// evalOutcome carries one SafeEvaluate result out of its goroutine.
type evalOutcome struct {
	res EvalResult
	err error
}

// hoverCaps is an adapter that can evaluate without side effects and cancel.
var hoverCaps = EvalCaps{SupportsEvaluateForHovers: true, SupportsCancelRequest: true}

// stoppedAt is a pause on thread 1 with no frame selected.
var stoppedAt = PauseState{Stopped: true, ThreadID: 1}

// startSafeEvaluate runs SafeEvaluate on a goroutine, as the socket command
// will, so the test goroutine can play the adapter.
func startSafeEvaluate(c *Client, caps EvalCaps, st PauseState, req EvalRequest, lim EvalLimits) <-chan evalOutcome {
	done := make(chan evalOutcome, 1)
	go func() {
		res, err := SafeEvaluate(context.Background(), c, caps, st, req, lim)
		done <- evalOutcome{res, err}
	}()
	return done
}

// argsOf decodes a request's arguments as the adapter would see them on the
// wire, so presence and absence of a field are both observable.
func argsOf(t *testing.T, req Request) map[string]interface{} {
	t.Helper()
	raw, err := json.Marshal(req.Arguments)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// answerStack reads the stackTrace request a safe evaluate opens with and
// answers it with the given frame ids.
func answerStack(t *testing.T, f *fakeAdapter, ids ...int) {
	t.Helper()
	req := f.readRequest()
	if req.Command != "stackTrace" {
		t.Fatalf("first request = %q, want stackTrace (frame validation)", req.Command)
	}
	frames := make([]StackFrame, 0, len(ids))
	for _, id := range ids {
		frames = append(frames, StackFrame{ID: id, Name: "f"})
	}
	f.respond(req, stackTraceBody{StackFrames: frames})
}

// assertNothingWasSent proves a refusal wrote nothing: the very next request
// the adapter sees must be an unrelated threads probe.
func assertNothingWasSent(t *testing.T, c *Client, f *fakeAdapter) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	errc := make(chan error, 1)
	go func() {
		_, err := c.Threads(ctx)
		errc <- err
	}()
	req := f.readRequest()
	if req.Command != "threads" {
		t.Fatalf("adapter received %q; a refused evaluate must write nothing", req.Command)
	}
	f.respond(req, threadsBody{})
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

// TestSafeEvaluateRefusedWhileRunning pins contract item 1: frame ids and
// variables references only mean something during a stop, so an evaluate (and
// a frame listing) while running is refused before anything reaches the
// adapter.
func TestSafeEvaluateRefusedWhileRunning(t *testing.T) {
	c, f := newFakePair(t, Handlers{})
	defer stopFake(c, f)

	got := <-startSafeEvaluate(c, hoverCaps, PauseState{Stopped: false, ThreadID: 1},
		EvalRequest{Expression: "x", Trusted: true}, DefaultEvalLimits())
	if !errors.Is(got.err, ErrEvalNotStopped) {
		t.Fatalf("err = %v, want ErrEvalNotStopped", got.err)
	}
	if _, err := PausedFrames(context.Background(), c, hoverCaps, PauseState{}, time.Second); !errors.Is(err, ErrEvalNotStopped) {
		t.Fatalf("PausedFrames while running: err = %v, want ErrEvalNotStopped", err)
	}
	assertNothingWasSent(t, c, f)
}

// TestSafeEvaluateRefusedWhenUntrusted pins contract item 4: the trust gate is
// checked first, ahead of every other decision, and writes nothing.
func TestSafeEvaluateRefusedWhenUntrusted(t *testing.T) {
	c, f := newFakePair(t, Handlers{})
	defer stopFake(c, f)

	got := <-startSafeEvaluate(c, hoverCaps, stoppedAt,
		EvalRequest{Expression: "x", Context: EvalContextWatch, AllowSideEffects: true}, DefaultEvalLimits())
	if !errors.Is(got.err, ErrEvalUntrusted) {
		t.Fatalf("err = %v, want ErrEvalUntrusted", got.err)
	}
	assertNothingWasSent(t, c, f)
}

// TestSafeEvaluateDefaultsToHoverInTheSelectedFrame pins contract items 1-2:
// with no context the request goes out as hover, carries no side-effect
// warning, and is evaluated in the SELECTED frame — including frame id 0, which
// is js-debug's innermost frame and which Client.Evaluate would omit for hover.
func TestSafeEvaluateDefaultsToHoverInTheSelectedFrame(t *testing.T) {
	c, f := newFakePair(t, Handlers{})
	defer stopFake(c, f)

	sel := 0
	st := PauseState{Stopped: true, ThreadID: 1, SelectedFrameID: &sel}
	done := startSafeEvaluate(c, hoverCaps, st, EvalRequest{Expression: "n", Trusted: true}, DefaultEvalLimits())

	answerStack(t, f, 7, 0)
	req := f.readRequest()
	if req.Command != "evaluate" {
		t.Fatalf("command = %q, want evaluate", req.Command)
	}
	args := argsOf(t, req)
	if args["context"] != EvalContextHover {
		t.Errorf("context = %v, want hover by default", args["context"])
	}
	if id, ok := args["frameId"].(float64); !ok || id != 0 {
		t.Errorf("frameId = %v (present=%v), want the selected frame 0 sent explicitly", args["frameId"], ok)
	}
	f.respond(req, EvaluateResult{Result: "42", Type: "int"})

	got := <-done
	if got.err != nil {
		t.Fatalf("SafeEvaluate: %v", got.err)
	}
	if got.res.SideEffectWarning {
		t.Error("hover evaluate carries sideEffectWarning; it should only flag watch/repl")
	}
	if got.res.Context != EvalContextHover || got.res.FrameID != 0 || got.res.Adapter != "fake" {
		t.Errorf("result metadata = %+v", got.res)
	}
	if got.res.Root == nil || got.res.Root.Value != "42" || got.res.Truncated {
		t.Errorf("root = %+v truncated=%v, want 42 untruncated", got.res.Root, got.res.Truncated)
	}
}

// TestSafeEvaluateDefaultsToTheInnermostFrame checks that with neither a
// requested nor a selected frame, the top of the paused stack is used.
func TestSafeEvaluateDefaultsToTheInnermostFrame(t *testing.T) {
	c, f := newFakePair(t, Handlers{})
	defer stopFake(c, f)

	done := startSafeEvaluate(c, hoverCaps, stoppedAt, EvalRequest{Expression: "n", Trusted: true}, DefaultEvalLimits())
	answerStack(t, f, 1000, 1001)
	req := f.readRequest()
	if id, _ := argsOf(t, req)["frameId"].(float64); id != 1000 {
		t.Errorf("frameId = %v, want innermost 1000", id)
	}
	f.respond(req, EvaluateResult{Result: "1"})
	if got := <-done; got.err != nil {
		t.Fatal(got.err)
	}
}

// TestSafeEvaluateRefusesAFrameThatIsNotPaused checks a stale frame id (one
// left over from before a step) is refused instead of being answered from some
// other scope, and that no evaluate is sent for it.
func TestSafeEvaluateRefusesAFrameThatIsNotPaused(t *testing.T) {
	c, f := newFakePair(t, Handlers{})
	defer stopFake(c, f)

	stale := 99
	done := startSafeEvaluate(c, hoverCaps, stoppedAt,
		EvalRequest{Expression: "n", FrameID: &stale, Trusted: true}, DefaultEvalLimits())
	answerStack(t, f, 1, 2)
	if got := <-done; !errors.Is(got.err, ErrEvalUnknownFrame) {
		t.Fatalf("err = %v, want ErrEvalUnknownFrame", got.err)
	}
	assertNothingWasSent(t, c, f)
}

// TestSafeEvaluateHoverWithoutAdapterSupportIsRefused checks the default is
// refused, not quietly upgraded to watch, when the adapter cannot promise a
// side-effect-free evaluation.
func TestSafeEvaluateHoverWithoutAdapterSupportIsRefused(t *testing.T) {
	c, f := newFakePair(t, Handlers{})
	defer stopFake(c, f)

	got := <-startSafeEvaluate(c, EvalCaps{}, stoppedAt, EvalRequest{Expression: "x", Trusted: true}, DefaultEvalLimits())
	if !errors.Is(got.err, ErrEvalNoHover) {
		t.Fatalf("err = %v, want ErrEvalNoHover", got.err)
	}
	assertNothingWasSent(t, c, f)
}

// TestSafeEvaluateWatchNeedsTheSideEffectFlag pins contract item 2: watch and
// repl are refused without --allow-side-effects, and with it the request goes
// out in that context and the answer carries sideEffectWarning. Unknown
// contexts are refused outright.
func TestSafeEvaluateWatchNeedsTheSideEffectFlag(t *testing.T) {
	c, f := newFakePair(t, Handlers{})
	defer stopFake(c, f)

	for _, ctxName := range []string{EvalContextWatch, EvalContextRepl} {
		got := <-startSafeEvaluate(c, hoverCaps, stoppedAt,
			EvalRequest{Expression: "f()", Context: ctxName, Trusted: true}, DefaultEvalLimits())
		if !errors.Is(got.err, ErrEvalSideEffects) {
			t.Fatalf("%s without flag: err = %v, want ErrEvalSideEffects", ctxName, got.err)
		}
	}
	got := <-startSafeEvaluate(c, hoverCaps, stoppedAt,
		EvalRequest{Expression: "f()", Context: "clipboard", AllowSideEffects: true, Trusted: true}, DefaultEvalLimits())
	if !errors.Is(got.err, ErrEvalBadContext) {
		t.Fatalf("unknown context: err = %v, want ErrEvalBadContext", got.err)
	}
	assertNothingWasSent(t, c, f)

	done := startSafeEvaluate(c, hoverCaps, stoppedAt,
		EvalRequest{Expression: "f()", Context: EvalContextWatch, AllowSideEffects: true, Trusted: true}, DefaultEvalLimits())
	answerStack(t, f, 3)
	req := f.readRequest()
	if ctx := argsOf(t, req)["context"]; ctx != EvalContextWatch {
		t.Errorf("context = %v, want watch", ctx)
	}
	f.respond(req, EvaluateResult{Result: "ok"})
	res := <-done
	if res.err != nil {
		t.Fatal(res.err)
	}
	if !res.res.SideEffectWarning {
		t.Error("watch with --allow-side-effects must carry sideEffectWarning:true")
	}
	if raw, _ := json.Marshal(res.res); !strings.Contains(string(raw), `"sideEffectWarning":true`) {
		t.Errorf("wire form %s lacks sideEffectWarning:true", raw)
	}
}

// TestSafeEvaluateTimeoutSendsCancelWhenSupported pins contract item 3: an
// adapter that never answers is given up on at the timeout, and when it
// advertised supportsCancelRequest a cancel naming the evaluate's seq follows.
func TestSafeEvaluateTimeoutSendsCancelWhenSupported(t *testing.T) {
	c, f := newFakePair(t, Handlers{})
	defer stopFake(c, f)

	lim := DefaultEvalLimits()
	lim.Timeout = 150 * time.Millisecond
	start := time.Now()
	done := startSafeEvaluate(c, hoverCaps, stoppedAt, EvalRequest{Expression: "for{}", Trusted: true}, lim)

	answerStack(t, f, 1)
	ev := f.readRequest()
	if ev.Command != "evaluate" {
		t.Fatalf("command = %q, want evaluate", ev.Command)
	}
	// Never answered.
	cancelReq := f.readRequest()
	if cancelReq.Command != "cancel" {
		t.Fatalf("after the timeout the adapter received %q, want cancel", cancelReq.Command)
	}
	if id, _ := argsOf(t, cancelReq)["requestId"].(float64); int(id) != ev.Seq {
		t.Errorf("cancel requestId = %v, want the evaluate's seq %d", id, ev.Seq)
	}
	got := <-done
	if !errors.Is(got.err, ErrEvalTimeout) {
		t.Fatalf("err = %v, want ErrEvalTimeout", got.err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("timeout took %v, want about %v", el, lim.Timeout)
	}
}

// TestSafeEvaluateTimeoutDropsTheResultWithoutCancel checks the other half of
// item 3: with no cancel support nothing extra is sent, and the adapter's late
// answer is dropped — it does not wedge the client or leak into the next
// request's result.
func TestSafeEvaluateTimeoutDropsTheResultWithoutCancel(t *testing.T) {
	c, f := newFakePair(t, Handlers{})
	defer stopFake(c, f)

	lim := DefaultEvalLimits()
	lim.Timeout = 100 * time.Millisecond
	done := startSafeEvaluate(c, EvalCaps{SupportsEvaluateForHovers: true}, stoppedAt,
		EvalRequest{Expression: "slow", Trusted: true}, lim)
	answerStack(t, f, 1)
	ev := f.readRequest()

	got := <-done
	if !errors.Is(got.err, ErrEvalTimeout) || !strings.Contains(got.err.Error(), "result dropped") {
		t.Fatalf("err = %v, want ErrEvalTimeout with the result dropped", got.err)
	}
	f.respond(ev, EvaluateResult{Result: "too late"})
	assertNothingWasSent(t, c, f)
}

// fakeTree serves stackTrace, evaluate and variables from a fixed reference
// map until the connection closes, recording the order references are read
// in. It writes with its own quiet framing because it runs off the test
// goroutine, where t.Fatalf is not allowed.
type fakeTree struct {
	mu   sync.Mutex
	refs []int
}

// serve answers requests until the pipe closes.
func (ft *fakeTree) serve(f *fakeAdapter, root EvaluateResult, tree map[int][]Variable) {
	seq := 1000
	for {
		body, err := readMessage(f.r)
		if err != nil {
			return
		}
		var req Request
		if json.Unmarshal(body, &req) != nil {
			return
		}
		var out interface{}
		switch req.Command {
		case "stackTrace":
			out = stackTraceBody{StackFrames: []StackFrame{{ID: 1}}}
		case "evaluate":
			out = root
		case "variables":
			var a variablesArgs
			raw, _ := json.Marshal(req.Arguments)
			_ = json.Unmarshal(raw, &a)
			ft.mu.Lock()
			ft.refs = append(ft.refs, a.VariablesReference)
			ft.mu.Unlock()
			out = variablesBody{Variables: tree[a.VariablesReference]}
		default:
			continue
		}
		seq++
		raw, _ := json.Marshal(out)
		msg, _ := json.Marshal(Response{Seq: seq, Type: TypeResponse, RequestSeq: req.Seq, Success: true, Command: req.Command, Body: raw})
		if _, err := f.conn.Write([]byte("Content-Length: " + itoa(len(msg)) + "\r\n\r\n")); err != nil {
			return
		}
		if _, err := f.conn.Write(msg); err != nil {
			return
		}
	}
}

// evalTreeWith runs one SafeEvaluate against a served tree.
func evalTreeWith(t *testing.T, root EvaluateResult, tree map[int][]Variable, lim EvalLimits) (EvalResult, []int) {
	t.Helper()
	c, f := newFakePair(t, Handlers{})
	defer stopFake(c, f)
	var ft fakeTree
	go ft.serve(f, root, tree)
	got := <-startSafeEvaluate(c, hoverCaps, stoppedAt, EvalRequest{Expression: "v", Trusted: true}, lim)
	if got.err != nil {
		t.Fatalf("SafeEvaluate: %v", got.err)
	}
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return got.res, append([]int(nil), ft.refs...)
}

// countNodes walks a result tree.
func countNodes(n *EvalNode) int {
	if n == nil {
		return 0
	}
	total := 1
	for _, c := range n.Children {
		total += countNodes(c)
	}
	return total
}

// TestSafeEvaluateExpandsBreadthFirst checks variablesReference expansion
// visits every node at one level before any at the next, so a cap trims the
// deep detail rather than a visible sibling.
func TestSafeEvaluateExpandsBreadthFirst(t *testing.T) {
	tree := map[int][]Variable{
		1:  {{Name: "a", VariablesReference: 10}, {Name: "b", VariablesReference: 20}},
		10: {{Name: "a1", VariablesReference: 11}},
		20: {{Name: "b1", Value: "2"}},
		11: {{Name: "a11", Value: "1"}},
	}
	res, refs := evalTreeWith(t, EvaluateResult{Result: "obj", VariablesReference: 1}, tree, DefaultEvalLimits())
	if want := []int{1, 10, 20, 11}; !equalInts(refs, want) {
		t.Errorf("variables order = %v, want breadth-first %v", refs, want)
	}
	if res.Truncated || res.Nodes != 6 || countNodes(res.Root) != 6 {
		t.Errorf("nodes = %d (tree %d) truncated=%v, want 6 untruncated", res.Nodes, countNodes(res.Root), res.Truncated)
	}
}

// TestSafeEvaluateCapsTruncate pins contract item 3's three size caps. Each
// one must stop the walk AND say so with truncated:true, because a silently
// partial tree reads as the whole value.
func TestSafeEvaluateCapsTruncate(t *testing.T) {
	t.Run("nodes", func(t *testing.T) {
		kids := make([]Variable, 10)
		for i := range kids {
			kids[i] = Variable{Name: "k", Value: "v"}
		}
		lim := DefaultEvalLimits()
		lim.MaxNodes = 5
		res, _ := evalTreeWith(t, EvaluateResult{Result: "list", VariablesReference: 1}, map[int][]Variable{1: kids}, lim)
		if res.Nodes != 5 || countNodes(res.Root) != 5 {
			t.Errorf("nodes = %d (tree %d), want the cap of 5", res.Nodes, countNodes(res.Root))
		}
		if !res.Truncated || !res.Root.ChildrenTruncated || !containsStr(res.TruncatedBy, "nodes") {
			t.Errorf("truncated=%v by=%v rootChildrenTruncated=%v, want truncated by nodes",
				res.Truncated, res.TruncatedBy, res.Root.ChildrenTruncated)
		}
	})

	t.Run("depth", func(t *testing.T) {
		// An endless chain, as a self-referencing object graph presents.
		tree := map[int][]Variable{}
		for ref := 1; ref < 50; ref++ {
			tree[ref] = []Variable{{Name: "next", VariablesReference: ref + 1}}
		}
		lim := DefaultEvalLimits()
		res, refs := evalTreeWith(t, EvaluateResult{Result: "node", VariablesReference: 1}, tree, lim)
		if len(refs) != lim.MaxDepth {
			t.Errorf("expanded %d levels, want the depth cap %d", len(refs), lim.MaxDepth)
		}
		if !res.Truncated || !containsStr(res.TruncatedBy, "depth") {
			t.Errorf("truncated=%v by=%v, want truncated by depth", res.Truncated, res.TruncatedBy)
		}
	})

	t.Run("output", func(t *testing.T) {
		lim := DefaultEvalLimits()
		lim.MaxOutBytes = 64
		big := strings.Repeat("é", 100) // 200 bytes; the clip must not split a rune
		res, refs := evalTreeWith(t, EvaluateResult{Result: big, VariablesReference: 1},
			map[int][]Variable{1: {{Name: "x"}}}, lim)
		if !res.Truncated || !containsStr(res.TruncatedBy, "output") {
			t.Errorf("truncated=%v by=%v, want truncated by output", res.Truncated, res.TruncatedBy)
		}
		if res.OutBytes > lim.MaxOutBytes || len(res.Root.Value) > lim.MaxOutBytes {
			t.Errorf("output %d bytes (value %d), want at most %d", res.OutBytes, len(res.Root.Value), lim.MaxOutBytes)
		}
		if !strings.HasPrefix(big, res.Root.Value) || strings.ContainsRune(res.Root.Value, '�') {
			t.Errorf("clipped value %q is not a clean prefix", res.Root.Value)
		}
		if len(refs) != 0 {
			t.Errorf("expanded %v after the output cap was hit", refs)
		}
	})
}

// TestEvaluateFramesListsPausedFrames checks the frames command's source: the
// stopped thread's stack, asked for by thread id.
func TestEvaluateFramesListsPausedFrames(t *testing.T) {
	c, f := newFakePair(t, Handlers{})
	defer stopFake(c, f)

	type out struct {
		frames []StackFrame
		err    error
	}
	done := make(chan out, 1)
	go func() {
		fr, err := PausedFrames(context.Background(), c, hoverCaps, PauseState{Stopped: true, ThreadID: 7}, time.Second)
		done <- out{fr, err}
	}()
	req := f.readRequest()
	if id, _ := argsOf(t, req)["threadId"].(float64); req.Command != "stackTrace" || id != 7 {
		t.Fatalf("request = %s threadId=%v, want stackTrace for thread 7", req.Command, id)
	}
	f.respond(req, stackTraceBody{StackFrames: []StackFrame{{ID: 0, Name: "main"}, {ID: 1, Name: "caller"}}})
	got := <-done
	if got.err != nil || len(got.frames) != 2 || got.frames[0].Name != "main" {
		t.Fatalf("frames = %+v err = %v", got.frames, got.err)
	}
}

// equalInts compares two int slices.
func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// containsStr reports whether s is in list.
func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// TestInitializeForEvalKeepsTheEvalCapabilities pins finding 2 at the source:
// the two capabilities Capabilities drops are read from the adapter's real
// initialize answer, both when advertised and when not, and Caps() is still
// recorded exactly as Initialize records it.
func TestInitializeForEvalKeepsTheEvalCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name string
		body map[string]interface{}
		want EvalCaps
	}{
		{"both supported", map[string]interface{}{
			"supportsEvaluateForHovers": true, "supportsCancelRequest": true, "supportsTerminateRequest": true,
		}, EvalCaps{SupportsEvaluateForHovers: true, SupportsCancelRequest: true}},
		{"both unsupported", map[string]interface{}{
			"supportsEvaluateForHovers": false, "supportsTerminateRequest": true,
		}, EvalCaps{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, f := newFakePair(t, Handlers{})
			defer stopFake(c, f)
			type out struct {
				caps Capabilities
				ec   EvalCaps
				err  error
			}
			done := make(chan out, 1)
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
				defer cancel()
				caps, ec, err := c.InitializeForEval(ctx, "fake")
				done <- out{caps, ec, err}
			}()
			req := f.readRequest()
			if req.Command != "initialize" {
				t.Fatalf("request = %q, want initialize", req.Command)
			}
			f.respond(req, tc.body)
			got := <-done
			if got.err != nil {
				t.Fatal(got.err)
			}
			if got.ec != tc.want {
				t.Errorf("EvalCaps = %+v, want %+v", got.ec, tc.want)
			}
			if !got.caps.SupportsTerminateRequest || !c.Caps().SupportsTerminateRequest {
				t.Error("Capabilities were not decoded and recorded alongside")
			}
		})
	}
}

// TestEvalCapsFromEventRevisesOnlyNamedKeys pins the capabilities event: a key
// it names changes, a key it omits is kept, and junk changes nothing.
func TestEvalCapsFromEventRevisesOnlyNamedKeys(t *testing.T) {
	prev := EvalCaps{SupportsCancelRequest: true}
	got := EvalCapsFromEvent(prev, json.RawMessage(`{"capabilities":{"supportsEvaluateForHovers":true}}`))
	if got != (EvalCaps{SupportsEvaluateForHovers: true, SupportsCancelRequest: true}) {
		t.Errorf("revision = %+v", got)
	}
	got = EvalCapsFromEvent(got, json.RawMessage(`{"capabilities":{"supportsCancelRequest":false}}`))
	if got != (EvalCaps{SupportsEvaluateForHovers: true}) {
		t.Errorf("withdrawal = %+v", got)
	}
	if EvalCapsFromEvent(prev, json.RawMessage(`not json`)) != prev {
		t.Error("junk changed the caps")
	}
}

// genHandlers counts the session's stops and resumes the way the editor does:
// from OnEvent, on the client's read goroutine.
func genHandlers(g *StopGen) Handlers {
	return Handlers{OnEvent: func(_ *Client, ev Event) { g.Observe(ev) }}
}

// pausedAtGen is a stop on thread 1 pinned to g's current generation.
func pausedAtGen(g *StopGen) PauseState {
	st := stoppedAt
	st.Gen, st.GenAt = g, g.Load()
	return st
}

// TestSafeEvaluateDiscardsAnAnswerThatCrossedAContinued pins the resume race:
// the adapter emits `continued` while the evaluate is in flight and THEN
// answers it. The answer may have been computed after the resume, so it is
// discarded and the caller is told the session resumed during evaluate.
func TestSafeEvaluateDiscardsAnAnswerThatCrossedAContinued(t *testing.T) {
	var g StopGen
	c, f := newFakePair(t, genHandlers(&g))
	defer stopFake(c, f)

	done := startSafeEvaluate(c, hoverCaps, pausedAtGen(&g), EvalRequest{Expression: "x", Trusted: true}, DefaultEvalLimits())
	answerStack(t, f, 1)
	ev := f.readRequest()
	f.event(EventContinued, map[string]int{"threadId": 1})
	f.respond(ev, EvaluateResult{Result: "from the next stop"})

	got := <-done
	if !errors.Is(got.err, ErrEvalResumed) || !strings.Contains(got.err.Error(), "resumed during evaluate") {
		t.Fatalf("err = %v, want ErrEvalResumed (resumed during evaluate)", got.err)
	}
	if got.res.Root != nil {
		t.Errorf("a discarded answer leaked out: %+v", got.res.Root)
	}
}

// TestSafeEvaluateDiscardsVariablesThatCrossedAStop is the same race inside the
// walk: a re-stop between a variables request and its answer fails the WHOLE
// evaluate rather than mixing two stops in one tree.
func TestSafeEvaluateDiscardsVariablesThatCrossedAStop(t *testing.T) {
	var g StopGen
	c, f := newFakePair(t, genHandlers(&g))
	defer stopFake(c, f)

	done := startSafeEvaluate(c, hoverCaps, pausedAtGen(&g), EvalRequest{Expression: "v", Trusted: true}, DefaultEvalLimits())
	answerStack(t, f, 1)
	f.respond(f.readRequest(), EvaluateResult{Result: "v", VariablesReference: 1})
	vr := f.readRequest()
	if vr.Command != "variables" {
		t.Fatalf("command = %q, want variables", vr.Command)
	}
	f.event(EventStopped, map[string]interface{}{"reason": "step", "threadId": 1})
	f.respond(vr, variablesBody{Variables: []Variable{{Name: "a", Value: "1"}}})

	if got := <-done; !errors.Is(got.err, ErrEvalResumed) {
		t.Fatalf("err = %v, want ErrEvalResumed", got.err)
	}
}

// TestPausedFramesDiscardsAStackThatCrossedAContinued pins the race for the
// frames command.
func TestPausedFramesDiscardsAStackThatCrossedAContinued(t *testing.T) {
	var g StopGen
	c, f := newFakePair(t, genHandlers(&g))
	defer stopFake(c, f)

	errc := make(chan error, 1)
	go func() {
		_, err := PausedFrames(context.Background(), c, hoverCaps, pausedAtGen(&g), time.Second)
		errc <- err
	}()
	req := f.readRequest()
	f.event(EventContinued, map[string]int{"threadId": 1})
	f.respond(req, stackTraceBody{StackFrames: []StackFrame{{ID: 1}}})
	if err := <-errc; !errors.Is(err, ErrEvalResumed) {
		t.Fatalf("err = %v, want ErrEvalResumed", err)
	}
}

// TestSafeEvaluateRefusesWhenResumedBeforeTheFirstRequest pins the check at
// the very start: a session that moved between snapshot and request sends
// nothing at all.
func TestSafeEvaluateRefusesWhenResumedBeforeTheFirstRequest(t *testing.T) {
	c, f := newFakePair(t, Handlers{})
	defer stopFake(c, f)
	var g StopGen
	st := pausedAtGen(&g)
	g.Bump()
	got := <-startSafeEvaluate(c, hoverCaps, st, EvalRequest{Expression: "v", Trusted: true}, DefaultEvalLimits())
	if !errors.Is(got.err, ErrEvalResumed) {
		t.Fatalf("err = %v, want ErrEvalResumed", got.err)
	}
	if _, err := PausedFrames(context.Background(), c, hoverCaps, st, time.Second); !errors.Is(err, ErrEvalResumed) {
		t.Fatalf("PausedFrames err = %v, want ErrEvalResumed", err)
	}
	assertNothingWasSent(t, c, f)
}

// TestSafeEvaluateBoundHoldsAgainstAnAdapterThatStopsReading pins the time
// bound: the adapter reads the evaluate and then never reads again, so even
// the cancel cannot be written. The evaluate still returns at its timeout,
// because the cancel is fire-and-forget.
func TestSafeEvaluateBoundHoldsAgainstAnAdapterThatStopsReading(t *testing.T) {
	c, f := newFakePair(t, Handlers{})
	defer stopFake(c, f)

	lim := DefaultEvalLimits()
	lim.Timeout = 200 * time.Millisecond
	start := time.Now()
	done := startSafeEvaluate(c, hoverCaps, stoppedAt, EvalRequest{Expression: "for{}", Trusted: true}, lim)
	answerStack(t, f, 1)
	f.readRequest() // the evaluate; nothing is read after it

	select {
	case got := <-done:
		if !errors.Is(got.err, ErrEvalTimeout) {
			t.Fatalf("err = %v, want ErrEvalTimeout", got.err)
		}
		if el := time.Since(start); el > lim.Timeout+500*time.Millisecond {
			t.Errorf("evaluate took %v, want about %v", el, lim.Timeout)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the evaluate outlived its bound: the cancel write blocked it")
	}
}

// TestPausedFramesBoundHoldsAgainstAHangingAdapter pins the frames bound: a
// stackTrace the adapter never answers fails at the timeout.
func TestPausedFramesBoundHoldsAgainstAHangingAdapter(t *testing.T) {
	c, f := newFakePair(t, Handlers{})
	defer stopFake(c, f)

	start := time.Now()
	errc := make(chan error, 1)
	go func() {
		_, err := PausedFrames(context.Background(), c, hoverCaps, stoppedAt, 200*time.Millisecond)
		errc <- err
	}()
	f.readRequest() // never answered, and nothing after it is read
	select {
	case err := <-errc:
		if !errors.Is(err, ErrEvalTimeout) {
			t.Fatalf("err = %v, want ErrEvalTimeout", err)
		}
		if el := time.Since(start); el > 700*time.Millisecond {
			t.Errorf("frames took %v, want about 200ms", el)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("frames outlived its bound")
	}
}

// The cancellable path used by visualization requests is bounded by its context too: against an
// adapter that never reads, it gives up at the deadline and the request is never delivered.
func TestCallCancellableNeverDeliversAfterItsDeadline(t *testing.T) {
	c, f := newFakePair(t, Handlers{})
	defer stopFake(c, f)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.callCancellable(ctx, true, "evaluate", map[string]interface{}{"expression": "x = 1"}); err == nil {
		t.Fatal("callCancellable succeeded against an adapter that never reads")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("callCancellable outlived its 150 ms context: %v", elapsed)
	}
	time.Sleep(200 * time.Millisecond)
	_ = f.conn.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if _, err := readMessage(f.r); err == nil {
		t.Fatal("the request reached the adapter after its deadline")
	}
}
