// =============================================================================
// File: internal/dap/evaluate.go
// Author: Vonzelle Brown
// Created: 2026-10-14
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// The SAFE evaluate path behind the debug-visualization socket
// command.
//
// Client.Evaluate is the raw request: it evaluates whatever it is handed, in
// whatever context, for as long as the adapter takes. That is right for the
// editor's own watch row, where a human typed the expression and is looking at
// the answer. It is wrong for a socket command that another process can call,
// because three things an evaluate can do are things the caller never asked
// for: run code with side effects (`watch` and `repl` both call functions),
// hang (an expression can loop forever), and return a value graph the size of
// the heap. SafeEvaluate is the bounded version and the only one the socket
// command may use.
//
// Clean-room: written against the DAP specification only. No Debug Visualizer
// source was read; see the debugviz-herdr-p8 spec.

package dap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// EvalContextHover is the protocol's side-effect-free evaluate context. An
// adapter that advertises supportsEvaluateForHovers promises to evaluate it
// without running user code (no function calls, no getters), which is why it
// is the default for a request nobody on screen typed.
const EvalContextHover = "hover"

// The bounds from the debugviz contract. They are defaults, not constants
// baked into the walk, so tests can exercise each cap with a small tree.
const (
	DefaultEvalTimeout     = 3 * time.Second
	DefaultEvalMaxDepth    = 6
	DefaultEvalMaxNodes    = 2000
	DefaultEvalMaxOutBytes = 256 << 10
)

// Refusals. Each is a sentinel so the socket layer can tell "you asked for
// something unsafe" from "the adapter could not answer".
var (
	ErrEvalUntrusted    = errors.New("evaluate refused: this workspace is not trusted")
	ErrEvalNotStopped   = errors.New("evaluate refused: the debuggee is not stopped")
	ErrEvalSideEffects  = errors.New("evaluate refused: watch/repl can run code with side effects; pass --allow-side-effects")
	ErrEvalNoHover      = errors.New("evaluate refused: the adapter does not support side-effect-free (hover) evaluation; use --allow-side-effects with watch")
	ErrEvalBadContext   = errors.New("evaluate refused: context must be hover, watch or repl")
	ErrEvalEmpty        = errors.New("evaluate refused: empty expression")
	ErrEvalUnknownFrame = errors.New("evaluate refused: frame is not one of the paused frames")
	ErrEvalTimeout      = errors.New("evaluate timed out")
	ErrEvalResumed      = errors.New("resumed during evaluate: the debuggee resumed or stopped again, so the answer was discarded")
)

// EvalCaps is the part of the adapter's initialize answer the safe path needs.
//
// 🔴 These two are NOT in Capabilities, and Client.Initialize decodes into
// Capabilities and discards the rest of the body. InitializeForEval is the
// handshake that keeps them, and EvalCapsFromEvent folds a mid-session
// `capabilities` revision in. The zero value is the safe one: no hover support
// means the default is REFUSED rather than silently promoted to watch, and no
// cancel support means a timed-out result is dropped rather than cancelled.
type EvalCaps struct {
	SupportsEvaluateForHovers bool `json:"supportsEvaluateForHovers"`
	SupportsCancelRequest     bool `json:"supportsCancelRequest"`
}

// StopGen is a session's stop generation: it moves on every stopped and
// continued event, and on every resume the caller starts itself.
//
// 🔴 Observe belongs in Handlers.OnEvent. readLoop runs that handler BEFORE it
// reads the next message, so a response that follows a continued event on the
// wire always finds the generation already moved. Counting the event later, on
// another goroutine, would leave a window in which an answer from the new stop
// passes for one from the old.
type StopGen struct{ n atomic.Uint64 }

// Observe advances the generation on stopped and continued events.
func (g *StopGen) Observe(ev Event) uint64 {
	if ev.Event == EventStopped || ev.Event == EventContinued {
		return g.n.Add(1)
	}
	return g.n.Load()
}

// Bump advances the generation for a resume the caller started itself; many
// adapters send no continued event in reply to a continue request.
func (g *StopGen) Bump() { g.n.Add(1) }

// Load returns the current generation.
func (g *StopGen) Load() uint64 { return g.n.Load() }

