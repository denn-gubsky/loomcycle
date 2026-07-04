package a2a

import (
	"context"
	"errors"
	"net/http"
	"testing"

	a2asdk "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	bridge "github.com/denn-gubsky/loomcycle/internal/a2a"
)

// TestPrincipalInterceptor_RejectsUnauthenticated pins the auth-enforcement
// fix: when an Authenticator is configured and the request fails it, the
// interceptor REJECTS at the frontier (a non-nil Before error short-circuits
// the SDK call before the executor runs), rather than letting the request
// proceed as an anonymous run. The binding endpoints are deliberately not
// wrapped by the HTTP bearer authMiddleware, so this interceptor is the
// only auth gate for the entire inbound A2A surface.
func TestPrincipalInterceptor_RejectsUnauthenticated(t *testing.T) {
	newCtx := func() *a2asrv.CallContext {
		_, cc := a2asrv.NewCallContext(context.Background(), a2asrv.NewServiceParams(http.Header{}))
		return cc
	}

	// Auth configured but the credential is rejected → ErrUnauthenticated,
	// principal left unauthenticated.
	denied := principalInterceptor{auth: func(http.Header) (string, bool, bool) { return "", false, false }}
	cc := newCtx()
	if _, _, err := denied.Before(context.Background(), cc, &a2asrv.Request{}); !errors.Is(err, a2asdk.ErrUnauthenticated) {
		t.Fatalf("Before err = %v, want ErrUnauthenticated", err)
	}
	if cc.User == nil || cc.User.Authenticated {
		t.Errorf("User = %#v, want Authenticated=false on rejection", cc.User)
	}

	// Auth configured and the credential is accepted → no error, principal
	// stamped with the resolved name.
	ok := principalInterceptor{auth: func(http.Header) (string, bool, bool) { return "alice", false, true }}
	cc = newCtx()
	newCtxOut, _, err := ok.Before(context.Background(), cc, &a2asrv.Request{})
	if err != nil {
		t.Fatalf("Before err = %v, want nil on accepted auth", err)
	}
	if cc.User == nil || !cc.User.Authenticated || cc.User.Name != "alice" {
		t.Errorf("User = %#v, want authenticated alice", cc.User)
	}
	// RFC AX: a non-restricted peer stamps restricted=false on the returned ctx.
	if bridge.OperatorKeyRestrictedFrom(newCtxOut) {
		t.Error("non-restricted peer must stamp OperatorKeyRestricted=false")
	}

	// A RESTRICTED peer → the returned ctx carries the bit for the executor.
	restricted := principalInterceptor{auth: func(http.Header) (string, bool, bool) { return "bob", true, true }}
	cc = newCtx()
	rctx, _, err := restricted.Before(context.Background(), cc, &a2asrv.Request{})
	if err != nil {
		t.Fatalf("Before err = %v, want nil on accepted (restricted) auth", err)
	}
	if !bridge.OperatorKeyRestrictedFrom(rctx) {
		t.Error("restricted peer must stamp OperatorKeyRestricted=true on the ctx")
	}

	// No authenticator (open/dev mode) → anonymous authenticated, no error.
	open := principalInterceptor{auth: nil}
	cc = newCtx()
	if _, _, err := open.Before(context.Background(), cc, &a2asrv.Request{}); err != nil {
		t.Fatalf("open-mode Before err = %v, want nil", err)
	}
	if cc.User == nil || !cc.User.Authenticated {
		t.Errorf("open-mode User = %#v, want authenticated anonymous", cc.User)
	}
}
