// Package builtin's Interruption tool — v0.8.16.
//
// Human-in-the-loop primitive. Three ops on a single discriminated
// `op` field, matching Memory / Channel / AgentDef / Evaluation /
// Context:
//
//   - ask     — surface a question to a human; agent loop blocks
//     until answered, timed out, or cancelled.
//   - notify  — fire-and-forget message (no answer expected).
//   - cancel  — agent unblocks a previously-asked question (e.g.
//     it figured the answer out on its own).
//
// `ask` is the load-bearing op — it blocks the loop via
// channels.Bus.Wait on a dedicated bus key ("intr:<id>"). The Web UI
// (the v0.8.16 default delivery surface) resolves via
// POST /v1/runs/{run_id}/interrupts/{interrupt_id}/resolve, which
// writes the resolved row + calls bus.Notify; this Execute wakes,
// reads the answer, returns it as the tool result, and the loop's
// next iteration uses the human's input.
//
// A dedicated heartbeat ticker fires opts.OnHeartbeat at a
// configurable interval (default 30s) while the call is blocked.
// Without this, a question that waits an hour gets swept as a
// crashed run by the v0.5.0 heartbeat sweeper (default StaleAfter
// 10 minutes, calibrated for tool-dispatch durations).
//
// Future-kinds (pause / wait_until / approval) slot in additively
// on the same `kind` enum column without reopening this tool — see
// doc-internal/rfcs/interruption-tool.md §8.
package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Interruption is the v0.8.16 human-in-the-loop tool.
type Interruption struct {
	// Store is the persistence backend. Required.
	Store store.Store

	// Bus is the in-process notification bus. Required — ask blocks
	// on bus.Wait, the HTTP resolve endpoint calls bus.Notify.
	// Without it, ask polls storage every Wait timeout, which would
	// make resolves visibly laggy.
	Bus *channels.Bus

	// SystemPublisher publishes to the `_system/interrupts/pending`
	// and `_system/interrupts/resolved` channels (operator-declared
	// per the v0.8.6 system-channels pattern). Optional — when nil,
	// the tool still functions but external Channel subscribers
	// (Web UI, Slack bot, etc.) won't see pub/sub signals. The
	// in-process bus.Notify path still works.
	SystemPublisher channels.SystemPublisher

	// The heartbeat-during-block path uses Store.UpdateHeartbeat
	// directly (run id recovered from ctx). No injected closure.

	// DefaultTimeout is the timeout applied when an `ask` doesn't
	// pass timeout_ms. 0 = no timeout (the interruption sits
	// pending indefinitely). Sourced from
	// LOOMCYCLE_INTERRUPTION_DEFAULT_TIMEOUT_MS at boot.
	DefaultTimeout time.Duration

	// MaxTimeout is the hard ceiling. timeout_ms above this is
	// clamped down to MaxTimeout. 0 = no ceiling. Sourced from
	// LOOMCYCLE_INTERRUPTION_MAX_TIMEOUT_MS.
	MaxTimeout time.Duration

	// HeartbeatInterval governs the ticker cadence. 0 = use the
	// 30-second default. Sourced from
	// LOOMCYCLE_INTERRUPTION_HEARTBEAT_INTERVAL_MS.
	HeartbeatInterval time.Duration

	// ResolvePollInterval is the durable-wake backstop cadence (F15).
	// A blocking `ask` wakes instantly via the in-process Bus when the
	// resolve lands on THIS runtime. In a multi-full-runtime topology
	// (several full runtimes sharing one DB — NOT the thin-client
	// `--upstream` shape, which routes every resolve to the owning
	// runtime), a resolve on a different runtime flips the DB row but
	// cannot reach this process's Bus. So while blocked we also poll the
	// interrupt row at this cadence and convert a detected terminal
	// status into a local Bus.Notify. 0 = use the default below.
	// Field exists mainly for test injection; not operator-tunable.
	ResolvePollInterval time.Duration

	// MaxPendingPerRun is the operator-global cap on simultaneous
	// pending interruptions per run. Agent yaml may NARROW (set its
	// own lower cap) but cannot exceed this. 0 = unbounded.
	// Sourced from LOOMCYCLE_INTERRUPTION_MAX_PENDING_PER_RUN.
	MaxPendingPerRun int

	// Backend selects the delivery surface. Valid values:
	//   - ""      / "webui" — default; agent loop blocks on bus.Wait;
	//                          humans answer via /ui/interrupts → POST
	//                          .../resolve.
	//   - "mcp_server:<name>" — instead of bus.Wait, the tool calls
	//                          the consumer's `mcp__<name>__ask` tool
	//                          via the per-run Dispatcher (recovered
	//                          from ctx). The consumer is responsible
	//                          for blocking until the human answers;
	//                          its tool result becomes the answer.
	//   - "cli"               — local-dev only; treated as webui from
	//                          the tool's POV (a separate process
	//                          consumes `_system/interrupts/pending`
	//                          and posts the resolve endpoint).
	//
	// Sourced from cfg.Interruption.Backend at boot.
	Backend string
}

