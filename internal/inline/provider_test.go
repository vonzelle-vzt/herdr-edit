// =============================================================================
// File: internal/inline/provider_test.go
// Author: Vonzelle Brown
// Created: 2026-10-14
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package inline

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// enabledCfg is an on, local-default config for tests.
var enabledCfg = Config{Enabled: true}

// collector gathers Debouncer results safely across goroutines.
type collector struct {
	mu  sync.Mutex
	got []Result
}

// add records one delivered result.
func (c *collector) add(r Result) {
	c.mu.Lock()
	c.got = append(c.got, r)
	c.mu.Unlock()
}

// results returns a snapshot of everything delivered so far.
func (c *collector) results() []Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Result(nil), c.got...)
}

// TestZeroConfigIsOff pins "off by default": the zero Config builds nothing.
func TestZeroConfigIsOff(t *testing.T) {
	var c Config
	if _, err := NewProvider(c, "/ws", nil, nil); !errors.Is(err, ErrDisabled) {
		t.Fatalf("NewProvider(zero) err = %v, want ErrDisabled", err)
	}
}

// TestDisabledConfigSendsNothing proves disabled means zero bytes leave the
// editor: the fake Ollama must see no request at all, through the debouncer.
func TestDisabledConfigSendsNothing(t *testing.T) {
	f := newFakeOllama(t, "x")
	var c collector
	d := NewDebouncer(Guard(Config{Enabled: false}, f.provider(t), nil), c.add)
	defer d.Close()
	d.Delay = 5 * time.Millisecond
	d.Trigger(Request{Prefix: "abc", Path: "main.go"})
	waitFor(t, func() bool { return len(c.results()) == 1 })
	if r := c.results()[0]; !errors.Is(r.Err, ErrDisabled) {
		t.Errorf("err = %v, want ErrDisabled", r.Err)
	}
	if n := f.hits.Load(); n != 0 {
		t.Fatalf("disabled config sent %d request(s)", n)
	}
}

// TestDenyListedPathsNeverSend proves a secret file's text never reaches the
// provider, while an ordinary file beside it does.
func TestDenyListedPathsNeverSend(t *testing.T) {
	f := newFakeOllama(t, "x")
	g := Guard(enabledCfg, f.provider(t), nil)
	for _, p := range []string{".env", "app/.env.local", "certs/server.pem", "/home/u/.ssh/config", "id_ed25519.pub", "config/secrets.yaml", "prod.tfvars"} {
		if _, err := g.Complete(context.Background(), Request{Prefix: "API_KEY=", Path: p}); !errors.Is(err, ErrDenied) {
			t.Errorf("%s: err = %v, want ErrDenied", p, err)
		}
	}
	if n := f.hits.Load(); n != 0 {
		t.Fatalf("deny-listed paths sent %d request(s)", n)
	}
	if _, err := g.Complete(context.Background(), Request{Prefix: "x", Path: "internal/app/env.go"}); err != nil {
		t.Fatalf("ordinary file refused: %v", err)
	}
	if f.hits.Load() != 1 {
		t.Fatal("ordinary file was not sent")
	}
}

// TestIsDeniedTable pins the deny-list boundary in both directions, including
// case variants that a naive exact match would let through.
func TestIsDeniedTable(t *testing.T) {
	denied := []string{".env", ".ENV", ".env.production", "a/b/.npmrc", ".aws/credentials", "~/.gnupg/pubring.kbx",
		"key.KEY", "store.p12", "vault.kdbx", "id_rsa", ".git-credentials", "db.secret.json", "terraform.tfstate"}
	allowed := []string{"", "main.go", "env.go", "README.md", "internal/app/keys.go", "environment.ts", "docs/key-bindings.md"}
	for _, p := range denied {
		if !IsDenied(p) {
			t.Errorf("IsDenied(%q) = false", p)
		}
	}
	for _, p := range allowed {
		if IsDenied(p) {
			t.Errorf("IsDenied(%q) = true", p)
		}
	}
}

