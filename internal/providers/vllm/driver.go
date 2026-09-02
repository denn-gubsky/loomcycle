// Package vllm implements the Provider interface for a vLLM server
// (https://docs.vllm.ai) exposing its OpenAI-compatible Chat Completions
// API — typically `http://localhost:8000/v1`. Like the deepseek driver,
// it is a thin wrapper around the openai driver: same wire shape, same
// SSE framing, same tool-call semantics, with the base URL pre-baked and
// ID() returning "vllm" so the resolver and per-provider accounting see
// it as a distinct provider.
//
// Why a dedicated driver rather than `provider: openai` with a base_url
// (RFC CK decision):
//
//   - Explicit config intent. `driver: vllm` documents a self-hosted
//     vLLM in agent/provider config; reusing `openai` forces a reader to
//     infer it from the base URL and muddies logs + cost rollups.
//   - Per-provider cost accounting. runs.model rollups key on (provider,
//     model); a local vLLM must not be conflated with hosted OpenAI.
//   - A home for vLLM quirks (extra_body sampling extensions, served-model
//     capability quirks) without contaminating the openai driver.
//
// Context length nuance: vLLM fixes the context window at server launch
// (`--max-model-len`), so — unlike ollama-local — there is no per-request
// num_ctx knob. Advertise the served window with
// `providers.<id>.capabilities.max_context_tokens` so the loop's
// compaction budget + the context gauge + Context op=self are accurate.
package vllm

import (
	"context"
	"net/http"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/openai"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

// defaultBaseURL is vLLM's default OpenAI-compatible endpoint. The openai
// driver appends "/chat/completions", so the "/v1" path is correct here.
const defaultBaseURL = "http://localhost:8000/v1"

// defaultKeyEnvName is the env-var NAME a secured vLLM's `--api-key` is read
// from (RFC AR tenant/user override); a keyless vLLM leaves it unset and no
// Authorization header is sent.
const defaultKeyEnvName = "VLLM_API_KEY"

// Driver wraps the openai driver with a vLLM base URL and a distinct ID.
// All wire behaviour (auth header, SSE parsing, retry, tool-call shape)
// comes from the embedded driver.
type Driver struct {
	inner *openai.Driver
	// id is the provider identity reported by ID(). Defaults to "vllm" in
	// New(); the driver registry sets it from DriverOptions.ID.
	id string
	// capsPatch is an optional operator override applied inside Capabilities().
	// Nil = advertise the openai driver's defaults.
	capsPatch *providers.CapabilityPatch
}

// New constructs a Driver. baseURL may be empty for the default local vLLM
// endpoint. httpClient may be nil to use the openai driver's default;
// streamOpts is forwarded — see openai.New for semantics.
func New(apiKey, baseURL string, streamOpts streamhttp.Options, httpClient *http.Client) *Driver {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	inner := openai.New(apiKey, baseURL, streamOpts, httpClient)
	// A tenant/user overrides its own vLLM key by VLLM_API_KEY, not
	// OPENAI_API_KEY, which the wrapped openai driver would otherwise resolve.
	inner.SetKeyEnvName(defaultKeyEnvName)
	return &Driver{inner: inner, id: "vllm"}
}

// ID returns "vllm" (by default) so the resolver dispatches `provider: vllm`
// config here rather than the openai driver. The registry may override it.
func (d *Driver) ID() string { return d.id }

// KeyEnvName reports the env-var name whose tenant/user credential can key
// this provider (RFC AX Layer-1 routing); delegates to the inner driver.
func (d *Driver) KeyEnvName() string { return d.inner.KeyEnvName() }

// SetKeyEnvName re-points the env-var name whose credential shadows the host
// key (RFC AR), delegating to the inner openai driver.
func (d *Driver) SetKeyEnvName(name string) { d.inner.SetKeyEnvName(name) }

// Capabilities reports the provider's advertised surface. vLLM serves a wide
// range of models, so the driver inherits the openai defaults and defers the
// per-deployment truth (context window, vision, thinking) to the operator's
// `capabilities:` override — applied last via capsPatch.
func (d *Driver) Capabilities() providers.Capabilities {
	base := d.inner.Capabilities()
	base.Local = true // RFC CR tier-routing: vLLM is a self-hosted backend
	return d.capsPatch.Apply(base)
}

// Call delegates to the openai driver. Setting a provider override on ctx
// makes the inner driver's per-attempt span carry loomcycle.provider="vllm"
// (matching the deepseek driver's OTEL handling — a wrapping span here would
// mismeasure streaming latency).
func (d *Driver) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	return d.inner.Call(lcotel.WithProviderOverride(ctx, d.ID()), req)
}

// Probe delegates to the openai driver's GET /v1/models against the base URL;
// vLLM returns the OpenAI-compatible {"data":[{"id":...}]} shape.
func (d *Driver) Probe(ctx context.Context) error { return d.inner.Probe(ctx) }

// ListModels delegates to the openai driver — see Probe for the wire rationale.
func (d *Driver) ListModels(ctx context.Context) ([]string, error) { return d.inner.ListModels(ctx) }
