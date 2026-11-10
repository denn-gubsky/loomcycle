package otel

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// clearOTELEnv blanks the variables the OTLP exporter and resource read, so a
// developer's own environment cannot leak into a test.
func clearOTELEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "OTEL_") {
			t.Setenv(k, "")
		}
	}
}

func resourceAttrs(t *testing.T, cfg Config) map[string]string {
	t.Helper()
	res, err := newResource(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, kv := range res.Attributes() {
		out[string(kv.Key)] = kv.Value.Emit()
	}
	return out
}

// TestNewResource_DefaultsThenEnvironmentOverrides: loomcycle's defaults are
// present, and OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES replace them.
func TestNewResource_DefaultsThenEnvironmentOverrides(t *testing.T) {
	clearOTELEnv(t)
	cfg := Config{ServiceVersion: "1.2.3", InstanceID: "replica-a", Environment: "prod"}

	got := resourceAttrs(t, cfg)
	for k, want := range map[string]string{
		"service.name":                "loomcycle",
		"service.version":             "1.2.3",
		"service.instance.id":         "replica-a",
		"deployment.environment.name": "prod",
		"telemetry.sdk.language":      "go",
	} {
		if got[k] != want {
			t.Errorf("default %s = %q, want %q", k, got[k], want)
		}
	}

	t.Setenv("OTEL_SERVICE_NAME", "loomcycle-eu")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.instance.id=pod-7,deployment.environment.name=staging,team=ops")
	got = resourceAttrs(t, cfg)
	for k, want := range map[string]string{
		"service.name":                "loomcycle-eu",
		"service.version":             "1.2.3",
		"service.instance.id":         "pod-7",
		"deployment.environment.name": "staging",
		"team":                        "ops",
	} {
		if got[k] != want {
			t.Errorf("overridden %s = %q, want %q", k, got[k], want)
		}
	}
}

// firstBytes accepts one connection on ln and returns what the client wrote
// first.
func firstBytes(t *testing.T, ln net.Listener) <-chan string {
	t.Helper()
	out := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			out <- ""
			return
		}
		defer c.Close()
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 64)
		n, _ := io.ReadAtLeast(c, buf, 16)
		out <- string(buf[:n])
	}()
	return out
}

func oneSpan(t *testing.T) []sdktrace.ReadOnlySpan {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	_, span := tp.Tracer("t").Start(context.Background(), "s")
	span.End()
	return exp.GetSpans().Snapshots()
}

// TestNewExporter_ProtocolPicksTheWire: grpc speaks HTTP/2 (its connection
// preface), http/protobuf posts to /v1/traces, and both find the collector
// through OTEL_EXPORTER_OTLP_ENDPOINT alone — loomcycle passes no endpoint.
func TestNewExporter_ProtocolPicksTheWire(t *testing.T) {
	for _, tc := range []struct {
		protocol Protocol
		want     string
	}{
		{ProtocolGRPC, "PRI * HTTP/2.0"},
		{ProtocolHTTPProtobuf, "POST /v1/traces"},
	} {
		t.Run(string(tc.protocol), func(t *testing.T) {
			clearOTELEnv(t)
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://"+ln.Addr().String())
			t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
			got := firstBytes(t, ln)
			exp, err := newExporter(context.Background(), tc.protocol, nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			_ = exp.ExportSpans(ctx, oneSpan(t))
			_ = exp.Shutdown(ctx)
			if b := <-got; !strings.HasPrefix(b, tc.want) {
				t.Errorf("collector saw %q, want a %q request", b, tc.want)
			}
		})
	}
}

// TestNewTracerProvider_HeadersFileReachTheCollector: the headers file is the
// one header source loomcycle sets, and its values arrive decoded.
func TestNewTracerProvider_HeadersFileReachTheCollector(t *testing.T) {
	clearOTELEnv(t)
	got := make(chan http.Header, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case got <- r.Header.Clone():
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)
	path := filepath.Join(t.TempDir(), "h")
	if err := os.WriteFile(path, []byte("authorization=Bearer%20abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := ResolveConfig(envOf(map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": srv.URL, "LOOMCYCLE_OTEL_EXPORTER_OTLP_HEADERS_FILE": path}))
	if err != nil {
		t.Fatal(err)
	}
	var st pipelineStats
	tp, err := newTracerProvider(context.Background(), cfg, &st, newErrorHandler(slog.New(slog.DiscardHandler), time.Now))
	if err != nil {
		t.Fatal(err)
	}
	_, span := tp.Tracer("t").Start(context.Background(), "s")
	span.End()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tp.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case h := <-got:
		if h.Get("Authorization") != "Bearer abc" {
			t.Errorf("Authorization = %q, want the file's decoded value", h.Get("Authorization"))
		}
	default:
		t.Fatal("collector received nothing")
	}
	if s := st.exported.Load(); s != 1 {
		t.Errorf("exported = %d, want 1", s)
	}
}

// lineCounter is a slog handler target that counts the records written.
type lineCounter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lineCounter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lineCounter) lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(l.buf.Bytes()))
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out
}

