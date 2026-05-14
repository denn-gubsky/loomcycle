package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
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

// maxJudgeRetries is the cap on retry attempts for transient
// Anthropic responses (429, 5xx). Past v0.1 of the bench surfaced
// 529 "overloaded" on a long sweep; without retry the mid-07/v4-pro
// run lost its semantic score and the row dropped to FAIL despite
// the model itself producing valid output. Capping at 3 keeps a
// stuck judge from indefinitely blocking a budget-capped sweep.
const maxJudgeRetries = 3

// Score runs the rubric prompt through the judge model and parses
// the {score, notes} JSON reply. Retries on transient errors (HTTP
// 429, 529, and 5xx) with exponential backoff honoring the
// Retry-After header when present.
func (j *anthropicJudge) Score(ctx context.Context, prompt string) (int, string, error) {
	body := map[string]any{
		"model":       j.model,
		"max_tokens":  512,
		"temperature": 0,
		"messages": []map[string]any{
			{"role": "user", "content": prompt},
		},
	}
	raw, _ := json.Marshal(body)

	var lastErr error
	for attempt := 0; attempt <= maxJudgeRetries; attempt++ {
		if attempt > 0 {
			wait := backoffFor(attempt, lastErr)
			select {
			case <-ctx.Done():
				return 0, "", ctx.Err()
			case <-time.After(wait):
			}
		}

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
			// Network errors are retryable — connection-reset and
			// DNS hiccups happen on long sweeps.
			lastErr = fmt.Errorf("judge HTTP: %w", err)
			continue
		}

		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
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

		err = &judgeHTTPError{status: resp.StatusCode, body: string(bodyBytes), retryAfter: resp.Header.Get("Retry-After")}
		if !isRetryable(resp.StatusCode) {
			return 0, "", err
		}
		lastErr = err
	}
	return 0, "", fmt.Errorf("judge: gave up after %d retries: %w", maxJudgeRetries, lastErr)
}

// judgeHTTPError carries the response context across the retry loop
// so backoffFor can honor Retry-After.
type judgeHTTPError struct {
	status     int
	body       string
	retryAfter string
}

func (e *judgeHTTPError) Error() string {
	return fmt.Sprintf("judge HTTP %d: %s", e.status, e.body)
}

// isRetryable returns true for status codes Anthropic surfaces under
// transient load: 408 (timeout), 425 (too-early), 429 (rate limit),
// 500/502/503/504/529 (server overload).
func isRetryable(status int) bool {
	switch status {
	case 408, 425, 429, 500, 502, 503, 504, 529:
		return true
	}
	return false
}

// backoffFor returns the delay before the next retry. Honors a
// numeric Retry-After header when the last error carries one;
// otherwise uses exponential backoff capped at 16 s so a stuck
// judge can't drag a sweep out indefinitely. attempt is 1-indexed.
func backoffFor(attempt int, lastErr error) time.Duration {
	if je, ok := lastErr.(*judgeHTTPError); ok && je.retryAfter != "" {
		if secs, err := strconv.Atoi(je.retryAfter); err == nil && secs > 0 {
			if secs > 30 {
				secs = 30
			}
			return time.Duration(secs) * time.Second
		}
	}
	d := time.Duration(1<<uint(attempt-1)) * time.Second
	if d > 16*time.Second {
		d = 16 * time.Second
	}
	return d
}
