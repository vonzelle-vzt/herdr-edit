// =============================================================================
// File: internal/dap/registry.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package dap

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudmanic/spice-edit/internal/toolpath"
)

// Adapter describes how to launch one debug adapter and what to ask it for.
type Adapter struct {
	// Name is what the status line calls it.
	Name string

	// AdapterID is the identifier sent in initialize. Adapters use it to
	// decide which language's launch configuration shape to expect.
	AdapterID string

	// Argv holds candidate command lines, tried in order — the same
	// several-names-per-tool problem internal/lsp/registry.go solves the same
	// way. SocketPlaceholder inside any argument is replaced with the real
	// socket path at spawn time, which is what keeps the "how does this
	// adapter dial back to us" detail in data rather than in client.go.
	//
	// Ignored when Locate is set: an adapter that needs real logic to find
	// itself cannot be expressed as a list of names.
	Argv [][]string

	// Locate resolves this adapter when a list of candidate binaries cannot.
	//
	// 🔴 This field is what makes DefaultAdapters an abstraction rather than a
	// table with one row in it. Delve is a binary on PATH and needs nothing
	// here; debugpy is a PYTHON LIBRARY that may live in the project's own
	// virtualenv, inside a VS Code extension, or in the system interpreter's
	// site-packages, and which interpreter runs it decides which one you get.
	// Squeezing that into Argv would have meant a special case in Resolve —
	// i.e. exactly the "table with one row plus an if" this stage exists to
	// disprove.
	//
	// root is the project directory. Returning nil means "not installed".
	Locate func(root string) *Command

	// Transport is how the protocol stream is reached. Delve is socket-only;
	// debugpy is stdio. See dap.Transport.
	Transport Transport

	// Languages this adapter debugs, as internal/lsp LanguageID values.
	Languages []string

	// ProgramIsDir says the `program` launch key names a DIRECTORY rather than
	// the file the user pressed F5 on.
	//
	// 🔴 Not cosmetic, and it is the first place a second adapter diverges from
	// the first. Delve's "debug" mode builds a PACKAGE, so program must be the
	// enclosing directory; debugpy runs a SCRIPT, so program must be the file
	// itself. Handing delve a file makes it refuse the launch outright, which is
	// loud — handing debugpy a directory makes it try to run the directory, which
	// is a different and much quieter kind of wrong.
	ProgramIsDir bool

	// Launch is the base launch configuration, merged with the program path at
	// start time. A user's .vscode/launch.json overrides it — see launchjson.go.
	Launch map[string]interface{}

	// InstallHint tells the user how to make this adapter exist, for the case
	// where naming the missing binary does not (debugpy is a library, not a
	// command, so "debugpy not found on PATH" would be actively misleading).
	InstallHint string
}

// DefaultAdapters is the built-in table: delve for Go, debugpy for Python.
//
// The second entry is here to keep the first one honest. Everything that turned
// out to be delve-shaped rather than DAP-shaped — the socket transport, the
// package-directory `program`, the launch response arriving early enough to get
// away with awaiting it — is a place this fork would have shipped an
// abstraction with exactly one implementation and no way to know.
var DefaultAdapters = []Adapter{
	{
		Name:      "delve",
		AdapterID: "go",
		// 🔴 `dlv dap` has NO stdio mode — it is a socket server. --client-addr
		// makes IT dial US, which is the direction with no readiness race: our
		// listener exists before the process does. See StartCommand.
		Argv:         [][]string{{"dlv", "dap", "--client-addr", "unix:" + SocketPlaceholder}},
		Transport:    TransportSocket,
		Languages:    []string{"go"},
		ProgramIsDir: true, // `mode: debug` builds a package, not a file
		Launch: map[string]interface{}{
			"request": "launch",
			// "debug" builds the package and runs it, which is what F5 on a
			// source file means. "exec" would require a prebuilt binary.
			"mode": "debug",

			// 🔴 outputMode:remote is what makes the debugged program's stdout
			// reach the user at all, and it is not the default.
			//
			// Without it delve hands the debuggee the ADAPTER's own stdio. We
			// point that at io.Discard so a debugged program cannot scribble
			// over the tcell screen — which means that without this key the
			// program's output is silently thrown away instead. Measured: a
			// fixture printing "5" produced no output event whatsoever until
			// this was set, and then arrived as
			// {"category":"stdout","output":"5\n"}.
			//
			// So the two halves are one decision: discard the inherited stdio
			// AND ask the adapter to forward it as events.
			"outputMode": "remote",
		},
	},
	{
		Name:      "debugpy",
		AdapterID: "python",
		Locate:    LocateDebugpy,
		// 🔴 stdio, not a socket. `python -m debugpy.adapter` only listens on a
		// port when given --port; with no arguments it speaks the protocol over
		// its own stdin/stdout (verified against debugpy 1.8.20's --help and a
		// full live session).
		Transport:    TransportStdio,
		Languages:    []string{"python"},
		ProgramIsDir: false, // debugpy runs the FILE
		Launch: map[string]interface{}{
			"request": "launch",
			"type":    "python",

			// 🔴 The sibling of delve's outputMode:remote, and load-bearing for
			// the same reason twice over on this transport. internalConsole
			// makes debugpy capture the debuggee's stdout and forward it as
			// `output` events. Any other console mode hands the program a real
			// terminal — which on stdio is OUR PROTOCOL STREAM, so a single
			// print would frame as garbage and desynchronise the session
			// permanently rather than merely scribbling on the screen.
			"console": "internalConsole",

			// The user asked to debug THEIR file; justMyCode:true would refuse
			// to stop in anything the packaging heuristics call library code,
			// which for a file run out of a temp directory is a coin toss.
			"justMyCode": false,
		},
		InstallHint: "run `pip install debugpy` in the project's virtualenv, " +
			"or install the MIT-licensed ms-python.debugpy VS Code extension",
	},
}

