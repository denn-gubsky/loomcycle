// Package openai implements the Provider interface for OpenAI's Chat
// Completions API. We picked Chat Completions over the newer Responses API
// because (a) most OpenAI-compatible endpoints (vLLM, LiteLLM, Anyscale,
// Together, ...) speak Chat Completions, not Responses, and (b) the Responses
// API ships built-in tools (web_search, code_interpreter) that fight our
// "we own the tool loop" stance — Chat Completions stays out of our way.
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/ratelimit"
	"github.com/denn-gubsky/loomcycle/internal/providers/streamhttp"
)

const (
	defaultBaseURL = "https://api.openai.com/v1"
)

// Driver speaks Chat Completions.
type Driver struct {
	apiKey      string
	baseURL     string
	http        *http.Client
	idleTimeout time.Duration
	// keyEnvName is the well-known env-var NAME whose tenant/user credential
	// overrides the operator host key for an inference request (RFC AR).
	// Defaults to "OPENAI_API_KEY"; the DeepSeek wrapper points it at
	// "DEEPSEEK_API_KEY" via SetKeyEnvName since it reuses this driver.
	keyEnvName string
}

// New constructs a Driver. baseURL may be empty for the default endpoint, or
// set to any OpenAI-compatible base (e.g. "http://localhost:8000/v1" for vLLM).
// streamOpts controls per-stream timeouts (zero = streamhttp defaults). When
// httpClient is nil, a fresh streaming client honoring streamOpts.HeaderTimeout
// is built.
func New(apiKey, baseURL string, streamOpts streamhttp.Options, httpClient *http.Client) *Driver {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	streamOpts = streamOpts.Resolve()
	if httpClient == nil {
		httpClient = streamhttp.NewClient(streamOpts.HeaderTimeout)
	}
	return &Driver{apiKey: apiKey, baseURL: baseURL, http: httpClient, idleTimeout: streamOpts.IdleTimeout, keyEnvName: "OPENAI_API_KEY"}
}

// SetKeyEnvName overrides the env-var name whose tenant/user credential shadows
// the host key (RFC AR). Used by the DeepSeek wrapper, which reuses this driver
// but must resolve DEEPSEEK_API_KEY, not OPENAI_API_KEY.
func (d *Driver) SetKeyEnvName(name string) { d.keyEnvName = name }

// resolveKey returns the API key to authenticate an inference request AND which
// credential scope it came from. A tenant/user credential named d.keyEnvName
// (default "OPENAI_API_KEY"; the DeepSeek wrapper sets "DEEPSEEK_API_KEY")
// overrides the operator's host key (RFC AR); source is "operator" when none
// did. The source/scopeID ride the per-call Usage so the server can attribute
// spend (RFC AV). Model-availability probes (fetchModels) stay on the host key
// — which models are reachable is an operator concern.
func (d *Driver) resolveKey(ctx context.Context) (key, source, scopeID string) {
	if r, ok := providers.ResolveCredentialFull(ctx, d.keyEnvName); ok {
		return r.Value, r.Scope, r.ScopeID
	}
	return d.apiKey, "operator", ""
}

func (d *Driver) ID() string { return "openai" }

func (d *Driver) Capabilities() providers.Capabilities {
	return providers.Capabilities{
		NativePromptCache: false, // OpenAI auto-caches; no explicit knob like Anthropic
		ParallelToolCalls: true,
		Streaming:         true,
		MaxContextTokens:  128_000, // gpt-4o family default; bigger on some
		SupportsThinking:  false,
		// SupportsEffort=true means the driver translates Request.Effort
		// to reasoning_effort on the wire. Whether the resolved model
		// actually honours it (o-series + GPT-5 do; chat-only models
		// like gpt-5.4-mini reject) is the API's decision — the driver
		// passes through; operators using effort with non-reasoning
		// models will see the API's 400 surface clearly.
		SupportsEffort: true,
		SupportsVision: true, // per-model refined by openaiSupportsVision
	}
}

