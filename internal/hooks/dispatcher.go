package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
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
	registry RegistryInterface
	client   *webhookClient
	// code runs code-js hook bodies; nil when code hooks are disabled, in which
	// case a code hook (e.g. one reloaded from the database) is unavailable and
	// its fail mode decides.
	code CodeRunner

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
// per-client timeout — per-hook timeouts apply via ctx). It serves
// operator-global hooks only; tenant hooks always dial through the
// private-address guard, here with no host vouched for.
func NewDispatcher(reg RegistryInterface, httpClient *http.Client) *Dispatcher {
	return NewDispatcherWithPrivateHosts(reg, httpClient, nil)
}

// NewDispatcherWithPrivateHosts is NewDispatcher plus the operator's
// hooks.private_host_allowlist: hosts (suffix-matched) a TENANT hook's
// callback may reach even though they resolve to a private address.
func NewDispatcherWithPrivateHosts(reg RegistryInterface, httpClient *http.Client, privateHostAllowlist []string) *Dispatcher {
	return &Dispatcher{
		registry: reg,
		client:   newWebhookClient(httpClient, privateHostAllowlist),
	}
}

// SetCodeRunner installs the runner for code-js hook bodies. Call it during
// boot wiring, before the server serves requests.
func (d *Dispatcher) SetCodeRunner(r CodeRunner) { d.code = r }

// Identity carries the loop-side fields the dispatcher needs to
// stamp onto the webhook payload. Filled by the loop from
// tools.RunIdentity(ctx).
type Identity struct {
	Agent   string
	UserID  string
	AgentID string
	// Tenant is the run's authoritative tenant (RunIdentity.TenantID). The
	// registry's Match uses it (RFC AF) so a tenant-scoped hook fires only on
	// its tenant's runs; an operator/global hook (Hook.Tenant=="") fires on all.
	Tenant string
	// The run the call belongs to, stamped onto every payload.
	RunID       string
	ParentRunID string
	Iteration   int
}

