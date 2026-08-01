// =============================================================================
// File: internal/app/launchpicker.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// launchpicker.go is the CALL SITE internal/dap's launch.json reader never had.
//
// 🔴 That reader was complete, careful and unit-tested, and this grep returned
// nothing but its own definitions:
//
//	grep -rn 'LoadLaunchConfigs\|ParseLaunchJSON' --include='*.go' . | grep -v _test.go
//
// which is the fork-wide pattern CLAUDE.md records happening three times before:
// a green test suite proves the engine works, not that anyone can reach it. This
// file is what makes the grep return a real caller.
//
// # What it does
//
// menuStartDebug is a ROUTER now, not a launcher. It decides which of two things
// F5 means and hands both to the same start function:
//
//	launch.json fails to parse  -> flash and STOP
//	no configurations           -> the language-keyed path, unchanged
//	a remembered choice         -> that configuration
//	exactly one configuration   -> that one
//	otherwise                   -> ask
//
// 🔴 The FIRST branch is the one worth arguing about. A malformed launch.json
// must NOT fall through to the language path: a user who typed a stray comma
// would watch F5 debug something — just not the thing they configured — and
// conclude the editor ignores their file. Stopping is the only honest answer,
// and internal/dap already distinguishes "absent" from "broken" for exactly
// this.
//
// # The remembered choice is in MEMORY and it is VISIBLE
//
// A sticky choice with no indicator is a hidden mode. Next week's F5 would run a
// different program from the file on screen, with nothing anywhere to explain
// it — the same shape as the stale flash this stage also fixes. So the menu row
// says which configuration it will run, `Choose debug configuration…` always
// reopens the picker, and the choice is dropped the moment launch.json changes
// on disk.
package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudmanic/spice-edit/internal/dap"
	"github.com/cloudmanic/spice-edit/internal/lsp"
)

// launchState caches a project's launch.json and remembers which configuration
// the user picked out of it.
//
// The cache exists because `enabled` predicates run from menuLayout, which the
// command palette rebuilds on every keystroke — re-reading and re-parsing the
// file that often would be real work for an answer that almost never changes.
// A stat is cheap; a read only happens when the stat says something moved.
type launchState struct {
	file   dap.LaunchFile
	err    error
	loaded bool

	// mtime and size together decide staleness. mtime alone is not enough:
	// two edits inside one filesystem timestamp tick are indistinguishable,
	// and a launch.json is exactly the sort of small file an editor rewrites
	// twice in a second.
	mtime time.Time
	size  int64

	// choice is the remembered configuration NAME, never an index.
	//
	// 🔴 An index would survive an edit that reorders the array and then run a
	// different configuration under the label the user chose — the worst
	// available outcome, because everything on screen would still look right.
	choice string
}

// launchFile returns the project's parsed launch.json, re-reading only when the
// file on disk has actually moved.
//
// 🔴 The remembered choice is cleared HERE, inside the comparison that detects
// the change, rather than by a separate poll somewhere else. CLAUDE.md's own
// lesson from debugRequestFloor: the guard has to live where the comparison is,
// because a guard a caller is supposed to remember is a guard the next caller
// forgets.
func (a *App) launchFile() (dap.LaunchFile, error) {
	path := dap.LaunchJSONFile(a.rootDir)
	fi, err := os.Stat(path)
	if err != nil {
		// No launch.json at all: forget everything, including a choice left
		// over from before the file was deleted.
		a.launch = launchState{loaded: true}
		return dap.LaunchFile{}, nil
	}
	if a.launch.loaded && fi.ModTime().Equal(a.launch.mtime) && fi.Size() == a.launch.size {
		return a.launch.file, a.launch.err
	}

	file, ferr := dap.LoadLaunchFile(a.rootDir)
	a.launch = launchState{
		file:   file,
		err:    ferr,
		loaded: true,
		mtime:  fi.ModTime(),
		size:   fi.Size(),
		// The choice does NOT survive: the configuration the user picked may
		// have been renamed, retargeted or deleted by the edit we just noticed.
		choice: "",
	}
	return file, ferr
}

// launchVarContext is what ${...} resolves against right now: the project root
// and whatever file is on screen.
func (a *App) launchVarContext() dap.LaunchVarContext {
	ctx := dap.LaunchVarContext{WorkspaceFolder: a.rootDir}
	if t := a.activeTabPtr(); t != nil && t.Path != "" && !t.Synthetic {
		ctx.File = t.Path
	}
	return ctx
}

// findLaunchConfig looks a configuration up by name.
func findLaunchConfig(file dap.LaunchFile, name string) (dap.LaunchConfig, bool) {
	for _, cfg := range file.Configurations {
		if cfg.Name == name {
			return cfg, true
		}
	}
	return dap.LaunchConfig{}, false
}

