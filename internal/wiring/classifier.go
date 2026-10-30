package wiring

import (
	"fmt"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/decision"
	"github.com/denn-gubsky/loomcycle/internal/lanegate"
	"github.com/denn-gubsky/loomcycle/internal/memory/classifier"
	"github.com/denn-gubsky/loomcycle/internal/memory/reranker"
	"github.com/denn-gubsky/loomcycle/internal/providerbuild"
)

// classifierKeyEnvName is the credential name the classifier answers to when the
// operator pointed one of its models at its own endpoint without naming a key.
const classifierKeyEnvName = "LOOMCYCLE_CLASSIFIER_API_KEY"

// classifierMaxOutputTokens bounds the chat model's reply: one JSON object with a
// class, a quote under 200 characters and a few concept names. The rerank's 256
// cut Ukrainian replies off mid-object on MTB (2026-10-30), and a cut reply is
// dropped, so the budget leaves room to spare.
const classifierMaxOutputTokens = 1024

// Classifier constructs the classifier cfg.Memory.Classifier declares, or nil when
// none is declared. Each model is resolved exactly as the reranker's of its kind and
// takes that kind's lane on its provider: the decision model the decision lane, the
// chat model the chat lane (with the floor while the provider has none). A declared
// model that cannot be built is an error, as for the reranker.
func Classifier(cfg *config.Config, sched *concurrency.CallScheduler) (*classifier.Classifier, error) {
	cc := cfg.Memory.Classifier
	if !cc.Configured() {
		return nil, nil
	}
	var dec decision.Driver
	var decModel string
	var decTimeout time.Duration
	if m := cc.Decision; m.Configured() {
		opts, provider, model, driver, err := providerbuild.ServiceDriverOptions(cfg, "memory.classifier.decision", providerbuild.ServiceEndpoint{
			Provider: m.Provider, Model: m.Model, BaseURL: m.BaseURL, APIKeyEnv: m.APIKeyEnv,
		}, classifierKeyEnvName)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(opts.BaseURL) == "" {
			return nil, fmt.Errorf("memory.classifier.decision: provider %q has no base URL", provider)
		}
		decTimeout = time.Duration(m.TimeoutMs) * time.Millisecond
		if decTimeout <= 0 {
			decTimeout = 30 * time.Second
		}
		d, err := decision.New(driver, decision.Options{
			ProviderID: provider, BaseURL: strings.TrimRight(opts.BaseURL, "/"),
			APIKey: opts.APIKey, KeyEnvName: opts.KeyEnvName, Timeout: decTimeout,
		})
		if err != nil {
			return nil, fmt.Errorf("memory.classifier.decision: %w", err)
		}
		dec, decModel = lanegate.WrapDecision(provider, d, sched), model
	}
	var chat classifier.Completer
	if m := cc.Chat; m.Configured() {
		p, provider, model, err := providerbuild.ServiceProvider(cfg, "memory.classifier.chat", providerbuild.ServiceEndpoint{
			Provider: m.Provider, Model: m.Model, BaseURL: m.BaseURL, APIKeyEnv: m.APIKeyEnv,
		}, classifierKeyEnvName)
		if err != nil {
			return nil, err
		}
		// The listwise reranker's model is a one-prompt chat call at temperature 0 with a
		// timeout, a context window and usage reporting — exactly what the chat step needs.
		cm := reranker.New(lanegate.WrapWithFloor(provider, p, sched), config.RerankerConfig{
			Provider: provider, Model: model, TimeoutMs: m.TimeoutMs, Effort: m.Effort, ContextTokens: m.ContextTokens,
		})
		cm.MaxOutputTokens = classifierMaxOutputTokens
		chat = cm
	}
	return classifier.New(dec, decModel, decTimeout, chat, cc.EffectiveMinProbability(), cc.EffectiveMinMargin()), nil
}
