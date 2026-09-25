// Package hooks implements the v0.7.x tool-use hook seam: external apps
// register HTTP-webhook callbacks against (agent, tool, phase) selectors,
// and the agent loop invokes them around tool dispatch so the hook can
// rewrite the input, short-circuit with a synthetic result, or rewrite
// the post-tool result.
//
// Trust model:
//
//   - Hooks run AFTER the policy layer (tools / allowed_hosts).
//     They may narrow the call further (deny / rewrite) but cannot widen
//     past what the operator's static config permits. This is a
//     non-negotiable invariant of the seam.
//   - Hooks cannot tear down the agent run. The worst they can do is
//     short-circuit one tool call with a synthetic IsError result.
//   - Webhook callbacks include agent_id and user_id for correlation but
//     do NOT include the agent's prompt or message history.
package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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
	// PhasePostFailure runs after a tool FAILS, before the post chain, with
	// the failure's structured classification in the payload. A post hook
	// still sees failures too (it always has); this phase is for a hook that
	// only cares about them.
	PhasePostFailure Phase = "post_failure"
	// PhaseAgentStart runs once per run, after the prompt is composed and
	// before the first model call. The hook may deny the run or add context
	// to its prompt.
	PhaseAgentStart Phase = "agent_start"
	// PhaseAgentStop runs each time the model finishes an answer, before the
	// run ends (or parks). The hook may let it finish, block it (the reason is
	// sent back as a user turn and the model tries again), or hold it for a
	// person's verdict.
	PhaseAgentStop Phase = "agent_stop"
	// PhaseSubagentStart runs in the parent when it is about to start a
	// sub-agent (the Agent tool). The hook may deny the child — the parent's
	// call gets the reason — or add context to the child's prompt.
	PhaseSubagentStart Phase = "subagent_start"
	// PhaseSubagentStop runs in the parent after a sub-agent finished, before
	// its result reaches the parent. The hook may deny the result (the parent
	// gets the reason as an error, and may retry) or add context to it. Sending
	// the child back to revise is agent_stop's, on the child.
	PhaseSubagentStop Phase = "subagent_stop"
	// PhasePreCompact runs before a compaction summarizes the conversation. The
	// hook may deny it.
	PhasePreCompact Phase = "pre_compact"
	// PhasePostCompact reports a compaction that happened. Observe only.
	PhasePostCompact Phase = "post_compact"
	// PhaseRunEnd reports how a run ended, whatever the outcome. Observe only.
	PhaseRunEnd Phase = "run_end"
)

// IsObservePhase reports whether hooks of this phase only report: their
// result is ignored.
func IsObservePhase(p Phase) bool { return p == PhasePostCompact || p == PhaseRunEnd }

// IsToolPhase reports whether hooks of this phase wrap a tool call. The
// others are about the run itself, and are selected by agent only.
func IsToolPhase(p Phase) bool {
	return p == PhasePre || p == PhasePost || p == PhasePostFailure
}

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
	ID string `json:"id"`
	// Tenant is the RFC AF authoritative owning-tenant. Empty "" = an
	// operator/global hook: it fires on EVERY run regardless of tenant
	// (preserving pre-RFC-AF admin + single-tenant behaviour). A non-empty
	// tenant scopes the hook to runs in that tenant ONLY (see Match's filter and
	// the dispatcher's Identity.Tenant). It is set AUTHORITATIVELY from the
	// registering principal (a non-admin tenant operator → its own tenant;
	// admin / legacy / MCP-operator → "" global), never from a caller-supplied
	// body field — so a tenant operator can register hooks but cannot intercept
	// another tenant's tool calls.
	Tenant      string        `json:"tenant"`
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
	// Registration order is chain order within a group — earlier
	// registrations run first in the Pre chain (LIFO in the Post chain, as
	// middleware) — and a run's tenant hooks always run before the
	// operator-global ones (see Registry.Match).
	RegisteredAt time.Time `json:"registered_at"`
	// Code is a code-js hook body: JavaScript defining a top-level
	// hook(ev) function that returns the decision. A hook has exactly one
	// body — CallbackURL or Code. A code body runs in-process in the code-js
	// sandbox, with Interruption as its only tool (see CodeRunner).
	Code string `json:"code,omitempty"`
}

// IsCode reports whether the hook's body is code-js rather than a webhook.
func (h *Hook) IsCode() bool { return h.Code != "" }

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

