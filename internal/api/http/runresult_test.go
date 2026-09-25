package http

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/redact"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

func readResult(t *testing.T, st store.Store, runID string) runResultRecord {
	t.Helper()
	run, err := st.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	var rec runResultRecord
	if len(run.Result) > 0 {
		if err := json.Unmarshal(run.Result, &rec); err != nil {
			t.Fatalf("result is not JSON (%q): %v", run.Result, err)
		}
	}
	return rec
}

// Every server path that makes a run terminal decides its result — the writer
// census. finishRun and finishRunCancelled carry the loop's answer (a cancelled
// run keeps the text it had produced); finishRunFailedReason never ran a loop
// and deliberately writes none.
func TestFinishPaths_WriteTheRunResult(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	res := loop.RunResult{StopReason: "end_turn", FinalText: "the answer", State: map[string]any{"step": "done"}}

	done := seedTenantRun(t, srv.store, "acme", "u1", "a_done")
	srv.finishRun(context.Background(), done.ID, res, nil, runStateMeta{})
	if got := readResult(t, srv.store, done.ID); got.FinalText != "the answer" || got.State["step"] != "done" {
		t.Errorf("finishRun result = %+v, want the final text and state", got)
	}

	cancelled := seedTenantRun(t, srv.store, "acme", "u1", "a_cancelled")
	srv.finishRunCancelled(context.Background(), cancelled.ID, loop.RunResult{FinalText: "half an answer"}, "stop", runStateMeta{})
	if got := readResult(t, srv.store, cancelled.ID); got.FinalText != "half an answer" {
		t.Errorf("finishRunCancelled result = %+v, want the partial text", got)
	}

	failed := seedTenantRun(t, srv.store, "acme", "u1", "a_failed")
	srv.finishRunFailedReason(failed.ID, "agent_id in use", runStateMeta{})
	if run, _ := srv.store.GetRun(context.Background(), failed.ID); len(run.Result) != 0 {
		t.Errorf("a run that never ran has result %q, want none", run.Result)
	}
}

// A team walk's run answers with its last state's output.
func TestTeamWalkRun_ResultIsTheWalksFinalOutput(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	_, runID, finish, err := srv.openTeamWalkRun(substrateAdminCtx(tenantOperatorCtx("acme")), "triage", false)
	if err != nil {
		t.Fatalf("openTeamWalkRun: %v", err)
	}
	finish("triaged: 3 issues", nil)
	if got := readResult(t, srv.store, runID); got.FinalText != "triaged: 3 issues" {
		t.Errorf("walk result = %+v, want its final output", got)
	}
}

// The result is on the single-run reads (HTTP GET /v1/agents/{id} and the
// connector GetRun behind MCP get_run) and deliberately not on listings.
func TestRunReads_SingleCarriesResultListDoesNot(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	run := seedTenantRun(t, srv.store, "acme", "u1", "a_read")
	srv.finishRun(context.Background(), run.ID, loop.RunResult{FinalText: "read me"}, nil, runStateMeta{})

	req := httptest.NewRequest("GET", "/v1/agents/a_read", nil)
	req.SetPathValue("agent_id", "a_read")
	req = req.WithContext(tenantOperatorCtx("acme"))
	rec := httptest.NewRecorder()
	srv.handleGetAgent(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"final_text":"read me"`) {
		t.Errorf("GET /v1/agents/{id} = %d %s, want the result", rec.Code, rec.Body)
	}

	got, err := srv.GetRun(tenantOperatorCtx("acme"), "a_read")
	if err != nil || !strings.Contains(string(got.Result), "read me") {
		t.Errorf("connector GetRun result = %q (%v), want the answer", got.Result, err)
	}
	listed, err := srv.ListRuns(tenantOperatorCtx("acme"), listFilter("u1"))
	if err != nil || len(listed) != 1 || len(listed[0].Result) != 0 {
		t.Errorf("ListRuns = %+v (%v), want one row without a result", listed, err)
	}
}

func listFilter(user string) connector.ListRunsFilter { return connector.ListRunsFilter{UserID: user} }

// The result repeats what the run's persisted events carry — its final text
// and Σ — and those are masked before they are stored. Every finish path
// masks the result the same way, or the secret is back at rest on the row.
func TestFinishPaths_RedactTheRunResult(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	const secret = "ghs_resultsecret_0123456789abcdef"
	srv.redactor = redact.New(map[string]string{"LOOMCYCLE_GITEA_TOKEN": secret}, true)
	res := loop.RunResult{
		FinalText:  "token is " + secret,
		State:      map[string]any{"creds": map[string]any{"token": secret}},
		Structured: map[string]any{"token": secret},
	}
	stored := func(runID string) string {
		run, err := srv.store.GetRun(context.Background(), runID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		return string(run.Result)
	}

	done := seedTenantRun(t, srv.store, "acme", "u1", "a_redact_done")
	srv.finishRun(context.Background(), done.ID, res, nil, runStateMeta{})
	cancelled := seedTenantRun(t, srv.store, "acme", "u1", "a_redact_cancelled")
	srv.finishRunCancelled(context.Background(), cancelled.ID, res, "stop", runStateMeta{})
	_, walkID, finish, err := srv.openTeamWalkRun(substrateAdminCtx(tenantOperatorCtx("acme")), "triage", false)
	if err != nil {
		t.Fatalf("openTeamWalkRun: %v", err)
	}
	finish("walk says "+secret, nil)

	for name, id := range map[string]string{"finishRun": done.ID, "finishRunCancelled": cancelled.ID, "team walk": walkID} {
		got := stored(id)
		if strings.Contains(got, secret) {
			t.Errorf("%s stored the secret in runs.result: %s", name, got)
		}
		if !json.Valid([]byte(got)) || got == "" {
			t.Errorf("%s result = %q, want the masked answer kept", name, got)
		}
	}
}
