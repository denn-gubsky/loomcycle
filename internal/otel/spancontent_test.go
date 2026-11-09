package otel_test

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"sync"
	"testing"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// attrOf returns the value of key on span, and whether the span carries it.
func attrOf(s tracetest.SpanStub, key string) (attribute.Value, bool) {
	for _, kv := range s.Attributes {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func spanNamed(t *testing.T, spans tracetest.SpanStubs, name string) tracetest.SpanStub {
	t.Helper()
	for _, s := range spans {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no %s span among %d", name, len(spans))
	return tracetest.SpanStub{}
}

func TestRecorders_StampTenantOnEverySpanOfARun(t *testing.T) {
	exp := withInMemoryExporter(t)
	ctx, run := lcotel.RecordRunStart(context.Background(), lcotel.RunStartAttrs{RunID: "r1", Tenant: "acme"})
	ictx, iter := lcotel.RecordIteration(ctx, 1)
	_, prov := lcotel.RecordProviderCall(ictx, lcotel.ProviderCallAttrs{Provider: "anthropic"})
	tctx, tool := lcotel.RecordToolCall(ictx, lcotel.ToolCallAttrs{Name: "mcp__srv__x"})
	_, mcp := lcotel.RecordMCPCall(tctx, "srv", "x")
	_, mem := lcotel.RecordMemorySearch(tctx, "inprocess")
	// A sub-run started under the parent's span names no tenant of its own.
	_, sub := lcotel.RecordRunStart(tctx, lcotel.RunStartAttrs{RunID: "r2"})
	for _, s := range []trace.Span{sub, mem, mcp, tool, prov, iter, run} {
		s.End()
	}
	_, cons := lcotel.RecordMemoryConsolidate(context.Background(), lcotel.MemoryConsolidateAttrs{Tenant: "beta", Scope: "user"})
	cons.End()

	spans := exp.GetSpans()
	if len(spans) != 8 {
		t.Fatalf("got %d spans, want 8", len(spans))
	}
	for _, s := range spans {
		want := "acme"
		if s.Name == lcotel.SpanMemoryConsolidate {
			want = "beta"
		}
		v, ok := attrOf(s, lcotel.AttrTenant)
		if !ok || v.AsString() != want {
			t.Errorf("%s: %s = %q (present %v), want %q", s.Name, lcotel.AttrTenant, v.AsString(), ok, want)
		}
	}
}

func TestRecorders_OmitTenantOutsideATenantsWork(t *testing.T) {
	exp := withInMemoryExporter(t)
	_, run := lcotel.RecordRunStart(context.Background(), lcotel.RunStartAttrs{RunID: "r1"})
	run.End()
	_, tool := lcotel.RecordToolCall(context.Background(), lcotel.ToolCallAttrs{Name: "Read"})
	tool.End()
	for _, s := range exp.GetSpans() {
		if v, ok := attrOf(s, lcotel.AttrTenant); ok {
			t.Errorf("%s carries %s=%q with no tenant anywhere", s.Name, lcotel.AttrTenant, v.AsString())
		}
	}
}

// recordingSampler keeps the start attributes it was asked about.
type recordingSampler struct {
	mu    sync.Mutex
	attrs map[string][]attribute.KeyValue
}

func (r *recordingSampler) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	r.mu.Lock()
	r.attrs[p.Name] = p.Attributes
	r.mu.Unlock()
	return sdktrace.SamplingResult{Decision: sdktrace.RecordAndSample}
}

func (r *recordingSampler) Description() string { return "recording" }

func TestRecordRunStart_TenantIsAStartAttribute(t *testing.T) {
	rs := &recordingSampler{attrs: map[string][]attribute.KeyValue{}}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(rs))
	t.Cleanup(lcotel.SetTracerProviderForTest(tp))
	_, span := lcotel.RecordRunStart(context.Background(), lcotel.RunStartAttrs{RunID: "r1", Tenant: "acme"})
	span.End()
	for _, kv := range rs.attrs[lcotel.SpanRun] {
		if string(kv.Key) == lcotel.AttrTenant && kv.Value.AsString() == "acme" {
			return
		}
	}
	t.Fatalf("the sampler saw %v for the run span, without %s=acme", rs.attrs[lcotel.SpanRun], lcotel.AttrTenant)
}

func runSpanUserAttrs(t *testing.T, tenant, user string) (hash, raw string) {
	t.Helper()
	exp := withInMemoryExporter(t)
	_, span := lcotel.RecordRunStart(context.Background(), lcotel.RunStartAttrs{RunID: "r1", Tenant: tenant, UserID: user})
	span.End()
	s := exp.GetSpans()[0]
	if v, ok := attrOf(s, lcotel.AttrUserHash); ok {
		hash = v.AsString()
	}
	if v, ok := attrOf(s, lcotel.AttrUserID); ok {
		raw = v.AsString()
	}
	return hash, raw
}

var testUserKey = []byte("0123456789abcdef0123456789abcdef")

func TestRecordRunStart_HashesTheUserWithoutTheRawID(t *testing.T) {
	t.Cleanup(lcotel.SetUserIDPolicyForTest(testUserKey, false))
	hash, raw := runSpanUserAttrs(t, "acme", "alice")
	if raw != "" {
		t.Errorf("%s = %q emitted without the opt-in", lcotel.AttrUserID, raw)
	}
	if want := lcotel.UserHash(testUserKey, "acme", "alice"); hash != want || len(hash) != 16 {
		t.Errorf("%s = %q, want %q (16 hex characters)", lcotel.AttrUserHash, hash, want)
	}
	if again, _ := runSpanUserAttrs(t, "acme", "alice"); again != hash {
		t.Errorf("the hash is not stable: %q then %q", hash, again)
	}
	if other, _ := runSpanUserAttrs(t, "beta", "alice"); other == hash {
		t.Error("the same user id in two tenants hashed the same")
	}
}

func TestRecordRunStart_RawUserIDOnlyWhenOptedInOrUnkeyed(t *testing.T) {
	t.Cleanup(lcotel.SetUserIDPolicyForTest(testUserKey, true))
	if hash, raw := runSpanUserAttrs(t, "acme", "alice"); hash == "" || raw != "alice" {
		t.Errorf("opted in: hash %q raw %q, want both", hash, raw)
	}
	lcotel.SetUserIDPolicyForTest(nil, false)
	if hash, raw := runSpanUserAttrs(t, "acme", "alice"); hash != "" || raw != "alice" {
		t.Errorf("no key: hash %q raw %q, want the raw id alone", hash, raw)
	}
}

func TestRecordMemoryConsolidate_HashesTheTargetUser(t *testing.T) {
	t.Cleanup(lcotel.SetUserIDPolicyForTest(testUserKey, false))
	exp := withInMemoryExporter(t)
	_, span := lcotel.RecordMemoryConsolidate(context.Background(), lcotel.MemoryConsolidateAttrs{Tenant: "acme", Scope: "user", ScopeID: "alice"})
	span.End()
	s := exp.GetSpans()[0]
	if v, ok := attrOf(s, lcotel.AttrUserID); ok {
		t.Errorf("consolidate span carries the raw %s=%q", lcotel.AttrUserID, v.AsString())
	}
	if v, _ := attrOf(s, lcotel.AttrUserHash); v.AsString() != lcotel.UserHash(testUserKey, "acme", "alice") {
		t.Errorf("%s = %q", lcotel.AttrUserHash, v.AsString())
	}
}

// Pinned outside Go (HMAC-SHA256 over "acme|alice", first 16 hex): a change
// would make every exported hash stop matching the CLI's.
func TestUserHash_MatchesTheHMACKnownAnswer(t *testing.T) {
	key, _ := hex.DecodeString("6888a893992739563b4d15a9e417320e7dd2ec2748da497abd27ee9cbae9b2f4")
	if got := lcotel.UserHash(key, "acme", "alice"); got != "449b272e5974ee6a" {
		t.Fatalf("UserHash = %s", got)
	}
}

// wantAttrs fails for every key in want the span lacks or carries another
// value for; values compare in their emitted string form.
func wantAttrs(t *testing.T, s tracetest.SpanStub, want map[string]string) {
	t.Helper()
	for k, w := range want {
		v, ok := attrOf(s, k)
		if !ok {
			t.Errorf("%s: %s missing", s.Name, k)
			continue
		}
		if got := v.Emit(); got != w {
			t.Errorf("%s: %s = %q, want %q", s.Name, k, got, w)
		}
	}
}

func TestRecordRunStart_DualEmitsGenAIAgentAttributes(t *testing.T) {
	exp := withInMemoryExporter(t)
	_, span := lcotel.RecordRunStart(context.Background(), lcotel.RunStartAttrs{
		RunID: "r1", AgentID: "ag_1", AgentName: "researcher", Tenant: "acme", SessionID: "sess_9",
	})
	span.End()
	wantAttrs(t, exp.GetSpans()[0], map[string]string{
		lcotel.AttrTenant:        "acme",
		lcotel.AttrAgentName:     "researcher",
		lcotel.AttrAgentID:       "ag_1",
		"gen_ai.operation.name":  "invoke_agent",
		"gen_ai.agent.name":      "researcher",
		"gen_ai.agent.id":        "ag_1",
		"gen_ai.conversation.id": "sess_9",
	})
}

func TestRecordToolCall_DualEmitsGenAIToolAttributes(t *testing.T) {
	exp := withInMemoryExporter(t)
	ctx := lcotel.WithTenant(context.Background(), "acme")
	_, span := lcotel.RecordToolCall(ctx, lcotel.ToolCallAttrs{Name: "Read", CallID: "toolu_1"})
	span.End()
	wantAttrs(t, exp.GetSpans()[0], map[string]string{
		lcotel.AttrTenant:       "acme",
		lcotel.AttrTool:         "Read",
		"gen_ai.operation.name": "execute_tool",
		"gen_ai.tool.name":      "Read",
		"gen_ai.tool.call.id":   "toolu_1",
	})
}

func TestGenAIProviderName_MapsToTheWellKnownValues(t *testing.T) {
	for in, want := range map[string]string{
		"anthropic": "anthropic", "openai": "openai", "deepseek": "deepseek",
		"gemini": "gcp.gemini", "bedrock": "aws.bedrock", "ollama": "ollama",
		"ollama-local": "ollama", "my-vllm": "my-vllm",
	} {
		if got := lcotel.GenAIProviderName(in); got != want {
			t.Errorf("GenAIProviderName(%q) = %q, want %q", in, got, want)
		}
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestSetProviderResponse_ClassifiesTheFailure(t *testing.T) {
	cases := []struct {
		name   string
		status int
		err    error
		want   string // error.type; "" = none
	}{
		{"ok", 200, nil, ""},
		{"rate limited", 429, nil, lcotel.ErrorTypeRateLimited},
		{"anthropic overloaded", 529, nil, lcotel.ErrorTypeOverloaded},
		{"unavailable", 503, nil, lcotel.ErrorTypeOverloaded},
		{"gateway timeout", 504, nil, lcotel.ErrorTypeTimeout},
		{"not found", 404, nil, lcotel.ErrorTypeClientError},
		{"internal", 500, nil, lcotel.ErrorTypeServerError},
		{"cancelled", 0, context.Canceled, lcotel.ErrorTypeCancelled},
		{"deadline", 0, context.DeadlineExceeded, lcotel.ErrorTypeTimeout},
		{"net timeout", 0, timeoutErr{}, lcotel.ErrorTypeTimeout},
		{"refused", 0, errors.New("dial tcp: connection refused"), lcotel.ErrorTypeNetwork},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exp := withInMemoryExporter(t)
			_, span := lcotel.RecordProviderCall(context.Background(), lcotel.ProviderCallAttrs{Provider: "p"})
			var resp *http.Response
			if tc.err == nil {
				resp = &http.Response{StatusCode: tc.status, Status: http.StatusText(tc.status)}
			}
			lcotel.SetProviderResponse(span, resp, tc.err)
			span.End()
			s := exp.GetSpans()[0]
			got, _ := attrOf(s, "error.type")
			if got.AsString() != tc.want {
				t.Errorf("error.type = %q, want %q", got.AsString(), tc.want)
			}
			if isErr := s.Status.Code == codes.Error; isErr != (tc.want != "") {
				t.Errorf("span status = %v with error.type %q", s.Status.Code, tc.want)
			}
			if code, ok := attrOf(s, "http.response.status_code"); (tc.err == nil) != ok || (ok && code.AsInt64() != int64(tc.status)) {
				t.Errorf("http.response.status_code = %v (present %v)", code.Emit(), ok)
			}
		})
	}
}

// A cancelled run did not produce its answer: an error described
// "cancelled", so a collector's keep-errors policy sees it.
func TestSetRunDone_ACancelledRunIsAnErrorDescribedCancelled(t *testing.T) {
	exp := withInMemoryExporter(t)
	_, span := lcotel.RecordRunStart(context.Background(), lcotel.RunStartAttrs{RunID: "r1"})
	lcotel.SetRunDone(span, lcotel.RunDoneAttrs{StopReason: "cancelled", Cancelled: true})
	span.End()
	s := exp.GetSpans()[0]
	if s.Status.Code != codes.Error || s.Status.Description != "cancelled" {
		t.Errorf("status = %v %q, want Error \"cancelled\"", s.Status.Code, s.Status.Description)
	}
	if len(s.Events) != 0 {
		t.Errorf("a cancel recorded %d error events; it is not an exception", len(s.Events))
	}
}

// sampledRunSpan reports whether a root run span of tenant is sampled under
// sampler, starting from parent (a context.Background() for a root).
func sampledRunSpan(t *testing.T, sampler sdktrace.Sampler, parent context.Context, tenant string) bool {
	t.Helper()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sampler))
	defer func() { _ = tp.Shutdown(context.Background()) }()
	defer lcotel.SetTracerProviderForTest(tp)()
	_, span := lcotel.RecordRunStart(parent, lcotel.RunStartAttrs{RunID: "r1", Tenant: tenant})
	defer span.End()
	return span.SpanContext().IsSampled()
}

