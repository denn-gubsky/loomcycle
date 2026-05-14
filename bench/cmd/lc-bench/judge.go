package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/denn-gubsky/loomcycle/bench/internal/grader"
)

// anthropicJudge is the production Judge. Calls Anthropic's
// /v1/messages directly (no loomcycle round-trip — keeps the judge
// independent of the system under test). Reads ANTHROPIC_API_KEY at
// construction time and pins to claude-sonnet-4-6 with temperature=0
// for repeatable grading.
type anthropicJudge struct {
	apiKey string
	model  string
	httpc  *http.Client
}

func newAnthropicJudge() grader.Judge {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		// No key = semantic axis becomes pass-through. The grader
		// surfaces a note so the matrix shows the operator that
		// semantic was skipped.
		return nil
	}
	model := os.Getenv("LOOMCYCLE_BENCH_JUDGE_MODEL")
	if model == "" {
		model = "claude-sonnet-4-6"
	}
	return &anthropicJudge{
		apiKey: key,
		model:  model,
		httpc: &http.Client{
			Timeout: 90 * time.Second,
		},
	}
}

// Score runs the rubric prompt through the judge model and parses
// the {score, notes} JSON reply.
func (j *anthropicJudge) Score(ctx context.Context, prompt string) (int, string, error) {
	body := map[string]any{
		"model":      j.model,
		"max_tokens": 512,
		"temperature": 0,
		"messages": []map[string]any{
			{"role": "user", "content": prompt},
		},
	}
	raw, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.anthropic.com/v1/messages", bytes.NewReader(raw))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", j.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := j.httpc.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("judge HTTP: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return 0, "", fmt.Errorf("judge HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}
	var env struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(bodyBytes, &env); err != nil {
		return 0, "", fmt.Errorf("judge decode: %w", err)
	}
	if len(env.Content) == 0 {
		return 0, "", fmt.Errorf("judge: empty content")
	}
	score, notes := grader.ParseJudgeResponse(env.Content[0].Text)
	if score < 0 {
		return 0, "", fmt.Errorf("judge: could not parse score from %q", env.Content[0].Text)
	}
	return score, notes, nil
}