const interruptionDescription = `Human-in-the-loop primitive (v0.8.16). ` +
	`ask: surface a question to a human; agent loop blocks until answered, timed out, or cancelled. ` +
	`notify: fire-and-forget message (no answer expected). ` +
	`cancel: agent unblocks a previously-asked question that it answered itself. ` +
	`Delivery surface is operator-selected (webui / mcp_server / cli); the tool interface is identical across them.`

const interruptionInputSchema = `{
  "type": "object",
  "properties": {
    "op":              {"type": "string", "enum": ["ask","notify","cancel"], "description": "Which operation to perform."},
    "question":        {"type": "string", "description": "ask only: the question text shown to the human."},
    "options":         {"type": "array",  "items": {"type": "string"}, "description": "ask only (optional): a fixed list of allowed answers. When set, the resolve endpoint refuses any answer not in this list. Absent or empty = free-text answer."},
    "context":         {"type": "string", "description": "ask only (optional): a short hint for the answerer (e.g. \"47 records pending\")."},
    "timeout_ms":      {"type": "integer", "description": "ask only (optional): how long to block before giving up. Absent = operator default; capped by operator max."},
    "priority":        {"type": "string", "enum": ["low","normal","high"], "description": "ask/notify (optional): informational badge for the UI. Default normal."},
    "message":         {"type": "string", "description": "notify only: the message text."},
    "interruption_id": {"type": "string", "description": "cancel only: the interrupt_id returned by a prior ask."}
  },
  "required": ["op"],
  "additionalProperties": false
}`

type interruptionInput struct {
	Op             string   `json:"op"`
	Question       string   `json:"question,omitempty"`
	Options        []string `json:"options,omitempty"`
	Context        string   `json:"context,omitempty"`
	TimeoutMS      int      `json:"timeout_ms,omitempty"`
	Priority       string   `json:"priority,omitempty"`
	Message        string   `json:"message,omitempty"`
	InterruptionID string   `json:"interruption_id,omitempty"`
}

// Name implements tools.Tool.
func (it *Interruption) Name() string { return "Interruption" }

// Description implements tools.Tool.
func (it *Interruption) Description() string { return interruptionDescription }

// InputSchema implements tools.Tool.
func (it *Interruption) InputSchema() json.RawMessage {
	return json.RawMessage(interruptionInputSchema)
}

// Execute implements tools.Tool. Dispatches off `op`; ACL gate
// shared across all ops.
func (it *Interruption) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	if it.Store == nil {
		return errResult("Interruption tool: not configured (no Store backend)"), nil
	}
	if it.Bus == nil {
		return errResult("Interruption tool: not configured (no Bus — blocking ask cannot wake on resolve)"), nil
	}
	var in interruptionInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errResult(fmt.Sprintf("invalid input JSON: %s", err)), nil
	}

	policy := tools.InterruptionPolicy(ctx)
	if !policy.Enabled {
		return errResult(
			"Interruption tool: not enabled for this agent — add `Interruption` to the agent's tools list",
		), nil
	}

	switch in.Op {
	case "ask":
		return it.execAsk(ctx, policy, in)
	case "notify":
		return it.execNotify(ctx, policy, in)
	case "cancel":
		return it.execCancel(ctx, in)
	case "":
		return errResult("missing required field: op"), nil
	default:
		return errResult(fmt.Sprintf("unknown op %q (must be one of: ask, notify, cancel)", in.Op)), nil
	}
}

