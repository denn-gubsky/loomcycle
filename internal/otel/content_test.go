package otel

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// testCapture masks "s3cr3t-value" in text and arguments, as a run's redactor
// masks a resolved credential.
func testCapture(maxChars int) *ContentCapture {
	mask := func(s string) string { return strings.ReplaceAll(s, "s3cr3t-value", "[redacted:credential]") }
	return &ContentCapture{
		Tenant:     "acme",
		MaxChars:   maxChars,
		Redact:     mask,
		RedactArgs: func(_ string, b []byte) []byte { return []byte(mask(string(b))) },
	}
}

// recordCapturedRun opens a run of tenant with capture c, one model call with
// content and one tool call with content.
func recordCapturedRun(ctx context.Context, tenant string, c *ContentCapture) {
	ctx = WithContentCapture(ctx, c)
	ctx, run := RecordRunStart(ctx, RunStartAttrs{RunID: "r1", Tenant: tenant})
	mctx, mc := RecordModelCall(ctx, ModelCallAttrs{Provider: "anthropic", Model: "m"})
	SetModelInputContent(mctx, mc,
		[]ContentPart{{Type: ContentPartText, Text: "you are helpful"}},
		[]ContentMessage{{Role: "user", Parts: []ContentPart{{Type: ContentPartText, Text: "my key is s3cr3t-value"}}}})
	SetModelOutputContent(mctx, mc, []ContentMessage{{Role: "assistant", FinishReason: "stop",
		Parts: []ContentPart{{Type: ContentPartText, Text: "noted"}}}})
	mc.End()
	tctx, tool := RecordToolCall(ctx, ToolCallAttrs{Name: "HTTP"})
	SetToolCallContent(tctx, tool, "HTTP", []byte(`{"url":"https://x.example/?k=s3cr3t-value"}`), "fetched s3cr3t-value")
	tool.End()
	run.End()
}

func TestContentCapture_ReachesTenantPipelineButNeverOperatorExporter(t *testing.T) {
	clearOTELEnv(t)
	operator := newFakeCollector(t)
	tenant := newFakeCollector(t)
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", operator.srv.URL+"/v1/traces")
	prevTP, prevRouting, prevOnly := otel.GetTracerProvider(), tenantRouting.Load(), routingOnly.Load()
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		tenantRouting.Store(prevRouting)
		routingOnly.Store(prevOnly)
		stats.enabled.Store(false)
	})
	r := fastRouter(TenantRouterConfig{})
	d := dest(tenant, nil)
	d.ContentCapture = true
	r.SetDestination("acme", d)
	shutdown, err := doInit(Config{
		Enabled: true, Protocol: ProtocolHTTPProtobuf, Propagators: []string{"tracecontext"},
		SamplerName: "parentbased_always_on", SamplerArg: 1,
		MaxQueueSize: 64, MaxExportBatchSize: 64, ExportTimeout: 2 * time.Second, ScheduleDelay: 20 * time.Millisecond,
		TenantRouter: r,
	})
	if err != nil {
		t.Fatal(err)
	}

	recordCapturedRun(context.Background(), "acme", testCapture(DefaultContentMaxChars))
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the operator's spans", func() bool { return len(operator.got()) == 3 })
	waitFor(t, "the tenant's spans", func() bool { return len(tenant.got()) == 3 })

	for _, s := range operator.got() {
		for _, k := range ContentAttributeKeys {
			if v, ok := s.attrs[string(k)]; ok {
				t.Errorf("operator: %s carries %s = %q", s.name, k, v)
			}
		}
		for k, v := range s.attrs {
			if strings.Contains(v, "s3cr3t") || strings.Contains(v, "helpful") {
				t.Errorf("operator: %s attribute %s carries content %q", s.name, k, v)
			}
		}
	}
	seen := map[string]string{}
	for _, s := range tenant.got() {
		for _, k := range ContentAttributeKeys {
			if v, ok := s.attrs[string(k)]; ok {
				seen[string(k)] = v
			}
		}
	}
	for _, k := range ContentAttributeKeys {
		v, ok := seen[string(k)]
		if !ok {
			t.Errorf("tenant: no span carries %s", k)
			continue
		}
		if strings.Contains(v, "s3cr3t-value") {
			t.Errorf("tenant: %s carries the credential unmasked: %q", k, v)
		}
	}
	if !strings.Contains(seen["gen_ai.input.messages"], "[redacted:credential]") {
		t.Errorf("tenant: input messages = %q, want the credential masked", seen["gen_ai.input.messages"])
	}
}