// Registry resolves and starts adapters for a project.
//
// It is deliberately NOT a copy of lsp.Manager. That type keeps one long-lived
// client per language and never retries a failure; a debug session is the
// opposite on both counts — it is per-run, and retrying is the entire workflow.
type Registry struct {
	root string

	mu sync.Mutex
	// missing records adapters whose binary is not installed. Sticky on
	// purpose: the answer will not change while the editor is running, and
	// re-running LookPath on every keypress is waste.
	//
	// 🔴 Nothing else is ever recorded here, and that is the point. lsp.Manager
	// has a single `failed` map covering both "not installed" and "did not
	// start", and copying it wholesale would be a real bug in this package:
	// the overwhelmingly common DAP failure is "the program did not compile",
	// which the user fixes in thirty seconds — and a sticky flag would make F5
	// dead for the rest of the session with nothing on screen to say why.
	// Launch failures are not recorded at all; every F5 tries again.
	missing map[string]bool
}

// NewRegistry returns a registry rooted at a project directory.
func NewRegistry(root string) *Registry {
	return &Registry{root: root, missing: make(map[string]bool)}
}

// AdapterFor finds the adapter handling a language id.
func AdapterFor(lang string) (Adapter, bool) {
	for _, a := range DefaultAdapters {
		for _, l := range a.Languages {
			if l == lang {
				return a, true
			}
		}
	}
	return Adapter{}, false
}

// Resolve turns an adapter into something executable, or nil when it is not
// installed.
//
// An adapter with a Locate hook answers for itself; everything else goes down
// the candidate-argv path, which mirrors lsp.Manager.resolve — node_modules/.bin
// first (a repo-pinned adapter matching the repo beats whatever is global), then
// PATH, then the directories developer tools actually install into.
func (r *Registry) Resolve(a Adapter) *Command {
	if a.Locate != nil {
		cmd := a.Locate(r.root)
		if cmd == nil {
			return nil
		}
		cmd.Transport = a.Transport
		return cmd
	}
	for _, argv := range a.Argv {
		if len(argv) == 0 {
			continue
		}
		local := filepath.Join(r.root, "node_modules", ".bin", argv[0])
		if fi, err := os.Stat(local); err == nil && !fi.IsDir() {
			return &Command{
				Argv:      append([]string{local}, argv[1:]...),
				Transport: a.Transport,
				Origin:    "node_modules/.bin",
			}
		}
		if p, err := exec.LookPath(argv[0]); err == nil {
			return &Command{
				Argv:      append([]string{p}, argv[1:]...),
				Transport: a.Transport,
				Origin:    "PATH",
			}
		}
		if p := toolpath.Look(argv[0]); p != "" {
			return &Command{
				Argv:      append([]string{p}, argv[1:]...),
				Transport: a.Transport,
				Origin:    filepath.Dir(p),
			}
		}
	}
	return nil
}

// findExecutable locates one binary by name on PATH, falling back to the tool
// directories. Returns "" when it is nowhere.
func findExecutable(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return toolpath.Look(name)
}

// Available reports which configured adapters are actually installed, so "F5
// does nothing" has a visible answer rather than being experienced.
func (r *Registry) Available() []string {
	var out []string
	for _, a := range DefaultAdapters {
		if r.Resolve(a) != nil {
			out = append(out, a.Name)
		}
	}
	return out
}

// Missing reports whether an adapter has already been found to be uninstalled.
func (r *Registry) Missing(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.missing[name]
}