// RunContext places a hook call in its run: which run, which run spawned it,
// and which loop iteration the tool call belongs to. Without it a hook could
// not correlate the calls of one run, tell a sub-agent's call from its
// parent's, or tell a retried call from the first one.
type RunContext struct {
	RunID       string `json:"run_id,omitempty"`
	ParentRunID string `json:"parent_run_id,omitempty"`
	Iteration   int    `json:"iteration"`
}

// PreHookCall is the JSON payload sent to a Pre webhook.
type PreHookCall struct {
	Phase    Phase  `json:"phase"`
	Owner    string `json:"owner"`
	HookName string `json:"hook_name"`
	Agent    string `json:"agent"`
	UserID   string `json:"user_id,omitempty"`
	AgentID  string `json:"agent_id,omitempty"`
	RunContext
	ToolCall ToolCall `json:"tool_call"`
}

// PreHookResult is the response a Pre webhook returns. Fields can be set
// independently:
//   - Input non-nil: the tool runs with this input instead of the model's.
//   - Deny non-nil: the tool does NOT run. The Deny payload becomes the
//     synthetic tool_result the model sees.
//   - AllowHosts non-empty: hostnames the hook approves for THIS tool
//     call (per-call scope, no server-side cache, not inherited by
//     sub-agents). Only takes effect when the hook's owner is listed
//     in the operator yaml's hooks.permit_host_widen.owners (or the
//     LOOMCYCLE_HOOKS_PERMIT_HOST_WIDEN_OWNERS env). Un-permitted
//     entries are dropped at the dispatcher with a WARN log + metric.
//     Matching semantics for each entry: a literal hostname (no
//     leading dot) is EXACT-match only ("acme.com" does not match
//     "careers.acme.com"); a leading-dot entry (".acme.com") is
//     suffix-match like the operator allowlist (matches "acme.com"
//     AND any subdomain). This is intentionally stricter than the
//     operator-list default so cautious hook authors can be surgical.
//
// Precedence: if Deny is set, AllowHosts is DROPPED (deny wins; we do
// not let a denied hook contribute hostnames to peers in the chain).
// If both Input and AllowHosts are set, both apply — the rewritten
// input executes against the widened policy.
//
// SECURITY — confused-deputy hazard: AllowHosts MUST NOT be derived
// blindly from PreHookCall.ToolCall.Input. The URL the model wants to
// fetch is untrusted. Hook authors must validate independently — e.g.,
// against the user's own preferences, a per-tenant allowlist, or a
// domain-reputation service. The audit event (host_widened) and the
// hooks_host_widen_total metric exist so operators can detect
// confused-deputy patterns post-hoc.
//
// If no field is set (or response body is empty / 204), the call
// passes through unchanged.
type PreHookResult struct {
	Input      json.RawMessage `json:"input,omitempty"`
	Deny       *ToolResult     `json:"deny,omitempty"`
	AllowHosts []string        `json:"allow_hosts,omitempty"`
}

// PostHookCall is the JSON payload sent to a Post webhook.
type PostHookCall struct {
	Phase    Phase  `json:"phase"`
	Owner    string `json:"owner"`
	HookName string `json:"hook_name"`
	Agent    string `json:"agent"`
	UserID   string `json:"user_id,omitempty"`
	AgentID  string `json:"agent_id,omitempty"`
	RunContext
	ToolCall   ToolCall   `json:"tool_call"`
	ToolResult ToolResult `json:"tool_result"`
}

// PostHookResult is the response a Post (or PostFailure) webhook returns.
// Both fields may be set:
//   - Result non-nil replaces the result the model sees.
//   - AdditionalContext is appended to the result's text — INTO the
//     tool_result rather than as a separate block, so tool results still lead
//     the next user turn and the context survives a transcript replay.
//
// Empty response / 204 = pass through unchanged.
type PostHookResult struct {
	Result            *ToolResult `json:"result,omitempty"`
	AdditionalContext string      `json:"additional_context,omitempty"`
}