func TestContentCapture_ResolvedCredentialInAPromptIsMasked(t *testing.T) {
	exp := withInMemorySyncer(t)
	prev := tenantRouting.Load()
	tenantRouting.Store(true)
	t.Cleanup(func() { tenantRouting.Store(prev) })

	recordCapturedRun(context.Background(), "acme", testCapture(DefaultContentMaxChars))
	n := 0
	for _, s := range exp.GetSpans() {
		for _, kv := range s.Attributes {
			if isContentKey(kv.Key) {
				n++
				if strings.Contains(kv.Value.AsString(), "s3cr3t-value") {
					t.Errorf("%s = %q: the credential is not masked", kv.Key, kv.Value.AsString())
				}
			}
		}
	}
	if n != len(ContentAttributeKeys) {
		t.Errorf("recorded %d content attributes, want %d", n, len(ContentAttributeKeys))
	}
}

func TestContentCapture_NoTraceRunCapturesNothing(t *testing.T) {
	exp := withInMemorySyncer(t)
	prev := tenantRouting.Load()
	tenantRouting.Store(true)
	t.Cleanup(func() { tenantRouting.Store(prev) })

	recordCapturedRun(WithNoTrace(context.Background()), "acme", testCapture(DefaultContentMaxChars))
	// A capture set under a no-trace run's ctx (a sub-run) is ignored too.
	ctx, run := RecordRunStart(context.Background(), RunStartAttrs{RunID: "nt", Tenant: "acme", NoTrace: true})
	recordCapturedRun(ctx, "acme", testCapture(DefaultContentMaxChars))
	run.End()
	for _, s := range exp.GetSpans() {
		for _, kv := range s.Attributes {
			if isContentKey(kv.Key) {
				t.Errorf("a no-trace run's span carries %s", kv.Key)
			}
		}
	}
}

func TestContentCapture_NothingWithoutACaptureOrATenantRouter(t *testing.T) {
	exp := withInMemorySyncer(t)
	prev := tenantRouting.Load()
	t.Cleanup(func() { tenantRouting.Store(prev) })

	tenantRouting.Store(true)
	recordCapturedRun(context.Background(), "acme", nil)
	// A sub-run clears its parent's capture with nil.
	recordCapturedRun(WithContentCapture(context.Background(), testCapture(100)), "acme", nil)
	// acme's capture on a run of another tenant.
	recordCapturedRun(context.Background(), "beta", testCapture(100))
	tenantRouting.Store(false)
	recordCapturedRun(context.Background(), "acme", testCapture(100))
	for _, s := range exp.GetSpans() {
		for _, kv := range s.Attributes {
			if isContentKey(kv.Key) {
				t.Errorf("a span carries %s without a capture in force", kv.Key)
			}
		}
	}
}

