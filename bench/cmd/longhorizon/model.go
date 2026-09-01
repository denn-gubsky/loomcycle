package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// model.go — drives the model per step through loomcycle's OpenAI-compatible
// gateway (POST /v1/chat/completions), so every arm rides the SAME provider
// routing and the RFC AV cost ledger. Token counts come from the response `usage`
// block (provider-reported), which is the truth this benchmark measures.

type ModelClient struct {
	base   string
	bearer string
	model  string
	httpc  *http.Client
}

func NewModelClient(base, bearer, model string, timeout time.Duration) *ModelClient {
	return &ModelClient{base: base, bearer: bearer, model: model,
		httpc: &http.Client{Timeout: timeout}}
}

type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature"`
}

type chatResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// Usage is the per-call token accounting.
type Usage struct {
	Prompt     int
	Completion int
}

// Call sends one chat completion (temperature 0 for determinism) and returns the
// content plus provider-reported token usage.
func (c *ModelClient) Call(ctx context.Context, messages []Message) (string, Usage, error) {
	body, _ := json.Marshal(chatRequest{Model: c.model, Messages: messages, Temperature: 0})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", Usage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return "", Usage{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", Usage{}, fmt.Errorf("chat/completions: %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var out chatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", Usage{}, fmt.Errorf("decode: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", Usage{}, fmt.Errorf("no choices in response")
	}
	return out.Choices[0].Message.Content,
		Usage{Prompt: out.Usage.PromptTokens, Completion: out.Usage.CompletionTokens}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
