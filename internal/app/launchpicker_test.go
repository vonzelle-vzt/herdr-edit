// =============================================================================
// File: internal/app/launchpicker_test.go
// Author: Vonzelle Brown
// Created: 2026-07-31
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/cloudmanic/spice-edit/internal/dap"
)

// writeLaunchJSON drops a .vscode/launch.json into root and returns its path.
func writeLaunchJSON(t *testing.T, root, body string) string {
	t.Helper()
	dir := filepath.Join(root, ".vscode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "launch.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// launchFixture is a project with a real Go file open and a launch.json holding
// whatever the caller needs.
func launchFixture(t *testing.T, body string) (*App, string) {
	t.Helper()
	a, path := debugFixture(t)
	writeLaunchJSON(t, a.rootDir, body)
	return a, path
}

// screenHasText reports whether any row of the rendered screen contains want.
//
// 🔴 It scans the SIMULATION SCREEN rather than a.paletteResults, because the
// claim under test is that the user is offered a choice — and this fork has
// twice shipped state that was correct while nothing reached the pixels. A
// picker that is "open" in a struct field and paints nothing is exactly the
// failure the ScreenPos lesson in CLAUDE.md is about.
func screenHasText(t *testing.T, a *App, want string) bool {
	t.Helper()
	scr, ok := a.screen.(tcell.SimulationScreen)
	if !ok {
		t.Fatal("test app is not backed by a simulation screen")
	}
	scr.Show()
	cells, w, h := scr.GetContents()
	for y := 0; y < h; y++ {
		var row strings.Builder
		for x := 0; x < w; x++ {
			if r := cells[y*w+x].Runes; len(r) > 0 {
				row.WriteRune(r[0])
			} else {
				row.WriteRune(' ')
			}
		}
		if strings.Contains(row.String(), want) {
			return true
		}
	}
	return false
}

// TestF5WithTwoLaunchConfigsOpensThePickerOnScreen is the Stage 2 oracle.
//
// 🔴 It goes RED against the language-keyed start path, which reads no
// launch.json at all: internal/dap/launchjson.go was complete, careful and
// tested with NO non-test caller, while a feature table elsewhere claimed
// "live session + launch.json". A user with two configurations got neither of
// them and no way to say which they meant.
//
// The three assertions are one claim: both names are ON SCREEN, and NOTHING was
// launched behind the picker. Asserting only the first would pass against a
// version that opened the picker AND started a session, which is worse than
// either — the user picks a configuration while a different program is already
// running.
func TestF5WithTwoLaunchConfigsOpensThePickerOnScreen(t *testing.T) {
	a, _ := launchFixture(t, `{
  "version": "0.2.0",
  "configurations": [
    // Two of them, so there is a genuine choice to make.
    {"name": "Launch Package", "type": "go", "request": "launch", "program": "${workspaceFolder}"},
    {"name": "Debug Current File", "type": "go", "request": "launch", "program": "${file}"}
  ]
}`)

	a.menuDebugStartOrContinue()
	a.draw()

	for _, want := range []string{"Launch Package", "Debug Current File"} {
		if !screenHasText(t, a, want) {
			t.Errorf("the configuration %q is not on screen after F5; status = %q",
				want, a.statusMsg)
		}
	}
	if a.debug != nil {
		t.Errorf("a session was started (%+v) instead of asking which configuration to run",
			a.debug)
	}
}

// TestMalformedLaunchJSONStopsRatherThanFallingBack is the branch worth arguing
// about, and the reason the router loads the file BEFORE anything else.
//
// 🔴 Falling through to the language-keyed path would debug SOMETHING — just not
// the thing the user configured — so a stray comma reads as "the editor ignores
// my launch.json" rather than as a syntax error they can fix. internal/dap
// already distinguishes an absent file from a broken one; this is the call site
// that has to honour the distinction.
func TestMalformedLaunchJSONStopsRatherThanFallingBack(t *testing.T) {
	a, _ := launchFixture(t, `{"configurations": [`)

	a.menuStartDebug()

	if a.debug != nil {
		t.Fatalf("a %s session was started behind a launch.json that does not parse",
			a.debug.adapter)
	}
	if !strings.Contains(a.statusMsg, "launch.json") {
		t.Errorf("status = %q; it must name the file the user has to fix", a.statusMsg)
	}
}

// TestNoLaunchJSONKeepsTheLanguageKeyedPath pins the no-regression case: a
// project with no configurations behaves exactly as it did before this file
// existed.
func TestNoLaunchJSONKeepsTheLanguageKeyedPath(t *testing.T) {
	a, path := debugFixture(t) // no .vscode at all
	t.Cleanup(a.stopDebugSession)

	a.menuStartDebug()

	if a.debug == nil {
		t.Fatal("F5 on a Go file with no launch.json started nothing")
	}
	if a.debug.adapter != "delve" {
		t.Errorf("adapter = %q, want delve", a.debug.adapter)
	}
	// delve builds a PACKAGE, so the target is the enclosing directory.
	if want := filepath.Dir(path); a.debug.config != want {
		t.Errorf("target = %q, want the package directory %q", a.debug.config, want)
	}
}

// TestSingleConfigurationRunsWithoutAsking checks the one-config case does not
// make the user pick from a list of one.
func TestSingleConfigurationRunsWithoutAsking(t *testing.T) {
	a, _ := launchFixture(t, `{
  "configurations": [
    {"name": "Only One", "type": "go", "request": "launch", "program": "${workspaceFolder}"}
  ]
}`)
	t.Cleanup(a.stopDebugSession)

	a.menuStartDebug()

	if a.paletteOpen {
		t.Error("a picker opened for a single configuration")
	}
	if a.debug == nil {
		t.Fatal("the only configuration was not started")
	}
	if a.debug.config != a.rootDir {
		t.Errorf("target = %q, want the expanded ${workspaceFolder} %q", a.debug.config, a.rootDir)
	}
}

// TestRememberedChoiceIsVisibleAndDroppedWhenLaunchJSONChanges pins both halves
// of the memory rule.
//
// 🔴 A sticky choice with no visible indicator is a HIDDEN MODE: next week's F5
// would run a different program from the file on screen with nothing to explain
// it. And a choice that outlived an edit to launch.json would name a
// configuration whose meaning has changed underneath it — the label would still
// look right, which is the worst version.
func TestRememberedChoiceIsVisibleAndDroppedWhenLaunchJSONChanges(t *testing.T) {
	a, _ := launchFixture(t, `{
  "configurations": [
    {"name": "Alpha", "type": "go", "request": "launch", "program": "${workspaceFolder}"},
    {"name": "Beta",  "type": "go", "request": "launch", "program": "${workspaceFolder}"}
  ]
}`)
	t.Cleanup(a.stopDebugSession)

	// Before any choice, the label says nothing about a configuration.
	if got := a.debugStartLabel(); got != "Start debugging" {
		t.Errorf("label = %q before a choice was made", got)
	}

	a.menuStartDebug()
	runPaletteRow(t, a, "Beta")

	if a.debug == nil {
		t.Fatal("picking a row started nothing")
	}

	// The label is read with no session in flight: while one is starting it
	// correctly says so instead, and asserting through that state would be
	// asserting about the wrong branch.
	a.stopDebugSession()
	a.debug = nil
	if got := a.debugStartLabel(); !strings.Contains(got, "Beta") {
		t.Errorf("label = %q; a remembered choice the menu does not name is a hidden mode", got)
	}

	// The next F5 re-runs it without asking.
	a.menuStartDebug()
	if a.paletteOpen {
		t.Error("the picker reopened despite a remembered choice")
	}
	if a.debug == nil {
		t.Fatal("the remembered choice did not start")
	}

	// Now the file changes underneath it.
	a.stopDebugSession()
	a.debug = nil
	path := writeLaunchJSON(t, a.rootDir, `{
  "configurations": [
    {"name": "Gamma", "type": "go", "request": "launch", "program": "${workspaceFolder}"},
    {"name": "Delta", "type": "go", "request": "launch", "program": "${workspaceFolder}"}
  ]
}`)
	// mtime granularity is coarse enough that two writes inside one tick are
	// indistinguishable; make the change unambiguous rather than sleeping.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	if got := a.debugStartLabel(); got != "Start debugging" {
		t.Errorf("label = %q; the choice survived an edit that deleted the configuration", got)
	}
	a.menuStartDebug()
	if !a.paletteOpen {
		t.Error("F5 did not re-ask after launch.json changed")
	}
	if a.debug != nil {
		t.Errorf("a %s session started against a stale choice", a.debug.adapter)
	}
}

// TestCompoundIsRefusedByNameNotOmitted pins the compound contract.
//
// The editor debugs ONE leaf session at a time — a deliberate cut recorded in
// CLAUDE.md, not an oversight. A compound that simply vanished from the picker
// would read as the picker being broken; a row that names it and says why tells
// the truth in the place the user is already looking.
func TestCompoundIsRefusedByNameNotOmitted(t *testing.T) {
	a, _ := launchFixture(t, `{
  "configurations": [
    {"name": "Server", "type": "node", "request": "launch", "program": "${workspaceFolder}/server.js"}
  ],
  "compounds": [
    {"name": "Full Stack", "configurations": ["Server", "Browser"]}
  ]
}`)

	a.menuStartDebug()
	a.draw()

	if !screenHasText(t, a, "Full Stack") {
		t.Error("the compound is not on screen; a user whose configuration simply vanished " +
			"reads that as a broken picker")
	}
	if !screenHasText(t, a, "not supported") {
		t.Error("the compound row does not say it is unsupported")
	}

	runPaletteRow(t, a, "Full Stack")
	if a.debug != nil {
		t.Fatalf("a %s session was started from a compound row", a.debug.adapter)
	}
	if !strings.Contains(a.statusMsg, "Full Stack") {
		t.Errorf("the refusal %q does not name the compound", a.statusMsg)
	}
	if !strings.Contains(a.statusMsg, "one at a time") {
		t.Errorf("the refusal %q does not explain the limit", a.statusMsg)
	}
}

// TestNothingToDebugMessageIsDerivedFromTheAdapterTable is the stale-flash
// oracle.
//
// 🔴 The message said "open a Go or Python file first" for the entire life of
// the js-debug adapter: true when written, a lie the moment a third row landed,
// and nothing could catch it because the sentence restated a list instead of
// reading one. This asserts the DERIVATION — every language the table claims
// must appear — so the message cannot go stale again without failing here.
func TestNothingToDebugMessageIsDerivedFromTheAdapterTable(t *testing.T) {
	dir := t.TempDir()
	notCode := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(notCode, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := newTestApp(t, dir)
	a.openFile(notCode)

	a.menuStartDebug()

	if a.debug != nil {
		t.Fatal("a session was started for a .txt file")
	}
	langs := dap.DebuggableLanguages()
	if len(langs) == 0 {
		t.Fatal("the adapter table claims no languages at all; this assertion would be vacuous")
	}
	for _, lang := range langs {
		if !strings.Contains(a.statusMsg, lang) {
			t.Errorf("status %q never mentions %q, which the adapter table says F5 can debug",
				a.statusMsg, lang)
		}
	}
}

// TestChooseDebugConfigurationAlwaysReopensThePicker covers the escape hatch. A
// remembered choice with no way to change it is a trap: the only cures would be
// editing launch.json or restarting the editor.
func TestChooseDebugConfigurationAlwaysReopensThePicker(t *testing.T) {
	a, _ := launchFixture(t, `{
  "configurations": [
    {"name": "Alpha", "type": "go", "request": "launch", "program": "${workspaceFolder}"},
    {"name": "Beta",  "type": "go", "request": "launch", "program": "${workspaceFolder}"}
  ]
}`)
	t.Cleanup(a.stopDebugSession)

	a.menuStartDebug()
	runPaletteRow(t, a, "Alpha")
	a.debug = nil

	// F5 would now re-run Alpha. This must ask again anyway.
	a.menuChooseLaunchConfig()
	if !a.paletteOpen {
		t.Fatal("Choose debug configuration did not open the picker")
	}
	a.draw()
	for _, want := range []string{"Alpha", "Beta"} {
		if !screenHasText(t, a, want) {
			t.Errorf("%q is not on screen in the reopened picker", want)
		}
	}
	if a.debug != nil {
		t.Error("choosing a configuration started a session before one was picked")
	}
}

// TestEmptyLaunchPickerNeverLeavesAStaleOverride is the openPaletteWith rule
// debugview.go's header states.
//
// 🔴 openPaletteWith returns early on an empty list WITHOUT clearing
// paletteOverride, so a caller that hands it nothing leaves whatever was there
// before — and the next Esc k shows last week's list under the wrong title.
// Every entry point has to check emptiness itself.
func TestEmptyLaunchPickerNeverLeavesAStaleOverride(t *testing.T) {
	a, _ := launchFixture(t, `{"version": "0.2.0", "configurations": []}`)

	// Something else is in the palette first, so a stale override is visible.
	a.openPaletteWith([]paletteCommand{{label: "LEFTOVER", enabled: alwaysTrue, action: func(*App) {}}}, "Old")
	a.closePalette()

	a.menuChooseLaunchConfig()

	if a.paletteOpen {
		t.Error("a picker opened over an empty configuration list")
	}
	if a.paletteOverride != nil {
		t.Errorf("paletteOverride survived as %v; the next Esc k would show it", a.paletteOverride)
	}
	if !strings.Contains(a.statusMsg, "launch.json") {
		t.Errorf("status = %q; nothing explained why no picker appeared", a.statusMsg)
	}
}

// TestPwaChromeIsNeverAutoSelectedByLanguage is the ONE-TASK claim: browser
// debugging exists, and no file on disk can conjure it.
//
// 🔴 The chrome row is the same binary and the same Locate hook as the Node row.
// If it claimed `javascript`, AdapterFor's first-match-wins scan would hand
// every .js file to whichever row came first in the slice — so a server-side
// script would launch a browser, decided by nothing but table order.
func TestPwaChromeIsNeverAutoSelectedByLanguage(t *testing.T) {
	dir := t.TempDir()
	js := filepath.Join(dir, "server.js")
	if err := os.WriteFile(js, []byte("console.log(1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	html := filepath.Join(dir, "index.html")
	if err := os.WriteFile(html, []byte("<html></html>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := newTestApp(t, dir)
	t.Cleanup(a.stopDebugSession)

	// A .js file reaches the NODE row.
	a.openFile(js)
	a.menuStartDebug()
	if a.debug == nil {
		t.Fatal("F5 on a .js file started nothing")
	}
	if a.debug.adapter != "js-debug" {
		t.Errorf("adapter = %q, want js-debug (the node row) — a browser was launched for a "+
			"server-side script", a.debug.adapter)
	}
	a.stopDebugSession()
	a.debug = nil

	// An .html file reaches nothing at all, and says so.
	a.openFile(html)
	a.menuStartDebug()
	if a.debug != nil {
		t.Fatalf("a %s session started for an .html file with no launch.json", a.debug.adapter)
	}

	// The only route to the browser adapter is a configuration that names it.
	writeLaunchJSON(t, dir, `{
  "configurations": [
    {"name": "Launch Chrome", "type": "chrome", "request": "launch", "url": "http://localhost:5173"}
  ]
}`)
	a.menuStartDebug()
	if a.debug == nil {
		t.Fatal("the chrome configuration started nothing")
	}
	if a.debug.adapter != "js-debug (chrome)" {
		t.Errorf("adapter = %q, want the chrome row", a.debug.adapter)
	}
	if a.debug.config != "http://localhost:5173" {
		t.Errorf("target = %q, want the url — a browser session runs no program", a.debug.config)
	}
}

// TestChromeSpecCarriesTheWorkspaceFolder is the wire-level half of the same
// feature, asserted without a live browser.
//
// 🔴 MEASURED in js-debug 1.117.0's own bundle: its chrome defaults hold
// webRoot:"${workspaceFolder}", its resolver expands that from __workspaceFolder,
// and the branch taken when that key is ABSENT sets webRoot to "/" outright.
// Every source url then maps to nothing on disk and no breakpoint binds — on a
// session that initializes, runs and terminates without one error. This asserts
// the key is on the wire, which is the only observable that distinguishes the
// working case from the broken one before Chrome is even involved.
func TestChromeSpecCarriesTheWorkspaceFolder(t *testing.T) {
	a, _ := launchFixture(t, `{
  "configurations": [
    {"name": "Launch Chrome", "type": "chrome", "request": "launch",
     "url": "http://localhost:5173", "webRoot": "${workspaceFolder}/src"}
  ]
}`)

	file, err := a.launchFile()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	spec, err := dap.ResolveLaunchConfig(file.Configurations[0], a.launchVarContext())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if got := spec.Args["__workspaceFolder"]; got != a.rootDir {
		t.Errorf("__workspaceFolder = %v, want %s — without it js-debug sets webRoot to \"/\" "+
			"and browser breakpoints never bind on a session that looks healthy", got, a.rootDir)
	}
	if got := spec.Args["type"]; got != "pwa-chrome" {
		t.Errorf("type = %v, want pwa-chrome; the standalone server does not know the "+
			"`chrome` alias the VS Code extension registers", got)
	}
	webRoot, _ := spec.Args["webRoot"].(string)
	if !filepath.IsAbs(webRoot) {
		t.Errorf("webRoot = %q, want an absolute path", webRoot)
	}
	if want := filepath.Join(a.rootDir, "src"); webRoot != want {
		t.Errorf("webRoot = %q, want %q", webRoot, want)
	}
}

// TestAttachConfigurationSendsAttachOnTheWire is the root path's missing verb,
// asserted against RECORDED WIRE TRAFFIC.
//
// 🔴 The root called client.Launch unconditionally. A `"request": "attach"`
// configuration would therefore START the process it was written to attach to,
// leaving the user stepping through a second copy while the one they cared about
// ran on untouched — and neither the adapter nor the protocol reports that as an
// error, because both verbs are valid requests. The only evidence that exists is
// which command came down the socket, which is why this reads the connection
// rather than a struct field.
func TestAttachConfigurationSendsAttachOnTheWire(t *testing.T) {
	server, argv := newFakeCoordinator(t)
	withFakeJsDebugAdapter(t, argv, true)

	a, _ := jsAppFixture(t)
	t.Cleanup(a.stopDebugSession)
	writeLaunchJSON(t, a.rootDir, `{
  "configurations": [
    {"name": "Attach to Runtime", "type": "fake-node", "request": "attach", "port": 9229}
  ]
}`)

	a.menuStartDebug()
	if a.debug == nil {
		t.Fatalf("the attach configuration started nothing; status %q", a.statusMsg)
	}

	if !pumpEvents(t, a, 60*time.Second, func() bool {
		root := server.conn(0)
		if root == nil {
			return false
		}
		for _, c := range root.commands() {
			if c == "attach" || c == "launch" {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("neither verb ever reached the coordinator; connections=%d status=%q",
			server.connCount(), a.statusMsg)
	}

	cmds := server.conn(0).commands()
	sawAttach, sawLaunch := false, false
	for _, c := range cmds {
		switch c {
		case "attach":
			sawAttach = true
		case "launch":
			sawLaunch = true
		}
	}
	if sawLaunch {
		t.Errorf("the root sent `launch` for a request:attach configuration; commands were %v — "+
			"that starts a SECOND copy of the process the user meant to join", cmds)
	}
	if !sawAttach {
		t.Errorf("the root never sent `attach`; commands were %v", cmds)
	}
}
