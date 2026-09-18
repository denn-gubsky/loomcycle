package tools

import (
	"context"
	"reflect"
	"testing"
)

func runCtx(id RunIdentityValue) context.Context {
	return WithRunIdentity(context.Background(), id)
}

func TestEffectiveMemoryScopes_DefaultsToWhatTheCallerOwns(t *testing.T) {
	for _, tc := range []struct {
		name     string
		id       RunIdentityValue
		declared []string
		want     []string
	}{
		{
			name: "absent gate, tenant member — own data plus the tenant plane",
			id:   RunIdentityValue{UserID: "u1", TenantID: "acme"},
			want: []string{"user", "tenant"},
		},
		{
			// The default NAMES tenant, but ConfineIsolatedScope refuses it for an
			// isolated run. Returning it here would be honest about intent and wrong
			// about effect — a grant that reads as given and is then refused
			// downstream is the same silent uselessness this default removes.
			name: "absent gate, ISOLATED member — tenant is not offered",
			id:   RunIdentityValue{UserID: "u1", TenantID: "acme", Isolated: true},
			want: []string{"user"},
		},
		{
			name: "absent gate, no tenant — user only",
			id:   RunIdentityValue{UserID: "u1"},
			want: []string{"user"},
		},
		{
			// `user` resolves its scope_id FROM the user id. Defaulting a userless
			// run to it would hand back a grant that cannot resolve.
			name: "absent gate, NO user — stays deny, there is nothing it owns",
			id:   RunIdentityValue{TenantID: "acme"},
			want: nil,
		},
		{
			name:     "a declared list is used verbatim — defaults never widen",
			id:       RunIdentityValue{UserID: "u1", TenantID: "acme"},
			declared: []string{"agent"},
			want:     []string{"agent"},
		},
		{
			// The operator's way to say no, now that empty means "default".
			name:     "explicit deny-all is honoured",
			id:       RunIdentityValue{UserID: "u1", TenantID: "acme"},
			declared: []string{DenyAllScopes},
			want:     nil,
		},
		{
			name:     "deny-all wins even when other scopes are listed beside it",
			id:       RunIdentityValue{UserID: "u1", TenantID: "acme"},
			declared: []string{"agent", DenyAllScopes},
			want:     nil,
		},
		{
			// The empty-list case that a substrate round-trip cannot distinguish
			// from absence, which is exactly why DenyAllScopes exists.
			name:     "an explicitly empty list takes the default, like absence",
			id:       RunIdentityValue{UserID: "u1"},
			declared: []string{},
			want:     []string{"user"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := EffectiveMemoryScopes(runCtx(tc.id), tc.declared)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEffectiveHistoryScopes_DefaultsToTheCallersOwnChats(t *testing.T) {
	for _, tc := range []struct {
		name     string
		id       RunIdentityValue
		declared []string
		want     []string
	}{
		{
			// NOT `self`: that is the AGENT's chats across every user, which is not
			// what the caller owns.
			name: "absent gate — the user's own chats, and nothing wider",
			id:   RunIdentityValue{UserID: "u1", TenantID: "acme"},
			want: []string{"user"},
		},
		{
			name: "absent gate, no user — stays deny",
			id:   RunIdentityValue{TenantID: "acme"},
			want: nil,
		},
		{
			name:     "a declared list is used verbatim",
			id:       RunIdentityValue{UserID: "u1"},
			declared: []string{"self"},
			want:     []string{"self"},
		},
		{
			name:     "explicit deny-all is honoured",
			id:       RunIdentityValue{UserID: "u1"},
			declared: []string{DenyAllScopes},
			want:     nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := EffectiveHistoryScopes(runCtx(tc.id), tc.declared)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// The resolver must not MUTATE the definition's slice. It is handed the live
// field off a shared config.AgentDef, and an append that happened to fit in the
// existing capacity would write through to every later run of that agent.
func TestEffectiveMemoryScopes_DoesNotAliasTheDefinitionsSlice(t *testing.T) {
	declared := make([]string, 0, 4) // capacity to spare — an append would land in it
	declared = append(declared, "agent")
	before := append([]string(nil), declared...)

	got := EffectiveMemoryScopes(runCtx(RunIdentityValue{UserID: "u1", TenantID: "acme"}), declared)
	_ = append(got, "mutated")

	// Compare the BACKING ARRAY, not the length-view. An append into spare
	// capacity leaves len() unchanged, so `reflect.DeepEqual(declared, before)`
	// passes over a definition that was just rewritten — this assertion was
	// written that way first and was vacuous.
	if arr := declared[:cap(declared)]; arr[len(before)] != "" {
		t.Errorf("the definition's backing array changed to %v — the resolver handed back "+
			"an aliasing slice, so one run's policy can rewrite the agent's own definition", arr)
	}
	if !reflect.DeepEqual(declared, before) {
		t.Errorf("the definition's slice changed to %v (was %v)", declared, before)
	}
}

func TestEffectiveSqlScopes_DefaultsToTheCallersOwnDatabase(t *testing.T) {
	for _, tc := range []struct {
		name     string
		id       RunIdentityValue
		declared []string
		want     []string
	}{
		{
			// NOT `agent`, though it is tempting: that database is durable and
			// shared across every run of the agent, so defaulting it hands out
			// cross-run state nobody asked for. NOT `tenant` either — a tenant
			// Document write needs the grant on BOTH memory_scopes and
			// sql_scopes, and half a capability fails more confusingly than none.
			name: "absent gate — the user's own database, and nothing wider",
			id:   RunIdentityValue{UserID: "u1", TenantID: "acme"},
			want: []string{"user"},
		},
		{
			name: "absent gate, no user — stays deny",
			id:   RunIdentityValue{TenantID: "acme"},
			want: nil,
		},
		{
			name:     "a declared list is used verbatim",
			id:       RunIdentityValue{UserID: "u1"},
			declared: []string{"agent", "run"},
			want:     []string{"agent", "run"},
		},
		{
			name:     "explicit deny-all is honoured",
			id:       RunIdentityValue{UserID: "u1"},
			declared: []string{DenyAllScopes},
			want:     nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := EffectiveSqlScopes(runCtx(tc.id), tc.declared)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEffectiveEvaluationScopes_DefaultsToJudgingOnlyItself(t *testing.T) {
	for _, tc := range []struct {
		name     string
		declared []string
		want     []string
	}{
		{
			// Every other value crosses to another run or another agent:
			// submit_siblings, submit_descendants, submit_any and read_any are
			// all reach, and stay default-deny.
			name: "absent gate — its own run, and nothing else",
			want: []string{"submit_self"},
		},
		{
			name:     "a declared list is used verbatim and is not widened",
			declared: []string{"read_any"},
			want:     []string{"read_any"},
		},
		{
			name:     "explicit deny-all is honoured",
			declared: []string{DenyAllScopes},
			want:     nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := EffectiveEvaluationScopes(tc.declared)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// The same aliasing hazard the memory resolver had: these are handed the LIVE
// slice off a shared config.AgentDef.
func TestEffectiveSqlScopes_DoesNotAliasTheDefinitionsSlice(t *testing.T) {
	declared := make([]string, 0, 4)
	declared = append(declared, "agent")
	before := append([]string(nil), declared...)

	got := EffectiveSqlScopes(runCtx(RunIdentityValue{UserID: "u1"}), declared)
	_ = append(got, "mutated")

	if arr := declared[:cap(declared)]; arr[len(before)] != "" {
		t.Errorf("the definition's backing array changed to %v — the resolver aliased it", arr)
	}
}
