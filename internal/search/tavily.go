package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
)

const tavilyDefaultEndpoint = "https://api.tavily.com/search"

// tavilyDriver is the Tavily driver — a RAG-purpose-built search API. POST with
// the key as a Bearer token + a JSON body {query, max_results}; results under
// results[] with the snippet in the "content" field.
type tavilyDriver struct{ base }

// NewTavily constructs the Tavily driver.
func NewTavily(opts ...Option) Provider {
	d := &tavilyDriver{}
	for _, o := range opts {
		o(&d.base)
	}
	return d
}

func (d *tavilyDriver) ID() string                  { return "tavily" }
func (d *tavilyDriver) KeyEnvName() string          { return "TAVILY_API_KEY" }
func (d *tavilyDriver) Probe(context.Context) error { return nil }

func (d *tavilyDriver) Search(ctx context.Context, q Query, apiKey string) (Results, error) {
	reqBody, _ := json.Marshal(map[string]any{"query": q.Text, "max_results": q.MaxResults})
	body, status, err := d.doJSON(ctx, "POST",
		firstNonEmpty(d.endpoint, tavilyDefaultEndpoint),
		map[string]string{"Authorization": "Bearer " + apiKey, "Content-Type": "application/json"},
		bytes.NewReader(reqBody))
	if err != nil {
		return Results{}, err
	}
	if status != 200 {
		return Results{}, fmt.Errorf("tavily: status %d: %s", status, truncate(body))
	}
	var parsed struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Results{}, fmt.Errorf("tavily: parse: %w", err)
	}
	out := make([]Result, 0, len(parsed.Results))
	for _, r := range parsed.Results {
		out = append(out, Result{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return Results{Provider: "tavily", Results: out, Raw: body}, nil
}
