// Package gemini implements the Provider interface for Google's
// Gemini API (generative language API, /v1beta/models surface).
//
// Gemini's wire shape differs from Anthropic and OpenAI in three
// material ways:
//
//   - Streaming endpoint takes the model name in the URL path
//     (`/v1beta/models/{model}:streamGenerateContent`) rather than
//     in the body. The request body carries no model field.
//   - Auth is via the `x-goog-api-key` header (also accepts the
//     legacy `?key=` query param; we use the header to keep keys
//     out of access logs).
//   - Streaming uses Server-Sent Events with `?alt=sse`. Without
//     that query param, the endpoint returns a JSON array of full
//     responses — not what we want for incremental rendering.
//
// Tool dispatch maps loomcycle's ToolUse / ToolResult onto Gemini's
// `functionCall` / `functionResponse` content parts. Gemini does NOT
// issue tool_call IDs (similar to Ollama), so the loop's
// synthesized-ID path handles correlation.
//
// Reasoning models (gemini-2.5-flash, gemini-2.5-pro) emit thinking
// in a `thoughtSignature` field and surface a thinking-tokens count
// in usageMetadata. This driver doesn't yet plumb that through to
// EventThinking — Gemini's thinking surface is opaque (signature is
// a base64 blob, not text). When Gemini opens up a text trace
// surface, this is the wiring point. Tracked alongside the rest of
// the EventThinking work in v0.7.1.
package gemini

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/ratelimit"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

const (
	defaultBaseURL = "https://generativelanguage.googleapis.com/v1beta"
)

// Driver speaks Gemini's generateContent API.
type Driver struct {
	apiKey      string
	baseURL     string
	http        *http.Client
	idleTimeout time.Duration
	// id is the provider identity reported by ID(). Defaults to "gemini" in
	// New(); the RFC BF driver registry sets it from DriverOptions.ID.
	id string
	// keyEnvName is the env-var NAME whose tenant/user credential overrides the
	// host key (RFC AR/AX). Defaults to "GEMINI_API_KEY" in New(); a
	// config-declared api_key_env re-points it via SetKeyEnvName so a custom-id
	// gemini provider resolves tenant overrides under its OWN var, matching the
	// var toDriverOptions already sourced the host key from.
	keyEnvName string
	// capsPatch is an optional operator override applied inside Capabilities()
	// (RFC BF). Nil = advertise the driver defaults.
	capsPatch *providers.CapabilityPatch
}

// New constructs a Driver. baseURL may be empty for the default
// endpoint, or set to a self-hosted Vertex AI Gemini gateway. The
// trailing path "/v1beta" is included in the default — operator
// overrides should match that pattern. streamOpts controls per-stream
// timeouts (zero = streamhttp defaults). When httpClient is nil, a
// fresh streaming client honoring streamOpts.HeaderTimeout is built.
func New(apiKey, baseURL string, streamOpts streamhttp.Options, httpClient *http.Client) *Driver {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	streamOpts = streamOpts.Resolve()
	if httpClient == nil {
		httpClient = streamhttp.NewClient(streamOpts.HeaderTimeout)
	}
	return &Driver{apiKey: apiKey, baseURL: baseURL, http: httpClient, idleTimeout: streamOpts.IdleTimeout, id: "gemini", keyEnvName: "GEMINI_API_KEY"}
}

func (d *Driver) ID() string { return d.id }

// SetKeyEnvName overrides the env-var name whose tenant/user credential shadows
// the host key (RFC AR). New() defaults it to GEMINI_API_KEY; the RFC BF registry
// factory forwards a config-declared api_key_env through here so a custom-id
// gemini provider's tenant overrides resolve under the SAME var the host key was
// read from.
func (d *Driver) SetKeyEnvName(name string) { d.keyEnvName = name }

// resolveKey returns the API key to authenticate an inference request AND which
// credential scope it came from. A tenant/user credential named GEMINI_API_KEY
// overrides the operator's host key (RFC AR); source is "operator" when none
// did. The source/scopeID ride the per-call Usage so the server can attribute
// spend (RFC AV). RFC AX: a RESTRICTED run with no override gets
// ErrOperatorKeyForbidden instead of the host key — Call aborts on it.
// Model-availability probes (fetchModels) stay on the host key — which models
// are reachable is an operator concern.
func (d *Driver) resolveKey(ctx context.Context) (key, source, scopeID string, err error) {
	return providers.ResolveKeyOrOperator(ctx, d.keyEnvName, d.apiKey)
}

