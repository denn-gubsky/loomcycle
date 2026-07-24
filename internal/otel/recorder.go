package otel

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Recorder helpers centralize the loomcycle span attribute set so every
// call site stays consistent without duplicating the attribute names.
// A future rename to OpenTelemetry's evolving semconv (`gen_ai.*` is the
// 2026-era candidate for LLM-call attributes) happens in one place.
//
// Attribute key prefix: every loomcycle-specific attribute uses
// `loomcycle.*` so a Jaeger / Tempo / Honeycomb search for `loomcycle.`
// scopes cleanly to our spans, and we don't collide with any OTEL
// semconv that downstream collectors may add.

// Span names (stable; operators write Jaeger filters against them).
const (
	SpanRun          = "loomcycle.run"
	SpanIteration    = "loomcycle.iteration"
	SpanProviderCall = "loomcycle.provider.call"
	SpanToolCall     = "loomcycle.tool.call"
	SpanMCPCall      = "loomcycle.mcp.call"
)

// Attribute keys (stable; operators write Jaeger filters against them).
const (
	AttrRunID         = "loomcycle.run_id"
	AttrAgentID       = "loomcycle.agent_id"
	AttrAgentName     = "loomcycle.agent_name"
	AttrUserID        = "loomcycle.user_id"
	AttrParentAgentID = "loomcycle.parent_agent_id"
	AttrIteration     = "loomcycle.iteration"
	AttrProvider      = "loomcycle.provider"
	// AttrProviderKind distinguishes a synthetic provider (no model HTTP
	// request) from a real LLM driver — set to "synthetic-code" for the
	// code-js provider so operators can filter/aggregate code-agent runs.
	AttrProviderKind = "loomcycle.provider.kind"
	// AttrProviderCodeHash is the sha256 of a code-agent's index.js source
	// (RFC J Decision 9). It answers "which code version produced this run"
	// without versioning the JS files separately. Empty for real providers.
	AttrProviderCodeHash = "loomcycle.provider.code_hash"
	AttrModel            = "loomcycle.model"
	AttrTier             = "loomcycle.tier"
	AttrEffort           = "loomcycle.effort"
	AttrTool             = "loomcycle.tool"
	AttrMCPServer        = "loomcycle.mcp_server"
	AttrMCPTool          = "loomcycle.mcp_tool"
	AttrInputTokens      = "loomcycle.input_tokens"
	AttrOutputTokens     = "loomcycle.output_tokens"
	AttrCacheReadTokens  = "loomcycle.cache_read_tokens"
	AttrStopReason       = "loomcycle.stop_reason"
	AttrToolIsError      = "loomcycle.tool_is_error"
	// AttrQueueWaitMs is the time (milliseconds) a run waited inside the
	// concurrency semaphore before its slot was granted. 0 = immediate
	// acquire (no queue contention). Operators graphing this attribute
	// per-user_id validate that per-tenant fairness is working — when
	// the cap is set, queue waits should distribute fairly across users
	// instead of all landing on whoever's behind a noisy tenant.
	AttrQueueWaitMs = "loomcycle.queue_wait_ms"
)

// RunStartAttrs is the minimum attribute set every loomcycle.run span
// carries. AgentName + UserID are optional (background runs or
// system-initiated ones may lack them); empty strings are skipped so
// Jaeger doesn't surface empty values.
type RunStartAttrs struct {
	RunID         string
	AgentID       string
	AgentName     string
	UserID        string
	ParentAgentID string // sub-runs only
}

// RecordRunStart opens the top-level loomcycle.run span. The returned
// ctx must be used for everything downstream so the iteration spans
// nest under it. Caller defers span.End().
func RecordRunStart(ctx context.Context, attrs RunStartAttrs) (context.Context, trace.Span) {
	return Tracer().Start(ctx, SpanRun, trace.WithAttributes(runStartKVs(attrs)...))
}

