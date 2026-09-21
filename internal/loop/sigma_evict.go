package loop

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// Σ retention classes, declared per property in the agent's `state_schema` under
// the `x-retention` keyword.
//
// Significance CANNOT be inferred from JSON — a key holding the task's spine and
// a key holding a scratch counter look identical to a walker. So the operator
// declares it, and anything undeclared is treated as load-bearing.
const (
	// RetentionCore is never evicted: the task's spine.
	RetentionCore = "core"
	// RetentionDerived is evictable — recomputable from core + observations.
	RetentionDerived = "derived"
	// RetentionScratch is evicted first: working notes.
	RetentionScratch = "scratch"

	// retentionKeyword is the JSON Schema extension the class is read from.
	// `x-` prefixed so a standard validator ignores it.
	retentionKeyword = "x-retention"
)

// retentionOf reads a property's declared class.
//
// ⚠️ DEFAULTS TO CORE, deliberately. An agent whose schema predates this
// feature — or whose author simply did not think about it — must not silently
// start losing state on upgrade. Eviction is opt-in per key, and the cost of
// that choice is that an undeclared schema gets no relief from Σ growth. It
// reports exhaustion instead, which is the honest failure.
func retentionOf(schema map[string]any, key string) string {
	props, _ := schema["properties"].(map[string]any)
	prop, _ := props[key].(map[string]any)
	switch c, _ := prop[retentionKeyword].(string); c {
	case RetentionScratch:
		return RetentionScratch
	case RetentionDerived:
		return RetentionDerived
	default:
		return RetentionCore
	}
}

// sigmaEvictionPlan chooses which Σ keys to drop to get under `budget` tokens.
//
// Order is scratch before derived, and least-recently-written first within a
// class — the class is the operator's statement of what matters, and recency is
// only the tiebreaker inside it. Core is never returned.
//
// It stops as soon as the remaining Σ fits: evicting more than necessary throws
// away context the run may still need, and the cheapest eviction is the one not
// performed.
func sigmaEvictionPlan(sigma map[string]any, schema map[string]any,
	lastWritten map[string]int, budget int) []string {
	if budget <= 0 || sigmaTokens(sigma) <= budget {
		return nil
	}
	type cand struct {
		key   string
		class string
		seq   int
	}
	var cands []cand
	for k := range sigma {
		c := retentionOf(schema, k)
		if c == RetentionCore {
			continue
		}
		cands = append(cands, cand{key: k, class: c, seq: lastWritten[k]})
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].class != cands[j].class {
			// scratch (evict first) sorts before derived.
			return cands[i].class == RetentionScratch
		}
		if cands[i].seq != cands[j].seq {
			return cands[i].seq < cands[j].seq // least recently written first
		}
		return cands[i].key < cands[j].key // stable for a deterministic plan
	})

	// Walk a copy so the projected size is measured, not guessed.
	remaining := make(map[string]any, len(sigma))
	for k, v := range sigma {
		remaining[k] = v
	}
	var plan []string
	for _, c := range cands {
		if sigmaTokens(remaining) <= budget {
			break
		}
		delete(remaining, c.key)
		plan = append(plan, c.key)
	}
	return plan
}

// sigmaTokens estimates Σ's contribution to the prompt.
//
// It measures the SERIALISED form because that is what statefulUserMessage puts
// in front of the model — a map's Go footprint is not what the provider bills.
func sigmaTokens(sigma map[string]any) int {
	b, err := json.Marshal(sigma)
	if err != nil {
		return 0
	}
	return len(b) / 4
}

// bankEvictedSigma hands the Σ entries about to be dropped to the harvest path,
// so a later recall can fetch back what structural compaction discarded.
//
// ⚠️ CALLED BEFORE THE DELETE, and that ordering is the whole point. Σ is the
// run's working memory; dropping it to save prompt space and NOT banking it
// trades a context problem for a data-loss one. The recap path learned this the
// expensive way — its harvest sat above the measurement, so declined
// distillations banked spans they then kept.
//
// Best-effort, like every other harvest: a run must not fail because a queue
// write did.
func bankEvictedSigma(ctx context.Context, opts RunOptions, emit func(providers.Event),
	sigma map[string]any, keys []string) {
	if len(keys) == 0 {
		return
	}
	dropped := make(map[string]any, len(keys))
	for _, k := range keys {
		if v, ok := sigma[k]; ok {
			dropped[k] = v
		}
	}
	b, err := json.Marshal(dropped)
	if err != nil {
		return
	}
	// Rendered as a message so it rides the same harvest path as every other
	// evicted span — one banking mechanism, not a second one that could drift.
	span := []providers.Message{{
		Role:    "assistant",
		Content: []providers.ContentBlock{{Type: "text", Text: "Evicted state: " + string(b)}},
	}}
	opts.RecallIndex.Harvest(ctx, span)
	harvestToMemory(ctx, opts, emit, span)
}
