package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/bench/internal/grader"
)

// Three judge backends + a consensus wrapper. The bench used to use
// just Anthropic; v3 adds DeepSeek and Gemini so the operator can
// `--judges anthropic,deepseek,gemini` and get a vote-and-average
// semantic score that's less biased toward any single provider's
// idea of "good".
//
// Operator-side caveats:
//   - Single-judge runs are still allowed (default = anthropic) and
//     are cheaper / faster. Use single-judge when grading speed
//     matters more than judge-bias reduction.
//   - The consensus is a simple median + concatenated notes. We do
//     NOT weight by judge confidence or model size.
//   - Each judge runs in parallel for one (case, run) — no fan-in
//     latency penalty beyond the slowest judge's response time.
//   - When ANY judge in the consensus errors out, the consensus uses
//     the median of the surviving judges. All-error = full failure.

// newJudge constructs the judge specified by names. names is a CSV
// list like "anthropic,deepseek,gemini" (or just "anthropic"). Empty
// names disables judging (returns nil, which the grader treats as
// pass-through).
//
// Skips named judges whose API key is not set (logs once at startup).
// If all named judges are unavailable, returns nil.
func newJudge(names []string) grader.Judge {
	var judges []grader.Judge
	for _, name := range names {
		switch strings.TrimSpace(strings.ToLower(name)) {
		case "anthropic":
			if j := newAnthropicJudge(); j != nil {
				judges = append(judges, j)
			}
		case "deepseek":
			if j := newDeepSeekJudge(); j != nil {
				judges = append(judges, j)
			}
		case "gemini":
			if j := newGeminiJudge(); j != nil {
				judges = append(judges, j)
			}
		default:
			// Unknown name — operator typo. Skip with a log, don't
			// fail the sweep.
		}
	}
	if len(judges) == 0 {
		return nil
	}
	if len(judges) == 1 {
		return judges[0]
	}
	return &consensusJudge{judges: judges}
}

// --- Anthropic judge ---

type anthropicJudge struct {
	apiKey string
	model  string
	httpc  *http.Client
}

func newAnthropicJudge() grader.Judge {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return nil
	}
	model := os.Getenv("LOOMCYCLE_BENCH_JUDGE_MODEL")
	if model == "" {
		model = "claude-sonnet-4-6"
	}
	return &anthropicJudge{
		apiKey: key,
		model:  model,
		httpc:  &http.Client{Timeout: 90 * time.Second},
	}
}

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
	resp, err := httpJudgeCall(ctx, j.httpc, http.MethodPost,
		"https://api.anthropic.com/v1/messages", raw,
		map[string]string{
			"Content-Type":      "application/json",
			"x-api-key":         j.apiKey,
			"anthropic-version": "2023-06-01",
		})
	if err != nil {
		return 0, "", err
	}
	var env struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(resp, &env); err != nil {
		return 0, "", fmt.Errorf("anthropic-judge decode: %w", err)
	}
	if len(env.Content) == 0 {
		return 0, "", fmt.Errorf("anthropic-judge: empty content")
	}
	return extractScoreOrErr(env.Content[0].Text, "anthropic")
}

// --- DeepSeek judge (OpenAI-compatible chat completions) ---

type deepSeekJudge struct {
	apiKey string
	model  string
	httpc  *http.Client
}

func newDeepSeekJudge() grader.Judge {
	key := os.Getenv("DEEPSEEK_API_KEY")
	if key == "" {
		return nil
	}
	model := os.Getenv("LOOMCYCLE_BENCH_JUDGE_MODEL_DEEPSEEK")
	if model == "" {
		// deepseek-chat is the operator's non-thinking variant.
		// Sweep #6 surfaced a critical bug with the reasoning-class
		// default (deepseek-v4-pro): extended thinking consumed all
		// of the 512-token max_tokens budget, leaving ZERO bytes of
		// final output. Every Sweep #6 row that used the deepseek
		// judge logged "could not parse score from \"\"", silently
		// degrading the consensus to single-judge anthropic. Switching
		// to deepseek-chat keeps grading fast + cheap and produces
		// real scored output.
		model = "deepseek-chat"
	}
	return &deepSeekJudge{
		apiKey: key,
		model:  model,
		httpc:  &http.Client{Timeout: 90 * time.Second},
	}
}

func (j *deepSeekJudge) Score(ctx context.Context, prompt string) (int, string, error) {
	body := map[string]any{
		"model":       j.model,
		"max_tokens":  512,
		"temperature": 0,
		"messages": []map[string]any{
			{"role": "user", "content": prompt},
		},
	}
	raw, _ := json.Marshal(body)
	resp, err := httpJudgeCall(ctx, j.httpc, http.MethodPost,
		"https://api.deepseek.com/v1/chat/completions", raw,
		map[string]string{
			"Content-Type":  "application/json",
			"Authorization": "Bearer " + j.apiKey,
		})
	if err != nil {
		return 0, "", err
	}
	var env struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(resp, &env); err != nil {
		return 0, "", fmt.Errorf("deepseek-judge decode: %w", err)
	}
	if len(env.Choices) == 0 {
		return 0, "", fmt.Errorf("deepseek-judge: empty choices")
	}
	return extractScoreOrErr(env.Choices[0].Message.Content, "deepseek")
}

// --- Gemini judge ---

type geminiJudge struct {
	apiKey string
	model  string
	httpc  *http.Client
}

