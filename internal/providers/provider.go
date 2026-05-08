// Package providers defines the LLM provider abstraction.
//
// A Provider talks one provider's HTTP API and streams a unified Event channel
// back. The agent loop in internal/loop is provider-agnostic; everything
// provider-specific (auth, request shape, SSE framing, cache_control placement)
// lives behind this interface.
package providers

import (
	"context"
	"encoding/json"
)

// Provider is one LLM endpoint. Implementations are stateless across calls;
// per-call state lives in Request and the returned channel.
type Provider interface {
	ID() string
	Capabilities() Capabilities
	Call(ctx context.Context, req Request) (<-chan Event, error)

	// Probe is a lightweight reachability + auth check. Returns nil
	// iff the provider responds successfully (auth valid, network
	// reachable). Used by the resolver's startup probe and periodic
	// re-probe loop. Implementations should hit a cheap endpoint
	// (typically GET /v1/models or /api/tags), respect the passed
	// context's deadline, and return without retry — the caller
	// owns retry/backoff policy.
	//
	// Probe is allowed to share its work with ListModels: drivers
	// commonly implement Probe by calling ListModels and treating a
	// non-empty result as "healthy" (the network round-trip
	// already proves reachability + auth). The interface keeps
	// them separate so callers that only need health (no model
	// list) don't pay the marshalling cost.
	Probe(ctx context.Context) error

	// ListModels returns the wire aliases the provider currently
	// serves. Used by the resolver to populate the per-model
	// availability matrix (Listed flag in ModelStatus). The exact
	// format depends on the provider's models endpoint:
	//   - Anthropic / OpenAI / DeepSeek: /v1/models response data[].id
	//   - Ollama: /api/tags response models[].name
	//
	// Returns an empty slice (not nil) when the endpoint succeeds
	// with zero models — that's a different signal than "probe
	// failed" and the resolver treats it as "provider reachable
	// but every model offline".
	ListModels(ctx context.Context) ([]string, error)
}

// Capabilities tells the loop what the provider can and can't do, so the loop
// can degrade gracefully instead of sending unsupported fields.
type Capabilities struct {
	NativePromptCache bool // Anthropic cache_control
	ParallelToolCalls bool
	Streaming         bool
	MaxContextTokens  int
	SupportsThinking  bool
}

// Request is one round-trip to the provider. The loop builds a fresh Request
// for each iteration, appending the previous tool_result(s) to Messages.
type Request struct {
	Model       string         `json:"model"`
	System      []ContentBlock `json:"system,omitempty"`
	Messages    []Message      `json:"messages"`
	Tools       []ToolSpec     `json:"tools,omitempty"`
	MaxTokens   int            `json:"max_tokens,omitempty"`
	Temperature *float64       `json:"temperature,omitempty"`
	Stream      bool           `json:"stream"`

	// Effort is the reasoning-effort hint: "low" / "medium" / "high"
	// or empty (= no hint, driver default). Drivers translate it to
	// their native parameter where supported (Anthropic
	// thinking.budget_tokens; OpenAI reasoning_effort; DeepSeek V4
	// thinking-mode toggle), silently ignored on models without a
	// reasoning surface (haiku-4-5, gpt-5.4-mini, etc.). The
	// translation lands in PR 3 of the resolve-matrix series; PR 1
	// adds the field but drivers ignore it.
	Effort string `json:"-"`

	// OnEvent, when set, is called for events that occur BEFORE the
	// response channel exists — most importantly, EventRetry frames
	// fired during a 429 retry sleep. Optional; the loop populates this
	// from RunOptions.OnEvent so adapter consumers see retry telemetry
	// live on the same SSE stream as the main response. Marshalling
	// callers should ignore (json:"-").
	//
	// Callback contract: the driver invokes OnEvent synchronously from
	// inside Call() (before the response channel exists). The callback
	// MUST NOT block — the driver is mid-retry-sleep and a slow callback
	// extends the rate-limit wait. SSE writes are fine; do not perform
	// network IO from here.
	OnEvent func(Event) `json:"-"`
}