// TestCapsAreAppliedOnTheWire proves the provider receives at most 4 KB of
// prefix and 1 KB of suffix, keeping the text nearest the cursor.
func TestCapsAreAppliedOnTheWire(t *testing.T) {
	f := newFakeOllama(t, "x")
	prefix := strings.Repeat("p", 10000) + "NEAR"
	suffix := "NEXT" + strings.Repeat("s", 5000)
	if _, err := Guard(enabledCfg, f.provider(t), nil).Complete(context.Background(), Request{Prefix: prefix, Suffix: suffix, Path: "a.go"}); err != nil {
		t.Fatal(err)
	}
	b := f.lastBody(t)
	if len(b.Prompt) != MaxPrefixBytes || !strings.HasSuffix(b.Prompt, "NEAR") {
		t.Errorf("prompt len %d, tail %q", len(b.Prompt), b.Prompt[len(b.Prompt)-4:])
	}
	if len(b.Suffix) != MaxSuffixBytes || !strings.HasPrefix(b.Suffix, "NEXT") {
		t.Errorf("suffix len %d", len(b.Suffix))
	}
}

// TestApplyCapsNeverSplitsARune checks the caps cut on rune boundaries, so a
// line of CJK or emoji never sends invalid UTF-8.
func TestApplyCapsNeverSplitsARune(t *testing.T) {
	r := ApplyCaps(Request{Prefix: strings.Repeat("界", 3000), Suffix: strings.Repeat("😀", 600)})
	if !utf8.ValidString(r.Prefix) || len(r.Prefix) > MaxPrefixBytes || len(r.Prefix) < MaxPrefixBytes-3 {
		t.Errorf("prefix len %d valid %v", len(r.Prefix), utf8.ValidString(r.Prefix))
	}
	if !utf8.ValidString(r.Suffix) || len(r.Suffix) > MaxSuffixBytes || len(r.Suffix) < MaxSuffixBytes-3 {
		t.Errorf("suffix len %d valid %v", len(r.Suffix), utf8.ValidString(r.Suffix))
	}
	small := Request{Prefix: "a", Suffix: "b", Path: "p", Language: "go"}
	if ApplyCaps(small) != small {
		t.Error("small request was altered")
	}
}

