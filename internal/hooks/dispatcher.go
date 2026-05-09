package hooks

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
)

// Dispatcher is the front door the agent loop calls into. It looks
// hooks up by (agent, tool, phase), invokes them in chain order via
// the webhook client, and returns the chain's final input/result
// after applying each hook's rewrite or short-circuit.
//
// One Dispatcher per server, shared across all runs.
type Dispatcher struct {
	registry *Registry
	client   *webhookClient
}

// NewDispatcher returns a Dispatcher backed by the given registry.
// httpClient may be nil (uses a default http.Client without a
// per-client timeout — per-hook timeouts apply via ctx).
func NewDispatcher(reg *Registry, httpClient *http.Client) *Dispatcher {
	return &Dispatcher{
		registry: reg,
		client:   newWebhookClient(httpClient),
	}
}

// Identity carries the loop-side fields the dispatcher needs to
// stamp onto the webhook payload. Filled by the loop from
// tools.RunIdentity(ctx).
type Identity struct {
	Agent   string
	UserID  string
	AgentID string
}

// PreOutcome is what RunPre returns to the loop:
//   - Input is the input the tool should actually run with. The loop
//     ALWAYS uses this (even when it's the unmodified original) — the
//     dispatcher pre-applies any rewrites in chain order.
//   - Deny is non-nil when a hook short-circuited the chain. The loop
//     MUST skip executeTool and treat Deny as the synthetic
//     tool_result.
type PreOutcome struct {
	Input json.RawMessage
	Deny  *ToolResult
}

// RunPre invokes the Pre chain for (agent, tool). Returns the
// possibly-rewritten input (or a synthetic deny result if any hook
// short-circuited).
//
// `originalInput` is the model's tool_use.input — passed in so the
// dispatcher can pass the running input forward through each hook.
func (d *Dispatcher) RunPre(ctx context.Context, ident Identity, tu ToolCall) PreOutcome {
	hooks := d.registry.Match(ident.Agent, tu.Name, PhasePre)
	current := tu.Input
	for _, h := range hooks {
		// Each hook in the chain sees the running input as it stands
		// after upstream rewrites — that's the whole point of an
		// ordered chain.
		call := PreHookCall{
			Phase:    PhasePre,
			Owner:    h.Owner,
			HookName: h.Name,
			Agent:    ident.Agent,
			UserID:   ident.UserID,
			AgentID:  ident.AgentID,
			ToolCall: ToolCall{ID: tu.ID, Name: tu.Name, Input: current},
		}
		var res PreHookResult
		if err := d.invoke(ctx, h, &call, &res); err != nil {
			// Fail-mode branch: open → pass through, closed → synthesize
			// a deny error so the loop short-circuits.
			if h.FailMode == FailClosed {
				log.Printf("hooks: pre %s/%s failed (fail_mode=closed): %v", h.Owner, h.Name, err)
				return PreOutcome{Deny: &ToolResult{
					IsError: true,
					Text:    "tool_call denied: hook " + h.Owner + "/" + h.Name + " unavailable",
				}}
			}
			log.Printf("hooks: pre %s/%s failed (fail_mode=open, passing through): %v", h.Owner, h.Name, err)
			continue
		}
		if res.Deny != nil {
			// First non-nil deny wins. Subsequent hooks in the chain
			// don't run — the synthetic result is what the model sees.
			return PreOutcome{Deny: res.Deny}
		}
		if len(res.Input) > 0 {
			current = res.Input
		}
	}
	return PreOutcome{Input: current}
}

// RunPost invokes the Post chain for (agent, tool). Each hook sees
// the result the prior hook produced — LIFO middleware ordering, so
// the LAST registered hook runs FIRST (innermost), and the FIRST
// registered hook runs LAST (outermost).
//
// Returns the rewritten result (or `original` if no hook produced a
// rewrite).
func (d *Dispatcher) RunPost(ctx context.Context, ident Identity, tu ToolCall, original ToolResult) ToolResult {
	hooks := d.registry.Match(ident.Agent, tu.Name, PhasePost) // already reversed by registry for Post
	current := original
	for _, h := range hooks {
		call := PostHookCall{
			Phase:      PhasePost,
			Owner:      h.Owner,
			HookName:   h.Name,
			Agent:      ident.Agent,
			UserID:     ident.UserID,
			AgentID:    ident.AgentID,
			ToolCall:   tu,
			ToolResult: current,
		}
		var res PostHookResult
		if err := d.invoke(ctx, h, &call, &res); err != nil {
			if h.FailMode == FailClosed {
				log.Printf("hooks: post %s/%s failed (fail_mode=closed): %v", h.Owner, h.Name, err)
				return ToolResult{
					IsError: true,
					Text:    "tool_result discarded: hook " + h.Owner + "/" + h.Name + " unavailable",
				}
			}
			log.Printf("hooks: post %s/%s failed (fail_mode=open, passing through): %v", h.Owner, h.Name, err)
			continue
		}
		if res.Result != nil {
			current = *res.Result
		}
	}
	return current
}

// invoke wraps the per-hook timeout around the webhook POST.
func (d *Dispatcher) invoke(ctx context.Context, h *Hook, body, out any) error {
	hookCtx, cancel := context.WithTimeout(ctx, h.Timeout)
	defer cancel()
	return d.client.post(hookCtx, h.CallbackURL, body, out)
}
