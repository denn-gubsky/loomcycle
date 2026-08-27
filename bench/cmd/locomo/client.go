package main

// client.go — the thin loomcycle HTTP client this harness needs.
//
// It drives an EXTERNALLY-RUNNING loomcycle, the same posture as lc-bench: the
// point is to measure the real vector store and the real configured embedder,
// not a harness substitute for them.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to one loomcycle instance as one principal.
type Client struct {
	base   string
	bearer string
	httpc  *http.Client
}

// NewClient builds a client. The bearer determines the tenant every write and
// read lands in — the memory routes derive the tenant from the PRINCIPAL and
// honour no ?tenant= override, so a dedicated tenant token is the only way to
// isolate this corpus from real memory.
func NewClient(base, bearer string, timeout time.Duration) *Client {
	return &Client{
		base:   strings.TrimRight(base, "/"),
		bearer: bearer,
		httpc:  &http.Client{Timeout: timeout},
	}
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	// Bound the error body: an HTML error page from a proxy in front of
	// loomcycle would otherwise land whole in a log line.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, truncate(strings.TrimSpace(string(raw)), 300))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s %s: decode response: %w", method, path, err)
	}
	return nil
}

// Identity is the subset of GET /v1/_me this harness checks.
type Identity struct {
	TenantID string   `json:"tenant_id"`
	Subject  string   `json:"subject"`
	Scopes   []string `json:"scopes"`
	IsAdmin  bool     `json:"is_admin"`
}

// Whoami resolves the principal behind the bearer. Called before any write, so
// the operator sees which tenant is about to receive ~6k rows.
func (c *Client) Whoami(ctx context.Context) (Identity, error) {
	var id Identity
	err := c.do(ctx, http.MethodGet, "/v1/_me", nil, &id)
	return id, err
}

type putEntryBody struct {
	Value json.RawMessage `json:"value"`
	Embed bool            `json:"embed,omitempty"`
}

type putEntryResponse struct {
	Embedded     bool   `json:"embedded"`
	EmbedWarning string `json:"embed_warning,omitempty"`
}

// PutEntry upserts one memory row and (when embed is set) embeds it
// synchronously on the instance's configured embedder.
//
// The row's embedded text is the JSON-encoded value — the endpoint has no
// embed_text field — so a string value embeds with its surrounding quotes.
// Two quote characters against a couple of hundred characters of turn text is
// noise for a dense embedder, but it IS an asymmetry with the un-quoted query
// and the report says so rather than leaving it unstated.
func (c *Client) PutEntry(ctx context.Context, scope, scopeID, key, value string, embed bool) (putEntryResponse, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return putEntryResponse{}, err
	}
	path := fmt.Sprintf("/v1/_memory/scopes/%s/%s/keys/%s",
		url.PathEscape(scope), url.PathEscape(scopeID), url.PathEscape(key))
	var out putEntryResponse
	err = c.do(ctx, http.MethodPut, path, putEntryBody{Value: raw, Embed: embed}, &out)
	return out, err
}

type searchRequest struct {
	Query   string `json:"query"`
	Scope   string `json:"scope"`
	ScopeID string `json:"scope_id"`
	TopK    int    `json:"top_k"`
}

type searchResponse struct {
	Entries []struct {
		Key       string  `json:"key"`
		Score     float64 `json:"score"`
		RankScore float64 `json:"rank_score"`
		Kind      string  `json:"kind"`
	} `json:"entries"`
	QueryEmbeddingDim int  `json:"query_embedding_dim"`
	Truncated         bool `json:"truncated"`
}

// Search runs one semantic query and returns the retrieved keys in rank order.
func (c *Client) Search(ctx context.Context, scope, scopeID, query string, topK int) ([]string, int, error) {
	var out searchResponse
	err := c.do(ctx, http.MethodPost, "/v1/_memory/search", searchRequest{
		Query: query, Scope: scope, ScopeID: scopeID, TopK: topK,
	}, &out)
	if err != nil {
		return nil, 0, err
	}
	keys := make([]string, 0, len(out.Entries))
	for _, e := range out.Entries {
		keys = append(keys, e.Key)
	}
	return keys, out.QueryEmbeddingDim, nil
}

// DeleteEntry removes one row — used by -mode=purge to reclaim a scope.
func (c *Client) DeleteEntry(ctx context.Context, scope, scopeID, key string) error {
	path := fmt.Sprintf("/v1/_memory/scopes/%s/%s/keys/%s",
		url.PathEscape(scope), url.PathEscape(scopeID), url.PathEscape(key))
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}
