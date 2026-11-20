// =============================================================================
// File: internal/app/inlinewire.go
// Author: Vonzelle Brown
// Created: 2026-11-20
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// inlinewire.go connects AI inline suggestions end to end. internal/inline
// (the provider, its privacy guard and the debouncer) and ghost.go (the dimmed
// text after the cursor) were each complete and tested with no caller between
// them; this file is the caller. The chain, in order:
//
//	typed rune → inlineAfterInsert → Debouncer.Trigger → provider (goroutine)
//	→ inlineResultEvent posted → applyInlineResult rejects a stale answer
//	→ ghost.offer → drawInline paints it dimmed → Tab accepts (inlineKey)
//
// 🔴 OFF is not a mode of the provider, it is the ABSENCE of one. With
// suggestions off a.inline.deb is nil, nothing has been constructed, and no
// code path here can open a socket: every method on a nil *inline.Debouncer
// is a no-op. TestInlineDisabledNeverTouchesTheNetwork holds a real listener
// to that.
//
// 🔴 Esc is the editor's LEADER, so the ghost never consumes it: Esc dismisses
// the suggestion AND still arms the leader, so `Esc s` saves with a
// suggestion on screen exactly as it does without one. Tab is the only key a
// suggestion ever takes, and only while it is visible.
//
// Cancellation is polled, like publishActive and maybeSyncLSP. Tab switches,
// closes, reloads, edits and cursor moves happen from dozens of call sites;
// settleInline compares the request in flight against the editor once per
// frame and cancels it the moment they disagree, so none of those sites needs
// a hook to remember.

package app

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/cloudmanic/spice-edit/internal/editor"
	"github.com/cloudmanic/spice-edit/internal/inline"
	"github.com/cloudmanic/spice-edit/internal/lsp"
	"github.com/cloudmanic/spice-edit/internal/spiceconfig"
)

// loadInlineSettings reads config.json's inline block. A seam so a test never
// reads the developer's real config. A load error is not reported here: New
// already flashed it, and Load hands back the defaults — suggestions OFF —
// which is the right way for a broken file to fail.
var loadInlineSettings = func() spiceconfig.Inline {
	cfg, _ := spiceconfig.Load(spiceconfig.DefaultPath())
	return cfg.Inline
}

// newInlineProvider builds the guarded provider. A seam so a test can count
// constructions (the disabled path must make none) or substitute a fake.
var newInlineProvider = inline.NewProvider

// inlineDebounce is the pause after the last keystroke before a request fires.
// A variable so a test does not wait out the real quarter second.
var inlineDebounce = inline.DebounceDelay

// inlineState is everything the App holds for inline suggestions. The zero
// value is OFF and ready to use, so every hand-built test App keeps working.
type inlineState struct {
	// ghost is the display half: what is shown, and whether it still fits.
	ghost ghostText
	// settings is the config.json block, re-read whenever suggestions start.
	settings spiceconfig.Inline
	// deb is non-nil exactly while suggestions are ON. It owns the provider.
	deb *inline.Debouncer
	// provider names the running provider, for the status flash.
	provider string
	// pending is the request in flight, zero when there is none.
	pending inlinePending
	// lastErr is the provider error last flashed, so a server that is down
	// reports once rather than on every keystroke. Cleared by a success.
	lastErr string
}

// inlinePending is the exact request a result must answer, and the editor
// state it was built from.
type inlinePending struct {
	tab    *editor.Tab
	buffer *editor.Buffer
	at     editor.Position
	req    inline.Request
}

// inlineResultEvent carries a provider answer from the debouncer's goroutine
// onto the UI goroutine. It is a selfApplyingEvent, so handleEvent needs no
// case of its own.
type inlineResultEvent struct {
	when   time.Time
	result inline.Result
}

// When satisfies tcell.Event.
func (e *inlineResultEvent) When() time.Time { return e.when }

// apply hands the result to the App on the main loop.
func (e *inlineResultEvent) apply(a *App) { a.applyInlineResult(e.result) }

// startInline loads the settings at launch and starts suggestions only when
// config.json enables them. Called from beginLifecycle, i.e. from Run: a test
// App that never runs never reads config or builds a provider.
func (a *App) startInline() {
	a.inline.settings = loadInlineSettings()
	if a.inline.settings.Enabled {
		a.enableInline()
	}
}

// inlineConfigFor translates config.json's block into the engine's Config for
// workspace root. Enabled is set because this is only ever called when the
// user has switched suggestions on — by config or from the ≡ menu, which is
// the same explicit consent. allowRemote opts THIS workspace in, since the
// user-level file is the only place this editor records the choice; without
// it a remote provider is refused by NewProvider.
func inlineConfigFor(s spiceconfig.Inline, root string) inline.Config {
	c := inline.Config{
		Enabled:  true,
		Provider: s.Provider,
		Ollama:   inline.OllamaConfig{Endpoint: s.URL, Model: s.Model},
		Remote:   inline.RemoteConfig{Endpoint: s.URL, Model: s.Model},
	}
	if s.AllowRemote && root != "" {
		c.Remote.Workspaces = []string{root}
	}
	return c
}