// PauseState is what the caller knows about the session, because the client
// does not track run state — the app does, from stopped/continued events.
type PauseState struct {
	Stopped  bool
	ThreadID int
	// SelectedFrameID is the frame the user is looking at, if any. nil means
	// the innermost paused frame.
	SelectedFrameID *int
	// Gen and GenAt pin the whole request to ONE stop: GenAt is Gen's value
	// when the snapshot above was taken, and it is compared before every
	// adapter request and again when each answer arrives. Nil Gen skips the
	// check.
	Gen   *StopGen
	GenAt uint64
}

// checkPaused refuses once the session has resumed or stopped again since the
// snapshot.
func (st PauseState) checkPaused() error {
	if st.Gen != nil && st.Gen.Load() != st.GenAt {
		return ErrEvalResumed
	}
	return nil
}

// pausedCall is one adapter request under the stopped-only rule. The answer is
// discarded when the generation moved while it was in flight: it may have been
// computed in the new stop, against a frame or reference from the old one.
func pausedCall(ctx context.Context, c *Client, st PauseState, canCancel bool, command string, args interface{}) (json.RawMessage, error) {
	if err := st.checkPaused(); err != nil {
		return nil, err
	}
	raw, err := c.callCancellable(ctx, canCancel, command, args)
	if perr := st.checkPaused(); perr != nil {
		return nil, perr
	}
	return raw, err
}

// InitializeForEval is Client.Initialize that also keeps the two capabilities
// the safe evaluate path needs. Same request, same Caps() bookkeeping — the
// body is simply decoded twice instead of having half of it thrown away.
func (c *Client) InitializeForEval(ctx context.Context, adapterID string) (Capabilities, EvalCaps, error) {
	raw, err := c.call(ctx, "initialize", initializeArgsForClient(adapterID))
	if err != nil {
		return Capabilities{}, EvalCaps{}, err
	}
	var caps Capabilities
	var ec EvalCaps
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &caps); err != nil {
			return Capabilities{}, EvalCaps{}, fmt.Errorf("%s: initialize body: %w", c.name, err)
		}
		if err := json.Unmarshal(raw, &ec); err != nil {
			return Capabilities{}, EvalCaps{}, fmt.Errorf("%s: initialize body: %w", c.name, err)
		}
	}
	c.mu.Lock()
	c.caps = caps
	c.mu.Unlock()
	return caps, ec, nil
}

// EvalCapsFromEvent applies a `capabilities` event body to prev. Only the
// fields the event actually names change: the event carries what was REVISED,
// and an absent key is not a withdrawal.
func EvalCapsFromEvent(prev EvalCaps, body json.RawMessage) EvalCaps {
	var ev struct {
		Capabilities struct {
			Hover  *bool `json:"supportsEvaluateForHovers"`
			Cancel *bool `json:"supportsCancelRequest"`
		} `json:"capabilities"`
	}
	if len(body) == 0 || json.Unmarshal(body, &ev) != nil {
		return prev
	}
	if ev.Capabilities.Hover != nil {
		prev.SupportsEvaluateForHovers = *ev.Capabilities.Hover
	}
	if ev.Capabilities.Cancel != nil {
		prev.SupportsCancelRequest = *ev.Capabilities.Cancel
	}
	return prev
}

// EvalRequest is one socket-command evaluate.
type EvalRequest struct {
	Expression string
	// FrameID overrides the selected frame. It must be one of the paused
	// frames; a stale id would be answered from another scope with no error.
	FrameID *int
	// Context is hover, watch or repl; "" means hover.
	Context string
	// AllowSideEffects is the --allow-side-effects flag. watch and repl are
	// refused without it.
	AllowSideEffects bool
	// Trusted is herdr-edit's own trust decision for the workspace. The
	// package cannot read it (internal/app owns trust and imports this one),
	// so the caller passes it in and SafeEvaluate refuses when it is false.
	Trusted bool
}

// EvalLimits bounds one evaluate end to end.
type EvalLimits struct {
	Timeout     time.Duration
	MaxDepth    int
	MaxNodes    int
	MaxOutBytes int
}

// DefaultEvalLimits returns the contract's bounds: 3 s, depth 6, 2000 nodes,
// 256 KB of output.
func DefaultEvalLimits() EvalLimits {
	return EvalLimits{
		Timeout:     DefaultEvalTimeout,
		MaxDepth:    DefaultEvalMaxDepth,
		MaxNodes:    DefaultEvalMaxNodes,
		MaxOutBytes: DefaultEvalMaxOutBytes,
	}
}

