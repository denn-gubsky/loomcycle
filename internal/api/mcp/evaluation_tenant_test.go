package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// The MCP `evaluation` meta-tool runs the Evaluation tool on the context
// mcpPrincipalCtx builds from the session's token. A substrate:tenant session
// must be confined to its tenant there; a substrate:admin session reads across
// tenants.

// globexScoredRun is a store with one globex run, scored once. The run's
// agent id is the MCP operator identity, so a submit_self from any MCP session
// derives role "self" against it: only the tenant boundary stands between an
// acme session and globex's selection data.
func globexScoredRun(t *testing.T) (store.Store, string) {
	t.Helper()
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "eval.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	sess, err := s.CreateSession(ctx, "globex", "worker", "bob")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	run, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: operatorAgentID, TenantID: "globex"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := s.EvaluationSubmit(ctx, store.EvaluationRow{
		EvalID: "eval_globex", RunID: run.ID, Score: 0.9, Rationale: "globex secret", EmitterRole: "self",
	}); err != nil {
		t.Fatalf("EvaluationSubmit: %v", err)
	}
	return s, run.ID
}

func TestMCPEvaluation_TenantSessionSubmitAgainstAnotherTenantsRunReadsLikeAMissingOne(t *testing.T) {
	s, globexRun := globexScoredRun(t)
	tool := &builtin.Evaluation{Store: s}
	ctx := mcpPrincipalCtx(auth.WithPrincipal(context.Background(),
		auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeTenant}}))

	theirs, _ := tool.Execute(ctx, json.RawMessage(`{"op":"submit","run_id":"`+globexRun+`","score":0.1}`))
	none, _ := tool.Execute(ctx, json.RawMessage(`{"op":"submit","run_id":"r_nope","score":0.1}`))
	got := strings.ReplaceAll(theirs.Text, globexRun, "<id>")
	want := strings.ReplaceAll(none.Text, "r_nope", "<id>")
	if !theirs.IsError || got != want {
		t.Errorf("acme session's submit against globex's run = %v %s, want the missing-run refusal %s", theirs.IsError, got, want)
	}
	rows, err := s.EvaluationListForRun(context.Background(), globexRun, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Errorf("globex's run holds %d evaluations, want its own 1", len(rows))
	}
}

func TestMCPEvaluation_AdminSessionReadsAnotherTenantsEvaluation(t *testing.T) {
	s, _ := globexScoredRun(t)
	tool := &builtin.Evaluation{Store: s}
	ctx := mcpPrincipalCtx(auth.WithPrincipal(context.Background(),
		auth.Principal{TenantID: "ops", Subject: "root", Scopes: []string{auth.ScopeAdmin}}))

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"get","eval_id":"eval_globex"}`))
	if res.IsError || !strings.Contains(res.Text, "globex secret") {
		t.Errorf("admin session's get of globex's evaluation = %s, want it read", res.Text)
	}
}
