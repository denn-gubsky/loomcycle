package http

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC FY Part E, D5: `use tier:` and `use model:` are asked when a run's
// model is resolved — at its start, a sub-run's, and a provider fallback's
// re-resolution (all of which carry the caller's set or the run's).

func runACL(t *testing.T, rules ...authz.Rule) *authz.Effective {
	t.Helper()
	eff, err := authz.Compile(authz.Input{Subject: authz.Subject{TenantID: "t", ID: "u"},
		Bindings: []authz.Binding{{ACL: "use", SubjectKind: authz.SubjectTenant}},
		ACLs:     map[string]authz.ACLVersion{"use": {ACL: authz.ACL{Name: "use", Rules: rules}}}, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	return eff
}

func TestResolveAgentDef_AsksTheCallersUseOfTheTierAndTheModel(t *testing.T) {
	f := newQueueRouteFixture(t, true)
	def := f.s.cfg().Agents["worker"]
	def.Routing = nil
	inRun := func(rules ...authz.Rule) context.Context {
		return tools.WithRunPolicy(context.Background(), tools.RunPolicy{ACL: runACL(t, rules...)})
	}
	deny := func(verb, sel string) authz.Rule {
		return authz.Rule{Effect: authz.Deny, Actions: []string{verb}, Resources: []string{sel}}
	}
	resolveIn := func(ctx context.Context) (string, error) {
		_, model, _, err := f.s.resolveAgentDef(ctx, def, "", "u", "worker", "", false)
		return model, err
	}
	if m, err := resolveIn(inRun(deny("use", "model:other-model"))); err != nil || m != "m1" {
		t.Fatalf("unrelated deny: %q %v, want m1", m, err)
	}
	if m, err := resolveIn(inRun(deny("use", "model:m1"))); err != nil || m != "m2" {
		t.Errorf("a denied model was not passed over: %q %v, want m2", m, err)
	}
	for name, ctx := range map[string]context.Context{
		"every model": inRun(deny("use", "model:*")),
		"the tier":    inRun(deny("use", "tier:low")),
	} {
		if _, err := resolveIn(ctx); !errors.Is(err, runner.ErrAgentNotAllowed) || !strings.Contains(err.Error(), "acl_denied") {
			t.Errorf("%s denied: %v, want an acl_denied refusal", name, err)
		}
	}
	// A view resolves without asking: no run, no run start.
	if m, err := resolveIn(context.Background()); err != nil || m != "m1" {
		t.Errorf("a view: %q %v", m, err)
	}
	// A pinned agent has no other candidate: a deny on its model refuses.
	pinned := def
	pinned.Tier, pinned.Provider, pinned.Model = "", "busy", "m1"
	if _, _, _, err := f.s.resolveAgentDef(inRun(deny("use", "model:m1")), pinned, "", "u", "worker", "", false); !errors.Is(err, runner.ErrAgentNotAllowed) {
		t.Errorf("a pinned agent on a denied model: %v, want an acl_denied refusal", err)
	}
}

// At a run's start the caller's own rules are asked: a deny on the model the
// agent is pinned to refuses POST /v1/runs.
func TestACLGate_AModelDenyRefusesTheRunAtItsStart(t *testing.T) {
	ts, st := aclRunFixtureMode(t, authz.ModeCompat)
	if code, out := startCondWorker(t, ts, "a_model_1", ""); code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	bindTenantWide(t, st, "no-stub", `[{"effect":"deny","actions":["use"],"resources":["model:stub-model"]}]`)
	if code, out := startCondWorker(t, ts, "a_model_2", ""); code != 403 || !strings.Contains(out, "acl_denied") {
		t.Errorf("a run on a denied model: %d %s, want 403 acl_denied", code, out)
	}
}

// The caller's set on the request, as an HTTP, gRPC or MCP principal carries
// it.
func asCaller(t *testing.T, rules ...authz.Rule) context.Context {
	t.Helper()
	return auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "t", Subject: "u", Effective: runACL(t, rules...)})
}

// Every resolution that leads to a model call asks, not only a run's start:
// the LLM gateway, pinned or resolved, a provider fallback's re-resolution
// (resolveAgent on the run's ctx) and a compaction's — and a decision call.
func TestModelUse_EveryInferencePathAsks(t *testing.T) {
	f := newQueueRouteFixture(t, true)
	denyM1 := authz.Rule{Effect: authz.Deny, Actions: []string{"use"}, Resources: []string{"model:m1"}}
	denied := func(err error) bool {
		return errors.Is(err, runner.ErrAgentNotAllowed) && strings.Contains(err.Error(), "acl_denied")
	}

	// The gateway: a pin on the denied model is refused; a tier call passes
	// it over for the next permitted one.
	if _, _, _, err := f.s.resolveGatewayRequest(asCaller(t, denyM1), &llmChatRequest{Provider: "busy", Model: "m1"}); !denied(err) {
		t.Errorf("gateway pin on a denied model: %v", err)
	}
	if _, m, _, err := f.s.resolveGatewayRequest(asCaller(t, denyM1), &llmChatRequest{Tier: "low"}); err != nil || m != "m2" {
		t.Errorf("gateway tier call: %q %v, want m2", m, err)
	}
	if _, _, _, err := f.s.resolveGatewayRequest(asCaller(t, authz.Rule{Effect: authz.Deny, Actions: []string{"use"}, Resources: []string{"model:*"}}), &llmChatRequest{Tier: "low"}); !denied(err) {
		t.Errorf("gateway tier call with every model denied: %v", err)
	}

	// A provider fallback re-resolves through resolveAgent on the run's ctx,
	// which carries no run-start mark.
	inRun := tools.WithRunPolicy(context.Background(), tools.RunPolicy{ACL: runACL(t, denyM1)})
	if _, m, _, err := f.s.resolveAgent(inRun, "", "u", "worker", "", false, nil); err != nil || m != "m2" {
		t.Errorf("fallback re-resolution: %q %v, want m2", m, err)
	}
	// A resolution for an off-run model call (a compaction, a recap) on the
	// caller's principal asks as well.
	if _, m, _, err := f.s.resolveAgent(asCaller(t, denyM1), "", "u", "worker", "", false, nil); err != nil || m != "m2" {
		t.Errorf("an off-run resolution: %q %v, want m2", m, err)
	}
	// A compaction's own model, and a decision model, are asked by name.
	if err := f.s.ModelUseGate(asCaller(t, denyM1), "m1"); !denied(err) {
		t.Errorf("a direct model call on a denied model: %v", err)
	}
	// A report-only resolution asks nothing.
	if _, m, _, err := f.s.resolveAgent(withReportOnlyResolution(asCaller(t, denyM1)), "", "u", "worker", "", false, nil); err != nil || m != "m1" {
		t.Errorf("a report: %q %v, want m1", m, err)
	}
}
