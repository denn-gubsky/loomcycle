package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// evaluationFixture builds an Evaluation tool over in-memory SQLite +
// a pre-created run owned by AgentID="a_target" with parent
// AgentID="a_parent". Tests override the policy on ctx per-case.
func evaluationFixture(t *testing.T) (*Evaluation, store.Store, string, func()) {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	ctx := context.Background()
	sess, err := s.CreateSession(ctx, "t1", "researcher", "u1")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	run, err := s.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID:       "a_target",
		ParentAgentID: "a_parent",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	tool := &Evaluation{
		Store:             s,
		MaxJudgementBytes: 32768,
		MaxRationaleBytes: 8192,
	}
	return tool, s, run.ID, func() { _ = s.Close() }
}

// ctxWithEmitter assembles a tool context carrying the emitter's
// AgentID + an EvaluationPolicy scope list.
func ctxWithEmitter(agentID string, scopes ...string) context.Context {
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: agentID})
	ctx = tools.WithEvaluationPolicy(ctx, tools.EvaluationPolicyValue{Scopes: scopes})
	return ctx
}

func TestEvaluationTool_SubmitRequiresRunID(t *testing.T) {
	tool, _, _, cleanup := evaluationFixture(t)
	defer cleanup()
	ctx := ctxWithEmitter("a_target", "submit_self")

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"submit","score":0.5}`))
	if !res.IsError {
		t.Fatalf("submit without run_id should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "run_id") {
		t.Errorf("error should mention run_id; got %s", res.Text)
	}
}

func TestEvaluationTool_SubmitRequiresScore(t *testing.T) {
	tool, _, runID, cleanup := evaluationFixture(t)
	defer cleanup()
	ctx := ctxWithEmitter("a_target", "submit_self")

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"submit","run_id":"`+runID+`"}`))
	if !res.IsError {
		t.Fatalf("submit without score should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "score") {
		t.Errorf("error should mention score; got %s", res.Text)
	}
}

func TestEvaluationTool_SubmitDerivesSelfRole(t *testing.T) {
	tool, _, runID, cleanup := evaluationFixture(t)
	defer cleanup()
	// Emitter == target agent_id ⇒ role "self"
	ctx := ctxWithEmitter("a_target", "submit_self")

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"submit","run_id":"`+runID+`","score":0.75}`))
	if res.IsError {
		t.Fatalf("submit self: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["emitter_role"] != "self" {
		t.Errorf("emitter_role = %v, want self", out["emitter_role"])
	}
	if out["score"].(float64) != 0.75 {
		t.Errorf("score = %v, want 0.75", out["score"])
	}
}

func TestEvaluationTool_SubmitDerivesParentRole(t *testing.T) {
	tool, _, runID, cleanup := evaluationFixture(t)
	defer cleanup()
	// Emitter == target.parent_agent_id ⇒ role "parent"
	ctx := ctxWithEmitter("a_parent", "submit_descendants")

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"submit","run_id":"`+runID+`","score":0.9}`))
	if res.IsError {
		t.Fatalf("submit parent: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["emitter_role"] != "parent" {
		t.Errorf("emitter_role = %v, want parent", out["emitter_role"])
	}
}

func TestEvaluationTool_SubmitDerivesExternalRole(t *testing.T) {
	tool, _, runID, cleanup := evaluationFixture(t)
	defer cleanup()
	// Empty emitter AgentID ⇒ role "external"
	// External submissions need submit_any (no dedicated scope).
	ctx := ctxWithEmitter("", "submit_any")

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"submit","run_id":"`+runID+`","score":0.1}`))
	if res.IsError {
		t.Fatalf("submit external: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["emitter_role"] != "external" {
		t.Errorf("emitter_role = %v, want external", out["emitter_role"])
	}
}

func TestEvaluationTool_SubmitDerivesUnrelatedRole(t *testing.T) {
	tool, _, runID, cleanup := evaluationFixture(t)
	defer cleanup()
	// Emitter unrelated to target ⇒ role "unrelated".
	// submit_any is the only scope that permits it.
	ctx := ctxWithEmitter("a_random", "submit_any")

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"submit","run_id":"`+runID+`","score":0.0}`))
	if res.IsError {
		t.Fatalf("submit unrelated: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["emitter_role"] != "unrelated" {
		t.Errorf("emitter_role = %v, want unrelated", out["emitter_role"])
	}
}

func TestEvaluationTool_SubmitRefusedWithoutScope(t *testing.T) {
	tool, _, runID, cleanup := evaluationFixture(t)
	defer cleanup()
	// Self submit but no submit_self scope (only read_any).
	ctx := ctxWithEmitter("a_target", "read_any")

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"submit","run_id":"`+runID+`","score":0.5}`))
	if !res.IsError {
		t.Fatalf("submit without submit_self should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "default-deny") {
		t.Errorf("error should mention default-deny; got %s", res.Text)
	}
}

func TestEvaluationTool_SubmitRefusedEmptyScopes(t *testing.T) {
	tool, _, runID, cleanup := evaluationFixture(t)
	defer cleanup()
	ctx := ctxWithEmitter("a_target") // no scopes at all

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"submit","run_id":"`+runID+`","score":0.5}`))
	if !res.IsError {
		t.Fatalf("submit with no scopes should refuse; got %s", res.Text)
	}
}

