package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// SubAgentRunner runs a named sub-agent with a given user prompt and
// returns the sub-agent's final assistant text. Implementations are
// expected to:
//
//   - Reject unknown agent names (operator-controlled allowlist:
//     cfg.Agents).
//   - Apply the sub-agent's own AllowedTools as the security floor; the
//     parent's tool set does NOT widen the child's. Parent and child are
//     both operator-vetted definitions, so each agent's declared
//     allow-list is authoritative for itself.
//   - When defID is non-empty, resolve the sub-agent against the
//     agent_defs row at that id (v0.8.5 substrate). The row's Name MUST
//     match the requested name — cross-name pinning is refused. The
//     def's mutable fields (system_prompt, allowed_tools, model, tier,
//     etc.) override the static cfg.Agents entry for this sub-run. The
//     pinned defID is also persisted on the sub-run row so evaluations
//     denormalise correctly.
//   - Persist the sub-agent's run as a separate session in the Store so
//     transcripts are replayable independently of the parent.
//   - Carry the parent's context.Context (so cancellation, deadlines,
//     and the loomcycle agent-depth value all propagate).
type SubAgentRunner func(ctx context.Context, name string, prompt string, defID string) (output string, err error)

// MaxAgentDepth caps recursion: a top-level run is depth 0, the agents
// it spawns are depth 1, etc. The cap is a safety rail against runaway
// self-spawning prompt loops; cv-batch-adapter spawning cv-adapter is
// depth 1 today. Hardcoded for v0.4.0; lifted to config in a later
// release if a real workflow demands deeper.
const MaxAgentDepth = 3

type ctxKeyAgentDepth struct{}

// IncrementAgentDepth bumps the depth counter on ctx. The Agent tool
// applies this before invoking the SubAgentRunner so a recursive child
// inherits a higher depth than the parent.
func IncrementAgentDepth(ctx context.Context) context.Context {
	d, _ := ctx.Value(ctxKeyAgentDepth{}).(int)
	return context.WithValue(ctx, ctxKeyAgentDepth{}, d+1)
}

// AgentDepth reports the current recursion depth (0 at the top level).
// Exposed so tests and the loop can assert depth invariants without
// reaching into the unexported context key.
func AgentDepth(ctx context.Context) int {
	d, _ := ctx.Value(ctxKeyAgentDepth{}).(int)
	return d
}

// AgentTool is the built-in `Agent` tool that spawns a named sub-agent
// in a fresh session. The model invokes it with {name, prompt}; the
// tool returns the sub-agent's final assistant text as a tool_result.
//
// Wire shape (Anthropic-compatible-ish — we don't expose Anthropic's
// own sub-agents API; this is loomcycle's own Tool with the same
// ergonomics):
//
//	{
//	  "name":   "cv-adapter",          // a key in cfg.Agents
//	  "prompt": "Generate CV for ..."  // user-message body the sub sees
//	}
//
// The sub-agent's `allowed_tools` (from its YAML AgentDef) is the sole
// authority on what the sub can use — the parent's allow-list does not
// widen or narrow it. This matches the trust model: each agent
// definition is operator-curated and self-describing.
//
// The parent observes only the tool_call (with input) and the
// tool_result (with the sub's final text). The sub's intermediate
// events go to the sub's own persisted transcript, retrievable via
// GET /v1/sessions/{sub-session-id}/transcript.
type AgentTool struct {
	// Run is the closure provided by the runtime that knows how to
	// look up sub-agent definitions, build the tool dispatcher, and
	// drive a fresh loop. Constructed by the HTTP server at boot.
	Run SubAgentRunner
}

// agentInput is the JSON shape the model sends.
type agentInput struct {
	Name   string `json:"name"`
	Prompt string `json:"prompt"`
	// DefID is optional (v0.8.5). When set, the sub-agent runs against
	// this specific agent_defs row instead of the currently-active
	// pointer / static cfg.Agents fallback. The row's name must match
	// `Name` — cross-name def pinning is refused. Empty = standard
	// active-or-static resolution.
	DefID string `json:"def_id,omitempty"`
}