// LifecycleHookCall is the payload an agent_start or agent_stop hook
// receives. Like the tool payloads it carries no prompt or history; an
// agent_stop hook gets the answer it is deciding on.
type LifecycleHookCall struct {
	Phase    Phase  `json:"phase"`
	Owner    string `json:"owner"`
	HookName string `json:"hook_name"`
	Agent    string `json:"agent"`
	UserID   string `json:"user_id,omitempty"`
	AgentID  string `json:"agent_id,omitempty"`
	RunContext
	// FinalText and StopReason are the answer an agent_stop hook decides on.
	FinalText  string `json:"final_text,omitempty"`
	StopReason string `json:"stop_reason,omitempty"`
	// StopHookActive is true when this answer is a retry after an agent_stop
	// hook blocked the previous one, and StopBlocks counts those blocks in a
	// row. A validator that would block forever can see it has already asked.
	StopHookActive bool `json:"stop_hook_active,omitempty"`
	StopBlocks     int  `json:"stop_blocks,omitempty"`
	// Subagent is the child's agent name (subagent_start / subagent_stop);
	// SubagentRunID its run, once it has one.
	Subagent      string `json:"subagent,omitempty"`
	SubagentRunID string `json:"subagent_run_id,omitempty"`
	// Status and Error say how a child (subagent_stop) or the run (run_end)
	// ended: completed, failed, cancelled or rejected.
	Status string `json:"status,omitempty"`
	Error  string `json:"error,omitempty"`
	// Trigger is what asked for a compaction (manual, auto, self);
	// ContextTokens and Window its footprint (pre_compact); BeforeTokens and
	// AfterTokens what it did (post_compact).
	Trigger       string `json:"trigger,omitempty"`
	ContextTokens int    `json:"context_tokens,omitempty"`
	Window        int    `json:"window,omitempty"`
	BeforeTokens  int    `json:"before_tokens,omitempty"`
	AfterTokens   int    `json:"after_tokens,omitempty"`
}

// LifecycleHookResult is what an agent_start or agent_stop hook returns.
// Empty response / 204 = allow.
//   - agent_start: decision "deny" (with a reason) stops the run before any
//     model call; additional_context is added to the prompt.
//   - agent_stop: decision "block" sends the reason back to the model as a
//     user turn and it answers again; "hold" holds the answer for a person's
//     verdict, as a run under review is held.
type LifecycleHookResult struct {
	Decision          string `json:"decision,omitempty"`
	Reason            string `json:"reason,omitempty"`
	AdditionalContext string `json:"additional_context,omitempty"`
}

// check refuses a result that does not apply to the phase, so a mistake in a
// hook is reported rather than read as "allow".
func (r LifecycleHookResult) check(p Phase) error {
	switch p {
	case PhaseAgentStart, PhaseSubagentStart, PhaseSubagentStop, PhasePreCompact:
		if p == PhasePreCompact && r.AdditionalContext != "" {
			return fmt.Errorf("additional_context does not apply to pre_compact; a compaction has no turn to add it to")
		}
		switch r.Decision {
		case "", "allow":
		case "deny":
			if r.AdditionalContext != "" {
				return fmt.Errorf("additional_context has nothing to go into when %s is denied", p)
			}
		default:
			return fmt.Errorf("decision %q does not apply to %s; it is \"allow\" or \"deny\"", r.Decision, p)
		}
	case PhaseAgentStop:
		if r.AdditionalContext != "" {
			return fmt.Errorf("additional_context does not apply to agent_stop; a block's reason is what the model is told")
		}
		switch r.Decision {
		case "", "allow", "hold":
		case "block":
			if strings.TrimSpace(r.Reason) == "" {
				return fmt.Errorf("a block needs a reason: it is what the model is told to fix")
			}
		default:
			return fmt.Errorf("decision %q does not apply to agent_stop; it is \"allow\", \"block\" or \"hold\"", r.Decision)
		}
	}
	return nil
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
	// Error is the failure's structured classification, when the tool gave
	// one. Carried TO a hook so it can branch on the category rather than
	// parse the text; a hook's own returned result does not set it (the
	// runtime keeps the tool's).
	Error *ToolError `json:"error,omitempty"`
}

// ToolError is the wire shape of a classified tool failure (mirrors
// tools.ErrorInfo).
type ToolError struct {
	Category    string `json:"category"`
	Retryable   bool   `json:"retryable"`
	Description string `json:"description,omitempty"`
}

// Decision is one thing a hook did to a tool call, reported so the run can
// record it. A hook that passed the call through unchanged reports nothing.
type Decision struct {
	Owner string
	Name  string
	Phase Phase
	// Kind: "deny" | "rewrite_input" | "rewrite_output" | "context" |
	// "block" | "hold" | "unavailable" (the hook failed; FailMode says what
	// that meant).
	Kind     string
	FailMode FailMode
	// Reason is shown to the run's viewer and persisted: for "unavailable" it
	// is decisionReason's short category, never a webhook's URL or response.
	Reason            string
	UpdatedInput      json.RawMessage
	AdditionalContext string
}

