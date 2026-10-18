package grpc

import (
	"context"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/runner"
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

// A credential with an agent allowlist is held to the run plane on gRPC too,
// however wide its scopes: it may start and follow runs, and is refused
// everything else — including a definition RPC its scope would otherwise
// allow, a direct decision, and an RPC nobody has listed.
func TestEnforceScope_RestrictedTokenIsHeldToTheRunPlane(t *testing.T) {
	restricted := auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: "acme", Subject: "dev", Scopes: []string{auth.ScopeTenant},
		Restrictions: auth.Restrictions{Agents: []string{"score"}},
	})
	free := auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant},
	})
	for _, rpc := range []string{"Run", "Continue", "SpawnRunBatch", "GetRun", "StreamRun", "CancelAgent", "StartConfiguredRun"} {
		if err := enforceScope(restricted, grpcMethodPrefix+rpc); err != nil {
			t.Errorf("%s is on the run plane but was refused: %v", rpc, err)
		}
	}
	for _, rpc := range []string{"AgentDef", "TeamDef", "ScheduleDef", "CredentialDef", "Crypto", "Decide", "ListDecisionModels", "Document", "PublishChannel", "UsageReport", "SomeRPCAddedLater"} {
		if err := enforceScope(restricted, grpcMethodPrefix+rpc); status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s: err = %v, want PermissionDenied for a restricted token", rpc, err)
		}
	}
	for _, rpc := range []string{"AgentDef", "TeamDef", "Decide", "Document"} {
		if err := enforceScope(free, grpcMethodPrefix+rpc); err != nil {
			t.Errorf("%s refused an UNRESTRICTED tenant token: %v", rpc, err)
		}
	}
}

// Every run-plane RPC must be one the scope table knows: a name here that the
// table does not list would be a typo that admits nothing, or worse, a stale
// name that outlived its RPC.
func TestGRPCRunPlaneRPCs_AreAllKnownRPCs(t *testing.T) {
	for rpc := range grpcRunPlaneRPCs {
		if _, ok := grpcConsumerScopes[rpc]; !ok {
			t.Errorf("grpcRunPlaneRPCs lists %q, which the scope table does not know", rpc)
		}
	}
}

// A rate refusal is ResourceExhausted with its own reason, so a client can
// tell "too often" from "too many at once".
func TestMapRunnerErr_RateLimited(t *testing.T) {
	err := mapRunnerErr(fmt.Errorf("%w: starting runs too often; retry in 3s", runner.ErrRateLimited))
	if status.Code(err) != codes.ResourceExhausted {
		t.Errorf("code = %v, want ResourceExhausted", status.Code(err))
	}
	if got := reasonFor(runner.ErrRateLimited); got != "rate_limited" {
		t.Errorf("reason = %q, want rate_limited", got)
	}
}
