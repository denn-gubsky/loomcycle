package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
)

const serperDefaultEndpoint = "https://google.serper.dev/search"

// serperDriver is the Serper.dev driver — cheap Google SERP JSON. POST with the
// key in the X-API-KEY header + a JSON body {q, num}; organic results under
// organic[] with the URL in the "link" field.
type serperDriver struct{ base }

// NewSerper constructs the Serper driver.
func NewSerper(opts ...Option) Provider {
	d := &serperDriver{}
	for _, o := range opts {
		o(&d.base)
	}
	return d
}

func (d *serperDriver) ID() string                  { return "serper" }
func (d *serperDriver) KeyEnvName() string          { return "SERPER_API_KEY" }
func (d *serperDriver) Probe(context.Context) error { return nil }

func (d *serperDriver) Search(ctx context.Context, q Query, apiKey string) (Results, error) {
	reqBody, _ := json.Marshal(map[string]any{"q": q.Text, "num": q.MaxResults})
	body, status, err := d.doJSON(ctx, "POST",
		firstNonEmpty(d.endpoint, serperDefaultEndpoint),
		map[string]string{"X-API-KEY": apiKey, "Content-Type": "application/json"},
		bytes.NewReader(reqBody))
	if err != nil {
		return Results{}, err
	}
	if status != 200 {
		return Results{}, fmt.Errorf("serper: status %d: %s", status, truncate(body))
	}
	var parsed struct {
		Organic []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Snippet string `json:"snippet"`
		} `json:"organic"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Results{}, fmt.Errorf("serper: parse: %w", err)
	}
	out := make([]Result, 0, len(parsed.Organic))
	for _, r := range parsed.Organic {
		out = append(out, Result{Title: r.Title, URL: r.Link, Snippet: r.Snippet})
	}
	return Results{Provider: "serper", Results: out, Raw: body}, nil
}