func newGeminiJudge() grader.Judge {
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		return nil
	}
	model := os.Getenv("LOOMCYCLE_BENCH_JUDGE_MODEL_GEMINI")
	if model == "" {
		// gemini-2.5-flash is the non-thinking variant. Sweep #6
		// surfaced the same critical bug here as on the deepseek
		// judge: gemini-2.5-pro's extended thinking consumed the
		// 512-token max_tokens budget and the response came back with
		// "empty candidates" on every case. Switching to 2.5-flash
		// (no thinking, fast structured output) restores real
		// consensus voting.
		//
		// Avoid 2.0-flash — deprecated as of 2026-05-15
		// (404 NOT_FOUND "no longer available to new users").
		model = "gemini-2.5-flash"
	}
	return &geminiJudge{
		apiKey: key,
		model:  model,
		httpc:  &http.Client{Timeout: 90 * time.Second},
	}
}

func (j *geminiJudge) Score(ctx context.Context, prompt string) (int, string, error) {
	body := map[string]any{
		"contents": []map[string]any{
			{"role": "user", "parts": []map[string]any{{"text": prompt}}},
		},
		"generationConfig": map[string]any{
			"temperature":     0,
			"maxOutputTokens": 512,
		},
	}
	raw, _ := json.Marshal(body)
	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s",
		j.model, j.apiKey)
	resp, err := httpJudgeCall(ctx, j.httpc, http.MethodPost, url, raw,
		map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return 0, "", err
	}
	var env struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(resp, &env); err != nil {
		return 0, "", fmt.Errorf("gemini-judge decode: %w", err)
	}
	if len(env.Candidates) == 0 || len(env.Candidates[0].Content.Parts) == 0 {
		return 0, "", fmt.Errorf("gemini-judge: empty candidates")
	}
	return extractScoreOrErr(env.Candidates[0].Content.Parts[0].Text, "gemini")
}

// --- Consensus judge ---

// consensusJudge fans out a score request to N child judges in
// parallel and aggregates the results: median score, concatenated
// notes from each judge with the judge's name as a prefix.
//
// Operator-side bias mitigation: each judge has its own model
// preferences. Anthropic Sonnet tends to score Anthropic-style
// outputs higher; DeepSeek scores DeepSeek-style more leniently;
// Gemini has its own quirks. Median across the three smooths out
// any one judge's bias.
type consensusJudge struct {
	judges []grader.Judge
}

func (c *consensusJudge) Score(ctx context.Context, prompt string) (int, string, error) {
	type result struct {
		idx     int
		score   int
		notes   string
		err     error
	}
	out := make(chan result, len(c.judges))
	for i, j := range c.judges {
		go func(i int, j grader.Judge) {
			score, notes, err := j.Score(ctx, prompt)
			out <- result{idx: i, score: score, notes: notes, err: err}
		}(i, j)
	}
	var scores []int
	var noteParts []string
	errCount := 0
	for range c.judges {
		r := <-out
		if r.err != nil {
			errCount++
			noteParts = append(noteParts, fmt.Sprintf("[judge-%d ERROR: %s]", r.idx, r.err.Error()))
			continue
		}
		scores = append(scores, r.score)
		noteParts = append(noteParts, fmt.Sprintf("[judge-%d %d/100: %s]", r.idx, r.score, r.notes))
	}
	if len(scores) == 0 {
		return 0, "", fmt.Errorf("all %d judges errored: %s", errCount, strings.Join(noteParts, "; "))
	}
	sort.Ints(scores)
	median := scores[len(scores)/2]
	return median, strings.Join(noteParts, " | "), nil
}

// --- Shared HTTP helpers ---

const maxJudgeRetries = 3

// httpJudgeCall executes a single HTTP POST with retry on transient
// errors (429, 5xx, network blips). Returns the raw response body
// bytes for the caller to decode.
func httpJudgeCall(ctx context.Context, hc *http.Client, method, url string, body []byte, headers map[string]string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt <= maxJudgeRetries; attempt++ {
		if attempt > 0 {
			wait := backoffFor(attempt, lastErr)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := hc.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("HTTP: %w", err)
			continue
		}
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return bodyBytes, nil
		}
		err = &judgeHTTPError{status: resp.StatusCode, body: string(bodyBytes), retryAfter: resp.Header.Get("Retry-After")}
		if !isRetryable(resp.StatusCode) {
			return nil, err
		}
		lastErr = err
	}
	return nil, fmt.Errorf("gave up after %d retries: %w", maxJudgeRetries, lastErr)
}

// extractScoreOrErr parses the judge's text reply into a (score, notes)
// tuple using the standard ParseJudgeResponse helper. Returns a
// provider-tagged error when the score can't be parsed (so consensus
// logs identify which judge produced the bad output).
func extractScoreOrErr(text, providerTag string) (int, string, error) {
	score, notes := grader.ParseJudgeResponse(text)
	if score < 0 {
		return 0, "", fmt.Errorf("%s-judge: could not parse score from %q", providerTag, text)
	}
	return score, notes, nil
}

// judgeHTTPError + isRetryable + backoffFor stay unchanged from v1.
type judgeHTTPError struct {
	status     int
	body       string
	retryAfter string
}

func (e *judgeHTTPError) Error() string {
	return fmt.Sprintf("judge HTTP %d: %s", e.status, e.body)
}

func isRetryable(status int) bool {
	switch status {
	case 408, 425, 429, 500, 502, 503, 504, 529:
		return true
	}
	return false
}

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