// rememberedLaunchName is the configuration F5 would run right now, or "" when
// it would fall back to the file on screen.
//
// It re-checks that the name still RESOLVES rather than trusting the stored
// string: the menu label is a promise about what the next F5 does, and a label
// naming a configuration that has since been deleted is worse than no label.
func (a *App) rememberedLaunchName() string {
	if a.launch.choice == "" {
		return ""
	}
	file, err := a.launchFile()
	if err != nil {
		return ""
	}
	if _, ok := findLaunchConfig(file, a.launch.choice); !ok {
		return ""
	}
	return a.launch.choice
}

// hasLaunchConfigurations reports whether this project offers a debug
// configuration at all.
//
// 🔴 It is what puts a browser configuration within reach. F5's menu row was
// gated on the active tab being a language some adapter claims — which is
// exactly what a config-only adapter is NOT — so a user editing index.html
// beside a "Launch Chrome" configuration would find the row greyed out and the
// only route to their configuration closed.
func (a *App) hasLaunchConfigurations() bool {
	file, err := a.launchFile()
	if err != nil {
		// A broken file still means "this project has configurations": the row
		// must stay reachable so pressing it can SAY the file is broken.
		return true
	}
	return len(file.Configurations) > 0 || len(file.Compounds) > 0
}

// -----------------------------------------------------------------------------
// The router
// -----------------------------------------------------------------------------

// menuStartDebug decides what F5 means and starts exactly one session.
//
// See this file's header for the branch order and why the malformed case stops
// rather than falling through.
func (a *App) menuStartDebug() {
	a.closeMenu()

	if a.debug != nil && (a.debug.starting || a.debug.running) {
		a.flash("A debug session is already running — Stop debugging first")
		return
	}

	file, err := a.launchFile()
	if err != nil {
		a.flash("launch.json: " + err.Error() + " — fix it, or F5 cannot know what you meant")
		return
	}

	if len(file.Configurations) == 0 && len(file.Compounds) == 0 {
		a.startDebugForActiveFile()
		return
	}

	if name := a.rememberedLaunchName(); name != "" {
		cfg, _ := findLaunchConfig(file, name)
		a.startLaunchConfig(cfg)
		return
	}

	if len(file.Configurations) == 1 && len(file.Compounds) == 0 {
		a.startLaunchConfig(file.Configurations[0])
		return
	}

	a.openLaunchPicker(file)
}

// menuChooseLaunchConfig always reopens the picker, whatever is remembered.
//
// The escape hatch for the remembered choice: without it, changing your mind
// would mean editing launch.json or restarting the editor.
func (a *App) menuChooseLaunchConfig() {
	a.closeMenu()
	file, err := a.launchFile()
	if err != nil {
		a.flash("launch.json: " + err.Error())
		return
	}
	if len(file.Configurations) == 0 && len(file.Compounds) == 0 {
		a.flash("This project has no .vscode/launch.json configurations")
		return
	}
	a.openLaunchPicker(file)
}

// openLaunchPicker offers every configuration, and every compound as a refusal.
//
// 🔴 It goes through openPaletteWith and checks emptiness at the call site.
// openPaletteWith returns early on an empty list WITHOUT clearing
// paletteOverride, so handing it nothing leaves whatever was there before and
// the next Esc k shows last week's list. debugview.go's header states the rule;
// this is another place that has to keep it.
func (a *App) openLaunchPicker(file dap.LaunchFile) {
	cmds := make([]paletteCommand, 0, len(file.Configurations)+len(file.Compounds))

	for _, cfg := range file.Configurations {
		cfg := cfg
		label := cfg.Name
		if label == "" {
			label = "(unnamed configuration)"
		}
		cmds = append(cmds, paletteCommand{
			label:    label,
			shortcut: launchRowHint(cfg),
			enabled:  alwaysTrue,
			action:   func(app *App) { app.startLaunchConfig(cfg) },
		})
	}

	// 🔴 Compounds are listed and REFUSED BY NAME rather than omitted. A user
	// whose compound simply vanished from this list would read the picker as
	// broken; a row that says why tells them the truth in the place they are
	// already looking. The limit itself is the one CLAUDE.md records — one
	// active leaf session, enforced out loud, because a second startDebugging
	// would replace the session on screen.
	for _, c := range file.Compounds {
		c := c
		name := c.Name
		if name == "" {
			name = "(unnamed compound)"
		}
		cmds = append(cmds, paletteCommand{
			label:    "⚠ Compound: " + name + " (not supported)",
			shortcut: strings.Join(c.Configurations, " + "),
			enabled:  alwaysTrue,
			action: func(app *App) {
				app.flash("Compound \"" + name + "\" runs " + itoa(len(c.Configurations)) +
					" targets at once; this editor debugs one at a time — pick a single configuration")
			},
		})
	}

	if len(cmds) == 0 {
		a.flash("This project has no .vscode/launch.json configurations")
		return
	}
	a.openPaletteWith(cmds, "Debug configuration")
}