func runStartKVs(a RunStartAttrs) []attribute.KeyValue {
	kvs := make([]attribute.KeyValue, 0, 5)
	if a.RunID != "" {
		kvs = append(kvs, attribute.String(AttrRunID, a.RunID))
	}
	if a.AgentID != "" {
		kvs = append(kvs, attribute.String(AttrAgentID, a.AgentID))
	}
	if a.AgentName != "" {
		kvs = append(kvs, attribute.String(AttrAgentName, a.AgentName))
	}
	if a.UserID != "" {
		kvs = append(kvs, attribute.String(AttrUserID, a.UserID))
	}
	if a.ParentAgentID != "" {
		kvs = append(kvs, attribute.String(AttrParentAgentID, a.ParentAgentID))
	}
	return kvs
}

// RecordIteration opens one loomcycle.iteration span per loop turn.
// Caller defers iterSpan.End() at the iteration boundary.
func RecordIteration(ctx context.Context, iter int) (context.Context, trace.Span) {
	return Tracer().Start(ctx, SpanIteration,
		trace.WithAttributes(attribute.Int(AttrIteration, iter)))
}

// ProviderCallAttrs identifies a provider HTTP request.
type ProviderCallAttrs struct {
	Provider string // "anthropic" | "openai" | "deepseek" | "gemini" | "ollama" | "ollama-local" | "code-js"
	Model    string
	Tier     string // optional — set when the agent's resolution went through a tier
	Effort   string // optional — set when the agent declared an effort hint
	Kind     string // optional — "synthetic-code" for code-js; empty for real LLM drivers
	CodeHash string // optional — sha256 of the code-agent's index.js (code-js only)
}

// RecordProviderCall opens loomcycle.provider.call for a single HTTP
// request. Caller defers span.End() at attempt boundary; retries are
// separate spans (one per attempt) so operators see retry latency.
func RecordProviderCall(ctx context.Context, attrs ProviderCallAttrs) (context.Context, trace.Span) {
	kvs := []attribute.KeyValue{}
	if attrs.Provider != "" {
		kvs = append(kvs, attribute.String(AttrProvider, attrs.Provider))
	}
	if attrs.Model != "" {
		kvs = append(kvs, attribute.String(AttrModel, attrs.Model))
	}
	if attrs.Tier != "" {
		kvs = append(kvs, attribute.String(AttrTier, attrs.Tier))
	}
	if attrs.Effort != "" {
		kvs = append(kvs, attribute.String(AttrEffort, attrs.Effort))
	}
	if attrs.Kind != "" {
		kvs = append(kvs, attribute.String(AttrProviderKind, attrs.Kind))
	}
	if attrs.CodeHash != "" {
		kvs = append(kvs, attribute.String(AttrProviderCodeHash, attrs.CodeHash))
	}
	return Tracer().Start(ctx, SpanProviderCall, trace.WithAttributes(kvs...))
}

// RecordToolCall opens loomcycle.tool.call around Dispatcher.Execute.
// The single canonical wrap-point covers every tool — built-in, MCP,
// sub-agent. MCP calls additionally open a nested loomcycle.mcp.call
// via RecordMCPCall.
func RecordToolCall(ctx context.Context, toolName string) (context.Context, trace.Span) {
	return Tracer().Start(ctx, SpanToolCall,
		trace.WithAttributes(attribute.String(AttrTool, toolName)))
}

// RecordMCPCall opens loomcycle.mcp.call inside the outer
// loomcycle.tool.call when the tool's name has the `mcp__server__tool`
// shape. Operators see two nested spans, the inner one carrying server
// + tool attributes parsed from the dispatched name.
func RecordMCPCall(ctx context.Context, server, tool string) (context.Context, trace.Span) {
	kvs := []attribute.KeyValue{}
	if server != "" {
		kvs = append(kvs, attribute.String(AttrMCPServer, server))
	}
	if tool != "" {
		kvs = append(kvs, attribute.String(AttrMCPTool, tool))
	}
	return Tracer().Start(ctx, SpanMCPCall, trace.WithAttributes(kvs...))
}

// RunDoneAttrs is the closing attribute set landed on the
// loomcycle.run span at finish. Set after the loop returns so totals
// reflect the whole run.
type RunDoneAttrs struct {
	InputTokens     int
	OutputTokens    int
	CacheReadTokens int
	StopReason      string
	Err             error // non-nil → SetStatus(Error, err.Error()) + RecordError
}

