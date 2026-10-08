package builtin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

func TestTeamDefMissingScope_HoldsRunOpsToTheRunScopes(t *testing.T) {
	principal := func(scopes ...string) context.Context {
		return auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "bob", Scopes: scopes})
	}
	for _, tc := range []struct {
		name  string
		ctx   context.Context
		input string
		op    string
		scope string
	}{
		{"no principal runs", context.Background(), `{"op":"run"}`, "", ""},
		{"read-only member cannot run", principal(auth.ScopeRunsRead), `{"op":"run"}`, "run", auth.ScopeRunsCreate},
		{"read-only member cannot cancel", principal(auth.ScopeRunsRead), `{"op":"cancel"}`, "cancel", auth.ScopeRunsCreate},
		{"read-only member polls", principal(auth.ScopeRunsRead), `{"op":"poll"}`, "", ""},
		{"create-only member runs", principal(auth.ScopeRunsCreate), `{"op":"run"}`, "", ""},
		{"create-only member cannot poll", principal(auth.ScopeRunsCreate), `{"op":"poll"}`, "poll", auth.ScopeRunsRead},
		{"scopeless member cannot run", principal(), `{"op":"run"}`, "run", auth.ScopeRunsCreate},
		{"tenant operator runs", principal(auth.ScopeTenant), `{"op":"run"}`, "", ""},
		{"tenant operator polls", principal(auth.ScopeTenant), `{"op":"poll"}`, "", ""},
		{"admin runs", principal(auth.ScopeAdmin), `{"op":"run"}`, "", ""},
		{"isolated user holds both run scopes", principal(auth.ScopeUser), `{"op":"poll"}`, "", ""},
		// The op is read as Execute reads it: any key case, last key wins.
		{"upper-case key", principal(auth.ScopeRunsRead), `{"OP":"run"}`, "run", auth.ScopeRunsCreate},
		{"repeated key", principal(auth.ScopeRunsRead), `{"op":"get","op":"run"}`, "run", auth.ScopeRunsCreate},
		{"unreadable input is Execute's to report", principal(), `{"op":7}`, "", ""},
		{"unknown op is Execute's to report", principal(), `{"op":"launch"}`, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op, scope := TeamDefMissingScope(tc.ctx, json.RawMessage(tc.input))
			if op != tc.op || scope != tc.scope {
				t.Errorf("op=%q scope=%q, want %q / %q", op, scope, tc.op, tc.scope)
			}
		})
	}
}

// Every definition op is open to a member whatever its scopes: only the run
// ops are held. Read off the tool's own op list, so a new op shows up here.
func TestTeamDefMissingScope_LeavesDefinitionOpsAlone(t *testing.T) {
	var schema struct {
		Properties struct {
			Op struct {
				Enum []string `json:"enum"`
			} `json:"op"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(teamDefInputSchema), &schema); err != nil || len(schema.Properties.Op.Enum) == 0 {
		t.Fatalf("the tool's op list: %v", err)
	}
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "bob"})
	held := map[string]bool{}
	for _, op := range schema.Properties.Op.Enum {
		in, _ := json.Marshal(map[string]string{"op": op})
		if _, scope := TeamDefMissingScope(ctx, in); scope != "" {
			held[op] = true
		}
	}
	if len(held) != 3 || !held["run"] || !held["cancel"] || !held["poll"] {
		t.Errorf("ops held to a run scope = %v, want exactly run, cancel and poll", held)
	}
}