// execAsk creates a pending interrupt row, emits the SSE event,
// publishes to _system/interrupts/pending, blocks on Bus.Wait with
// a heartbeat ticker, then reads the resolved row + returns the
// answer as the tool result.
func (it *Interruption) execAsk(ctx context.Context, policy tools.InterruptionPolicyValue, in interruptionInput) (tools.Result, error) {
	if in.Question == "" {
		return errResult("ask: missing required field: question"), nil
	}
	if !kindAllowed("question", policy.Kinds) {
		return errResult("ask: kind 'question' is not in this agent's allowed kinds (interruption.kinds yaml)"), nil
	}
	priority := normalizePriority(in.Priority)

	ident := tools.RunIdentity(ctx)
	runID := tools.RunID(ctx)
	if runID == "" {
		return errResult("Interruption tool: run id missing from ctx (server wiring bug — please report)"), nil
	}

	// max_pending check. Agent yaml MAY narrow below the global; we
	// honour the smaller of the two (0 on either side = "no cap on
	// that axis"). Per-run, not per-user — see RFC §6 Q5.
	cap := effectiveMaxPending(policy.MaxPending, it.MaxPendingPerRun)
	if cap > 0 {
		n, err := it.Store.InterruptCountPendingByRun(ctx, runID)
		if err != nil {
			return errResult(fmt.Sprintf("ask: storage error counting pending: %s", err)), nil
		}
		if n >= cap {
			return errResult(fmt.Sprintf(
				"ask: max_pending (%d) already reached for this run — wait for one of the %d pending interruptions to resolve or cancel",
				cap, n,
			)), nil
		}
	}

	// Compute the effective timeout. Caller value clamped to
	// [0, MaxTimeout]; absent → DefaultTimeout.
	timeout := it.resolveTimeout(in.TimeoutMS)

	id := store.MintInterruptID(time.Now())
	now := time.Now()
	var expiresAt time.Time
	if timeout > 0 {
		expiresAt = now.Add(timeout)
	}
	var optsJSON json.RawMessage
	if len(in.Options) > 0 {
		b, err := json.Marshal(in.Options)
		if err != nil {
			return errResult(fmt.Sprintf("ask: marshal options: %s", err)), nil
		}
		optsJSON = b
	}
	row := store.InterruptRow{
		InterruptID: id,
		RunID:       runID,
		Kind:        store.InterruptKindQuestion,
		Status:      store.InterruptStatusPending,
		Question:    in.Question,
		Options:     optsJSON,
		ContextData: in.Context,
		Priority:    priority,
		CreatedAt:   now,
		ExpiresAt:   expiresAt,
		UserID:      ident.UserID,
		AgentID:     ident.AgentID,
		// AgentName isn't directly on RunIdentity; ctx carries it via
		// the policy / loop wiring. Empty is fine — listing queries
		// gracefully fall back without it.
	}
	if _, err := it.Store.InterruptCreate(ctx, row); err != nil {
		return errResult(fmt.Sprintf("ask: storage error: %s", err)), nil
	}

	// Emit the SSE event for the run's own stream — Web UI consumers
	// already have an open connection and use this for real-time
	// modal rendering without a follow-up GET.
	tools.EventEmitter(ctx)(providers.Event{
		Type: providers.EventInterruptionPending,
		Interruption: &providers.InterruptionEventInfo{
			InterruptID: id,
			Kind:        store.InterruptKindQuestion,
			Question:    in.Question,
			Options:     optsJSON,
			Context:     in.Context,
			Priority:    priority,
			ExpiresAt:   rfc3339OrEmpty(expiresAt),
		},
	})

	// Publish to _system/interrupts/pending so non-run subscribers
	// (Web UI inbox view, Slack notifier, etc.) learn about it via
	// the Channel substrate rather than an SSE event they don't see.
	if it.SystemPublisher != nil && ident.UserID != "" {
		payload, _ := json.Marshal(map[string]any{
			"interrupt_id": id,
			"run_id":       runID,
			"kind":         store.InterruptKindQuestion,
			"question":     in.Question,
			"options":      json.RawMessage(optsJSON),
			"priority":     priority,
			"expires_at":   rfc3339OrEmpty(expiresAt),
			"agent_id":     ident.AgentID,
		})
		_, _ = it.SystemPublisher.PublishNow(
			ctx,
			"_system/interrupts/pending",
			store.MemoryScopeUser, ident.UserID,
			payload, channels.SystemPublisherUserID,
			0, // maxMessages — operator yaml configures
			0, // defaultTTL — operator yaml configures
		)
	}

	// Backend selection:
	//   - mcp_server:<name> → call the consumer's MCP tool directly
	//     via the per-run Dispatcher. The consumer's tool blocks
	//     until the human answers; we just write the row, dispatch,
	//     persist the answer, return.
	//   - webui / cli / "" → block on bus.Wait. The resolve handler
	//     (or the cli answerer) writes the row + notifies.
	if mcpName, ok := mcpServerFromBackend(it.Backend); ok {
		return it.execAskViaMCP(ctx, id, mcpName, in, optsJSON, expiresAt, timeout)
	}

	// Block on the bus. Heartbeat ticker fires in a sibling goroutine
	// so the run stays alive across long waits.
	if err := it.blockWithHeartbeat(ctx, id, timeout); err != nil {
		// ctx-cancelled OR timeout fired AFTER bus.Wait returned but
		// before we could read the row. Recover the terminal status
		// from storage; "still pending" → finalise it ourselves and
		// report cancel/timeout.
		return it.finaliseUnresolved(ctx, id, err)
	}

	// Wake path: bus.Notify fired. Re-read the row to get the
	// resolved answer (the resolve handler wrote it before Notify).
	final, err := it.Store.InterruptGet(ctx, id)
	if err != nil {
		return errResult(fmt.Sprintf("ask: post-resolve read: %s", err)), nil
	}
	return it.toolResultForStatus(final), nil
}

