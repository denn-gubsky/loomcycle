package http

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/loop"
)

// An idempotency_key answers a repeat with the run that holds it: its ids, its
// stream, its answer. A token limited to certain agents shares its tenant and
// user with the tenant's other tokens, so it shares their keys too, and must
// not be answered with a run its allowlist does not reach — whichever agent it
// names. It gets the answer any unavailable key gets, on every run-start
// route; its own keyed runs, and the unrestricted token's, deduplicate as
// before.
func TestRestrictedToken_IdempotencyKeyDoesNotAnswerWithAnotherAgentsRun(t *testing.T) {
	srv, ts, st := restrictedFixtureServer(t)
	keyed := func(agent, key string) string {
		return runBodyWith(agent, "idempotency_key", `"`+key+`"`)
	}
	if code, body := bearerCall(t, ts, freeBearer, "POST", "/v1/runs", keyed("other", "k-shared")); code != 200 {
		t.Fatalf("the unrestricted token's keyed run: %d %s", code, body)
	}
	held, found, err := st.RunByIdempotencyKey(context.Background(), clientRunKey("acme", "dev", "k-shared"))
	if err != nil || !found {
		t.Fatalf("the key's run: found %v, err %v", found, err)
	}
	leaks := func(body string) bool {
		return strings.Contains(body, held.ID) || strings.Contains(body, held.AgentID) || strings.Contains(body, "done")
	}

	for _, agent := range []string{"allowed", "other"} {
		code, body := bearerCall(t, ts, restrictedBearer, "POST", "/v1/runs", keyed(agent, "k-shared"))
		if code == 200 || leaks(body) || !strings.Contains(body, "idempotency_key is not available") {
			t.Errorf("restricted /v1/runs naming %q with the shared key: %d %s, want the key refused as unavailable", agent, code, body)
		}
	}
	code, body := bearerCall(t, ts, restrictedBearer, "POST", "/v1/runs:batch", `{"spawns":[`+keyed("allowed", "k-shared")+`]}`)
	if code != 200 || leaks(body) || !strings.Contains(body, "idempotency_key is not available") {
		t.Errorf("restricted /v1/runs:batch with the shared key: %d %s, want the child refused as unavailable", code, body)
	}

	// MCP spawn_run and the gRPC run starts go through the connector.
	restricted := auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: "acme", Subject: "dev", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead},
		Restrictions: auth.Restrictions{Agents: []string{"allowed"}},
	})
	seg := []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "go"}}}}
	res, err := srv.SpawnRun(restricted, connector.SpawnRunRequest{Agent: "allowed", Segments: seg, IdempotencyKey: "k-shared"})
	if res.Deduplicated || res.RunID == held.ID || res.FinalText != "" || err == nil && !strings.Contains(res.Error, "idempotency_key is not available") {
		t.Errorf("restricted SpawnRun with the shared key = %+v, %v; want the key refused as unavailable", res, err)
	}

	// Unchanged: the unrestricted token joins the run that holds the key, and
	// the restricted token's own key answers with its own run.
	if code, body := bearerCall(t, ts, freeBearer, "POST", "/v1/runs", keyed("other", "k-shared")); code != 200 || !strings.Contains(body, `"deduplicated":true`) {
		t.Errorf("unrestricted repeat: %d %s, want it deduplicated", code, body)
	}
	if code, body := bearerCall(t, ts, restrictedBearer, "POST", "/v1/runs", keyed("allowed", "k-own")); code != 200 {
		t.Fatalf("restricted keyed run of its own agent: %d %s", code, body)
	}
	own, found, err := st.RunByIdempotencyKey(context.Background(), clientRunKey("acme", "dev", "k-own"))
	if err != nil || !found {
		t.Fatalf("the restricted token's own key: found %v, err %v", found, err)
	}
	if code, body := bearerCall(t, ts, restrictedBearer, "POST", "/v1/runs", keyed("allowed", "k-own")); code != 200 ||
		!strings.Contains(body, `"deduplicated":true`) || !strings.Contains(body, own.ID) {
		t.Errorf("restricted repeat of its own key: %d %s, want its own run deduplicated", code, body)
	}
}
