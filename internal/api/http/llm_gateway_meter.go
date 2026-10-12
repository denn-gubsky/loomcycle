package http

import (
	"context"
	"encoding/json"
	"log"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// gatewayMeter accounts for one raw model call: what the provider reported it
// used, and, for a call that ends without a report, how much went in and came
// out.
//
// A gateway call used to be a log line and nothing else. It is metered here
// by watching the provider's own event stream, so every path that serves a
// call — either wire format, streamed or not, ended normally or not — is
// metered by putting its channel through tap, with nothing to remember in the
// path itself.
type gatewayMeter struct {
	// usage is the provider's last usage report; reported says there was one.
	usage    providers.Usage
	reported bool
	// inputBytes is the size of what was sent; outputBytes of what came back.
	// Used only when the provider reported nothing.
	inputBytes, outputBytes int
}

// newGatewayMeter starts a meter for a request about to be sent.
func newGatewayMeter(req providers.Request) *gatewayMeter {
	m := &gatewayMeter{}
	// The JSON size of the prompt: not a token count, but it grows with one.
	if b, err := json.Marshal(struct {
		System   []providers.ContentBlock `json:"system"`
		Messages []providers.Message      `json:"messages"`
		Tools    []providers.ToolSpec     `json:"tools"`
	}{req.System, req.Messages, req.Tools}); err == nil {
		m.inputBytes = len(b)
	}
	return m
}

// tap returns a channel that carries every event of ch, having noted each one
// first. It closes when ch does, after the last event was noted, so a reader
// that has read it to its end may read the meter.
//
// Every reader of a gateway call's events reads to the end, including after an
// error: a usage report is often the LAST thing a provider sends.
func (m *gatewayMeter) tap(ch <-chan providers.Event) <-chan providers.Event {
	out := make(chan providers.Event)
	go func() {
		defer close(out)
		for ev := range ch {
			m.note(ev)
			out <- ev
		}
	}()
	return out
}

// note records what one event says about the call's size.
func (m *gatewayMeter) note(ev providers.Event) {
	if ev.Usage != nil {
		// Where a report rides is the driver's choice: on Done for every
		// real driver, on a usage event for a stream the run loop produced.
		m.usage, m.reported = *ev.Usage, true
	}
	m.outputBytes += len(ev.Text)
	if ev.ToolUse != nil {
		m.outputBytes += len(ev.ToolUse.Name) + len(ev.ToolUse.Input)
	}
}

// bytesPerEstimatedToken turns a byte count into a token estimate. Three, not
// the usual four, so the estimate errs high: it is only ever used to count an
// unreported call against a budget, where too little is the unsafe direction.
const bytesPerEstimatedToken = 3

// settleGatewayCall closes the account for one gateway call.
//
// A call the provider reported on is written to the usage ledger and counted
// against the caller's budget, whole: cache-creation and cache-read tokens
// are tokens a run's budget counts, so they count here.
//
// A call that ended with NO report — the caller hung up, the provider failed
// first, or it reported zero — is counted against the BUDGET by an estimate
// from its size, and written to the log, but not to the ledger. Counting nothing would make
// "read the answer, then drop the connection before the usage arrives" a way
// to call the model for free under a hard limit. Writing a guess into the
// ledger would put a number nobody measured into what an operator bills from.
// The budget is the control; the ledger is the record.
//
// KNOWN LIMIT: the estimate lives in this replica's month-to-date counter,
// which is rebuilt from the ledger at a restart and is not shared with other
// replicas. So it is as durable as the counter itself — the advisory,
// per-replica enforcement the budgets have today — and no more. Making it
// durable needs a ledger row marked as estimated, which belongs with strict
// budget enforcement, where the counter stops being advisory.
//
// wireUser is the request's own user field, used only when there is no
// principal. The call belongs to no run, so it is filed under the caller
// alone, as an off-run decision is.
func (s *Server) settleGatewayCall(ctx context.Context, wireUser, providerID, modelID string, m *gatewayMeter) {
	tenant, user := tenantFromCtx(ctx), auth.SubjectForFairness(ctx, wireUser)
	// A report of zero tokens for a call that sent a prompt is not a report:
	// it is a driver or a provider that did not count. Treating it as "this
	// call was free" would let any provider that omits its usage serve
	// unmetered calls, so it falls through to the estimate like no report.
	if u := m.usage; m.reported && u.InputTokens+u.OutputTokens+u.CacheCreationTokens+u.CacheReadTokens > 0 {
		// The provider and model the call was dispatched to: a usage row is
		// priced by them.
		u.Provider, u.Model = providerID, modelID
		// On a ctx that outlives the request: the tokens were spent whether
		// or not the caller is still there to hear the answer.
		_ = s.recordCallUsage(context.WithoutCancel(ctx), "", tools.RunIdentityValue{TenantID: tenant, UserID: user}, "", 0, &u)
		return
	}
	est := int64((m.inputBytes + m.outputBytes + bytesPerEstimatedToken - 1) / bytesPerEstimatedToken)
	if est == 0 {
		return
	}
	s.limits.Add(tenant, user, est)
	log.Printf("llm_gateway: no usage report (call ended early or failed) provider=%s model=%s user_id=%q — counted %d estimated tokens against the budget, not written to the usage ledger",
		providerID, modelID, user, est)
}
