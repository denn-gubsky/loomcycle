package grpc

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// countErrStore is a store whose admin-token count always fails, as it does
// during a database outage.
type countErrStore struct{ store.Store }

func (countErrStore) OperatorTokenDefCountActiveAdmin(context.Context) (int, error) {
	return 0, errors.New("store unavailable")
}

// TestGrpcAuth_StoreErrorFailsClosed is the gRPC twin of the HTTP test: wired
// to the real HTTP server's open-mode predicate, as main.go wires it, a call
// with no bearer during a store error is Unauthenticated, not served.
func TestGrpcAuth_StoreErrorFailsClosed(t *testing.T) {
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "grpc-failclosed.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	broken := countErrStore{Store: st}
	httpSrv := lchttp.New(&config.Config{}, oneProvider{&answerProvider{}}, []tools.Tool{}, concurrency.New(4, 4, time.Second), broken)
	adapter := New(Config{
		Store:             broken,
		CancelReg:         cancel.NewRegistry(),
		Connector:         httpSrv,
		Runner:            httpSrv,
		PrincipalResolver: httpSrv.ResolvePrincipal,
		AuthConfigured:    httpSrv.AuthConfigured,
	})

	called := false
	_, err = adapter.UnaryAuthInterceptor()(context.Background(), &loomcyclepb.GetAgentRequest{},
		&googlegrpc.UnaryServerInfo{FullMethod: loomcyclepb.Loomcycle_GetAgent_FullMethodName},
		func(context.Context, any) (any, error) { called = true; return nil, nil })
	if called || status.Code(err) != codes.Unauthenticated {
		t.Errorf("no-bearer call during a store error: handler called = %v, err = %v; want Unauthenticated and not called", called, err)
	}
}
