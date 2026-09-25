package http

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func isolatedMemberCtx(tenant, subject string) context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: tenant, Subject: subject, Scopes: []string{auth.ScopeUser},
	})
}

// An isolated member is confined to its own sessions, and a run's content —
// the prompt it was given, its answer, its spec — is that session's content by
// another route. Each read that returns it answers another user's run in the
// same tenant with the opaque not-found a missing run gets; the member's own
// run and a tenant operator's whole-tenant view are unchanged.
func TestRunContentReads_IsolatedMemberCannotReadAnotherUsersRun(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	ctx := context.Background()
	alice := seedTenantRun(t, srv.store, "acme", "alice", "a_alice")
	own := seedTenantRun(t, srv.store, "acme", "bob", "a_bob")
	for _, r := range []store.Run{alice, own} {
		srv.makeRecordingEmit(ctx, r.ID, tools.RunIdentityValue{}, r.SessionID, func(providers.Event) {})(
			snapshotEvent("system for "+r.UserID, "secret plan of "+r.UserID))
		if err := srv.store.FinishRun(ctx, r.ID, store.RunCompleted, "end_turn",
			store.Usage{Result: []byte(`{"final_text":"answer for ` + r.UserID + `"}`)}, ""); err != nil {
			t.Fatalf("FinishRun: %v", err)
		}
	}
	bob := isolatedMemberCtx("acme", "bob")

	if code, body := getPrompt(t, srv, bob, alice.ID); code != 404 || strings.Contains(body, "alice") {
		t.Errorf("GET /prompt of alice's run as bob = %d %s, want an opaque 404", code, body)
	}
	if code, body := getPrompt(t, srv, bob, own.ID); code != 200 || !strings.Contains(body, "secret plan of bob") {
		t.Errorf("GET /prompt of bob's own run = %d %s, want 200", code, body)
	}
	if code, body := getPrompt(t, srv, tenantOperatorCtx("acme"), alice.ID); code != 200 {
		t.Errorf("GET /prompt as a tenant operator = %d %s, want 200", code, body)
	}

	getAgent := func(c context.Context, agentID string) (int, string) {
		req := httptest.NewRequest("GET", "/v1/agents/"+agentID, nil).WithContext(c)
		req.SetPathValue("agent_id", agentID)
		rec := httptest.NewRecorder()
		srv.handleGetAgent(rec, req)
		return rec.Code, rec.Body.String()
	}
	if code, body := getAgent(bob, "a_alice"); code != 404 || strings.Contains(body, "answer for alice") {
		t.Errorf("GET /v1/agents of alice's run as bob = %d %s, want an opaque 404", code, body)
	}
	if code, body := getAgent(bob, "a_bob"); code != 200 || !strings.Contains(body, "answer for bob") {
		t.Errorf("GET /v1/agents of bob's own run = %d %s, want 200 with its result", code, body)
	}

	var nf *store.ErrNotFound
	if got, err := srv.GetRun(bob, "a_alice"); !errors.As(err, &nf) {
		t.Errorf("connector.GetRun (MCP get_run) of alice's run as bob = (%s, %v), want not-found", got.Result, err)
	}
	if got, err := srv.GetRun(bob, "a_bob"); err != nil || !strings.Contains(string(got.Result), "answer for bob") {
		t.Errorf("connector.GetRun of bob's own run = (%s, %v), want its result", got.Result, err)
	}
	if _, err := srv.GetRun(tenantOperatorCtx("acme"), "a_alice"); err != nil {
		t.Errorf("connector.GetRun as a tenant operator: %v, want the run", err)
	}

	// The replay of a run's events carries its prompt and answer too.
	stream := func(c context.Context, runID string) (int, string) {
		req := httptest.NewRequest("GET", "/v1/runs/"+runID+"/stream", nil).WithContext(c)
		req.SetPathValue("run_id", runID)
		rec := httptest.NewRecorder()
		srv.handleRunStream(rec, req)
		return rec.Code, rec.Body.String()
	}
	if code, body := stream(bob, alice.ID); code != 404 || strings.Contains(body, "alice") {
		t.Errorf("GET /v1/runs/{id}/stream of alice's run as bob = %d %s, want an opaque 404", code, body)
	}
	if code, _ := stream(bob, own.ID); code != 200 {
		t.Errorf("GET /v1/runs/{id}/stream of bob's own run = %d, want 200", code)
	}
	visit := func(providers.Event) error { return nil }
	if err := srv.StreamRunEvents(bob, alice.ID, 0, visit); !errors.Is(err, connector.ErrRunNotInFlight) {
		t.Errorf("connector.StreamRunEvents (gRPC StreamRun) of alice's run as bob = %v, want the opaque not-in-flight", err)
	}
	if err := srv.StreamRunEvents(bob, own.ID, 0, visit); err != nil {
		t.Errorf("connector.StreamRunEvents of bob's own run: %v", err)
	}
}
