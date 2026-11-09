package otel

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	genai "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
)

// SpanModelCall is one model call as the loop sees it: from just before the
// provider is called until its stream ends (the done event, an error, or a
// cancel). The provider.call spans of its HTTP attempts are its children. It
// exists because those attempt spans end when the response headers arrive,
// so they cannot carry what only the end of the stream knows: usage, the
// finish reason, the time to the first token.
const SpanModelCall = "loomcycle.model.call"

const (
	// AttrTTFTMs is the time from the call to its first streamed text,
	// thinking or tool event. Absent when nothing streamed.
	AttrTTFTMs = "loomcycle.ttft_ms"
	// AttrCacheWriteTokens is the prompt tokens written to the provider's
	// cache by the call.
	AttrCacheWriteTokens = "loomcycle.cache_write_tokens"
	// AttrCostUSD is the call's cost in US dollars, from the provider's own
	// report or the operator's pricing table. Absent when unpriced or priced
	// in another currency.
	AttrCostUSD = "loomcycle.cost_usd"
	// AttrFallbackFrom is on the first call after a provider fallback: the
	// "provider/model" whose failed call the run moved away from. With the
	// span's own provider and model it is the whole hop.
	AttrFallbackFrom = "loomcycle.fallback_from"
)

// ModelCallAttrs identifies a model call before it is made.
type ModelCallAttrs struct {
	Provider string
	Model    string
	Tier     string
	Effort   string
	// Request parameters, recorded only when set.
	Temperature *float64
	TopP        *float64
	MaxTokens   int
	// FallbackFrom is the "provider/model" the previous call failed over
	// from, when this call is the first after a fallback.
	FallbackFrom string
}

// RecordModelCall opens loomcycle.model.call. The returned ctx must be the
// one the provider is called with, so its attempt spans nest under this one.
// The caller ends the span on every exit (SetModelCallResult, then End).
func RecordModelCall(ctx context.Context, a ModelCallAttrs) (context.Context, trace.Span) {
	kvs := make([]attribute.KeyValue, 0, 12)
	if a.Provider != "" {
		kvs = append(kvs, attribute.String(AttrProvider, a.Provider))
	}
	if a.Model != "" {
		kvs = append(kvs, attribute.String(AttrModel, a.Model))
	}
	if a.Tier != "" {
		kvs = append(kvs, attribute.String(AttrTier, a.Tier))
	}
	if a.Effort != "" {
		kvs = append(kvs, attribute.String(AttrEffort, a.Effort))
	}
	if a.FallbackFrom != "" {
		kvs = append(kvs, attribute.String(AttrFallbackFrom, a.FallbackFrom))
	}
	kvs = append(kvs, genAIOperationChat)
	kvs = genAIString(genai.GenAIProviderNameKey, GenAIProviderName(a.Provider), kvs)
	kvs = genAIString(genai.GenAIRequestModelKey, a.Model, kvs)
	if a.Temperature != nil {
		kvs = append(kvs, genai.GenAIRequestTemperatureKey.Float64(*a.Temperature))
	}
	if a.TopP != nil {
		kvs = append(kvs, genai.GenAIRequestTopPKey.Float64(*a.TopP))
	}
	if a.MaxTokens > 0 {
		kvs = append(kvs, genai.GenAIRequestMaxTokensKey.Int(a.MaxTokens))
	}
	ctx, span := Tracer().Start(ctx, SpanModelCall, trace.WithAttributes(withTenantAttr(ctx, kvs)...))
	if st := meters.Load(); st != nil {
		return ctx, &modelSpan{Span: span, st: st, provider: a.Provider, model: a.Model, start: time.Now()}
	}
	return ctx, span
}

// ModelCallResult is how a model call ended.
type ModelCallResult struct {
	// Usage is set when the stream finished with the driver's done event.
	Usage *ModelUsage
	// FinishReason is the done event's stop reason.
	FinishReason string
	// TTFT is the time to the first streamed event; 0 = nothing streamed.
	TTFT time.Duration
	// Cancelled: the call was stopped (run or turn cancel), not failed.
	Cancelled bool
	// Err is the failure, recorded as the span's error event. ErrMessage,
	// when set instead, is recorded as the status alone — for a run whose
	// error text must not leave the box.
	Err        error
	ErrMessage string
}

// ModelUsage is what the done event reported.
type ModelUsage struct {
	ResponseModel    string
	ResponseID       string
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	// CostUSD is the call's cost when it is priced in US dollars.
	CostUSD   float64
	CostKnown bool
}

// SetModelCallResult stamps how the call ended. A cancelled call is an error
// with the description "cancelled": it did not produce its turn. It also
// records the call's duration and time to first token in the model metrics,
// on a span that does not record too; the span attributes are a no-op on a
// nil / non-recording span.
func SetModelCallResult(span trace.Span, r ModelCallResult) {
	if ms, ok := span.(*modelSpan); ok {
		ms.recordResult(r)
	}
	if span == nil || !span.IsRecording() {
		return
	}
	var kvs []attribute.KeyValue
	if r.TTFT > 0 {
		kvs = append(kvs, attribute.Int64(AttrTTFTMs, r.TTFT.Milliseconds()))
	}
	if r.FinishReason != "" {
		kvs = append(kvs, attribute.String(AttrStopReason, r.FinishReason),
			genai.GenAIResponseFinishReasonsKey.StringSlice([]string{r.FinishReason}))
	}
	if u := r.Usage; u != nil {
		kvs = append(kvs,
			attribute.Int(AttrInputTokens, u.InputTokens),
			attribute.Int(AttrOutputTokens, u.OutputTokens),
			attribute.Int(AttrCacheReadTokens, u.CacheReadTokens),
			attribute.Int(AttrCacheWriteTokens, u.CacheWriteTokens),
			genai.GenAIUsageInputTokensKey.Int(u.InputTokens),
			genai.GenAIUsageOutputTokensKey.Int(u.OutputTokens),
			genai.GenAIUsageCacheReadInputTokensKey.Int(u.CacheReadTokens),
			genai.GenAIUsageCacheCreationInputTokensKey.Int(u.CacheWriteTokens),
		)
		kvs = genAIString(genai.GenAIResponseModelKey, u.ResponseModel, kvs)
		kvs = genAIString(genai.GenAIResponseIDKey, u.ResponseID, kvs)
		if u.CostKnown {
			kvs = append(kvs, attribute.Float64(AttrCostUSD, u.CostUSD))
		}
	}
	if len(kvs) > 0 {
		span.SetAttributes(kvs...)
	}
	switch {
	case r.Cancelled:
		span.SetAttributes(genai.ErrorTypeKey.String(ErrorTypeCancelled))
		span.SetStatus(codes.Error, "cancelled")
	case r.ErrMessage != "":
		SetSpanErrorMessage(span, r.ErrMessage)
	case r.Err != nil:
		SetSpanError(span, r.Err)
	}
}
