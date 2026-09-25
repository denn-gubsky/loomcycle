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
	"time"
)

// Dispatcher is the front door the agent loop calls into. It fires the hooks of
// the run a call belongs to — the Set on the call's ctx (see WithSet) — in chain
// order via the webhook client or the code runner, and returns the chain's
// final input/result after applying each hook's rewrite or short-circuit.
//
// One Dispatcher per server, shared across all runs; the hooks are the run's.
//
// hostWidenPermitted / hostWidenDenied are atomic counters incremented
// whenever a Pre-hook's allow_hosts is honoured or dropped at
// dispatch time. Lets operators graph widening volume without
// scraping the audit-event stream. Surfaced via Stats().
type Dispatcher struct {
	// base is fired when ctx carries no Set. The server leaves it nil — a run
	// fires only what it carries; tests set a fixed chain.
	base   *Set
	client *webhookClient
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
	HostWidenPermitted int64 // Pre-hook allow_hosts honoured (the hook may widen)
	HostWidenDenied    int64 // Pre-hook allow_hosts dropped (it may not)
}

// Stats returns a snapshot of the dispatcher's counters. Cheap —
// atomic loads only; safe to call from any goroutine.
func (d *Dispatcher) Stats() DispatcherStats {
	return DispatcherStats{
		HostWidenPermitted: d.hostWidenPermitted.Load(),
		HostWidenDenied:    d.hostWidenDenied.Load(),
	}
}

// NewDispatcher returns a Dispatcher whose calls fire base when their ctx
// carries no Set (nil: fire nothing). httpClient may be nil (uses a default
// http.Client without a per-client timeout — per-hook timeouts apply via ctx).
// A tenant's hooks always dial through the private-address guard, here with no
// host vouched for.
func NewDispatcher(base *Set, httpClient *http.Client) *Dispatcher {
	return NewDispatcherWithPrivateHosts(base, httpClient, nil)
}

// NewDispatcherWithPrivateHosts is NewDispatcher plus the operator's
// hooks.private_host_allowlist: hosts (suffix-matched) a TENANT hook's
// callback may reach even though they resolve to a private address.
func NewDispatcherWithPrivateHosts(base *Set, httpClient *http.Client, privateHostAllowlist []string) *Dispatcher {
	return &Dispatcher{
		base:   base,
		client: newWebhookClient(httpClient, privateHostAllowlist),
	}
}

