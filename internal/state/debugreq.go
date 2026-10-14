// =============================================================================
// File: internal/state/debugreq.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// debugreq.go is the REVERSE of debugsession.go, and the exact shape openreq.go
// already established: debug-session.json flows editor -> panels ("here is what
// the debugger is doing"), debug-request.json flows panels -> editor ("do the
// next thing").
//
// That direction is what makes the Debug panel a debugger rather than a
// read-only mirror of one. The DAP client stays in the editor — the panel never
// speaks the protocol — but pressing `n` in the panel steps the program, because
// the request lands here and the editor honours it from its polled call site.
//
// The mechanism is deliberately identical to openreq.go: one small JSON file,
// temp-file plus rename so a reader never sees half a payload, a monotonic
// sequence so a request is honoured exactly once, and a malformed payload
// treated as ABSENT rather than as an error — a panel writing junk must not be
// able to break the editor.
//
// 🔴 There is ONE deliberate difference from openreq.go and it is the whole
// reason this file exists separately: consumeOpenRequest starts its high-water
// mark at 0, so a request left on disk by a previous session is honoured once at
// startup. For "open a file" that is harmless. For "start a debug session" it
// means LAUNCHING A PROCESS the user did not ask for, minutes or days later, on
// whatever project the editor happens to be opened on next. The editor floors
// the sequence at its own start time instead — see App.consumeDebugRequest.

package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// The actions a panel may ask for. They are the DAP verbs the editor already
// implements, spelled the way the protocol spells them so there is no
// translation table to get wrong — except toggle-breakpoint, which is not a DAP
// request at all but an edit to the editor's own marks.
const (
	DebugActionStart            = "start"
	DebugActionContinue         = "continue"
	DebugActionNext             = "next"
	DebugActionStepIn           = "stepIn"
	DebugActionStepOut          = "stepOut"
	DebugActionPause            = "pause"
	DebugActionStop             = "stop"
	DebugActionTest             = "test"
	DebugActionToggleBreakpoint = "toggle-breakpoint"

	// The two QUERIES. Every other action is fire-and-forget through
	// debug-request.json; these travel addressed to one editor and answer as
	// JSON — see WriteDebugQuery.
	DebugActionEvaluate = "evaluate"
	DebugActionFrames   = "frames"
)

// debugActions is the single list every check reads: the CLI validates against
// it, ReadDebugRequest rejects against it, and the help text prints it. A second
// hand-written copy would drift the first time an action was added, and the
// symptom would be a key that works from the panel and not from the CLI.
var debugActions = []string{
	DebugActionStart, DebugActionContinue, DebugActionNext, DebugActionStepIn,
	DebugActionStepOut, DebugActionPause, DebugActionStop, DebugActionTest, DebugActionToggleBreakpoint,
	DebugActionEvaluate, DebugActionFrames,
}

// IsDebugQuery reports whether action is a query rather than a fire-and-forget
// request.
func IsDebugQuery(action string) bool {
	return action == DebugActionEvaluate || action == DebugActionFrames
}

// DebugActions returns the accepted actions, in the order the help lists them.
func DebugActions() []string {
	out := make([]string, len(debugActions))
	copy(out, debugActions)
	return out
}

// ValidDebugAction reports whether action is one the editor will honour.
func ValidDebugAction(action string) bool {
	for _, a := range debugActions {
		if a == action {
			return true
		}
	}
	return false
}

// DebugRequestFile is the path panels write and the editor polls.
func DebugRequestFile() string {
	return filepath.Join(Dir(), "debug-request.json")
}

// DebugRequest is one instruction to the editor's debugger.
//
// File and Line carry toggle-breakpoint's location, while File and Node identify
// one pytest test. Line is 1-based on the wire — the same convention as
// OpenRequest and active.json, converted by the editor on the way in.
//
// Expression, FrameID, Context and AllowSideEffects belong to evaluate and mean
// exactly what they mean on dap.EvalRequest. FrameID is a pointer because 0 is
// a real frame id (js-debug's innermost) and nil means "the selected frame".
// ID and Instance belong to the queries: ID names the request and its reply,
// Instance the one editor session it is addressed to.
type DebugRequest struct {
	Action string `json:"action"`
	File   string `json:"file"`
	Line   int    `json:"line"`
	Node   string `json:"node,omitempty"`
	Seq    int64  `json:"seq"`

	ID       string `json:"id,omitempty"`
	Instance string `json:"instance,omitempty"`

	Expression       string `json:"expression,omitempty"`
	FrameID          *int   `json:"frameId,omitempty"`
	Context          string `json:"context,omitempty"`
	AllowSideEffects bool   `json:"allowSideEffects,omitempty"`
}

