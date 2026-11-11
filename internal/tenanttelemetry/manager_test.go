package tenanttelemetry

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/denn-gubsky/loomcycle/internal/coord"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/credential/credentialtest"
	"github.com/denn-gubsky/loomcycle/internal/netguard"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// collector is an OTLP/HTTP collector over TLS that counts the spans it is
// sent and keeps the headers they came with.
type collector struct {
	srv *httptest.Server

	mu      sync.Mutex
	spans   int
	headers []http.Header
	refuse  bool
}

func newCollector(t *testing.T) *collector {
	t.Helper()
	c := &collector{}
	c.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		defer c.mu.Unlock()
		c.headers = append(c.headers, r.Header.Clone())
		if c.refuse {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, "denied "+r.Header.Get("Authorization"))
			return
		}
		var req coltracepb.ExportTraceServiceRequest
		_ = proto.Unmarshal(body, &req)
		for _, rs := range req.GetResourceSpans() {
			for _, ss := range rs.GetScopeSpans() {
				c.spans += len(ss.GetSpans())
			}
		}
		out, _ := proto.Marshal(&coltracepb.ExportTraceServiceResponse{})
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(out)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *collector) lastAuth() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.headers) == 0 {
		return ""
	}
	return c.headers[len(c.headers)-1].Get("Authorization")
}

func (c *collector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.spans
}

type fixture struct {
	st     *sqlite.Store
	engine *credential.Engine
	router *lcotel.TenantRouter
	m      *Manager
	now    time.Time
	tls    *tls.Config
}

