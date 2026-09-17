package http

import (
	"errors"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

func boolPtr(b bool) *bool { return &b }

// RFC DC D7, the raisable half. max_tokens and max_iterations are bounded in
// practice by the per-scope token budget, so raising them changes what one run
// costs rather than what the deployment can spend.
func TestResourceOverride_TokensAndIterationsMayBeRaised(t *testing.T) {
	def := config.AgentDef{MaxTokens: 1000, MaxIterations: 4}

	got, err := applyResourceOverride(def, &resourceOverride{MaxTokens: 8000, MaxIterations: 40})
	if err != nil {
		t.Fatalf("raising was refused: %v", err)
	}
	if got.MaxTokens != 8000 || got.MaxIterations != 40 {
		t.Errorf("got max_tokens=%d max_iterations=%d, want 8000/40", got.MaxTokens, got.MaxIterations)
	}
}

// A pointer, so "bound this runaway agent for one run" is expressible. A plain
// bool could only ever raise the cap.
func TestResourceOverride_UnboundedIterationsTurnsOffAsWellAsOn(t *testing.T) {
	unbounded := config.AgentDef{UnboundedIterations: true}
	got, err := applyResourceOverride(unbounded, &resourceOverride{UnboundedIterations: boolPtr(false)})
	if err != nil {
		t.Fatal(err)
	}
	if got.UnboundedIterations {
		t.Error("could not bound an unbounded agent for one run")
	}

	bounded := config.AgentDef{}
	got, err = applyResourceOverride(bounded, &resourceOverride{UnboundedIterations: boolPtr(true)})
	if err != nil || !got.UnboundedIterations {
		t.Errorf("unbounded=%v err=%v; want it raised", got.UnboundedIterations, err)
	}
}

// RFC DC D7, the load-bearing half. Fan-out width may ONLY be lowered, because
// it is the only bound on sub-agent fan-out that exists: a child takes no
// admission slot, is not budget-checked at spawn, and skips the per-provider
// gate for its parent's provider.
func TestResourceOverride_FanoutMayOnlyBeLowered(t *testing.T) {
	def := config.AgentDef{MaxConcurrentChildren: 8}

	got, err := applyResourceOverride(def, &resourceOverride{MaxConcurrentChildren: 2})
	if err != nil {
		t.Fatalf("lowering was refused: %v", err)
	}
	if got.MaxConcurrentChildren != 2 {
		t.Errorf("max_concurrent_children = %d, want 2", got.MaxConcurrentChildren)
	}

	_, err = applyResourceOverride(def, &resourceOverride{MaxConcurrentChildren: 16})
	if !errors.Is(err, runner.ErrInvalidArgument) {
		t.Errorf("err = %v, want a refusal — raising fan-out would leave its width unbounded", err)
	}
}

// "Declares nothing" must not mean "unlimited". An agent with no explicit value
// is already capped at the Agent tool's default, and that is the ceiling an
// override is measured against — otherwise the commonest definition shape would
// be the one with no bound at all.
func TestResourceOverride_UndeclaredFanoutCeilingIsTheToolDefault(t *testing.T) {
	def := config.AgentDef{} // declares nothing

	_, err := applyResourceOverride(def, &resourceOverride{
		MaxConcurrentChildren: builtin.DefaultMaxConcurrentChildren + 1,
	})
	if !errors.Is(err, runner.ErrInvalidArgument) {
		t.Errorf("err = %v, want a refusal: an undeclared agent is capped at the tool default (%d), "+
			"not unbounded", err, builtin.DefaultMaxConcurrentChildren)
	}

	if _, err := applyResourceOverride(def, &resourceOverride{
		MaxConcurrentChildren: builtin.DefaultMaxConcurrentChildren,
	}); err != nil {
		t.Errorf("lowering to exactly the default was refused: %v", err)
	}
}

// The definition is never mutated — what keeps an override out of content_sha256.
func TestResourceOverride_LeavesTheDefinitionUntouched(t *testing.T) {
	def := config.AgentDef{MaxTokens: 1000, MaxIterations: 4, MaxConcurrentChildren: 8}
	if _, err := applyResourceOverride(def, &resourceOverride{
		MaxTokens: 9000, MaxIterations: 40, MaxConcurrentChildren: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if def.MaxTokens != 1000 || def.MaxIterations != 4 || def.MaxConcurrentChildren != 8 {
		t.Errorf("the caller's definition was mutated: %+v", def)
	}
}

func TestResourceOverride_ZeroIsANoOp(t *testing.T) {
	def := config.AgentDef{MaxTokens: 1000, MaxConcurrentChildren: 8}
	for _, ov := range []*resourceOverride{nil, {}} {
		got, err := applyResourceOverride(def, ov)
		if err != nil {
			t.Fatalf("%+v: %v", ov, err)
		}
		if got.MaxTokens != 1000 || got.MaxConcurrentChildren != 8 {
			t.Errorf("%+v changed the definition: %+v", ov, got)
		}
	}
}
