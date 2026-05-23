package otel

import (
	"context"
	"strings"

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
	AttrRunID           = "loomcycle.run_id"
	AttrAgentID         = "loomcycle.agent_id"
	AttrAgentName       = "loomcycle.agent_name"
	AttrUserID          = "loomcycle.user_id"
	AttrParentAgentID   = "loomcycle.parent_agent_id"
	AttrIteration       = "loomcycle.iteration"
	AttrProvider        = "loomcycle.provider"
	AttrModel           = "loomcycle.model"
	AttrTier            = "loomcycle.tier"
	AttrEffort          = "loomcycle.effort"
	AttrTool            = "loomcycle.tool"
	AttrMCPServer       = "loomcycle.mcp_server"
	AttrMCPTool         = "loomcycle.mcp_tool"
	AttrInputTokens     = "loomcycle.input_tokens"
	AttrOutputTokens    = "loomcycle.output_tokens"
	AttrCacheReadTokens = "loomcycle.cache_read_tokens"
	AttrStopReason      = "loomcycle.stop_reason"
	AttrToolIsError     = "loomcycle.tool_is_error"
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
	Provider string // "anthropic" | "openai" | "deepseek" | "gemini" | "ollama" | "ollama-local"
	Model    string
	Tier     string // optional — set when the agent's resolution went through a tier
	Effort   string // optional — set when the agent declared an effort hint
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

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
