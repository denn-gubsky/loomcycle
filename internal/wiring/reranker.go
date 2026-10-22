package wiring

import (
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/decision"
	"github.com/denn-gubsky/loomcycle/internal/lanegate"
	"github.com/denn-gubsky/loomcycle/internal/memory/reranker"
	"github.com/denn-gubsky/loomcycle/internal/providerbuild"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// rerankerKeyEnvName is the credential name a reranker answers to when the
// operator pointed it at its own endpoint without naming a key (see
// providerbuild.ServiceDriverOptions).
const rerankerKeyEnvName = "LOOMCYCLE_RERANKER_API_KEY"

// Reranker constructs the reranker cfg.Memory.Reranker declares, of its kind,
// or returns nil (an untyped nil) when none is declared. A declared reranker that
// cannot be built is an error: an operator who configured one expects it to run,
// and would otherwise learn otherwise only from `reranked: false` on every search.
//
// sched is the server's call scheduler: the reranker's calls take the lane of
// their kind on the provider the block resolved to (chat for listwise, decision
// for the decision kind). nil gates nothing — a caller with no server
// scheduler, such as a bench command.
func Reranker(cfg *config.Config, sched *concurrency.CallScheduler) (reranker.Reranker, error) {
	rc := cfg.Memory.Reranker
	if !rc.Configured() {
		return nil, nil
	}
	if rc.EffectiveKind() == config.RerankerKindDecision {
		d, err := decisionReranker(cfg, rc, sched)
		if err != nil {
			return nil, err
		}
		return d, nil
	}
	m, err := listwiseReranker(cfg, sched)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// listwiseReranker constructs the listwise reranker cfg.Memory.Reranker
// declares, or nil when none is declared. Reranker is the production path, for
// either kind.
func listwiseReranker(cfg *config.Config, sched *concurrency.CallScheduler) (*reranker.Model, error) {
	p, rc, err := listwiseDriver(cfg, sched)
	if err != nil || p == nil {
		return nil, err
	}
	return reranker.New(p, rc), nil
}

// listwiseDriver is the listwise reranker's driver, in its provider's chat lane,
// and the block with the provider and model it resolved to; a nil driver when no
// block is declared.
func listwiseDriver(cfg *config.Config, sched *concurrency.CallScheduler) (providers.Provider, config.RerankerConfig, error) {
	rc := cfg.Memory.Reranker
	if !rc.Configured() {
		return nil, rc, nil
	}
	if rc.EffectiveKind() != config.RerankerKindListwise {
		return nil, rc, fmt.Errorf("memory.reranker: kind %q is not built as a listwise model (use Reranker)", rc.Kind)
	}
	p, provider, model, err := providerbuild.ServiceProvider(cfg, "memory.reranker", providerbuild.ServiceEndpoint{
		Provider: rc.Provider, Model: rc.Model, BaseURL: rc.BaseURL, APIKeyEnv: rc.APIKeyEnv,
	}, rerankerKeyEnvName)
	if err != nil {
		return nil, rc, err
	}
	rc.Provider, rc.Model = provider, model
	// Under the id the block RESOLVED to: an alias may have supplied it. With a
	// floor: while that provider has no chat lane, reranks are still bounded.
	return lanegate.WrapWithFloor(provider, p, sched), rc, nil
}

// decisionReranker builds the decision reranker rc declares, its driver in the
// decision lane of the provider the block resolved to.
func decisionReranker(cfg *config.Config, rc config.RerankerConfig, sched *concurrency.CallScheduler) (*reranker.Decision, error) {
	rc, driver, opts, err := decisionEndpoint(cfg, rc)
	if err != nil {
		return nil, err
	}
	return reranker.NewDecision(rc, driver, opts, func(d decision.Driver) decision.Driver {
		return lanegate.WrapDecision(rc.Provider, d, sched)
	})
}

// decisionEndpoint resolves a decision reranker's endpoint and key exactly as a
// listwise reranker's (providerbuild.ServiceDriverOptions): rc with the provider
// and model it resolved to, the provider's driver, and the driver options.
func decisionEndpoint(cfg *config.Config, rc config.RerankerConfig) (config.RerankerConfig, string, providers.DriverOptions, error) {
	opts, provider, model, driver, err := providerbuild.ServiceDriverOptions(cfg, "memory.reranker", providerbuild.ServiceEndpoint{
		Provider: rc.Provider, Model: rc.Model, BaseURL: rc.BaseURL, APIKeyEnv: rc.APIKeyEnv,
	}, rerankerKeyEnvName)
	if err != nil {
		return rc, "", opts, err
	}
	rc.Provider, rc.Model = provider, model
	return rc, driver, opts, nil
}