func TestTenantSampler_RatioDecidesARootRunOfItsTenant(t *testing.T) {
	ratios := map[string]float64{"acme": 1, "beta": 0}
	keepAll := lcotel.NewTenantSampler(sdktrace.ParentBased(sdktrace.AlwaysSample()), ratios)
	keepNone := lcotel.NewTenantSampler(sdktrace.ParentBased(sdktrace.NeverSample()), ratios)

	if sampledRunSpan(t, keepAll, context.Background(), "beta") {
		t.Error("ratio 0 kept a root run of its tenant under an always-on base")
	}
	if !sampledRunSpan(t, keepNone, context.Background(), "acme") {
		t.Error("ratio 1 dropped a root run of its tenant under an always-off base")
	}
	if sampledRunSpan(t, keepNone, context.Background(), "gamma") || !sampledRunSpan(t, keepAll, context.Background(), "gamma") {
		t.Error("a tenant with no ratio did not follow the base sampler")
	}
}

func TestTenantSampler_ASampledParentWins(t *testing.T) {
	s := lcotel.NewTenantSampler(sdktrace.ParentBased(sdktrace.NeverSample()), map[string]float64{"beta": 0})
	sampled := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1}, SpanID: trace.SpanID{1}, TraceFlags: trace.FlagsSampled, Remote: true,
	})
	parent := trace.ContextWithRemoteSpanContext(context.Background(), sampled)
	if !sampledRunSpan(t, s, parent, "beta") {
		t.Error("a sampled caller's trace was dropped by the tenant's ratio 0")
	}
}

func TestParseTenantSamplerRatios_ValidatesTenantsAndRatios(t *testing.T) {
	got, err := lcotel.ParseTenantSamplerRatios(" acme=1, beta=0.1 ")
	if err != nil || len(got) != 2 || got["acme"] != 1 || got["beta"] != 0.1 {
		t.Fatalf("parse = %v, %v", got, err)
	}
	for _, bad := range []string{"acme", "acme=x", "acme=1.5", "acme=-0.1", "ac/me=1", "acme=1,acme=0", "=1", "acme=NaN"} {
		if _, err := lcotel.ParseTenantSamplerRatios(bad); err == nil {
			t.Errorf("%q parsed without an error", bad)
		}
	}
	merged := lcotel.MergeTenantSamplerRatios(map[string]float64{"acme": 0.5, "beta": 1}, map[string]float64{"acme": 0})
	if merged["acme"] != 0 || merged["beta"] != 1 {
		t.Errorf("merge = %v, want the environment's acme=0 over the yaml, beta kept", merged)
	}
}