// EvalNode is one value in the expanded result: the DAP variable itself plus
// the children read through its variablesReference.
//
// VariablesReference is carried for the visualiser but is PERISHABLE — it dies
// at the next continue or step, like every reference (see Client.Variables).
type EvalNode struct {
	Variable
	Children []*EvalNode `json:"children,omitempty"`
	// ChildrenTruncated means this node has children that were not read, or
	// were read only in part, because a cap was hit.
	ChildrenTruncated bool `json:"childrenTruncated,omitempty"`
	// ExpandError is why reading this node's children failed, if it did.
	ExpandError string `json:"expandError,omitempty"`
}

// EvalResult is a bounded evaluate answer.
type EvalResult struct {
	Expression        string    `json:"expression"`
	FrameID           int       `json:"frameId"`
	Context           string    `json:"context"`
	Adapter           string    `json:"adapter"`
	SideEffectWarning bool      `json:"sideEffectWarning,omitempty"`
	Root              *EvalNode `json:"root"`
	Nodes             int       `json:"nodes"`
	OutBytes          int       `json:"outBytes"`
	Truncated         bool      `json:"truncated,omitempty"`
	// TruncatedBy names every cap that was hit: depth, nodes, output, timeout.
	TruncatedBy []string `json:"truncatedBy,omitempty"`
}

// cancelArgs is the protocol's cancel request body.
type cancelArgs struct {
	RequestID int `json:"requestId"`
}

// PausedFrames lists the frames of the stopped thread, for frame selection,
// within timeout. It is refused while running: frame ids only mean something
// during a stop.
func PausedFrames(ctx context.Context, c *Client, caps EvalCaps, st PauseState, timeout time.Duration) ([]StackFrame, error) {
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	frames, err := pausedFrames(tctx, c, caps, st)
	if err != nil {
		return nil, timeoutErr(tctx, err)
	}
	return frames, nil
}

// pausedFrames is PausedFrames under the caller's deadline.
func pausedFrames(ctx context.Context, c *Client, caps EvalCaps, st PauseState) ([]StackFrame, error) {
	if !st.Stopped {
		return nil, ErrEvalNotStopped
	}
	raw, err := pausedCall(ctx, c, st, caps.SupportsCancelRequest, "stackTrace", stackTraceArgs{ThreadID: st.ThreadID})
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

// SafeEvaluate evaluates an expression under the debugviz contract: trusted
// workspace only, stopped only, hover by default, watch/repl only with
// AllowSideEffects (and then flagged), bounded in time, and expanded
// breadth-first under depth, node and output caps.
//
// Every refusal is decided BEFORE anything is written to the adapter.
func SafeEvaluate(ctx context.Context, c *Client, caps EvalCaps, st PauseState, req EvalRequest, lim EvalLimits) (EvalResult, error) {
	if !req.Trusted {
		return EvalResult{}, ErrEvalUntrusted
	}
	if !st.Stopped {
		return EvalResult{}, ErrEvalNotStopped
	}
	if req.Expression == "" {
		return EvalResult{}, ErrEvalEmpty
	}
	evalCtx, warn, err := evalPolicy(caps, req)
	if err != nil {
		return EvalResult{}, err
	}

	tctx, cancel := context.WithTimeout(ctx, lim.Timeout)
	defer cancel()

	frameID, err := resolveEvalFrame(tctx, c, caps, st, req.FrameID)
	if err != nil {
		return EvalResult{}, err
	}

	// 🔴 Built by hand rather than through Client.Evaluate: that one omits the
	// frame for hover (it only forces it for watch), and js-debug's innermost
	// frame is id 0 — so a hover there would be answered globally. During a
	// stop there is ALWAYS a frame, so it is always sent.
	id := frameID
	raw, err := pausedCall(tctx, c, st, caps.SupportsCancelRequest, "evaluate",
		evaluateArgs{Expression: req.Expression, FrameID: &id, Context: evalCtx})
	if err != nil {
		return EvalResult{}, err
	}
	var body EvaluateResult
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			return EvalResult{}, err
		}
	}

	res := EvalResult{
		Expression:        req.Expression,
		FrameID:           frameID,
		Context:           evalCtx,
		Adapter:           c.Name(),
		SideEffectWarning: warn,
	}
	w := evalWalk{ctx: tctx, c: c, caps: caps, st: st, lim: lim, res: &res}
	res.Root = w.expand(Variable{
		Name: req.Expression, Value: body.Result, Type: body.Type,
		VariablesReference: body.VariablesReference,
	})
	// A resume mid-walk invalidates the WHOLE answer, not just the unread
	// part: the references already read were answered from the old stop, and a
	// tree mixing two stops is worse than no tree.
	if w.err != nil {
		return EvalResult{}, w.err
	}
	return res, nil
}