// Call sends the request and returns a channel of Events. The goroutine that
// reads the response closes the channel when the stream ends.
//
// 429 retry: ratelimit.Do honours OpenAI's Retry-After header when present;
// otherwise it reads the bigger of x-ratelimit-reset-{requests,tokens}
// (relative durations like "120ms" or "12.5s") so we wait for the more
// constrained bucket. The same body bytes are re-sent — full conversation
// context preserved across the rate limit.
func (d *Driver) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	body, err := buildRequestBody(req)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	streamCtx, cancelStream := context.WithCancel(ctx)

	// RFC AR/AV: resolve the key + its owning scope ONCE per call (not per retry
	// attempt) so the header uses it and the source rides the per-call Usage.
	apiKey, credSource, credScopeID := d.resolveKey(ctx)

	attempt := func(attemptCtx context.Context) (*http.Response, error) {
		// v0.10.0 OTEL: one loomcycle.provider.call span per attempt.
		// The provider attribute defaults to "openai" but a wrapping
		// driver (DeepSeek today) can set lcotel.WithProviderOverride
		// on the ctx so Jaeger operators see the right provider label
		// without a meaningless wrapping span.
		provider := lcotel.ProviderOverride(attemptCtx)
		if provider == "" {
			provider = "openai"
		}
		spanCtx, span := lcotel.RecordProviderCall(attemptCtx, lcotel.ProviderCallAttrs{
			Provider: provider,
			Model:    req.Model,
			Effort:   req.Effort,
		})
		defer span.End()
		httpReq, err := http.NewRequestWithContext(spanCtx, "POST", d.baseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			lcotel.SetSpanError(span, err)
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")
		if apiKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+apiKey)
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
		Provider:    "openai",
		ParseHeader: ratelimit.OpenAIRetryAfter,
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
		return nil, fmt.Errorf("openai %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}

	resp.Body = streamhttp.WrapBody(resp.Body, d.idleTimeout, cancelStream)

	out := make(chan providers.Event, 16)
	go func() {
		defer cancelStream()
		streamEvents(streamCtx, resp.Body, out, credSource, credScopeID)
	}()
	return out, nil
}

// --- request marshalling ---
//
// Chat Completions message shapes:
//
//   { "role": "system",    "content": "..." }
//   { "role": "user",      "content": "..." }
//   { "role": "assistant", "content": "...", "tool_calls": [...] }
//   { "role": "tool",      "tool_call_id": "...", "content": "..." }
//
// Our internal ContentBlock union flattens to these as follows:
//   - Anthropic-style assistant message with tool_use blocks → role:"assistant"
//     with the text concatenated into "content" and tool_use blocks lifted
//     into "tool_calls".
//   - tool_result blocks (which we encode in user-role messages) → split out
//     into separate role:"tool" messages.

type wireRequest struct {
	Model     string        `json:"model"`
	Messages  []wireMessage `json:"messages"`
	Tools     []wireTool    `json:"tools,omitempty"`
	MaxTokens int           `json:"max_tokens,omitempty"`
	// MaxCompletionTokens is the reasoning-model spelling of the output cap.
	// OpenAI's o-series / GPT-5 reasoning models REJECT max_tokens with a 400
	// ("Unsupported parameter: max_tokens … use max_completion_tokens instead")
	// — buildRequestBody sends exactly one of the two based on the model. Chat
	// models (gpt-4o, DeepSeek's OpenAI-compat surface) keep max_tokens.
	MaxCompletionTokens int      `json:"max_completion_tokens,omitempty"`
	Temperature         *float64 `json:"temperature,omitempty"`
	Stream              bool     `json:"stream"`

	// Sampling knobs OpenAI's /v1/chat/completions accepts (DeepSeek's
	// OpenAI-compat surface inherits the same field names). top_k is NOT an
	// OpenAI param, so it's intentionally absent here.
	TopP             *float64 `json:"top_p,omitempty"`
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64 `json:"presence_penalty,omitempty"`
	Seed             *int     `json:"seed,omitempty"`
	Stop             []string `json:"stop,omitempty"`

	// stream_options.include_usage tells OpenAI to include final usage in the
	// last data frame before [DONE]. Without this we have no token counts.
	StreamOptions *wireStreamOptions `json:"stream_options,omitempty"`

	// ReasoningEffort is the wire param OpenAI's reasoning models
	// (o-series, GPT-5+) accept. Pass-through of the operator's
	// effort hint: "low" / "medium" / "high". Empty = omit (driver
	// default; chat models that don't accept the param ignore it).
	// DeepSeek's /v1/chat/completions wrapper inherits this verbatim
	// — DeepSeek V4 accepts the same field name per its OpenAI-compat
	// surface. The API rejects the field on non-reasoning models
	// (gpt-5.4-mini, etc.) with a 400; operators using effort with
	// those models should expect the rejection rather than silent drop.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

type wireStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireMessage struct {
	Role string `json:"role"`
	// Content is `any` so a user message can carry EITHER the flat string form
	// (text-only — byte-identical to the pre-vision path) OR the content-array
	// form []wireContentPart when an image block is present (RFC AT). omitempty
	// drops only a nil interface — assistant/user paths therefore leave Content
	// nil (not "") to keep the empty-content omission (RFC Q); tool messages
	// force content present via MarshalJSON below.
	Content    any            `json:"content,omitempty"`
	Name       string         `json:"name,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	// ReasoningContent is the per-turn reasoning trace DeepSeek V4 Pro
	// (and deepseek-reasoner) require operators to echo back on
	// subsequent assistant-history turns. omitempty keeps it off the
	// wire for non-thinking models / non-DeepSeek endpoints.
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// MarshalJSON forces `content` to always be present on role:"tool"
// messages, even when empty. OpenAI tolerates an omitted content on a
// tool message, but DeepSeek's stricter deserializer rejects it with
// 400 "missing field content" — which broke every tool-using DeepSeek
// agent the moment a tool returned empty output (a silent `mkdir`, a
// script that only writes files; see RFC Q / finding F10). For every
// other role the standard omitempty marshaling is preserved byte-for-
// byte — notably an assistant message carrying only tool_calls still
// omits content, which both OpenAI and DeepSeek accept.
func (m wireMessage) MarshalJSON() ([]byte, error) {
	type alias wireMessage // shed the MarshalJSON method to avoid recursion
	if m.Role == "tool" {
		// The outer Content (no omitempty, depth 0) overrides the
		// embedded alias's omitempty content (depth 1) — encoding/json
		// resolves the same-name conflict in favour of the shallower
		// field, so `content` is always emitted for tool messages. A
		// tool message's content is always the flat string form, so the
		// `any` coerces cleanly (zero value "" when no output).
		txt, _ := m.Content.(string)
		return json.Marshal(struct {
			alias
			Content string `json:"content"`
		}{alias: alias(m), Content: txt})
	}
	return json.Marshal(alias(m))
}

// wireContentPart is one element of the OpenAI content-array form, used only
// when a user message carries an image (RFC AT). A part is either a text part
// or an image_url part holding an inline data-URI.
type wireContentPart struct {
	Type     string        `json:"type"` // "text" | "image_url"
	Text     string        `json:"text,omitempty"`
	ImageURL *wireImageURL `json:"image_url,omitempty"`
}

// wireImageURL carries the data-URI the driver builds internally from the image
// block's media_type + base64. Callers never send URLs (SSRF — RFC AT §6).
type wireImageURL struct {
	URL string `json:"url"` // "data:<media_type>;base64,<data>"
}

type wireToolCall struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"` // always "function"
	Function wireToolCallFn `json:"function"`
}

type wireToolCallFn struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string, not a JSON object
}

type wireTool struct {
	Type     string       `json:"type"` // "function"
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

func buildRequestBody(req providers.Request) ([]byte, error) {
	// Refuse an image to a text-only OpenAI model before the call — a clear
	// error beats an opaque provider 400 (RFC AT §5.2). DeepSeek (which
	// delegates Call here) never reaches this: its Capabilities advertises
	// SupportsVision=false, so the loop gates an image to DeepSeek upstream.
	if providers.RequestHasImage(req) && !openaiSupportsVision(req.Model) {
		return nil, fmt.Errorf("openai model %q does not support image input", req.Model)
	}

	w := wireRequest{
		Model:            req.Model,
		Temperature:      req.Temperature,
		TopP:             req.TopP,
		FrequencyPenalty: req.FrequencyPenalty,
		PresencePenalty:  req.PresencePenalty,
		Seed:             req.Seed,
		Stop:             req.Stop,
		Stream:           true,
		StreamOptions:    &wireStreamOptions{IncludeUsage: true},
		// Pass-through of Request.Effort to OpenAI's reasoning_effort
		// param. Empty stays empty — omitempty drops it from the wire.
		// "low" / "medium" / "high" pass through verbatim; the API
		// rejects unknown strings, which is fine because we only ever
		// receive values from the validated Effort enum at the config
		// layer.
		ReasoningEffort: req.Effort,
	}

	// Reasoning models (o-series / GPT-5) reject max_tokens and require
	// max_completion_tokens; chat models (gpt-4o) and DeepSeek's OpenAI-compat
	// surface keep max_tokens. Send exactly one, by model — omitempty drops a
	// zero cap either way.
	if openaiIsReasoningModel(req.Model) {
		w.MaxCompletionTokens = req.MaxTokens
	} else {
		w.MaxTokens = req.MaxTokens
	}

	// System blocks become a single role:"system" message at the top.
	// Cacheable hint is dropped — OpenAI auto-caches and gives no API knob.
	if len(req.System) > 0 {
		var sysText strings.Builder
		for _, sb := range req.System {
			if sysText.Len() > 0 {
				sysText.WriteString("\n\n")
			}
			sysText.WriteString(sb.Text)
		}
		w.Messages = append(w.Messages, wireMessage{Role: "system", Content: sysText.String()})
	}

	for _, m := range req.Messages {
		w.Messages = append(w.Messages, flattenMessage(m)...)
	}

	for _, t := range req.Tools {
		w.Tools = append(w.Tools, wireTool{
			Type: "function",
			Function: wireFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}

	return json.Marshal(w)
}

// flattenMessage maps one of our ContentBlock-union messages into one or more
// Chat Completions wire messages. The split is needed because:
//   - Assistant messages combine text + tool_use blocks (one wire message).
//   - User messages may carry tool_result blocks; each tool_result becomes
//     its own role:"tool" wire message, and any non-tool_result text blocks
//     stay in the original user message.
func flattenMessage(m providers.Message) []wireMessage {
	if m.Role == "assistant" {
		var text strings.Builder
		var calls []wireToolCall
		for _, c := range m.Content {
			switch c.Type {
			case "text":
				text.WriteString(c.Text)
			case "tool_use":
				calls = append(calls, wireToolCall{
					ID:   c.ToolUseID,
					Type: "function",
					Function: wireToolCallFn{
						Name:      c.ToolName,
						Arguments: string(c.ToolInput),
					},
				})
			}
		}
		// Echo back reasoning_content if the message carries one
		// (DeepSeek V4 Pro / deepseek-reasoner contract — sending the
		// assistant turn back without it 400s with "reasoning_content
		// in the thinking mode must be passed back"). omitempty keeps
		// the field off the wire for non-thinking models / non-DeepSeek
		// endpoints; vanilla OpenAI ignores unknown fields anyway.
		wm := wireMessage{Role: "assistant", ToolCalls: calls, ReasoningContent: m.Reasoning}
		// Leave Content nil (omitted) for a tool_calls-only assistant turn —
		// both OpenAI and DeepSeek require that (RFC Q). Now that Content is
		// `any`, an empty string "" would be EMITTED by omitempty, so only set
		// it when there is text.
		if text.Len() > 0 {
			wm.Content = text.String()
		}
		return []wireMessage{wm}
	}

	// user role: tool_result blocks become their own role:"tool" messages; text
	// + image blocks form one user message. An image forces the OpenAI
	// content-array form (text + image_url parts, original order); a text-only
	// turn keeps the flat string content (byte-identical to the pre-vision path).
	var out []wireMessage
	var userText strings.Builder
	var parts []wireContentPart
	hasImage := false
	for _, c := range m.Content {
		switch c.Type {
		case "tool_result":
			out = append(out, wireMessage{
				Role:       "tool",
				ToolCallID: c.ToolUseID,
				Content:    c.Text,
			})
		case "text":
			if userText.Len() > 0 {
				userText.WriteString("\n")
			}
			userText.WriteString(c.Text)
			parts = append(parts, wireContentPart{Type: "text", Text: c.Text})
		case "image":
			hasImage = true
			parts = append(parts, wireContentPart{
				Type:     "image_url",
				ImageURL: &wireImageURL{URL: "data:" + c.MediaType + ";base64," + c.Data},
			})
		}
	}
	switch {
	case hasImage:
		// Plain user content (array form) comes first; tool messages follow.
		out = append([]wireMessage{{Role: "user", Content: parts}}, out...)
	case userText.Len() > 0:
		out = append([]wireMessage{{Role: "user", Content: userText.String()}}, out...)
	}
	return out
}

// openaiIsReasoningModel reports whether the named model is an OpenAI reasoning
// model (o-series or GPT-5 line) that requires max_completion_tokens and rejects
// max_tokens. Match is by name so the shared driver serves DeepSeek safely:
// deepseek-* never matches, so DeepSeek keeps max_tokens (its OpenAI-compat
// surface accepts it). gpt-4o is NOT a reasoning model — the "gpt-5" substring
// won't match it, and the o-series prefixes are anchored so "gpt-4o" is excluded.
// An unknown model is treated as a chat model (keeps max_tokens); a wrong guess
// surfaces as a provider 400, never a silent drop.
func openaiIsReasoningModel(model string) bool {
	m := strings.ToLower(model)
	if strings.Contains(m, "gpt-5") {
		return true
	}
	// o-series: o1 / o3 / o4 (+ future o5), optionally suffixed (-mini, dates).
	for _, p := range []string{"o1", "o3", "o4", "o5"} {
		if m == p || strings.HasPrefix(m, p+"-") {
			return true
		}
	}
	return false
}

// openaiSupportsVision reports whether the named OpenAI model accepts image
// input. The modern multimodal families (gpt-4o, gpt-4.1, gpt-4-turbo, the
// gpt-5 line, the o-series) support it; the legacy text-only families
// (gpt-3.5-turbo and the original gpt-4 / gpt-4-32k snapshots) do not. An
// unknown model defaults to supported — a wrong guess surfaces as a provider
// 400, never a silent drop, and never wrongly BLOCKS a valid request (RFC AT
// §7). Capabilities() takes no model arg, so this is the per-model affordance.
func openaiSupportsVision(model string) bool {
	m := strings.ToLower(model)
	if strings.HasPrefix(m, "gpt-3.5") {
		return false
	}
	return !openaiTextOnlyModels[m]
}

// openaiTextOnlyModels are the exact original gpt-4 snapshots that predate
// vision (gpt-4-turbo and later are multimodal). Matched exactly — NOT by
// prefix — so a turbo/preview snapshot like gpt-4-0125-preview is not wrongly
// gated.
var openaiTextOnlyModels = map[string]bool{
	"gpt-4":          true,
	"gpt-4-0314":     true,
	"gpt-4-0613":     true,
	"gpt-4-32k":      true,
	"gpt-4-32k-0314": true,
	"gpt-4-32k-0613": true,
}

// --- streaming response parsing ---
//
// Chat Completions SSE frames look like:
//   data: {"choices":[{"delta":{"content":"..."}}]}\n\n
//   data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"...","function":{...}}]}}]}\n\n
//   data: [DONE]\n\n
//
// Tool calls stream by index: the first delta for an index has the id +
// function.name, subsequent deltas only fill in function.arguments piecewise.
// We accumulate into toolAcc[index] and emit one EventToolCall per index when
// streaming completes.

type chunkDelta struct {
	Content   string             `json:"content"`
	ToolCalls []chunkToolCallDel `json:"tool_calls"`
	// ReasoningContent streams alongside Content for thinking-mode
	// models (DeepSeek V4 Pro, deepseek-reasoner). The deltas
	// concatenate into the per-turn reasoning trace, which the
	// driver surfaces on EventDone.Reasoning so the loop can echo
	// it back on the next iteration.
	ReasoningContent string `json:"reasoning_content"`
}

type chunkToolCallDel struct {
	Index    int            `json:"index"`
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Function chunkFunctionD `json:"function"`
}

type chunkFunctionD struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chunk struct {
	// Model is the wire-resolved alias (e.g. "gpt-4o-mini-2024-07-18"
	// or "deepseek-chat") set on every chunk by OpenAI-compatible
	// servers. We capture it so the final EventDone.Usage carries
	// the actual billed model rather than an empty string. Without
	// this, runs.model never populates downstream — same regression
	// class as the v0.4.0 Anthropic fix (commit 5bdccfc), just for
	// OpenAI / DeepSeek / vLLM / any OpenAI-compatible endpoint.
	Model   string `json:"model"`
	Choices []struct {
		Delta        chunkDelta `json:"delta"`
		FinishReason string     `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		// gpt-4o-mini and later expose cached prompt tokens here:
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

// toolAccumulator buffers a streaming tool_call.
type toolAccumulator struct {
	id   string
	name string
	args strings.Builder
}

func streamEvents(ctx context.Context, body io.ReadCloser, out chan<- providers.Event, credSource, credScopeID string) {
	defer body.Close()
	defer close(out)

	// send respects ctx so a cancelled request doesn't leak this goroutine on
	// a full unread channel. Returns false if ctx ended; callers should return
	// immediately so defer close(out) fires and the consumer's range exits.
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

	tools := map[int]*toolAccumulator{}
	var stopReason string
	var usage *providers.Usage
	// model is the wire-resolved alias captured from the first
	// chunk that carries one. Stamped onto Usage when the usage
	// chunk arrives (or onto an empty Usage at done time if
	// stream_options.include_usage didn't fire — defensive, since
	// some OpenAI-compatible servers omit usage on cancelled
	// streams).
	var model string
	// reasoningBuf accumulates `delta.reasoning_content` chunks for
	// thinking-mode models (DeepSeek V4 Pro / deepseek-reasoner).
	// Surfaced on EventDone.Reasoning so the loop can stamp it onto
	// the assistant Message it appends to the conversation history.
	// The next iteration's request body then echoes it back via
	// wireMessage.ReasoningContent — DeepSeek's API requires this or
	// it 400s with "reasoning_content in the thinking mode must be
	// passed back". Empty for non-thinking models.
	var reasoningBuf strings.Builder

	// textBuf coalesces consecutive `delta.content` chunks into
	// reasonable-sized EventText emissions. OpenAI-compatible endpoints
	// (especially DeepSeek) often stream one token per delta — every
	// delta becomes an EventText becomes a render line on log-based
	// consumers, producing the "every word on its own line" visual
	// noise reported in the field. Anthropic's wire is naturally chunked
	// at multi-token granularity; this brings the OpenAI path closer to
	// that shape without adding a timer goroutine.
	//
	// Flush points: textCoalesceMin reached, the new chunk contains a
	// newline (preserve formatting boundaries), end-of-stream (before
	// tool_call emissions and EventDone). The threshold is small enough
	// that a streaming UI still feels live (~64-char chunks ≈ a phrase
	// per render frame); larger reduces event count more but degrades
	// typewriter feel.
	var textBuf strings.Builder
	const textCoalesceMin = 64
	flushText := func() bool {
		if textBuf.Len() == 0 {
			return true
		}
		s := textBuf.String()
		textBuf.Reset()
		return send(providers.Event{Type: providers.EventText, Text: s})
	}

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
			continue // skip malformed frames; final usage frame may be malformed in cancelled streams
		}
		if c.Model != "" && model == "" {
			model = c.Model
		}

		for _, ch := range c.Choices {
			if ch.Delta.ReasoningContent != "" {
				// Surface live as EventThinking so consumers can render
				// the trace as it streams. ALSO accumulate into
				// reasoningBuf — EventDone.Reasoning still carries the
				// consolidated trace because the loop stamps it onto
				// the assistant Message for next-turn echo (DeepSeek's
				// "reasoning_content must be passed back" requirement).
				if !send(providers.Event{Type: providers.EventThinking, Text: ch.Delta.ReasoningContent}) {
					return
				}
				reasoningBuf.WriteString(ch.Delta.ReasoningContent)
			}
			if ch.Delta.Content != "" {
				textBuf.WriteString(ch.Delta.Content)
				if textBuf.Len() >= textCoalesceMin || strings.ContainsRune(ch.Delta.Content, '\n') {
					if !flushText() {
						return
					}
				}
			}
			for _, tc := range ch.Delta.ToolCalls {
				acc, ok := tools[tc.Index]
				if !ok {
					acc = &toolAccumulator{}
					tools[tc.Index] = acc
				}
				if tc.ID != "" {
					acc.id = tc.ID
				}
				if tc.Function.Name != "" {
					acc.name = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					acc.args.WriteString(tc.Function.Arguments)
				}
			}
			if ch.FinishReason != "" {
				stopReason = mapStopReason(ch.FinishReason)
			}
		}

		if c.Usage != nil {
			u := &providers.Usage{
				InputTokens:  c.Usage.PromptTokens,
				OutputTokens: c.Usage.CompletionTokens,
				Model:        model,
			}
			if c.Usage.PromptTokensDetails != nil {
				u.CacheReadTokens = c.Usage.PromptTokensDetails.CachedTokens
			}
			usage = u
		}
	}
	if err := scanner.Err(); err != nil {
		// Flush any buffered text before the error event so bytes the
		// wire delivered aren't silently dropped just because the read
		// failed mid-stream. Best-effort: a send failure here is fine
		// (the error event still surfaces the failure to the caller).
		_ = flushText()
		send(providers.Event{Type: providers.EventError, Error: "stream read: " + err.Error()})
		return
	}

	// Flush any text still buffered by the coalescer before tool_call /
	// done events — preserves "text precedes tool_call" ordering and
	// guarantees no text is dropped on stream end.
	if !flushText() {
		return
	}

	// Emit accumulated tool calls in index order before the done event.
	// Indices may be non-contiguous (e.g. 0, 2 with a gap at 1) — legal per
	// the OpenAI spec — so we sort the actual keys rather than iterating
	// 0..len(tools), which would silently drop any index >= len(tools).
	indices := make([]int, 0, len(tools))
	for i := range tools {
		indices = append(indices, i)
	}
	sort.Ints(indices)
	for _, i := range indices {
		acc := tools[i]
		args := acc.args.String()
		if args == "" {
			args = "{}"
		}
		if !send(providers.Event{
			Type: providers.EventToolCall,
			ToolUse: &providers.ToolUse{
				ID:    acc.id,
				Name:  acc.name,
				Input: json.RawMessage(args),
			},
		}) {
			return
		}
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
		Reasoning:  reasoningBuf.String(),
	})
}

// mapStopReason translates OpenAI's finish_reason vocabulary into our shared
// vocabulary. Both sides have "stop"/"end_turn", "tool_calls"/"tool_use", and
// "length"/"max_tokens"; we normalise on the Anthropic spelling because the
// agent loop already branches on that.
func mapStopReason(openaiReason string) string {
	switch openaiReason {
	case "stop":
		return "end_turn"
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	default:
		return openaiReason
	}
}

// Probe checks reachability + auth by hitting GET /v1/models with the
// passed context's deadline. Reuses fetchModels' round-trip so a single
// HTTP call covers both health and the model-list payload — the
// caller decides which signal it needs.
func (d *Driver) Probe(ctx context.Context) error {
	_, err := d.fetchModels(ctx)
	return err
}

// ListModels returns the wire aliases the OpenAI-compatible endpoint
// currently serves. Used by the resolver to populate the Listed flag
// in ModelStatus. DeepSeek's /v1/models has the same response shape,
// so the deepseek wrapper inherits this method unchanged.
func (d *Driver) ListModels(ctx context.Context) ([]string, error) {
	return d.fetchModels(ctx)
}

// fetchModels is the shared GET /v1/models round-trip. OpenAI's
// response shape:
//
//	{"object": "list",
//	 "data": [{"id": "gpt-5.4", "object": "model", "created": ...},
//	          {"id": "gpt-4o-mini", ...},
//	          ...]}
//
// We surface only the IDs. OpenAI's response is small enough (a few
// dozen models for a typical org) that one page suffices.
func (d *Driver) fetchModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+d.apiKey)
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai /v1/models: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("openai /v1/models: status %d (%s)", resp.StatusCode, string(body))
	}
	var doc struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("openai /v1/models decode: %w", err)
	}
	out := make([]string, 0, len(doc.Data))
	for _, m := range doc.Data {
		out = append(out, m.ID)
	}
	return out, nil
}
