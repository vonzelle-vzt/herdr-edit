// =============================================================================
// File: internal/inline/provider.go
// Author: Vonzelle Brown
// Created: 2026-10-14
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// Package inline is the engine behind AI inline (ghost text) completion: the
// provider interface, the debounce that turns keystrokes into requests, and
// the privacy guard every request passes through before it can leave the
// editor.
//
// 🔴 The guard is the feature's safety story: off by default, never a
// deny-listed file, never more than 4 KB of prefix or 1 KB of suffix, and
// nothing logged but timings. It is intrinsic, not opt-in: every exported
// constructor yields a *Guarded, the Debouncer accepts nothing else, and the
// concrete clients are unexported so none can be reached around it.
package inline

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Request is everything a provider is allowed to see about the cursor.
type Request struct {
	Prefix   string // buffer text before the cursor
	Suffix   string // buffer text after the cursor
	Language string // lsp-style language id, e.g. "go"
	Path     string // file path, used for the deny-list and nothing else

	// Revision is the host's buffer revision the request was built from. It
	// is never sent to a provider; it rides back in Result.Request so the host
	// can refuse an answer computed against text that has since changed.
	Revision uint64
}

// Suggestion is a provider's answer: the text to show as ghost text.
type Suggestion struct {
	Text string
}

// Provider produces a completion for the text around the cursor. It must
// honour ctx: a cancelled context aborts the request, in flight included.
type Provider interface {
	Name() string
	Complete(ctx context.Context, req Request) (Suggestion, error)
}

const (
	// MaxPrefixBytes caps how much text before the cursor may be sent.
	MaxPrefixBytes = 4 * 1024
	// MaxSuffixBytes caps how much text after the cursor may be sent.
	MaxSuffixBytes = 1024
	// DebounceDelay is how long after the last keystroke a request fires.
	DebounceDelay = 250 * time.Millisecond
)

var (
	// ErrDisabled is returned when inline completion is switched off.
	ErrDisabled = errors.New("inline: completion is disabled")
	// ErrDenied is returned for a file on the secret deny-list.
	ErrDenied = errors.New("inline: file is on the secret deny-list")
	// ErrNoProvider is returned when the config names no usable provider.
	ErrNoProvider = errors.New("inline: no provider configured")
	// ErrRemoteNotOptedIn is returned when a remote provider is configured
	// but this workspace has not opted in to it.
	ErrRemoteNotOptedIn = errors.New("inline: remote provider not enabled for this workspace")
	// ErrNoKey is returned when a remote provider has no API key.
	ErrNoKey = errors.New("inline: no API key for remote provider")
	// ErrRemoteUnimplemented is returned for a remote kind with no client yet.
	ErrRemoteUnimplemented = errors.New("inline: remote provider client not implemented")
)

// Provider names accepted in Config.Provider.
const (
	ProviderOllama    = "ollama"
	ProviderOpenAI    = "openai"    // any OpenAI-compatible endpoint
	ProviderAnthropic = "anthropic" // Anthropic messages API
)

// Config is the user-facing switchboard. The zero value is OFF.
//
// 🔴 There is deliberately no API-key field anywhere in it. A key in a config
// file ends up in dotfile repos and screenshots; remote keys come from the
// environment (or a keychain lookup a caller passes in), never from here.
type Config struct {
	Enabled  bool         // master switch; false sends nothing
	Provider string       // "" or "ollama" selects the local default
	Ollama   OllamaConfig // local provider settings
	Remote   RemoteConfig // opt-in remote provider settings
}

// RemoteConfig describes an opt-in remote provider. Selecting one in
// Config.Provider is not enough: the workspace must also be listed.
type RemoteConfig struct {
	Endpoint   string   // base URL; empty means the provider's own default
	Model      string   // model name sent to the provider
	Workspaces []string // absolute workspace roots that opted in
	KeyEnv     string   // env var holding the key; empty means the kind's default
}

// defaultKeyEnv is the env var each remote kind reads its key from when
// RemoteConfig.KeyEnv is empty.
var defaultKeyEnv = map[string]string{
	ProviderOpenAI:    "OPENAI_API_KEY",
	ProviderAnthropic: "ANTHROPIC_API_KEY",
}

// KeyLookup resolves a secret by name. os.Getenv satisfies it; a keychain
// backend can too, which is the seam for the OS-keychain route.
type KeyLookup func(name string) string

// RemoteOptedIn reports whether workspace is one of the roots that opted in
// to the remote provider. Paths are cleaned so a trailing slash still matches.
func (c Config) RemoteOptedIn(workspace string) bool {
	if workspace == "" {
		return false
	}
	ws := filepath.Clean(workspace)
	for _, w := range c.Remote.Workspaces {
		if w != "" && filepath.Clean(w) == ws {
			return true
		}
	}
	return false
}

