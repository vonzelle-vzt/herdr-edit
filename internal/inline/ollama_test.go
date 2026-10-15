// =============================================================================
// File: internal/inline/ollama_test.go
// Author: Vonzelle Brown
// Created: 2026-10-14
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package inline

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeOllama is an httptest stand-in for `ollama serve`. It records every
// request body it receives and answers with reply, or blocks until the
// client goes away when block is set.
type fakeOllama struct {
	srv       *httptest.Server
	hits      atomic.Int32
	parked    atomic.Int32  // requests now held in the blocking select
	cancelled chan struct{} // receives once per request aborted by the client
	release   chan struct{} // closed at cleanup so a blocked handler cannot wedge Close
	block     atomic.Bool
	reply     string

	mu     sync.Mutex
	bodies []ollamaRequest
}

// newFakeOllama starts a fake server answering reply; it closes on cleanup.
func newFakeOllama(t *testing.T, reply string) *fakeOllama {
	t.Helper()
	f := &fakeOllama{reply: reply, cancelled: make(chan struct{}, 16), release: make(chan struct{})}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		if r.URL.Path != "/api/generate" || r.Method != http.MethodPost {
			http.Error(w, "wrong route", http.StatusNotFound)
			return
		}
		var body ollamaRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.mu.Unlock()
		if f.block.Load() {
			f.parked.Add(1)
			select {
			case <-r.Context().Done():
				f.cancelled <- struct{}{}
			case <-f.release:
			}
			return
		}
		_ = json.NewEncoder(w).Encode(ollamaResponse{Response: f.reply + "<" + body.Prompt[max(0, len(body.Prompt)-3):] + ">"})
	}))
	t.Cleanup(f.srv.Close)
	t.Cleanup(func() { close(f.release) }) // runs first: cleanups are LIFO
	return f
}

// lastBody returns the most recent request body the fake received.
func (f *fakeOllama) lastBody(t *testing.T) ollamaRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		t.Fatal("fake ollama received no request")
	}
	return f.bodies[len(f.bodies)-1]
}

// provider builds an Ollama client pointed at the fake.
func (f *fakeOllama) provider(t *testing.T) *ollama {
	t.Helper()
	o, err := newOllama(OllamaConfig{Endpoint: f.srv.URL, Model: "coder:test"})
	if err != nil {
		t.Fatalf("NewOllama: %v", err)
	}
	return o
}

// TestOllamaSendsPrefixSuffixAndModel pins the wire shape: model from
// config, prefix as prompt, suffix for fill-in-the-middle, non-streaming.
func TestOllamaSendsPrefixSuffixAndModel(t *testing.T) {
	f := newFakeOllama(t, "ok")
	s, err := f.provider(t).Complete(context.Background(), Request{Prefix: "func main() {", Suffix: "}", Language: "go", Path: "main.go"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if s.Text != "ok<) {>" {
		t.Errorf("suggestion = %q", s.Text)
	}
	b := f.lastBody(t)
	if b.Model != "coder:test" || b.Prompt != "func main() {" || b.Suffix != "}" || b.Stream {
		t.Errorf("body = %+v", b)
	}
}

// TestOllamaRefusesNonLoopbackEndpoints proves the local default cannot be
// pointed at another machine: the privacy promise of "ollama" is localhost.
func TestOllamaRefusesNonLoopbackEndpoints(t *testing.T) {
	for _, ep := range []string{"http://example.com:11434", "http://10.0.0.5:11434", "http://[2001:db8::1]:11434", "ftp://127.0.0.1"} {
		if _, err := newOllama(OllamaConfig{Endpoint: ep, Model: "m"}); err == nil {
			t.Errorf("%s accepted", ep)
		}
	}
	for _, ep := range []string{"", "http://localhost:11434", "http://127.0.0.1:1", "http://[::1]:11434"} {
		if _, err := newOllama(OllamaConfig{Endpoint: ep, Model: "m"}); err != nil {
			t.Errorf("%q refused: %v", ep, err)
		}
	}
}

// TestOllamaRequiresAModel pins that the model comes from config: there is
// no hard-coded model name to silently fall back to.
func TestOllamaRequiresAModel(t *testing.T) {
	if _, err := newOllama(OllamaConfig{Model: "  "}); !errors.Is(err, ErrNoModel) {
		t.Fatalf("err = %v, want ErrNoModel", err)
	}
}

// TestOllamaCancellationAbortsInFlightRequest proves cancelling ctx kills the
// HTTP request itself — the server sees its client disappear — rather than
// leaving it running while the caller merely stops waiting.
func TestOllamaCancellationAbortsInFlightRequest(t *testing.T) {
	f := newFakeOllama(t, "")
	f.block.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := f.provider(t).Complete(ctx, Request{Prefix: "x"})
		done <- err
	}()
	waitFor(t, func() bool { return f.hits.Load() == 1 })
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Complete did not return after cancel")
	}
	select {
	case <-f.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("server never saw the request aborted")
	}
}