// Start spawns an adapter for a language.
//
// A missing binary is recorded and never retried. Anything after that — the
// adapter starting but refusing to launch, the program failing to build — is
// NOT recorded, so the next F5 tries again. See Registry.missing.
func (r *Registry) Start(ctx context.Context, a Adapter, h Handlers) (*Client, error) {
	r.mu.Lock()
	if r.missing[a.Name] {
		r.mu.Unlock()
		return nil, &NotInstalledError{Name: a.Name, Command: firstCommand(a), Hint: a.InstallHint}
	}
	r.mu.Unlock()

	cmd := r.Resolve(a)
	if cmd == nil {
		r.mu.Lock()
		r.missing[a.Name] = true
		r.mu.Unlock()
		return nil, &NotInstalledError{Name: a.Name, Command: firstCommand(a), Hint: a.InstallHint}
	}
	return StartCommand(ctx, a.Name, *cmd, r.root, h)
}

// NotInstalledError is the one failure worth distinguishing by type: it is the
// only one the user fixes by installing something rather than by fixing their
// code, and it is the only one that is sticky.
type NotInstalledError struct {
	Name    string
	Command string
	Hint    string
}

// Error renders the message shown on the status line.
func (e *NotInstalledError) Error() string {
	msg := e.Name + " is not installed"
	if e.Command != "" {
		msg += " (" + e.Command + " not found on PATH)"
	}
	if e.Hint != "" {
		msg += " — " + e.Hint
	}
	return msg
}

// firstCommand names the binary an adapter's first candidate argv would run,
// for the "not installed" message. Empty for an adapter that resolves itself
// through Locate, where there is no single binary to name.
func firstCommand(a Adapter) string {
	for _, argv := range a.Argv {
		if len(argv) > 0 {
			return argv[0]
		}
	}
	return ""
}

// -----------------------------------------------------------------------------
// debugpy resolution
// -----------------------------------------------------------------------------

// debugpyProbeTimeout bounds the one subprocess this file runs. An interpreter
// that cannot answer `import debugpy` in five seconds is not one we want to run
// a debug session through.
const debugpyProbeTimeout = 5 * time.Second

// debugpyImportProbe memoises, per interpreter path, whether `import debugpy`
// succeeds. Resolve is called from Available() as well as from Start(), and
// spawning a Python once per status refresh would be a real cost for an answer
// that cannot change while the editor runs.
var debugpyImportProbe sync.Map // string -> bool

// LocateDebugpy finds a debugpy adapter for a project, in the order that makes
// the project's own answer win.
//
//  1. an interpreter belonging to the PROJECT (.venv, venv, $VIRTUAL_ENV) that
//     can import debugpy — the same reasoning as lsp's node_modules/.bin rule,
//     and stronger here: a debugger running under a different interpreter from
//     the code debugs a different set of installed packages;
//  2. the MIT-licensed copy bundled inside a VS Code ms-python.debugpy
//     extension, made importable through PYTHONPATH;
//  3. a system python3 with debugpy installed.
//
// Returns nil when none of the three answers.
func LocateDebugpy(root string) *Command {
	if py := projectPython(root); py != "" && pythonCanImportDebugpy(py) {
		return &Command{Argv: []string{py, "-m", "debugpy.adapter"}, Origin: "venv " + py}
	}

	if libs := findDebugpyInVSCodeExtensions(vscodeExtensionRoots()); libs != "" {
		// The bundled copy is a library directory, so it needs an interpreter of
		// its own. Prefer the project's, so the debuggee still runs under the
		// python the project expects even though the adapter came from elsewhere.
		py := projectPython(root)
		if py == "" {
			py = systemPython()
		}
		if py != "" {
			return &Command{
				Argv:   []string{py, "-m", "debugpy.adapter"},
				Env:    []string{"PYTHONPATH=" + libs},
				Origin: "vscode extension " + libs,
			}
		}
	}

	if py := systemPython(); py != "" && pythonCanImportDebugpy(py) {
		return &Command{Argv: []string{py, "-m", "debugpy.adapter"}, Origin: "system " + py}
	}
	return nil
}

// projectPython returns the project's own interpreter, or "" when it has none.
//
// $VIRTUAL_ENV is consulted last rather than first on purpose: it describes the
// shell the EDITOR was launched from, which for a long-lived editor opened
// against several projects is not necessarily the shell that matches the file on
// screen. A .venv sitting in the project root is unambiguous.
func projectPython(root string) string {
	var candidates []string
	for _, name := range []string{".venv", "venv", ".virtualenv"} {
		candidates = append(candidates,
			filepath.Join(root, name, "bin", "python"),
			filepath.Join(root, name, "bin", "python3"),
		)
	}
	if env := os.Getenv("VIRTUAL_ENV"); env != "" {
		candidates = append(candidates,
			filepath.Join(env, "bin", "python"),
			filepath.Join(env, "bin", "python3"),
		)
	}
	for _, p := range candidates {
		if isExecutableFile(p) {
			return p
		}
	}
	return ""
}

