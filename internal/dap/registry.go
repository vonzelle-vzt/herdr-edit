// =============================================================================
// File: internal/dap/registry.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package dap

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
	Argv [][]string

	// Languages this adapter debugs, as internal/lsp LanguageID values.
	Languages []string

	// Launch is the base launch configuration, merged with the program path at
	// start time. A user's .vscode/launch.json overrides it — see launchjson.go.
	Launch map[string]interface{}
}

// DefaultAdapters is the built-in table. Delve only: this fork's users write Go,
// a second adapter is one table entry away, and an adapter that is not installed
// simply never starts.
var DefaultAdapters = []Adapter{
	{
		Name:      "delve",
		AdapterID: "go",
		// 🔴 `dlv dap` has NO stdio mode — it is a socket server. --client-addr
		// makes IT dial US, which is the direction with no readiness race: our
		// listener exists before the process does. See StartCommand.
		Argv:      [][]string{{"dlv", "dap", "--client-addr", "unix:" + SocketPlaceholder}},
		Languages: []string{"go"},
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

// Resolve finds the first candidate argv whose binary actually exists,
// returning nil when none does.
//
// Mirrors lsp.Manager.resolve, including the node_modules/.bin lookup first:
// a JS project's debug adapter is pinned in the repo the same way its language
// server is, and a repo-local version matching the repo is likelier to be right
// than whatever is installed globally.
func (r *Registry) Resolve(a Adapter) []string {
	for _, argv := range a.Argv {
		if len(argv) == 0 {
			continue
		}
		local := filepath.Join(r.root, "node_modules", ".bin", argv[0])
		if fi, err := os.Stat(local); err == nil && !fi.IsDir() {
			return append([]string{local}, argv[1:]...)
		}
		if p, err := exec.LookPath(argv[0]); err == nil {
			return append([]string{p}, argv[1:]...)
		}
	}
	return nil
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
		return nil, &NotInstalledError{Name: a.Name, Command: firstCommand(a)}
	}
	r.mu.Unlock()

	argv := r.Resolve(a)
	if argv == nil {
		r.mu.Lock()
		r.missing[a.Name] = true
		r.mu.Unlock()
		return nil, &NotInstalledError{Name: a.Name, Command: firstCommand(a)}
	}
	return StartCommand(ctx, a.Name, argv, r.root, h)
}

// NotInstalledError is the one failure worth distinguishing by type: it is the
// only one the user fixes by installing something rather than by fixing their
// code, and it is the only one that is sticky.
type NotInstalledError struct {
	Name    string
	Command string
}

// Error renders the message shown on the status line.
func (e *NotInstalledError) Error() string {
	if e.Command == "" {
		return e.Name + " is not installed"
	}
	return e.Name + " is not installed (" + e.Command + " not found on PATH)"
}

// firstCommand names the binary an adapter's first candidate argv would run,
// for the "not installed" message.
func firstCommand(a Adapter) string {
	for _, argv := range a.Argv {
		if len(argv) > 0 {
			return argv[0]
		}
	}
	return ""
}

// Describe summarises registry state in one line for the status bar.
func (r *Registry) Describe() string {
	if avail := r.Available(); len(avail) > 0 {
		return "dap: " + strings.Join(avail, ", ") + " installed"
	}
	return "dap: no debug adapters installed"
}
