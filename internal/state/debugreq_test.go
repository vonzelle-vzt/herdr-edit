// =============================================================================
// File: internal/state/debugreq_test.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestDebugRequest_RoundTrip covers the happy path across the file boundary,
// for the one action that carries a location as well as a verb.
func TestDebugRequest_RoundTrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := WriteDebugRequest(DebugActionToggleBreakpoint, "/proj/main.go", 42); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, ok := ReadDebugRequest()
	if !ok {
		t.Fatal("a request that was just written did not read back")
	}
	if got.Action != DebugActionToggleBreakpoint || got.File != "/proj/main.go" || got.Line != 42 {
		t.Fatalf("round-tripped to %+v", got)
	}
	if got.Seq == 0 {
		t.Error("seq must be set so the editor can tell new requests from honoured ones")
	}
}

// TestDebugTestRequestRoundTripsNode preserves the pytest node independently of its file.
func TestDebugTestRequestRoundTripsNode(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := WriteDebugRequest(DebugActionTest, "/proj/test_calc.py", 0, "TestK::test_a[p1]"); err != nil {
		t.Fatal(err)
	}
	got, ok := ReadDebugRequest()
	if !ok || got.Node != "TestK::test_a[p1]" || got.File != "/proj/test_calc.py" {
		t.Fatalf("round-trip = %+v, ok=%v", got, ok)
	}
}

// TestDebugRequest_SeqAdvances pins that two writes are distinguishable.
// Without a moving sequence the editor either replays the last request on every
// poll — stepping the program forever — or ignores everything after the first.
func TestDebugRequest_SeqAdvances(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := WriteDebugRequest(DebugActionNext, "", 0); err != nil {
		t.Fatal(err)
	}
	first, _ := ReadDebugRequest()
	if err := WriteDebugRequest(DebugActionNext, "", 0); err != nil {
		t.Fatal(err)
	}
	second, _ := ReadDebugRequest()
	if second.Seq <= first.Seq {
		t.Fatalf("seq did not advance: %d then %d", first.Seq, second.Seq)
	}
}

// TestDebugRequest_MissingIsAbsent covers the no-file case: a panel that has
// never been used is not an error condition.
func TestDebugRequest_MissingIsAbsent(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if _, ok := ReadDebugRequest(); ok {
		t.Fatal("with no file present there should be no request")
	}
}

// TestDebugRequest_MalformedIsAbsent pins that junk reads as "no request"
// rather than as an error the editor has to handle.
//
// The unknown-action case is the one that matters most: the editor switches on
// the verb, so an unrecognised one would either fall through silently (leaving
// the sequence unadvanced and the same junk re-read on every poll) or need a
// complaint path on the editor's hot loop. Rejecting it here means neither.
func TestDebugRequest_MalformedIsAbsent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	junk := []string{
		"",
		"not json",
		"{",
		"{}",
		`{"action":"start"}`,                     // no sequence
		`{"action":"start","seq":0}`,             // explicit zero sequence
		`{"action":"rm -rf /","seq":1}`,          // unknown action
		`{"action":"","seq":1}`,                  // empty action
		`{"action":"toggle-breakpoint","seq":1}`, // needs a file
		`{"action":"toggle-breakpoint","file":"","seq":1}`,
	}
	for _, j := range junk {
		if err := os.WriteFile(DebugRequestFile(), []byte(j), 0o644); err != nil {
			t.Fatal(err)
		}
		if got, ok := ReadDebugRequest(); ok {
			t.Errorf("malformed payload %q was accepted as %+v", j, got)
		}
	}
}

// TestDebugRequest_WriteRefusesJunk pins the other end of the same rule: the
// CLI refuses an action the editor would not honour, so a typo is reported to
// the person who made it instead of becoming a key that silently does nothing.
func TestDebugRequest_WriteRefusesJunk(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := WriteDebugRequest("contnue", "", 0); err == nil {
		t.Error("a misspelled action was accepted")
	}
	if err := WriteDebugRequest(DebugActionToggleBreakpoint, "", 0); err == nil {
		t.Error("toggle-breakpoint with no file was accepted, and has nothing to toggle")
	}
	if _, err := os.Stat(DebugRequestFile()); !os.IsNotExist(err) {
		t.Errorf("a refused request still wrote a file: %v", err)
	}
}

