package otel

import (
	"go.opentelemetry.io/otel/attribute"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	genai "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
)

// The tenant's copy of a span. A tenant's destination receives the spans of
// its own work with a minimal resource and only the attributes classified
// below as the tenant's to see; everything else on the operator's copy (its
// resource, its infrastructure attributes, error text that may name its hosts)
// stays with the operator.

// AttrNoTrace marks a span opened for a no-trace run (one whose content is
// stored nowhere). It is stamped only while tenant export is on, beside the
// tenant attribute, so the router can tell such a span apart at its end, when
// the context it was opened under is gone. A marked span is never routed to a
// tenant's destination.
const AttrNoTrace = "loomcycle.no_trace"

// tenantAttrPolicy classifies every attribute key loomcycle writes on a span
// or a span event: true is kept on the tenant's copy, false is dropped. A key
// not listed is dropped, and TestTenantAttrPolicy_ClassifiesEveryEmittedKey
// fails on a key the source writes that is not listed, so a new attribute is
// left out of tenant exports until someone decides it belongs there.
var tenantAttrPolicy = map[string]bool{
	// Who and what: the tenant's own runs, agents and users.
	AttrTenant:        true,
	AttrRunID:         true,
	AttrAgentID:       true,
	AttrAgentName:     true,
	AttrParentAgentID: true,
	AttrIteration:     true,
	// The keyed hash, not the raw id: a raw user id is on the operator's copy
	// only when the operator opted in for its own backend.
	AttrUserHash: true,
	AttrUserID:   false,

	// Model calls: provider, model, routing tier and usage.
	AttrProvider:         true,
	AttrProviderKind:     true,
	AttrModel:            true,
	AttrTier:             true,
	AttrEffort:           true,
	AttrModelPurpose:     true,
	AttrFallbackFrom:     true,
	AttrInputTokens:      true,
	AttrOutputTokens:     true,
	AttrCacheReadTokens:  true,
	AttrCacheWriteTokens: true,
	AttrCostUSD:          true,
	AttrStopReason:       true,
	AttrTTFTMs:           true,
	AttrAttempt:          true,
	AttrRetryAfterMs:     true,
	// The source hash of a code agent identifies the operator's static code.
	AttrProviderCodeHash: false,
	// Waits in the operator's lanes and concurrency semaphore describe the
	// deployment's load, not the tenant's run.
	AttrLaneWaitMs:  false,
	AttrQueueWaitMs: false,

	// Tools.
	AttrTool:        true,
	AttrToolIsError: true,
	AttrMCPServer:   true,
	AttrMCPTool:     true,

	// The tenant's memory work.
	AttrMemoryBackend:              true,
	AttrMemoryMode:                 true,
	AttrMemoryTopK:                 true,
	AttrMemoryScope:                true,
	AttrMemoryFilterIDs:            true,
	AttrMemoryFilterExcludeIDs:     true,
	AttrDeadlinkDropped:            true,
	AttrConsolidateAdded:           true,
	AttrConsolidateUpdated:         true,
	AttrConsolidateSuperseded:      true,
	AttrConsolidateNoop:            true,
	AttrConsolidateSessionsRead:    true,
	AttrConsolidatePendingDrained:  true,
	AttrConsolidateCountsTruncated: true,
	AttrConsolidateWatermarkLagMs:  true,

	// Context compaction (an event on the run span).
	"compaction.trigger":       true,
	"compaction.before_tokens": true,
	"compaction.after_tokens":  true,

	// The classified failure, never its text.
	"error.type":                true,
	"http.response.status_code": true,
	"exception.type":            true,
	// The error's own text and stack may quote the operator's hosts, paths
	// and upstream answers.
	"exception.message":    false,
	"exception.stacktrace": false,

	// GenAI semantic conventions, about the tenant's own calls.
	string(genai.GenAIOperationNameKey):                 true,
	string(genai.GenAIProviderNameKey):                  true,
	string(genai.GenAIRequestModelKey):                  true,
	string(genai.GenAIRequestTemperatureKey):            true,
	string(genai.GenAIRequestTopPKey):                   true,
	string(genai.GenAIRequestMaxTokensKey):              true,
	string(genai.GenAIResponseModelKey):                 true,
	string(genai.GenAIResponseFinishReasonsKey):         true,
	string(genai.GenAIUsageInputTokensKey):              true,
	string(genai.GenAIUsageOutputTokensKey):             true,
	string(genai.GenAIUsageCacheReadInputTokensKey):     true,
	string(genai.GenAIUsageCacheCreationInputTokensKey): true,
	string(genai.GenAIAgentIDKey):                       true,
	string(genai.GenAIAgentNameKey):                     true,
	string(genai.GenAIConversationIDKey):                true,
	string(genai.GenAIToolNameKey):                      true,
	string(genai.GenAIToolCallIDKey):                    true,
	// Captured content (content.go): on a span only when the tenant turned
	// capture on, and stripped from the operator's copy. The tenant's copy
	// admits exactly ContentAttributeKeys.
	string(genai.GenAISystemInstructionsKey): true,
	string(genai.GenAIInputMessagesKey):      true,
	string(genai.GenAIOutputMessagesKey):     true,
	string(genai.GenAIToolCallArgumentsKey):  true,
	string(genai.GenAIToolCallResultKey):     true,

	// Never on a tenant's span, listed so the census sees them decided: the
	// help reconcile is substrate work, a webhook's receive span names the
	// webhook's tenant under its own key (the router reads AttrTenant only),
	// and the no-trace marker keeps a span from being routed at all.
	AttrHelpWritten:   false,
	AttrHelpPruned:    false,
	AttrHelpUnchanged: false,
	AttrHelpFailed:    false,
	AttrHelpDegraded:  false,
	"webhook.name":    false,
	"webhook.tenant":  false,
	"webhook.verdict": false,
	AttrNoTrace:       false,
	// The operator's resource attribute; a tenant's copy carries a resource
	// of its own (tenantResource).
	"deployment.environment.name": false,
}