// evalPolicy decides the context to send and whether the answer carries a
// side-effect warning. hover is refused rather than upgraded when the adapter
// cannot do it: quietly sending watch instead would run the very code the
// default exists to avoid.
func evalPolicy(caps EvalCaps, req EvalRequest) (string, bool, error) {
	switch req.Context {
	case "", EvalContextHover:
		if !caps.SupportsEvaluateForHovers {
			return "", false, ErrEvalNoHover
		}
		return EvalContextHover, false, nil
	case EvalContextWatch, EvalContextRepl:
		if !req.AllowSideEffects {
			return "", false, ErrEvalSideEffects
		}
		return req.Context, true, nil
	default:
		return "", false, ErrEvalBadContext
	}
}

// resolveEvalFrame picks the frame to evaluate in — the requested one, else
// the selected one, else the innermost — and checks it is actually paused.
// The check is what turns a stale id (left over from before a step) into a
// refusal instead of an answer from some other scope.
func resolveEvalFrame(ctx context.Context, c *Client, caps EvalCaps, st PauseState, want *int) (int, error) {
	frames, err := pausedFrames(ctx, c, caps, st)
	if err != nil {
		return 0, timeoutErr(ctx, err)
	}
	if len(frames) == 0 {
		return 0, ErrEvalUnknownFrame
	}
	if want == nil {
		want = st.SelectedFrameID
	}
	if want == nil {
		return frames[0].ID, nil
	}
	for _, f := range frames {
		if f.ID == *want {
			return f.ID, nil
		}
	}
	return 0, fmt.Errorf("%w: %d", ErrEvalUnknownFrame, *want)
}

// evalWalk is one breadth-first expansion under the caps.
type evalWalk struct {
	ctx  context.Context
	c    *Client
	caps EvalCaps
	st   PauseState
	lim  EvalLimits
	res  *EvalResult
	// err is a resume seen mid-walk; it stops the walk and fails the evaluate.
	err error
}

// evalQueued is a node awaiting expansion and its depth (root = 0).
type evalQueued struct {
	n     *EvalNode
	depth int
}

// expand builds the tree from the root value, breadth-first so that when a cap
// cuts the walk short it is the DEEP detail that is lost, never a sibling the
// user can see at the top level.
func (w *evalWalk) expand(root Variable) *EvalNode {
	rn, ok := w.add(root)
	if !ok {
		return rn
	}
	queue := []evalQueued{{rn, 0}}
	for len(queue) > 0 {
		q := queue[0]
		queue = queue[1:]
		if q.n.VariablesReference <= 0 {
			continue
		}
		if q.depth >= w.lim.MaxDepth {
			q.n.ChildrenTruncated = true
			w.truncate("depth")
			continue
		}
		if w.res.Nodes >= w.lim.MaxNodes {
			q.n.ChildrenTruncated = true
			w.truncate("nodes")
			continue
		}
		if w.ctx.Err() != nil {
			q.n.ChildrenTruncated = true
			w.truncate("timeout")
			continue
		}
		kids, err := w.variables(q.n.VariablesReference, w.lim.MaxNodes-w.res.Nodes)
		if errors.Is(err, ErrEvalResumed) {
			w.err = err
			return rn
		}
		if err != nil {
			q.n.ChildrenTruncated = true
			if w.ctx.Err() != nil {
				w.truncate("timeout")
			} else {
				q.n.ExpandError = err.Error()
			}
			continue
		}
		for i, v := range kids {
			if w.res.Nodes >= w.lim.MaxNodes {
				q.n.ChildrenTruncated = true
				w.truncate("nodes")
				break
			}
			cn, ok := w.add(v)
			q.n.Children = append(q.n.Children, cn)
			if !ok {
				if i < len(kids)-1 {
					q.n.ChildrenTruncated = true
				}
				return rn
			}
			queue = append(queue, evalQueued{cn, q.depth + 1})
		}
	}
	return rn
}