// WriteDebugRequest asks the editor to perform action. Used by the CLI that the
// Debug panel shells out to.
//
// It refuses an unknown action HERE rather than letting the editor discover it,
// so `herdr-edit --debug contnue` is a typo the user is told about instead of a
// key that silently does nothing.
func WriteDebugRequest(action, file string, line int, node ...string) error {
	if !ValidDebugAction(action) {
		return errors.New("unknown debug action: " + action)
	}
	if IsDebugQuery(action) {
		return errors.New(action + " is a query; use WriteDebugQuery")
	}
	if action == DebugActionToggleBreakpoint && file == "" {
		return errors.New("toggle-breakpoint needs a file, optionally as file:line")
	}
	var testNode string
	if len(node) > 0 {
		testNode = node[0]
	}
	if action == DebugActionTest && (file == "" || testNode == "") {
		return errors.New("test needs a file and pytest node")
	}
	req := DebugRequest{Action: action, File: file, Line: line, Node: testNode, Seq: time.Now().UnixNano()}
	return writeStateJSON(req, "debug-request-*.tmp", DebugRequestFile())
}

// writeStateJSON writes v to path, temp-file plus rename so a reader never
// sees half a payload. The temp file sits beside path, so the rename never
// crosses a filesystem. The ONE writer for every debug request and reply.
func writeStateJSON(v any, pattern, path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

// ParseDebugQuery parses the arguments after `--debug evaluate` or
// `--debug frames` into a request:
//
//	evaluate --expr E [--frame N] [--context hover|watch|repl] [--allow-side-effects] [--instance ID]
//	frames [--instance ID]
//
// It lives here rather than in main.go so the CLI's reading of its flags is
// tested on the same path the request then travels. The context is checked
// for spelling only; whether it is ALLOWED is the editor's decision
// (dap.SafeEvaluate), made where the adapter's capabilities are known. A
// missing --expr is refused by WriteDebugQuery, before anything is written:
// parsing accepts the bare verb so `--debug evaluate` resolves like every
// other action and is then told what it lacks.
func ParseDebugQuery(action string, args []string) (DebugRequest, error) {
	req := DebugRequest{Action: action}
	if !IsDebugQuery(action) {
		return DebugRequest{}, fmt.Errorf("%q is not a debug query", action)
	}
	for i := 0; i < len(args); i++ {
		flag := args[i]
		value := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", flag)
			}
			i++
			return args[i], nil
		}
		if flag == "--instance" {
			v, err := value()
			if err != nil {
				return DebugRequest{}, err
			}
			req.Instance = v
			continue
		}
		if action == DebugActionFrames {
			return DebugRequest{}, fmt.Errorf("frames takes only --instance, got %q", flag)
		}
		switch flag {
		case "--expr":
			v, err := value()
			if err != nil {
				return DebugRequest{}, err
			}
			req.Expression = v
		case "--frame":
			v, err := value()
			if err != nil {
				return DebugRequest{}, err
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return DebugRequest{}, fmt.Errorf("--frame needs a frame id (an integer >= 0), got %q", v)
			}
			req.FrameID = &n
		case "--context":
			v, err := value()
			if err != nil {
				return DebugRequest{}, err
			}
			if v != "hover" && v != "watch" && v != "repl" {
				return DebugRequest{}, fmt.Errorf("--context must be hover, watch or repl, got %q", v)
			}
			req.Context = v
		case "--allow-side-effects":
			req.AllowSideEffects = true
		default:
			return DebugRequest{}, fmt.Errorf("unknown evaluate flag %q", flag)
		}
	}
	return req, nil
}

// ReadDebugRequest returns the pending request, or ok=false when there is none
// or it cannot be trusted.
//
// Missing, unparseable, sequence-less and unknown-action payloads all read as
// ABSENT rather than as an error. The editor polls this several times a second;
// a panel that wrote junk — or a half-finished file from some other tool — must
// cost nothing more than a poll that found nothing.
func ReadDebugRequest() (DebugRequest, bool) {
	raw, err := os.ReadFile(DebugRequestFile())
	if err != nil {
		return DebugRequest{}, false
	}
	var req DebugRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return DebugRequest{}, false
	}
	// A query here is addressed to nobody: queries travel through
	// WriteDebugQuery, to exactly one editor.
	if req.Seq == 0 || !ValidDebugAction(req.Action) || IsDebugQuery(req.Action) {
		return DebugRequest{}, false
	}
	if req.Action == DebugActionToggleBreakpoint && req.File == "" {
		return DebugRequest{}, false
	}
	if req.Action == DebugActionTest && (req.File == "" || req.Node == "") {
		return DebugRequest{}, false
	}
	return req, true
}

