// Package search is loomcycle's first-class, config-declared catalog of
// web-search providers behind one interface — the search analog of
// internal/providers (RFC BB). Each driver normalizes its provider's JSON to a
// common []Result shape so the WebSearch tool can fall over from one provider to
// the next transparently, with a per-agent priority list and a routing view.
//
// Design boundary: drivers are PURE HTTP clients. They take an already-resolved
// apiKey and never touch credential/ctx machinery — key resolution, the RFC AR
// tenant-override, and the RFC AX operator-key restriction are the WebSearch
// tool's job. That keeps this package free of internal/providers + internal/tools
// imports and makes each driver testable with a plain key + a fake HTTP client.
package search

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
)

// maxResponseBytes caps a provider response the same way the legacy WebSearch
// did (1 MiB) — a runaway body can never blow the process or the context window.
const maxResponseBytes = 1 << 20

// Query is a normalized search request. Host narrowing + result rendering are
// the WebSearch tool's concern (applied to the normalized Results, provider-
// agnostic), NOT a driver's — so every driver stays a thin "text → SERP" client.
type Query struct {
	Text       string
	MaxResults int
}

// Result is one normalized hit — the lowest-common-denominator "web discovery"
// shape (title/url/snippet) every provider maps onto, so the fallback circuit
// can swap providers without the agent seeing a different shape (RFC BB). A
// provider's richer output rides Results.Raw, out of the fallback circuit.
type Result struct {
	Title   string
	URL     string
	Snippet string
}

// Results is a provider's normalized response.
type Results struct {
	Provider string
	Results  []Result
	Raw      json.RawMessage // the provider's original body, for future power-ops
}

// Provider is one search backend (brave/serper/exa/tavily/searxng). Mirrors the
// shape of providers.Provider but search-flavored.
type Provider interface {
	ID() string
	// Search runs the query with the already-resolved apiKey ("" for keyless
	// providers like SearXNG). Key resolution + the operator-key restriction
	// are the caller's job — the driver is a pure HTTP client.
	Search(ctx context.Context, q Query, apiKey string) (Results, error)
	// KeyEnvName is the well-known env-var name the provider's key resolves
	// from (e.g. "SERPER_API_KEY"); "" for a keyless provider. Used by the
	// WebSearch tool + routing view to test tenant/operator keyability without
	// spending a paid query.
	KeyEnvName() string
	// Probe is a cheap reachability check. Most paid search APIs have no free
	// probe, so their Probe is a no-op returning nil (availability is tracked
	// by last-outcome instead); SearXNG implements a real /healthz probe.
	Probe(ctx context.Context) error
}

// base carries the test-injectable endpoint + client shared by every driver.
type base struct {
	endpoint string       // "" → the driver's default
	client   *http.Client // nil → http.DefaultClient (relies on the ctx deadline)
}

// Option customizes a driver at construction — the seam tests use to point a
// driver at a fake server + client.
type Option func(*base)

// WithEndpoint overrides a driver's default upstream URL (tests).
func WithEndpoint(url string) Option { return func(b *base) { b.endpoint = url } }

// WithHTTPClient injects an *http.Client (tests control dialing).
func WithHTTPClient(c *http.Client) Option { return func(b *base) { b.client = c } }

func (b base) httpClient() *http.Client {
	if b.client != nil {
		return b.client
	}
	return http.DefaultClient
}

// doJSON issues one request and returns the (byte-capped) body + status code.
// The caller sets the ctx deadline; drivers don't own timeout policy.
func (b base) doJSON(ctx context.Context, method, url string, headers map[string]string, body io.Reader) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := b.httpClient().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return data, resp.StatusCode, nil
}

// firstNonEmpty returns a if non-empty, else b — used for endpoint defaulting.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// truncate bounds an upstream error body so a driver's error stays log-friendly.
func truncate(b []byte) string {
	const n = 200
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
}