// systemPython finds an interpreter on PATH or in the tool directories.
func systemPython() string {
	for _, name := range []string{"python3", "python"} {
		if p := findExecutable(name); p != "" {
			return p
		}
	}
	return ""
}

// isExecutableFile reports whether p is a regular file we could exec.
func isExecutableFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0
}

// pythonCanImportDebugpy reports whether an interpreter has debugpy importable,
// memoised per interpreter.
//
// 🔴 It runs the interpreter rather than looking for a site-packages directory,
// because "importable" is what actually decides whether the adapter starts, and
// the two disagree constantly: a .pth file, a PYTHONPATH already in the
// environment, a namespace package or a system install shadowed by a user one
// all make the directory check answer a question nobody asked. The cost is one
// subprocess per interpreter per editor session.
func pythonCanImportDebugpy(python string) bool {
	if v, ok := debugpyImportProbe.Load(python); ok {
		return v.(bool)
	}
	ctx, cancel := context.WithTimeout(context.Background(), debugpyProbeTimeout)
	defer cancel()
	ok := exec.CommandContext(ctx, python, "-c", "import debugpy").Run() == nil
	debugpyImportProbe.Store(python, ok)
	return ok
}

// vscodeExtensionRoots lists the directories VS Code and its close relatives
// unpack extensions into.
func vscodeExtensionRoots() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{
		filepath.Join(home, ".vscode", "extensions"),
		filepath.Join(home, ".vscode-insiders", "extensions"),
		filepath.Join(home, ".vscode-server", "extensions"),
	}
}

// osiLicenses is the ALLOW-list of SPDX identifiers whose code this editor will
// run out of somebody else's extension directory.
//
// 🔴 An allow-list, never a deny-list, and the difference is the whole guard.
// This repo is MIT; a VS Code extension directory is full of things that are
// not. ms-python.vscode-pylance in particular is licensed only for use with
// Microsoft products — resolving an adapter out of it would put a licence
// violation into a binary we ship. A deny-list would have to enumerate every
// such extension in advance, and the failure mode of missing one is silent.
var osiLicenses = map[string]bool{
	"MIT":          true,
	"Apache-2.0":   true,
	"BSD-2-Clause": true,
	"BSD-3-Clause": true,
	"ISC":          true,
	"0BSD":         true,
}

// vscodeExtensionLicense reads an extension's declared SPDX licence, or "" when
// package.json has no readable `license` string.
func vscodeExtensionLicense(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return ""
	}
	var pkg struct {
		License string `json:"license"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return ""
	}
	return strings.TrimSpace(pkg.License)
}

// vscodeExtensionIsOSILicensed reports whether we may run code out of dir.
//
// 🔴 A MISSING field and "SEE LICENSE IN LICENSE.txt" are both DENY, not
// unknown-so-probably-fine. The second is exactly how a restrictive licence is
// spelled in this ecosystem — it is what Pylance ships — so treating an
// unrecognised value as permissive would defeat the check in precisely the case
// it exists for. There is no fallback to reading LICENSE.txt: a licence this
// code cannot name is a licence a human has to look at.
func vscodeExtensionIsOSILicensed(dir string) bool {
	return osiLicenses[vscodeExtensionLicense(dir)]
}

// findDebugpyInVSCodeExtensions returns the `bundled/libs` directory of an
// installed, OSI-licensed ms-python.debugpy extension, or "" when there is none.
//
// Only ms-python.debugpy is ever considered — the search is by directory prefix
// rather than by scanning every extension for something importable — and the
// licence is then checked on top. Both gates matter: the name keeps us out of
// unrelated extensions, and the licence keeps us out of a differently-licensed
// future release of this one.
func findDebugpyInVSCodeExtensions(roots []string) string {
	found := ""
	for _, root := range roots {
		matches, err := filepath.Glob(filepath.Join(root, "ms-python.debugpy-*"))
		if err != nil {
			continue
		}
		sort.Strings(matches) // newest version last, so it wins
		for _, dir := range matches {
			if !vscodeExtensionIsOSILicensed(dir) {
				continue
			}
			libs := filepath.Join(dir, "bundled", "libs")
			if fi, err := os.Stat(filepath.Join(libs, "debugpy")); err != nil || !fi.IsDir() {
				continue
			}
			found = libs
		}
	}
	return found
}

// Describe summarises registry state in one line for the status bar.
func (r *Registry) Describe() string {
	if avail := r.Available(); len(avail) > 0 {
		return "dap: " + strings.Join(avail, ", ") + " installed"
	}
	return "dap: no debug adapters installed"
}
