package http

import "encoding/json"

// openai_compat_types.go — v0.11.3 OpenAI-compatible wire shapes for
// POST /v1/chat/completions. Mirrors the OpenAI Chat Completions API
// closely enough that consumers using the OpenAI SDK can point at
// loomcycle by changing only the base URL.
//
// The shim translates these shapes into loomcycle's native
// llmChatRequest/llmChatResponse + delegates to the same dispatch
// path /v1/_llm/chat uses (prepareGatewayDispatch). Security policy
// (per-user quota, resolver pin precedence, log auditing) lives in
// one place; the shim is purely a wire-format translator.

// openaiChatRequest is the POST /v1/chat/completions body. Only the
// fields loomcycle's resolver + providers consume are honored;
// unknown fields decode silently. Documented unhandled fields
// (`n`, `presence_penalty`, `frequency_penalty`, `top_p`, `seed`,
// `response_format`, `logit_bias`, `tool_choice`, `top_logprobs`,
// `user`) are accepted but ignored — that matches "drop-in for
// OpenAI SDK consumers" better than rejecting requests for fields
// loomcycle doesn't apply.
type openaiChatRequest struct {
	Model       string              `json:"model"`
	Messages    []openaiChatMessage `json:"messages"`
	Tools       []openaiTool        `json:"tools,omitempty"`
	MaxTokens   int                 `json:"max_tokens,omitempty"`
	Temperature *float64            `json:"temperature,omitempty"`
	Stop        json.RawMessage     `json:"stop,omitempty"` // string OR []string per OpenAI; accepted-but-ignored in v1
	Stream      bool                `json:"stream,omitempty"`

	// loomcycle-specific extensions kept namespaced so OpenAI SDKs
	// that pass-through unknown fields don't trip on them. Consumers
	// who want loomcycle's per-user quota / tier overlay can set
	// these explicitly; everyone else gets the resolver default.
	LoomcycleProvider string `json:"loomcycle_provider,omitempty"`
	LoomcycleTier     string `json:"loomcycle_tier,omitempty"`
	LoomcycleUserID   string `json:"loomcycle_user_id,omitempty"`
	LoomcycleUserTier string `json:"loomcycle_user_tier,omitempty"`

	// OpenAI's `user` field — opaque end-user identifier. We map it
	// to loomcycle's user_id when LoomcycleUserID isn't explicitly
	// set, so SDK callers who already pass `user: "alice"` get
	// per-user quota tracking automatically.
	User string `json:"user,omitempty"`
}

// openaiChatMessage is one message in the OpenAI shape.
//
// `Content` is `string | []ContentPart | null` per the OpenAI spec.
// v1 handles the string form fully; arrays are flattened to
// text-only (multimodal image/audio parts ignored — same scope cut
// as the native loomcycle shape). `null` for assistant turns with
// tool_calls is normal.
type openaiChatMessage struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content,omitempty"` // string | []part | null
	ToolCalls  []openaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	Name       string           `json:"name,omitempty"`
}

// openaiToolCall is an assistant turn's tool-invocation entry. Note
// that OpenAI wraps the actual function in a `function` sub-object;
// loomcycle's native shape flattens id/name/input.
type openaiToolCall struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"type"` // always "function" today
	Function openaiToolCallFunction `json:"function"`
}

type openaiToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON STRING per OpenAI (not parsed object)
}

// openaiTool describes one tool. OpenAI wraps every tool in a
// `function` envelope; loomcycle's native shape is flat. The shim
// unwraps on the way in and re-wraps on the way out.
type openaiTool struct {
	Type     string             `json:"type"` // always "function" today
	Function openaiToolFunction `json:"function"`
}

type openaiToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"` // OpenAI calls it "parameters"; equivalent to loomcycle's input_schema
}

// openaiChatResponse is the non-streaming response shape.
type openaiChatResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`  // always "chat.completion"
	Created int64          `json:"created"` // Unix timestamp (seconds)
	Model   string         `json:"model"`
	Choices []openaiChoice `json:"choices"`
	Usage   openaiUsage    `json:"usage"`
}

type openaiChoice struct {
	Index        int               `json:"index"`
	Message      openaiChatMessage `json:"message"`
	FinishReason string            `json:"finish_reason"`
}

type openaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// openaiChunkResponse is the streaming chunk shape — one per SSE
// frame. The terminal `data: [DONE]\n\n` frame is NOT a JSON
// object; the handler emits the raw `[DONE]` string after the last
// chunk per OpenAI's protocol.
type openaiChunkResponse struct {
	ID      string              `json:"id"`
	Object  string              `json:"object"` // always "chat.completion.chunk"
	Created int64               `json:"created"`
	Model   string              `json:"model"`
	Choices []openaiChunkChoice `json:"choices"`
	// Usage is sent only on the FINAL chunk per OpenAI's
	// stream_options.include_usage semantics. v1 always includes it
	// on the final chunk (the operator-friendly default).
	Usage *openaiUsage `json:"usage,omitempty"`
}

type openaiChunkChoice struct {
	Index        int              `json:"index"`
	Delta        openaiChunkDelta `json:"delta"`
	FinishReason *string          `json:"finish_reason"` // null until final chunk
}

// openaiChunkDelta is the incremental update payload per chunk.
// Fields appear or don't based on what changed in that chunk —
// `Role` on the first chunk only; `Content` for text deltas;
// `ToolCalls` for tool-call deltas.
type openaiChunkDelta struct {
	Role      string                `json:"role,omitempty"`
	Content   string                `json:"content,omitempty"`
	ToolCalls []openaiChunkToolCall `json:"tool_calls,omitempty"`
}

// openaiChunkToolCall is the per-chunk tool-call delta. The `Index`
// field is the position in the message's tool_calls array — required
// because each tool call is built up incrementally over multiple
// chunks. v1 emits each tool call as a single chunk (full args at
// once); a future iteration can stream partial-JSON args.
type openaiChunkToolCall struct {
	Index    int                    `json:"index"`
	ID       string                 `json:"id,omitempty"`   // only on first chunk for this tool
	Type     string                 `json:"type,omitempty"` // "function"; only on first chunk
	Function *openaiChunkToolCallFn `json:"function,omitempty"`
}

type openaiChunkToolCallFn struct {
	Name      string `json:"name,omitempty"`      // only on first chunk
	Arguments string `json:"arguments,omitempty"` // accumulating partial JSON; v1 sends complete in one chunk
}
