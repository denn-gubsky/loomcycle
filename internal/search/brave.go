package search

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

const braveDefaultEndpoint = "https://api.search.brave.com/res/v1/web/search"

// braveDriver is the Brave Search API driver (lifted from the pre-RFC-BB
// WebSearch). GET with the key in the X-Subscription-Token header; results under
// web.results[]. Titles/descriptions carry <strong> highlight tags — the
// WebSearch render path strips them, so the normalized Snippet keeps them raw.
type braveDriver struct{ base }

// NewBrave constructs the Brave driver.
func NewBrave(opts ...Option) Provider {
	d := &braveDriver{}
	for _, o := range opts {
		o(&d.base)
	}
	return d
}

func (d *braveDriver) ID() string                  { return "brave" }
func (d *braveDriver) KeyEnvName() string          { return "BRAVE_API_KEY" }
func (d *braveDriver) Probe(context.Context) error { return nil }

func (d *braveDriver) Search(ctx context.Context, q Query, apiKey string) (Results, error) {
	v := url.Values{}
	v.Set("q", q.Text)
	v.Set("count", fmt.Sprint(q.MaxResults))
	body, status, err := d.doJSON(ctx, "GET",
		firstNonEmpty(d.endpoint, braveDefaultEndpoint)+"?"+v.Encode(),
		map[string]string{"Accept": "application/json", "X-Subscription-Token": apiKey}, nil)
	if err != nil {
		return Results{}, err
	}
	if status != 200 {
		return Results{}, fmt.Errorf("brave: status %d: %s", status, truncate(body))
	}
	var parsed struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Results{}, fmt.Errorf("brave: parse: %w", err)
	}
	out := make([]Result, 0, len(parsed.Web.Results))
	for _, r := range parsed.Web.Results {
		out = append(out, Result{Title: r.Title, URL: r.URL, Snippet: r.Description})
	}
	return Results{Provider: "brave", Results: out, Raw: body}, nil
}