// add accounts one node against the node and output caps. It reports false
// when the output cap was reached, in which case the node's value has been
// clipped to fit and the walk must stop.
func (w *evalWalk) add(v Variable) (*EvalNode, bool) {
	n := &EvalNode{Variable: v}
	w.res.Nodes++
	left := w.lim.MaxOutBytes - w.res.OutBytes
	size := len(v.Name) + len(v.Value) + len(v.Type)
	if size <= left {
		w.res.OutBytes += size
		return n, true
	}
	keep := left - len(v.Name) - len(v.Type)
	n.Value = clipUTF8(v.Value, keep)
	n.VariablesReference = 0
	w.res.OutBytes += len(n.Name) + len(n.Value) + len(n.Type)
	w.truncate("output")
	return n, false
}

// variables reads one reference's children through the cancellable path,
// asking for at most limit when the adapter can page and clipping when it
// cannot (it then sends everything — see Client.Variables).
func (w *evalWalk) variables(ref, limit int) ([]Variable, error) {
	args := variablesArgs{VariablesReference: ref}
	if w.c.Caps().SupportsVariablePaging {
		args.Count = limit
	}
	raw, err := pausedCall(w.ctx, w.c, w.st, w.caps.SupportsCancelRequest, "variables", args)
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

// truncate records that a cap was hit, once per cap.
func (w *evalWalk) truncate(reason string) {
	w.res.Truncated = true
	for _, r := range w.res.TruncatedBy {
		if r == reason {
			return
		}
	}
	w.res.TruncatedBy = append(w.res.TruncatedBy, reason)
}

// clipUTF8 cuts s to at most n bytes without splitting a rune.
func clipUTF8(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// timeoutErr turns a deadline into ErrEvalTimeout so the caller can say
// "timed out" rather than relaying a transport-shaped context error.
func timeoutErr(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: %v", ErrEvalTimeout, err)
	}
	return err
}

// callCancellable is call() plus a cancel on deadline.
//
// It exists because call() does not expose the request's seq, and DAP's
// cancel names its target by that seq. On deadline the request is cancelled
// when the adapter advertised supportsCancelRequest; otherwise its pending
// entry is simply removed, so the late answer arrives to nobody and is
// dropped by readLoop — the result is never delivered after the caller gave up.
func (c *Client) callCancellable(ctx context.Context, canCancel bool, command string, args interface{}) (json.RawMessage, error) {
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

	// Removed off this goroutine: c.write holds c.mu for as long as a stuck
	// write lasts, and waiting for the lock here would carry that past the
	// deadline. ch is buffered, so a late answer never blocks readLoop.
	defer func() {
		go func() {
			c.mu.Lock()
			delete(c.pending, seq)
			c.mu.Unlock()
		}()
	}()

	// writeCtx sends nothing once ctx is done and aborts a blocked write at ctx's deadline or on an
	// early cancel, so a request that is not sent is never delivered later.
	if err := c.writeCtx(ctx, Request{Seq: seq, Type: TypeRequest, Command: command, Arguments: args}); err != nil {
		return nil, timeoutErr(ctx, fmt.Errorf("%s: %w", command, err))
	}

	select {
	case resp := <-ch:
		if !resp.Success {
			return nil, c.responseError(command, resp)
		}
		return resp.Body, nil
	case <-ctx.Done():
		how := "result dropped"
		if canCancel {
			go c.sendCancel(seq)
			how = "cancel sent"
		}
		err := fmt.Errorf("%s: %s: %s: %w", c.name, command, how, ctx.Err())
		return nil, timeoutErr(ctx, err)
	}
}

// sendCancel asks the adapter to abandon request seq. It is fire-and-forget:
// callers run it on its own goroutine so a blocked write never extends their
// deadline, no pending entry is registered, and a write failure is only logged.
func (c *Client) sendCancel(seq int) {
	c.mu.Lock()
	c.nextSeq++
	cseq := c.nextSeq
	c.mu.Unlock()
	if err := c.write(Request{Seq: cseq, Type: TypeRequest, Command: "cancel", Arguments: cancelArgs{RequestID: seq}}); err != nil {
		c.log("cancel request " + fmt.Sprint(seq) + ": " + err.Error())
	}
}