func (i Identity) runContext() RunContext {
	return RunContext{RunID: i.RunID, ParentRunID: i.ParentRunID, Iteration: i.Iteration}
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
	// Decisions is what each hook in the chain did, in order, for the run to
	// record. A hook that passed the call through reports nothing.
	Decisions []Decision
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
	hooks := d.registry.Match(ident.Tenant, ident.Agent, tu.Name, PhasePre)
	current := tu.Input
	var (
		allowHosts       []string
		allowHostsSeen   map[string]struct{} // dedup set; lazy-init
		grantingOwner    string
		grantingHookName string
		decisions        []Decision
	)
	for _, h := range hooks {
		// Each hook in the chain sees the running input as it stands
		// after upstream rewrites — that's the whole point of an
		// ordered chain.
		call := PreHookCall{
			Phase:      PhasePre,
			Owner:      h.Owner,
			HookName:   h.Name,
			Agent:      ident.Agent,
			UserID:     ident.UserID,
			AgentID:    ident.AgentID,
			RunContext: ident.runContext(),
			ToolCall:   ToolCall{ID: tu.ID, Name: tu.Name, Input: current},
		}
		var res PreHookResult
		if err := d.invoke(ctx, h, &call, &res); err != nil {
			// Fail-mode branch: open → pass through, closed → synthesize
			// a deny error so the loop short-circuits.
			decisions = append(decisions, Decision{Owner: h.Owner, Name: h.Name, Phase: PhasePre,
				Kind: "unavailable", FailMode: failModeOf(h), Reason: err.Error()})
			if ctx.Err() != nil {
				// The run was cancelled while the hook ran (a code hook can be
				// waiting on a person's answer). Fail-open must not run the tool
				// of a run that is already over.
				return PreOutcome{Deny: &ToolResult{IsError: true, Text: "tool_call cancelled: the run ended while hook " + h.Owner + "/" + h.Name + " was deciding"}, Decisions: decisions}
			}
			if h.FailMode == FailClosed {
				log.Printf("hooks: pre %s/%s failed (fail_mode=closed): %v", h.Owner, h.Name, err)
				return PreOutcome{Deny: &ToolResult{
					IsError: true,
					Text:    "tool_call denied: hook " + h.Owner + "/" + h.Name + " unavailable",
				}, Decisions: decisions}
			}
			log.Printf("hooks: pre %s/%s failed (fail_mode=open, passing through): %v", h.Owner, h.Name, err)
			continue
		}
		if res.Deny != nil {
			// First non-nil deny wins. Subsequent hooks in the chain
			// don't run — the synthetic result is what the model sees.
			// Any AllowHosts accumulated from prior hooks is DISCARDED
			// (we don't carry policy widenings into a denied call).
			decisions = append(decisions, Decision{Owner: h.Owner, Name: h.Name, Phase: PhasePre,
				Kind: "deny", Reason: res.Deny.Text})
			return PreOutcome{Deny: res.Deny, Decisions: decisions}
		}
		if len(res.Input) > 0 {
			current = res.Input
			decisions = append(decisions, Decision{Owner: h.Owner, Name: h.Name, Phase: PhasePre,
				Kind: "rewrite_input", UpdatedInput: res.Input})
		}
		if len(res.AllowHosts) > 0 {
			if !d.registry.IsHostWidenPermitted(h.Tenant, h.Owner) {
				// Operator never opted this (tenant, owner) in. Drop with a
				// WARN log so operators can spot un-authorised widening
				// attempts (e.g., a hook that started returning allow_hosts
				// after a code update without the corresponding yaml change, or
				// a tenant claiming an owner string permitted only for another
				// tenant). Counter exposed via Stats() for graphability.
				d.hostWidenDenied.Add(1)
				log.Printf("hooks: pre %s/%s (tenant=%q) returned allow_hosts=%v but (tenant,owner) is NOT on hooks.permit_host_widen.owners; dropping grant",
					h.Owner, h.Name, h.Tenant, res.AllowHosts)
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
		Decisions:         decisions,
	}
}

// failModeOf is the hook's effective fail mode: unset means open.
func failModeOf(h *Hook) FailMode {
	if h.FailMode == FailClosed {
		return FailClosed
	}
	return FailOpen
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

// PostOutcome is what RunPost returns to the loop.
type PostOutcome struct {
	// Result is the result the model sees, after every rewrite.
	Result ToolResult
	// AdditionalContext is what the hooks asked to add, in chain order; the
	// loop appends it to the result's text.
	AdditionalContext []string
	Decisions         []Decision
}

// RunPost invokes the Post chain for (agent, tool). Each hook sees the result
// the prior hook produced — LIFO middleware ordering, so the LAST registered
// hook runs FIRST (innermost), and the FIRST registered hook runs LAST
// (outermost).
//
// When the tool FAILED, the post_failure chain runs first, innermost to the
// post chain: a hook registered only for failures sees the failure before any
// general post hook has rewritten it.
func (d *Dispatcher) RunPost(ctx context.Context, ident Identity, tu ToolCall, original ToolResult) PostOutcome {
	chain := d.registry.Match(ident.Tenant, ident.Agent, tu.Name, PhasePost) // already reversed by registry for Post
	if original.IsError {
		chain = append(d.registry.Match(ident.Tenant, ident.Agent, tu.Name, PhasePostFailure), chain...)
	}
	out := PostOutcome{Result: original}
	for _, h := range chain {
		call := PostHookCall{
			Phase:      h.Phase,
			Owner:      h.Owner,
			HookName:   h.Name,
			Agent:      ident.Agent,
			UserID:     ident.UserID,
			AgentID:    ident.AgentID,
			RunContext: ident.runContext(),
			ToolCall:   tu,
			ToolResult: out.Result,
		}
		var res PostHookResult
		if err := d.invoke(ctx, h, &call, &res); err != nil {
			out.Decisions = append(out.Decisions, Decision{Owner: h.Owner, Name: h.Name, Phase: h.Phase,
				Kind: "unavailable", FailMode: failModeOf(h), Reason: err.Error()})
			if h.FailMode == FailClosed {
				log.Printf("hooks: %s %s/%s failed (fail_mode=closed): %v", h.Phase, h.Owner, h.Name, err)
				out.Result = ToolResult{
					IsError: true,
					Text:    "tool_result discarded: hook " + h.Owner + "/" + h.Name + " unavailable",
				}
				out.AdditionalContext = nil
				return out
			}
			log.Printf("hooks: %s %s/%s failed (fail_mode=open, passing through): %v", h.Phase, h.Owner, h.Name, err)
			continue
		}
		if res.Result != nil {
			// A hook replaces text and is_error; the structured error stays the
			// tool's, and only while the call is still a failure.
			rewritten := *res.Result
			rewritten.Error = nil
			if rewritten.IsError {
				rewritten.Error = out.Result.Error
			}
			out.Result = rewritten
			out.Decisions = append(out.Decisions, Decision{Owner: h.Owner, Name: h.Name, Phase: h.Phase, Kind: "rewrite_output"})
		}
		if ctxText := strings.TrimSpace(res.AdditionalContext); ctxText != "" {
			out.AdditionalContext = append(out.AdditionalContext, ctxText)
			out.Decisions = append(out.Decisions, Decision{Owner: h.Owner, Name: h.Name, Phase: h.Phase,
				Kind: "context", AdditionalContext: ctxText})
		}
	}
	return out
}

// invoke runs one hook: a code body in the code-js runner, otherwise a
// webhook POST under the per-hook timeout. out is *PreHookResult or
// *PostHookResult.
func (d *Dispatcher) invoke(ctx context.Context, h *Hook, body, out any) error {
	if h.IsCode() {
		return d.invokeCode(ctx, h, body, out)
	}
	hookCtx, cancel := context.WithTimeout(ctx, h.Timeout)
	defer cancel()
	return d.client.post(hookCtx, d.client.clientFor(h), h.CallbackURL, body, out)
}

// errCodeHooksDisabled is a code hook met with no runner installed.
var errCodeHooksDisabled = errors.New("code hooks are not enabled on this server")

// invokeCode runs a code body and translates its decision into the response
// shape the chain applies. The runner applies the hook's timeout to each run of
// the JavaScript itself, so no deadline is put on ctx here: that would count a
// person's answer to the hook's question against a budget meant for code.
func (d *Dispatcher) invokeCode(ctx context.Context, h *Hook, body, out any) error {
	if d.code == nil {
		return errCodeHooksDisabled
	}
	dec, err := d.code.Run(ctx, h, eventFor(h.Phase), body)
	if err != nil {
		return err
	}
	switch o := out.(type) {
	case *PreHookResult:
		res, err := dec.preResult(h)
		if err != nil {
			return err
		}
		*o = res
	case *PostHookResult:
		res, err := dec.postResult()
		if err != nil {
			return err
		}
		*o = res
	default:
		return fmt.Errorf("hooks: unexpected response type %T", out)
	}
	return nil
}
