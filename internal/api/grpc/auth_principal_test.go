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

// The gRPC interceptor authenticated but never enforced per-RPC scope (RFC L
// PR2 gap): a narrow token could call any admin RPC — incl. OperatorTokenDef
// mint — over gRPC, escalating to substrate:admin. enforceScope closes it.
func TestGrpcEnforceScope_AdminRPCDeniedForNarrowToken(t *testing.T) {
	const otdMethod = grpcMethodPrefix + "OperatorTokenDef"

	// runs:read token → must be DENIED on the admin OperatorTokenDef RPC.
	narrow := auth.WithPrincipal(context.Background(),
		auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeRunsRead}})
	if err := enforceScope(narrow, otdMethod); status.Code(err) != codes.PermissionDenied {
		t.Errorf("narrow token on OperatorTokenDef: code = %v, want PermissionDenied", status.Code(err))
	}

	// substrate:admin → allowed on the same admin RPC.
	admin := auth.WithPrincipal(context.Background(),
		auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeAdmin}})
	if err := enforceScope(admin, otdMethod); err != nil {
		t.Errorf("admin token on OperatorTokenDef: %v, want nil", err)
	}

	// A consumer RPC (Run) is reachable with runs:create, not admin.
	runner := auth.WithPrincipal(context.Background(),
		auth.Principal{Scopes: []string{auth.ScopeRunsCreate}})
	if err := enforceScope(runner, grpcMethodPrefix+"Run"); err != nil {
		t.Errorf("runs:create on Run: %v, want nil", err)
	}
	// ...but that same runs:create token is DENIED an admin RPC.
	if err := enforceScope(runner, grpcMethodPrefix+"PauseRuntime"); status.Code(err) != codes.PermissionDenied {
		t.Errorf("runs:create on PauseRuntime: code = %v, want PermissionDenied", status.Code(err))
	}

	// Open mode / no principal stamped → skip the gate (parity with HTTP).
	if err := enforceScope(context.Background(), otdMethod); err != nil {
		t.Errorf("no principal should skip scope gate, got %v", err)
	}
}

// Deny-by-default: an unmapped/unknown method requires admin, so a future
// admin RPC is protected even if nobody maps it.
func TestGrpcRequiredScopeForRPC_DefaultsToAdmin(t *testing.T) {
	if got := requiredScopeForRPC(grpcMethodPrefix + "SomeFutureAdminRPC"); got != auth.ScopeAdmin {
		t.Errorf("unmapped RPC scope = %q, want %q", got, auth.ScopeAdmin)
	}
	if got := requiredScopeForRPC(grpcMethodPrefix + "GetAgent"); got != auth.ScopeRunsRead {
		t.Errorf("GetAgent scope = %q, want runs:read", got)
	}
}

// TestGrpcRequiredScopeForRPC_RFCAFTenantPlane locks the RFC AF split on gRPC:
// the 8 def families + hook RPCs accept substrate:tenant, while OperatorTokenDef
// (token minting) stays admin-only via the deny-by-default.
func TestGrpcRequiredScopeForRPC_RFCAFTenantPlane(t *testing.T) {
	tenantRPCs := []string{
		"AgentDef", "SkillDef", "MCPServerDef", "ScheduleDef",
		"A2AServerCardDef", "A2AAgentDef", "WebhookDef", "MemoryBackendDef",
		"RegisterHook", "ListHooks", "DeleteHook",
	}
	for _, name := range tenantRPCs {
		if got := requiredScopeForRPC(grpcMethodPrefix + name); got != auth.ScopeTenant {
			t.Errorf("%s scope = %q, want substrate:tenant", name, got)
		}
	}
	// Minting stays operator-only.
	if got := requiredScopeForRPC(grpcMethodPrefix + "OperatorTokenDef"); got != auth.ScopeAdmin {
		t.Errorf("OperatorTokenDef scope = %q, want substrate:admin (minting stays operator-only)", got)
	}

	// End-to-end: a substrate:tenant token passes AgentDef but is DENIED
	// OperatorTokenDef.
	tenant := auth.WithPrincipal(context.Background(),
		auth.Principal{TenantID: "jobember", Subject: "svc", Scopes: []string{auth.ScopeTenant}})
	if err := enforceScope(tenant, grpcMethodPrefix+"AgentDef"); err != nil {
		t.Errorf("substrate:tenant on AgentDef: %v, want nil", err)
	}
	if err := enforceScope(tenant, grpcMethodPrefix+"OperatorTokenDef"); status.Code(err) != codes.PermissionDenied {
		t.Errorf("substrate:tenant on OperatorTokenDef: code = %v, want PermissionDenied", status.Code(err))
	}
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