// CodeRunner runs a code-js hook body. It lives outside this package so the
// dispatcher does not depend on the JavaScript engine; the server installs one
// only when code hooks are enabled.
type CodeRunner interface {
	// Compile parses a code body without running it, so a broken body is
	// refused at registration rather than at its first matching call.
	Compile(src string) error
	// Run executes the hook for one call. event names what is being decided
	// ("pre_tool_use", "post_tool_use", "post_tool_use_failure"); payload is
	// the same call a webhook would receive. It returns the hook's decision,
	// or an error that the dispatcher treats like an unreachable webhook.
	Run(ctx context.Context, h *Hook, event string, payload any) (CodeDecision, error)
}

// CodeDecision is what a code-js hook returns. A pre hook may deny, rewrite
// the input or grant hosts; a post hook may replace the output or add
// context. A field that does not apply to the hook's phase is refused, so a
// mistake in a hook body is reported rather than silently ignored.
type CodeDecision struct {
	// Decision is "allow" (or empty) to let the call through, "deny" to stop
	// it (pre and agent_start), "block" or "hold" (agent_stop).
	Decision          string          `json:"decision,omitempty"`
	Reason            string          `json:"reason,omitempty"`
	UpdatedInput      json.RawMessage `json:"updated_input,omitempty"`
	UpdatedOutput     *ToolResult     `json:"updated_output,omitempty"`
	AdditionalContext string          `json:"additional_context,omitempty"`
	AllowHosts        []string        `json:"allow_hosts,omitempty"`
}

// preResult translates a pre hook's decision into the webhook response shape
// the pre chain already applies.
func (d CodeDecision) preResult(h *Hook) (PreHookResult, error) {
	if d.UpdatedOutput != nil || d.AdditionalContext != "" {
		return PreHookResult{}, fmt.Errorf("updated_output and additional_context apply to post hooks only")
	}
	switch d.Decision {
	case "", "allow":
		return PreHookResult{Input: d.UpdatedInput, AllowHosts: d.AllowHosts}, nil
	case "deny":
		text := d.Reason
		if text == "" {
			text = "tool_call denied by hook " + h.Owner + "/" + h.Name
		}
		return PreHookResult{Deny: &ToolResult{IsError: true, Text: text}}, nil
	default:
		return PreHookResult{}, fmt.Errorf("decision %q is not one of \"allow\", \"deny\"", d.Decision)
	}
}

// postResult translates a post / post_failure hook's decision.
func (d CodeDecision) postResult() (PostHookResult, error) {
	if len(d.UpdatedInput) > 0 || len(d.AllowHosts) > 0 {
		return PostHookResult{}, fmt.Errorf("updated_input and allow_hosts apply to pre hooks only")
	}
	if d.Decision != "" && d.Decision != "allow" {
		return PostHookResult{}, fmt.Errorf("decision %q does not apply after the tool ran; return updated_output to change its result", d.Decision)
	}
	return PostHookResult{Result: d.UpdatedOutput, AdditionalContext: d.AdditionalContext}, nil
}

// lifecycleResult translates an agent_start / agent_stop hook's decision.
func (d CodeDecision) lifecycleResult() (LifecycleHookResult, error) {
	if len(d.UpdatedInput) > 0 || d.UpdatedOutput != nil || len(d.AllowHosts) > 0 {
		return LifecycleHookResult{}, fmt.Errorf("updated_input, updated_output and allow_hosts apply to tool hooks only")
	}
	return LifecycleHookResult{Decision: d.Decision, Reason: d.Reason, AdditionalContext: d.AdditionalContext}, nil
}

// eventFor names the thing a hook in this phase decides on, as a code body
// sees it.
func eventFor(p Phase) string {
	switch p {
	case PhasePre:
		return "pre_tool_use"
	case PhasePostFailure:
		return "post_tool_use_failure"
	case PhaseAgentStart, PhaseAgentStop, PhaseSubagentStart, PhaseSubagentStop,
		PhasePreCompact, PhasePostCompact, PhaseRunEnd:
		return string(p)
	default:
		return "post_tool_use"
	}
}
