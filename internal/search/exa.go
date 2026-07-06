package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const exaDefaultEndpoint = "https://api.exa.ai/search"

// exaDriver is the Exa neural-search driver. POST with the key in the x-api-key
// header + a JSON body {query, numResults, contents}. We request a short text
// snippet via contents.text so the normalized Snippet is populated; the snippet
// falls back to the first highlight when text is absent.
//
// ASSUMPTION: Exa returns results[] with title/url plus text (and/or
// highlights[]) when contents is requested. Validated against captured fixtures;
// the live shape is exercised by the E2E smoke, not unit tests.
type exaDriver struct{ base }

// NewExa constructs the Exa driver.
func NewExa(opts ...Option) Provider {
	d := &exaDriver{}
	for _, o := range opts {
		o(&d.base)
	}
	return d
}

func (d *exaDriver) ID() string                  { return "exa" }
func (d *exaDriver) KeyEnvName() string          { return "EXA_API_KEY" }
func (d *exaDriver) Probe(context.Context) error { return nil }

func (d *exaDriver) Search(ctx context.Context, q Query, apiKey string) (Results, error) {
	reqBody, _ := json.Marshal(map[string]any{
		"query":      q.Text,
		"numResults": q.MaxResults,
		"contents":   map[string]any{"text": map[string]any{"maxCharacters": 300}},
	})
	body, status, err := d.doJSON(ctx, "POST",
		firstNonEmpty(d.endpoint, exaDefaultEndpoint),
		map[string]string{"x-api-key": apiKey, "Content-Type": "application/json"},
		bytes.NewReader(reqBody))
	if err != nil {
		return Results{}, err
	}
	if status != 200 {
		return Results{}, fmt.Errorf("exa: status %d: %s", status, truncate(body))
	}
	var parsed struct {
		Results []struct {
			Title      string   `json:"title"`
			URL        string   `json:"url"`
			Text       string   `json:"text"`
			Highlights []string `json:"highlights"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Results{}, fmt.Errorf("exa: parse: %w", err)
	}
	out := make([]Result, 0, len(parsed.Results))
	for _, r := range parsed.Results {
		snippet := strings.TrimSpace(r.Text)
		if snippet == "" && len(r.Highlights) > 0 {
			snippet = strings.TrimSpace(r.Highlights[0])
		}
		out = append(out, Result{Title: r.Title, URL: r.URL, Snippet: snippet})
	}
	return Results{Provider: "exa", Results: out, Raw: body}, nil
}
