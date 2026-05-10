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
	return &Driver{inner: openai.New(apiKey, baseURL, streamOpts, httpClient)}
}

// ID returns "deepseek" so the provider resolver in cmd/loomcycle
// dispatches per-agent `provider: deepseek` config to this driver
// rather than the OpenAI one. Distinct from the inner driver's ID()
// — that's what makes the wrapper worth its weight.
func (d *Driver) ID() string { return "deepseek" }

// Capabilities mirrors OpenAI's today. DeepSeek's V3 chat / coder
// models behave identically to OpenAI Chat Completions for tool use
// and streaming. Diverges later if/when:
//
//   - reasoner model support lands (SupportsThinking → true, plus
//     reasoning_content event handling)
//   - DeepSeek's prompt-cache plumbing graduates from
//     auto-on-server to a caller-controlled knob like Anthropic's
//     cache_control (NativePromptCache → true)
//
// MaxContextTokens left at OpenAI's 128K default; DeepSeek-V3 is
// 128K-context too. The reasoner model is 64K — when we add it,
// callers should pass MaxTokens explicitly rather than rely on this
// default.
func (d *Driver) Capabilities() providers.Capabilities {
	return d.inner.Capabilities()
}

// Call delegates to the OpenAI driver. The request body, retry
// strategy, and SSE framing are identical between the two services.
func (d *Driver) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	return d.inner.Call(ctx, req)
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
