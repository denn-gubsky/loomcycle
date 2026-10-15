// Package llamacpp implements the Provider interface for a llama.cpp
// server (llama-server, https://github.com/ggml-org/llama.cpp) exposing its
// OpenAI-compatible Chat Completions API — typically `http://localhost:8080/v1`.
// Like the deepseek/vllm drivers it is a thin wrapper around the openai
// driver: same wire shape, same SSE framing, same tool-call semantics, with
// the base URL pre-baked and ID() returning "llamacpp" so the resolver and
// per-provider accounting see it as a distinct provider.
//
// Why a dedicated driver rather than `provider: openai` with a base_url
// (RFC CK decision):
//
//   - Explicit config intent. `driver: llamacpp` documents a self-hosted
//     llama-server; reusing `openai` forces a reader to infer it from the
//     base URL and muddies logs + cost rollups.
//   - Per-provider cost accounting keyed on (provider, model).
//   - A home for llama.cpp quirks without contaminating the openai driver.
//
// Slot pinning (RFC EI P4c, opt-in with `options.pin_slots: true`): llama-server
// keeps one prompt cache per slot (`--parallel N`). With pinning on, each call
// is sent with `id_slot` = the number of the call-lane slot it was granted and
// `cache_prompt: true`. A run's reservation (`lanes.chat.affinity_hold_ms`)
// keeps it on the same lane slot between calls, so its next call returns to
// the llama-server slot that still holds its prompt. The lane numbers slots
// from 0 below its `max_concurrent`, so set that to llama-server's `--parallel`:
// a number past the server's slots is refused by the server. A call no lane
// granted (an uncapped provider) is sent unpinned.
//
// Context length nuance: llama-server fixes the context window at launch
// (`-c` / `--ctx-size`), so there is no per-request num_ctx knob. Advertise
// the served window with `providers.<id>.capabilities.max_context_tokens` so
// the loop's compaction budget + context gauge + Context op=self are accurate.
package llamacpp

import (
	"context"
	"net/http"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/openai"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

// defaultBaseURL is llama-server's default OpenAI-compatible endpoint. The
// openai driver appends "/chat/completions", so "/v1" is correct here.
const defaultBaseURL = "http://localhost:8080/v1"

// defaultKeyEnvName is the env-var NAME a secured llama-server's `--api-key`
// is read from (RFC AR); a keyless server leaves it unset (no Authorization
// header sent).
const defaultKeyEnvName = "LLAMACPP_API_KEY"

// Driver wraps the openai driver with a llama.cpp base URL and a distinct ID.
type Driver struct {
	inner     *openai.Driver
	id        string
	capsPatch *providers.CapabilityPatch
}

// New constructs a Driver. baseURL may be empty for the default local
// llama-server endpoint. httpClient may be nil; streamOpts is forwarded to
// the inner driver — see openai.New for semantics.
func New(apiKey, baseURL string, streamOpts streamhttp.Options, httpClient *http.Client) *Driver {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	inner := openai.New(apiKey, baseURL, streamOpts, httpClient)
	inner.SetKeyEnvName(defaultKeyEnvName)
	return &Driver{inner: inner, id: "llamacpp"}
}

// ID returns "llamacpp" (by default) so the resolver dispatches
// `provider: llamacpp` config here rather than the openai driver.
func (d *Driver) ID() string { return d.id }

// KeyEnvName reports the env-var name whose credential can key this provider
// (RFC AX Layer-1 routing); delegates to the inner driver.
func (d *Driver) KeyEnvName() string { return d.inner.KeyEnvName() }

// PinSlots turns slot pinning on (see the package doc).
func (d *Driver) PinSlots() { d.inner.SetBodyExtra(slotFields) }

// slotFields is the pinning a call's lane slot asks for, nil for a call no
// lane granted.
func slotFields(ctx context.Context) map[string]any {
	slot, ok := providers.LaneSlot(ctx)
	if !ok {
		return nil
	}
	return map[string]any{"id_slot": slot, "cache_prompt": true}
}

// SetKeyEnvName re-points the credential env-var name (RFC AR), delegating to
// the inner openai driver.
func (d *Driver) SetKeyEnvName(name string) { d.inner.SetKeyEnvName(name) }

// Capabilities inherits the openai defaults and defers per-deployment truth
// (context window, vision) to the operator's `capabilities:` override, applied
// last via capsPatch. A single-model llama-server usually needs at least
// max_context_tokens set to its `-c` value.
func (d *Driver) Capabilities() providers.Capabilities {
	base := d.inner.Capabilities()
	// A grammar over the tokens, not a schema the model is shown: the loop
	// puts the schema in the prompt (RFC DI).
	base.StructuredOutputNative = false
	base.Local = true // RFC CR tier-routing: llama.cpp is a self-hosted backend
	return d.capsPatch.Apply(base)
}

// EnforcesStructuredOutput implements providers.ModelStructuredOutputEnforcer.
// llama.cpp applies response_format as a grammar over EVERY sampled token, so with
// tools in the request the model could no longer emit a tool call; the format
// is enforced only on a tool-free request.
func (d *Driver) EnforcesStructuredOutput(_ string, hasTools bool) bool {
	return d.Capabilities().SupportsStructuredOutput && !hasTools
}

// Call delegates to the openai driver; the ctx provider override makes the
// inner per-attempt span carry loomcycle.provider="llamacpp".
func (d *Driver) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	return d.inner.Call(lcotel.WithProviderOverride(ctx, d.ID()), req)
}

// Probe delegates to GET /v1/models; llama-server returns the OpenAI-compatible
// {"data":[{"id":...}]} shape.
func (d *Driver) Probe(ctx context.Context) error { return d.inner.Probe(ctx) }

// ListModels delegates to the openai driver — see Probe for the wire rationale.
func (d *Driver) ListModels(ctx context.Context) ([]string, error) { return d.inner.ListModels(ctx) }