// execAskViaMCP is the consumer-MCP delivery path. Calls the named
// MCP server's `ask` tool via the per-run Dispatcher; the consumer
// blocks on its own (HTTP round-trip from loomcycle → consumer →
// human → consumer → loomcycle). Result becomes the answer.
//
// On success we finalise the row as resolved + return the answer to
// the agent. On dispatch error (consumer down, etc.) we mark the
// row cancelled (audit attribution = "agent_cancel" since the agent
// triggered the dispatch but couldn't complete it).
func (it *Interruption) execAskViaMCP(ctx context.Context, interruptID, mcpName string, in interruptionInput, _ json.RawMessage, _ time.Time, timeout time.Duration) (tools.Result, error) {
	disp := tools.DispatcherFromCtx(ctx)
	if disp == nil {
		// No dispatcher → fall back to webui path. Operator misconfig
		// (yaml says mcp_server but server registration absent),
		// surfaces clearly as a tool error rather than silently
		// blocking.
		_ = it.Store.InterruptFinish(ctx, interruptID, store.InterruptStatusCancelled, store.InterruptResolvedByAgentCancel)
		return errResult(fmt.Sprintf(
			"ask: backend mcp_server:%s configured but no dispatcher attached to ctx (server wiring bug)",
			mcpName,
		)), nil
	}
	toolName := "mcp__" + mcpName + "__ask"

	// Build the args payload — the consumer's tool decides its own
	// schema, but we forward the agent-supplied question+options+
	// context unchanged so the consumer can render them faithfully.
	argMap := map[string]any{
		"question":     in.Question,
		"interrupt_id": interruptID,
	}
	if len(in.Options) > 0 {
		argMap["options"] = in.Options
	}
	if in.Context != "" {
		argMap["context"] = in.Context
	}
	if timeout > 0 {
		argMap["timeout_ms"] = int(timeout / time.Millisecond)
	}
	args, _ := json.Marshal(argMap)

	// Apply timeout to the dispatch ctx so a hung consumer doesn't
	// pin the run forever.
	callCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	res := disp.Execute(callCtx, toolName, args)
	if res.IsError {
		// Consumer surfaced an error tool result — treat as a
		// failed delivery, mark cancelled.
		_ = it.Store.InterruptFinish(context.WithoutCancel(ctx), interruptID, store.InterruptStatusCancelled, store.InterruptResolvedByAgentCancel)
		return res, nil
	}

	// Consumer returned a successful tool result. The result text is
	// the human's answer (typically free-text JSON). Persist it +
	// return it to the agent.
	if err := it.Store.InterruptResolve(context.WithoutCancel(ctx), interruptID, res.Text, store.InterruptResolvedByMCP, nil); err != nil {
		// Resolve write failed but the consumer already collected
		// the answer. Surface as is_error so the agent doesn't see
		// a phantom-success answer that doesn't match storage.
		return errResult(fmt.Sprintf("ask: persist answer from mcp_server:%s: %s", mcpName, err)), nil
	}

	out, _ := json.Marshal(map[string]any{
		"interrupt_id": interruptID,
		"answer":       res.Text,
		"resolved_at":  time.Now().UTC().Format(time.RFC3339Nano),
		"resolved_by":  store.InterruptResolvedByMCP,
	})
	return tools.Result{Text: string(out)}, nil
}