// DebugReplyTimeout is how long the CLI waits for a query's answer. The editor
// reads requests from its event loop, which an idle editor only turns every
// 10s (the tree-refresh tick), and an evaluate may then take its own 3s — so
// anything shorter would report "no answer" from an editor that was about to
// give one.
const DebugReplyTimeout = 25 * time.Second

// MaxDebugQueryAge is how old a query may be when the editor takes it. Beyond it the CLI could stop
// waiting (DebugReplyTimeout) before the editor's bounded evaluate (3 s plus margin) answers, so the
// query is dropped UNEXECUTED: a side effect must never run for a caller that already gave up.
const MaxDebugQueryAge = DebugReplyTimeout - 5*time.Second

// DebugInstance is one editor session that can answer queries. ID is what
// --instance takes; PID lets a reader drop the entry of an editor that died
// without unregistering.
type DebugInstance struct {
	ID   string `json:"id"`
	PID  int    `json:"pid"`
	Root string `json:"root"`
}

// 🔴 Queries are ADDRESSED, unlike every other debug request. debug-request.json
// is one global file every editor reads, which is fine for "continue" and fatal
// for `--allow-side-effects` evaluate: an expression that writes must run in
// exactly one session. So each editor session registers under
// debug-instances/, a query is written to that one session's directory under
// debug-queries/, and its answer comes back in its own file under
// debug-replies/ — one file per request id, so concurrent queries never
// overwrite each other.
func debugInstanceDir() string { return filepath.Join(Dir(), "debug-instances") }
func debugQueryDir(instance string) string {
	return filepath.Join(Dir(), "debug-queries", instance)
}
func debugReplyFile(id string) string {
	return filepath.Join(Dir(), "debug-replies", id+".json")
}

// validDebugName reports whether s is safe as a file name: an instance or
// request id read from a command line must not be able to name a path.
func validDebugName(s string) bool {
	return s != "" && strings.Trim(s, "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ-") == ""
}

// RegisterDebugInstance records inst as able to answer queries.
func RegisterDebugInstance(inst DebugInstance) error {
	if !validDebugName(inst.ID) {
		return fmt.Errorf("invalid debug instance id %q", inst.ID)
	}
	return writeStateJSON(inst, "instance-*.tmp", filepath.Join(debugInstanceDir(), inst.ID+".json"))
}

// UnregisterDebugInstance removes id's registration and any queries still
// waiting for it.
func UnregisterDebugInstance(id string) {
	if !validDebugName(id) {
		return
	}
	os.Remove(filepath.Join(debugInstanceDir(), id+".json"))
	os.RemoveAll(debugQueryDir(id))
}

// DebugInstanceRegistered reports whether id is registered.
func DebugInstanceRegistered(id string) bool {
	if !validDebugName(id) {
		return false
	}
	_, err := os.Stat(filepath.Join(debugInstanceDir(), id+".json"))
	return err == nil
}

// processAlive reports whether pid is a running process. Only "no such
// process" counts as dead; anything else (a process owned by another user, a
// platform that cannot probe) is kept, which at worst makes the CLI ask for
// --instance.
var processAlive = func(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || !(errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH))
}