func TestEvaluationTool_SubmitUnknownRunID(t *testing.T) {
	tool, _, _, cleanup := evaluationFixture(t)
	defer cleanup()
	ctx := ctxWithEmitter("a_target", "submit_any")

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"submit","run_id":"r_nope","score":0.5}`))
	if !res.IsError {
		t.Fatalf("submit against missing run should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "not found") {
		t.Errorf("error should mention not found; got %s", res.Text)
	}
}

func TestEvaluationTool_GetRequiresReadAny(t *testing.T) {
	tool, _, runID, cleanup := evaluationFixture(t)
	defer cleanup()
	// First submit one row (with permissive scope) so there's something to get.
	subCtx := ctxWithEmitter("a_target", "submit_self")
	res, _ := tool.Execute(subCtx, json.RawMessage(`{"op":"submit","run_id":"`+runID+`","score":0.5}`))
	if res.IsError {
		t.Fatalf("setup submit: %s", res.Text)
	}
	evalID := decodeResult(t, res.Text)["eval_id"].(string)

	// Get without read_any → refuse.
	ctx := ctxWithEmitter("a_target", "submit_self")
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"get","eval_id":"`+evalID+`"}`))
	if !res.IsError {
		t.Fatalf("get without read_any should refuse; got %s", res.Text)
	}

	// With read_any → succeed.
	ctx = ctxWithEmitter("a_target", "read_any")
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"get","eval_id":"`+evalID+`"}`))
	if res.IsError {
		t.Fatalf("get with read_any: %s", res.Text)
	}
	if decodeResult(t, res.Text)["eval_id"] != evalID {
		t.Error("get returned wrong row")
	}
}

func TestEvaluationTool_ListForRun(t *testing.T) {
	tool, _, runID, cleanup := evaluationFixture(t)
	defer cleanup()
	// Submit two evaluations against the same run.
	subCtx := ctxWithEmitter("a_target", "submit_self")
	for _, sc := range []string{"0.4", "0.6"} {
		res, _ := tool.Execute(subCtx, json.RawMessage(`{"op":"submit","run_id":"`+runID+`","score":`+sc+`}`))
		if res.IsError {
			t.Fatalf("setup submit: %s", res.Text)
		}
	}
	ctx := ctxWithEmitter("a_target", "read_any")
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"list_for_run","run_id":"`+runID+`"}`))
	if res.IsError {
		t.Fatalf("list_for_run: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	rows := out["evaluations"].([]any)
	if len(rows) != 2 {
		t.Errorf("expected 2 rows, got %d", len(rows))
	}
}

func TestEvaluationTool_AggregateRequiresDefID(t *testing.T) {
	tool, _, _, cleanup := evaluationFixture(t)
	defer cleanup()
	ctx := ctxWithEmitter("a_target", "read_any")

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"aggregate"}`))
	if !res.IsError {
		t.Fatalf("aggregate without def_id should refuse; got %s", res.Text)
	}
}

func TestEvaluationTool_OversizedJudgement(t *testing.T) {
	tool, _, runID, cleanup := evaluationFixture(t)
	defer cleanup()
	tool.MaxJudgementBytes = 32 // tight cap for the test

	ctx := ctxWithEmitter("a_target", "submit_self")
	big := `{"op":"submit","run_id":"` + runID + `","score":0.5,"judgement":` +
		`{"long":"` + strings.Repeat("x", 200) + `"}}`
	res, _ := tool.Execute(ctx, json.RawMessage(big))
	if !res.IsError {
		t.Fatalf("oversized judgement should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "exceeds max") {
		t.Errorf("error should mention exceeds max; got %s", res.Text)
	}
}

func TestEvaluationTool_UnknownOp(t *testing.T) {
	tool, _, _, cleanup := evaluationFixture(t)
	defer cleanup()
	ctx := ctxWithEmitter("a_target", "submit_any", "read_any")

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"frob"}`))
	if !res.IsError {
		t.Fatalf("unknown op should refuse; got %s", res.Text)
	}
}

func TestEvaluationTool_MissingOp(t *testing.T) {
	tool, _, _, cleanup := evaluationFixture(t)
	defer cleanup()
	ctx := ctxWithEmitter("a_target", "submit_any")

	res, _ := tool.Execute(ctx, json.RawMessage(`{}`))
	if !res.IsError {
		t.Fatalf("missing op should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "op") {
		t.Errorf("error should mention op; got %s", res.Text)
	}
}

func TestEvaluationTool_OversizedRawInputRejectedPreUnmarshal(t *testing.T) {
	// The per-field caps (MaxJudgementBytes / MaxRationaleBytes) fire
	// AFTER json.Unmarshal, which means without the pre-unmarshal guard
	// a 50 MB judgement would allocate 50 MB on heap before being
	// rejected. The pre-unmarshal guard caps the raw input at
	// (MaxJudgement + MaxRationale + envelopeHeadroom) so this case
	// fails fast without the allocation.
	tool, _, _, cleanup := evaluationFixture(t)
	defer cleanup()
	tool.MaxJudgementBytes = 64
	tool.MaxRationaleBytes = 64
	ctx := ctxWithEmitter("a_target", "submit_self")

	// Build a payload larger than 64+64+4096 = 4224 bytes.
	huge := `{"op":"submit","run_id":"x","score":0.5,"rationale":"` +
		strings.Repeat("z", 8000) + `"}`
	res, _ := tool.Execute(ctx, json.RawMessage(huge))
	if !res.IsError {
		t.Fatalf("oversized raw input should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "exceeds max") {
		t.Errorf("error should mention exceeds max; got %s", res.Text)
	}
}

func TestEvaluationTool_NoStore(t *testing.T) {
	tool := &Evaluation{}
	ctx := ctxWithEmitter("a_target", "submit_self")
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"submit","run_id":"r1","score":0.5}`))
	if !res.IsError {
		t.Fatalf("no Store should refuse; got %s", res.Text)
	}
	if !strings.Contains(res.Text, "not configured") {
		t.Errorf("error should mention not configured; got %s", res.Text)
	}
}
