package config

// The blocks validate checks, in its order; see validate.

import (
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// validateEmbedder checks the memory.embedder block when it is set. Part of
// validate.
func validateEmbedder(c *Config) error {
	// v0.9.0 Vector Memory: validate the memory.embedder block when
	// set. Empty block = vector ops refuse with embedder_not_configured
	// at the tool layer (caught at first use, not boot). Set block
	// must have a known provider AND a model.
	if c.Memory.Embedder.Provider != "" || c.Memory.Embedder.Model != "" ||
		c.Memory.Embedder.BaseURL != "" || c.Memory.Embedder.APIKeyEnv != "" ||
		c.Memory.Embedder.Dimensions != 0 || c.Memory.Embedder.API != "" {
		if err := c.requireModelKind("memory.embedder.model", c.Memory.Embedder.Model, ModelKindEmbedder); err != nil {
			return err
		}
		provider, model, err := c.ExpandServiceModel("memory.embedder", c.Memory.Embedder.Provider, c.Memory.Embedder.Model)
		if err != nil {
			return err
		}
		if provider == "" {
			return fmt.Errorf("memory.embedder: provider is required when embedder block is set (or name a models: alias that carries one)")
		}
		if model == "" {
			return fmt.Errorf("memory.embedder: model is required when embedder block is set")
		}
		key, err := c.EmbedderKey(provider)
		if err != nil {
			return err
		}
		if err := providers.ValidateEmbedder(key, model, c.Memory.Embedder.Dimensions); err != nil {
			return fmt.Errorf("memory.embedder: %w", err)
		}
		if c.Memory.Embedder.TimeoutMs < 0 {
			return fmt.Errorf("memory.embedder.timeout_ms must be >= 0")
		}
		if c.Memory.Embedder.BatchSize < 0 {
			return fmt.Errorf("memory.embedder.batch_size must be >= 0")
		}
		if c.Memory.Embedder.Dimensions < 0 {
			return fmt.Errorf("memory.embedder.dimensions must be >= 0")
		}
		if c.Memory.Embedder.BaseURL != "" {
			if err := requireHTTPBaseURL("memory.embedder.base_url", c.Memory.Embedder.BaseURL); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateReranker checks the memory.reranker block when it is set. Part of
// validate.
func validateReranker(c *Config) error {
	// memory.reranker: structural checks here; whether the provider exists is
	// decided when it is built, against the fully-layered providers map, and a
	// provider that does not exist fails boot there.
	if rr := c.Memory.Reranker; rr.isSet() {
		// The kind of model follows the kind of rerank; an unknown rerank kind is
		// refused below, under its own name.
		wantKind := ModelKindChat
		if rr.EffectiveKind() == RerankerKindDecision {
			wantKind = ModelKindDecision
		}
		if err := c.requireModelKind("memory.reranker.model", rr.Model, wantKind); err != nil {
			return err
		}
		provider, model, err := c.ExpandServiceModel("memory.reranker", rr.Provider, rr.Model)
		if err != nil {
			return err
		}
		if provider == "" {
			return fmt.Errorf("memory.reranker: provider is required when the reranker block is set (or name a models: alias that carries one)")
		}
		if model == "" {
			return fmt.Errorf("memory.reranker: model is required when the reranker block is set")
		}
		if rr.TimeoutMs < 0 {
			return fmt.Errorf("memory.reranker.timeout_ms must be >= 0")
		}
		if rr.ContextTokens < 0 {
			return fmt.Errorf("memory.reranker.context_tokens must be >= 0")
		}
		for _, src := range rr.Sources {
			known := false
			for _, n := range RerankerSourceNames {
				known = known || src == n
			}
			if !known {
				return fmt.Errorf("memory.reranker.sources: %q is not one of documents, facts, notes, traces", src)
			}
		}
		switch rr.EffectiveKind() {
		case RerankerKindListwise:
		case RerankerKindDecision:
			// A decision call has no reasoning to switch off and no context window to
			// request. A copied listwise block must not look configured while these
			// are silently ignored.
			if rr.Effort != "" {
				return fmt.Errorf("memory.reranker.effort does not apply to kind: decision (remove it)")
			}
			if rr.ContextTokens != 0 {
				return fmt.Errorf("memory.reranker.context_tokens does not apply to kind: decision (remove it)")
			}
		default:
			return fmt.Errorf("memory.reranker.kind: %q is not one of listwise, decision", rr.Kind)
		}
		switch rr.Effort {
		case "", "low", "medium", "high":
		default:
			return fmt.Errorf("memory.reranker.effort: %q is not one of low, medium, high", rr.Effort)
		}
		if rr.BaseURL != "" {
			if err := requireHTTPBaseURL("memory.reranker.base_url", rr.BaseURL); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateConsolidationBands checks the memory consolidation similarity
// bands. Part of validate.
func validateConsolidationBands(c *Config) error {
	// Consolidation similarity bands. Validated on the EFFECTIVE values so
	// setting only one of the pair is still checked against the other's
	// default — an operator who lowers merge_threshold to 0.80 and leaves
	// related_threshold at 0.85 has inverted the bands and must hear about it
	// at boot, not by watching the pass mis-classify every fact.
	if cm := c.Memory.Consolidation; cm.MergeThreshold != 0 || cm.RelatedThreshold != 0 {
		merge, related := cm.EffectiveMergeThreshold(), cm.EffectiveRelatedThreshold()
		// Phrased as a NEGATED in-range test, not as an out-of-range test, so a
		// NaN fails it. yaml.v3 resolves `.nan` into a float64 NaN, and every
		// IEEE-754 comparison against NaN is false — so `merge <= 0 || merge > 1`
		// waves it through, config loads clean, and the consolidator's system
		// prompt ends up reading "similarity >= NaN". Inverting puts NaN in the
		// error branch for free. (`.inf` compares normally and was always caught.)
		if !(merge > 0 && merge <= 1) {
			return fmt.Errorf("memory.consolidation.merge_threshold must be > 0 and <= 1 (got %v)", merge)
		}
		if !(related > 0 && related <= 1) {
			return fmt.Errorf("memory.consolidation.related_threshold must be > 0 and <= 1 (got %v)", related)
		}
		if !(related < merge) {
			return fmt.Errorf("memory.consolidation: related_threshold (%v) must be < merge_threshold (%v)", related, merge)
		}
	}
	return nil
}