// tenantKeep returns the attributes of kvs the tenant may see, nil when none.
func tenantKeep(kvs []attribute.KeyValue) []attribute.KeyValue {
	var out []attribute.KeyValue
	for _, kv := range kvs {
		if tenantAttrPolicy[string(kv.Key)] {
			out = append(out, kv)
		}
	}
	return out
}

// tenantResource is the whole resource a tenant's copy carries: the service,
// its version and the tenant. Not the operator's resource, which may name its
// hosts, replicas, environment and whatever OTEL_RESOURCE_ATTRIBUTES holds.
func tenantResource(tenant, version string) *sdkresource.Resource {
	attrs := []attribute.KeyValue{semconv.ServiceName("loomcycle"), attribute.String(AttrTenant, tenant)}
	if version != "" {
		attrs = append(attrs, semconv.ServiceVersion(version))
	}
	return sdkresource.NewWithAttributes(semconv.SchemaURL, attrs...)
}

// spanRoute reads what routing needs from an ended span: its tenant and
// whether it was opened for a no-trace run.
func spanRoute(s sdktrace.ReadOnlySpan) (tenant string, noTrace bool) {
	return startRoute(s.Attributes())
}

// startRoute is spanRoute over a span's attributes, which a sampler sees as
// the start attributes of a span not yet opened.
func startRoute(kvs []attribute.KeyValue) (tenant string, noTrace bool) {
	for _, kv := range kvs {
		switch string(kv.Key) {
		case AttrTenant:
			tenant = kv.Value.AsString()
		case AttrNoTrace:
			noTrace = kv.Value.AsBool()
		}
	}
	return tenant, noTrace
}

// tenantSpan is the tenant's copy of an ended span: the span as recorded, seen
// through the resource, attributes, events, links and status the tenant may
// see. The underlying span is shared with the operator's pipeline and never
// modified.
type tenantSpan struct {
	sdktrace.ReadOnlySpan
	res    *sdkresource.Resource
	attrs  []attribute.KeyValue
	events []sdktrace.Event
	links  []sdktrace.Link
	status sdktrace.Status
}

func newTenantSpan(s sdktrace.ReadOnlySpan, res *sdkresource.Resource) tenantSpan {
	t := tenantSpan{ReadOnlySpan: s, res: res, attrs: tenantKeep(s.Attributes())}
	for _, e := range s.Events() {
		e.Attributes = tenantKeep(e.Attributes)
		t.events = append(t.events, e)
	}
	// A link's span context is kept, so the tenant's trace still points where
	// it came from; link attributes are not the tenant's to read.
	for _, l := range s.Links() {
		l.Attributes = nil
		t.links = append(t.links, l)
	}
	// The status code says the span failed; its description is error text.
	t.status = sdktrace.Status{Code: s.Status().Code}
	return t
}

func (t tenantSpan) Resource() *sdkresource.Resource  { return t.res }
func (t tenantSpan) Attributes() []attribute.KeyValue { return t.attrs }
func (t tenantSpan) Events() []sdktrace.Event         { return t.events }
func (t tenantSpan) Links() []sdktrace.Link           { return t.links }
func (t tenantSpan) Status() sdktrace.Status          { return t.status }

// SpanContext is the span's, without the marker of a trace kept only for the
// tenant: it describes the operator's sampling, not the tenant's trace.
func (t tenantSpan) SpanContext() trace.SpanContext {
	sc := t.ReadOnlySpan.SpanContext()
	return sc.WithTraceState(sc.TraceState().Delete(tenantOnlyKey))
}