// agentInputSchema is the JSON Schema the model sees. name + prompt are
// required; def_id is optional. additionalProperties:false enforces the
// exact shape (operator-vetted parents can't accidentally smuggle in
// undocumented fields).
const agentInputSchema = `{
  "type": "object",
  "properties": {
    "name":   {"type": "string", "description": "Sub-agent name. Must match a key in the loomcycle.yaml agents map."},
    "prompt": {"type": "string", "description": "User-message body the sub-agent sees. Treat this as the task description; do not include auth tokens (the sub-agent gets its own auth context)."},
    "def_id": {"type": "string", "description": "Optional. Pin this sub-run to a specific agent_defs row id (returned by AgentDef.create or AgentDef.fork). The row's name must match the 'name' field. Empty = use the currently-active version (or static cfg.Agents)."}
  },
  "required": ["name", "prompt"],
  "additionalProperties": false
}`

const agentDescription = `Spawn a named sub-agent in a fresh session and return its final output. ` +
	`The sub-agent has its own tool allowlist (from loomcycle.yaml); your tool set does not transfer. ` +
	`Use this when the work is well-described by another agent's role (e.g. coordinator agents that fan out to specialists). ` +
	`The sub-agent's transcript is persisted separately and can be inspected via the API.`

// Name implements tools.Tool.
func (a *AgentTool) Name() string { return "Agent" }

// Description implements tools.Tool.
func (a *AgentTool) Description() string { return agentDescription }

// InputSchema implements tools.Tool.
func (a *AgentTool) InputSchema() json.RawMessage { return json.RawMessage(agentInputSchema) }

// Execute implements tools.Tool. Validates the input, enforces the
// recursion depth cap, and delegates to the SubAgentRunner. Errors are
// surfaced as IsError tool_result so the model can self-correct rather
// than the run being torn down.
func (a *AgentTool) Execute(ctx context.Context, input json.RawMessage) (tools.Result, error) {
	if a.Run == nil {
		return tools.Result{IsError: true, Text: "Agent tool not wired to a sub-agent runner (operator misconfiguration)"}, nil
	}

	var in agentInput
	if err := json.Unmarshal(input, &in); err != nil {
		return tools.Result{IsError: true, Text: fmt.Sprintf("invalid input JSON: %s", err)}, nil
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return tools.Result{IsError: true, Text: "missing required field: name"}, nil
	}
	if in.Prompt == "" {
		return tools.Result{IsError: true, Text: "missing required field: prompt"}, nil
	}

	// Recursion guard. Counter starts at 0 for the top-level run; a
	// sub-agent invocation runs at depth+1. Refuse if that would push
	// past MaxAgentDepth.
	if AgentDepth(ctx) >= MaxAgentDepth {
		return tools.Result{
			IsError: true,
			Text: fmt.Sprintf(
				"max sub-agent recursion depth (%d) reached at agent %q; refusing to spawn deeper",
				MaxAgentDepth, in.Name,
			),
		}, nil
	}
	subCtx := IncrementAgentDepth(ctx)

	output, err := a.Run(subCtx, in.Name, in.Prompt, in.DefID)
	if err != nil {
		// Surface as an error tool_result. The parent can decide whether
		// to retry, fall back, or give up. We don't tear down the parent
		// run — the sub failure is treated as a tool error, same shape
		// as a 4xx from HTTP or an MCP server returning is_error.
		return tools.Result{IsError: true, Text: err.Error()}, nil
	}
	if output == "" {
		// A sub-agent that ended_turn with no text is rare but possible
		// (e.g. it only made tool calls and stopped). Surface a hint
		// rather than an empty result so the parent's model has
		// something concrete to read.
		return tools.Result{Text: fmt.Sprintf("(sub-agent %q completed with no final text)", in.Name)}, nil
	}
	return tools.Result{Text: output}, nil
}

// errAgentBadInput is reserved for future structured errors. Currently
// we surface every input problem as an IsError tool_result, but a
// caller that wants to break out of the loop entirely (rather than let
// the model self-correct) can return this from a SubAgentRunner.
var errAgentBadInput = errors.New("agent: bad input")

var _ tools.Tool = (*AgentTool)(nil)
