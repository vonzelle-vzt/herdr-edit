// =============================================================================
// File: internal/langconf/langconf_test.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// Tests for the compiled-in language table. Two kinds live here: assertions
// about specific languages we care about behaving correctly (Rust's missing
// apostrophe, Python's `"""`), and the drift guard that re-runs the generator
// and demands the committed data.go match it byte for byte.

package langconf

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"testing"

	"github.com/cloudmanic/spice-edit/internal/lsp"
)

// noSourceExit is the exit code gen/main.go uses for "no VS Code install to
// read", so the drift test can tell a missing toolchain from a real
// disagreement instead of skipping on any failure at all.
const noSourceExit = 3

// TestBlockCommentTokensPerLanguage pins the block-comment data this change
// exists to add: Python's block comment is a triple quote on both sides while
// Go's and Rust's are the C form. Before this table there was no block-comment
// data anywhere in the editor, so every one of these was empty.
func TestBlockCommentTokensPerLanguage(t *testing.T) {
	cases := []struct {
		path       string
		wantStart  string
		wantEnd    string
		wantLine   string
		wantExists bool
	}{
		{path: "main.py", wantStart: `"""`, wantEnd: `"""`, wantLine: "#", wantExists: true},
		{path: "main.go", wantStart: "/*", wantEnd: "*/", wantLine: "//", wantExists: true},
		{path: "lib.rs", wantStart: "/*", wantEnd: "*/", wantLine: "//", wantExists: true},
		{path: "app.ts", wantStart: "/*", wantEnd: "*/", wantLine: "//", wantExists: true},
		{path: "style.css", wantStart: "/*", wantEnd: "*/", wantExists: true},
		{path: "shader.wat", wantExists: false},
	}
	for _, c := range cases {
		start, end, ok := BlockComment(c.path)
		if ok != c.wantExists {
			t.Errorf("%s: BlockComment ok = %v, want %v", c.path, ok, c.wantExists)
			continue
		}
		if start != c.wantStart || end != c.wantEnd {
			t.Errorf("%s: block comment = %q/%q, want %q/%q", c.path, start, end, c.wantStart, c.wantEnd)
		}
		if c.wantLine == "" {
			continue
		}
		if line, lok := LineComment(c.path); !lok || line != c.wantLine {
			t.Errorf("%s: line comment = %q (ok=%v), want %q", c.path, line, lok, c.wantLine)
		}
	}
}

// TestRustOmitsTheLifetimeApostrophe is the data half of the bug this change
// fixes. `'a` in Rust is a lifetime, not a string, so upstream deliberately
// leaves `'` out of Rust's auto-closing pairs while keeping it for Go and
// Python. The editor-side behaviour is pinned by
// TestRustDoesNotAutoCloseALifetimeApostrophe in internal/editor.
func TestRustOmitsTheLifetimeApostrophe(t *testing.T) {
	rust, ok := AutoClosePairs("lib.rs")
	if !ok {
		t.Fatal("rust should be covered by the table")
	}
	if _, closes := rust['\'']; closes {
		t.Error("rust must not auto-close ' — 'a is a lifetime, not a string")
	}
	for _, open := range []rune{'(', '[', '{', '"'} {
		if _, closes := rust[open]; !closes {
			t.Errorf("rust should still auto-close %q", open)
		}
	}
	for _, path := range []string{"main.go", "main.py"} {
		pairs, pok := AutoClosePairs(path)
		if !pok {
			t.Fatalf("%s should be covered by the table", path)
		}
		if got, closes := pairs['\'']; !closes || got != '\'' {
			t.Errorf("%s should auto-close ' with ', got %q (ok=%v)", path, got, closes)
		}
	}
}

// TestSurroundPairsAreNotTheAutoClosePairs proves the two tables are kept
// apart for a reason: Rust surrounds a selection with `<`/`>` and `"` but
// auto-closes neither `<` nor `'`. Collapsing them into one map would either
// lose the angle brackets or resurrect the lifetime-apostrophe bug.
func TestSurroundPairsAreNotTheAutoClosePairs(t *testing.T) {
	surround, ok := SurroundPairs("lib.rs")
	if !ok {
		t.Fatal("rust should have surrounding pairs")
	}
	if got := surround['<']; got != '>' {
		t.Errorf("rust should surround with <>, got %q", got)
	}
	auto, _ := AutoClosePairs("lib.rs")
	if _, closes := auto['<']; closes {
		t.Error("rust must NOT auto-close < — that would pair every less-than")
	}
	if reflect.DeepEqual(auto, surround) {
		t.Error("auto-closing and surrounding pairs are identical for rust; the distinction was lost")
	}
}