// SetRunDone closes out the run span's attribute set. Used at the run
// boundary (publishRunState path) so totals + error status reflect the
// final state.
func SetRunDone(span trace.Span, a RunDoneAttrs) {
	if span == nil || !span.IsRecording() {
		return
	}
	kvs := []attribute.KeyValue{}
	if a.InputTokens > 0 {
		kvs = append(kvs, attribute.Int(AttrInputTokens, a.InputTokens))
	}
	if a.OutputTokens > 0 {
		kvs = append(kvs, attribute.Int(AttrOutputTokens, a.OutputTokens))
	}
	if a.CacheReadTokens > 0 {
		kvs = append(kvs, attribute.Int(AttrCacheReadTokens, a.CacheReadTokens))
	}
	if a.StopReason != "" {
		kvs = append(kvs, attribute.String(AttrStopReason, a.StopReason))
	}
	if len(kvs) > 0 {
		span.SetAttributes(kvs...)
	}
	if a.Err != nil {
		span.RecordError(a.Err)
		span.SetStatus(codes.Error, a.Err.Error())
	}
}

// RecordQueueWait stamps the queue-wait duration on the run span.
// Called at the run-creation sites right after AcquireForUser returns
// successfully. No-op on a nil / non-recording span. The wait is
// truncated to milliseconds since attribute storage is more compact
// than a Duration string and operators graph in ms naturally.
func RecordQueueWait(span trace.Span, wait time.Duration) {
	if span == nil || !span.IsRecording() {
		return
	}
	span.SetAttributes(attribute.Int64(AttrQueueWaitMs, wait.Milliseconds()))
}

// RecordCompaction adds a "context.compaction" event to the run/iteration span
// when the loop compacts (trigger = manual|auto|self), with before/after token
// estimates. Per-run-shape metric → flows through OTEL (the documented path;
// /metrics stays substrate-only). No-op on a nil/non-recording span.
func RecordCompaction(span trace.Span, trigger string, before, after int) {
	if span == nil || !span.IsRecording() {
		return
	}
	span.AddEvent("context.compaction", trace.WithAttributes(
		attribute.String("compaction.trigger", trigger),
		attribute.Int("compaction.before_tokens", before),
		attribute.Int("compaction.after_tokens", after),
	))
}

// RecordCompactionCtx is RecordCompaction against the current span on ctx — for
// callers (the loop) that hold a ctx but not the span directly.
func RecordCompactionCtx(ctx context.Context, trigger string, before, after int) {
	RecordCompaction(trace.SpanFromContext(ctx), trigger, before, after)
}

// --- RFC BL P1 retrieval telemetry ---
//
// Per-op memory-retrieval shape (latency, dead-link drops, boot reconcile)
// flows through OTEL, NOT the in-process /metrics endpoint: that endpoint is
// substrate-only by architectural lock (RFC observability-profiles Decision 1
// — process + concurrency state only), and per-op shape is aggregated
// downstream from spans by the OTEL Collector's spanmetrics connector. These
// recorders mirror the loomcycle.memory.search span so
// the two memory backends produce one consistent series. All are no-op-safe:
// with OTEL unconfigured Tracer() is a no-op and every call here costs nothing.

// SpanMemorySearch is one memory retrieval. The span DURATION is the retrieval
// latency, so a downstream spanmetrics connector derives the
// loomcycle.memory.search.latency histogram (p50/p95) from it, labeled by the
// backend + mode attributes below.
const SpanMemorySearch = "loomcycle.memory.search"

// SpanHelpReconcile is the one-shot boot reconcile of the help-topic search
// index (RFC BL PR5). Its attributes carry the ReconcileStats counts.
const SpanHelpReconcile = "loomcycle.help.reconcile"

