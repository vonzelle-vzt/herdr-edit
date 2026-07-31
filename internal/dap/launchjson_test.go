// =============================================================================
// File: internal/dap/launchjson_test.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package dap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestStripJSONCLeavesStringsAlone is the test the regex approach fails.
//
// The usual stripper is `(^|[^:])//.*$`, whose "not after a colon" guard exists
// solely to save `http://`. That guard is a coincidence of URLs having a colon
// before their slashes; every OTHER `//` inside a string still gets eaten. Each
// case below is a string a regex stripper truncates, turning a valid file into
// a parse error the user is then blamed for.
func TestStripJSONCLeavesStringsAlone(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // the value that must survive at key "v"
	}{
		{"url after colon", `{"v": "http://example.com/x"}`, "http://example.com/x"},
		{"url with no colon guard", `{"v": "//leading-double-slash"}`, "//leading-double-slash"},
		{"unc path", `{"v": "\\\\server\\share"}`, `\\server\share`},
		{"regex with slashes", `{"v": "a//b//c"}`, "a//b//c"},
		{"block comment marker inside a string", `{"v": "not /* a comment */ really"}`, "not /* a comment */ really"},
		{"escaped quote then slashes", `{"v": "he said \"hi\" // not a comment"}`, `he said "hi" // not a comment`},
		{"trailing backslash before the closing quote", `{"v": "ends with a backslash \\"}`, `ends with a backslash \`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got struct {
				V string `json:"v"`
			}
			stripped := StripJSONC([]byte(tc.in))
			if err := json.Unmarshal(stripped, &got); err != nil {
				t.Fatalf("stripping broke valid JSON: %v\ninput:   %s\nstripped: %s", err, tc.in, stripped)
			}
			if got.V != tc.want {
				t.Errorf("value = %q, want %q\nstripped: %s", got.V, tc.want, stripped)
			}
		})
	}
}

// TestStripJSONCRemovesRealComments checks it still does the job it exists for.
func TestStripJSONCRemovesRealComments(t *testing.T) {
	in := `{
  // a line comment
  "name": "Debug", // a trailing line comment
  /* a block
     comment spanning lines */
  "type": "go"
}`
	var got map[string]string
	if err := json.Unmarshal(StripJSONC([]byte(in)), &got); err != nil {
		t.Fatalf("unmarshal: %v\nstripped: %s", err, StripJSONC([]byte(in)))
	}
	if got["name"] != "Debug" || got["type"] != "go" {
		t.Errorf("got %v, want name=Debug type=go", got)
	}
}

// TestStripJSONCRemovesTrailingCommas covers the other thing encoding/json
// refuses and VS Code accepts. A launch.json with a trailing comma opens fine
// in the user's other editor, so rejecting it would be us being wrong.
func TestStripJSONCRemovesTrailingCommas(t *testing.T) {
	cases := []string{
		`{"a": 1, "b": 2,}`,
		`{"a": [1, 2, 3,],}`,
		`{"a": 1, /* why not */ }`,
		"{\"a\": [1, 2,\n // last\n ]}",
	}
	for _, in := range cases {
		var got map[string]interface{}
		stripped := StripJSONC([]byte(in))
		if err := json.Unmarshal(stripped, &got); err != nil {
			t.Errorf("trailing comma survived: %v\ninput: %s\nstripped: %s", err, in, stripped)
		}
	}
}

// TestStripJSONCKeepsCommasThatAreNotTrailing is the other direction: a comma
// separating two real values must survive, or the stripper silently corrupts
// every multi-key object.
func TestStripJSONCKeepsCommasThatAreNotTrailing(t *testing.T) {
	in := `{"a": 1, "b": 2, "c": [1, 2, 3]}`
	var got map[string]interface{}
	if err := json.Unmarshal(StripJSONC([]byte(in)), &got); err != nil {
		t.Fatalf("unmarshal: %v\nstripped: %s", err, StripJSONC([]byte(in)))
	}
	if len(got) != 3 {
		t.Fatalf("got %d keys, want 3 — a separating comma was eaten", len(got))
	}
	// And a comma inside a string is not a comma at all.
	var s struct {
		V string `json:"v"`
	}
	if err := json.Unmarshal(StripJSONC([]byte(`{"v": "a,}"}`)), &s); err != nil {
		t.Fatalf("comma inside a string: %v", err)
	}
	if s.V != "a,}" {
		t.Errorf("value = %q, want %q", s.V, "a,}")
	}
}

// TestParseLaunchJSONReadsAVSCodeFile parses a realistic launch.json — comments,
// trailing comma, and keys we do not model — and checks the unmodelled keys
// survive into Args rather than being silently discarded.
func TestParseLaunchJSONReadsAVSCodeFile(t *testing.T) {
	in := []byte(`{
  // See https://go.microsoft.com/fwlink/?linkid=830387
  "version": "0.2.0",
  "configurations": [
    {
      "name": "Launch Package",
      "type": "go",
      "request": "launch",
      "mode": "debug",
      "program": "${workspaceFolder}",
      "args": ["--verbose"],
    },
    {
      "name": "Attach to Process",
      "type": "go",
      "request": "attach",
      "mode": "local",
      "processId": 0
    }
  ]
}`)
	cfgs, err := ParseLaunchJSON(in)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cfgs) != 2 {
		t.Fatalf("got %d configurations, want 2", len(cfgs))
	}
	first := cfgs[0]
	if first.Name != "Launch Package" || first.Type != "go" || first.Request != "launch" {
		t.Errorf("first config = %+v", first)
	}
	// The keys we do NOT model must still reach the adapter.
	if first.Args["mode"] != "debug" {
		t.Errorf("mode was dropped from Args: %v", first.Args)
	}
	if first.Args["program"] != "${workspaceFolder}" {
		t.Errorf("program was dropped from Args: %v", first.Args)
	}
	if _, ok := first.Args["args"]; !ok {
		t.Errorf("the user's args list was dropped: %v", first.Args)
	}
	if cfgs[1].Request != "attach" {
		t.Errorf("second config request = %q, want attach", cfgs[1].Request)
	}
}

// TestLoadLaunchConfigsMissingFileIsNotAnError pins the "most projects have
// none" case: an absent launch.json must not surface as a failure.
func TestLoadLaunchConfigsMissingFileIsNotAnError(t *testing.T) {
	cfgs, err := LoadLaunchConfigs(t.TempDir())
	if err != nil {
		t.Fatalf("a missing launch.json reported an error: %v", err)
	}
	if len(cfgs) != 0 {
		t.Errorf("got %d configs from a project with no launch.json", len(cfgs))
	}
}

// TestLoadLaunchConfigsReadsFromDisk covers the real path, including the
// .vscode directory location.
func TestLoadLaunchConfigsReadsFromDisk(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".vscode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{
  "version": "0.2.0",
  "configurations": [
    // the one we actually use
    {"name": "Debug main", "type": "go", "request": "launch", "program": "./cmd/app"}
  ]
}`
	if err := os.WriteFile(filepath.Join(dir, "launch.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgs, err := LoadLaunchConfigs(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfgs) != 1 || cfgs[0].Name != "Debug main" {
		t.Fatalf("got %+v, want one config named 'Debug main'", cfgs)
	}
}

// TestLoadLaunchConfigsMalformedIsAnError checks a genuinely broken file is
// reported. Silently ignoring it would leave the user wondering why the
// configuration they wrote does nothing.
func TestLoadLaunchConfigsMalformedIsAnError(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".vscode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "launch.json"), []byte(`{"configurations": [`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadLaunchConfigs(root); err == nil {
		t.Fatal("a truncated launch.json parsed without error")
	}
}
