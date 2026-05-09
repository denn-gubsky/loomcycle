// Package hooks implements the v0.7.x tool-use hook seam: external apps
// register HTTP-webhook callbacks against (agent, tool, phase) selectors,
// and the agent loop invokes them around tool dispatch so the hook can
// rewrite the input, short-circuit with a synthetic result, or rewrite
// the post-tool result.
//
// Trust model:
//
//   - Hooks run AFTER the policy layer (allowed_tools / allowed_hosts).
//     They may narrow the call further (deny / rewrite) but cannot widen
//     past what the operator's static config permits. This is a
//     non-negotiable invariant of the seam.
//   - Hooks cannot tear down the agent run. The worst they can do is
//     short-circuit one tool call with a synthetic IsError result.
//   - Webhook callbacks include agent_id and user_id for correlation but
//     do NOT include the agent's prompt or message history.
package hooks

import (
	"encoding/json"
	"time"
)

// Phase discriminates pre- vs post-tool-use hooks.
type Phase string

const (
	// PhasePre runs before the tool dispatcher. The hook can rewrite
	// the input the tool sees or short-circuit the call with a synthetic
	// result that the model receives in lieu of running the tool.
	PhasePre Phase = "pre"
	// PhasePost runs after the tool dispatcher. The hook receives the
	// real tool result and can rewrite it before the loop emits the
	// EventToolResult and appends the user-turn content block.
	PhasePost Phase = "post"
)

// FailMode controls how the dispatcher treats webhook errors and timeouts.
type FailMode string

const (
	// FailOpen: webhook timeout / 5xx / network error → original input
	// or result passes through unchanged. Default. Right for
	// telemetry-shaped hooks where the hook should never block tool
	// dispatch when the registering app is down.
	FailOpen FailMode = "open"
	// FailClosed: webhook timeout / 5xx / network error → tool fails
	// with IsError=true. Right for security-shaped hooks (injection
	// scanners) where a down hook would let bypassed payloads through.
	FailClosed FailMode = "closed"
)

// Hook is one registered webhook. The (Owner, Name) tuple is the identity:
// re-registering the same (Owner, Name) replaces the prior registration so
// app restarts can't cascade duplicate hooks. ID is loomcycle-assigned and
// used by the DELETE endpoint.
//
// Filtering: a hook fires when its Agents glob list matches the running
// agent's name AND its Tools glob list matches the dispatched tool's name.
// Empty/nil list means "match all" (equivalent to ["*"]). Glob syntax
// is exact match or trailing-* prefix glob (e.g. "mcp__jobs__*"). No
// regex, no middle wildcards — the model is intentionally simple.
type Hook struct {
	ID          string        `json:"id"`
	Owner       string        `json:"owner"` // app UID; (Owner, Name) is identity
	Name        string        `json:"name"`
	Phase       Phase         `json:"phase"`
	Agents      []string      `json:"agents"` // exact or "prefix*"; empty = ["*"]
	Tools       []string      `json:"tools"`  // exact or "prefix*"; empty = ["*"]
	CallbackURL string        `json:"callback_url"`
	FailMode    FailMode      `json:"fail_mode"` // "open" (default) | "closed"
	TimeoutMs   int           `json:"timeout_ms"`
	Timeout     time.Duration `json:"-"` // resolved at registration time
	// RegisteredAt is the wall-clock instant the registration landed.
	// Determines chain order across owners — earlier registrations run
	// first in the Pre chain (LIFO in the Post chain, as middleware).
	RegisteredAt time.Time `json:"registered_at"`
}

// Matches returns true when this hook's selector matches the given
// (agent, tool, phase). Empty selector lists match anything.
func (h *Hook) Matches(agent, tool string, phase Phase) bool {
	if h.Phase != phase {
		return false
	}
	if !globsMatch(h.Agents, agent) {
		return false
	}
	if !globsMatch(h.Tools, tool) {
		return false
	}
	return true
}

// PreHookCall is the JSON payload sent to a Pre webhook.
type PreHookCall struct {
	Phase    Phase    `json:"phase"`
	Owner    string   `json:"owner"`
	HookName string   `json:"hook_name"`
	Agent    string   `json:"agent"`
	UserID   string   `json:"user_id,omitempty"`
	AgentID  string   `json:"agent_id,omitempty"`
	ToolCall ToolCall `json:"tool_call"`
}

// PreHookResult is the response a Pre webhook returns. Either field can
// be set independently:
//   - Input non-nil: the tool runs with this input instead of the model's.
//   - Deny non-nil: the tool does NOT run. The Deny payload becomes the
//     synthetic tool_result the model sees.
//
// If both are set, Deny wins (short-circuit takes precedence). If neither
// is set (or response body is empty / 204), the call passes through
// unchanged.
type PreHookResult struct {
	Input json.RawMessage `json:"input,omitempty"`
	Deny  *ToolResult     `json:"deny,omitempty"`
}

// PostHookCall is the JSON payload sent to a Post webhook.
type PostHookCall struct {
	Phase      Phase      `json:"phase"`
	Owner      string     `json:"owner"`
	HookName   string     `json:"hook_name"`
	Agent      string     `json:"agent"`
	UserID     string     `json:"user_id,omitempty"`
	AgentID    string     `json:"agent_id,omitempty"`
	ToolCall   ToolCall   `json:"tool_call"`
	ToolResult ToolResult `json:"tool_result"`
}

// PostHookResult is the response a Post webhook returns. If Result is
// nil (response empty / 204), the result passes through unchanged.
type PostHookResult struct {
	Result *ToolResult `json:"result,omitempty"`
}

// ToolCall is the wire shape for a tool invocation in hook payloads.
// Mirrors providers.ToolUse but stays in this package to avoid a
// circular import.
type ToolCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// ToolResult is the wire shape for a tool result in hook payloads.
// Mirrors tools.Result but stays in this package for the same reason.
type ToolResult struct {
	Text    string `json:"text"`
	IsError bool   `json:"is_error,omitempty"`
}
