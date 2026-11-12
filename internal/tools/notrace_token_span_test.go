package tools_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/retentionmode"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// memoryTool opens a memory-search span, as the Memory tool's search does.
type memoryTool struct{}

func (memoryTool) Name() string                 { return "Memory" }
func (memoryTool) Description() string          { return "memory" }
func (memoryTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (memoryTool) Execute(ctx context.Context, _ json.RawMessage) (tools.Result, error) {
	_, s := lcotel.RecordMemorySearch(ctx, "inprocess")
	s.End()
	return tools.Result{Text: "ok"}, nil
}

// A no-trace token's tool calls outside any run (the MCP, HTTP and gRPC tool
// surfaces) store their content nowhere, so their spans — the tool call and
// the memory work under it — never reach the tenant's own collector.
func TestDispatcherCall_ANoTraceTokensSpansAreNeverSentToTheTenant(t *testing.T) {
	var mu sync.Mutex
	var names []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req coltracepb.ExportTraceServiceRequest
		_ = proto.Unmarshal(body, &req)
		mu.Lock()
		for _, rs := range req.GetResourceSpans() {
			for _, ss := range rs.GetScopeSpans() {
				for _, s := range ss.GetSpans() {
					names = append(names, s.GetName())
				}
			}
		}
		mu.Unlock()
		out, _ := proto.Marshal(&coltracepb.ExportTraceServiceResponse{})
		_, _ = w.Write(out)
	}))
	defer srv.Close()
	r := lcotel.NewTenantRouter(lcotel.TenantRouterConfig{ScheduleDelay: 10 * time.Millisecond})
	r.SetDestination("acme", lcotel.TenantDestination{Endpoint: srv.URL, Protocol: lcotel.ProtocolHTTPProtobuf,
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}, Ratio: 1})
	t.Cleanup(lcotel.SetTenantRouterForTest(r))

	d := tools.NewDispatcher([]tools.Tool{memoryTool{}})
	call := func(retention string) {
		ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "acme", UserID: "dev"})
		ctx = auth.WithPrincipal(ctx, auth.Principal{TenantID: "acme", Subject: "dev", Restrictions: auth.Restrictions{Retention: retention}})
		if _, err := d.Call(ctx, "Memory", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		_ = r.ForceFlush(context.Background())
	}
	call(retentionmode.None)
	call("")
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(names)
		mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(names) != 2 {
		t.Errorf("the tenant's collector got %v; want only the ordinary token's tool and memory spans", names)
	}
}
