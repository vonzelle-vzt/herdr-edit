// =============================================================================
// File: internal/lsp/registry.go
// Author: Spicer Matthews <spicer@cloudmanic.com>
// Created: 2026-07-29
// Copyright: 2026 Cloudmanic, LLC. All rights reserved.
// =============================================================================

package lsp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Manager owns one server per language per project and routes documents to the
// right one.
//
// Servers are started LAZILY, on the first file that needs them. Starting every
// configured server at boot would mean paying for a Python server in a
// TypeScript repo, and a language server is not a cheap process — tsserver
// alone can take a second and a few hundred megabytes. It also means a repo
// with no matching server never spawns anything at all.
type Manager struct {
	root string

	mu      sync.Mutex
	clients map[string]*Client // language id -> client
	failed  map[string]bool    // do not retry a server that already failed to start
	docs    map[string]string  // uri -> language id, so DidClose reaches the right server

	// OnDiagnostics and OnLog are forwarded from every managed client. Called
	// from background goroutines: the app must marshal to its event loop.
	OnDiagnostics func(uri string, diags []Diagnostic)
	OnLog         func(string)
}

// Server describes how to launch one language server.
type Server struct {
	Name string
	// Argv candidates, tried in order. Several servers ship under more than one
	// name across install methods (vtsls vs typescript-language-server), and the
	// alternative to trying both is asking every user to configure it.
	Argv [][]string
	// Languages this server handles, as LanguageID values.
	Languages []string
}

// DefaultServers is the built-in table. It intentionally covers the languages
// this editor's users actually write and nothing more; anything missing is one
// entry away, and a server that is not installed simply never starts.
var DefaultServers = []Server{
	{
		Name: "typescript",
		Argv: [][]string{
			{"vtsls", "--stdio"},
			{"typescript-language-server", "--stdio"},
		},
		Languages: []string{"typescript", "typescriptreact", "javascript", "javascriptreact"},
	},
	{
		Name:      "python",
		Argv:      [][]string{{"basedpyright-langserver", "--stdio"}, {"pyright-langserver", "--stdio"}},
		Languages: []string{"python"},
	},
	{
		Name:      "gopls",
		Argv:      [][]string{{"gopls"}},
		Languages: []string{"go"},
	},
	{
		Name:      "rust-analyzer",
		Argv:      [][]string{{"rust-analyzer"}},
		Languages: []string{"rust"},
	},
	{
		Name:      "tailwindcss",
		Argv:      [][]string{{"tailwindcss-language-server", "--stdio"}},
		Languages: []string{"css", "scss", "html"},
	},
	{
		Name:      "json",
		Argv:      [][]string{{"vscode-json-language-server", "--stdio"}},
		Languages: []string{"json", "jsonc"},
	},
	{
		Name:      "yaml",
		Argv:      [][]string{{"yaml-language-server", "--stdio"}},
		Languages: []string{"yaml"},
	},
	{
		Name:      "bash",
		Argv:      [][]string{{"bash-language-server", "start"}},
		Languages: []string{"shellscript"},
	},
	{
		Name:      "clangd",
		Argv:      [][]string{{"clangd"}},
		Languages: []string{"c", "cpp"},
	},
}

// NewManager returns a manager rooted at a project directory.
func NewManager(root string) *Manager {
	return &Manager{
		root:    root,
		clients: make(map[string]*Client),
		failed:  make(map[string]bool),
		docs:    make(map[string]string),
	}
}

// serverFor finds the table entry handling a language.
func serverFor(lang string) (Server, bool) {
	for _, s := range DefaultServers {
		for _, l := range s.Languages {
			if l == lang {
				return s, true
			}
		}
	}
	return Server{}, false
}

// resolve finds the first candidate argv whose binary actually exists.
//
// Also searches the project's own node_modules/.bin, because that is where a JS
// project's pinned server lives — and a repo-local version matching the repo's
// TypeScript is more likely to be right than whatever is installed globally.
func (m *Manager) resolve(s Server) []string {
	for _, argv := range s.Argv {
		if len(argv) == 0 {
			continue
		}
		local := filepath.Join(m.root, "node_modules", ".bin", argv[0])
		if fi, err := os.Stat(local); err == nil && !fi.IsDir() {
			return append([]string{local}, argv[1:]...)
		}
		if p, err := exec.LookPath(argv[0]); err == nil {
			return append([]string{p}, argv[1:]...)
		}
	}
	return nil
}