// TestTimingCarriesNoText pins "nothing logged beyond timings": the only
// report type has no field that could hold buffer text.
func TestTimingCarriesNoText(t *testing.T) {
	f := newFakeOllama(t, "x")
	var got []Timing
	g := Guard(enabledCfg, f.provider(t), func(tm Timing) { got = append(got, tm) })
	if _, err := g.Complete(context.Background(), Request{Prefix: "abc", Path: "a.go"}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Provider != ProviderOllama || !got[0].OK || got[0].Duration <= 0 {
		t.Fatalf("timings = %+v", got)
	}
	typ := reflect.TypeOf(Timing{})
	for i := 0; i < typ.NumField(); i++ {
		if n := typ.Field(i).Name; n != "Provider" && n != "Duration" && n != "OK" {
			t.Errorf("Timing grew field %s; it must not carry request content", n)
		}
	}
}

// TestDebounceCoalescesBursts proves a burst of keystrokes produces exactly
// one request, for the latest text, against a real HTTP fake.
func TestDebounceCoalescesBursts(t *testing.T) {
	f := newFakeOllama(t, "s")
	var c collector
	d := NewDebouncer(Guard(enabledCfg, f.provider(t), nil), c.add)
	defer d.Close()
	// A wide window and no sleeps: the burst lands well inside it however
	// loaded the machine is, so exactly one request may fire.
	d.Delay = 500 * time.Millisecond
	for _, p := range []string{"f", "fu", "fun", "func"} {
		d.Trigger(Request{Prefix: p, Path: "a.go"})
	}
	waitFor(t, func() bool { return len(c.results()) == 1 })
	time.Sleep(100 * time.Millisecond)
	if n := f.hits.Load(); n != 1 {
		t.Fatalf("burst sent %d requests, want 1", n)
	}
	if r := c.results(); len(r) != 1 || r[0].Request.Prefix != "func" || r[0].Suggestion.Text != "s<unc>" {
		t.Fatalf("results = %+v", r)
	}
}

// TestDefaultDebounceIs250ms pins the contract's delay, so a zero Delay
// means 250 ms and not "fire immediately".
func TestDefaultDebounceIs250ms(t *testing.T) {
	if DebounceDelay != 250*time.Millisecond {
		t.Fatalf("DebounceDelay = %v", DebounceDelay)
	}
	f := newFakeOllama(t, "s")
	d := NewDebouncer(Guard(enabledCfg, f.provider(t), nil), func(Result) {})
	defer d.Close()
	d.Trigger(Request{Prefix: "a", Path: "a.go"})
	time.Sleep(150 * time.Millisecond)
	if f.hits.Load() != 0 {
		t.Fatal("request fired before 250ms")
	}
	waitFor(t, func() bool { return f.hits.Load() == 1 })
}

// TestNewKeystrokeCancelsInFlight proves a keystroke during a slow request
// aborts that HTTP request on the server and never delivers its stale answer.
func TestNewKeystrokeCancelsInFlight(t *testing.T) {
	f := newFakeOllama(t, "s")
	f.block.Store(true)
	var c collector
	d := NewDebouncer(Guard(enabledCfg, f.provider(t), nil), c.add)
	defer d.Close()
	d.Delay = 5 * time.Millisecond
	d.Trigger(Request{Prefix: "old", Path: "a.go"})
	// Wait until the handler is parked, not just entered: clearing block before
	// it reaches the check would let the old request answer instead of blocking.
	waitFor(t, func() bool { return f.parked.Load() == 1 })

	f.block.Store(false)
	d.Trigger(Request{Prefix: "new", Path: "a.go"})
	select {
	case <-f.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight request was not aborted by the new keystroke")
	}
	waitFor(t, func() bool { return len(c.results()) == 1 })
	time.Sleep(30 * time.Millisecond)
	r := c.results()
	if len(r) != 1 || r[0].Request.Prefix != "new" || r[0].Err != nil {
		t.Fatalf("results = %+v, want only the new request", r)
	}
}

// TestCancelAndCloseDropPendingWork proves Cancel stops a scheduled request
// and Close makes later keystrokes inert.
func TestCancelAndCloseDropPendingWork(t *testing.T) {
	f := newFakeOllama(t, "s")
	d := NewDebouncer(Guard(enabledCfg, f.provider(t), nil), func(Result) {})
	defer d.Close()
	d.Delay = 10 * time.Millisecond
	d.Trigger(Request{Prefix: "a", Path: "a.go"})
	d.Cancel()
	d.Close()
	d.Trigger(Request{Prefix: "b", Path: "a.go"})
	time.Sleep(60 * time.Millisecond)
	if n := f.hits.Load(); n != 0 {
		t.Fatalf("sent %d request(s) after Cancel/Close", n)
	}
}

// TestOllamaIsTheEnabledDefault pins item 2: once enabled, an empty provider
// name selects the local Ollama client.
func TestOllamaIsTheEnabledDefault(t *testing.T) {
	p, err := NewProvider(Config{Enabled: true, Ollama: OllamaConfig{Model: "m"}}, "/ws", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != ProviderOllama {
		t.Fatalf("default provider = %s", p.Name())
	}
}

// TestRemoteProvidersAreOptInPerWorkspace proves selecting a remote kind is
// not enough: the workspace must be listed, and a key must come from lookup.
func TestRemoteProvidersAreOptInPerWorkspace(t *testing.T) {
	c := Config{Enabled: true, Provider: ProviderAnthropic, Remote: RemoteConfig{Workspaces: []string{"/work/opted"}}}
	env := func(name string) string {
		if name == "ANTHROPIC_API_KEY" {
			return "sk-test"
		}
		return ""
	}
	if _, err := NewProvider(c, "/work/other", env, nil); !errors.Is(err, ErrRemoteNotOptedIn) {
		t.Errorf("unlisted workspace err = %v", err)
	}
	if _, err := NewProvider(c, "/work/opted/", func(string) string { return "" }, nil); !errors.Is(err, ErrNoKey) {
		t.Errorf("missing key err = %v", err)
	}
	if k, err := c.RemoteKey(env); err != nil || k != "sk-test" {
		t.Errorf("RemoteKey = %q, %v", k, err)
	}
	c.Remote.KeyEnv = "MY_KEY"
	if _, err := c.RemoteKey(env); !errors.Is(err, ErrNoKey) {
		t.Errorf("custom KeyEnv should not read the default var, err = %v", err)
	}
	if _, err := NewProvider(Config{Enabled: true, Provider: "bogus"}, "/ws", env, nil); !errors.Is(err, ErrNoProvider) {
		t.Errorf("unknown provider err = %v", err)
	}
}

// TestConfigHasNoKeyField pins that an API key cannot be put in config: a
// key belongs in the keychain or the environment, never a dotfile.
func TestConfigHasNoKeyField(t *testing.T) {
	var walk func(reflect.Type)
	walk = func(typ reflect.Type) {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if n := strings.ToLower(f.Name); (strings.Contains(n, "key") && n != "keyenv") || strings.Contains(n, "token") || strings.Contains(n, "secret") {
				t.Errorf("%s.%s looks like a secret field", typ.Name(), f.Name)
			}
			if f.Type.Kind() == reflect.Struct {
				walk(f.Type)
			}
		}
	}
	walk(reflect.TypeOf(Config{}))
}

// countingProvider is an in-memory provider that records what it was sent.
type countingProvider struct {
	mu   sync.Mutex
	reqs []Request
}

// Name identifies the fake.
func (c *countingProvider) Name() string { return "counting" }

// Complete records req and echoes its prefix back as the suggestion.
func (c *countingProvider) Complete(_ context.Context, req Request) (Suggestion, error) {
	c.mu.Lock()
	c.reqs = append(c.reqs, req)
	c.mu.Unlock()
	return Suggestion{Text: req.Prefix}, nil
}

// TestEveryExportedConstructorIsGuarded drives each exported way of getting
// something that sends — NewProvider, Guard, NewDebouncer over either, and
// the zero values — at a real fake Ollama, and requires the privacy rules on
// all of them: a secret path never arrives, oversize text arrives capped, and
// a disabled config sends nothing at all.
func TestEveryExportedConstructorIsGuarded(t *testing.T) {
	f := newFakeOllama(t, "x")
	on := Config{Enabled: true, Ollama: OllamaConfig{Endpoint: f.srv.URL, Model: "m"}}
	viaNewProvider, err := NewProvider(on, "/ws", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	viaGuard := Guard(on, f.provider(t), nil)

	type sender func(p *Guarded, req Request) error
	direct := func(p *Guarded, req Request) error {
		_, err := p.Complete(context.Background(), req)
		return err
	}
	debounced := func(p *Guarded, req Request) error {
		got := make(chan Result, 1)
		d := NewDebouncer(p, func(r Result) { got <- r })
		defer d.Close()
		d.Delay = time.Millisecond
		d.Trigger(req)
		select {
		case r := <-got:
			return r.Err
		case <-time.After(2 * time.Second):
			t.Fatal("debouncer delivered nothing")
			return nil
		}
	}
	paths := []sender{direct, debounced}
	for name, p := range map[string]*Guarded{"NewProvider": viaNewProvider, "Guard": viaGuard} {
		for i, send := range paths {
			before := f.hits.Load()
			if err := send(p, Request{Prefix: "API_KEY=", Path: "deploy/.env"}); !errors.Is(err, ErrDenied) {
				t.Errorf("%s/%d .env err = %v, want ErrDenied", name, i, err)
			}
			if f.hits.Load() != before {
				t.Fatalf("%s/%d: a .env request reached the server", name, i)
			}
			big := Request{Prefix: strings.Repeat("p", 3*MaxPrefixBytes), Suffix: strings.Repeat("s", 3*MaxSuffixBytes), Path: "a.go"}
			if err := send(p, big); err != nil {
				t.Fatalf("%s/%d: %v", name, i, err)
			}
			if b := f.lastBody(t); len(b.Prompt) != MaxPrefixBytes || len(b.Suffix) != MaxSuffixBytes {
				t.Errorf("%s/%d: uncapped on the wire: prompt %d suffix %d", name, i, len(b.Prompt), len(b.Suffix))
			}
		}
	}

	before := f.hits.Load()
	off := on
	off.Enabled = false
	if p, err := NewProvider(off, "/ws", nil, nil); !errors.Is(err, ErrDisabled) || p != nil {
		t.Errorf("NewProvider(disabled) = %v, %v", p, err)
	}
	for name, p := range map[string]*Guarded{
		"Guard(disabled)":         Guard(off, f.provider(t), nil),
		"Guard(disabled, Guard)":  Guard(off, viaGuard, nil),
		"Guard(enabled, off one)": Guard(on, Guard(off, f.provider(t), nil), nil),
		"zero Guarded":            {},
		"nil Guarded":             nil,
	} {
		for i, send := range paths {
			if err := send(p, Request{Prefix: "x", Path: "a.go"}); !errors.Is(err, ErrDisabled) {
				t.Errorf("%s/%d err = %v, want ErrDisabled", name, i, err)
			}
		}
	}
	if n := f.hits.Load() - before; n != 0 {
		t.Fatalf("disabled providers sent %d request(s)", n)
	}
}

// TestNoStaleDeliveryAfterTrigger pins the debouncer's atomicity: a Trigger
// that lands after a request completed but before its result is delivered
// must suppress that result. The hook runs exactly in that window.
func TestNoStaleDeliveryAfterTrigger(t *testing.T) {
	var c collector
	d := NewDebouncer(Guard(enabledCfg, &countingProvider{}, nil), c.add)
	defer d.Close()
	d.Delay = time.Millisecond
	hooked := make(chan struct{})
	var once sync.Once
	d.beforeDeliver = func() {
		once.Do(func() {
			d.mu.Lock()
			d.Delay = time.Hour // the superseding request stays pending
			d.mu.Unlock()
			d.Trigger(Request{Prefix: "new", Path: "a.go"})
			close(hooked)
		})
	}
	d.Trigger(Request{Prefix: "old", Path: "a.go"})
	select {
	case <-hooked:
	case <-time.After(2 * time.Second):
		t.Fatal("the request never completed")
	}
	time.Sleep(20 * time.Millisecond)
	if r := c.results(); len(r) != 0 {
		t.Fatalf("delivered %+v after a newer Trigger had returned", r)
	}
}

// TestNoStaleDeliveryUnderRace hammers Trigger against instant completions
// (run with -race): no result may be delivered once a newer Trigger has
// returned, which only holds if the check and delivery are one atomic step.
func TestNoStaleDeliveryUnderRace(t *testing.T) {
	var returned atomic.Int64
	var stale atomic.Int32
	var delivered atomic.Int32
	d := NewDebouncer(Guard(enabledCfg, &countingProvider{}, nil), func(r Result) {
		delivered.Add(1)
		i, _ := strconv.Atoi(r.Request.Prefix)
		if returned.Load() > int64(i) {
			stale.Add(1)
		}
	})
	defer d.Close()
	d.Delay = time.Nanosecond
	for i := 1; i <= 2000; i++ {
		d.Trigger(Request{Prefix: strconv.Itoa(i), Path: "a.go"})
		returned.Store(int64(i))
		if i%50 == 0 {
			time.Sleep(time.Millisecond)
		}
	}
	if n := stale.Load(); n != 0 {
		t.Fatalf("%d stale result(s) delivered after a newer Trigger returned (%d delivered)", n, delivered.Load())
	}
}

// TestNilDebouncerIsDisabled pins that "nil is disabled" holds for the
// debouncer itself as well as its provider: a nil *Debouncer ignores every
// call, and NewDebouncer(nil, …) delivers ErrDisabled — neither panics.
func TestNilDebouncerIsDisabled(t *testing.T) {
	var nd *Debouncer
	nd.Trigger(Request{Prefix: "x"})
	nd.Cancel()
	nd.Close()

	got := make(chan Result, 1)
	d := NewDebouncer(nil, func(r Result) { got <- r })
	defer d.Close()
	d.Delay = time.Millisecond
	d.Trigger(Request{Prefix: "x", Path: "a.go"})
	select {
	case r := <-got:
		if !errors.Is(r.Err, ErrDisabled) {
			t.Fatalf("err = %v, want ErrDisabled", r.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("nil-provider debouncer delivered nothing")
	}
}

// TestResultCarriesTheRequestRevision pins the anchor's plumbing: the
// revision a request was built at comes back with its result.
func TestResultCarriesTheRequestRevision(t *testing.T) {
	got := make(chan Result, 1)
	d := NewDebouncer(Guard(enabledCfg, &countingProvider{}, nil), func(r Result) { got <- r })
	defer d.Close()
	d.Delay = time.Millisecond
	d.Trigger(Request{Prefix: "x", Path: "a.go", Revision: 42})
	select {
	case r := <-got:
		if r.Request.Revision != 42 {
			t.Fatalf("revision = %d, want 42", r.Request.Revision)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("nothing delivered")
	}
}