// TestDebugRequest_EveryActionIsAccepted walks the published action list and
// proves each one round-trips. It reads DebugActions() rather than restating
// the eight verbs, so an action added to the list without being wired up here
// cannot pass by being forgotten.
func TestDebugRequest_EveryActionIsAccepted(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, action := range DebugActions() {
		file := ""
		node := ""
		if action == DebugActionToggleBreakpoint {
			file = "/proj/main.go"
		}
		if action == DebugActionTest {
			file, node = "/proj/test_main.py", "test_one"
		}
		if IsDebugQuery(action) {
			// The two queries travel through WriteDebugQuery, with the CLI's own
			// parse of their flags.
			args := []string{"--instance", "a"}
			if action == DebugActionEvaluate {
				args = append(args, "--expr", "x")
			}
			q, err := ParseDebugQuery(action, args)
			if err != nil {
				t.Fatalf("%s: %v", action, err)
			}
			if _, err := WriteDebugQuery(q); err != nil {
				t.Fatalf("%s: %v", action, err)
			}
			if got := TakeDebugQueries("a"); len(got) != 1 || got[0].Action != action {
				t.Fatalf("%s did not round-trip (got %+v)", action, got)
			}
			continue
		} else if err := WriteDebugRequest(action, file, 1, node); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		got, ok := ReadDebugRequest()
		if !ok || got.Action != action {
			t.Fatalf("%s did not round-trip (got %+v, ok=%v)", action, got, ok)
		}
		if !ValidDebugAction(action) {
			t.Errorf("%s is in DebugActions() but ValidDebugAction rejects it", action)
		}
	}
}