// mcpServerFromBackend parses "mcp_server:<name>" into (<name>, true).
// Returns ("", false) for any other backend value.
func mcpServerFromBackend(backend string) (string, bool) {
	const prefix = "mcp_server:"
	if strings.HasPrefix(backend, prefix) {
		name := backend[len(prefix):]
		if name != "" {
			return name, true
		}
	}
	return "", false
}

// execNotify is fire-and-forget — no blocking. Writes a row with
// status=resolved immediately (since the notify carries no answer
// to wait for); the Web UI / dashboards still surface it via the
// system channel publish.
func (it *Interruption) execNotify(ctx context.Context, policy tools.InterruptionPolicyValue, in interruptionInput) (tools.Result, error) {
	if in.Message == "" {
		return errResult("notify: missing required field: message"), nil
	}
	if !kindAllowed("question", policy.Kinds) {
		// notify writes a row with kind=question (a degenerate
		// no-answer ask), so it gates on the same "question" kind
		// as ask. An operator that wants notify-but-not-ask
		// granularity must wait for a future v0.9.x split; v0.8.16
		// treats notify as a degenerate ask deliberately, because
		// both surfaces share the same delivery channels +
		// audit-row shape and splitting them would proliferate the
		// kind enum without a real use case.
		return errResult("notify: kind 'question' is not in this agent's allowed kinds (interruption.kinds must include 'question')"), nil
	}

	ident := tools.RunIdentity(ctx)
	runID := tools.RunID(ctx)
	if runID == "" {
		return errResult("Interruption tool: run id missing from ctx"), nil
	}

	priority := normalizePriority(in.Priority)
	id := store.MintInterruptID(time.Now())
	now := time.Now()
	row := store.InterruptRow{
		InterruptID: id,
		RunID:       runID,
		Kind:        store.InterruptKindQuestion,
		// Status starts pending then immediately transitions to
		// resolved via InterruptFinish below — keeps the row's
		// status enum honest (every row passes through pending).
		Status:    store.InterruptStatusPending,
		Question:  in.Message,
		Priority:  priority,
		CreatedAt: now,
		UserID:    ident.UserID,
		AgentID:   ident.AgentID,
	}
	if _, err := it.Store.InterruptCreate(ctx, row); err != nil {
		return errResult(fmt.Sprintf("notify: storage error: %s", err)), nil
	}
	// notify is treated as cancelled (no human response) for the
	// terminal-status enum.  resolved_by = "agent_cancel" preserves
	// audit attribution.
	if err := it.Store.InterruptFinish(ctx, id, store.InterruptStatusCancelled, store.InterruptResolvedByAgentCancel); err != nil {
		return errResult(fmt.Sprintf("notify: finish: %s", err)), nil
	}

	if it.SystemPublisher != nil && ident.UserID != "" {
		payload, _ := json.Marshal(map[string]any{
			"interrupt_id": id,
			"run_id":       runID,
			"kind":         store.InterruptKindQuestion,
			"message":      in.Message,
			"priority":     priority,
			"notify":       true,
			"agent_id":     ident.AgentID,
		})
		_, _ = it.SystemPublisher.PublishNow(
			ctx,
			"_system/interrupts/pending",
			store.MemoryScopeUser, ident.UserID,
			payload, channels.SystemPublisherUserID, 0, 0,
		)
	}

	out, _ := json.Marshal(map[string]any{
		"interrupt_id": id,
		"status":       "delivered",
		"created_at":   now.UTC().Format(time.RFC3339Nano),
	})
	return tools.Result{Text: string(out)}, nil
}