// RemoteKey finds the remote provider's API key via lookup, reading the env
// var named in config or the kind's default. It never reads the config file.
func (c Config) RemoteKey(lookup KeyLookup) (string, error) {
	name := c.Remote.KeyEnv
	if name == "" {
		name = defaultKeyEnv[c.Provider]
	}
	if name == "" || lookup == nil {
		return "", ErrNoKey
	}
	key := strings.TrimSpace(lookup(name))
	if key == "" {
		return "", ErrNoKey
	}
	return key, nil
}

// NewProvider builds the provider the config selects for workspace, already
// wrapped in the guard. Disabled config yields ErrDisabled. onTiming may be nil.
func NewProvider(c Config, workspace string, lookup KeyLookup, onTiming func(Timing)) (*Guarded, error) {
	if !c.Enabled {
		return nil, ErrDisabled
	}
	switch c.Provider {
	case "", ProviderOllama:
		o, err := newOllama(c.Ollama)
		if err != nil {
			return nil, err
		}
		return Guard(c, o, onTiming), nil
	case ProviderOpenAI, ProviderAnthropic:
		if !c.RemoteOptedIn(workspace) {
			return nil, ErrRemoteNotOptedIn
		}
		if _, err := c.RemoteKey(lookup); err != nil {
			return nil, err
		}
		return nil, ErrRemoteUnimplemented
	}
	return nil, ErrNoProvider
}

// deniedNames are exact base names that hold secrets.
var deniedNames = map[string]bool{
	".env": true, ".envrc": true, ".netrc": true, "_netrc": true, ".npmrc": true,
	".pypirc": true, ".pgpass": true, ".git-credentials": true, ".htpasswd": true,
	"credentials": true, "credentials.json": true, "id_rsa": true, "id_dsa": true,
	"id_ecdsa": true, "id_ed25519": true, "known_hosts": true, "authorized_keys": true,
	".dockercfg": true, "kubeconfig": true, "terraform.tfstate": true,
}

// deniedExts are extensions of key, certificate and vault files.
var deniedExts = map[string]bool{
	".pem": true, ".key": true, ".p12": true, ".pfx": true, ".jks": true,
	".keystore": true, ".kdbx": true, ".gpg": true, ".asc": true, ".tfvars": true,
	".tfstate": true, ".ppk": true, ".der": true, ".crt": true, ".cer": true,
}

// deniedDirs are directory names whose every file is treated as secret.
var deniedDirs = map[string]bool{
	".ssh": true, ".gnupg": true, ".aws": true, ".kube": true, ".docker": true,
}

// IsDenied reports whether path is on the secret deny-list. Matching is by
// lowercased name, so a variant spelling (.ENV, id_rsa.pub) cannot slip past.
func IsDenied(path string) bool {
	if path == "" {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(path))
	parts := strings.Split(strings.ToLower(clean), "/")
	base := parts[len(parts)-1]
	for _, dir := range parts[:len(parts)-1] {
		if deniedDirs[dir] {
			return true
		}
	}
	if deniedNames[base] || deniedExts[filepath.Ext(base)] {
		return true
	}
	// .env.local, .env.production, id_rsa.pub, secrets.yaml, *.secret …
	for _, p := range []string{".env.", "id_rsa", "id_dsa", "id_ecdsa", "id_ed25519", "secret"} {
		if strings.HasPrefix(base, p) {
			return true
		}
	}
	return strings.Contains(base, ".secret") || strings.Contains(base, "secrets.")
}

// ApplyCaps trims the prefix to its last MaxPrefixBytes and the suffix to
// its first MaxSuffixBytes, never splitting a UTF-8 rune: the text nearest
// the cursor is what a completion needs, so that is the end that survives.
func ApplyCaps(req Request) Request {
	if len(req.Prefix) > MaxPrefixBytes {
		start := len(req.Prefix) - MaxPrefixBytes
		for start < len(req.Prefix) && !utf8.RuneStart(req.Prefix[start]) {
			start++
		}
		req.Prefix = req.Prefix[start:]
	}
	if len(req.Suffix) > MaxSuffixBytes {
		end := MaxSuffixBytes
		for end > 0 && !utf8.RuneStart(req.Suffix[end]) {
			end--
		}
		req.Suffix = req.Suffix[:end]
	}
	return req
}

// Timing is the only thing the engine reports about a request: which
// provider, how long, and whether it succeeded. Never the text.
type Timing struct {
	Provider string
	Duration time.Duration
	OK       bool
}

