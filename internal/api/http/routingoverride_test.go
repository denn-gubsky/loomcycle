package http

import (
	"context"
	"testing"

	"errors"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
	"github.com/denn-gubsky/loomcycle/internal/runner"
)

// routingServer builds a server with two tiers and two reachable providers, so
// an override has somewhere real to route to and somewhere real to be refused.
func routingServer(t *testing.T) *Server {
	t.Helper()
	cfg := makeBaseConfig()
	cfg.Tiers = map[string][]config.TierCandidate{
		"middle": {{Provider: "primary", Model: "model-a"}, {Provider: "secondary", Model: "model-b"}},
		"cheap":  {{Provider: "secondary", Model: "model-b"}},
	}
	srv, _ := makeServer(t, completingProvider(), cfg)
	res := resolve.NewResolver([]string{"primary", "secondary"}, map[string][]resolve.Candidate{
		"middle": {{Provider: "primary", Model: "model-a"}, {Provider: "secondary", Model: "model-b"}},
		"cheap":  {{Provider: "secondary", Model: "model-b"}},
	})
	res.SetReachable("primary", true, []string{"model-a"}, "")
	res.SetReachable("secondary", true, []string{"model-b"}, "")
	srv.SetResolver(res)
	return srv
}

func TestRoutingOverride_TierIsSelectedWithinOperatorConfig(t *testing.T) {
	srv := routingServer(t)
	def := config.AgentDef{Tier: "middle"}

	got, err := srv.applyRoutingOverride(context.Background(), def, &routingOverride{Tier: "cheap"})
	if err != nil {
		t.Fatalf("applyRoutingOverride: %v", err)
	}
	if got.Tier != "cheap" {
		t.Errorf("tier = %q, want cheap", got.Tier)
	}

	// D6: operator configuration is not overridable — only the selection within it.
	_, err = srv.applyRoutingOverride(context.Background(), def, &routingOverride{Tier: "invented"})
	if !errors.Is(err, runner.ErrInvalidArgument) {
		t.Errorf("err = %v, want ErrInvalidArgument for a tier nobody configured", err)
	}
}

// RFC DC V5. The definition's declared set is the boundary: an override selects
// within it and cannot widen it, which is what keeps WHICH VENDOR sees the
// conversation an operator decision.
func TestRoutingOverride_ModelMustBeOneTheDefinitionAllows(t *testing.T) {
	srv := routingServer(t)
	declared := config.AgentDef{
		Tier: "middle",
		Models: map[string][]config.TierCandidate{
			"middle": {{Provider: "primary", Model: "model-a"}},
		},
	}

	if _, err := srv.applyRoutingOverride(context.Background(), declared, &routingOverride{Model: "model-a"}); err != nil {
		t.Errorf("a declared model was refused: %v", err)
	}
	_, err := srv.applyRoutingOverride(context.Background(), declared, &routingOverride{Model: "model-b"})
	if !errors.Is(err, runner.ErrInvalidArgument) {
		t.Errorf("err = %v, want a refusal: model-b is outside the definition's declared set", err)
	}
}

// With NO declared set the operator did not restrict the agent, so the live
// cascade is the boundary — and it also says which provider serves the model.
func TestRoutingOverride_UndeclaredAgentIsBoundedByTheLiveCascade(t *testing.T) {
	srv := routingServer(t)
	def := config.AgentDef{Tier: "middle"}

	got, err := srv.applyRoutingOverride(context.Background(), def, &routingOverride{Model: "model-b"})
	if err != nil {
		t.Fatalf("model-b is in the middle cascade but was refused: %v", err)
	}
	if got.Provider != "secondary" {
		t.Errorf("provider = %q, want secondary — derived from the cascade, not guessed", got.Provider)
	}
	if got.Tier != "" {
		t.Error("naming a model must PIN: a cascade that may route elsewhere is not an answer to " +
			"'use this model'")
	}

	if _, err := srv.applyRoutingOverride(context.Background(), def, &routingOverride{Model: "model-nowhere"}); !errors.Is(err, runner.ErrInvalidArgument) {
		t.Errorf("err = %v, want a refusal for a model no cascade reaches", err)
	}
}

// A provider override alone NARROWS the cascade rather than collapsing it.
// Clearing the tier would trade fallback for a pin the caller never asked for.
func TestRoutingOverride_ProviderAloneNarrowsButKeepsTheTier(t *testing.T) {
	srv := routingServer(t)
	def := config.AgentDef{Tier: "middle"}

	got, err := srv.applyRoutingOverride(context.Background(), def, &routingOverride{Provider: "secondary"})
	if err != nil {
		t.Fatalf("applyRoutingOverride: %v", err)
	}
	if got.Tier != "middle" {
		t.Errorf("tier = %q, want it kept — the caller named a vendor, not a model", got.Tier)
	}
	if len(got.Providers) != 1 || got.Providers[0] != "secondary" {
		t.Errorf("providers = %v, want the cascade narrowed to [secondary]", got.Providers)
	}

	pinned := config.AgentDef{Providers: []string{"primary"}}
	if _, err := srv.applyRoutingOverride(context.Background(), pinned, &routingOverride{Provider: "secondary"}); !errors.Is(err, runner.ErrInvalidArgument) {
		t.Errorf("err = %v, want a refusal: secondary is outside the definition's declared providers", err)
	}
}

// An unknown effort is refused at the wire rather than dropped: "effort was
// ignored" and "effort was applied" are indistinguishable from the outside.
func TestRoutingOverride_UnknownEffortIsRefusedNotDropped(t *testing.T) {
	srv := routingServer(t)
	def := config.AgentDef{Tier: "middle"}

	if _, err := srv.applyRoutingOverride(context.Background(), def, &routingOverride{Effort: "extreme"}); !errors.Is(err, runner.ErrInvalidArgument) {
		t.Errorf("err = %v, want ErrInvalidArgument", err)
	}
	got, err := srv.applyRoutingOverride(context.Background(), def, &routingOverride{Effort: "high"})
	if err != nil || got.Effort != "high" {
		t.Errorf("effort = %q, err = %v; want high", got.Effort, err)
	}
}

// The definition is never mutated — that is what keeps an override out of
// content_sha256 (D2). Asserted on the caller's own value, since a method that
// took a pointer would pass every test above and still corrupt the stored def.
func TestRoutingOverride_LeavesTheDefinitionUntouched(t *testing.T) {
	srv := routingServer(t)
	def := config.AgentDef{Tier: "middle", Effort: "low", Providers: []string{"primary", "secondary"}}

	if _, err := srv.applyRoutingOverride(context.Background(), def, &routingOverride{
		Tier: "cheap", Effort: "high", Provider: "secondary",
	}); err != nil {
		t.Fatal(err)
	}
	if def.Tier != "middle" || def.Effort != "low" {
		t.Errorf("the caller's definition was mutated: tier=%q effort=%q", def.Tier, def.Effort)
	}
	if len(def.Providers) != 2 {
		t.Errorf("the caller's Providers slice was mutated: %v", def.Providers)
	}
}

func TestRoutingOverride_ZeroIsANoOp(t *testing.T) {
	srv := routingServer(t)
	def := config.AgentDef{Tier: "middle", Effort: "low"}
	for _, ov := range []*routingOverride{nil, {}} {
		got, err := srv.applyRoutingOverride(context.Background(), def, ov)
		if err != nil {
			t.Fatalf("%+v: %v", ov, err)
		}
		if got.Tier != "middle" || got.Effort != "low" {
			t.Errorf("%+v changed the definition: %+v", ov, got)
		}
	}
}
