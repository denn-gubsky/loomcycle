package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/denn-gubsky/loomcycle/internal/config"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// TestMCPPoolBuild_TraceparentReachesOnlyTheOperatorsServers pins the trust
// line on the real pool-build path: the run's traceparent goes to the
// operator's servers (static yaml, a version saved with operator authority)
// and never to one a tenant registered, even one whose host the operator
// vouched for.
func TestMCPPoolBuild_TraceparentReachesOnlyTheOperatorsServers(t *testing.T) {
	defer lcotel.SetInboundForTest(lcotel.InboundParent, propagation.TraceContext{})()
	var (
		mu  sync.Mutex
		got = map[string]string{}
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		got[r.URL.Path] = r.Header.Get("traceparent")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	cfg := &config.Config{
		// The tenant's server must dial so the test sees what it was sent.
		Env:        config.Env{MCPAllowPrivateIPs: true, HTTPPrivateHostAllowlist: []string{u.Hostname()}},
		MCPServers: map[string]config.MCPServer{"static-peer": {Transport: "http", URL: srv.URL + "/static"}},
	}
	reg := mcp.NewDynamicRegistry()
	reg.Set(mcp.DynamicMCPServerSpec{TenantID: "acme", Name: "operator-def", Transport: "http", URL: srv.URL + "/operator", OperatorAuthored: true})
	reg.Set(mcp.DynamicMCPServerSpec{TenantID: "acme", Name: "tenant-def", Transport: "http", URL: srv.URL + "/tenant"})
	var credSubstitute func(ctx context.Context, s, targetURL string) (string, []string, error)
	build := mcpPoolBuild(cfg, mcpLookupView{reg}, &credSubstitute, (&warnLog{}).logger())

	tp := sdktrace.NewTracerProvider()
	defer func() { _ = tp.Shutdown(context.Background()) }()
	ctx, span := tp.Tracer("t").Start(context.Background(), "loomcycle.mcp.call")
	defer span.End()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for _, name := range []string{"static-peer", "operator-def", "tenant-def"} {
		c, err := build("acme", name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := c.Call(ctx, "initialize", map[string]any{}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, path := range []string{"/static", "/operator"} {
		if got[path] == "" {
			t.Errorf("%s: the operator's server got no traceparent", path)
		}
	}
	if v, ok := got["/tenant"]; !ok || v != "" {
		t.Errorf("tenant server: requested=%v traceparent=%q, want a request with none", ok, v)
	}
}