const (
	// AttrMemoryBackend labels a memory.search span by backend ("inprocess" |
	// backend kind) so operators can split latency/error series per backend.
	AttrMemoryBackend = "loomcycle.memory.backend"
	// AttrMemoryMode is the hybrid-vs-degrade dimension: "hybrid" (vector +
	// full-text fused by RRF) or "vector" (the cheap pure-vector path).
	AttrMemoryMode = "loomcycle.memory.mode"
	// AttrMemoryTopK is the retrieval's requested top_k.
	AttrMemoryTopK = "loomcycle.memory.top_k"
	// AttrDeadlinkDropped is the number of hits the read-time dead-link guard
	// dropped on this retrieval (RFC BL §2.10). The downstream
	// loomcycle.memory.deadlink.dropped counter derives from the same-named
	// span event.
	AttrDeadlinkDropped = "loomcycle.memory.deadlink_dropped"

	// AttrHelpWritten / Pruned / Unchanged / Failed / Degraded mirror the
	// help.ReconcileStats fields (RFC BL PR5) so the counters + degraded gauge
	// derive from the reconcile span.
	AttrHelpWritten   = "loomcycle.help.written"
	AttrHelpPruned    = "loomcycle.help.pruned"
	AttrHelpUnchanged = "loomcycle.help.unchanged"
	AttrHelpFailed    = "loomcycle.help.failed"
	AttrHelpDegraded  = "loomcycle.help.degraded"
)

// EventDeadlinkDropped is the span event emitted when the read-time dead-link
// guard drops at least one hit. A downstream connector counts it into the
// loomcycle.memory.deadlink.dropped metric.
const EventDeadlinkDropped = "loomcycle.memory.deadlink.dropped"

// --- RFC BL P2 consolidation telemetry ---

// SpanMemoryConsolidate is ONE consolidation pass over ONE memory target. The
// span DURATION is the pass's wall-clock cost, and the child run's own
// loomcycle.run / loomcycle.provider.call spans nest underneath it — so tokens,
// model, and per-attempt latency come from where they are already authoritative
// instead of being re-derived here (re-resolving the provider at the dispatcher
// is exactly the probe-vs-fire drift the fan-out's serial-dispatch probe already
// documents as a known gap).
const SpanMemoryConsolidate = "loomcycle.memory.consolidate"

// Consolidation attribute keys.
//
// NAMESPACE NOTE: these carry the span's own `consolidate` segment
// (loomcycle.memory.consolidate.*) rather than sitting flat under
// loomcycle.memory.* like the search span's keys. That deviates from the
// help-reconcile precedent (span loomcycle.help.reconcile, attrs
// loomcycle.help.written) DELIBERATELY: help.reconcile owns the whole
// loomcycle.help.* namespace, while loomcycle.memory.* is already shared with
// the retrieval span, so a flat `loomcycle.memory.added` would read as a count
// of plain KV writes. Do not "normalize" these to the flat form — operators
// filter on them, and the qualification is what keeps write-path and
// consolidation-path counters from colliding.
const (
	// AttrMemoryScope is the consolidated target's memory scope ("user").
	AttrMemoryScope = "loomcycle.memory.scope"

	// AttrConsolidateAdded is the number of memory keys that did not exist
	// before the pass and do afterwards.
	AttrConsolidateAdded = "loomcycle.memory.consolidate.added"
	// AttrConsolidateUpdated is the number of pre-existing keys the pass
	// rewrote in place.
	//
	// THIS IS ALSO WHERE A DROPPED-AS-DUPLICATE OUTCOME LANDS. The runtime
	// cannot observe "the pass decided a fact was a duplicate and merged it"
	// as a distinct event — the pass reports that in prose, and parsing a
	// model's prose into a metric would make the counter a measure of the
	// model's phrasing. Under the pipeline's deterministic subject-derived
	// keys a merged duplicate IS an in-place rewrite, so it is counted here
	// and named honestly rather than given a counter the runtime cannot fill.
	AttrConsolidateUpdated = "loomcycle.memory.consolidate.updated"
	// AttrConsolidateSuperseded is the number of keys the pass removed from
	// the live set (the soft-archive path). A hard delete is indistinguishable
	// from a supersede here and counts the same, because both mean "the pass
	// retired a fact" — which is what an operator is asking.
	AttrConsolidateSuperseded = "loomcycle.memory.consolidate.superseded"
	// AttrConsolidateNoop is true when the pass changed nothing: no add, no
	// update, no supersede. A target whose passes are all no-ops but whose lag
	// keeps growing is the signature of a pass that runs and accomplishes
	// nothing — invisible without this bit, because the run itself succeeds.
	AttrConsolidateNoop = "loomcycle.memory.consolidate.noop"
	// AttrConsolidateSessionsRead is how many settled chats sat past the
	// target's watermark when the pass started — the size of the backlog it
	// was handed, not necessarily what it folded in.
	AttrConsolidateSessionsRead = "loomcycle.memory.consolidate.sessions_read"
	// AttrConsolidatePendingDrained is how many queue rows the pass acked.
	AttrConsolidatePendingDrained = "loomcycle.memory.consolidate.pending_drained"
	// AttrConsolidateCountsTruncated is set when the target holds more keys
	// than the observation window, so the add/update/supersede counts would be
	// wrong. They are OMITTED in that case rather than under-reported: a
	// truncated count that looks plausible is worse than a missing one.
	AttrConsolidateCountsTruncated = "loomcycle.memory.consolidate.counts_truncated"

	// AttrConsolidateWatermarkLagMs is how far behind now() the target's
	// watermark sits AFTER the pass — the single most useful operator signal
	// for "is consolidation keeping up". A lag that grows without bound means
	// that target is stuck: the pass is firing but the watermark is not moving,
	// which is precisely the livelock the ascending scan and the iteration cap
	// are designed to prevent. 0 on a target that has never advanced (there is
	// no meaningful lag against a watermark that was never set).
	AttrConsolidateWatermarkLagMs = "loomcycle.memory.consolidate.watermark_lag_ms"
)

