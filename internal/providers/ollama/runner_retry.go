package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// runnerRetryWaits is how long to wait before each retry of a call whose
// response says the model runner died; its length is the retry bound.
//
// Why so short when a reload takes ~10–25 s: Ollama does not restart a dead
// runner on its own — it restarts it on the NEXT request, and that request
// blocks until the model is loaded again. So the retried call itself absorbs
// the reload; the wait only has to let Ollama notice the dead process and
// stop routing to it. The second, longer wait covers a runner that died again
// while reloading under the same load that killed it.
var runnerRetryWaits = []time.Duration{2 * time.Second, 5 * time.Second}

// runnerDeathPeek bounds how much of an error body is read to classify it.
// Ollama's error bodies are one short JSON object.
const runnerDeathPeek = 4096

// IsRunnerDeath reports whether an Ollama error response says the per-model
// runner process (llama-server) died or its connection dropped — a transient
// fault: Ollama starts a fresh runner on the next request.
//
// Ollama reports it only in the message text, and with more than one status:
// 500 from /api/chat and /v1/systemone, 500 or 400 from /api/embed. A reply
// that merely names a missing or unknown model does not match.
func IsRunnerDeath(status int, body []byte) bool {
	if status != http.StatusInternalServerError && status != http.StatusBadRequest {
		return false
	}
	msg := string(body)
	var doc struct {
		Error string `json:"error"`
	}
	// The raw text is kept when the body is not a whole JSON object (a
	// truncated read, or a proxy's plain-text page).
	if json.Unmarshal(body, &doc) == nil && doc.Error != "" {
		msg = doc.Error
	}
	switch {
	// "llama-server process has terminated: signal: aborted (core dumped)",
	// "llama-server process no longer running: unknown". Ollama before the
	// runner was renamed said "llama runner process ...".
	case strings.Contains(msg, "llama-server"), strings.Contains(msg, "llama runner process"):
		return true
	// "an error was encountered while running the model: unexpected EOF". The
	// same prefix also fronts faults a retry cannot fix, so the EOF is required.
	case strings.Contains(msg, "an error was encountered while running the model") && strings.Contains(msg, "EOF"):
		return true
	// Ollama's own request to the runner's loopback port failed:
	// `Post "http://127.0.0.1:41233/completion": EOF`,
	// `do embedding request: Post "http://127.0.0.1:41233/embedding": EOF`.
	case strings.Contains(msg, "http://127.0.0.1:") &&
		(strings.Contains(msg, "EOF") || strings.Contains(msg, "connection refused") || strings.Contains(msg, "connection reset")):
		return true
	}
	return false
}

// retryOnRunnerDeath calls attempt, and again after each of waits while the
// response is a runner death. The response it returns has an unread body.
//
// It is safe to send the same request again because the response is an error
// status: no stream began, so nothing reached the caller. It never sees a 200,
// and so never retries a fault reported inside a started stream.
//
// After the last wait the runner-death response is returned as it came, for
// the caller's ordinary error path. onRetry is called before each wait.
func retryOnRunnerDeath(ctx context.Context, waits []time.Duration, onRetry func(attempt int, wait time.Duration), attempt func() (*http.Response, error)) (*http.Response, error) {
	for i := 0; ; i++ {
		resp, err := attempt()
		if err != nil || i >= len(waits) {
			return resp, err
		}
		if resp.StatusCode != http.StatusInternalServerError && resp.StatusCode != http.StatusBadRequest {
			return resp, nil
		}
		peek, _ := io.ReadAll(io.LimitReader(resp.Body, runnerDeathPeek))
		if !IsRunnerDeath(resp.StatusCode, peek) {
			// Hand the bytes just read back to the caller, which reads the body
			// for its error message.
			resp.Body = struct {
				io.Reader
				io.Closer
			}{io.MultiReader(bytes.NewReader(peek), resp.Body), resp.Body}
			return resp, nil
		}
		// Drain + close so the connection is reusable for the retry.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		onRetry(i+1, waits[i])
		if err := sleepCtx(ctx, waits[i]); err != nil {
			return nil, err
		}
	}
}

// sleepCtx waits d, or returns ctx's error as soon as ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func logRunnerRetry(provider, model, endpoint string, attempt int, wait time.Duration) {
	log.Printf(`{"event":"ollama_runner_retry","provider":%q,"model":%q,"endpoint":%q,"attempt":%d,"wait":%q}`,
		provider, model, endpoint, attempt, wait.String())
}
