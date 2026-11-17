package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/memory/nodestats"
)

// DefinitionText is what a concept is represented by in the definition
// baseline: "<name>: <meaning>", then its alternative labels.
func DefinitionText(c Concept) string {
	s := c.Name
	if c.Meaning != "" {
		s += ": " + c.Meaning
	}
	if len(c.Alt) > 0 {
		s += " (also: " + strings.Join(c.Alt, ", ") + ")"
	}
	return s
}

// EmbedDefinitions embeds every concept's definition with Ollama's /api/embed,
// one request at a time. On the first failure it stops and returns nil with
// the reason: the baseline is then skipped, not partly computed.
func EmbedDefinitions(ctx context.Context, url, model string, cs []Concept) (map[string][]float64, string) {
	if url == "" {
		return nil, "definition baseline skipped: no --embed-url"
	}
	client := &http.Client{Timeout: 60 * time.Second}
	out := map[string][]float64{}
	for _, c := range cs {
		v, err := embedOne(ctx, client, url, model, DefinitionText(c))
		if err != nil {
			return nil, fmt.Sprintf("definition baseline skipped: embedder unreachable or failing (%v)", err)
		}
		u, err := unitVec(v)
		if err != nil {
			return nil, fmt.Sprintf("definition baseline skipped: concept %s: %v", c.ID, err)
		}
		out[c.ID] = u
	}
	return out, fmt.Sprintf("definition baseline: %d concepts embedded with %s", len(out), model)
}

func embedOne(ctx context.Context, client *http.Client, url, model, text string) (nodestats.Vector, error) {
	body, err := json.Marshal(map[string]any{"model": model, "input": text})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var r struct {
		Embeddings []nodestats.Vector `json:"embeddings"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	if len(r.Embeddings) != 1 || len(r.Embeddings[0]) == 0 {
		return nil, fmt.Errorf("got %d embeddings", len(r.Embeddings))
	}
	return r.Embeddings[0], nil
}