// execCancel transitions a pending interrupt to status=cancelled.
// Agent-side path — distinct from human "I don't want to answer"
// which the resolve endpoint maps to a 200 with no answer (out of
// scope for v0.8.16).
func (it *Interruption) execCancel(ctx context.Context, in interruptionInput) (tools.Result, error) {
	if in.InterruptionID == "" {
		return errResult("cancel: missing required field: interruption_id"), nil
	}
	err := it.Store.InterruptFinish(
		ctx,
		in.InterruptionID,
		store.InterruptStatusCancelled,
		store.InterruptResolvedByAgentCancel,
	)
	var nf *store.ErrNotFound
	if errors.As(err, &nf) {
		return errResult(fmt.Sprintf("cancel: interruption_id %q not found", in.InterruptionID)), nil
	}
	wasPending := true
	if err != nil {
		if errors.Is(err, store.ErrInterruptAlreadyTerminal) {
			wasPending = false
		} else {
			return errResult(fmt.Sprintf("cancel: storage error: %s", err)), nil
		}
	}
	// If the row was already cancelled / timed_out / resolved, the
	// tool reports was_pending: false. The bus.Notify still fires —
	// any concurrent ask waiter wakes and sees the terminal status.
	// (For race-safety; harmless when no waiter exists.)
	it.Bus.Notify("intr:" + in.InterruptionID)

	out, _ := json.Marshal(map[string]any{
		"interrupt_id": in.InterruptionID,
		"status":       store.InterruptStatusCancelled,
		"was_pending":  wasPending,
	})
	return tools.Result{Text: string(out)}, nil
}

// defaultResolvePollInterval is the durable-wake poll cadence when
// Interruption.ResolvePollInterval is unset. Deliberately coarse: it only
// bounds cross-runtime wake latency in a multi-full-runtime topology, and a
// blocked `ask` is a human-in-the-loop event (low volume), so the periodic
// InterruptGet adds negligible load in the common single-runtime case.
const defaultResolvePollInterval = 15 * time.Second

