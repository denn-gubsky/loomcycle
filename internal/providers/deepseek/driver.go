// Package deepseek implements the Provider interface for DeepSeek's
// service. DeepSeek exposes an OpenAI-compatible Chat Completions API
// at https://api.deepseek.com (with the `/v1` path appended), so the
// driver is a thin wrapper around the existing OpenAI driver — same
// wire shape, same SSE framing, same tool-call semantics — with the
// base URL pre-baked and ID() returning "deepseek" so the resolver
// (and per-run accounting) sees it as a distinct provider.
//
// Why a separate package rather than reusing `provider: openai` with
// a custom base URL:
//
//   - Explicit yaml config. `provider: deepseek` documents the
//     intent in agent definitions; reusing `openai` would require
//     readers to know the base-URL override means "this is actually
//     DeepSeek" and would confuse logs.
//   - Per-provider cost accounting. runs.model rollups should not
//     conflate OpenAI and DeepSeek pricing — they're orders of
//     magnitude apart, and a downstream price-table lookup is
//     keyed on (provider, model).
//   - A place to absorb DeepSeek-specific quirks without
//     contaminating the OpenAI driver. Today the wire is identical;
//     when DeepSeek's reasoning model (deepseek-reasoner) gains
//     proper support, the `reasoning_content` field handling lands
//     here, not in openai/.
package deepseek

