package hooks

import (
	"context"
	"encoding/json"
	"errors"
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

// storedDefLookup serves one stored HookDef answering phase, whose webhook is
// url, from the shared tenant — the store row a yaml agent's ref resolves to.
func storedDefLookup(url string, phase Phase) LookupDef {
	return func(_ context.Context, tenant, name string, _ int) (Def, string, error) {
		if tenant != "" || name != "stored" {
			return Def{}, "", DefNotFound(errors.New("no HookDef " + name))
		}
		return Def{Event: phase, FailMode: FailClosed, Body: DefBody{Kind: BodyKindHTTP, URL: url}}, "hdf_stored", nil
	}
}

// A static (yaml) agent that names a STORED HookDef by ref dials it through the
// guard: the def is a store row, writable on the runtime plane, so the yaml
// that names it does not make its URL the operator's config.
func TestDispatcher_StaticAgentRefToStoredDefCannotReachAPrivateAddress(t *testing.T) {
	hook := newFakeHook(t, `{"input":{"rewritten":true}}`)
	set := NewSet()
	src := Source{Owner: "agent:a", Tenant: "", OperatorAuthored: true, OperatorConfig: true}
	ref := EventHooks{PhasePre: {{Ref: "stored"}}}
	if err := Resolve(context.Background(), src, ref, nil, storedDefLookup(hook.srv.URL, PhasePre), NewPermits(nil), set); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	NewDispatcher(set, nil).RunPre(context.Background(), Identity{Agent: "a"},
		ToolCall{ID: "t1", Name: "WebFetch", Input: json.RawMessage(`{"secret":"tool input"}`)})
	if n := atomic.LoadInt32(&hook.calls); n != 0 {
		t.Fatalf("a stored HookDef named by a static agent reached a loopback callback %d time(s)", n)
	}
}

// Census: on every resolution path (an agent's or a run's hooks through
// Resolve, a channel's through ResolveChannel), whatever the source, a hook
// resolved from a stored HookDef never dials unguarded; only an inline webhook
// of a static definition in the shared tenant does.
func TestResolve_OnlyAStaticDefinitionsInlineWebhookDialsUnguarded(t *testing.T) {
	const url = "http://127.0.0.1:1/hook"
	type resolver func(src Source, e EventHooks, lookup LookupDef, set *Set) error
	paths := map[string]struct {
		phase   Phase
		resolve resolver
	}{
		"Resolve": {PhasePre, func(src Source, e EventHooks, lookup LookupDef, set *Set) error {
			return Resolve(context.Background(), src, e, nil, lookup, NewPermits(nil), set)
		}},
		"Resolve/tool": {PhasePre, func(src Source, e EventHooks, lookup LookupDef, set *Set) error {
			return Resolve(context.Background(), src, nil, ToolHooks{"WebFetch": e}, lookup, NewPermits(nil), set)
		}},
		"ResolveChannel": {PhaseChannelPublish, func(src Source, e EventHooks, lookup LookupDef, set *Set) error {
			return ResolveChannel(context.Background(), src, e, lookup, set)
		}},
	}
	for name, p := range paths {
		for _, tenant := range []string{"", "t1"} {
			for _, static := range []bool{false, true} {
				for _, kind := range []string{"inline", "ref"} {
					src := Source{Owner: "agent:a", Tenant: tenant, OperatorAuthored: true, OperatorConfig: static}
					entry := Entry{Ref: "stored"}
					if kind == "inline" {
						entry = Entry{Inline: &Inline{Name: "gate", URL: url, FailMode: FailClosed}}
					}
					set := NewSet()
					if err := p.resolve(src, EventHooks{p.phase: {entry}}, storedDefLookup(url, p.phase), set); err != nil {
						t.Fatalf("%s tenant=%q static=%v %s: resolve: %v", name, tenant, static, kind, err)
					}
					hs := set.List()
					if len(hs) != 1 {
						t.Fatalf("%s: resolved %d hooks, want 1", name, len(hs))
					}
					want := kind == "inline" && static && tenant == ""
					if got := hs[0].OperatorConfig; got != want {
						t.Errorf("%s tenant=%q static=%v %s: OperatorConfig=%v, want %v", name, tenant, static, kind, got, want)
					}
				}
			}
		}
	}
}