// blockWithHeartbeat is the load-bearing wait: bus.Wait blocks the
// loop's tool dispatch goroutine until the resolve endpoint calls
// bus.Notify. A sibling goroutine fires the run's heartbeat
// callback every HeartbeatInterval so the sweeper doesn't mark the
// run as dead.
//
// On clean resolve, returns nil. On timeout fired BY bus.Wait
// itself, returns a sentinel error indicating timeout; on ctx
// cancel, returns ctx.Err(). The caller distinguishes via errors.Is
// and calls InterruptFinish accordingly.
func (it *Interruption) blockWithHeartbeat(ctx context.Context, interruptID string, timeout time.Duration) error {
	// bus.Wait with timeout=0 returns immediately (the bus contract).
	// For "no operator timeout" we pass a very large duration so the
	// timeout branch never fires; ctx is still respected.
	waitTimeout := timeout
	if waitTimeout <= 0 {
		// 100 years — effectively no timeout. ctx still trumps.
		waitTimeout = 100 * 365 * 24 * time.Hour
	}

	runID := tools.RunID(ctx)
	done := make(chan struct{})
	defer close(done)

	hbInterval := it.HeartbeatInterval
	if hbInterval <= 0 {
		hbInterval = 30 * time.Second
	}
	pollInterval := it.ResolvePollInterval
	if pollInterval <= 0 {
		pollInterval = defaultResolvePollInterval
	}

	// Sibling goroutine, two jobs:
	//
	//  1. Heartbeat. Calls Store.UpdateHeartbeat(runID) at
	//     HeartbeatInterval cadence so the v0.5.0 sweeper doesn't mark
	//     this run as dead while the loop's per-iteration heartbeat is
	//     suspended inside our blocking Wait. The DB write uses a ctx
	//     that survives the loop's eventual ctx-cancel so a heartbeat in
	//     flight at cancel time still commits cleanly (cheaper than a
	//     failed DB call's retry storm).
	//
	//  2. Durable resolve-poll (F15). A resolve landing on a DIFFERENT
	//     full runtime flips the DB row but cannot reach this process's
	//     in-memory Bus, so the Wait below would hang to its timeout.
	//     Re-read the row at pollInterval and Bus.Notify when it goes
	//     terminal. We re-Notify on EVERY terminal tick (not once): Bus
	//     notifications are edge-triggered and lost if they fire before
	//     Wait registers its waker, so repeating until `done` closes is
	//     self-healing. In the single-runtime case the resolve handler's
	//     own Bus.Notify wakes Wait first and `done` closes before the
	//     poll ever fires — this is purely the cross-runtime backstop.
	go func() {
		hbTicker := time.NewTicker(hbInterval)
		defer hbTicker.Stop()
		pollTicker := time.NewTicker(pollInterval)
		defer pollTicker.Stop()
		for {
			select {
			case <-hbTicker.C:
				if runID != "" {
					hbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
					_ = it.Store.UpdateHeartbeat(hbCtx, runID)
					cancel()
				}
			case <-pollTicker.C:
				pCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				row, err := it.Store.InterruptGet(pCtx, interruptID)
				cancel()
				if err == nil && row.Status != store.InterruptStatusPending {
					it.Bus.Notify("intr:" + interruptID)
				}
			case <-done:
				return
			}
		}
	}()

	woke := it.Bus.Wait(ctx, "intr:"+interruptID, waitTimeout)
	if woke {
		return nil
	}
	// Distinguish ctx-cancel from timeout. Bus.Wait collapses both
	// to "false"; ctx.Err() is the deterministic discriminator.
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errInterruptionTimeout
}

// finaliseUnresolved handles the bus.Wait-returned-false path.
// Reads the storage row to recover the row state; transitions to
// timed_out or cancelled depending on cause; returns the tool
// result. Two race shapes to handle:
//
//  1. bus.Wait returned false due to ctx cancel → run is being
//     torn down. Finish the row as cancelled (resolved_by =
//     agent_cancel, semantically "run abandoned").
//  2. bus.Wait returned false due to timer → mark timed_out. The
//     sweeper handles process-crashed rows; this path handles the
//     "still running, but Tom-and-Jerry'd by the timer" case.
//
// In both cases the row may ALREADY have been finalised by another
// thread (the sweeper, the resolve endpoint racing with the
// timeout, etc.). InterruptFinish returns ErrInterruptAlreadyTerminal
// in that case — read the row and report whatever's in storage.
func (it *Interruption) finaliseUnresolved(ctx context.Context, interruptID string, cause error) (tools.Result, error) {
	var status, resolvedBy string
	if errors.Is(cause, errInterruptionTimeout) {
		status = store.InterruptStatusTimedOut
		resolvedBy = store.InterruptResolvedByTimeout
	} else {
		// ctx cancel + everything else → cancelled (agent-side
		// abandonment, surfaces as cancellation in the audit log).
		status = store.InterruptStatusCancelled
		resolvedBy = store.InterruptResolvedByAgentCancel
	}
	// Best-effort finalise. The follow-up read is authoritative.
	// Use a fresh ctx for the storage write so ctx-cancel-after-
	// wait doesn't sabotage the finalise.
	bg := context.WithoutCancel(ctx)
	_ = it.Store.InterruptFinish(bg, interruptID, status, resolvedBy)

	row, err := it.Store.InterruptGet(bg, interruptID)
	if err != nil {
		return errResult(fmt.Sprintf("ask: post-block read: %s", err)), nil
	}
	return it.toolResultForStatus(row), nil
}

