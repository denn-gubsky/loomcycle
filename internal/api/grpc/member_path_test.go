package grpc

import (
	"context"
	"testing"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// The principal classes of RFC CB, as a token mints them (auth.GrantableUserScopes
// for the two user modes).
var (
	memberP   = auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead, auth.ScopeChannelPublish, auth.ScopeChannelRead}}
	isolatedP = auth.Principal{TenantID: "acme", Subject: "ivy", Scopes: []string{auth.ScopeUser}}
	operatorP = auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}
	adminP    = auth.Principal{TenantID: "acme", Subject: "root", Scopes: []string{auth.ScopeAdmin}}
)

func as(p auth.Principal) context.Context { return auth.WithPrincipal(context.Background(), p) }

// A member reaches, over gRPC, the RPCs whose HTTP routes it reaches on the
// member path: the definition families, Path / Document / Memory, its
// credentials, history, usage, the user list, a team's own channels, the
// channel fan-in/out — with the handlers' confinement then holding it to its
// tenant.
func TestMemberPath_AdmitsAMemberToTheRPCsItsHTTPRoutesOpen(t *testing.T) {
	for _, rpc := range []string{
		"AgentDef", "SkillDef", "MCPServerDef", "ScheduleDef", "A2AServerCardDef", "A2AAgentDef",
		"WebhookDef", "MemoryBackendDef", "DocumentSourceDef", "VolumeDef", "TeamDef", "HookDef",
		"Path", "Document", "Memory", "CredentialDef", "History", "UsageReport", "TokenLimit",
		"DirectoryUsers", "ListTeamChannels", "PeekTeamChannel", "ReleaseTeamChannel",
		"AwaitChannels", "BroadcastChannels",
	} {
		if err := enforceScope(as(memberP), grpcMethodPrefix+rpc); err != nil {
			t.Errorf("member on %s: %v, want admitted (its HTTP route is on the member path)", rpc, err)
		}
	}
}

// The operator controls stay the operator's on gRPC as on HTTP: token
// minting, runtime admin, the per-subject directory view, erasure, retention
// overrides, crypto with the tenant's keys and access-control management.
func TestMemberPath_RefusesAMemberTheOperatorOnlyRPCs(t *testing.T) {
	for _, rpc := range []string{
		"OperatorTokenDef", "PauseRuntime", "ResumeRuntime", "GetRuntimeState", "ResolveProbe",
		"CreateSnapshot", "ListSnapshots", "RestoreSnapshot", "DirectoryTenants", "DirectoryInspect",
		"ErasureReport", "ErasureExecute", "RetentionOverride", "Crypto", "GroupDef", "AclDef", "AclBinding",
	} {
		if err := enforceScope(as(memberP), grpcMethodPrefix+rpc); status.Code(err) != codes.PermissionDenied {
			t.Errorf("member on %s: %v, want PermissionDenied", rpc, err)
		}
	}
}

// The member path is for a NON-isolated principal: an isolated user stays
// where it was — its own credentials, nothing of the tenant plane.
func TestMemberPath_LeavesAnIsolatedUserWhereItWas(t *testing.T) {
	for _, rpc := range []string{
		"AgentDef", "TeamDef", "Path", "Document", "Memory", "History", "UsageReport", "TokenLimit",
		"DirectoryUsers", "ListTeamChannels", "AwaitChannels", "BroadcastChannels", "Crypto",
	} {
		if err := enforceScope(as(isolatedP), grpcMethodPrefix+rpc); status.Code(err) != codes.PermissionDenied {
			t.Errorf("isolated user on %s: %v, want PermissionDenied", rpc, err)
		}
	}
	if err := enforceScope(as(isolatedP), grpcMethodPrefix+"CredentialDef"); err != nil {
		t.Errorf("isolated user on CredentialDef: %v, want admitted (RFC CN self-service)", err)
	}
}

// The Memory RPC is tenant-confined like its HTTP routes (/v1/_memory/*), not
// left at the admin default an unmapped RPC gets.
func TestMemberPath_MemoryIsTenantConfinedNotAdminOnly(t *testing.T) {
	if got := requiredScopeForRPC(grpcMethodPrefix + "Memory"); got != auth.ScopeTenant {
		t.Errorf("Memory requires %q, want substrate:tenant", got)
	}
	for _, p := range []auth.Principal{operatorP, adminP} {
		if err := enforceScope(as(p), grpcMethodPrefix+"Memory"); err != nil {
			t.Errorf("%v on Memory: %v, want admitted", p.Scopes, err)
		}
	}
}