func TestTruncateContent_CutsAtTheLimitWithAMarker(t *testing.T) {
	for _, tc := range []struct {
		in   string
		max  int
		want string
	}{
		{"short", 10, "short"},
		{"exactly10!", 10, "exactly10!"},
		{"0123456789abc", 10, "0123456789…[truncated 3 chars]"},
		// Characters, not bytes: a cut never splits a letter.
		{"привіт світ", 6, "привіт…[truncated 5 chars]"},
	} {
		if got := TruncateContent(tc.in, tc.max); got != tc.want {
			t.Errorf("TruncateContent(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
		}
	}
}

func TestContentCapture_FieldIsRedactedThenTruncated(t *testing.T) {
	exp := withInMemorySyncer(t)
	prev := tenantRouting.Load()
	tenantRouting.Store(true)
	t.Cleanup(func() { tenantRouting.Store(prev) })

	c := testCapture(300)
	ctx := WithContentCapture(WithTenant(context.Background(), "acme"), c)
	tctx, tool := RecordToolCall(ctx, ToolCallAttrs{Name: "Read"})
	// The secret straddles the limit: cut first, its head would survive.
	result := strings.Repeat("a", 290) + "s3cr3t-value" + strings.Repeat("b", 500)
	SetToolCallContent(tctx, tool, "Read", []byte(`{}`), result)
	tool.End()
	got := toolResultOf(t, exp)
	if strings.Contains(got, "s3cr3") {
		t.Errorf("a fragment of the secret survived the cut: %q", got)
	}
	// 290 + len("[redacted:credential]") + 500 = 811 characters, 300 kept.
	if !strings.HasSuffix(got, "…[truncated 511 chars]") {
		t.Errorf("result = …%q, want the truncation marker", got[max(0, len(got)-40):])
	}
	if n := len([]rune(strings.TrimSuffix(got, "…[truncated 511 chars]"))); n != 300 {
		t.Errorf("kept %d characters, want 300", n)
	}
}

func toolResultOf(t *testing.T, exp *tracetestExporter) string {
	t.Helper()
	for _, s := range exp.GetSpans() {
		for _, kv := range s.Attributes {
			if string(kv.Key) == "gen_ai.tool.call.result" {
				return kv.Value.AsString()
			}
		}
	}
	t.Fatal("no span carries gen_ai.tool.call.result")
	return ""
}

func TestContentCapture_ImageIsADescriptorNeverItsData(t *testing.T) {
	exp := withInMemorySyncer(t)
	prev := tenantRouting.Load()
	tenantRouting.Store(true)
	t.Cleanup(func() { tenantRouting.Store(prev) })

	ctx := WithContentCapture(WithTenant(context.Background(), "acme"), testCapture(DefaultContentMaxChars))
	mctx, mc := RecordModelCall(ctx, ModelCallAttrs{Provider: "anthropic", Model: "m"})
	SetModelInputContent(mctx, mc, nil, []ContentMessage{{Role: "user", Parts: []ContentPart{
		{Type: ContentPartText, Text: "what is this?"},
		{Type: "image", MediaType: "image/png", Bytes: 2048, Text: "aWdub3JlZA=="},
	}}})
	mc.End()
	var in string
	for _, s := range exp.GetSpans() {
		for _, kv := range s.Attributes {
			if string(kv.Key) == "gen_ai.input.messages" {
				in = kv.Value.AsString()
			}
		}
	}
	var msgs []struct {
		Parts []map[string]any `json:"parts"`
	}
	if err := json.Unmarshal([]byte(in), &msgs); err != nil {
		t.Fatalf("input messages %q: %v", in, err)
	}
	if len(msgs) != 1 || len(msgs[0].Parts) != 2 {
		t.Fatalf("input messages = %s", in)
	}
	img := msgs[0].Parts[1]
	want := map[string]any{"type": "image", "media_type": "image/png", "bytes": float64(2048)}
	if len(img) != len(want) {
		t.Errorf("image part = %v, want exactly %v", img, want)
	}
	for k, v := range want {
		if img[k] != v {
			t.Errorf("image part %s = %v, want %v", k, img[k], v)
		}
	}
	if strings.Contains(in, "aWdub3JlZA") {
		t.Errorf("the image's data reached the span: %s", in)
	}
}

// ContentAttributeKeys is the one list: what the recorders write, what the
// operator's exporter strips and what the tenant copy admits are all it.
func TestContentAttributeKeys_OneListForCaptureStripAndTenantCopy(t *testing.T) {
	exp := withInMemorySyncer(t)
	prev := tenantRouting.Load()
	tenantRouting.Store(true)
	t.Cleanup(func() { tenantRouting.Store(prev) })
	recordCapturedRun(context.Background(), "acme", testCapture(DefaultContentMaxChars))

	written := map[attribute.Key]bool{}
	var plain []attribute.KeyValue
	for _, s := range exp.GetSpans() {
		for _, kv := range s.Attributes {
			plain = append(plain, kv)
			if strings.HasPrefix(string(kv.Key), "gen_ai.") && !tenantAttrPolicyBeforeCapture(kv.Key) {
				written[kv.Key] = true
			}
		}
	}
	listed := map[attribute.Key]bool{}
	for _, k := range ContentAttributeKeys {
		listed[k] = true
		if !written[k] {
			t.Errorf("%s is listed but no recorder writes it", k)
		}
		if !tenantAttrPolicy[string(k)] {
			t.Errorf("%s is not admitted on the tenant's copy", k)
		}
	}
	for k := range written {
		if !listed[k] {
			t.Errorf("a recorder writes %s, which is not in ContentAttributeKeys: the operator would receive it", k)
		}
	}
	for _, kv := range withoutContent(plain) {
		if listed[kv.Key] {
			t.Errorf("stripping kept %s", kv.Key)
		}
	}
}

// tenantAttrPolicyBeforeCapture reports whether k is a gen_ai key loomcycle
// writes with capture off (not content).
func tenantAttrPolicyBeforeCapture(k attribute.Key) bool {
	return tenantAttrPolicy[string(k)] && !isContentKey(k)
}

func TestContentStripExporter_StripsEventsAndLeavesTheSharedSpanWhole(t *testing.T) {
	rec := &tracetestExporter{}
	strip := &contentStripExporter{next: rec}
	tp := sdktrace.NewTracerProvider()
	_, span := tp.Tracer("t").Start(context.Background(), "s",
		trace.WithAttributes(attribute.String("loomcycle.tenant", "acme"), ContentAttributeKeys[1].String("prompt")))
	span.AddEvent("e", trace.WithAttributes(ContentAttributeKeys[4].String("result")))
	span.End()
	ro := span.(sdktrace.ReadOnlySpan)
	if err := strip.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{ro}); err != nil {
		t.Fatal(err)
	}
	got := rec.GetSpans()
	if len(got) != 1 || hasContentKey(got[0].Attributes) || len(got[0].Attributes) != 1 {
		t.Errorf("exported attributes = %v, want the tenant only", got)
	}
	if !hasContentKey(ro.Attributes()) || !hasContentKey(ro.Events()[0].Attributes) {
		t.Error("stripping modified the span the tenant's pipeline shares")
	}
}