// LiveDebugInstances lists the registered sessions whose editor is still
// running, ordered by id. A dead editor's registration is removed on the way.
func LiveDebugInstances() []DebugInstance {
	entries, err := os.ReadDir(debugInstanceDir())
	if err != nil {
		return nil
	}
	var out []DebugInstance
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(debugInstanceDir(), e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var inst DebugInstance
		if json.Unmarshal(raw, &inst) != nil || !validDebugName(inst.ID) {
			continue
		}
		if !processAlive(inst.PID) {
			UnregisterDebugInstance(inst.ID)
			continue
		}
		out = append(out, inst)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ChooseDebugInstance is the CLI's addressing rule: the instance named by
// want, else the only live one. With several live and none named it refuses
// and lists them — guessing would run the expression in a session the caller
// did not pick.
func ChooseDebugInstance(want string) (DebugInstance, error) {
	live := LiveDebugInstances()
	if want != "" {
		for _, inst := range live {
			if inst.ID == want {
				return inst, nil
			}
		}
		return DebugInstance{}, fmt.Errorf("no running debug session has instance id %q%s", want, listDebugInstances(live))
	}
	switch len(live) {
	case 0:
		return DebugInstance{}, errors.New("no herdr-edit is running a debug session")
	case 1:
		return live[0], nil
	}
	return DebugInstance{}, fmt.Errorf("%d herdr-edit debug sessions are running; pass --instance ID%s", len(live), listDebugInstances(live))
}

// listDebugInstances renders the instances for an error message.
func listDebugInstances(live []DebugInstance) string {
	var b strings.Builder
	for _, inst := range live {
		fmt.Fprintf(&b, "\n  %s\t%s", inst.ID, inst.Root)
	}
	return b.String()
}

// debugQueryCounter keeps two queries from one process unique even when the
// clock does not advance between them.
var debugQueryCounter atomic.Int64

// WriteDebugQuery sends an evaluate or frames request to req.Instance and
// returns the request id its reply will carry.
func WriteDebugQuery(req DebugRequest) (string, error) {
	if !IsDebugQuery(req.Action) {
		return "", errors.New("not a debug query: " + req.Action)
	}
	if req.Action == DebugActionEvaluate && req.Expression == "" {
		return "", errors.New("evaluate needs --expr EXPRESSION")
	}
	if !validDebugName(req.Instance) {
		return "", fmt.Errorf("invalid debug instance id %q", req.Instance)
	}
	req.Seq = time.Now().UnixNano()
	req.ID = fmt.Sprintf("%d-%d-%d", os.Getpid(), req.Seq, debugQueryCounter.Add(1))
	path := filepath.Join(debugQueryDir(req.Instance), req.ID+".json")
	if err := writeStateJSON(req, "query-*.tmp", path); err != nil {
		return "", err
	}
	return req.ID, nil
}

// TakeDebugQueries returns, in sequence order, the queries waiting for
// instance, removing each so it is answered exactly once. Unreadable and
// misaddressed files are removed and skipped.
func TakeDebugQueries(instance string) []DebugRequest {
	if !validDebugName(instance) {
		return nil
	}
	dir := debugQueryDir(instance)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []DebugRequest
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue // a writer's temp file, not yet renamed
		}
		path := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(path)
		os.Remove(path)
		if err != nil {
			continue
		}
		var req DebugRequest
		if json.Unmarshal(raw, &req) != nil || !IsDebugQuery(req.Action) || req.Seq == 0 ||
			req.Instance != instance || !validDebugName(req.ID) || e.Name() != req.ID+".json" {
			continue
		}
		if req.Action == DebugActionEvaluate && req.Expression == "" {
			continue
		}
		if time.Since(time.Unix(0, req.Seq)) > MaxDebugQueryAge {
			continue // its caller may already have given up; never execute it
		}
		out = append(out, req)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// DebugReply is one answer. Reply is the document itself, verbatim.
type DebugReply struct {
	ID    string          `json:"id"`
	Reply json.RawMessage `json:"reply"`
}

// WriteDebugReply answers the query with request id id. doc must be JSON.
func WriteDebugReply(id string, doc []byte) error {
	if !json.Valid(doc) {
		return errors.New("debug reply is not JSON")
	}
	if !validDebugName(id) {
		return fmt.Errorf("invalid debug request id %q", id)
	}
	return writeStateJSON(DebugReply{ID: id, Reply: doc}, "reply-*.tmp", debugReplyFile(id))
}

// ReadDebugReply returns the answer to id if it has arrived, removing it.
func ReadDebugReply(id string) (json.RawMessage, bool) {
	if !validDebugName(id) {
		return nil, false
	}
	raw, err := os.ReadFile(debugReplyFile(id))
	if err != nil {
		return nil, false
	}
	var r DebugReply
	if json.Unmarshal(raw, &r) != nil || r.ID != id || len(r.Reply) == 0 {
		return nil, false
	}
	os.Remove(debugReplyFile(id))
	return r.Reply, true
}

// AwaitDebugReply polls for the answer to the query id sent to instance until
// timeout. On timeout the query is withdrawn if no editor has taken it yet.
func AwaitDebugReply(instance, id string, timeout time.Duration) (json.RawMessage, error) {
	deadline := time.Now().Add(timeout)
	for {
		if r, ok := ReadDebugReply(id); ok {
			return r, nil
		}
		if time.Now().After(deadline) {
			if validDebugName(instance) && validDebugName(id) {
				os.Remove(filepath.Join(debugQueryDir(instance), id+".json"))
			}
			return nil, fmt.Errorf("debug session %s did not answer within %s", instance, timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