// TokenLimit carries its operation in the request: listing budgets is open to
// a member (GET /v1/_limits), setting and deleting them is not (PUT / DELETE
// are carved out). The interceptor holds that, before the handler runs.
func TestMemberPath_TokenLimitWritesStayOperatorOnly(t *testing.T) {
	principals := map[string]auth.Principal{"member": memberP, "operator": operatorP}
	s := New(Config{
		AuthConfigured: func(context.Context) bool { return true },
		PrincipalResolver: func(_ context.Context, bearer string) (auth.Principal, bool) {
			p, ok := principals[bearer]
			return p, ok
		},
	})
	call := func(bearer, op string) (reached bool, err error) {
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+bearer))
		_, err = s.UnaryAuthInterceptor()(ctx, &loomcyclepb.TokenLimitRequest{Op: op},
			&googlegrpc.UnaryServerInfo{FullMethod: grpcMethodPrefix + "TokenLimit"},
			func(context.Context, any) (any, error) { reached = true; return nil, nil })
		return reached, err
	}
	if reached, err := call("member", "list"); err != nil || !reached {
		t.Errorf("member list: reached=%v err=%v, want admitted", reached, err)
	}
	for _, op := range []string{"set", "delete"} {
		if reached, err := call("member", op); status.Code(err) != codes.PermissionDenied || reached {
			t.Errorf("member %s: reached=%v err=%v, want PermissionDenied before the handler", op, reached, err)
		}
		if reached, err := call("operator", op); err != nil || !reached {
			t.Errorf("operator %s: reached=%v err=%v, want admitted", op, reached, err)
		}
	}
	// An op the table does not know is refused to a member rather than let
	// through: the handler would reject it anyway, and the gate must not
	// depend on that.
	if _, err := call("member", "purge"); status.Code(err) != codes.PermissionDenied {
		t.Errorf("member unknown op: %v, want PermissionDenied", err)
	}
}

// A team's own channels are channels: the HTTP routes ask a member for the
// channel scope the operation needs, beyond the team gate. So does gRPC, or a
// token granted only runs:read would read a team's messages here.
func TestMemberPath_TeamChannelsAskTheChannelScope(t *testing.T) {
	mc := &teamChannelMock{}
	s := New(Config{Connector: mc})
	runsOnly := auth.Principal{TenantID: "acme", Subject: "bob", Scopes: []string{auth.ScopeRunsRead}}
	readOnly := auth.Principal{TenantID: "acme", Subject: "bob", Scopes: []string{auth.ScopeRunsRead, auth.ScopeChannelRead}}

	if _, err := s.ListTeamChannels(as(runsOnly), &loomcyclepb.ListTeamChannelsRequest{Team: "t"}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("runs-only list: %v, want PermissionDenied", err)
	}
	if _, err := s.PeekTeamChannel(as(runsOnly), &loomcyclepb.PeekTeamChannelRequest{Team: "t", Name: "n"}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("runs-only peek: %v, want PermissionDenied", err)
	}
	if _, err := s.ReleaseTeamChannel(as(readOnly), &loomcyclepb.ReleaseTeamChannelRequest{Team: "t", Name: "n"}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("channel:read-only release: %v, want PermissionDenied (a release is a channel write)", err)
	}
	if _, err := s.ListTeamChannels(as(readOnly), &loomcyclepb.ListTeamChannelsRequest{Team: "t"}); err != nil {
		t.Errorf("channel:read list: %v, want admitted", err)
	}
	for _, p := range []auth.Principal{memberP, operatorP} {
		mc.releaseReq = connector.TeamChannelReleaseRequest{}
		if _, err := s.ReleaseTeamChannel(as(p), &loomcyclepb.ReleaseTeamChannelRequest{Team: "t", Name: "n"}); err != nil || mc.releaseReq.Team != "t" {
			t.Errorf("%v release: %v (connector asked %+v), want admitted", p.Scopes, err, mc.releaseReq)
		}
	}
}