// Message is one turn in the conversation.
type Message struct {
	Role    string         `json:"role"` // "user" | "assistant"
	Content []ContentBlock `json:"content"`
}

// ContentBlock is one piece of message content. Type discriminates the union.
//
//   - "text"        : plain text. Text field set.
//   - "tool_use"    : assistant requests a tool call. ToolUseID, ToolName, ToolInput set.
//   - "tool_result" : user-side result of a previous tool_use. ToolUseID, Text or ToolResult set.
//
// Cacheable is an Anthropic-specific hint: when true and the provider's
// Capabilities().NativePromptCache is true, the driver places a cache_control
// breakpoint at the end of this block.
type ContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Cacheable bool            `json:"-"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	ToolName  string          `json:"name,omitempty"`
	ToolInput json.RawMessage `json:"input,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// ToolSpec describes one tool to the model.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// EventType discriminates the streamed Event union. The loop emits these on
// the channel returned by Call(); the HTTP layer forwards them as SSE.
type EventType string

const (
	EventStarted    EventType = "started"
	EventText       EventType = "text"
	EventToolCall   EventType = "tool_call"
	EventToolResult EventType = "tool_result"
	EventUsage      EventType = "usage"
	EventDone       EventType = "done"
	EventError      EventType = "error"
	// EventRetry signals that a provider returned 429 and the driver is
	// about to sleep before retrying. Surfaced live during the retry sleep
	// (not after) so adapter consumers can show "waiting on rate limit"
	// to end users. The retry itself is invisible to the agent loop —
	// EventRetry is purely informational.
	EventRetry EventType = "retry"
)

// Event is one streamed datum from a provider call (or, after the loop layer
// has wrapped it, from the loop itself).
type Event struct {
	Type    EventType `json:"type"`
	Text    string    `json:"text,omitempty"`
	ToolUse *ToolUse  `json:"tool_use,omitempty"`
	Usage   *Usage    `json:"usage,omitempty"`
	Error   string    `json:"error,omitempty"`
	// IsError flags a tool_result whose execution failed. Surviving the
	// persist+replay round-trip matters because a continuation that lost
	// the flag would re-feed the model a successful-looking result.
	IsError bool `json:"is_error,omitempty"`
	// Retry carries the retry telemetry on EventRetry. Nil otherwise.
	Retry *RetryInfo `json:"retry,omitempty"`

	// StopReason is set on the final assistant Event of a provider call:
	// "end_turn" | "tool_use" | "max_tokens" | "stop_sequence".
	StopReason string `json:"stop_reason,omitempty"`
}

// RetryInfo accompanies an EventRetry. Each field is set every time.
type RetryInfo struct {
	Provider string `json:"provider"`
	Attempt  int    `json:"attempt"`
	WaitMs   int64  `json:"wait_ms"`
	// Reason is one of the RetryReason* constants below. It's a
	// stable wire string — adapters string-match against it.
	Reason string `json:"reason"`
}

// RetryReason* are the values of RetryInfo.Reason. They're declared on
// the providers package (not on ratelimit) because they're part of the
// wire contract — adapters and SSE consumers depend on these strings.
// Do not change without bumping a major version.
const (
	RetryReasonHeader   = "retry-after header"
	RetryReasonSchedule = "exponential backoff"
)

// ToolUse is the model's request to invoke a tool.
type ToolUse struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// Usage is one provider call's token accounting.
type Usage struct {
	InputTokens         int    `json:"input_tokens"`
	OutputTokens        int    `json:"output_tokens"`
	CacheCreationTokens int    `json:"cache_creation_input_tokens,omitempty"`
	CacheReadTokens     int    `json:"cache_read_input_tokens,omitempty"`
	Model               string `json:"model,omitempty"`
}