// Available reports which configured servers are actually installed. Used by
// the status line and by doctor-style output, so "no diagnostics" can be
// explained rather than just experienced.
func (m *Manager) Available() []string {
	var out []string
	for _, s := range DefaultServers {
		if m.resolve(s) != nil {
			out = append(out, s.Name)
		}
	}
	return out
}

// clientFor returns the running server for a language, starting it if needed.
//
// A server that fails to start is recorded and never retried for the life of
// the editor: retrying on every keystroke against a missing binary would spawn
// processes in a loop, and the answer will not change.
func (m *Manager) clientFor(ctx context.Context, lang string) *Client {
	s, ok := serverFor(lang)
	if !ok {
		return nil
	}

	m.mu.Lock()
	if c, ok := m.clients[s.Name]; ok {
		m.mu.Unlock()
		return c
	}
	if m.failed[s.Name] {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	argv := m.resolve(s)
	if argv == nil {
		m.mu.Lock()
		m.failed[s.Name] = true // not installed; silent, this is the normal case
		m.mu.Unlock()
		return nil
	}

	c, err := Start(ctx, s.Name, argv, URI(m.root))
	if err != nil {
		m.mu.Lock()
		m.failed[s.Name] = true
		m.mu.Unlock()
		if m.OnLog != nil {
			m.OnLog(err.Error())
		}
		return nil
	}
	c.OnDiagnostics = m.OnDiagnostics
	c.OnLog = m.OnLog

	m.mu.Lock()
	// Another goroutine may have won the race while we were starting; keep the
	// winner and shut ours down rather than leaking a second server.
	if existing, ok := m.clients[s.Name]; ok {
		m.mu.Unlock()
		c.Stop()
		return existing
	}
	m.clients[s.Name] = c
	m.mu.Unlock()
	return c
}

// DidOpen routes a newly opened document to its server, starting one if this is
// the first file of that language.
func (m *Manager) DidOpen(ctx context.Context, path, text string) {
	lang := LanguageID(path)
	c := m.clientFor(ctx, lang)
	if c == nil {
		return
	}
	uri := URI(path)
	m.mu.Lock()
	m.docs[uri] = lang
	m.mu.Unlock()
	_ = c.DidOpen(uri, lang, text)
}

// DidChange forwards an edit.
func (m *Manager) DidChange(path, text string) {
	if c := m.existing(path); c != nil {
		_ = c.DidChange(URI(path), text)
	}
}

// DidSave forwards a write, which is when lint-style servers do their work.
func (m *Manager) DidSave(path, text string) {
	if c := m.existing(path); c != nil {
		_ = c.DidSave(URI(path), text)
	}
}

// DidClose stops tracking a document.
func (m *Manager) DidClose(path string) {
	uri := URI(path)
	if c := m.existing(path); c != nil {
		_ = c.DidClose(uri)
	}
	m.mu.Lock()
	delete(m.docs, uri)
	m.mu.Unlock()
}

// existing returns the already-running server for a path, without starting one.
// Used by the edit path so typing in a file whose server never started does not
// repeatedly attempt to spawn it.
func (m *Manager) existing(path string) *Client {
	s, ok := serverFor(LanguageID(path))
	if !ok {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.clients[s.Name]
}

// Hover asks the server covering path what is at pos.
func (m *Manager) Hover(ctx context.Context, path string, pos Position) (string, error) {
	c := m.existing(path)
	if c == nil {
		return "", nil
	}
	return c.Hover(ctx, URI(path), pos)
}

// Definition resolves go-to-definition for path at pos.
func (m *Manager) Definition(ctx context.Context, path string, pos Position) ([]Location, error) {
	c := m.existing(path)
	if c == nil {
		return nil, nil
	}
	return c.Definition(ctx, URI(path), pos)
}

// Stop shuts every server down. Called on quit; best-effort throughout, since
// a wedged server must never keep the editor from exiting.
// Completion asks the server owning path for completions at pos.
func (m *Manager) Completion(ctx context.Context, path string, pos Position) ([]CompletionItem, error) {
	c := m.existing(path)
	if c == nil {
		return nil, nil
	}
	return c.Completion(ctx, URI(path), pos)
}

// References proxies textDocument/references for path.
func (m *Manager) References(ctx context.Context, path string, pos Position) ([]Location, error) {
	c := m.existing(path)
	if c == nil {
		return nil, nil
	}
	return c.References(ctx, URI(path), pos)
}

// Rename proxies textDocument/rename for path.
func (m *Manager) Rename(ctx context.Context, path string, pos Position, newName string) (map[string][]TextEdit, error) {
	c := m.existing(path)
	if c == nil {
		return nil, nil
	}
	return c.Rename(ctx, URI(path), pos, newName)
}

// DocumentSymbol proxies textDocument/documentSymbol.
func (m *Manager) DocumentSymbol(ctx context.Context, path string) ([]Symbol, error) {
	c := m.existing(path)
	if c == nil {
		return nil, nil
	}
	return c.DocumentSymbol(ctx, URI(path))
}

// SignatureHelp proxies textDocument/signatureHelp.
func (m *Manager) SignatureHelp(ctx context.Context, path string, pos Position) (string, error) {
	c := m.existing(path)
	if c == nil {
		return "", nil
	}
	return c.SignatureHelp(ctx, URI(path), pos)
}

// WorkspaceSymbol searches every RUNNING server for query and merges their
// answers.
//
// 🔴 This is the one Manager method that cannot use clientFor/existing: every
// other request resolves ONE client from a file path, and a workspace search
// has no path to resolve from. Instead it snapshots the clients map (same
// lock-then-copy discipline as Stop, below) and fans the query out to each
// one, because a project can easily have a Go server and a TypeScript server
// both wanting to answer.
//
// Servers that have not started yet (lazy start — see the package doc) are
// simply not in the map and are not queried; this method never starts one.
// Starting a server just to satisfy a symbol search would turn an instant
// picker into a multi-second wait the first time it's used, and the caller
// (menuWorkspaceSymbol) already checks Running() before prompting and tells
// the user to open a file first when nothing is up yet.
func (m *Manager) WorkspaceSymbol(ctx context.Context, query string) ([]Symbol, error) {
	m.mu.Lock()
	clients := make([]*Client, 0, len(m.clients))
	for _, c := range m.clients {
		clients = append(clients, c)
	}
	m.mu.Unlock()

	var out []Symbol
	var firstErr error
	for _, c := range clients {
		syms, err := c.WorkspaceSymbol(ctx, query)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		out = append(out, syms...)
	}
	// Only surface an error when EVERY server failed and there is nothing to
	// show; a partial answer from one server while another timed out is still
	// useful and should not be thrown away.
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// CodeAction proxies textDocument/codeAction.
func (m *Manager) CodeAction(ctx context.Context, path string, rng Range, diags []Diagnostic) ([]CodeAction, error) {
	c := m.existing(path)
	if c == nil {
		return nil, nil
	}
	return c.CodeAction(ctx, URI(path), rng, diags)
}

func (m *Manager) Stop() {
	m.mu.Lock()
	clients := make([]*Client, 0, len(m.clients))
	for _, c := range m.clients {
		clients = append(clients, c)
	}
	m.clients = make(map[string]*Client)
	m.mu.Unlock()

	var wg sync.WaitGroup
	for _, c := range clients {
		wg.Add(1)
		go func(c *Client) { defer wg.Done(); c.Stop() }(c)
	}
	wg.Wait()
}

// Running lists the servers currently up, for the status line.
func (m *Manager) Running() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.clients))
	for name := range m.clients {
		out = append(out, name)
	}
	return out
}

// Describe summarises manager state in one line for the status bar, so "why are
// there no squiggles" has a visible answer.
func (m *Manager) Describe() string {
	running := m.Running()
	if len(running) > 0 {
		return "lsp: " + strings.Join(running, ", ")
	}
	if avail := m.Available(); len(avail) > 0 {
		return "lsp: none running (" + strings.Join(avail, ", ") + " installed)"
	}
	return "lsp: no language servers installed"
}