// TestDebugRequest_LeavesNoTempFiles pins the atomic-write discipline: the temp
// file is a sibling of the target so the rename is atomic, and it is always
// renamed away rather than accumulating in the state directory.
func TestDebugRequest_LeavesNoTempFiles(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for i := 0; i < 5; i++ {
		if err := WriteDebugRequest(DebugActionContinue, "", 0); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(Dir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(DebugRequestFile()) {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("state directory holds %v, want just debug-request.json", names)
	}
}

// TestDebugQuery_ParsesTheCLIFlags pins `--debug evaluate` and `--debug frames`
// argument handling, including frame 0 (a real id, not "unset") and every
// refusal the CLI reports before anything is written.
func TestDebugQuery_ParsesTheCLIFlags(t *testing.T) {
	q, err := ParseDebugQuery(DebugActionEvaluate, []string{"--expr", "a.b[0]", "--frame", "0", "--context", "watch", "--allow-side-effects"})
	if err != nil {
		t.Fatal(err)
	}
	if q.Expression != "a.b[0]" || q.FrameID == nil || *q.FrameID != 0 || q.Context != "watch" || !q.AllowSideEffects {
		t.Fatalf("parsed %+v", q)
	}
	if q, err := ParseDebugQuery(DebugActionEvaluate, []string{"--expr", "x"}); err != nil || q.FrameID != nil || q.Context != "" || q.AllowSideEffects {
		t.Fatalf("defaults: %+v, %v — want no frame, no context, no side effects", q, err)
	}
	if _, err := ParseDebugQuery(DebugActionFrames, nil); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{DebugActionEvaluate, DebugActionFrames} {
		q, err := ParseDebugQuery(action, []string{"--instance", "4242", "--expr", "x"}[:map[string]int{DebugActionEvaluate: 4, DebugActionFrames: 2}[action]])
		if err != nil || q.Instance != "4242" {
			t.Errorf("%s --instance: %+v, %v", action, q, err)
		}
	}
	bad := map[string][]string{
		"expr no value":  {"--expr"},
		"negative frame": {"--expr", "x", "--frame", "-1"},
		"word frame":     {"--expr", "x", "--frame", "top"},
		"bad context":    {"--expr", "x", "--context", "eval"},
		"unknown flag":   {"--expr", "x", "--script", "y"},
	}
	for name, args := range bad {
		if _, err := ParseDebugQuery(DebugActionEvaluate, args); err == nil {
			t.Errorf("%s: %v parsed", name, args)
		}
	}
	if _, err := ParseDebugQuery(DebugActionFrames, []string{"--expr", "x"}); err == nil {
		t.Error("frames accepted arguments")
	}
	if _, err := ParseDebugQuery(DebugActionContinue, nil); err == nil {
		t.Error("continue parsed as a query")
	}
}

// TestDebugQuery_RoundTripsAndRefusesBadPayloads pins the query's fields on the
// wire, that a query reaches only the instance it names, and that a query in
// the global request file is honoured by nobody.
func TestDebugQuery_RoundTripsAndRefusesBadPayloads(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	zero := 0
	id, err := WriteDebugQuery(DebugRequest{Action: DebugActionEvaluate, Instance: "a", Expression: "x", FrameID: &zero, Context: "repl", AllowSideEffects: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := TakeDebugQueries("b"); len(got) != 0 {
		t.Fatalf("instance b took a query addressed to a: %+v", got)
	}
	got := TakeDebugQueries("a")
	if len(got) != 1 || got[0].ID != id || got[0].Expression != "x" || got[0].FrameID == nil || *got[0].FrameID != 0 ||
		got[0].Context != "repl" || !got[0].AllowSideEffects {
		t.Fatalf("round trip = %+v", got)
	}
	if again := TakeDebugQueries("a"); len(again) != 0 {
		t.Fatalf("a query was taken twice: %+v", again)
	}
	// `--debug evaluate` with no --expr parses (it resolves like every verb)
	// and is refused at the write, so nothing reaches the editor.
	q, err := ParseDebugQuery(DebugActionEvaluate, []string{"--context", "hover"})
	if err != nil {
		t.Fatal(err)
	}
	q.Instance = "a"
	if _, err := WriteDebugQuery(q); err == nil || !strings.Contains(err.Error(), "--expr") {
		t.Errorf("an evaluate with no expression: err = %v, want a refusal naming --expr", err)
	}
	if _, err := WriteDebugQuery(DebugRequest{Action: DebugActionFrames, Instance: "../x"}); err == nil {
		t.Error("an instance id that names a path was accepted")
	}
	if err := WriteDebugRequest(DebugActionEvaluate, "", 0); err == nil {
		t.Error("WriteDebugRequest accepted a query, which would skip its fields")
	}
	if err := os.WriteFile(DebugRequestFile(), []byte(`{"action":"evaluate","expression":"x","seq":5}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadDebugRequest(); ok {
		t.Error("a query in the global request file was honoured; it is addressed to nobody")
	}
	dir := filepath.Join(Dir(), "debug-queries", "a")
	if err := os.WriteFile(filepath.Join(dir, "junk.json"), []byte(`{"action":"evaluate","seq":5,"instance":"a","id":"junk"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := TakeDebugQueries("a"); len(got) != 0 {
		t.Errorf("an evaluate with no expression was taken: %+v", got)
	}
}

// TestDebugQuery_ConcurrentQueriesKeepTheirOwnReplies pins unique requests: two
// queries sent at the same moment both arrive, and each caller reads exactly
// its own answer.
func TestDebugQuery_ConcurrentQueriesKeepTheirOwnReplies(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	exprs := []string{"left", "right"}
	ids := make([]string, len(exprs))
	var wg sync.WaitGroup
	for i, e := range exprs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := WriteDebugQuery(DebugRequest{Action: DebugActionEvaluate, Instance: "a", Expression: e})
			if err != nil {
				t.Error(err)
			}
			ids[i] = id
		}()
	}
	wg.Wait()
	if ids[0] == ids[1] {
		t.Fatalf("two queries share id %s", ids[0])
	}

	// The editor's side: take both and answer each with its own expression.
	taken := TakeDebugQueries("a")
	if len(taken) != 2 {
		t.Fatalf("took %d queries, want both", len(taken))
	}
	for _, q := range taken {
		doc, _ := json.Marshal(map[string]string{"expr": q.Expression})
		if err := WriteDebugReply(q.ID, doc); err != nil {
			t.Fatal(err)
		}
	}

	results := make([]string, len(exprs))
	for i := range exprs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := AwaitDebugReply("a", ids[i], 5*time.Second)
			if err != nil {
				t.Error(err)
				return
			}
			var doc map[string]string
			_ = json.Unmarshal(r, &doc)
			results[i] = doc["expr"]
		}()
	}
	wg.Wait()
	for i, e := range exprs {
		if results[i] != e {
			t.Errorf("query %q read the answer %q", e, results[i])
		}
	}
}

// TestDebugReply_IsKeyedByTheRequestID pins the reply half: an answer is read
// only by the query it answers, once, a non-JSON reply is refused, and a query
// nobody answers times out and is withdrawn.
func TestDebugReply_IsKeyedByTheRequestID(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := WriteDebugReply("q41", []byte(`{"schemaVersion":1,"error":"old"}`)); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadDebugReply("q42"); ok {
		t.Error("query 42 read query 41's answer")
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = WriteDebugReply("q42", []byte(`{"schemaVersion":1,"kind":"text"}`))
	}()
	got, err := AwaitDebugReply("a", "q42", 5*time.Second)
	if err != nil || string(got) != `{"schemaVersion":1,"kind":"text"}` {
		t.Fatalf("await = %s, %v", got, err)
	}
	if _, ok := ReadDebugReply("q42"); ok {
		t.Error("an answer was read twice")
	}
	if err := WriteDebugReply("q43", []byte("not json")); err == nil {
		t.Error("a non-JSON reply was written")
	}
	id, err := WriteDebugQuery(DebugRequest{Action: DebugActionFrames, Instance: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AwaitDebugReply("a", id, 60*time.Millisecond); err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Errorf("unanswered query: err = %v", err)
	}
	if left := TakeDebugQueries("a"); len(left) != 0 {
		t.Errorf("an abandoned query stayed queued: %+v", left)
	}
	filepath.WalkDir(Dir(), func(path string, d os.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(path, ".tmp") {
			t.Errorf("temp file left behind: %s", path)
		}
		return nil
	})
}

// TestDebugInstance_AddressingRule pins how the CLI picks a session: the only
// live one, else the one --instance names; several without --instance is a
// refusal that lists them, and a dead editor's registration is dropped.
func TestDebugInstance_AddressingRule(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dead := 999999
	defer func(f func(int) bool) { processAlive = f }(processAlive)
	processAlive = func(pid int) bool { return pid != dead }

	if _, err := ChooseDebugInstance(""); err == nil || !strings.Contains(err.Error(), "no herdr-edit") {
		t.Errorf("no sessions: err = %v", err)
	}
	for _, inst := range []DebugInstance{{ID: "100", PID: 100, Root: "/p/one"}, {ID: "dead", PID: dead, Root: "/p/gone"}} {
		if err := RegisterDebugInstance(inst); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ChooseDebugInstance("")
	if err != nil || got.ID != "100" {
		t.Fatalf("one live session: %+v, %v", got, err)
	}
	if DebugInstanceRegistered("dead") {
		t.Error("a dead editor's registration survived a listing")
	}
	if err := RegisterDebugInstance(DebugInstance{ID: "200", PID: 200, Root: "/p/two"}); err != nil {
		t.Fatal(err)
	}
	_, err = ChooseDebugInstance("")
	if err == nil || !strings.Contains(err.Error(), "--instance") {
		t.Fatalf("two sessions without --instance: err = %v, want a refusal", err)
	}
	for _, want := range []string{"100\t/p/one", "200\t/p/two"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not list %q", err, want)
		}
	}
	if got, err := ChooseDebugInstance("200"); err != nil || got.Root != "/p/two" {
		t.Errorf("--instance 200: %+v, %v", got, err)
	}
	if _, err := ChooseDebugInstance("300"); err == nil || !strings.Contains(err.Error(), "100") {
		t.Errorf("--instance 300: err = %v, want a refusal listing the live ones", err)
	}
	UnregisterDebugInstance("200")
	if DebugInstanceRegistered("200") {
		t.Error("unregister left the registration")
	}
	if err := RegisterDebugInstance(DebugInstance{ID: "a/b"}); err == nil {
		t.Error("an id that names a path was registered")
	}
}

// A query older than MaxDebugQueryAge is dropped UNEXECUTED: its caller may already have stopped
// waiting, and a side effect must never run for nobody.
func TestTakeDebugQueriesDropsQueriesTheCallerMayHaveAbandoned(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fresh, err := WriteDebugQuery(DebugRequest{Action: DebugActionEvaluate, Instance: "inst1", Expression: "x"})
	if err != nil {
		t.Fatal(err)
	}
	stale := DebugRequest{Action: DebugActionEvaluate, Instance: "inst1", Expression: "x = 1",
		Seq: time.Now().Add(-MaxDebugQueryAge - time.Second).UnixNano(), ID: "stale-1"}
	raw, _ := json.Marshal(stale)
	if err := os.WriteFile(filepath.Join(debugQueryDir("inst1"), "stale-1.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	got := TakeDebugQueries("inst1")
	if len(got) != 1 || got[0].ID != fresh {
		t.Fatalf("took %+v, want only the fresh query %s", got, fresh)
	}
}