// TestPipeline_BlackHoleCollectorNeverBlocksARun: with a collector that
// accepts connections and never answers, ending spans returns at once, the
// overflow and the failed batches are counted, and the outage is logged once.
func TestPipeline_BlackHoleCollectorNeverBlocksARun(t *testing.T) {
	clearOTELEnv(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var (
		connsMu sync.Mutex
		conns   []net.Conn
	)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			connsMu.Lock()
			conns = append(conns, c) // held open, never read or answered
			connsMu.Unlock()
		}
	}()
	defer func() {
		_ = ln.Close()
		connsMu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		connsMu.Unlock()
	}()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://"+ln.Addr().String())
	t.Setenv("OTEL_EXPORTER_OTLP_TIMEOUT", "100")

	cfg := Config{
		Enabled: true, Protocol: ProtocolHTTPProtobuf, SamplerName: "always_on", SamplerArg: 1,
		MaxQueueSize: 8, MaxExportBatchSize: 4, ExportTimeout: 200 * time.Millisecond, ScheduleDelay: 10 * time.Millisecond,
	}
	var logs lineCounter
	fixed := time.Unix(1_800_000_000, 0)
	errs := newErrorHandler(slog.New(slog.NewTextHandler(&logs, nil)), func() time.Time { return fixed })
	prevHandler := otel.GetErrorHandler()
	otel.SetErrorHandler(errs)
	defer otel.SetErrorHandler(prevHandler)

	var st pipelineStats
	tp, err := newTracerProvider(context.Background(), cfg, &st, errs)
	if err != nil {
		t.Fatal(err)
	}
	tracer := tp.Tracer("t")
	const spans = 200
	start := time.Now()
	for i := 0; i < spans; i++ {
		_, span := tracer.Start(context.Background(), "loomcycle.run")
		span.SetAttributes(attribute.Int("i", i))
		span.End()
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("recording %d spans took %v against a dead collector; the caller must never wait on export", spans, elapsed)
	}

	deadline := time.Now().Add(10 * time.Second)
	for st.exportFailures.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = tp.Shutdown(ctx)

	if got := st.ended.Load(); got != spans {
		t.Errorf("ended = %d, want %d", got, spans)
	}
	if st.exportFailures.Load() == 0 {
		t.Error("no export failure counted against a collector that never answers")
	}
	if d := st.dropped.Load(); d < spans-uint64(cfg.MaxQueueSize) {
		t.Errorf("dropped = %d, want at least %d (all but one queue's worth)", d, spans-cfg.MaxQueueSize)
	}
	if e := st.exported.Load(); e != 0 {
		t.Errorf("exported = %d, want 0", e)
	}
	if lines := logs.lines(); len(lines) != 1 {
		t.Errorf("logged %d lines for one outage, want 1:\n%s", len(lines), strings.Join(lines, "\n"))
	}
}

// TestErrorHandler_OneWarnPerOutageAndAFiveMinuteSummary pins the rate limit
// on a fake clock.
func TestErrorHandler_OneWarnPerOutageAndAFiveMinuteSummary(t *testing.T) {
	var logs lineCounter
	now := time.Unix(1_800_000_000, 0)
	h := newErrorHandler(slog.New(slog.NewTextHandler(&logs, nil)), func() time.Time { return now })
	boom := errors.New("connection refused")

	for i := 0; i < 50; i++ {
		h.Handle(boom)
		now = now.Add(time.Second)
	}
	if n := len(logs.lines()); n != 1 {
		t.Fatalf("after 50 errors in under 5 minutes: %d lines, want 1", n)
	}
	now = now.Add(5 * time.Minute)
	h.Handle(boom)
	lines := logs.lines()
	if len(lines) != 2 || !strings.Contains(lines[1], "suppressed_errors=50") {
		t.Fatalf("summary after 5 minutes: %q", lines)
	}
	h.Handle(boom) // inside the next window
	if n := len(logs.lines()); n != 2 {
		t.Fatalf("an error right after a summary logged: %d lines", n)
	}
	h.succeeded()
	lines = logs.lines()
	if len(lines) != 3 || !strings.Contains(lines[2], "recovered") || !strings.Contains(lines[2], "suppressed_errors=1") {
		t.Fatalf("recovery line: %q", lines)
	}
	h.succeeded()
	if n := len(logs.lines()); n != 3 {
		t.Fatalf("a success with no outage logged: %d lines", n)
	}
	h.Handle(boom)
	if n := len(logs.lines()); n != 4 {
		t.Fatalf("a new outage after recovery: %d lines, want its own Warn", n)
	}
}

// TestGateProcessor_UnsampledSpansAreNotCounted: only spans that would be
// exported enter the counts.
func TestGateProcessor_UnsampledSpansAreNotCounted(t *testing.T) {
	var st pipelineStats
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(recordOnly{}),
		sdktrace.WithSpanProcessor(&gateProcessor{next: sdktrace.NewSimpleSpanProcessor(exp), st: &st, maxInFlight: 8}),
	)
	_, span := tp.Tracer("t").Start(context.Background(), "s")
	span.End()
	_ = tp.Shutdown(context.Background())
	if st.ended.Load() != 0 || st.dropped.Load() != 0 {
		t.Errorf("unsampled span counted: ended=%d dropped=%d", st.ended.Load(), st.dropped.Load())
	}
}

// recordOnly records spans without sampling them: OnEnd sees them, the batch
// processor would not export them.
type recordOnly struct{}

func (recordOnly) ShouldSample(sdktrace.SamplingParameters) sdktrace.SamplingResult {
	return sdktrace.SamplingResult{Decision: sdktrace.RecordOnly}
}
func (recordOnly) Description() string { return "recordOnly" }