// KeyEnvName reports the env-var name whose tenant/user credential can key this
// provider (RFC AX Layer-1 routing). Same var resolveKey resolves.
func (d *Driver) KeyEnvName() string { return d.keyEnvName }

func (d *Driver) Capabilities() providers.Capabilities {
	return d.capsPatch.Apply(providers.Capabilities{
		// Gemini has implicit prompt caching on long contexts but no
		// operator-controlled cache_control like Anthropic. Mark
		// false until we add the explicit-cache surface.
		NativePromptCache: false,
		ParallelToolCalls: true,
		Streaming:         true,
		// gemini-2.5-pro tops out at 2M; gemini-2.5-flash at 1M.
		// (gemini-2.0-flash was retired by Google 2026-05 — no
		// longer available to new users; replaced by 2.5-flash.)
		// Report the optimistic case.
		MaxContextTokens: 2_000_000,
		// gemini-2.5-flash / 2.5-pro support thinking via
		// generationConfig.thinkingConfig. Capability is true; the
		// driver attaches thinkingBudget when Effort is set on the
		// request (see Call).
		SupportsThinking: true,
		SupportsEffort:   true,
		// All current Gemini models (2.5 flash / pro) are multimodal.
		// No per-model gate — operator-trust per RFC AT §5.3; a non-vision
		// model surfaces via the existing provider-fallback error.
		SupportsVision: true,
	})
}

