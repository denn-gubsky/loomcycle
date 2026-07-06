package search

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// searxngDriver is the self-hosted SearXNG meta-search driver — KEYLESS. GET
// {base}/search?q=&format=json; results under results[] with the snippet in the
// "content" field. Because it's operator-hosted (no paid query), it implements a
// real Probe against {base}/healthz for the routing view's availability.
type searxngDriver struct {
	base
	baseURL string // e.g. http://searxng:8080 (no trailing slash)
}

// NewSearXNG constructs the SearXNG driver against the operator's base URL.
func NewSearXNG(baseURL string, opts ...Option) Provider {
	d := &searxngDriver{baseURL: strings.TrimRight(baseURL, "/")}
	for _, o := range opts {
		o(&d.base)
	}
	return d
}

func (d *searxngDriver) ID() string         { return "searxng" }
func (d *searxngDriver) KeyEnvName() string { return "" } // keyless

func (d *searxngDriver) Probe(ctx context.Context) error {
	_, status, err := d.doJSON(ctx, "GET", d.baseURL+"/healthz", nil, nil)
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("searxng: healthz status %d", status)
	}
	return nil
}

func (d *searxngDriver) Search(ctx context.Context, q Query, _ string) (Results, error) {
	v := url.Values{}
	v.Set("q", q.Text)
	v.Set("format", "json")
	body, status, err := d.doJSON(ctx, "GET",
		firstNonEmpty(d.endpoint, d.baseURL+"/search")+"?"+v.Encode(),
		map[string]string{"Accept": "application/json"}, nil)
	if err != nil {
		return Results{}, err
	}
	if status != 200 {
		return Results{}, fmt.Errorf("searxng: status %d: %s", status, truncate(body))
	}
	var parsed struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Results{}, fmt.Errorf("searxng: parse: %w", err)
	}
	out := make([]Result, 0, len(parsed.Results))
	for _, r := range parsed.Results {
		out = append(out, Result{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return Results{Provider: "searxng", Results: out, Raw: body}, nil
}
