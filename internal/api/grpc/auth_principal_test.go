package grpc

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

func mdCtx(bearer string) context.Context {
	return metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer "+bearer))
}

func TestGrpcAuthenticate_OpenMode(t *testing.T) {
	// authConfigured returns false → pass through even without a bearer.
	s := &Server{authConfigured: func(context.Context) bool { return false }}
	if _, err := s.authenticate(context.Background()); err != nil {
		t.Errorf("open mode should pass through, got %v", err)
	}
}

func TestGrpcAuthenticate_StampsPrincipal(t *testing.T) {
	s := &Server{
		authConfigured: func(context.Context) bool { return true },
		principalResolver: func(_ context.Context, bearer string) (auth.Principal, bool) {
			if bearer != "good" {
				return auth.Principal{}, false
			}
			return auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeRunsCreate}}, true
		},
	}
	// Valid bearer → principal stamped into the returned ctx.
	ctx, err := s.authenticate(mdCtx("good"))
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok || p.TenantID != "acme" || p.Subject != "alice" {
		t.Errorf("principal not stamped: ok=%v p=%+v", ok, p)
	}
	// Invalid bearer → Unauthenticated.
	if _, err := s.authenticate(mdCtx("bad")); status.Code(err) != codes.Unauthenticated {
		t.Errorf("bad bearer: code = %v, want Unauthenticated", status.Code(err))
	}
	// Missing metadata → Unauthenticated.
	if _, err := s.authenticate(context.Background()); status.Code(err) != codes.Unauthenticated {
		t.Errorf("missing md: code = %v, want Unauthenticated", status.Code(err))
	}
}
