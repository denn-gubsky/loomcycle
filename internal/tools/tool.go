// Package tools defines the Tool interface and the dispatcher that routes
// tool_use calls from the model to a built-in or MCP-backed implementation.
package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// Tool is one tool the agent can invoke. The Name is what the model sees and
// what allowlists are matched against. The InputSchema is JSON Schema; the
// dispatcher passes the raw model-generated input straight through.
type Tool interface {
	Name() string
	Description() string
	InputSchema() json.RawMessage
	Execute(ctx context.Context, input json.RawMessage) (Result, error)
}

// Result is the output of one tool invocation. Text is the human-readable
// payload the model will see in the next tool_result block. IsError flags a
// failed execution (the model should self-correct, not surface to the user).
type Result struct {
	Text    string
	IsError bool
}

// Spec converts a Tool to the providers.ToolSpec the model receives.
func Spec(t Tool) providers.ToolSpec {
	return providers.ToolSpec{
		Name:        t.Name(),
		Description: t.Description(),
		InputSchema: t.InputSchema(),
	}
}

// Dispatcher resolves tool names to Tool implementations and invokes them.
// A new Dispatcher is built per run with the run's allowed-tools list so
// off-policy calls fail fast.
type Dispatcher struct {
	tools map[string]Tool
}

// NewDispatcher builds a dispatcher from the given tools.
func NewDispatcher(tools []Tool) *Dispatcher {
	m := make(map[string]Tool, len(tools))
	for _, t := range tools {
		m[t.Name()] = t
	}
	return &Dispatcher{tools: m}
}

// Specs returns the providers.ToolSpec slice for all registered tools, in the
// order they were passed to NewDispatcher (map iteration would be non-deterministic).
func (d *Dispatcher) Specs(order []Tool) []providers.ToolSpec {
	out := make([]providers.ToolSpec, 0, len(order))
	for _, t := range order {
		if _, ok := d.tools[t.Name()]; ok {
			out = append(out, Spec(t))
		}
	}
	return out
}

// ctxKeyAgentTools is the context key under which the runtime stores
// the calling agent's effective allowed_tools list (after agent + caller
// narrowing). Tools that need to apply secondary subset checks (like
// the built-in Skill tool, which validates skill `allowed-tools` ⊆
// agent `allowed_tools` at call time) read it via AgentTools.
type ctxKeyAgentTools struct{}

// WithAgentTools attaches the agent's effective tool names to ctx. The
// HTTP server calls this once per run before invoking the loop so any
// tool that resolves dynamic permissions has the same view of "what
// the agent can use."
func WithAgentTools(ctx context.Context, names []string) context.Context {
	return context.WithValue(ctx, ctxKeyAgentTools{}, names)
}

// AgentTools returns the agent's effective tool names from ctx, or
// nil if not attached. Returning nil from a tool that requires this
// list should cause the tool to refuse with a clear "misconfigured
// runtime" message.
func AgentTools(ctx context.Context) []string {
	v, _ := ctx.Value(ctxKeyAgentTools{}).([]string)
	return v
}

// ctxKeyHostPolicy is the context key for the run's effective HTTP
// host narrowing policy — what the CALLER (top-level: HTTP request
// body; sub-agent: inherited from the parent's ctx) asked for in
// allowed_hosts / web_search_filter. Sub-agents read this via
// HostPolicy and re-apply the parent's narrowing to their own tools,
// so a parent that worked against ["localhost"] under
// CALLER_AUTHORITATIVE doesn't spawn children that mysteriously fall
// back to the operator's static allowlist (which typically doesn't
// include localhost). See server.runSubAgent.
type ctxKeyHostPolicy struct{}

// HostPolicyValue captures the caller-authoritative HTTP host policy.
//
// HasList distinguishes "caller didn't supply a list at all" (false:
// fall back to operator's static allowlist) from "caller supplied a
// list, possibly empty" (true: the list IS the policy, deny-all if
// empty). The two cases are different in CALLER_AUTHORITATIVE mode:
// nil → operator's static list; empty → deny everything.
type HostPolicyValue struct {
	AllowedHosts    []string
	HasList         bool
	WebSearchFilter string
}

// WithHostPolicy attaches the caller's host narrowing policy to ctx.
// runRequest sets this once for top-level runs; sub-agents inherit it
// via the ctx chain (Agent tool's Execute → runSubAgent passes the
// parent's ctx through).
func WithHostPolicy(ctx context.Context, p HostPolicyValue) context.Context {
	return context.WithValue(ctx, ctxKeyHostPolicy{}, p)
}

// HostPolicy returns the caller's host narrowing policy from ctx, or
// the zero value (HasList=false, no narrowing) if not attached.
func HostPolicy(ctx context.Context) HostPolicyValue {
	v, _ := ctx.Value(ctxKeyHostPolicy{}).(HostPolicyValue)
	return v
}

// ctxKeyRunIdentity is the context key under which the runtime
// stashes the current run's user_id and agent_id (v0.4 tracking
// fields). Sub-agents read these via RunIdentity to inherit the
// parent's user_id and to know whose agent_id is their parent.
type ctxKeyRunIdentity struct{}

// RunIdentityValue is the shape stored in ctx by WithRunIdentity. The
// "Value" suffix avoids a naming collision with store.RunIdentity
// (which is the persistence-layer struct with more fields).
type RunIdentityValue struct {
	UserID  string
	AgentID string
}

// WithRunIdentity attaches the current run's identity to ctx. The
// HTTP server calls this once per run before invoking the loop so the
// AgentTool's SubAgentRunner can read it back via RunIdentity and
// thread userID/parentAgentID through to the new sub-agent's session
// + run records.
func WithRunIdentity(ctx context.Context, ident RunIdentityValue) context.Context {
	return context.WithValue(ctx, ctxKeyRunIdentity{}, ident)
}

// RunIdentity returns the current run's identity from ctx, or zero
// value if not attached. The HTTP server's runSubAgent uses the
// AgentID as the new sub-run's parent_agent_id and the UserID for
// inheritance into the sub-agent's session.
func RunIdentity(ctx context.Context) RunIdentityValue {
	v, _ := ctx.Value(ctxKeyRunIdentity{}).(RunIdentityValue)
	return v
}

// Execute looks up the named tool and runs it. Unknown tool names return an
// error result (the model can self-correct) rather than a hard error.
func (d *Dispatcher) Execute(ctx context.Context, name string, input json.RawMessage) Result {
	t, ok := d.tools[name]
	if !ok {
		return Result{Text: fmt.Sprintf("tool not found: %s", name), IsError: true}
	}
	res, err := t.Execute(ctx, input)
	if err != nil {
		return Result{Text: err.Error(), IsError: true}
	}
	return res
}
