package wiring

import (
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lanegate"
	"github.com/denn-gubsky/loomcycle/internal/memory/unitgen"
	"github.com/denn-gubsky/loomcycle/internal/providerbuild"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// unitGeneratorKeyEnvName is the credential a unit generator answers to when its
// endpoint is overridden without a named key — see providerbuild.ServiceProvider.
const unitGeneratorKeyEnvName = "LOOMCYCLE_UNIT_GENERATOR_API_KEY"

// UnitGenerator constructs the generator declared in cfg.Memory.UnitGenerator,
// or returns nil when none is declared. A declared one that cannot be built is
// an error.
//
// The generator makes its calls on a driver of its own, so that driver is
// wrapped in the chat lane of the provider the block resolved to (sched, the
// server's call scheduler): a generation call is counted by the provider's
// max_concurrent like any other chat call. nil gates nothing.
func UnitGenerator(cfg *config.Config, sched *concurrency.CallScheduler) (*unitgen.Generator, error) {
	p, gc, err := unitGeneratorDriver(cfg, sched)
	if err != nil || p == nil {
		return nil, err
	}
	return unitgen.New(p, gc), nil
}

// unitGeneratorDriver is the generator's driver, in its provider's chat lane,
// and the block with the provider and model it resolved to; a nil driver when no
// block is declared.
func unitGeneratorDriver(cfg *config.Config, sched *concurrency.CallScheduler) (providers.Provider, config.UnitGeneratorConfig, error) {
	gc := cfg.Memory.UnitGenerator
	if !gc.Configured() {
		return nil, gc, nil
	}
	p, provider, model, err := providerbuild.ServiceProvider(cfg, "memory.unit_generator", providerbuild.ServiceEndpoint{
		Provider: gc.Provider, Model: gc.Model, BaseURL: gc.BaseURL, APIKeyEnv: gc.APIKeyEnv,
	}, unitGeneratorKeyEnvName)
	if err != nil {
		return nil, gc, err
	}
	gc.Provider, gc.Model = provider, model
	return lanegate.Wrap(provider, p, sched), gc, nil
}
