// Package contextplugin implements the RFC Z context-transform plugin chain:
// fast, built-in transforms applied to a COPY of the outbound LLM request on
// every turn, between the assembled agent context and the provider call. The
// first (and only built-in today) is `redact` — outbound secret redaction.
//
// The chain is built once from runtime config (config.ContextPluginSpec) and
// shared read-only across runs. Contract every plugin must honour:
//
//   - Deterministic + stable (idempotent on its own output) — so the cached
//     prompt prefix stays byte-stable turn over turn and any future replay holds.
//   - Never mutate its input — return new slices (copy-on-write), leaving the
//     loop's canonical messages, the persisted transcript, and the code-js
//     replay input all byte-stable.
//   - Preserve ContentBlock.Cacheable markers on blocks it passes through.
//
// The loop applies the chain only for real LLM providers; the synthetic code-js
// provider is exempt (local replay, no external leak, and redacted bytes would
// trip replay divergence).
package contextplugin

import (
	"context"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// Plugin transforms the outbound request. See the package doc for the contract.
type Plugin interface {
	Name() string
	Transform(ctx context.Context, system []providers.ContentBlock, msgs []providers.Message) (
		[]providers.ContentBlock, []providers.Message, error)
}

// builder constructs a plugin from its spec + the run-secret value set (used by
// the redact plugin's Tier-A exact masking; ignored by plugins that don't need it).
type builder func(spec config.ContextPluginSpec, secrets map[string]string) (Plugin, error)

// registry maps a built-in plugin name to its constructor. The canonical list
// of names; config.knownContextPluginNames mirrors it for load-time validation.
var registry = map[string]builder{
	"redact": newRedactPlugin,
}

// Build resolves the runtime-wide plugin chain from config, in declared order.
// `secrets` is the operator secret-value set (env-derived) the redact plugin
// masks exactly; the Tier-B heuristic patterns apply regardless. A disabled
// spec is skipped; an unknown name is an error (config validation rejects these
// at load — Build is the defensive runtime authority). Returns nil for an empty
// or all-disabled chain so the loop can skip cheaply.
func Build(specs []config.ContextPluginSpec, secrets map[string]string) ([]Plugin, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	out := make([]Plugin, 0, len(specs))
	for i, spec := range specs {
		if !spec.IsEnabled() {
			continue
		}
		b, ok := registry[spec.Name]
		if !ok {
			return nil, fmt.Errorf("context_plugins[%d]: unknown plugin %q", i, spec.Name)
		}
		p, err := b(spec, secrets)
		if err != nil {
			return nil, fmt.Errorf("context_plugins[%d] (%s): %w", i, spec.Name, err)
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// Apply runs the chain in order, threading each plugin's output into the next.
// Plugins are copy-on-write, so the caller's `system`/`msgs` are not mutated;
// the returned pair is what feeds the outbound providers.Request.
func Apply(ctx context.Context, chain []Plugin, system []providers.ContentBlock, msgs []providers.Message) (
	[]providers.ContentBlock, []providers.Message, error) {
	var err error
	for _, p := range chain {
		if system, msgs, err = p.Transform(ctx, system, msgs); err != nil {
			return nil, nil, fmt.Errorf("context plugin %q: %w", p.Name(), err)
		}
	}
	return system, msgs, nil
}