// enableInline constructs the provider and debouncer and reports whether
// suggestions are now on. A construction failure is flashed, in words that
// name the config key to change, and leaves them off.
//
// ⚠️ Construction runs on the UI goroutine, and the ollama provider resolves
// its endpoint's host to prove it is loopback. That lookup is bounded by
// internal/inline's resolveTimeout (5s); an IP literal — the default url —
// needs no lookup at all, and "localhost" is answered from the hosts file. It
// happens once, at launch or on the ≡ toggle, never per keystroke.
func (a *App) enableInline() bool {
	if a.inline.deb != nil {
		return true
	}
	s := a.inline.settings
	p, err := newInlineProvider(inlineConfigFor(s, a.rootDir), a.rootDir, os.Getenv, nil)
	if err != nil {
		a.flash("Inline suggestions not started: " + inlineSetupError(err, s))
		return false
	}
	scr := a.screen
	d := inline.NewDebouncer(p, func(r inline.Result) {
		// Runs on the debouncer's goroutine with its lock held: post and
		// return. PostEvent never blocks; a full queue drops the answer,
		// and the next keystroke asks again.
		if scr != nil {
			_ = scr.PostEvent(&inlineResultEvent{when: time.Now(), result: r})
		}
	})
	d.Delay = inlineDebounce
	a.inline.deb = d
	a.inline.provider = p.Name()
	a.inline.lastErr = ""
	return true
}

// stopInline switches suggestions off: the debouncer is closed (cancelling a
// request in flight and refusing later triggers), the provider goes with it,
// and any suggestion on screen is dropped. Idempotent; shutdownBackground
// calls it, and so does the ≡ toggle.
func (a *App) stopInline() {
	a.inline.deb.Close()
	a.inline.deb = nil
	a.inline.provider = ""
	a.inline.pending = inlinePending{}
	a.inline.ghost.dismiss()
}

// inlineSetupError turns a construction error into the config change that
// fixes it. The engine's own messages name its internals, not the file.
func inlineSetupError(err error, s spiceconfig.Inline) string {
	name := s.Provider
	if name == "" {
		name = inline.ProviderOllama
	}
	switch {
	case errors.Is(err, inline.ErrNoModel):
		return `set "model" in the inline block of config.json`
	case errors.Is(err, inline.ErrNotLocal):
		return `the ollama "url" must be on this machine (localhost)`
	case errors.Is(err, inline.ErrRemoteNotOptedIn):
		return fmt.Sprintf(`%q sends code off this machine; set "allowRemote": true to allow it`, name)
	case errors.Is(err, inline.ErrNoKey):
		return fmt.Sprintf("no API key for %q in the environment", name)
	case errors.Is(err, inline.ErrRemoteUnimplemented):
		return fmt.Sprintf(`%q is not supported yet; use "ollama"`, name)
	case errors.Is(err, inline.ErrNoProvider):
		return fmt.Sprintf("unknown provider %q", name)
	}
	return err.Error()
}

// menuToggleInlineSuggestions is the ≡ menu's on/off switch. Turning on
// re-reads config.json first, so a model added since launch is picked up
// without restarting the editor.
func (a *App) menuToggleInlineSuggestions() {
	a.closeMenu()
	if a.inline.deb != nil {
		a.stopInline()
		a.flash("Inline suggestions off")
		return
	}
	a.inline.settings = loadInlineSettings()
	if a.enableInline() {
		a.flash("Inline suggestions on (" + a.inline.provider + ") — Tab accepts")
	}
}

// inlineSuggestionsLabel is the toggle row's label, showing the current state.
func (a *App) inlineSuggestionsLabel() string {
	if a.inline.deb != nil {
		return "Toggle inline suggestions (on)"
	}
	return "Toggle inline suggestions (off)"
}

// editorHasKeyboard reports whether keys are going to the text: no modal,
// palette, finder, find bar or menu has taken them. A suggestion only lives
// while this holds — Tab belongs to whatever overlay is open, so a ghost left
// visible beside one would promise an accept that cannot happen.
func (a *App) editorHasKeyboard() bool {
	return !a.anyModalOpen() && !a.paletteOpen
}

// inlineRequestable reports whether tab may be sent to a provider at all. The
// deny-list is checked here as well as in the guard, so a secrets file never
// even schedules a request — the guard remains the backstop.
func inlineRequestable(tab *editor.Tab) bool {
	return ghostEligible(tab) && !inline.IsDenied(tab.Path)
}