// A run decides to capture at its start, but its content reaches the tenant's
// collector only while the destination, as it is when each batch leaves,
// captures: a tenant that turns capture off stops a run already capturing,
// and content never reaches a destination set without capture.
func TestTenantRouter_ContentLeavesOnlyWhileTheDestinationCaptures(t *testing.T) {
	clearOTELEnv(t)
	tenant := newFakeCollector(t)
	r := fastRouter(TenantRouterConfig{})
	on := dest(tenant, nil)
	on.ContentCapture = true
	r.SetDestination("acme", on)
	routedProvider(t, r)
	c := testCapture(DefaultContentMaxChars)

	recordCapturedRun(context.Background(), "acme", c)
	waitFor(t, "the spans with capture on", func() bool { return len(tenant.got()) == 3 })
	if !anyContent(tenant.got()) {
		t.Fatal("capture on: no span carries content")
	}

	off := on
	off.ContentCapture = false
	r.SetDestination("acme", off) // same endpoint: the pipeline is kept
	recordCapturedRun(context.Background(), "acme", c)
	waitFor(t, "the spans with capture off", func() bool { return len(tenant.got()) == 6 })
	if after := tenant.got()[3:]; anyContent(after) {
		t.Errorf("capture turned off: content still reached the collector: %+v", after)
	}
}

// Each recording asks the capture whether the tenant still captures.
func TestContentCaptureFrom_NothingOnceTheTenantStopsCapturing(t *testing.T) {
	r := fastRouter(TenantRouterConfig{})
	t.Cleanup(SetTenantRouterForTest(r))
	active := true
	c := testCapture(DefaultContentMaxChars)
	c.Active = func() bool { return active }
	ctx := WithContentCapture(WithTenant(context.Background(), "acme"), c)
	if ContentCaptureFrom(ctx) == nil {
		t.Fatal("active capture: nothing")
	}
	active = false
	if ContentCaptureFrom(ctx) != nil {
		t.Error("the tenant stopped capturing, yet content would still be recorded")
	}
}

func anyContent(spans []collectedSpan) bool {
	for _, s := range spans {
		for _, k := range ContentAttributeKeys {
			if _, ok := s.attrs[string(k)]; ok {
				return true
			}
		}
	}
	return false
}