// TestMultiRuneOpenersAreDroppedFromTheRuneTables records what the single-rune
// projection throws away. Python opens `r"` and `f'`, TypeScript opens `${`
// and `/**`; none of those can be triggered by one typed rune, so they are
// absent from the rune tables while surviving in the Config for a caller that
// could use them.
func TestMultiRuneOpenersAreDroppedFromTheRuneTables(t *testing.T) {
	py, ok := ForPath("main.py")
	if !ok {
		t.Fatal("python should be covered")
	}
	multi := 0
	for _, p := range py.AutoClosing {
		if len([]rune(p.Open)) > 1 {
			multi++
		}
	}
	if multi == 0 {
		t.Fatal("python's config should still carry its multi-rune openers (r\", f\", …)")
	}
	pairs, _ := AutoClosePairs("main.py")
	for open := range pairs {
		if open == 0 {
			t.Error("a zero rune reached the rune table")
		}
	}
	// The projection keeps exactly the six single-rune openers Python has.
	want := []rune{'(', '[', '{', '"', '\'', '`'}
	if len(pairs) != len(want) {
		t.Errorf("python rune pairs = %d, want %d (%q)", len(pairs), len(want), pairs)
	}
	for _, open := range want {
		if _, closes := pairs[open]; !closes {
			t.Errorf("python should auto-close %q", open)
		}
	}
}

// TestNotInIsRecordedButNotEnforced pins the honest limitation. Upstream marks
// Rust's `"` as notIn:["string"]; we keep that data and still offer the pair,
// because nothing here can tell whether the cursor is inside a string. If a
// future change starts enforcing it, this test is where the claim must be
// updated rather than quietly left behind.
func TestNotInIsRecordedButNotEnforced(t *testing.T) {
	rust, ok := ForPath("lib.rs")
	if !ok {
		t.Fatal("rust should be covered")
	}
	if scopes := NotInScopes(rust); len(scopes) == 0 {
		t.Fatal("rust's config should record a notIn scope for its quote pair")
	}
	pairs, _ := AutoClosePairs("lib.rs")
	if _, closes := pairs['"']; !closes {
		t.Error(`rust's " is offered unconditionally today; dropping notIn pairs entirely would regress it`)
	}
}

// TestUncoveredExtensionsReportThemselvesAsUncovered makes sure an unknown
// file type answers "no data" rather than "no pairs" — the distinction the
// editor's fallback depends on.
func TestUncoveredExtensionsReportThemselvesAsUncovered(t *testing.T) {
	for _, path := range []string{"module.wat", "notes.xyzzy", "", "Makefile.unknownext"} {
		if Covers(path) {
			t.Errorf("%q should not be covered by the table", path)
		}
		if _, ok := AutoClosePairs(path); ok {
			t.Errorf("%q should have no auto-close pairs", path)
		}
		if _, _, ok := BlockComment(path); ok {
			t.Errorf("%q should have no block comment", path)
		}
	}
}

// TestLanguageOfReusesTheLSPMapping is the anti-drift guard for the one thing
// this package deliberately does not own: the extension→language map. A second
// copy would let the editor and the language server disagree about what a file
// is, and nothing on screen would explain it.
func TestLanguageOfReusesTheLSPMapping(t *testing.T) {
	for _, path := range []string{"a.rs", "b.py", "c.go", "d.tsx", "e.unknown", "f.md"} {
		if got, want := LanguageOf(path), lsp.LanguageID(path); got != want {
			t.Errorf("%s: LanguageOf = %q, lsp.LanguageID = %q — the maps have drifted", path, got, want)
		}
	}
}

// TestTableIsPopulatedAndSorted is the smoke test that the generator ran at
// all: an empty table would let every other assertion above fail loudly, but
// this one names the actual failure.
func TestTableIsPopulatedAndSorted(t *testing.T) {
	ids := Languages()
	if len(ids) < 40 {
		t.Fatalf("only %d languages in the table — did the generator run? (SourceVersion=%q)", len(ids), SourceVersion)
	}
	if !sort.StringsAreSorted(ids) {
		t.Error("Languages() must return a sorted list")
	}
	if SourceVersion == "" {
		t.Error("SourceVersion is empty; provenance is not checkable")
	}
	for _, id := range ids {
		cfg := configs[id]
		if cfg.ID != id {
			t.Errorf("config keyed %q carries ID %q", id, cfg.ID)
		}
	}
}

