package http

import (
	"encoding/json"
)

// llm_gateway_types.go — v0.11.0 LLM Gateway wire shapes.
//
// The gateway exposes loomcycle's resolver + provider auth + retry
// layer as a direct LLM call surface, bypassing the agent loop.
// External consumers (n8n LoomCycleChatModel sub-node first; any
// LangChain-compatible consumer in principle) hit POST /v1/_llm/chat
// with the shapes here.
//
// The on-the-wire JSON is Anthropic-style: messages with content-block
// arrays, tool definitions with input_schema, and content blocks that
// discriminate on `type`. Same shape the agent loop's transcript
// already uses internally — keeps the substrate's single mental model
// for "an LLM turn".

// llmChatRequest is the POST /v1/_llm/chat body.
type llmChatRequest struct {
	Messages      []llmChatMessage `json:"messages"`
	Tools         []llmChatTool    `json:"tools,omitempty"`
	MaxTokens     int              `json:"max_tokens,omitempty"`
	Temperature   *float64         `json:"temperature,omitempty"`
	StopSequences []string         `json:"stop_sequences,omitempty"`
	Stream        bool             `json:"stream,omitempty"`

	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Tier     string `json:"tier,omitempty"`

	UserID     string `json:"user_id,omitempty"`
	UserTier   string `json:"user_tier,omitempty"`
	UserBearer string `json:"user_bearer,omitempty"`
}

// llmChatMessage is one turn in the conversation. Mirrors LangChain's
// `BaseMessage` shape so consumers map without reshaping.
//
// `Content` is a string when the role is "user", "system", or "tool"
// (`Content: "the message text"`). For "assistant" turns that include
// tool calls, `Content` may be empty/null and `ToolCalls` carries the
// requested calls. For "tool" turns, `ToolCallID` correlates back to
// the assistant turn's `ToolCalls[].ID`.
type llmChatMessage struct {
	Role       string            `json:"role"` // "system" | "user" | "assistant" | "tool"
	Content    string            `json:"content,omitempty"`
	ToolCalls  []llmChatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string            `json:"tool_call_id,omitempty"`
}

// llmChatToolCall is one assistant-requested tool invocation.
type llmChatToolCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// llmChatTool describes one tool the model may call.
type llmChatTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// llmChatResponse is the non-streaming response shape.
type llmChatResponse struct {
	ID         string            `json:"id"`
	RequestID  string            `json:"request_id"`
	Provider   string            `json:"provider"`
	Model      string            `json:"model"`
	Content    []llmContentBlock `json:"content"`
	StopReason string            `json:"stop_reason"`
	Usage      llmUsage          `json:"usage"`
}

// llmContentBlock is a single output content block. `Type` discriminates
// the union — "text" or "tool_use".
type llmContentBlock struct {
	Type string `json:"type"`

	// Type=="text"
	Text string `json:"text,omitempty"`

	// Type=="tool_use"
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// llmUsage is the token-accounting payload. Cache fields are populated
// only on providers that surface them (Anthropic today); other
// providers leave them zero.
type llmUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

// Streaming frame envelopes. Each frame is one SSE event whose
// `event:` line carries one of these names. The wire shape mirrors
// Anthropic's streaming events because operators already know that
// model and consumers (LangChain BaseChatModel implementations) map
// it cleanly.

// llmStreamProviderChosen — gateway-specific frame emitted before any
// content. Lets consumers observe which provider the resolver picked
// without parsing the eventual `done` frame.
type llmStreamProviderChosen struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	RequestID string `json:"request_id"`
}

// llmStreamContentBlockStart — start of a content block. `Block`
// carries the initial state (empty text for "text" blocks; tool
// metadata for "tool_use" blocks).
type llmStreamContentBlockStart struct {
	Index int             `json:"index"`
	Block llmContentBlock `json:"block"`
}

// llmStreamContentBlockDelta — incremental update to a content block.
// `Delta` is shape-discriminated on `type`: "text_delta" for "text"
// blocks, "input_json_delta" for "tool_use" blocks (partial JSON
// fragment).
type llmStreamContentBlockDelta struct {
	Index int            `json:"index"`
	Delta llmStreamDelta `json:"delta"`
}

// llmStreamDelta is the per-delta payload.
type llmStreamDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`         // for "text_delta"
	PartialJSON string `json:"partial_json,omitempty"` // for "input_json_delta"
}

// llmStreamContentBlockStop — end of a content block at `Index`.
type llmStreamContentBlockStop struct {
	Index int `json:"index"`
}

// llmStreamMessageDelta — mid-stream message-level update (stop_reason
// being set as the message wraps up; running usage counters).
type llmStreamMessageDelta struct {
	Delta llmMessageDeltaPayload `json:"delta"`
	Usage llmUsage               `json:"usage"`
}

type llmMessageDeltaPayload struct {
	StopReason string `json:"stop_reason,omitempty"`
}

// llmStreamDone — terminal success frame. Carries the same identifying
// fields as the non-streaming response so consumers can rebuild the
// response from the stream.
type llmStreamDone struct {
	ID         string   `json:"id"`
	StopReason string   `json:"stop_reason"`
	Usage      llmUsage `json:"usage"`
}

// llmStreamError — terminal failure frame. `Type` discriminates the
// error family (provider_error, request_error, internal_error);
// `Code` is a machine-readable identifier; `Message` is human text.
type llmStreamError struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}
