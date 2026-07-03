package webhook

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/runner"
)

// TestSpawnSetupError_TokenLimitMaps429 is the RFC AW regression: a webhook run
// refused at admission by a hard token budget must map to 429
// "token_limit_exceeded" (mirroring the HTTP run endpoint), not the generic 503
// — so a webhook client branches retry-next-window instead of retry-storming a
// budget-exhausted scope. Fails on the pre-fix code (falls to the default 503).
func TestSpawnSetupError_TokenLimitMaps429(t *testing.T) {
	rec := &Receiver{}

	w := httptest.NewRecorder()
	rec.spawnSetupErrorResponse(w, runner.ErrTokenLimitExceeded)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("token-limit refusal status = %d, want 429", w.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != "token_limit_exceeded" {
		t.Fatalf("error code = %q, want token_limit_exceeded", body["error"])
	}

	// A generic setup error still maps to the transient 503 (unchanged).
	w2 := httptest.NewRecorder()
	rec.spawnSetupErrorResponse(w2, runner.ErrBackpressure)
	if w2.Code != http.StatusServiceUnavailable {
		t.Fatalf("backpressure status = %d, want 503", w2.Code)
	}
}
