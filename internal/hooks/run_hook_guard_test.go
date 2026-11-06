package hooks

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
)

// resolvedLoopbackHook resolves one fail-closed pre hook on the fake loopback
// server the way the runtime does, from src's definition.
func resolvedLoopbackHook(t *testing.T, src Source) (*fakeHook, *Set) {
	t.Helper()
	hook := newFakeHook(t, `{"input":{"rewritten":true}}`)
	set := NewSet()
	added := EventHooks{PhasePre: {{Inline: &Inline{Name: "gate", URL: hook.srv.URL, FailMode: FailClosed}}}}
	if err := Resolve(context.Background(), src, added, nil, nil, NewPermits(nil), set); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return hook, set
}

// A hook a run request adds is the caller's, not the operator's, even in the
// shared tenant (open mode, the legacy token, an admin): it must not reach a
// private or metadata address, or POST /v1/runs is a blind SSRF.
func TestDispatcher_RunAddedHookCannotReachAPrivateAddressInSharedTenant(t *testing.T) {
	hook, set := resolvedLoopbackHook(t, Source{Owner: "run", Tenant: ""})
	out := NewDispatcher(set, nil).RunPre(context.Background(),
		Identity{Agent: "a"},
		ToolCall{ID: "t1", Name: "WebFetch", Input: json.RawMessage(`{"secret":"tool input"}`)},
	)
	if n := atomic.LoadInt32(&hook.calls); n != 0 {
		t.Fatalf("a run-added hook reached a loopback callback %d time(s)", n)
	}
	if out.Deny == nil || !out.Deny.IsError {
		t.Errorf("a refused fail-closed hook must deny the call, got %+v", out)
	}
}

// An operator-authored definition that is not the operator's static config
// (an AgentDef written on the operator plane) is dialed through the guard too.
func TestDispatcher_RuntimeDefinitionHookCannotReachAPrivateAddress(t *testing.T) {
	hook, set := resolvedLoopbackHook(t, Source{Owner: "agent:a", Tenant: "", OperatorAuthored: true})
	NewDispatcher(set, nil).RunPre(context.Background(), Identity{Agent: "a"},
		ToolCall{ID: "t1", Name: "WebFetch", Input: json.RawMessage(`{}`)})
	if n := atomic.LoadInt32(&hook.calls); n != 0 {
		t.Fatalf("a runtime definition's hook reached a loopback callback %d time(s)", n)
	}
}
