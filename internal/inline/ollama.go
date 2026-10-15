// =============================================================================
// File: internal/inline/ollama.go
// Author: Vonzelle Brown
// Created: 2026-10-14
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package inline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultOllamaEndpoint is where a stock `ollama serve` listens.
const DefaultOllamaEndpoint = "http://127.0.0.1:11434"

// maxOllamaResponse bounds how much of a response body is read; a completion
// is a few lines, so anything bigger is a misbehaving server.
const maxOllamaResponse = 256 * 1024

// ollamaMaxTokens bounds the generated completion length.
const ollamaMaxTokens = 128

var (
	// ErrNotLocal is returned for an Ollama endpoint that is not loopback.
	ErrNotLocal = errors.New("inline: ollama endpoint must be localhost")
	// ErrNoModel is returned when no Ollama model is configured.
	ErrNoModel = errors.New("inline: no ollama model configured")
)

// OllamaConfig configures the local provider.
type OllamaConfig struct {
	Endpoint string // "" means DefaultOllamaEndpoint
	Model    string // required, e.g. "qwen2.5-coder:1.5b"
}

// ollama is the local-only default provider, talking to /api/generate. It is
// unexported so it can only be reached through NewProvider's guard.
type ollama struct {
	endpoint *url.URL
	model    string
	client   *http.Client
}

// newOllama validates cfg and builds the provider. The endpoint must be a
// loopback address: the local default is the whole privacy promise of
// choosing it, so a remote host here is refused rather than honoured.
func newOllama(cfg OllamaConfig) (*ollama, error) {
	raw := cfg.Endpoint
	if raw == "" {
		raw = DefaultOllamaEndpoint
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("inline: bad ollama endpoint %q", raw)
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, ErrNoModel
	}
	ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
	defer cancel()
	if _, err := loopbackAddrs(ctx, u.Hostname()); err != nil {
		return nil, err
	}
	return &ollama{
		endpoint: u,
		model:    cfg.Model,
		client: &http.Client{
			// 🔴 No proxy and no redirects: either would carry the buffer
			// text to a host the loopback check never saw.
			// The dial re-resolves and re-checks, so a name that later
			// resolves elsewhere (DNS rebinding) is refused, not followed.
			Transport: &http.Transport{Proxy: nil, DialContext: loopbackDial},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// resolveTimeout bounds the construction-time lookup of the endpoint's host.
const resolveTimeout = 5 * time.Second

// lookupIPAddr resolves a host name; a test swaps it for a fixed answer.
var lookupIPAddr = net.DefaultResolver.LookupIPAddr

// loopbackAddrs resolves host and returns its addresses only when EVERY one is
// loopback (127.0.0.0/8 or ::1). A name is not trusted for what it says —
// "localhost" can be remapped in /etc/hosts — only for what it resolves to.
func loopbackAddrs(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			return nil, ErrNotLocal
		}
		return []net.IP{ip}, nil
	}
	addrs, err := lookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot resolve %q: %v", ErrNotLocal, host, err)
	}
	if len(addrs) == 0 {
		return nil, ErrNotLocal
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		if !a.IP.IsLoopback() {
			return nil, ErrNotLocal
		}
		ips = append(ips, a.IP)
	}
	return ips, nil
}

// loopbackDial connects only to an address loopbackAddrs approved, dialling
// the checked IP itself so no second lookup can substitute another host.
func loopbackDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := loopbackAddrs(ctx, host)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	for _, ip := range ips {
		var c net.Conn
		if c, err = d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port)); err == nil {
			return c, nil
		}
	}
	return nil, err
}

// Name identifies the provider in timings.
func (o *ollama) Name() string { return ProviderOllama }

// ollamaRequest is the /api/generate body. Suffix makes fill-in-the-middle
// models complete between the two halves rather than only after the prefix.
type ollamaRequest struct {
	Model   string         `json:"model"`
	Prompt  string         `json:"prompt"`
	Suffix  string         `json:"suffix,omitempty"`
	Stream  bool           `json:"stream"`
	Options map[string]any `json:"options,omitempty"`
}

// ollamaResponse is the non-streaming /api/generate answer.
type ollamaResponse struct {
	Response string `json:"response"`
	Error    string `json:"error"`
}

// Complete asks Ollama for a completion. ctx cancellation aborts the HTTP
// request itself, so a superseded keystroke stops the work, not just the wait.
func (o *ollama) Complete(ctx context.Context, req Request) (Suggestion, error) {
	body, err := json.Marshal(ollamaRequest{
		Model:   o.model,
		Prompt:  req.Prefix,
		Suffix:  req.Suffix,
		Stream:  false,
		Options: map[string]any{"num_predict": ollamaMaxTokens},
	})
	if err != nil {
		return Suggestion{}, err
	}
	endpoint := o.endpoint.JoinPath("api", "generate")
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return Suggestion{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := o.client.Do(httpReq)
	if err != nil {
		return Suggestion{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxOllamaResponse))
	if err != nil {
		return Suggestion{}, err
	}
	var out ollamaResponse
	if resp.StatusCode != http.StatusOK {
		// Report the status only: an error body could echo the prompt back.
		return Suggestion{}, fmt.Errorf("inline: ollama returned HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return Suggestion{}, fmt.Errorf("inline: bad ollama response: %w", err)
	}
	if out.Error != "" {
		return Suggestion{}, errors.New("inline: ollama reported an error")
	}
	return Suggestion{Text: out.Response}, nil
}
