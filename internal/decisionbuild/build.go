// Package decisionbuild turns a loaded config's `decision:` block into a
// decision.Service.
//
// It is apart from internal/decision because config validates the block against
// the decision driver registry, so internal/decision cannot import config.
package decisionbuild

import (
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/decision"
	"github.com/denn-gubsky/loomcycle/internal/lanegate"
	"github.com/denn-gubsky/loomcycle/internal/providerbuild"
)

// Build constructs the decision service cfg.Decision declares, or returns nil
// when the block is unset. A declared block that cannot be built is an error:
// an operator who listed a model expects it to answer.
//
// Each entry resolves through providerbuild.ServiceDriverOptions, as the
// memory reranker and the unit generator do, so an alias reaches the provider
// as the model it names and the endpoint and key are the provider's own. One
// driver is built per provider.
//
// Every driver is handed out inside its provider's decision lane (sched, the
// server's call scheduler): providers.<id>.lanes.decision caps the decisions in
// flight to that provider, and its max_models_in_flight counts a decision
// model beside the chat models. A nil sched gates nothing — a caller with no
// server scheduler, such as a CLI command.
func Build(cfg *config.Config, sched *concurrency.CallScheduler) (*decision.Service, error) {
	dc := cfg.Decision
	if !dc.Configured() {
		return nil, nil
	}
	drivers := map[string]decision.Driver{}
	var specs []decision.ModelSpec
	for _, e := range cfg.DecisionEntries() {
		// No endpoint or key override, so the key name is always the provider's
		// own and the last argument (a service's own key name) is never used.
		opts, provider, model, driverName, err := providerbuild.ServiceDriverOptions(cfg, "decision",
			providerbuild.ServiceEndpoint{Provider: e.Provider, Model: e.Name}, "")
		if err != nil {
			return nil, err
		}
		d, ok := drivers[provider]
		if !ok {
			d, err = decision.New(driverName, decision.Options{
				ProviderID: provider, BaseURL: opts.BaseURL,
				APIKey: opts.APIKey, KeyEnvName: opts.KeyEnvName,
				Timeout: time.Duration(dc.TimeoutMs) * time.Millisecond,
			})
			if err != nil {
				return nil, fmt.Errorf("decision: %q on provider %q: %w", e.Name, provider, err)
			}
			d = lanegate.WrapDecision(provider, d, sched)
			drivers[provider] = d
		}
		specs = append(specs, decision.ModelSpec{Name: e.Name, Provider: provider, Model: model, Driver: d})
	}
	return decision.NewService(dc.Default, specs)
}
