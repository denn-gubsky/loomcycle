package grpc

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestGrpcAuth_DeclaredPrincipalsTurnAuthOn is the gRPC twin of the HTTP test
// (RFC ES F2): wired to the HTTP server's open-mode predicate and resolver, as
// main.go wires them, a deployment whose only credential is a config principal
// refuses a call with no bearer and serves the principal as itself.
func TestGrpcAuth_DeclaredPrincipalsTurnAuthOn(t *testing.T) {
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "grpc-declared.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	const secret = "lct_declared_bob"
	cfg := &config.Config{
		Principals: map[string]config.PrincipalDef{
			"bob": {Tenant: "acme", Subject: "bob", Scopes: []string{auth.ScopeTenant}, TokenEnv: "LOOMCYCLE_BOB_TOKEN"},
		},
		ResolvedPrincipals: []auth.DeclaredPrincipal{{
			Secret:    secret,
			Principal: auth.Principal{TenantID: "acme", Subject: "bob", Scopes: []string{auth.ScopeTenant}, TokenDefID: "cfg:bob"},
		}},
	}
	httpSrv := lchttp.New(cfg, oneProvider{&answerProvider{}}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	adapter := New(Config{
		Store:             st,
		CancelReg:         cancel.NewRegistry(),
		Connector:         httpSrv,
		Runner:            httpSrv,
		PrincipalResolver: httpSrv.ResolvePrincipal,
		AuthConfigured:    httpSrv.AuthConfigured,
	})
	info := &googlegrpc.UnaryServerInfo{FullMethod: loomcyclepb.Loomcycle_GetAgent_FullMethodName}

	called := false
	_, err = adapter.UnaryAuthInterceptor()(context.Background(), &loomcyclepb.GetAgentRequest{}, info,
		func(context.Context, any) (any, error) { called = true; return nil, nil })
	if called || status.Code(err) != codes.Unauthenticated {
		t.Errorf("no bearer with only config principals: handler called = %v, err = %v; want Unauthenticated", called, err)
	}

	var got auth.Principal
	var ok bool
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+secret))
	_, err = adapter.UnaryAuthInterceptor()(ctx, &loomcyclepb.GetAgentRequest{}, info,
		func(ctx context.Context, _ any) (any, error) {
			got, ok = auth.PrincipalFromContext(ctx)
			return nil, nil
		})
	if err != nil || !ok || got.TenantID != "acme" || got.Subject != "bob" {
		t.Errorf("principal bearer: err = %v, principal = (%+v, %v); want acme/bob", err, got, ok)
	}
}
