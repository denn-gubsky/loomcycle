package http

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