func loopbackDial(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

func newFixture(t *testing.T, cfg Config, tlsFrom *collector) *fixture {
	t.Helper()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sealer, err := credential.NewSealer(base64.StdEncoding.EncodeToString(make([]byte, 32)), "")
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{st: st, engine: credential.NewEngine(st, sealer), now: time.Now().UTC()}
	f.router = lcotel.NewTenantRouter(lcotel.TenantRouterConfig{ScheduleDelay: 20 * time.Millisecond, ExportTimeout: 2 * time.Second})
	t.Cleanup(func() { _ = f.router.Shutdown(context.Background()) })
	if tlsFrom != nil {
		f.tls = tlsFrom.srv.Client().Transport.(*http.Transport).TLSClientConfig
	}
	if cfg.MaxRatio == 0 {
		cfg.MaxRatio = 1
	}
	cfg.Enabled = true
	f.m, err = New(Deps{
		Config: cfg, Store: st, Credentials: f.engine, Router: f.router, Dial: loopbackDial,
		Now: func() time.Time { return f.now }, TLSConfig: f.tls, ServiceVersion: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// put checks and stores a policy, as the connector does.
func (f *fixture) put(ctx context.Context, tenant string, in PolicyInput, by string) (store.TenantTelemetryPolicyRow, error) {
	row, err := f.m.Check(ctx, tenant, in)
	if err != nil {
		return row, err
	}
	return f.m.Store(ctx, row, by)
}

// cred stores a tenant-scope credential; urls nil leaves it unrestricted.
func (f *fixture) cred(t *testing.T, tenant, scope, scopeID, name, value string, urls []string, expires *time.Time) {
	t.Helper()
	var allowed *[]string
	if urls != nil {
		allowed = &urls
	}
	if _, err := f.engine.PutInline(context.Background(), credential.Identity{TenantID: tenant, Scope: scope, ScopeID: scopeID, Name: name}, value, expires, allowed, nil); err != nil {
		t.Fatal(err)
	}
}

func ratio(r float64) *float64 { return &r }

func TestManagerPut_RefusesWhatTheOperatorOrTheCredentialDoesNotAllow(t *testing.T) {
	c := newCollector(t)
	f := newFixture(t, Config{AllowedHosts: []string{"127.0.0.1", "*.collector.example"}, MaxRatio: 0.5}, c)
	ctx := context.Background()
	ep := c.srv.URL
	f.cred(t, "acme", "tenant", "", "bound", "authorization=Bearer abcdefgh", []string{ep}, nil)
	f.cred(t, "acme", "tenant", "", "anywhere", "authorization=Bearer abcdefgh", nil, nil)
	f.cred(t, "acme", "tenant", "", "elsewhere", "authorization=Bearer abcdefgh", []string{"https://other.example"}, nil)
	f.cred(t, "beta", "tenant", "", "theirs", "authorization=Bearer abcdefgh", []string{ep}, nil)
	f.cred(t, "acme", "user", "alice", "mine", "authorization=Bearer abcdefgh", []string{ep}, nil)
	f.cred(t, "acme", "tenant", "", "old", "authorization=Bearer abcdefgh", []string{ep}, nil)
	credentialtest.Expire(t, f.st, credential.Identity{TenantID: "acme", Scope: "tenant", Name: "old"}, time.Now().Add(-time.Hour))
	f.cred(t, "acme", "tenant", "", "framing", "host=evil.example", []string{ep}, nil)
	f.cred(t, "acme", "tenant", "", "notheaders", "just a token", []string{ep}, nil)

	cases := map[string]PolicyInput{
		"http without the lab override":      {Endpoint: strings.Replace(ep, "https://", "http://", 1)},
		"host the operator does not allow":   {Endpoint: "https://otel.attacker.example"},
		"bare suffix of an allowed wildcard": {Endpoint: "https://collector.example"},
		"user info":                          {Endpoint: strings.Replace(ep, "https://", "https://u:p@", 1)},
		"query":                              {Endpoint: ep + "/?token=x"},
		"no endpoint":                        {},
		"unknown protocol":                   {Endpoint: ep, Protocol: "zipkin"},
		"grpc with a path":                   {Endpoint: ep + "/v1", Protocol: "grpc"},
		"metrics signal":                     {Endpoint: ep, Signals: []string{"traces", "metrics"}},
		"unknown signal":                     {Endpoint: ep, Signals: []string{"logs"}},
		"content capture on":                 {Endpoint: ep, ContentCapture: "on"},
		"ratio over one":                     {Endpoint: ep, SamplerRatio: ratio(1.5)},
		"ratio below zero":                   {Endpoint: ep, SamplerRatio: ratio(-0.1)},
		"ratio above the operator cap":       {Endpoint: ep, SamplerRatio: ratio(0.75)},
		"credential not bound to anything":   {Endpoint: ep, HeadersCredential: "anywhere"},
		"credential bound elsewhere":         {Endpoint: ep, HeadersCredential: "elsewhere"},
		"missing credential":                 {Endpoint: ep, HeadersCredential: "nope"},
		"another tenant's credential":        {Endpoint: ep, HeadersCredential: "theirs"},
		"a user's credential":                {Endpoint: ep, HeadersCredential: "mine"},
		"expired credential":                 {Endpoint: ep, HeadersCredential: "old"},
		"framing header in the block":        {Endpoint: ep, HeadersCredential: "framing"},
		"value that is not a header block":   {Endpoint: ep, HeadersCredential: "notheaders"},
		"credential name with a slash":       {Endpoint: ep, HeadersCredential: "a/b"},
	}
	for name, in := range cases {
		if _, err := f.put(ctx, "acme", in, "ops"); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		} else if strings.Contains(err.Error(), "abcdefgh") {
			t.Errorf("%s: the refusal quotes the credential value: %v", name, err)
		}
	}
	if _, err := f.put(ctx, "", PolicyInput{Endpoint: ep}, "ops"); !errors.Is(err, ErrInvalid) {
		t.Errorf("a policy for no tenant: err = %v", err)
	}
	if rows, _ := f.st.TenantTelemetryPolicyList(ctx); len(rows) != 0 {
		t.Errorf("refused writes stored %d rows", len(rows))
	}
	if got := f.router.Tenants(); len(got) != 0 {
		t.Errorf("refused writes set destinations %v", got)
	}

	row, err := f.put(ctx, "acme", PolicyInput{Endpoint: ep + "/", HeadersCredential: "bound", SamplerRatio: ratio(0.5)}, "ops")
	if err != nil {
		t.Fatalf("a good policy: %v", err)
	}
	if row.Endpoint != ep || row.Protocol != "http/protobuf" || len(row.Signals) != 1 || row.SamplerRatio != 0.5 || row.UpdatedBy != "ops" {
		t.Errorf("stored %+v", row)
	}
	if got := f.router.Tenants(); len(got) != 1 || got[0] != "acme" {
		t.Errorf("destinations = %v, want acme", got)
	}
}

func TestManagerPut_HTTPEndpointNeedsTheLabOverrideAndNoCredential(t *testing.T) {
	f := newFixture(t, Config{AllowHTTP: true}, nil)
	if _, err := f.put(context.Background(), "acme", PolicyInput{Endpoint: "http://collector.lab:4318"}, "ops"); err != nil {
		t.Fatalf("lab http endpoint: %v", err)
	}
}

func TestManagerTest_SendsThroughTheRealPipelineAndScrubsFailures(t *testing.T) {
	c := newCollector(t)
	f := newFixture(t, Config{}, c)
	ctx := context.Background()
	const token = "Bearer tok-0123456789"
	f.cred(t, "acme", "tenant", "", "otel", "authorization="+token, []string{c.srv.URL}, nil)
	if err := f.m.Test(ctx, "acme"); !errors.Is(err, ErrInvalid) {
		t.Errorf("test with no policy: %v", err)
	}
	if _, err := f.put(ctx, "acme", PolicyInput{Endpoint: c.srv.URL, HeadersCredential: "otel"}, "ops"); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Test(ctx, "acme"); err != nil {
		t.Fatalf("test against a working collector: %v", err)
	}
	if c.count() != 1 || c.lastAuth() != token {
		t.Fatalf("collector got %d spans, auth %q", c.count(), c.lastAuth())
	}
	c.mu.Lock()
	c.refuse = true
	c.mu.Unlock()
	err := f.m.Test(ctx, "acme")
	if err == nil || !strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "tok-0123456789") {
		t.Fatalf("test against a refusing collector: %v", err)
	}
}

func TestManager_DialGuardRefusesPrivateAndMetadataAddresses(t *testing.T) {
	// The real guard, as the binary builds it: no private address allowed.
	guard := netguard.GuardedDialContext(false, nil)
	for _, ep := range []string{
		"http://localhost:4318",  // a name that resolves to loopback at dial time
		"http://127.0.0.1:4318",  // loopback literal
		"http://169.254.169.254", // cloud metadata
		"http://10.0.0.7:4318",   // private range
		"http://[::1]:4318",      // IPv6 loopback
	} {
		f := newFixture(t, Config{AllowHTTP: true}, nil)
		f.m.dial = guard
		if _, err := f.put(context.Background(), "acme", PolicyInput{Endpoint: ep}, "ops"); err != nil {
			t.Fatalf("%s: the PUT checks no address, the dial does: %v", ep, err)
		}
		err := f.m.Test(context.Background(), "acme")
		if err == nil {
			t.Errorf("%s: a test export reached it", ep)
			continue
		}
		if !strings.Contains(err.Error(), "blocked") {
			t.Errorf("%s: refused for another reason: %v", ep, err)
		}
	}
}

func TestManagerRefresh_PicksUpARotatedOrRemovedCredential(t *testing.T) {
	c := newCollector(t)
	f := newFixture(t, Config{}, c)
	ctx := context.Background()
	f.cred(t, "acme", "tenant", "", "otel", "authorization=Bearer first-value", []string{c.srv.URL}, nil)
	if _, err := f.put(ctx, "acme", PolicyInput{Endpoint: c.srv.URL, HeadersCredential: "otel"}, "ops"); err != nil {
		t.Fatal(err)
	}
	export := func() {
		t.Helper()
		if err := endOneSpan(f.router, "acme"); err != nil {
			t.Fatal(err)
		}
	}
	export()
	waitFor(t, func() bool { return c.lastAuth() == "Bearer first-value" })

	// Rotated: the refresh sees the credential row change and rebuilds.
	f.now = f.now.Add(time.Second)
	f.cred(t, "acme", "tenant", "", "otel", "authorization=Bearer second-value", []string{c.srv.URL}, nil)
	if err := f.m.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	export()
	waitFor(t, func() bool { return c.lastAuth() == "Bearer second-value" })

	// Removed: the export stops and the status says why.
	if _, err := f.engine.Delete(ctx, credential.Identity{TenantID: "acme", Scope: "tenant", Name: "otel"}); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.router.Tenants(); len(got) != 0 {
		t.Errorf("destinations after the credential went: %v", got)
	}
	if st := f.m.Status("acme"); !strings.Contains(st.ApplyError, "not a credential of this tenant") {
		t.Errorf("status = %+v", st)
	}

	// The policy deleted on another replica (straight in the store): the
	// refresh removes what this one still had.
	f.cred(t, "acme", "tenant", "", "otel", "authorization=Bearer third-value", []string{c.srv.URL}, nil)
	if err := f.m.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.router.Tenants(); len(got) != 1 {
		t.Fatalf("destinations after the credential came back: %v", got)
	}
	if _, err := f.st.TenantTelemetryPolicyDelete(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.router.Tenants(); len(got) != 0 {
		t.Errorf("destinations after the policy went: %v", got)
	}
}

func TestManagerRefresh_StopsWhenTheCredentialExpires(t *testing.T) {
	c := newCollector(t)
	f := newFixture(t, Config{}, c)
	ctx := context.Background()
	// The credential store keeps the wall clock, so the expiry is a real
	// moment shortly ahead, and nothing about the row changes when it passes.
	exp := time.Now().Add(300 * time.Millisecond)
	f.cred(t, "acme", "tenant", "", "otel", "authorization=Bearer abcdefgh", []string{c.srv.URL}, &exp)
	if _, err := f.put(ctx, "acme", PolicyInput{Endpoint: c.srv.URL, HeadersCredential: "otel"}, "ops"); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Refresh(ctx); err != nil || len(f.router.Tenants()) != 1 {
		t.Fatalf("before the expiry: err=%v destinations=%v", err, f.router.Tenants())
	}
	time.Sleep(time.Until(exp) + 50*time.Millisecond)
	f.now = time.Now()
	if err := f.m.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.router.Tenants(); len(got) != 0 {
		t.Errorf("an expired credential still exports: %v", got)
	}
	if st := f.m.Status("acme"); !strings.Contains(st.ApplyError, "expired") {
		t.Errorf("status = %+v", st)
	}
}

func TestManagerPut_EmptySignalsMeanTraces(t *testing.T) {
	c := newCollector(t)
	f := newFixture(t, Config{}, c)
	row, err := f.put(context.Background(), "acme", PolicyInput{Endpoint: c.srv.URL, Signals: []string{}}, "ops")
	if err != nil {
		t.Fatal(err)
	}
	if len(row.Signals) != 1 || row.Signals[0] != SignalTraces {
		t.Errorf("signals = %v, want [traces]", row.Signals)
	}
	if got := f.router.Tenants(); len(got) != 1 {
		t.Errorf("destinations = %v", got)
	}
}

func TestManagerRefresh_ARowWithoutTracesExportsNothing(t *testing.T) {
	c := newCollector(t)
	f := newFixture(t, Config{}, c)
	// A row a later build may store (metrics only): this one exports no traces.
	if err := f.st.TenantTelemetryPolicyPut(context.Background(), store.TenantTelemetryPolicyRow{
		TenantID: "acme", Endpoint: c.srv.URL, Protocol: "http/protobuf", Signals: []string{"metrics"}, SamplerRatio: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.router.Tenants(); len(got) != 0 {
		t.Errorf("a row without traces exports: %v", got)
	}
}

// memBackplane delivers every publish to every other subscriber, as the
// Postgres backplane does between replicas.
type memBackplane struct {
	mu   sync.Mutex
	subs []chan coord.Event
}

func (b *memBackplane) Publish(_ context.Context, topic string, payload []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		ch <- coord.Event{Topic: topic, Payload: payload}
	}
	return nil
}

func (b *memBackplane) Subscribe(ctx context.Context, topic string) (<-chan coord.Event, error) {
	ch := make(chan coord.Event, 16)
	b.mu.Lock()
	b.subs = append(b.subs, ch)
	b.mu.Unlock()
	go func() { <-ctx.Done() }()
	return ch, nil
}

func (b *memBackplane) Close() error { return nil }

func TestManager_AWriteOnOneReplicaReachesAnotherOverTheBackplane(t *testing.T) {
	c := newCollector(t)
	a := newFixture(t, Config{}, c)
	// Replica b shares a's store, with a router of its own.
	b := &fixture{st: a.st, engine: a.engine, now: a.now}
	b.router = lcotel.NewTenantRouter(lcotel.TenantRouterConfig{})
	t.Cleanup(func() { _ = b.router.Shutdown(context.Background()) })
	var err error
	b.m, err = New(Deps{Config: Config{Enabled: true, MaxRatio: 1}, Store: a.st, Credentials: a.engine, Router: b.router, Dial: loopbackDial, TLSConfig: a.tls})
	if err != nil {
		t.Fatal(err)
	}
	bp := &memBackplane{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.m.Subscribe(ctx, bp); err != nil {
		t.Fatal(err)
	}
	a.m.SetBackplane(bp)
	if _, err := a.put(ctx, "acme", PolicyInput{Endpoint: c.srv.URL}, "ops"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(b.router.Tenants()) == 1 })
	if _, err := a.m.Delete(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(b.router.Tenants()) == 0 })
}

func TestConfigFromEnv_GateOffReadsNothingElse(t *testing.T) {
	env := map[string]string{"LOOMCYCLE_TENANT_TELEMETRY_MAX_RATIO": "nonsense"}
	cfg, err := ConfigFromEnv(func(k string) string { return env[k] })
	if err != nil || cfg.Enabled {
		t.Fatalf("gate off: cfg=%+v err=%v", cfg, err)
	}
	env["LOOMCYCLE_TENANT_TELEMETRY"] = "1"
	if _, err := ConfigFromEnv(func(k string) string { return env[k] }); err == nil {
		t.Error("gate on: a bad ratio passed")
	}
	env["LOOMCYCLE_TENANT_TELEMETRY_MAX_RATIO"] = "0.25"
	env["LOOMCYCLE_TENANT_TELEMETRY_ALLOWED_HOSTS"] = "Grafana.NET, *.otel.example ,"
	env["LOOMCYCLE_TENANT_TELEMETRY_QUEUE"] = "128"
	cfg, err = ConfigFromEnv(func(k string) string { return env[k] })
	if err != nil || !cfg.Enabled || cfg.MaxRatio != 0.25 || cfg.QueueSize != 128 || cfg.MaxPipelines != lcotel.DefaultTenantMaxPipelines ||
		len(cfg.AllowedHosts) != 2 || cfg.AllowedHosts[0] != "grafana.net" {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	for _, bad := range []string{"a*.b", "*", "*.a.*"} {
		env["LOOMCYCLE_TENANT_TELEMETRY_ALLOWED_HOSTS"] = bad
		if _, err := ConfigFromEnv(func(k string) string { return env[k] }); err == nil {
			t.Errorf("allowed host %q accepted", bad)
		}
	}
}

func TestHostAllowed_ExactOrUnderASuffix(t *testing.T) {
	list := []string{"otel.example.com", "*.grafana.net"}
	for host, want := range map[string]bool{
		"otel.example.com": true, "OTEL.example.com": true, "x.otel.example.com": false,
		"a.grafana.net": true, "a.b.grafana.net": true, "grafana.net": false, "evilgrafana.net": false,
	} {
		if got := hostAllowed(list, host); got != want {
			t.Errorf("%s: %v, want %v", host, got, want)
		}
	}
	if !hostAllowed(nil, "anything.example") {
		t.Error("an empty list must admit every host")
	}
}

// endOneSpan ends one sampled span of tenant on a provider whose only
// processor is r, and flushes r.
func endOneSpan(r *lcotel.TenantRouter, tenant string) error {
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(r))
	_, s := tp.Tracer("t").Start(context.Background(), "work", trace.WithAttributes(attribute.String(lcotel.AttrTenant, tenant)))
	s.End()
	return r.ForceFlush(context.Background())
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// faultyCredentials fails every resolution with an error naming internals.
type faultyCredentials struct{}

const faultMarker = "MARKER-pg-10.0.3.4:5432-relation-credential_defs"

func (faultyCredentials) ResolveFor(context.Context, string, string, string, string, string) (credential.Resolved, bool, error) {
	return credential.Resolved{}, false, errors.New(faultMarker)
}

func TestManager_ACredentialFaultReachesTheTenantOnlyAsAGenericLine(t *testing.T) {
	c := newCollector(t)
	f := newFixture(t, Config{}, c)
	ctx := context.Background()
	// Stored directly: the write path would have refused it.
	if err := f.st.TenantTelemetryPolicyPut(ctx, store.TenantTelemetryPolicyRow{
		TenantID: "acme", Endpoint: c.srv.URL, Protocol: "http/protobuf", HeadersCredential: "otel", Signals: []string{"traces"}, SamplerRatio: 1,
	}); err != nil {
		t.Fatal(err)
	}
	f.m.creds = faultyCredentials{}
	err := f.m.Test(ctx, "acme")
	if err == nil {
		t.Fatal("a credential fault passed the test")
	}
	if got := PublicError("acme", err); strings.Contains(got, "MARKER") || got != genericTestError {
		t.Errorf("test error shown to the tenant: %q", got)
	}
	if err := f.m.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if st := f.m.Status("acme"); strings.Contains(st.ApplyError, "MARKER") || st.ApplyError != genericApplyError {
		t.Errorf("status shown to the tenant: %q", st.ApplyError)
	}
	// What the tenant may read still reads: a refusal in its own words, an
	// exporter failure as the exporter (scrubbed) put it.
	if got := PublicError("acme", invalid("endpoint must use https")); got != "endpoint must use https" {
		t.Errorf("a refusal: %q", got)
	}
}

// A stored row is held to the operator's rules as they are now, not as they
// were when it was written: one restored from another deployment, or written
// before the operator narrowed its host list, is not exported.
func TestManagerRefresh_AStoredRowTheRulesNowRefuseIsNotExported(t *testing.T) {
	c := newCollector(t)
	f := newFixture(t, Config{AllowedHosts: []string{"otel.allowed.example"}}, c)
	ctx := context.Background()
	if err := f.st.TenantTelemetryPolicyPut(ctx, store.TenantTelemetryPolicyRow{
		TenantID: "acme", Endpoint: c.srv.URL, Protocol: "http/protobuf", Signals: []string{"traces"}, SamplerRatio: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.router.Tenants(); len(got) != 0 {
		t.Errorf("a row the host list refuses exports: %v", got)
	}
	if st := f.m.Status("acme"); !strings.Contains(st.ApplyError, "not one the operator allows") {
		t.Errorf("status = %+v", st)
	}
}