// EventConsolidateWatermarkLag carries the post-pass watermark lag as a span
// event so a downstream connector can derive a GAUGE from it. The lag is a
// per-target level, not a count, and the in-process /metrics endpoint is
// substrate-only by architectural lock — so the gauge is materialised
// downstream from this event, exactly as the deadlink counter is.
const EventConsolidateWatermarkLag = "loomcycle.memory.consolidate.watermark_lag"

// MemoryConsolidateAttrs identifies the target a pass is about to consolidate.
// Empty fields are skipped so a trace never shows blank values.
//
// NO transcript content, NO memory fact text, NO recall query, and no
// credential ever appears here — a consolidation trace is about the SHAPE of a
// pass (how much moved, how far behind it is), and the whole point of the
// pipeline is that the facts themselves are private to the target's scope.
type MemoryConsolidateAttrs struct {
	Scope     string // memory scope — "user" for every fan-out target today
	ScopeID   string // the target's scope id (its user id)
	AgentName string // the consolidator agent the pass runs
	Tier      string // the user tier the pass resolves its model through
}

// RecordMemoryConsolidate opens loomcycle.memory.consolidate for one pass.
// The returned ctx MUST be the one handed to the run, so the run's own spans
// nest under this one. Caller defers span.End() and calls
// SetMemoryConsolidateResult once the outcome is observed.
func RecordMemoryConsolidate(ctx context.Context, a MemoryConsolidateAttrs) (context.Context, trace.Span) {
	kvs := make([]attribute.KeyValue, 0, 4)
	if a.Scope != "" {
		kvs = append(kvs, attribute.String(AttrMemoryScope, a.Scope))
	}
	if a.ScopeID != "" {
		kvs = append(kvs, attribute.String(AttrUserID, a.ScopeID))
	}
	if a.AgentName != "" {
		kvs = append(kvs, attribute.String(AttrAgentName, a.AgentName))
	}
	if a.Tier != "" {
		kvs = append(kvs, attribute.String(AttrTier, a.Tier))
	}
	return Tracer().Start(ctx, SpanMemoryConsolidate, trace.WithAttributes(kvs...))
}

// ConsolidateOutcome is what the RUNTIME can observe about a finished pass, as
// opposed to what the pass says about itself. Every field here is derived from
// store state the runtime read before and after (row sets, cursor position,
// queue depth) or from the loop's own usage events — never from the model's
// report, which is prose and would make the metrics a measure of phrasing.
type ConsolidateOutcome struct {
	Added          int
	Updated        int
	Superseded     int
	SessionsRead   int
	PendingDrained int
	// CountsTruncated suppresses Added/Updated/Superseded (see
	// AttrConsolidateCountsTruncated).
	CountsTruncated bool
	// WatermarkLag is now() minus the post-pass watermark. Zero when the target
	// has never advanced.
	WatermarkLag time.Duration
	// Provider / Model are the identities that ACTUALLY served the pass, taken
	// from the loop's usage events (post-fallback), so they cannot drift from
	// what ran.
	Provider string
	Model    string
	// Err marks the span as failed. A pass that errored must not read as a
	// success in traces, or the derived error series silently under-reports.
	Err error
}