// inlineRequestFor builds the request for tab's cursor. It collects only the
// lines nearest the cursor, just past the caps the guard enforces, so the cost
// per keystroke is bounded by the caps rather than by the file's size. The
// guard still trims to the exact byte limits.
func inlineRequestFor(tab *editor.Tab) inline.Request {
	lines := tab.Buffer.Lines
	cur := tab.Buffer.Clamp(tab.Cursor)
	line := []rune(lines[cur.Line])
	before, after := string(line[:cur.Col]), string(line[cur.Col:])

	pre := []string{before}
	for i, n := cur.Line-1, len(before); i >= 0 && n <= inline.MaxPrefixBytes; i-- {
		pre = append(pre, lines[i])
		n += len(lines[i]) + 1
	}
	for i, j := 0, len(pre)-1; i < j; i, j = i+1, j-1 {
		pre[i], pre[j] = pre[j], pre[i]
	}
	suf := []string{after}
	for i, n := cur.Line+1, len(after); i < len(lines) && n <= inline.MaxSuffixBytes; i++ {
		suf = append(suf, lines[i])
		n += len(lines[i]) + 1
	}
	return inline.Request{
		Prefix:   strings.Join(pre, "\n"),
		Suffix:   strings.Join(suf, "\n"),
		Language: lsp.LanguageID(tab.Path),
		Path:     tab.Path,
		Revision: revisionOf(tab),
	}
}

// inlineAfterInsert is the trigger: handleKey calls it right after a typed
// rune reached the buffer, beside the as-you-type completion trigger. It
// (re)arms the debouncer, which cancels whatever was pending or in flight —
// the superseding-request case.
func (a *App) inlineAfterInsert(tab *editor.Tab) {
	d := a.inline.deb
	if d == nil {
		return
	}
	if a.completionOpen || !a.editorHasKeyboard() || !inlineRequestable(tab) {
		// The popup wins, and an ineligible tab gets nothing: drop what was
		// in flight rather than let it answer for text that has moved on.
		d.Cancel()
		a.inline.pending = inlinePending{}
		return
	}
	req := inlineRequestFor(tab)
	a.inline.pending = inlinePending{tab: tab, buffer: tab.Buffer, at: tab.Cursor, req: req}
	d.Trigger(req)
}

// settleInline cancels the request in flight once the editor no longer looks
// the way it did when the request was built: another tab, a closed tab, a
// reloaded buffer, any edit, a cursor move, or a modal taking the keyboard.
// Every comparison is O(1), so running it per frame costs nothing.
func (a *App) settleInline() {
	p := a.inline.pending
	if p.tab == nil {
		return
	}
	tab := a.activeTabPtr()
	if tab == p.tab && tab.Buffer == p.buffer && tab.Cursor == p.at &&
		revisionOf(tab) == p.req.Revision && a.editorHasKeyboard() {
		return
	}
	a.inline.deb.Cancel()
	a.inline.pending = inlinePending{}
}

// applyInlineResult offers a provider answer as ghost text, or drops it. An
// answer is dropped when suggestions were switched off meanwhile, when it is
// not the answer to the request still pending (a newer keystroke superseded
// it while it sat in the event queue), or when ghost.offer finds the buffer
// or cursor has moved — the anchor check that makes a slow provider late,
// never wrong. Comparing the whole Request, not just the revision, matters:
// typing over an auto-closed quote moves the cursor without an edit, so two
// requests can share a revision and differ only in where they were asked.
func (a *App) applyInlineResult(r inline.Result) {
	p := a.inline.pending
	if a.inline.deb == nil || p.tab == nil || r.Request != p.req {
		return
	}
	a.inline.pending = inlinePending{}
	if r.Err != nil {
		if msg := r.Err.Error(); msg != a.inline.lastErr {
			a.inline.lastErr = msg
			a.flash("Inline suggestions: " + msg)
		}
		return
	}
	a.inline.lastErr = ""
	tab := a.activeTabPtr()
	if tab != p.tab || tab.Buffer != p.buffer || !a.editorHasKeyboard() {
		return
	}
	a.inline.ghost.offer(tab, p.req.Revision, p.at, r.Suggestion.Text)
}

// inlineKey is handleKey's first look at every key. It returns true only when
// a visible suggestion was accepted with Tab; every other key dismisses the
// suggestion and returns false, so the key is handled exactly as if no
// suggestion had existed — Esc included, which is what keeps the leader
// working with a suggestion on screen.
func (a *App) inlineKey(ev *tcell.EventKey) bool {
	g := &a.inline.ghost
	if !g.active {
		return false
	}
	// A "Window too small" frame returns from draw before drawInline runs, so
	// ghost.drawn still describes an older frame; the size is checked here.
	if !a.editorHasKeyboard() || a.width < minWidth || a.height < minHeight {
		g.dismiss()
		return false
	}
	return g.handleKey(ev, a.activeTabPtr(), a.completionOpen)
}

// drawInline is the per-frame hook, called from draw after Tab.Render and the
// other editor overlays and before the find bar, status bar and modals. It
// settles the request in flight, then paints the suggestion — last of the
// editor overlays, so the text it shifts right carries their styling with it.
func (a *App) drawInline() {
	a.settleInline()
	g := &a.inline.ghost
	if !g.active {
		return
	}
	tab := a.activeTabPtr()
	if tab == nil || !a.editorHasKeyboard() {
		g.dismiss()
		return
	}
	ex, ey, ew, eh := a.editorRect()
	g.draw(a.screen, a.theme, tab, a.completionOpen, ex, ey, ew, eh)
}