// TestOllamaDoesNotFollowRedirects guards the loopback check: a redirect to
// another host would carry the buffer there without being checked.
func TestOllamaDoesNotFollowRedirects(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { elsewhere.Add(1) }))
	defer other.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/api/generate", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()
	o, err := newOllama(OllamaConfig{Endpoint: redirector.URL, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.Complete(context.Background(), Request{Prefix: "secret-ish"}); err == nil {
		t.Error("a redirect answer was treated as a completion")
	}
	if elsewhere.Load() != 0 {
		t.Error("redirect was followed")
	}
}

// TestOllamaErrorsDoNotEchoTheBody pins "nothing logged beyond timings": an
// error body may quote the prompt, so only the status reaches the caller.
func TestOllamaErrorsDoNotEchoTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model failed on PROMPT-TEXT", http.StatusInternalServerError)
	}))
	defer srv.Close()
	o, _ := newOllama(OllamaConfig{Endpoint: srv.URL, Model: "m"})
	_, err := o.Complete(context.Background(), Request{Prefix: "PROMPT-TEXT"})
	if err == nil || strings.Contains(err.Error(), "PROMPT-TEXT") {
		t.Fatalf("err = %v", err)
	}
}

// waitFor polls cond for up to two seconds; tests use it for server-side
// observations that land on another goroutine.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 2s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// stubLookup makes every name resolve to ips for the duration of the test.
func stubLookup(t *testing.T, ips ...string) {
	t.Helper()
	orig := lookupIPAddr
	t.Cleanup(func() { lookupIPAddr = orig })
	setLookup(ips...)
}

// setLookup swaps the resolver's answer mid-test.
func setLookup(ips ...string) {
	lookupIPAddr = func(context.Context, string) ([]net.IPAddr, error) {
		out := make([]net.IPAddr, 0, len(ips))
		for _, s := range ips {
			out = append(out, net.IPAddr{IP: net.ParseIP(s)})
		}
		return out, nil
	}
}

// TestOllamaHostMustResolveToLoopback pins that a host NAME is judged by what
// it resolves to: "localhost" remapped to another machine, or a name with even
// one non-loopback address, is refused before anything is sent.
func TestOllamaHostMustResolveToLoopback(t *testing.T) {
	for _, ips := range [][]string{{"10.0.0.5"}, {"127.0.0.1", "10.0.0.5"}, {"::1", "2001:db8::1"}, {}} {
		stubLookup(t, ips...)
		for _, ep := range []string{"http://localhost:11434", "http://ollama.internal:11434"} {
			if _, err := newOllama(OllamaConfig{Endpoint: ep, Model: "m"}); !errors.Is(err, ErrNotLocal) {
				t.Errorf("%s resolving to %v: err = %v, want ErrNotLocal", ep, ips, err)
			}
		}
	}
	stubLookup(t, "127.0.0.1", "::1")
	if _, err := newOllama(OllamaConfig{Endpoint: "http://ollama.internal:11434", Model: "m"}); err != nil {
		t.Errorf("all-loopback name refused: %v", err)
	}
}

// TestOllamaDialRechecksResolution pins the rebinding guard: a name that
// resolved to loopback when the provider was built but resolves elsewhere at
// request time sends nothing.
func TestOllamaDialRechecksResolution(t *testing.T) {
	f := newFakeOllama(t, "ok")
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(f.srv.URL, "http://"))
	stubLookup(t, "127.0.0.1")
	o, err := newOllama(OllamaConfig{Endpoint: "http://ollama.internal:" + port, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.Complete(context.Background(), Request{Prefix: "a"}); err != nil {
		t.Fatalf("loopback name did not reach the fake: %v", err)
	}
	// A pooled connection is already a checked loopback socket; drop it so
	// the next request must dial, which is where the re-check lives.
	o.client.CloseIdleConnections()
	before := f.hits.Load()
	setLookup("10.0.0.5")
	if _, err := o.Complete(context.Background(), Request{Prefix: "a"}); !errors.Is(err, ErrNotLocal) {
		t.Fatalf("rebound name: err = %v, want ErrNotLocal", err)
	}
	if f.hits.Load() != before {
		t.Fatal("a request reached the server after the name rebound")
	}
}