// toolResultForStatus produces the tool result for whatever the
// final row state is.
func (it *Interruption) toolResultForStatus(row store.InterruptRow) tools.Result {
	switch row.Status {
	case store.InterruptStatusResolved:
		out, _ := json.Marshal(map[string]any{
			"interrupt_id": row.InterruptID,
			"answer":       row.Answer,
			"resolved_at":  rfc3339OrEmpty(row.ResolvedAt),
			"resolved_by":  row.ResolvedBy,
		})
		return tools.Result{Text: string(out)}
	case store.InterruptStatusDeclined:
		// RFC BH P2: the operator declined to answer. This is deliberately
		// a NON-error result (no IsError) — an error could make the agent
		// retry the question or abort, but the operator's intent is for the
		// agent to PROCEED without this input. Distinct from the cancelled
		// branch below, which stays IsError (run-cancel / timeout).
		out, _ := json.Marshal(map[string]any{
			"declined":     true,
			"note":         "The operator declined to answer; proceed without this input.",
			"interrupt_id": row.InterruptID,
			"resolved_by":  row.ResolvedBy,
		})
		return tools.Result{Text: string(out)}
	case store.InterruptStatusTimedOut:
		return tools.Result{
			IsError: true,
			Text:    fmt.Sprintf("interruption timed_out (id=%s) — no answer received in the allotted window", row.InterruptID),
		}
	case store.InterruptStatusCancelled:
		return tools.Result{
			IsError: true,
			Text:    fmt.Sprintf("interruption cancelled (id=%s, resolved_by=%s)", row.InterruptID, row.ResolvedBy),
		}
	default:
		// Pending — should never happen after finalise. Surface as
		// an explicit error so the operator can investigate.
		return tools.Result{
			IsError: true,
			Text:    fmt.Sprintf("interruption in unexpected state %q after block (id=%s)", row.Status, row.InterruptID),
		}
	}
}

// resolveTimeout clamps the caller's timeout_ms to [0, MaxTimeout]
// and substitutes DefaultTimeout when absent (0).
func (it *Interruption) resolveTimeout(reqMS int) time.Duration {
	if reqMS <= 0 {
		return it.DefaultTimeout
	}
	t := time.Duration(reqMS) * time.Millisecond
	if it.MaxTimeout > 0 && t > it.MaxTimeout {
		return it.MaxTimeout
	}
	return t
}

// --- helpers ---------------------------------------------------------

// errInterruptionTimeout is the sentinel returned by
// blockWithHeartbeat when bus.Wait fires its own timer (vs ctx
// being cancelled). Internal — never surfaces to the model.
var errInterruptionTimeout = fmt.Errorf("interruption: blocking wait timed out")

func kindAllowed(kind string, kinds []string) bool {
	if len(kinds) == 0 {
		// Empty allowlist defaults to ["question"] — the only
		// v0.8.16 kind. Future expansions land here as opt-in.
		return kind == store.InterruptKindQuestion
	}
	for _, k := range kinds {
		if k == kind {
			return true
		}
	}
	return false
}

func normalizePriority(p string) string {
	switch strings.ToLower(p) {
	case store.InterruptPriorityLow:
		return store.InterruptPriorityLow
	case store.InterruptPriorityHigh:
		return store.InterruptPriorityHigh
	default:
		return store.InterruptPriorityNormal
	}
}

func effectiveMaxPending(perAgent, perOperator int) int {
	switch {
	case perAgent <= 0 && perOperator <= 0:
		return 0
	case perAgent <= 0:
		return perOperator
	case perOperator <= 0:
		return perAgent
	case perAgent < perOperator:
		return perAgent
	default:
		return perOperator
	}
}

func rfc3339OrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
