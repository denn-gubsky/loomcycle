package http

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/runner"
)

// TestDecision_ADirectCallIsCountedAgainstItsUsersCap — a direct decision takes
// no run slot, so the per-user cap is what ties the calls a caller has in
// flight (or waiting for the decision lane) to that caller, as it does for the
// LLM gateway and /v1/embeddings. A caller at their cap is refused before any
// model is asked, with the sentinel gRPC and MCP map and the typed error the
// HTTP 429 body is built from; the count is given back when the call ends.
func TestDecision_ADirectCallIsCountedAgainstItsUsersCap(t *testing.T) {
	e := newDecisionRunEnv(t, true, map[string]config.AgentDef{}, nil)
	e.srv.sem.WithPerUserCap(1)
	ctx := auth.WithPrincipal(context.Background(), restrictedPrincipal()) // alice @ acme

	held, err := e.srv.sem.ReserveUser(context.Background(), "acme", "alice")
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.srv.Decision(ctx, json.RawMessage(decisionCall))
	if !errors.Is(err, runner.ErrPerUserQuotaExhausted) || !concurrency.IsPerUserQuotaExhausted(err) {
		t.Fatalf("a direct decision by a caller at their cap = %v, want the per-user quota refusal", err)
	}
	if n := e.endpoint.count(); n != 0 {
		t.Fatalf("the decision model was asked %d times for a refused caller", n)
	}

	held()
	res, err := e.srv.Decision(ctx, json.RawMessage(decisionCall))
	if err != nil || res.IsError {
		t.Fatalf("a direct decision under the cap = %q, %v; want an answer", res.Text, err)
	}
	if n := e.endpoint.count(); n != 1 {
		t.Fatalf("decision model calls = %d, want 1", n)
	}
	if st := e.srv.sem.Stats(); len(st.PerUser) != 0 || st.Active != 0 {
		t.Fatalf("after the call: active=%d perUser=%v, want nothing held", st.Active, st.PerUser)
	}
}