// SetMemoryConsolidateResult stamps a finished pass's observed outcome on the
// span, records the watermark-lag event, and marks the span errored when the
// pass failed. No-op on a nil / non-recording span.
func SetMemoryConsolidateResult(span trace.Span, out ConsolidateOutcome) {
	if span == nil || !span.IsRecording() {
		return
	}
	kvs := []attribute.KeyValue{
		attribute.Int(AttrConsolidateSessionsRead, out.SessionsRead),
		attribute.Int(AttrConsolidatePendingDrained, out.PendingDrained),
		attribute.Int64(AttrConsolidateWatermarkLagMs, out.WatermarkLag.Milliseconds()),
	}
	if out.CountsTruncated {
		kvs = append(kvs, attribute.Bool(AttrConsolidateCountsTruncated, true))
	} else {
		kvs = append(kvs,
			attribute.Int(AttrConsolidateAdded, out.Added),
			attribute.Int(AttrConsolidateUpdated, out.Updated),
			attribute.Int(AttrConsolidateSuperseded, out.Superseded),
			// noop is only meaningful when the counts are trustworthy.
			attribute.Bool(AttrConsolidateNoop, out.Added == 0 && out.Updated == 0 && out.Superseded == 0),
		)
	}
	if out.Provider != "" {
		kvs = append(kvs, attribute.String(AttrProvider, out.Provider))
	}
	if out.Model != "" {
		kvs = append(kvs, attribute.String(AttrModel, out.Model))
	}
	span.SetAttributes(kvs...)

	span.AddEvent(EventConsolidateWatermarkLag, trace.WithAttributes(
		attribute.Int64(AttrConsolidateWatermarkLagMs, out.WatermarkLag.Milliseconds())))

	if out.Err != nil {
		SetSpanError(span, out.Err)
	}
}

// RecordMemorySearch opens loomcycle.memory.search for one retrieval; the span
// duration IS the retrieval latency. `backend` labels the series. Caller defers
// span.End() and calls SetMemorySearchResult once mode/top_k/dead-link counts
// are known.
func RecordMemorySearch(ctx context.Context, backend string) (context.Context, trace.Span) {
	var kvs []attribute.KeyValue
	if backend != "" {
		kvs = append(kvs, attribute.String(AttrMemoryBackend, backend))
	}
	return Tracer().Start(ctx, SpanMemorySearch, trace.WithAttributes(kvs...))
}

// SetMemorySearchResult stamps the retrieval's mode + top_k on the span, and —
// when the dead-link guard dropped hits — records the count as both an attribute
// and a span event (the loomcycle.memory.deadlink.dropped counter source).
// No-op on a nil / non-recording span.
func SetMemorySearchResult(span trace.Span, mode string, topK, deadlinkDropped int) {
	if span == nil || !span.IsRecording() {
		return
	}
	kvs := []attribute.KeyValue{attribute.Int(AttrMemoryTopK, topK)}
	if mode != "" {
		kvs = append(kvs, attribute.String(AttrMemoryMode, mode))
	}
	if deadlinkDropped > 0 {
		kvs = append(kvs, attribute.Int(AttrDeadlinkDropped, deadlinkDropped))
	}
	span.SetAttributes(kvs...)
	if deadlinkDropped > 0 {
		span.AddEvent(EventDeadlinkDropped,
			trace.WithAttributes(attribute.Int(AttrDeadlinkDropped, deadlinkDropped)))
	}
}

// RecordDeadlinkDropped records a dead-link drop count on `span` (attribute +
// event) when n > 0 — for call sites that ride an enclosing span rather than
// opening their own memory.search span (the help query path rides the
// loomcycle.tool.call span). No-op when n <= 0 or the span isn't recording.
func RecordDeadlinkDropped(span trace.Span, n int) {
	if span == nil || n <= 0 || !span.IsRecording() {
		return
	}
	span.SetAttributes(attribute.Int(AttrDeadlinkDropped, n))
	span.AddEvent(EventDeadlinkDropped, trace.WithAttributes(attribute.Int(AttrDeadlinkDropped, n)))
}

