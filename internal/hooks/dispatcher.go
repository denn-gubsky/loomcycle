package hooks

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync/atomic"
)

// Dispatcher is the front door the agent loop calls into. It looks
// hooks up by (agent, tool, phase), invokes them in chain order via
// the webhook client, and returns the chain's final input/result
// after applying each hook's rewrite or short-circuit.
//
// One Dispatcher per server, shared across all runs.
//
// hostWidenPermitted / hostWidenDenied are atomic counters incremented
// whenever a Pre-hook's allow_hosts is honoured or dropped at
// dispatch time. Lets operators graph widening volume without
// scraping the audit-event stream. Surfaced via Stats().
type Dispatcher struct {
	registry *Registry
	client   *webhookClient

	hostWidenPermitted atomic.Int64
	hostWidenDenied    atomic.Int64
}

// DispatcherStats is a point-in-time snapshot of dispatcher counters,
// intended for operator observability endpoints. Today only the
// host-widen counters exist; future counters land here.
type DispatcherStats struct {
	HostWidenPermitted int64 // Pre-hook allow_hosts honoured (owner in permit list)
	HostWidenDenied    int64 // Pre-hook allow_hosts dropped (owner NOT in permit list)
}

// Stats returns a snapshot of the dispatcher's counters. Cheap —
// atomic loads only; safe to call from any goroutine.
func (d *Dispatcher) Stats() DispatcherStats {
	return DispatcherStats{
		HostWidenPermitted: d.hostWidenPermitted.Load(),
		HostWidenDenied:    d.hostWidenDenied.Load(),
	}
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
//   - AllowHosts carries the UNION of per-call host grants from all
//     permitted-owner Pre-hooks in the chain. The loop attaches this
//     to ctx via tools.WithExtraAllowedHosts before executing the
//     tool. Empty/nil when no hook granted anything or all granting
//     hooks had un-permitted owners (their grants are silently
//     dropped at the dispatcher, with a WARN log + metric increment).
//     Also nil when the chain ended in Deny — a denied hook does NOT
//     contribute hostnames to peers (CLAUDE.md confused-deputy
//     guidance).
//   - GrantingHookOwner and GrantingHookName name the LAST permitted
//     hook in the chain that contributed to AllowHosts. Carried so
//     the loop's audit event (host_widened) names a single
//     attribution rather than the whole chain. When multiple
//     permitted hooks each contributed, "last permitted to contribute"
//     is the attribution the audit log shows; operators can correlate
//     to the metric for the full picture.
type PreOutcome struct {
	Input             json.RawMessage
	Deny              *ToolResult
	AllowHosts        []string
	GrantingHookOwner string
	GrantingHookName  string
}

// RunPre invokes the Pre chain for (agent, tool). Returns the
// possibly-rewritten input (or a synthetic deny result if any hook
// short-circuited), plus any host-widening grants accumulated from
// permitted-owner hooks in the chain.
//
// `originalInput` is the model's tool_use.input — passed in so the
// dispatcher can pass the running input forward through each hook.
//
// AllowHosts accumulation rules:
//   - A hook contributes to the outcome's AllowHosts only when its
//     Owner is on the registry's host-widen permit list (operator
//     yaml's hooks.permit_host_widen.owners). Otherwise the field is
//     dropped with a WARN log + counter increment.
//   - Contributions are UNION'd across all permitted hooks in the
//     chain (de-duplicated, order-preserved by first-seen).
//   - Deny wins: if any hook in the chain returns a non-nil Deny,
//     RunPre returns immediately with Deny set and AllowHosts nil —
//     no permitted hook's earlier grant carries over. A denied hook
//     short-circuits the chain AND nukes any pending widening
//     (confused-deputy mitigation: don't let a hook that ends in
//     deny still influence policy via a sibling).
//   - GrantingHook{Owner,Name} are stamped to the LAST permitted
//     hook that contributed at least one host. Carried for the
//     audit event so operators see a single attribution.
func (d *Dispatcher) RunPre(ctx context.Context, ident Identity, tu ToolCall) PreOutcome {
	hooks := d.registry.Match(ident.Agent, tu.Name, PhasePre)
	current := tu.Input
	var (
		allowHosts       []string
		allowHostsSeen   map[string]struct{} // dedup set; lazy-init
		grantingOwner    string
		grantingHookName string
	)
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
			// Any AllowHosts accumulated from prior hooks is DISCARDED
			// (we don't carry policy widenings into a denied call).
			return PreOutcome{Deny: res.Deny}
		}
		if len(res.Input) > 0 {
			current = res.Input
		}
		if len(res.AllowHosts) > 0 {
			if !d.registry.IsHostWidenPermitted(h.Owner) {
				// Operator never opted this owner in. Drop with a WARN
				// log so operators can spot un-authorised widening
				// attempts (e.g., a hook that started returning
				// allow_hosts after a code update without the
				// corresponding yaml change). Counter exposed via
				// Stats() for graphability.
				d.hostWidenDenied.Add(1)
				log.Printf("hooks: pre %s/%s returned allow_hosts=%v but owner is NOT on hooks.permit_host_widen.owners; dropping grant",
					h.Owner, h.Name, res.AllowHosts)
				continue
			}
			// Permitted. Union into the accumulator (dedup on
			// case-insensitive hostname — DNS is case-insensitive and
			// the operator allowlist normalizes the same way).
			if allowHostsSeen == nil {
				allowHostsSeen = make(map[string]struct{})
			}
			for _, host := range res.AllowHosts {
				key := normaliseHost(host)
				if key == "" {
					continue
				}
				if _, dup := allowHostsSeen[key]; dup {
					continue
				}
				allowHostsSeen[key] = struct{}{}
				allowHosts = append(allowHosts, key)
			}
			d.hostWidenPermitted.Add(1)
			grantingOwner = h.Owner
			grantingHookName = h.Name
		}
	}
	return PreOutcome{
		Input:             current,
		AllowHosts:        allowHosts,
		GrantingHookOwner: grantingOwner,
		GrantingHookName:  grantingHookName,
	}
}

// normaliseHost lower-cases the host entry and trims surrounding
// whitespace. Preserves a single leading dot (suffix-match opt-in).
// Empty / whitespace-only inputs map to "" so the caller drops them.
func normaliseHost(h string) string {
	// Trim spaces but NOT the leading dot — the dot is semantic.
	for len(h) > 0 && (h[0] == ' ' || h[0] == '\t') {
		h = h[1:]
	}
	for len(h) > 0 && (h[len(h)-1] == ' ' || h[len(h)-1] == '\t') {
		h = h[:len(h)-1]
	}
	if h == "" || h == "." {
		return ""
	}
	// ASCII lower-case (hostnames are ASCII for the alphanum range;
	// IDNA was already resolved on the wire side before we see it).
	out := make([]byte, len(h))
	for i := 0; i < len(h); i++ {
		c := h[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
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
