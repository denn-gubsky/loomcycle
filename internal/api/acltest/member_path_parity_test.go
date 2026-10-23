package acltest

import (
	"context"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	lcgrpc "github.com/denn-gubsky/loomcycle/internal/api/grpc"
	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
	"github.com/denn-gubsky/loomcycle/internal/api/memberpath"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// RFC EZ Q1: a principal is admitted to a gRPC RPC exactly when it is admitted
// to the RPC's HTTP twin route — for a member, on the member path; for the
// other classes, by their scopes. Every RPC the service registers is either
// compared here through its twins (internal/api/memberpath) or listed there
// without one, with the reason. The comparison asks each transport's own gate
// functions (http.RouteAdmits, grpc.RPCAdmits), which the middleware and the
// interceptors run, under compat with no ACL rows.
func TestMemberPath_GRPCAdmitsExactlyWhatItsHTTPTwinAdmits(t *testing.T) {
	principals := map[string]auth.Principal{
		"member":          {TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead, auth.ScopeChannelPublish, auth.ScopeChannelRead}},
		"tenant operator": {TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}},
		"admin":           {TenantID: "acme", Subject: "root", Scopes: []string{auth.ScopeAdmin}},
		"isolated user":   {TenantID: "acme", Subject: "ivy", Scopes: []string{auth.ScopeUser}},
	}
	// Where an isolated user is admitted on HTTP and not on gRPC. RFC EZ Q1
	// gives gRPC the MEMBER path and leaves isolated users where they were;
	// these are the two places that leaves, listed so that closing one fails
	// here until its entry goes.
	isolatedOnlyOnHTTP := map[string]string{
		"DirectoryUsers": "GET /v1/_users is open to any authenticated principal; the RPC asks substrate:tenant",
		"History":        "POST /v1/_history asks runs:read, which substrate:user implies; the RPC asks substrate:tenant",
	}
	diverged := map[string]bool{}
	var unary, streams []string
	for _, m := range loomcyclepb.Loomcycle_ServiceDesc.Methods {
		unary = append(unary, m.MethodName)
	}
	for _, s := range loomcyclepb.Loomcycle_ServiceDesc.Streams {
		streams = append(streams, s.StreamName)
	}
	registered := append(append([]string(nil), unary...), streams...)
	noTwin := memberpath.NoTwin()
	for _, rpc := range registered {
		twins := memberpath.Twins(rpc)
		if len(twins) == 0 {
			if _, ok := noTwin[rpc]; !ok {
				t.Errorf("%s has no HTTP twin in internal/api/memberpath and no reason for that", rpc)
			}
			continue
		}
		// The op is read from the request by the unary interceptor only.
		if memberpath.ByOp(rpc) && slices.Contains(streams, rpc) {
			t.Errorf("%s is op-keyed but streaming: the stream interceptor cannot read its op", rpc)
		}
		for class, p := range principals {
			for _, tw := range twins {
				h := lchttp.RouteAdmits(p, tw.Method, tw.Path)
				g := lcgrpc.RPCAdmits(p, rpc, tw.Op)
				if class == "isolated user" && h && !g {
					if _, known := isolatedOnlyOnHTTP[rpc]; known {
						diverged[rpc] = true
						continue
					}
				}
				if h != g {
					t.Errorf("%s: %s (op %q) admitted=%v on gRPC, but its twin %s %s admitted=%v on HTTP",
						class, rpc, tw.Op, g, tw.Method, tw.Path, h)
				}
			}
		}
	}
	for rpc := range isolatedOnlyOnHTTP {
		if !diverged[rpc] {
			t.Errorf("%s no longer admits an isolated user only on HTTP; drop it from isolatedOnlyOnHTTP", rpc)
		}
	}
	for _, rpc := range memberpath.TwinnedRPCs() {
		if !slices.Contains(registered, rpc) {
			t.Errorf("memberpath lists twins for %s, which the service does not register", rpc)
		}
	}
	for rpc := range noTwin {
		if !slices.Contains(registered, rpc) {
			t.Errorf("memberpath lists %s as twinless, which the service does not register", rpc)
		}
	}
}

// The member path over a real gRPC connection, behind the real interceptors,
// in compat and in enforce (where the baseline member ACL decides): a member
// reads usage, budgets and the user list and authors definitions, and is
// refused budget writes, token minting, runtime admin and access-control
// management. A tenant operator keeps budget writes.
func TestMemberPath_AMemberCallsItsRPCsOverGRPC(t *testing.T) {
	for _, mode := range []authz.Mode{authz.ModeCompat, authz.ModeEnforce} {
		t.Run(string(mode), func(t *testing.T) {
			e := newEnv(t, mode)
			as := func(bearer string) context.Context {
				return metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+bearer)
			}
			refusedAtTheGate := func(err error) bool {
				st := status.Convert(err)
				return st.Code() == codes.PermissionDenied &&
					(strings.Contains(st.Message(), "insufficient scope") || strings.HasPrefix(st.Message(), "acl_denied"))
			}
			hard := int64(1000)
			admitted := map[string]func(context.Context) error{
				"UsageReport": func(ctx context.Context) error {
					_, err := e.grpc.UsageReport(ctx, &loomcyclepb.UsageReportRequest{})
					return err
				},
				"TokenLimit list": func(ctx context.Context) error {
					_, err := e.grpc.TokenLimit(ctx, &loomcyclepb.TokenLimitRequest{Op: "list"})
					return err
				},
				"DirectoryUsers": func(ctx context.Context) error {
					_, err := e.grpc.DirectoryUsers(ctx, &loomcyclepb.DirectoryUsersRequest{})
					return err
				},
				"AgentDef": func(ctx context.Context) error {
					_, err := e.grpc.AgentDef(ctx, &loomcyclepb.SubstrateRequest{InputJson: []byte(`{"op":"list"}`)})
					return err
				},
			}
			for name, call := range admitted {
				if err := call(as(member)); refusedAtTheGate(err) {
					t.Errorf("member %s: %v, want past the gate", name, err)
				}
			}
			refused := map[string]func(context.Context) error{
				"TokenLimit set": func(ctx context.Context) error {
					_, err := e.grpc.TokenLimit(ctx, &loomcyclepb.TokenLimitRequest{Op: "set", Scope: "tenant", HardLimit: &hard})
					return err
				},
				"OperatorTokenDef": func(ctx context.Context) error {
					_, err := e.grpc.OperatorTokenDef(ctx, &loomcyclepb.SubstrateRequest{InputJson: []byte(`{"op":"list"}`)})
					return err
				},
				"PauseRuntime": func(ctx context.Context) error {
					_, err := e.grpc.PauseRuntime(ctx, &loomcyclepb.PauseRuntimeRequest{})
					return err
				},
				"GroupDef": func(ctx context.Context) error {
					_, err := e.grpc.GroupDef(ctx, &loomcyclepb.SubstrateRequest{InputJson: []byte(`{"op":"list"}`)})
					return err
				},
			}
			for name, call := range refused {
				if err := call(as(member)); !refusedAtTheGate(err) {
					t.Errorf("member %s: %v, want refused at the gate", name, err)
				}
			}
			if err := refused["TokenLimit set"](as(operator)); err != nil {
				t.Errorf("operator TokenLimit set: %v, want admitted", err)
			}
		})
	}
}
