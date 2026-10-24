package grpc

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/erasure"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestErasureExecute_AuditRefusalIsUnavailable: the connector refuses a live
// erasure whose intent record cannot be written. Over HTTP that is a 503; over
// gRPC it must be Unavailable, naming the reason, not an opaque Internal.
func TestErasureExecute_AuditRefusalIsUnavailable(t *testing.T) {
	mc := &mockConnector{erasureErr: fmt.Errorf("%w: no audit sink configured", erasure.ErrAuditUnavailable)}
	srv := &Server{connector: mc}
	f := false
	_, err := srv.ErasureExecute(context.Background(), &loomcyclepb.ErasureExecuteRequest{
		Subject: "alice", DryRun: &f, Confirm: "alice",
	})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("code = %v (%v), want Unavailable", status.Code(err), err)
	}
}

// TestErasureExecute_WritesTheEraseAuditPair drives a live erasure over gRPC
// against the REAL HTTP server as the connector, and asserts the same two
// records an HTTP erasure writes. Before the audit moved into the connector
// path, a gRPC erasure deleted the subject and recorded nothing.
func TestErasureExecute_WritesTheEraseAuditPair(t *testing.T) {
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "grpc-erasure.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4}}
	httpSrv := lchttp.New(cfg, oneProvider{&answerProvider{}}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	sink := &eraseSink{}
	httpSrv.SetEraseAudit(sink)
	adapter := New(Config{Store: st, CancelReg: cancel.NewRegistry(), Connector: httpSrv, Runner: httpSrv})
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant},
	})

	f := false
	if _, err := adapter.ErasureExecute(ctx, &loomcyclepb.ErasureExecuteRequest{
		Subject: "alice", DryRun: &f, Confirm: "alice",
	}); err != nil {
		t.Fatalf("erasure: %v", err)
	}
	if len(sink.events) != 2 || sink.events[0].Action != "erase_intent" || sink.events[1].Action != "erase_result" {
		t.Fatalf("audit = %+v, want erase_intent then erase_result", sink.events)
	}
	for i, ev := range sink.events {
		if ev.TargetTenant != "acme" || ev.TargetSubject != "alice" || ev.ActorSubject != "ops" {
			t.Errorf("record %d does not name the target and the actor: %+v", i, ev)
		}
	}
}

type eraseSink struct{ events []audit.Event }

func (s *eraseSink) Record(ev audit.Event) error {
	s.events = append(s.events, ev)
	return nil
}