// match is the chain for one call: the run's Set on ctx, else the base.
func (d *Dispatcher) match(ctx context.Context, ident Identity, tool string, phase Phase) []*Hook {
	if s := SetFrom(ctx); s != nil {
		return s.Match(ident.Agent, tool, phase)
	}
	return d.base.Match(ident.Agent, tool, phase)
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
	// Tenant is the run's authoritative tenant (RunIdentity.TenantID).
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
//   - A hook contributes to the outcome's AllowHosts only when it may
//     widen (Hook.WidenPermitted: resolved with the run's hooks, from
//     the operator's hooks.permit_host_widen list and the definition
//     it came from). Otherwise the field is dropped with a WARN log +
//     counter increment.
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
	hooks := d.match(ctx, ident, tu.Name, PhasePre)
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
				Kind: "unavailable", FailMode: failModeOf(h), Reason: decisionReason(err)})
			if ctx.Err() != nil {
				log.Printf("hooks: pre %s/%s failed (run ended): %v", h.Owner, h.Name, err)
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
			if !h.WidenPermitted {
				// Not granted when the run's hooks were resolved: the operator's
				// permit list does not name it, or it did not come from an
				// operator-authored definition. Drop with a WARN log so an
				// operator can spot an un-authorised widening attempt (a hook
				// that started returning allow_hosts without the matching yaml
				// change). Counter exposed via Stats() for graphability.
				d.hostWidenDenied.Add(1)
				log.Printf("hooks: pre %s/%s (tenant=%q) returned allow_hosts=%v but may not widen hosts (not on hooks.permit_host_widen, or not from an operator-authored definition); dropping grant",
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
// post chain: a hook listed only for failures sees the failure before any
// general post hook has rewritten it.
func (d *Dispatcher) RunPost(ctx context.Context, ident Identity, tu ToolCall, original ToolResult) PostOutcome {
	chain := d.match(ctx, ident, tu.Name, PhasePost) // already reversed for Post
	if original.IsError {
		failure := d.match(ctx, ident, tu.Name, PhasePostFailure)
		chain = append(append(make([]*Hook, 0, len(failure)+len(chain)), failure...), chain...)
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
				Kind: "unavailable", FailMode: failModeOf(h), Reason: decisionReason(err)})
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
	case *LifecycleHookResult:
		res, err := dec.lifecycleResult()
		if err != nil {
			return err
		}
		*o = res
	default:
		return fmt.Errorf("hooks: unexpected response type %T", out)
	}
	return nil
}

// Matches reports whether any hook of this phase would fire for the run.
func (d *Dispatcher) Matches(ctx context.Context, ident Identity, phase Phase) bool {
	return len(d.match(ctx, ident, "", phase)) > 0
}

// lifecycleCall builds the payload for one run-lifecycle hook.
func lifecycleCall(h *Hook, ident Identity, info LifecycleInfo) LifecycleHookCall {
	return LifecycleHookCall{
		Phase: h.Phase, Owner: h.Owner, HookName: h.Name,
		Agent: ident.Agent, UserID: ident.UserID, AgentID: ident.AgentID,
		RunContext: ident.runContext(),
		FinalText:  info.FinalText, StopReason: info.StopReason,
		StopHookActive: info.StopBlocks > 0, StopBlocks: info.StopBlocks,
		Subagent: info.Subagent, SubagentRunID: info.SubagentRunID,
		Status: info.Status, Error: info.Error,
		Trigger: info.Trigger, ContextTokens: info.ContextTokens, Window: info.Window,
		BeforeTokens: info.BeforeTokens, AfterTokens: info.AfterTokens,
	}
}

// invokeLifecycle runs one lifecycle hook and checks its result applies to
// the phase.
func (d *Dispatcher) invokeLifecycle(ctx context.Context, h *Hook, ident Identity, info LifecycleInfo) (LifecycleHookResult, error) {
	call := lifecycleCall(h, ident, info)
	var res LifecycleHookResult
	if err := d.invoke(ctx, h, &call, &res); err != nil {
		return LifecycleHookResult{}, err
	}
	if err := res.check(h.Phase); err != nil {
		return LifecycleHookResult{}, err
	}
	return res, nil
}

// GateOutcome is what RunGate returns.
type GateOutcome struct {
	// Denied stops what the gate guards; Reason says why.
	Denied bool
	Reason string
	// AdditionalContext is what the hooks asked to add, in chain order.
	AdditionalContext []string
	Decisions         []Decision
}

// RunGate runs a chain that may let something go ahead, deny it, or add
// context to it: agent_start (the run), subagent_start (a child's start),
// subagent_stop (a child's result reaching its parent) and pre_compact (a
// compaction). Chain order; the first deny stops the chain. A hook that
// fails denies when it fails closed, and is skipped when it fails open.
func (d *Dispatcher) RunGate(ctx context.Context, ident Identity, phase Phase, info LifecycleInfo) GateOutcome {
	var out GateOutcome
	for _, h := range d.match(ctx, ident, "", phase) {
		res, err := d.invokeLifecycle(ctx, h, ident, info)
		if err != nil {
			out.Decisions = append(out.Decisions, Decision{Owner: h.Owner, Name: h.Name, Phase: h.Phase,
				Kind: "unavailable", FailMode: failModeOf(h), Reason: decisionReason(err)})
			if h.FailMode == FailClosed || ctx.Err() != nil {
				log.Printf("hooks: %s %s/%s failed (fail_mode=%s): %v", phase, h.Owner, h.Name, failModeOf(h), err)
				out.Denied, out.Reason = true, "hook "+h.Owner+"/"+h.Name+" is unavailable"
				out.AdditionalContext = nil
				return out
			}
			log.Printf("hooks: %s %s/%s failed (fail_mode=open, passing through): %v", phase, h.Owner, h.Name, err)
			continue
		}
		if res.Decision == "deny" {
			reason := res.Reason
			if reason == "" {
				reason = "denied by hook " + h.Owner + "/" + h.Name
			}
			out.Decisions = append(out.Decisions, Decision{Owner: h.Owner, Name: h.Name, Phase: h.Phase, Kind: "deny", Reason: reason})
			out.Denied, out.Reason = true, reason
			out.AdditionalContext = nil
			return out
		}
		if text := strings.TrimSpace(res.AdditionalContext); text != "" {
			out.AdditionalContext = append(out.AdditionalContext, text)
			out.Decisions = append(out.Decisions, Decision{Owner: h.Owner, Name: h.Name, Phase: h.Phase,
				Kind: "context", AdditionalContext: text})
		}
	}
	return out
}

// ObserveBudget bounds one Observe call end to end: every matching hook, one
// after another. The run has moved on (or ended), so nothing waits for it, but
// it must not live on as a goroutine.
const ObserveBudget = 30 * time.Second

// Observe runs a chain that only reports — post_compact and run_end — off the
// caller's path. Their results are ignored and their failures only logged:
// what they would observe has already happened. A code body may notify but not
// ask, since a question has nothing to hold. The returned channel closes when
// the chain is done, for a caller (or a test) that wants to wait.
func (d *Dispatcher) Observe(ctx context.Context, ident Identity, phase Phase, info LifecycleInfo) <-chan struct{} {
	done := make(chan struct{})
	matched := d.match(ctx, ident, "", phase)
	if len(matched) == 0 {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		octx, cancel := context.WithTimeout(WithObserve(context.WithoutCancel(ctx)), ObserveBudget)
		defer cancel()
		for _, h := range matched {
			if _, err := d.invokeLifecycle(octx, h, ident, info); err != nil {
				log.Printf("hooks: %s %s/%s failed (observe only, ignored): %v", phase, h.Owner, h.Name, err)
			}
		}
	}()
	return done
}

type observeKey struct{}

// WithObserve marks ctx as an observe-only hook call.
func WithObserve(ctx context.Context) context.Context {
	return context.WithValue(ctx, observeKey{}, true)
}

// IsObserve reports whether ctx is an observe-only hook call, where a code body
// may not ask.
func IsObserve(ctx context.Context) bool { v, _ := ctx.Value(observeKey{}).(bool); return v }

// LifecycleInfo is what a run-lifecycle hook decides or reports on. Each
// phase fills the fields it has: agent_stop the answer; subagent_* the child;
// pre/post_compact the compaction; run_end how the run ended.
type LifecycleInfo struct {
	FinalText  string
	StopReason string
	// StopBlocks counts the blocks this answer has already had in a row.
	StopBlocks int

	Subagent      string
	SubagentRunID string
	Status        string
	Error         string

	Trigger       string
	ContextTokens int
	Window        int
	BeforeTokens  int
	AfterTokens   int
}

// Stop outcomes.
const (
	StopAllow = "allow"
	StopBlock = "block"
	StopHold  = "hold"
	// StopCancelled: the run ended while a hook was deciding. Nobody approved
	// the answer, so the run must end cancelled, never completed.
	StopCancelled = "cancelled"
)

// StopOutcome is what RunAgentStop returns to the loop.
type StopOutcome struct {
	// Kind is StopAllow, StopBlock, StopHold or StopCancelled.
	Kind string
	// Reason is a block's feedback for the model, or why the answer is held.
	Reason string
	// By names the hook ("<owner>/<name>") whose block or hold this is.
	By        string
	Decisions []Decision
}

// RunAgentStop runs the agent_stop chain, in registration order, the run's
// tenant hooks before the operator-global ones.
//
// Precedence is block > hold > allow. The first block stops the chain: the
// model has to try again anyway, and holding a person on an answer an
// automated check has already rejected would waste their time. A hold does
// not stop the chain, so a later hook may still block. A hook that fails holds
// the answer when it fails closed — the gate a closed agent_stop hook stands
// for is a person's — and is skipped when it fails open. If the run ends while
// a hook is deciding, the outcome is StopCancelled, whatever the fail mode.
func (d *Dispatcher) RunAgentStop(ctx context.Context, ident Identity, stop LifecycleInfo) StopOutcome {
	out := StopOutcome{Kind: StopAllow}
	for _, h := range d.match(ctx, ident, "", PhaseAgentStop) {
		name := h.Owner + "/" + h.Name
		res, err := d.invokeLifecycle(ctx, h, ident, stop)
		if err != nil {
			out.Decisions = append(out.Decisions, Decision{Owner: h.Owner, Name: h.Name, Phase: h.Phase,
				Kind: "unavailable", FailMode: failModeOf(h), Reason: decisionReason(err)})
			if ctx.Err() != nil {
				// The run ended while the hook was deciding. Reported as allow,
				// the loop left normally and recorded an answer nobody approved
				// as a completion.
				log.Printf("hooks: agent_stop %s failed (run ended): %v", name, err)
				out.Kind, out.Reason, out.By = StopCancelled, "the run ended while hook "+name+" was deciding", name
				return out
			}
			if h.FailMode == FailClosed {
				log.Printf("hooks: agent_stop %s failed (fail_mode=closed, holding): %v", name, err)
				if out.Kind == StopAllow {
					out.Kind, out.Reason, out.By = StopHold, "hook "+name+" is unavailable", name
				}
				continue
			}
			log.Printf("hooks: agent_stop %s failed (fail_mode=open, passing through): %v", name, err)
			continue
		}
		switch res.Decision {
		case "block":
			out.Decisions = append(out.Decisions, Decision{Owner: h.Owner, Name: h.Name, Phase: h.Phase, Kind: "block", Reason: res.Reason})
			out.Kind, out.Reason, out.By = StopBlock, res.Reason, name
			return out
		case "hold":
			out.Decisions = append(out.Decisions, Decision{Owner: h.Owner, Name: h.Name, Phase: h.Phase, Kind: "hold", Reason: res.Reason})
			if out.Kind == StopAllow {
				out.Kind, out.Reason, out.By = StopHold, res.Reason, name
			}
		}
	}
	return out
}