// TestGeneratedDataMatchesTheSource re-runs the generator into a temp dir and
// demands the committed data.go be byte-identical. This is what stops the
// table drifting from upstream after someone hand-edits it — a hand-edit is
// invisible to every other test here, because every other test asserts what
// the hand-edit would have said.
//
// It skips loudly when VS Code is not installed (CI), and FAILS when it is
// installed and the data disagrees.
func TestGeneratedDataMatchesTheSource(t *testing.T) {
	goBin := goToolPath(t)
	out := filepath.Join(t.TempDir(), "data.go")

	// 🔴 BUILD the generator, then run the binary. `go run` does NOT propagate the
	// child's exit code — it reports 1 whatever the program returned — so the
	// noSourceExit sentinel was unreachable through it and this test FAILED on CI
	// instead of skipping, in an environment with no VS Code to regenerate from.
	// TestSkipSentinelMatchesTheGenerator did not catch it: it proved the constant
	// agreed with the generator, not that the exit code ever reached the caller.
	bin := filepath.Join(t.TempDir(), "langconfgen")
	build := exec.Command(goBin, "build", "-o", bin, "./gen")
	var buildErr bytes.Buffer
	build.Stderr = &buildErr
	if err := build.Run(); err != nil {
		t.Fatalf("building ./gen failed: %v\n%s", err, buildErr.String())
	}

	cmd := exec.Command(bin, "-out", out)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == noSourceExit {
			t.Skipf("SKIPPING DRIFT CHECK: no VS Code installation to regenerate from — %s", stderr.String())
		}
		t.Fatalf("running the generator failed: %v\n%s", err, stderr.String())
	}

	fresh, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read regenerated data: %v", err)
	}
	committed, err := os.ReadFile("data.go")
	if err != nil {
		t.Fatalf("read committed data.go: %v", err)
	}
	if !bytes.Equal(fresh, committed) {
		t.Errorf("data.go does not match a fresh run of the generator (%d bytes committed, %d regenerated).\n"+
			"Either it was hand-edited, or the installed VS Code version changed.\n"+
			"Fix with: go generate ./internal/langconf\n%s",
			len(committed), len(fresh), firstDiff(committed, fresh))
	}
}

// firstDiff renders the first differing line of two files, so a drift failure
// says what changed instead of only that something did.
func firstDiff(a, b []byte) string {
	al := bytes.Split(a, []byte("\n"))
	bl := bytes.Split(b, []byte("\n"))
	for i := 0; i < len(al) && i < len(bl); i++ {
		if !bytes.Equal(al[i], bl[i]) {
			return "first difference at line " + itoa(i+1) + ":\n  committed:   " + string(al[i]) + "\n  regenerated: " + string(bl[i])
		}
	}
	return "files share a common prefix; one is longer than the other"
}

// itoa is a tiny int-to-string so firstDiff does not pull strconv in for one
// call in a failure path.
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

// goToolPath finds the go command for the drift test, falling back to GOROOT
// when PATH does not carry it — `go test` can be invoked from a toolchain that
// is not on PATH, and skipping in that case would silently disable the guard.
func goToolPath(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("go"); err == nil {
		return p
	}
	p := filepath.Join(runtime.GOROOT(), "bin", "go")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("no go tool on PATH or in GOROOT (%s)", runtime.GOROOT())
	}
	return p
}

// TestSkipSentinelMatchesTheGenerator stops the drift check quietly becoming a
// gate that measures nothing. TestGeneratedDataMatchesTheSource skips only on
// exit code noSourceExit and fails on anything else; if the generator's copy of
// that constant changed, the skip branch would stop matching and a genuine
// "no VS Code" run would report a hard failure instead — or, worse, a changed
// constant here would swallow real generator errors as skips.
func TestSkipSentinelMatchesTheGenerator(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("gen", "main.go"))
	if err != nil {
		t.Fatalf("read gen/main.go: %v", err)
	}
	want := "const noSourceExit = " + itoa(noSourceExit)
	if !bytes.Contains(src, []byte(want)) {
		t.Errorf("gen/main.go does not declare %q — the skip sentinel has drifted", want)
	}
}