// RecordDeadlinkDroppedCtx is RecordDeadlinkDropped against the current span on
// ctx — for callers (the help query path) that hold a ctx but not the span.
func RecordDeadlinkDroppedCtx(ctx context.Context, n int) {
	RecordDeadlinkDropped(trace.SpanFromContext(ctx), n)
}

// RecordHelpReconcile emits a one-shot loomcycle.help.reconcile span carrying
// the boot-reconcile outcome (RFC BL PR5 ReconcileStats). Opened + ended here
// because reconcile runs once at boot, off any request span. The counts land
// as attributes so a downstream connector derives written/pruned/unchanged/
// failed counters + a degraded gauge.
func RecordHelpReconcile(ctx context.Context, written, pruned, unchanged, failed int, degraded bool) {
	_, span := Tracer().Start(ctx, SpanHelpReconcile)
	span.SetAttributes(
		attribute.Int(AttrHelpWritten, written),
		attribute.Int(AttrHelpPruned, pruned),
		attribute.Int(AttrHelpUnchanged, unchanged),
		attribute.Int(AttrHelpFailed, failed),
		attribute.Bool(AttrHelpDegraded, degraded),
	)
	span.End()
}

// SetSpanError marks a span as failed with the given error. The message
// is truncated to a sane length so a noisy provider error doesn't blow
// up Jaeger storage.
func SetSpanError(span trace.Span, err error) {
	if span == nil || err == nil || !span.IsRecording() {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, truncate(err.Error(), 500))
}

// SetSpanErrorMessage marks a span as failed with a fixed message — no
// `err` value. Used for non-error-typed failures (HTTP non-2xx, tool
// IsError=true).
func SetSpanErrorMessage(span trace.Span, msg string) {
	if span == nil || msg == "" || !span.IsRecording() {
		return
	}
	span.SetStatus(codes.Error, truncate(msg, 500))
}

// providerOverrideKey is a ctx key that a wrapping driver can set to
// rename the provider attribute the inner driver stamps on its
// per-attempt span. Today only DeepSeek uses it: DeepSeek wraps the
// OpenAI driver, but Jaeger operators filter on
// `loomcycle.provider="deepseek"` rather than "openai" to find
// DeepSeek calls — the override carries the intent through without
// duplicating the span (which would mismeasure streaming latency
// because the outer wrapping returns before the channel is drained).
type providerOverrideKey struct{}

// WithProviderOverride returns ctx with a provider-name override set.
// The inner driver's RecordProviderCall consults this via
// ProviderOverride and uses the override instead of its hardcoded
// driver name.
func WithProviderOverride(ctx context.Context, provider string) context.Context {
	if provider == "" {
		return ctx
	}
	return context.WithValue(ctx, providerOverrideKey{}, provider)
}

// ProviderOverride returns the provider override stored on ctx, if
// any. Empty string means no override — use the driver's own name.
func ProviderOverride(ctx context.Context) string {
	if v, ok := ctx.Value(providerOverrideKey{}).(string); ok {
		return v
	}
	return ""
}

// ParseMCPToolName splits dispatched MCP tool names of the form
// `mcp__<server>__<tool>` into (server, tool). Returns ("","") when
// the name doesn't match. Mirrors the canonical naming scheme in
// internal/tools/mcp/pool.go:NewTool.
func ParseMCPToolName(name string) (server, tool string) {
	const prefix = "mcp__"
	if !strings.HasPrefix(name, prefix) {
		return "", ""
	}
	rest := name[len(prefix):]
	sep := strings.Index(rest, "__")
	if sep < 0 {
		return "", ""
	}
	return rest[:sep], rest[sep+2:]
}

// truncate returns s truncated to at most `max` bytes, with `…`
// appended. Backs the cut to the nearest preceding rune boundary so
// the result is always valid UTF-8 — important because provider
// error messages may contain non-ASCII text (DeepSeek's Chinese
// status messages, Anthropic's Unicode JSON error bodies). Slicing a
// string mid-rune produces malformed UTF-8 that breaks OTLP/protobuf
// serialization or downstream Jaeger/Tempo rendering.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max] + "…"
}