// launchRowHint is the right-hand column of a picker row: what the
// configuration actually runs, so two rows called "Launch" are told apart.
func launchRowHint(cfg dap.LaunchConfig) string {
	kind := cfg.Type
	if kind == "" {
		kind = "?"
	}
	if cfg.Request != "" && cfg.Request != "launch" {
		kind += " " + cfg.Request
	}
	return kind
}

// -----------------------------------------------------------------------------
// The two ways in
// -----------------------------------------------------------------------------

// startLaunchConfig resolves one configuration and runs it, remembering it as
// the project's choice.
//
// The choice is recorded only AFTER the resolve succeeds. Remembering one that
// refused to resolve would put a name in the menu label that F5 then cannot
// honour, so every subsequent F5 would re-flash the same refusal while claiming
// to be about to run it.
func (a *App) startLaunchConfig(cfg dap.LaunchConfig) {
	spec, err := dap.ResolveLaunchConfig(cfg, a.launchVarContext())
	if err != nil {
		a.flash("launch.json: " + err.Error())
		return
	}
	a.launch.choice = cfg.Name
	a.startDebugSpec(spec)
}

// startDebugForActiveFile is the language-keyed path: F5 on a source file, with
// the adapter chosen by what the file is. Unchanged in behaviour — it is now
// expressed as a LaunchSpec so it and the launch.json path share one start
// function.
func (a *App) startDebugForActiveFile() {
	tab := a.activeTabPtr()
	if !a.hasDebuggableTab() {
		a.flash(noDebuggableTabMessage())
		return
	}
	adapter, ok := dap.AdapterFor(lsp.LanguageID(tab.Path))
	if !ok {
		a.flash("No debug adapter for this file type")
		return
	}

	// 🔴 What `program` names is per-adapter. Delve's debug mode builds a Go
	// PACKAGE, so it has to be the enclosing directory; debugpy runs a SCRIPT,
	// so it has to be the file. Hardcoding the directory makes debugpy try to
	// execute a directory.
	program := tab.Path
	if adapter.ProgramIsDir {
		program = filepath.Dir(tab.Path)
	}
	a.startDebugSpec(dap.SpecForFile(adapter, program))
}

// noDebuggableTabMessage names the file types F5 can actually start a session
// for, DERIVED from the adapter table.
//
// 🔴 The sentence it replaces was "open a Go or Python file first", written when
// that was true and left behind when js-debug landed — so the editor told users
// it could not debug JavaScript while shipping a JavaScript adapter with three
// live oracles. A message that restates a list cannot track it; this one is
// generated from the same table AdapterFor reads, so the two cannot disagree.
func noDebuggableTabMessage() string {
	langs := dap.DebuggableLanguages()
	if len(langs) == 0 {
		return "Nothing here to debug — no debug adapters are configured"
	}
	return "Nothing here to debug — F5 opens a session for: " + strings.Join(langs, ", ") +
		" (other targets come from .vscode/launch.json)"
}

// startDebugSpec is the ONE place a session is started, whichever route asked
// for it.
//
// 🔴 Two start paths would mean two copies of everything runDebugSession does —
// the coordinator's own configurationDone, adoptChildSession, the verbatim child
// configuration — and the copy is where breakpoint binding quietly stops. That
// is not hypothetical here: every one of those steps is a measured trap
// documented in CLAUDE.md's js-debug section, and each fails silently.
func (a *App) startDebugSpec(spec dap.LaunchSpec) {
	if a.dapReg == nil {
		a.dapReg = dap.NewRegistry(a.rootDir)
	}

	// Snapshot the breakpoints HERE, on the main goroutine. Reading
	// a.breakpoints from the background one would race the poll in Run that
	// keeps it current.
	bps := a.enabledBreakpoints()

	// A new run starts a clean console; the previous run's output belonged to a
	// different program state and mixing the two silently is worse than losing it.
	a.lastDebugOutput = nil

	a.debug = &debugSession{
		adapter: spec.Adapter.Name, config: spec.Target, starting: true,
		lazyBind: spec.Adapter.BreakpointsBindLazily,
		bound:    map[string][]boundBreakpoint{},
	}
	go a.runDebugSession(spec, bps)

	what := spec.Adapter.Name
	if spec.Name != "" && spec.Name != spec.Adapter.Name {
		what = spec.Name + " (" + spec.Adapter.Name + ")"
	}
	if len(bps) == 0 {
		a.flash("Starting " + what + " — no breakpoints set, the program will run to completion")
		return
	}
	a.flash(fmt.Sprintf("Starting %s with %d breakpoint(s)…", what, len(bps)))
}
