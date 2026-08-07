// =============================================================================
// File: internal/dap/launchselect_test.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package dap

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeLaunchJSON drops a real .vscode/launch.json into root.
func writeLaunchJSON(t *testing.T, root, body string) {
	t.Helper()
	dir := filepath.Join(root, ".vscode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "launch.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestLaunchSpecNamesAFileThatExists is the Stage 1 oracle, and its assertion is
// an os.Stat rather than a string comparison ON PURPOSE.
//
// 🔴 An oracle that compares the resolved program against
// filepath.Join(root, "cmd", "app") is a SECOND COPY OF THE SUBSTITUTION
// FORMULA: it agrees with any expansion that is self-consistent, including one
// that is wrong in the same way twice. CLAUDE.md records this exact failure —
// ScreenPos shipped wrong twice with green tests, because the tests restated
// the arithmetic they were checking. Asking the filesystem is a question the
// code under test cannot answer for itself.
//
// The whole path is real: JSONC on disk with a comment and a trailing comma,
// a directory that genuinely exists, and a stat at the end.
func TestLaunchSpecNamesAFileThatExists(t *testing.T) {
	root := t.TempDir()

	// A real directory, created for real. delve's `program` is a PACKAGE
	// directory, which is why this is a MkdirAll and not a WriteFile.
	want := filepath.Join(root, "cmd", "app")
	if err := os.MkdirAll(want, 0o755); err != nil {
		t.Fatal(err)
	}

	writeLaunchJSON(t, root, `{
  "version": "0.2.0",
  "configurations": [
    // JSONC, exactly as VS Code writes it.
    {
      "name": "Launch Package",
      "type": "go",
      "request": "launch",
      "program": "${workspaceFolder}/cmd/app",
    },
  ]
}`)

	file, err := LoadLaunchFile(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(file.Configurations) != 1 {
		t.Fatalf("got %d configurations, want 1", len(file.Configurations))
	}

	spec, err := ResolveLaunchConfig(file.Configurations[0], LaunchVarContext{
		WorkspaceFolder: root,
		File:            filepath.Join(root, "cmd", "app", "main.go"),
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	program, _ := spec.Args["program"].(string)
	if program == "" {
		t.Fatal("the resolved configuration has no program at all")
	}

	// 🔴 THE assertion: the resolved program must name something on disk.
	// A literal "${workspaceFolder}/cmd/app" fails here; so does any partial
	// expansion, and so does an expansion against the wrong root.
	fi, err := os.Stat(program)
	if err != nil {
		t.Fatalf("the resolved program does not exist on disk: %v\nresolved to: %s", err, program)
	}
	if !fi.IsDir() {
		t.Fatalf("resolved program %s is not the package directory delve needs", program)
	}
	if spec.Adapter.Name != "delve" {
		t.Errorf("adapter = %q, want delve — selection must follow the config's type", spec.Adapter.Name)
	}
	if spec.Request != "launch" {
		t.Errorf("request = %q, want launch", spec.Request)
	}
	if spec.Name != "Launch Package" {
		t.Errorf("name = %q, want the configuration's own name", spec.Name)
	}
}

// TestCompoundsAreRead pins the field that had never been read at all.
//
// A user with a compound saw their configuration simply MISSING from a picker
// built off `configurations` alone, which reads as the picker being broken
// rather than as a feature this editor does not have.
func TestCompoundsAreRead(t *testing.T) {
	file, err := ParseLaunchFile([]byte(`{
  "version": "0.2.0",
  "configurations": [
    {"name": "Server", "type": "node", "request": "launch"},
    {"name": "Browser", "type": "chrome", "request": "launch", "url": "http://localhost:3000"}
  ],
  "compounds": [
    {"name": "Full Stack", "configurations": ["Server", "Browser"]},
    {"name": "Multi Root", "configurations": [{"folder": "web", "name": "Browser"}]}
  ]
}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(file.Compounds) != 2 {
		t.Fatalf("got %d compounds, want 2 — a compound the user can see in their own file "+
			"must not vanish here", len(file.Compounds))
	}
	if file.Compounds[0].Name != "Full Stack" {
		t.Errorf("compound name = %q", file.Compounds[0].Name)
	}
	if !reflect.DeepEqual(file.Compounds[0].Configurations, []string{"Server", "Browser"}) {
		t.Errorf("members = %v, want [Server Browser]", file.Compounds[0].Configurations)
	}
	// The multi-root object shape carries the name in a field rather than being
	// a bare string. Rendering it as a Go map into a user-facing refusal would
	// be worse than dropping it.
	if !reflect.DeepEqual(file.Compounds[1].Configurations, []string{"Browser"}) {
		t.Errorf("multi-root members = %v, want [Browser]", file.Compounds[1].Configurations)
	}
	// And the wrappers still see only the configurations.
	cfgs, err := ParseLaunchJSON([]byte(`{"configurations": [{"name": "A", "type": "go"}], "compounds": [{"name": "C"}]}`))
	if err != nil {
		t.Fatalf("wrapper parse: %v", err)
	}
	if len(cfgs) != 1 {
		t.Errorf("the thin wrapper returned %d configurations, want 1", len(cfgs))
	}
}

// TestUnresolvableVariablesRefuseRatherThanLaunch is the "wrong program, no
// error" guard.
//
// ${command:...} and ${input:...} are answered by a VS Code prompt this editor
// has no equivalent of. Expanding them to nothing launches SOMETHING — a
// program at a truncated path — and the user debugs a different thing than the
// one they configured, with a session that looks perfectly healthy. The refusal
// has to NAME the variable, or "this configuration cannot run" is unactionable.
func TestUnresolvableVariablesRefuseRatherThanLaunch(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  map[string]interface{}
		want string
	}{
		{
			"command",
			map[string]interface{}{"name": "Pick", "type": "go", "program": "${command:pickProcess}"},
			"${command:pickProcess}",
		},
		{
			"input",
			map[string]interface{}{"name": "Ask", "type": "go", "args": []interface{}{"--env", "${input:envName}"}},
			"${input:envName}",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ResolveLaunchConfig(
				LaunchConfig{Name: tc.cfg["name"].(string), Type: "go", Args: tc.cfg},
				LaunchVarContext{WorkspaceFolder: t.TempDir(), File: "/tmp/x/main.go"})
			if err == nil {
				t.Fatal("an unresolvable variable was launched instead of refused")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal %q does not name %s", err, tc.want)
			}
		})
	}
}

// TestFileVariableWithNoFileOpenRefuses is the same class one step quieter: a
// "Debug Current File" configuration pressed with nothing open expands ${file}
// to "" and launches a program at a path that is not there. delve reports that
// as a build failure in a directory the user has never heard of.
func TestFileVariableWithNoFileOpenRefuses(t *testing.T) {
	_, err := ResolveLaunchConfig(
		LaunchConfig{Name: "Debug Current File", Type: "python",
			Args: map[string]interface{}{"name": "Debug Current File", "type": "python", "program": "${file}"}},
		LaunchVarContext{WorkspaceFolder: t.TempDir()})
	if err == nil {
		t.Fatal("${file} with no file open resolved instead of refusing")
	}
	if !strings.Contains(err.Error(), "${file}") {
		t.Errorf("refusal %q does not name the variable", err)
	}
}

// TestMergeOrderMakesTheConfigAuthoritative pins the four-step merge, including
// the one step that deliberately overrides the user.
func TestMergeOrderMakesTheConfigAuthoritative(t *testing.T) {
	root := t.TempDir()
	cfg := LaunchConfig{
		Name: "Launch Chrome", Type: "chrome", Request: "launch",
		Args: map[string]interface{}{
			"name": "Launch Chrome", "type": "chrome", "request": "launch",
			"url":     "http://localhost:8080",
			"webRoot": "${workspaceFolder}/public",
			// A key the adapter row also sets, so "the user wins" is testable.
			"console": "integratedTerminal",
		},
	}
	spec, err := ResolveLaunchConfig(cfg, LaunchVarContext{WorkspaceFolder: root, File: filepath.Join(root, "index.html")})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// 1. the adapter's defaults are the base.
	// 2. the user's keys overwrite them.
	if got := spec.Args["console"]; got != "integratedTerminal" {
		t.Errorf("console = %v; the adapter's default beat the user's file", got)
	}
	// 3. type is FORCED to the canonical id — `chrome` is an alias the VS Code
	//    extension registers and the standalone server has never heard of.
	if got := spec.Args["type"]; got != "pwa-chrome" {
		t.Errorf("type = %v, want pwa-chrome; the standalone server does not know the alias", got)
	}
	// 4. the workspace folder goes under the adapter's own key.
	if got := spec.Args["__workspaceFolder"]; got != root {
		t.Errorf("__workspaceFolder = %v, want %s — without it js-debug sets webRoot to \"/\" "+
			"and no browser breakpoint ever binds", got, root)
	}
	// 5. program is NOT injected over a config that named a url.
	if _, ok := spec.Args["program"]; ok {
		t.Errorf("program %v was injected into a configuration that named a url", spec.Args["program"])
	}
	// Variables inside the user's own keys are expanded too.
	if got := spec.Args["webRoot"]; got != filepath.Join(root, "public") {
		t.Errorf("webRoot = %v, want the expanded %s", got, filepath.Join(root, "public"))
	}
	if spec.Target != "http://localhost:8080" {
		t.Errorf("target = %q, want the url — a browser session runs no program", spec.Target)
	}
}

// TestProgramIsInjectedOnlyWhenTheConfigNamesNeither covers the other side of
// rule 5, and the ProgramIsDir divergence with it.
func TestProgramIsInjectedOnlyWhenTheConfigNamesNeither(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "pkg", "main.go")

	// Go: the enclosing DIRECTORY, because `mode: debug` builds a package.
	spec, err := ResolveLaunchConfig(
		LaunchConfig{Name: "Go", Type: "go", Args: map[string]interface{}{"name": "Go", "type": "go"}},
		LaunchVarContext{WorkspaceFolder: root, File: file})
	if err != nil {
		t.Fatalf("resolve go: %v", err)
	}
	if got := spec.Args["program"]; got != filepath.Join(root, "pkg") {
		t.Errorf("go program = %v, want the package directory", got)
	}

	// Python: the FILE.
	py := filepath.Join(root, "pkg", "main.py")
	spec, err = ResolveLaunchConfig(
		LaunchConfig{Name: "Py", Type: "python", Args: map[string]interface{}{"name": "Py", "type": "python"}},
		LaunchVarContext{WorkspaceFolder: root, File: py})
	if err != nil {
		t.Fatalf("resolve python: %v", err)
	}
	if got := spec.Args["program"]; got != py {
		t.Errorf("python program = %v, want the file %s", got, py)
	}

	// A config that named its own program keeps it.
	spec, err = ResolveLaunchConfig(
		LaunchConfig{Name: "Explicit", Type: "python",
			Args: map[string]interface{}{"name": "Explicit", "type": "python", "program": "${workspaceFolder}/other.py"}},
		LaunchVarContext{WorkspaceFolder: root, File: py})
	if err != nil {
		t.Fatalf("resolve explicit: %v", err)
	}
	if got := spec.Args["program"]; got != filepath.Join(root, "other.py") {
		t.Errorf("program = %v; the active tab overwrote a program the user chose", got)
	}
}

// TestAttachRequestSurvivesResolution is the guard for the root path having
// called client.Launch unconditionally. A `request: "attach"` configuration
// that is sent as `launch` starts a SECOND copy of the program the user meant
// to attach to.
func TestAttachRequestSurvivesResolution(t *testing.T) {
	spec, err := ResolveLaunchConfig(
		LaunchConfig{Name: "Attach", Type: "go", Request: "attach",
			Args: map[string]interface{}{"name": "Attach", "type": "go", "request": "attach", "mode": "local", "processId": 1234.0}},
		LaunchVarContext{WorkspaceFolder: t.TempDir(), File: filepath.Join(t.TempDir(), "main.go")})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if spec.Request != "attach" {
		t.Fatalf("request = %q, want attach", spec.Request)
	}
	if spec.Args["request"] != "attach" {
		t.Errorf("wire request = %v, want attach", spec.Args["request"])
	}
	// The adapter row's default is request:launch; the config has to beat it.
	if spec.Args["mode"] != "local" {
		t.Errorf("the user's mode was replaced by the adapter default: %v", spec.Args["mode"])
	}
	if _, ok := spec.Args["program"]; ok {
		t.Errorf("attach config picked up an active-file program: %v", spec.Args["program"])
	}
}

// TestUnknownConfigTypeIsNamed pins the message a user with a `type` we cannot
// serve actually reads. "Cannot start" alone gives them nothing to change.
func TestUnknownConfigTypeIsNamed(t *testing.T) {
	_, err := ResolveLaunchConfig(
		LaunchConfig{Name: "Rust thing", Type: "lldb", Args: map[string]interface{}{"name": "Rust thing", "type": "lldb"}},
		LaunchVarContext{WorkspaceFolder: t.TempDir()})
	if err == nil {
		t.Fatal("an unknown type resolved to an adapter")
	}
	for _, want := range []string{"lldb", "Rust thing"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err, want)
		}
	}
}

// TestSpecForFileIsTheOldPathByteForByte is what makes the ONE launch path safe
// to introduce.
//
// 🔴 The language-keyed start path is covered by three LIVE oracles against real
// adapters. Routing it through a LaunchSpec is only sound if the map that
// reaches the wire is unchanged — a key added here would silently invalidate
// every one of those measurements, and the symptom would appear in a live
// session rather than in this package.
func TestSpecForFileIsTheOldPathByteForByte(t *testing.T) {
	for _, lang := range []string{"go", "python", "javascript"} {
		adapter, ok := AdapterFor(lang)
		if !ok {
			t.Fatalf("no adapter for %s", lang)
		}
		// Exactly what runDebugSession built inline before launchselect.go.
		want := make(map[string]interface{}, len(adapter.Launch)+1)
		for k, v := range adapter.Launch {
			want[k] = v
		}
		want["program"] = "/tmp/fixture"

		got := SpecForFile(adapter, "/tmp/fixture")
		if !reflect.DeepEqual(got.Args, want) {
			t.Errorf("%s: wire config drifted from the pre-refactor one\n got: %v\nwant: %v",
				lang, got.Args, want)
		}
		if got.Request != "launch" {
			t.Errorf("%s: request = %q, want launch — the old path called Launch unconditionally",
				lang, got.Request)
		}
		if _, ok := got.Args[adapter.WorkspaceFolderKey]; adapter.WorkspaceFolderKey != "" && ok {
			t.Errorf("%s: the file path gained a %s key the live oracles never measured",
				lang, adapter.WorkspaceFolderKey)
		}
	}
}

// TestExpandLaunchVarsCoversTheOnesVSCodeWrites sweeps the substitutions a real
// launch.json uses, including the ones this editor deliberately leaves alone.
func TestExpandLaunchVarsCoversTheOnesVSCodeWrites(t *testing.T) {
	ctx := LaunchVarContext{
		WorkspaceFolder: filepath.Join("/proj", "site"),
		File:            filepath.Join("/proj", "site", "src", "app.js"),
	}
	t.Setenv("HERDR_LAUNCH_TEST", "from-env")

	for _, tc := range []struct{ in, want string }{
		{"${workspaceFolder}", filepath.Join("/proj", "site")},
		{"${workspaceFolderBasename}", "site"},
		{"${file}", filepath.Join("/proj", "site", "src", "app.js")},
		{"${fileDirname}", filepath.Join("/proj", "site", "src")},
		{"${fileBasename}", "app.js"},
		{"${fileBasenameNoExtension}", "app"},
		{"${fileExtname}", ".js"},
		{"${relativeFile}", filepath.Join("src", "app.js")},
		{"${relativeFileDirname}", "src"},
		{"${env:HERDR_LAUNCH_TEST}", "from-env"},
		{"${workspaceFolder}/dist/**/*.js", filepath.Join("/proj", "site") + "/dist/**/*.js"},
		// Unknown tokens survive: delve and js-debug run their own substitution
		// pass, so eating one they understand is worse than passing it along.
		{"${someFutureThing}", "${someFutureThing}"},
		// An unterminated ${ is not a variable and must not loop forever.
		{"cost is ${100", "cost is ${100"},
		{"no variables here", "no variables here"},
	} {
		if got := ExpandLaunchVars(tc.in, ctx); got != tc.want {
			t.Errorf("ExpandLaunchVars(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestLoadLaunchFileMissingAndMalformed keeps the two file-level behaviours the
// wrappers' tests assert, now at the level that actually reads the file.
func TestLoadLaunchFileMissingAndMalformed(t *testing.T) {
	f, err := LoadLaunchFile(t.TempDir())
	if err != nil {
		t.Fatalf("a missing launch.json reported an error: %v", err)
	}
	if len(f.Configurations) != 0 || len(f.Compounds) != 0 {
		t.Errorf("a project with no launch.json produced %+v", f)
	}

	root := t.TempDir()
	writeLaunchJSON(t, root, `{"configurations": [`)
	if _, err := LoadLaunchFile(root); err == nil {
		t.Fatal("a truncated launch.json parsed without error")
	}
}