// Guarded is a provider with the privacy rules applied. Its fields are
// unexported, so the only way to get a working one is Guard or NewProvider;
// the zero value and a nil pointer are disabled and send nothing.
type Guarded struct {
	enabled  bool
	inner    Provider
	onTiming func(Timing)
}

// Guard wraps p so every call is refused when disabled, refused for a
// deny-listed path, and capped before p sees it. onTiming may be nil.
func Guard(c Config, p Provider, onTiming func(Timing)) *Guarded {
	if g, ok := p.(*Guarded); ok && g != nil {
		// Unwrap so a re-guard cannot switch an already-disabled guard on.
		p = g.inner
		c.Enabled = c.Enabled && g.enabled
	}
	return &Guarded{enabled: c.Enabled, inner: p, onTiming: onTiming}
}

// Name reports the wrapped provider's name.
func (g *Guarded) Name() string {
	if g == nil || g.inner == nil {
		return ""
	}
	return g.inner.Name()
}

// Complete enforces the rules, then delegates and reports the timing.
func (g *Guarded) Complete(ctx context.Context, req Request) (Suggestion, error) {
	if g == nil || !g.enabled || g.inner == nil {
		return Suggestion{}, ErrDisabled
	}
	if IsDenied(req.Path) {
		return Suggestion{}, ErrDenied
	}
	start := time.Now()
	s, err := g.inner.Complete(ctx, ApplyCaps(req))
	if g.onTiming != nil {
		g.onTiming(Timing{Provider: g.inner.Name(), Duration: time.Since(start), OK: err == nil})
	}
	return s, err
}

// Result is what a Debouncer delivers: the request it answered and the
// outcome. Cancelled requests are never delivered.
type Result struct {
	Request    Request
	Suggestion Suggestion
	Err        error
}

// Debouncer turns a stream of keystrokes into at most one request at a time.
// Each Trigger restarts the delay and cancels any request already in flight;
// only the latest request's result is ever delivered.
//
// 🔴 deliver runs on a background goroutine WHILE the debouncer's lock is held:
// that is what makes "still the latest?" and the delivery one atomic step, so
// a Trigger that has returned can never be followed by a stale delivery. It
// must therefore be quick and must not call back into the Debouncer — post a
// tcell event (CLAUDE.md, "custom tcell events").
type Debouncer struct {
	Delay   time.Duration // 0 means DebounceDelay
	p       *Guarded
	deliver func(Result)

	mu     sync.Mutex
	timer  *time.Timer
	cancel context.CancelFunc
	gen    uint64
	closed bool

	// beforeDeliver, when a test sets it, runs between a request completing
	// and the delivery check. Per debouncer, so a test's hook can never be seen
	// by another test's in-flight request.
	beforeDeliver func()
}

// NewDebouncer builds a debouncer calling p and handing each surviving result
// to deliver. A nil p is disabled, so every request fails with ErrDisabled.
// A nil *Debouncer is inert: its methods do nothing.
func NewDebouncer(p *Guarded, deliver func(Result)) *Debouncer {
	return &Debouncer{p: p, deliver: deliver}
}

// Trigger records a keystroke: cancel what is pending or in flight and
// schedule req to fire after the delay.
func (d *Debouncer) Trigger(req Request) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	d.stopLocked()
	d.gen++
	gen := d.gen
	delay := d.Delay
	if delay <= 0 {
		delay = DebounceDelay
	}
	d.timer = time.AfterFunc(delay, func() { d.fire(gen, req) })
}

// Cancel drops whatever is pending or in flight without scheduling anything,
// for when the cursor leaves the buffer or the ghost text is dismissed.
func (d *Debouncer) Cancel() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stopLocked()
	d.gen++
}

// Close cancels everything and makes later Triggers no-ops.
func (d *Debouncer) Close() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stopLocked()
	d.gen++
	d.closed = true
}

// stopLocked stops the pending timer and cancels the in-flight request.
func (d *Debouncer) stopLocked() {
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
	if d.cancel != nil {
		d.cancel()
		d.cancel = nil
	}
}

// fire runs one request if it is still the latest, and delivers its result
// only if nothing superseded it while it ran.
func (d *Debouncer) fire(gen uint64, req Request) {
	d.mu.Lock()
	if gen != d.gen || d.closed {
		d.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	d.mu.Unlock()

	s, err := Suggestion{}, error(ErrDisabled)
	if d.p != nil {
		s, err = d.p.Complete(ctx, req)
	}
	cancel()
	if d.beforeDeliver != nil {
		d.beforeDeliver()
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if gen != d.gen || d.closed {
		return
	}
	d.cancel = nil
	if d.deliver != nil {
		d.deliver(Result{Request: req, Suggestion: s, Err: err})
	}
}