// Call sends the request and returns a channel of Events.
//
// 429 retry: ratelimit.Do honours Gemini's `retry-after` header
// when present; otherwise it falls back to exponential backoff.
func (d *Driver) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	body, err := buildRequestBody(req)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	streamCtx, cancelStream := context.WithCancel(ctx)

	// RFC AR/AV: resolve the key + its owning scope ONCE per call (not per retry
	// attempt) so the header uses it and the source rides the per-call Usage.
	// RFC AX: a restricted run with no override refuses here (never the host key).
	apiKey, credSource, credScopeID, err := d.resolveKey(ctx)
	if err != nil {
		cancelStream()
		return nil, err
	}

	url := d.baseURL + "/models/" + req.Model + ":streamGenerateContent?alt=sse"
	attempt := func(attemptCtx context.Context) (*http.Response, error) {
		// v0.10.0 OTEL: one loomcycle.provider.call span per attempt.
		spanCtx, span := lcotel.RecordProviderCall(attemptCtx, lcotel.ProviderCallAttrs{
			Provider: "gemini",
			Model:    req.Model,
			Effort:   req.Effort,
		})
		defer span.End()
		httpReq, err := http.NewRequestWithContext(spanCtx, "POST", url, bytes.NewReader(body))
		if err != nil {
			lcotel.SetSpanError(span, err)
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")
		if apiKey != "" {
			httpReq.Header.Set("x-goog-api-key", apiKey)
		}
		resp, err := d.http.Do(httpReq)
		if err != nil {
			lcotel.SetSpanError(span, err)
		} else if resp != nil && resp.StatusCode >= 400 {
			lcotel.SetSpanErrorMessage(span, "http "+resp.Status)
		}
		return resp, err
	}

	resp, err := ratelimit.Do(streamCtx, ratelimit.Config{
		Provider:    "gemini",
		ParseHeader: ratelimit.OpenAIRetryAfter, // Gemini uses the same header shape
		OnEvent:     req.OnEvent,
	}, attempt)
	if err != nil {
		cancelStream()
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("http: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		cancelStream()
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("gemini %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}

	resp.Body = streamhttp.WrapBody(resp.Body, d.idleTimeout, cancelStream)

	out := make(chan providers.Event, 16)
	go func() {
		defer cancelStream()
		streamEvents(streamCtx, resp.Body, out, req.Model, credSource, credScopeID)
	}()
	return out, nil
}

// --- request marshalling ---

type wireRequest struct {
	Contents          []wireContent  `json:"contents"`
	Tools             []wireTool     `json:"tools,omitempty"`
	SystemInstruction *wireContent   `json:"systemInstruction,omitempty"`
	GenerationConfig  *wireGenConfig `json:"generationConfig,omitempty"`
}

type wireContent struct {
	Role  string     `json:"role,omitempty"`
	Parts []wirePart `json:"parts"`
}

// wirePart is the union type Gemini uses for message body. Only one
// of Text / FunctionCall / FunctionResponse is set per part.
type wirePart struct {
	Text             string                `json:"text,omitempty"`
	FunctionCall     *wireFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *wireFunctionResponse `json:"functionResponse,omitempty"`
	InlineData       *wireInlineData       `json:"inlineData,omitempty"` // type == "image" (RFC AT)
}

// wireInlineData is Gemini's inline binary part: {"mimeType","data"} with the
// data being raw base64 (no "data:" prefix). camelCase keys match the rest of
// this driver (functionCall/functionResponse); the Gemini API also accepts the
// snake_case form.
type wireInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type wireFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type wireFunctionResponse struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

type wireTool struct {
	FunctionDeclarations []wireFunctionDecl `json:"functionDeclarations"`
}

type wireFunctionDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type wireGenConfig struct {
	MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
	// Gemini generationConfig sampling knobs. frequency/presence_penalty are
	// not Gemini params (dropped). seed is supported on recent models.
	TopP           *float64            `json:"topP,omitempty"`
	TopK           *int                `json:"topK,omitempty"`
	Seed           *int                `json:"seed,omitempty"`
	StopSequences  []string            `json:"stopSequences,omitempty"`
	ThinkingConfig *wireThinkingConfig `json:"thinkingConfig,omitempty"`
}

type wireThinkingConfig struct {
	// ThinkingBudget caps tokens spent on internal reasoning. -1 =
	// dynamic (model decides). 0 disables thinking on supported
	// models. Positive values are explicit token budgets.
	ThinkingBudget int `json:"thinkingBudget"`
}

// buildRequestBody marshals a providers.Request into Gemini's wire
// shape. The model field is intentionally NOT in the body — Gemini
// reads it from the URL path.
func buildRequestBody(req providers.Request) ([]byte, error) {
	w := wireRequest{
		Contents: make([]wireContent, 0, len(req.Messages)),
	}

	// System segments → systemInstruction (Gemini's dedicated slot).
	// Loomcycle's loop concatenates system content into req.System;
	// we flatten to one wireContent with text parts.
	if len(req.System) > 0 {
		var sysParts []wirePart
		for _, c := range req.System {
			if c.Type == "text" && c.Text != "" {
				sysParts = append(sysParts, wirePart{Text: c.Text})
			}
		}
		if len(sysParts) > 0 {
			w.SystemInstruction = &wireContent{Parts: sysParts}
		}
	}

	for _, msg := range req.Messages {
		role := msg.Role
		// Gemini uses "user" and "model" instead of "user" and
		// "assistant". Translate.
		if role == "assistant" {
			role = "model"
		}
		parts, err := flattenMessageContent(msg)
		if err != nil {
			return nil, err
		}
		if len(parts) == 0 {
			continue
		}
		w.Contents = append(w.Contents, wireContent{Role: role, Parts: parts})
	}

	if len(req.Tools) > 0 {
		decls := make([]wireFunctionDecl, 0, len(req.Tools))
		for _, t := range req.Tools {
			decls = append(decls, wireFunctionDecl{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  sanitizeGeminiSchema(t.InputSchema),
			})
		}
		w.Tools = []wireTool{{FunctionDeclarations: decls}}
	}

	if req.MaxTokens > 0 || req.Temperature != nil || req.Effort != "" ||
		req.TopP != nil || req.TopK != nil || req.Seed != nil || len(req.Stop) > 0 {
		gc := &wireGenConfig{
			MaxOutputTokens: req.MaxTokens,
			Temperature:     req.Temperature,
			TopP:            req.TopP,
			TopK:            req.TopK,
			Seed:            req.Seed,
			StopSequences:   req.Stop,
		}
		if budget := geminiEffortBudget(req.Effort, req.MaxTokens); budget >= 0 {
			gc.ThinkingConfig = &wireThinkingConfig{ThinkingBudget: budget}
		}
		w.GenerationConfig = gc
	}

	return json.Marshal(w)
}

// flattenMessageContent translates one loomcycle Message's content
// blocks into Gemini wire parts. Mixed text + tool_use / tool_result
// blocks are common on the assistant turn after a tool call;
// ordering is preserved so the wire roundtrips faithfully.
func flattenMessageContent(msg providers.Message) ([]wirePart, error) {
	var out []wirePart
	for _, c := range msg.Content {
		switch c.Type {
		case "text":
			if c.Text != "" {
				out = append(out, wirePart{Text: c.Text})
			}
		case "image":
			out = append(out, wirePart{InlineData: &wireInlineData{MimeType: c.MediaType, Data: c.Data}})
		case "tool_use":
			out = append(out, wirePart{FunctionCall: &wireFunctionCall{
				Name: c.ToolName,
				Args: c.ToolInput,
			}})
		case "tool_result":
			// Gemini's functionResponse expects a wrapped object;
			// loomcycle's tool_result text becomes {"content": "..."}
			// so the model sees structured JSON either way.
			respBody, err := json.Marshal(map[string]string{"content": c.Text})
			if err != nil {
				return nil, fmt.Errorf("marshal tool_result: %w", err)
			}
			out = append(out, wirePart{FunctionResponse: &wireFunctionResponse{
				Name:     c.ToolName,
				Response: respBody,
			}})
		}
	}
	return out, nil
}

// geminiEffortBudget translates loomcycle's effort hint into a
// thinkingBudget value. Returns -1 to mean "no thinkingConfig"
// (drivers leave the field off the wire entirely).
//
//	effort == ""     → no thinkingConfig (driver default; thinking
//	                   may still happen on 2.5 models per Gemini's
//	                   own dynamic budget — we just don't assert)
//	effort == "low"  → thinkingBudget=0 (explicitly disable)
//	effort == "medium" → thinkingBudget=2048
//	effort == "high" → thinkingBudget=8192 (or maxTokens-1024 if smaller)
//
// The "low → disable" choice mirrors Anthropic's effort=low → no
// thinking block, so operators get consistent semantics across
// providers.
func geminiEffortBudget(effort string, maxTokens int) int {
	switch effort {
	case "":
		return -1
	case "low":
		return 0
	case "medium":
		budget := 2048
		if maxTokens > 0 && budget >= maxTokens {
			budget = maxTokens - 256
			if budget <= 0 {
				return 0
			}
		}
		return budget
	case "high":
		budget := 8192
		if maxTokens > 0 && budget >= maxTokens {
			budget = maxTokens - 1024
			if budget < 1024 {
				return 0
			}
		}
		return budget
	default:
		return -1
	}
}

// --- streaming response parsing ---

type chunk struct {
	Candidates []struct {
		Content struct {
			Parts []chunkPart `json:"parts"`
			Role  string      `json:"role"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata *chunkUsage `json:"usageMetadata"`
	ModelVersion  string      `json:"modelVersion"`
}

// chunkPart mirrors wirePart on the response side — Gemini uses the
// same union shape for input and output.
type chunkPart struct {
	Text         string `json:"text"`
	FunctionCall *struct {
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	} `json:"functionCall"`
}

type chunkUsage struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
	// CachedContentTokenCount is set when implicit caching kicks in
	// on long contexts. Maps to Usage.CacheReadTokens.
	CachedContentTokenCount int `json:"cachedContentTokenCount"`
	// ThoughtsTokenCount is the cost of internal reasoning on
	// thinking-mode models. Surfaced to operators via Usage so cost
	// retros distinguish thinking from response output.
	ThoughtsTokenCount int `json:"thoughtsTokenCount"`
}

func streamEvents(ctx context.Context, body io.ReadCloser, out chan<- providers.Event, requestModel, credSource, credScopeID string) {
	defer body.Close()
	defer close(out)

	send := func(ev providers.Event) bool {
		select {
		case out <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	var stopReason string
	var usage *providers.Usage
	model := requestModel

	// pendingFunctionCalls accumulates tool_call parts seen in the
	// stream. Gemini emits one functionCall per chunk on a tool turn;
	// we surface them as EventToolCall after the stream ends so the
	// loop sees them in arrival order with synthesised IDs.
	type pendingCall struct {
		name string
		args json.RawMessage
	}
	var pending []pendingCall

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if string(payload) == "[DONE]" {
			break
		}
		var c chunk
		if err := json.Unmarshal(payload, &c); err != nil {
			continue
		}
		if c.ModelVersion != "" {
			model = c.ModelVersion
		}

		for _, cand := range c.Candidates {
			for _, p := range cand.Content.Parts {
				if p.Text != "" {
					if !send(providers.Event{Type: providers.EventText, Text: p.Text}) {
						return
					}
				}
				if p.FunctionCall != nil {
					args := p.FunctionCall.Args
					if len(args) == 0 {
						args = json.RawMessage("{}")
					}
					pending = append(pending, pendingCall{
						name: p.FunctionCall.Name,
						args: args,
					})
				}
			}
			if cand.FinishReason != "" {
				stopReason = mapStopReason(cand.FinishReason)
			}
		}

		if c.UsageMetadata != nil {
			usage = &providers.Usage{
				InputTokens:     c.UsageMetadata.PromptTokenCount,
				OutputTokens:    c.UsageMetadata.CandidatesTokenCount,
				CacheReadTokens: c.UsageMetadata.CachedContentTokenCount,
				Model:           model,
			}
		}
	}
	if err := scanner.Err(); err != nil {
		send(providers.Event{Type: providers.EventError, Error: "stream read: " + err.Error()})
		return
	}

	// Emit accumulated function calls in arrival order. Gemini doesn't
	// issue tool_call IDs; the loop synthesises them.
	if len(pending) > 0 {
		for _, p := range pending {
			if !send(providers.Event{
				Type: providers.EventToolCall,
				ToolUse: &providers.ToolUse{
					Name:  p.name,
					Input: p.args,
				},
			}) {
				return
			}
		}
		// Function calls override "STOP" finishReason — the loop must
		// iterate, not terminate.
		if stopReason == "end_turn" {
			stopReason = "tool_use"
		}
	}

	if usage == nil {
		usage = &providers.Usage{Model: model}
	}
	// RFC AV: tag the per-call usage with which credential scope paid.
	if usage != nil {
		usage.CredentialSource = credSource
		usage.CredentialScopeID = credScopeID
	}
	send(providers.Event{
		Type:       providers.EventDone,
		StopReason: stopReason,
		Usage:      usage,
	})
}

// mapStopReason translates Gemini's finishReason vocabulary into the
// shared stop-reason vocabulary the loop branches on.
//
//	STOP        → end_turn
//	MAX_TOKENS  → max_tokens
//	SAFETY      → end_turn (the loop treats it as a regular stop;
//	              the safety-rejected text is the model's final
//	              answer, surfaced as IsError if Gemini set the
//	              candidate's content to a safety block)
//	RECITATION  → end_turn
//	OTHER       → end_turn (catch-all)
//	(empty)     → end_turn
func mapStopReason(reason string) string {
	switch reason {
	case "STOP", "":
		return "end_turn"
	case "MAX_TOKENS":
		return "max_tokens"
	case "SAFETY", "RECITATION", "OTHER":
		return "end_turn"
	default:
		return "end_turn"
	}
}

// --- probe + listmodels ---

// Probe checks reachability + auth by hitting GET /v1beta/models.
// Reuses fetchModels' round-trip — a single HTTP call covers both
// health and the model-list payload.
func (d *Driver) Probe(ctx context.Context) error {
	_, err := d.fetchModels(ctx)
	return err
}

// ListModels returns the wire-name list Gemini's /v1beta/models
// endpoint serves. The wire response prefixes names with "models/";
// we strip the prefix so the resolver matches against bare aliases
// (e.g. "gemini-2.0-flash") consistent with the other drivers.
func (d *Driver) ListModels(ctx context.Context) ([]string, error) {
	return d.fetchModels(ctx)
}

func (d *Driver) fetchModels(ctx context.Context) ([]string, error) {
	url := d.baseURL + "/models"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("build models request: %w", err)
	}
	if d.apiKey != "" {
		req.Header.Set("x-goog-api-key", d.apiKey)
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("gemini /models %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	var body struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode /models: %w", err)
	}
	out := make([]string, 0, len(body.Models))
	for _, m := range body.Models {
		// Strip the "models/" prefix the API uses internally.
		name := strings.TrimPrefix(m.Name, "models/")
		if name != "" {
			out = append(out, name)
		}
	}
	return out, nil
}

// sanitizeGeminiSchema transforms a JSON-Schema tool input into the
// OpenAPI-3.0 subset Gemini's function_declarations.parameters
// accepts. Without this, MCP tool schemas generated by zod-to-json-
// schema (and similar tools that produce $ref / oneOf / anyOf /
// allOf) get rejected with `400 INVALID_ARGUMENT` at deeply nested
// paths.
//
// Three transforms applied (in order):
//
//  1. Collect `$defs` + `definitions` from anywhere in the tree
//     into a flat ref-string → schema lookup.
//  2. Walk the tree:
//     - Replace any `$ref` node with the looked-up definition
//     (deep-copied; cycles emit `{}`; unresolved refs emit `{}`).
//     - Collapse `allOf: [...]` by merging each variant's
//     `properties` + `required` into the parent.
//     - Collapse `oneOf: [...]` / `anyOf: [...]` by inlining the
//     FIRST variant's fields into the parent. This is lossy but
//     Gemini-compatible — the alternative (drop entirely) loses
//     type info; picking one variant preserves a usable shape for
//     the model.
//     - Strip the keys Gemini doesn't accept on each node:
//     `additionalProperties`, `$schema`, `$id`, `$defs`,
//     `definitions`.
//
// Best-effort: on parse failure (malformed schema), returns the
// original bytes so Gemini surfaces a clear 400 rather than the
// driver swallowing the problem.
//
// Documented in `docs/TOOLS.md`. Operators don't need to know
// which tools are Gemini-compatible — the driver does the work.
func sanitizeGeminiSchema(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return raw
	}
	defs := collectGeminiDefs(parsed)
	parsed = transformGeminiSchema(parsed, defs, map[string]bool{})
	out, err := json.Marshal(parsed)
	if err != nil {
		return raw
	}
	return out
}

// collectGeminiDefs walks the schema tree and gathers every
// `$defs` and `definitions` map it finds, keyed by canonical
// JSON-pointer ref strings (`#/$defs/Foo`, `#/definitions/Bar`).
// Nested definition blocks (a sub-schema with its own `$defs`)
// are merged into the same flat namespace — zod-to-json-schema
// hoists everything to the top, but other generators don't, so
// we tolerate both shapes.
func collectGeminiDefs(node any) map[string]any {
	defs := map[string]any{}
	collectGeminiDefsInto(node, defs)
	return defs
}

func collectGeminiDefsInto(node any, defs map[string]any) {
	switch n := node.(type) {
	case map[string]any:
		if d, ok := n["$defs"].(map[string]any); ok {
			for k, v := range d {
				defs["#/$defs/"+k] = v
			}
		}
		if d, ok := n["definitions"].(map[string]any); ok {
			for k, v := range d {
				defs["#/definitions/"+k] = v
			}
		}
		for _, v := range n {
			collectGeminiDefsInto(v, defs)
		}
	case []any:
		for _, item := range n {
			collectGeminiDefsInto(item, defs)
		}
	}
}

// transformGeminiSchema is the workhorse: per-node, it inlines
// `$ref`, collapses combinators, strips unsupported keys, then
// recurses. `visited` carries the set of ref strings currently
// being inlined on this path — entering a ref already in the set
// breaks the cycle by emitting `{}` rather than recursing
// forever.
func transformGeminiSchema(node any, defs map[string]any, visited map[string]bool) any {
	switch n := node.(type) {
	case map[string]any:
		// $ref takes precedence — replace the whole node.
		if ref, ok := n["$ref"].(string); ok {
			if visited[ref] {
				return map[string]any{}
			}
			def, found := defs[ref]
			if !found {
				return map[string]any{}
			}
			next := make(map[string]bool, len(visited)+1)
			for k, v := range visited {
				next[k] = v
			}
			next[ref] = true
			return transformGeminiSchema(deepCopyJSON(def), defs, next)
		}
		// Strip top-level def blocks + unsupported keys.
		delete(n, "$defs")
		delete(n, "definitions")
		delete(n, "additionalProperties")
		delete(n, "$schema")
		delete(n, "$id")
		// allOf / oneOf / anyOf: merge ALL variants' properties +
		// required into the parent. Gemini's OpenAPI subset can't
		// represent unions — the alternative (pick first variant)
		// silently drops every other variant's shape, which breaks
		// every Zod `z.discriminatedUnion` call once the model
		// chooses any variant past the first. Merging is lossy at
		// the discriminator-enum level (a `kind` field with
		// `enum: ["create"]` collapses to bare `type: string`) but
		// preserves every variant's payload shape — strictly less
		// surprising than dropping data.
		//
		// `mergeGeminiSchemaInto` defends against type-conflicting
		// variants (e.g. one variant `type: object`, another
		// `type: array`) by skipping the structural fields of the
		// conflicting variant.
		for _, key := range []string{"allOf", "oneOf", "anyOf"} {
			if variants, ok := n[key].([]any); ok {
				for _, v := range variants {
					sub, ok := transformGeminiSchema(v, defs, visited).(map[string]any)
					if !ok {
						continue
					}
					mergeGeminiSchemaInto(n, sub)
				}
				delete(n, key)
			}
		}
		// Recurse into remaining children.
		for k, v := range n {
			n[k] = transformGeminiSchema(v, defs, visited)
		}
		return n
	case []any:
		for i := range n {
			n[i] = transformGeminiSchema(n[i], defs, visited)
		}
		return n
	}
	return node
}

// mergeGeminiSchemaInto folds `src` into `dst` non-destructively:
// existing keys on `dst` win (so parent constraints aren't
// silently overwritten by a variant). `properties` maps are
// merged key-by-key; `required` slices are union-ed.
//
// Type-conflict defense: if `dst` and `src` declare different
// `type` values (e.g. `object` vs `array`), only non-structural
// metadata from `src` is inherited. The structural fields —
// `properties`, `items`, `required` — describe the shape that
// goes WITH the type; folding them across a type boundary
// produces a schema that's MORE broken than the input (e.g.
// `type: array` with `properties: {...}` is malformed). dst's
// type wins, src's structural fields drop.
func mergeGeminiSchemaInto(dst, src map[string]any) {
	typesConflict := false
	if dstType, dstHas := dst["type"]; dstHas {
		if srcType, srcHas := src["type"]; srcHas {
			ds, dok := dstType.(string)
			ss, sok := srcType.(string)
			// Both strings: simple compare. Either non-string (rare
			// `type: [...]` union form): conservatively treat as
			// conflicting — the schema is already in territory
			// Gemini won't fully accept.
			typesConflict = !dok || !sok || ds != ss
		}
	}
	structuralFields := map[string]bool{
		"properties": true,
		"items":      true,
		"required":   true,
	}
	for k, v := range src {
		if typesConflict && structuralFields[k] {
			continue
		}
		switch k {
		case "properties":
			if vmap, ok := v.(map[string]any); ok {
				dstProps, _ := dst["properties"].(map[string]any)
				if dstProps == nil {
					dstProps = map[string]any{}
					dst["properties"] = dstProps
				}
				for pk, pv := range vmap {
					if _, exists := dstProps[pk]; !exists {
						dstProps[pk] = pv
					}
				}
			}
		case "required":
			if varr, ok := v.([]any); ok {
				dstReq, _ := dst["required"].([]any)
				seen := map[string]bool{}
				for _, r := range dstReq {
					if s, ok := r.(string); ok {
						seen[s] = true
					}
				}
				for _, r := range varr {
					if s, ok := r.(string); ok && !seen[s] {
						dstReq = append(dstReq, s)
						seen[s] = true
					}
				}
				dst["required"] = dstReq
			}
		default:
			if _, exists := dst[k]; !exists {
				dst[k] = v
			}
		}
	}
}

// deepCopyJSON returns a structural copy of a parsed JSON value
// so the inlined ref doesn't share state with the original
// definition (subsequent transforms would otherwise corrupt the
// def for any later $ref pointing at the same key).
func deepCopyJSON(v any) any {
	switch n := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(n))
		for k, vv := range n {
			out[k] = deepCopyJSON(vv)
		}
		return out
	case []any:
		out := make([]any, len(n))
		for i, vv := range n {
			out[i] = deepCopyJSON(vv)
		}
		return out
	}
	return v
}
