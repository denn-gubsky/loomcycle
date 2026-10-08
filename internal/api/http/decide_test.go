package http

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The run-less decision path's behaviour on the real surfaces (the key
// restriction, the budget, the ledger row, the scope gates) is tested in
// internal/api/decidetest, through real authentication. These are the two
// seams only this package can reach.

// TestDecision_ABareRunlessDispatchIsStillRefused — the server's generic
// builtin dispatch, with the context an MCP or admin surface stamps (a
// principal, an identity, no run), must NOT reach a decision model: that
// context carries no key restriction and nobody to charge. Only
// Server.Decision prepares a call without a run.
func TestDecision_ABareRunlessDispatchIsStillRefused(t *testing.T) {
	e := newDecisionRunEnv(t, true, map[string]config.AgentDef{}, nil)
	e.srv.cfg().Env.OperatorKeyRestriction = true
	ctx := auth.WithPrincipal(context.Background(), restrictedPrincipal())
	ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{TenantID: "acme", UserID: "alice", AgentID: "a_mcp-operator"})
	ctx = tools.WithAgentName(ctx, "mcp-operator")

	res, err := e.srv.dispatchBuiltin(ctx, "Decision", json.RawMessage(decisionCall))
	if err != nil {
		t.Fatalf("dispatchBuiltin: %v", err)
	}
	if !res.IsError || !strings.HasPrefix(res.Text, "Decision: no_run: ") {
		t.Errorf("result = %q, want the no_run refusal", res.Text)
	}
	if n := e.endpoint.count(); n != 0 {
		t.Errorf("a bare run-less dispatch reached the decision model %d times", n)
	}
	// The same server and principal through the run-less path: refused there
	// too, but for the key, which is the rule the bare dispatch would have
	// skipped.
	res, err = e.srv.Decision(auth.WithPrincipal(context.Background(), restrictedPrincipal()), json.RawMessage(decisionCall))
	if err != nil || !strings.HasPrefix(res.Text, "Decision: operator_key_restricted: ") || e.endpoint.count() != 0 {
		t.Errorf("Server.Decision = %q, %v after %d model calls; want the key refusal and no call", res.Text, err, e.endpoint.count())
	}
}

// TestRecordRunSideCallUsage_BillsARunlessCallOnlyWhenAdmitted — a model call
// with no run is charged only when the server admitted it as one
// (tools.WithMeteredOffRunCall). "No run id" alone records nothing: that is
// also an operator route dispatching a tool directly (a Memory search's
// rerank), which must not start being billed.
func TestRecordRunSideCallUsage_BillsARunlessCallOnlyWhenAdmitted(t *testing.T) {
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := New(&config.Config{}, &stubResolver{}, []tools.Tool{}, concurrency.New(1, 1, time.Second), st)
	u := &providers.Usage{InputTokens: 1116, OutputTokens: 4, Provider: "ollama", Model: "nimble", CredentialSource: "tenant"}

	// An identity, as every admin and MCP dispatch stamps, is not an admission.
	bare := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "acme", UserID: "alice"})
	srv.RecordRunSideCallUsage(bare, u)
	if rows, _ := st.TokenUsageForRun(context.Background(), ""); len(rows) != 0 {
		t.Fatalf("a run-less call nobody admitted was billed: %+v", rows)
	}
	if used := srv.limits.UsedFor("user", "acme", "alice"); used != 0 {
		t.Fatalf("a run-less call nobody admitted moved the budget counter to %d", used)
	}

	admitted := tools.WithMeteredOffRunCall(bare, tools.MeteredOffRunCallValue{TenantID: "acme", UserID: "alice"})
	srv.RecordRunSideCallUsage(admitted, u)
	rows, err := st.TokenUsageForRun(context.Background(), "")
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %+v, %v; want the one admitted call", rows, err)
	}
	r := rows[0]
	if r.RunID != "" || r.SessionID != "" || r.AgentID != "" || r.TenantID != "acme" || r.UserID != "alice" ||
		r.InputTokens != 1116 || r.OutputTokens != 4 || r.CredentialSource != "tenant" {
		t.Errorf("row = %+v, want acme/alice's 1116+4 tokens on the tenant's key with no run", r)
	}
	if used := srv.limits.UsedFor("user", "acme", "alice"); used != 1120 {
		t.Errorf("the budget counter = %d, want 1120", used)
	}
}

// TestDecideFailureStatus_ARefusalThatIsNotTheToolsOwn — a failed result whose
// text carries no decision code was refused before the tool ran (the
// dispatcher turning away an unknown argument). When it is classified as the
// caller's input it is a 400 invalid_input, never a 502 that reads as the
// provider's fault; with nothing to go on it is a failed call.
func TestDecideFailureStatus_ARefusalThatIsNotTheToolsOwn(t *testing.T) {
	for _, c := range []struct {
		name   string
		res    connector.ToolResult
		status int
		code   string
	}{
		{"an unknown argument", connector.ToolResult{IsError: true, Text: `unknown field "op"`,
			ErrorInfo: &tools.ErrorInfo{Category: tools.CategoryValidation}}, 400, "invalid_input"},
		{"unclassified", connector.ToolResult{IsError: true, Text: "something else"}, 502, "call_failed"},
		{"a code this table does not know", connector.ToolResult{IsError: true, Text: "Decision: brand_new: x",
			ErrorInfo: &tools.ErrorInfo{Category: tools.CategoryTransient, Retryable: true}}, 502, "brand_new"},
		{"a new code for the caller's own mistake", connector.ToolResult{IsError: true, Text: "Decision: brand_new: x",
			ErrorInfo: &tools.ErrorInfo{Category: tools.CategoryValidation}}, 400, "brand_new"},
		{"no_run, which this path never produces", connector.ToolResult{IsError: true, Text: "Decision: no_run: x",
			ErrorInfo: &tools.ErrorInfo{Category: tools.CategoryBusiness}}, 502, "no_run"},
	} {
		if status, code := decideFailureStatus(c.res); status != c.status || code != c.code {
			t.Errorf("%s: (%d, %q), want (%d, %q)", c.name, status, code, c.status, c.code)
		}
	}
}