import (
	"context"
	"net/http"
	"strings"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/openai"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

// defaultBaseURL is DeepSeek's OpenAI-compatible Chat Completions
// endpoint. The OpenAI driver appends "/chat/completions" so the
// "/v1" path is correct here.
const defaultBaseURL = "https://api.deepseek.com/v1"

// Driver wraps the OpenAI driver with a DeepSeek base URL and a
// distinct ID. All other behaviour (auth header, SSE parsing, retry,
// tool-call shape) comes from the embedded driver.
type Driver struct {
	inner *openai.Driver
	// id is the provider identity reported by ID(). Defaults to "deepseek" in
	// New(); the RFC BF driver registry sets it from DriverOptions.ID.
	id string
	// capsPatch is an optional operator override applied inside Capabilities()
	// (RFC BF), AFTER the DeepSeek-specific patches. Nil = advertise defaults.
	capsPatch *providers.CapabilityPatch
}

// New constructs a Driver. baseURL may be empty for the public
// DeepSeek endpoint, or set to a self-hosted OpenAI-compatible mirror
// (e.g. an internal vLLM serving a DeepSeek model). httpClient may be
// nil to use the OpenAI driver's default. streamOpts is forwarded to
// the inner driver — see openai.New for semantics.
func New(apiKey, baseURL string, streamOpts streamhttp.Options, httpClient *http.Client) *Driver {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	inner := openai.New(apiKey, baseURL, streamOpts, httpClient)
	// RFC AR: a tenant/user overrides its own DeepSeek key by the env-var name
	// DEEPSEEK_API_KEY, not OPENAI_API_KEY — the wrapped driver would otherwise
	// resolve the OpenAI name.
	inner.SetKeyEnvName("DEEPSEEK_API_KEY")
	return &Driver{inner: inner, id: "deepseek"}
}

// ID returns "deepseek" (by default) so the provider resolver in cmd/loomcycle
// dispatches per-agent `provider: deepseek` config to this driver rather than
// the OpenAI one. Distinct from the inner driver's ID() — that's what makes the
// wrapper worth its weight. The RFC BF registry may override it via the factory.
func (d *Driver) ID() string { return d.id }

// KeyEnvName reports the env-var name whose tenant/user credential can key this
// provider (RFC AX Layer-1 routing). Delegates to the inner OpenAI driver, whose
// SetKeyEnvName was pointed at "DEEPSEEK_API_KEY" in New — the same literal the
// inner driver's resolveKey resolves.
func (d *Driver) KeyEnvName() string { return d.inner.KeyEnvName() }

// SetKeyEnvName overrides the env-var name whose tenant/user credential shadows
// the host key (RFC AR), delegating to the inner OpenAI driver. New() already
// points it at DEEPSEEK_API_KEY; the RFC BF registry factory forwards a
// config-declared api_key_env through here (e.g. a self-hosted OpenAI-compatible
// DeepSeek mirror naming its own key var).
func (d *Driver) SetKeyEnvName(name string) { d.inner.SetKeyEnvName(name) }

// Capabilities reports the provider's MAXIMUM surface — what at
// least one model on DeepSeek can do — not the per-model details.
// The interface contract is per-provider, not per-model (see
// providers.Provider's Capabilities() signature), so the driver
// surfaces the union and decides per-call (via IsThinkingModel)
// whether to actually attach thinking-related wire params.
//
// Key divergence from OpenAI's defaults: SupportsThinking=true.
// DeepSeek's reasoner / v4-pro variants are thinking-class models;
// the OpenAI driver returns false because OpenAI's chat-class
// models don't think. Operators picking deepseek-v4-pro should
// budget max_tokens accordingly — the 2026-05-15 bench discovered
// that judge calls with max_tokens=512 returned empty content
// because the model consumed the budget on hidden reasoning.
func (d *Driver) Capabilities() providers.Capabilities {
	caps := d.inner.Capabilities()
	caps.SupportsThinking = true
	// DeepSeek's text models don't accept image input. The inner OpenAI driver
	// reports SupportsVision=true (RFC AT), so override to false here — the
	// loop then gates an image to DeepSeek upstream with a clear error rather
	// than the inner OpenAI wire builder producing an image_url DeepSeek 400s on.
	caps.SupportsVision = false
	// RFC BF operator override applies last, so an operator can, e.g., re-enable
	// vision for a self-hosted multimodal DeepSeek mirror behind base_url.
	return d.capsPatch.Apply(caps)
}

// IsThinkingModel reports whether the named DeepSeek model is a
// thinking-class variant (extended reasoning enabled by default).
// Used by future per-call decisions where the driver needs to
// distinguish v4-pro/reasoner from v4-flash/chat — the interface
// doesn't take a model parameter on Capabilities() so this helper
// is the per-model affordance.
//
// Naming convention as of 2026-05-15:
//   - thinking-class: deepseek-v4-pro, deepseek-reasoner,
//     deepseek-r1, deepseek-v3-pro (and future *-pro / *-reasoner)
//   - non-thinking: deepseek-chat, deepseek-v4-flash,
//     deepseek-v3.2, deepseek-coder
//
// Conservative default for unknown model names: false. This
// reflects the assumption that new releases default to chat-class;
// thinking variants tend to be explicit.
func IsThinkingModel(model string) bool {
	lower := strings.ToLower(model)
	thinkingMarkers := []string{
		"-pro",
		"reasoner",
		"-r1",
		// The whole DeepSeek V4 family enforces the thinking-mode
		// reasoning_content round-trip — INCLUDING deepseek-v4-flash. Production
		// (2026-07-02) proved a cross-provider fallback landing on
		// deepseek-v4-flash still 400s ("reasoning_content ... must be passed
		// back") on a reasoning-less history, even with the effort hint dropped
		// (#608). So "flash" is NOT a safe non-thinking sibling — classify the
		// v4 family as thinking so the downgrader routes it to deepseek-chat.
		"v4",
	}
	for _, m := range thinkingMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// NonThinkingSibling implements providers.ThinkingDowngrader. A DeepSeek
// thinking model (deepseek-reasoner / *-pro / *-r1 / the v4 family) requires
// reasoning_content echoed on every assistant turn and 400s on a turn lacking
// it. When a cross-provider fallback lands on such a model with a reasoning-less
// history, the loop swaps it for the non-thinking model returned here.
//
// Target: ALWAYS deepseek-chat (the V3 non-thinking model). The same-generation
// *-flash is NOT a safe target — production (2026-07-02) proved deepseek-v4-flash
// ALSO runs in thinking mode and 400s ("reasoning_content ... must be passed
// back") on the reasoning-less, cross-provider-stripped history, even after the
// effort hint is dropped (#608). deepseek-chat is the canonical, always-available
// model that never requires reasoning_content. Returns ("", false) for a
// non-thinking model (incl. deepseek-chat itself → no infinite downgrade).
func (d *Driver) NonThinkingSibling(model string) (string, bool) {
	if !IsThinkingModel(model) {
		return "", false
	}
	return "deepseek-chat", true
}

// Call delegates to the OpenAI driver. The request body, retry
// strategy, and SSE framing are identical between the two services.
//
// v0.10.0 OTEL: a wrapping span here would mismeasure latency
// because `defer span.End()` fires when the channel is returned —
// well before the streaming HTTP response is consumed. Instead, set
// a provider-override on the ctx so the inner OpenAI driver's
// per-attempt span carries `loomcycle.provider="deepseek"`. Jaeger
// operators filtering by `provider="deepseek"` see DeepSeek calls
// distinctly with correct streaming-attempt durations.
func (d *Driver) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	return d.inner.Call(lcotel.WithProviderOverride(ctx, "deepseek"), req)
}

// Probe delegates to the OpenAI driver, which hits GET /v1/models
// against whatever base URL was configured. DeepSeek's /v1/models
// response uses the OpenAI-compatible shape ({"data": [{"id": ...}]}),
// so the inner driver's parser works unchanged. Listed wire aliases
// observed in production: deepseek-chat (V3 chat), deepseek-reasoner
// (R1), deepseek-v4-flash, deepseek-v4-pro.
func (d *Driver) Probe(ctx context.Context) error {
	return d.inner.Probe(ctx)
}

// ListModels delegates to the OpenAI driver. See Probe's docstring
// for the wire-shape rationale.
func (d *Driver) ListModels(ctx context.Context) ([]string, error) {
	return d.inner.ListModels(ctx)
}
