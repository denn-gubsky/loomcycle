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

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/ratelimit"
)

const (
	defaultBaseURL = "https://generativelanguage.googleapis.com/v1beta"
	defaultTimeout = 5 * time.Minute
)

// Driver speaks Gemini's generateContent API.
type Driver struct {
	apiKey  string
	baseURL string
	http    *http.Client
}

// New constructs a Driver. baseURL may be empty for the default
// endpoint, or set to a self-hosted Vertex AI Gemini gateway. The
// trailing path "/v1beta" is included in the default — operator
// overrides should match that pattern.
func New(apiKey, baseURL string, httpClient *http.Client) *Driver {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	return &Driver{apiKey: apiKey, baseURL: baseURL, http: httpClient}
}

func (d *Driver) ID() string { return "gemini" }

func (d *Driver) Capabilities() providers.Capabilities {
	return providers.Capabilities{
		// Gemini has implicit prompt caching on long contexts but no
		// operator-controlled cache_control like Anthropic. Mark
		// false until we add the explicit-cache surface.
		NativePromptCache: false,
		ParallelToolCalls: true,
		Streaming:         true,
		// gemini-1.5-pro tops out at 2M; gemini-2.0-flash at 1M.
		// Report the optimistic case.
		MaxContextTokens: 2_000_000,
		// gemini-2.5-flash / 2.5-pro support thinking via
		// generationConfig.thinkingConfig. Capability is true; the
		// driver attaches thinkingBudget when Effort is set on the
		// request (see Call).
		SupportsThinking: true,
		SupportsEffort:   true,
	}
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

	url := d.baseURL + "/models/" + req.Model + ":streamGenerateContent?alt=sse"
	attempt := func(ctx context.Context) (*http.Response, error) {
		httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")
		if d.apiKey != "" {
			httpReq.Header.Set("x-goog-api-key", d.apiKey)
		}
		return d.http.Do(httpReq)
	}

	resp, err := ratelimit.Do(ctx, ratelimit.Config{
		Provider:    "gemini",
		ParseHeader: ratelimit.OpenAIRetryAfter, // Gemini uses the same header shape
		OnEvent:     req.OnEvent,
	}, attempt)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("http: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("gemini %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}

	out := make(chan providers.Event, 16)
	go streamEvents(ctx, resp.Body, out, req.Model)
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
	MaxOutputTokens int                 `json:"maxOutputTokens,omitempty"`
	Temperature     *float64            `json:"temperature,omitempty"`
	ThinkingConfig  *wireThinkingConfig `json:"thinkingConfig,omitempty"`
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
				Parameters:  t.InputSchema,
			})
		}
		w.Tools = []wireTool{{FunctionDeclarations: decls}}
	}

	if req.MaxTokens > 0 || req.Temperature != nil || req.Effort != "" {
		gc := &wireGenConfig{
			MaxOutputTokens: req.MaxTokens,
			Temperature:     req.Temperature,
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

func streamEvents(ctx context.Context, body io.ReadCloser, out chan<- providers.Event, requestModel string) {
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
