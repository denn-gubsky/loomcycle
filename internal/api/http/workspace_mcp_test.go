package http

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
)

// RFC GW D35 through connector.Workspace, the path the MCP server's
// `workspace` tool takes: the owner is the authenticated principal's tenant
// and subject, never anything in the input.

func wsMCPFixture(t *testing.T, mcp config.WorkspaceMCPConfig) *wsRunFixture {
	t.Helper()
	f := newWSRunFixture(t, map[string]config.AgentDef{}, nil)
	cfg := f.srv.cfg()
	cfg.Workspace.MCP = mcp
	return f
}

func wsPrincipalCtx(tenant, subject, token string, scopes ...string) context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{TenantID: tenant, Subject: subject, TokenDefID: token, Scopes: scopes})
}

func wsMCPCall(t *testing.T, f *wsRunFixture, ctx context.Context, input string) (string, bool) {
	t.Helper()
	res, err := f.srv.Workspace(ctx, json.RawMessage(input))
	if err != nil {
		t.Fatalf("%s: %v", input, err)
	}
	return res.Text, res.IsError
}

// The switch, the principal and the scope are each checked before anything
// reaches a worker.
func TestWorkspaceMCP_RefusedWithoutTheSwitchAPrincipalOrTheScope(t *testing.T) {
	off := wsMCPFixture(t, config.WorkspaceMCPConfig{})
	if text, isErr := wsMCPCall(t, off, wsPrincipalCtx("acme", "alice", "otd_1", auth.ScopeRunsCreate), `{"op":"list"}`); !isErr || !strings.Contains(text, "workspaces_off") {
		t.Errorf("switch off: %s", text)
	}
	f := wsMCPFixture(t, config.WorkspaceMCPConfig{Enabled: true})
	for name, ctx := range map[string]context.Context{
		"no principal": context.Background(),
		"legacy token": auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "default", Subject: "operator", Legacy: true, Scopes: []string{auth.ScopeAdmin}}),
		"no subject":   wsPrincipalCtx("acme", "", "otd_1", auth.ScopeRunsCreate),
		"no tenant":    wsPrincipalCtx("", "alice", "otd_1", auth.ScopeRunsCreate),
	} {
		if text, isErr := wsMCPCall(t, f, ctx, `{"op":"open","name":"w"}`); !isErr || !strings.Contains(text, "no_principal") {
			t.Errorf("%s: %s, want no_principal", name, text)
		}
	}
	if text, isErr := wsMCPCall(t, f, wsPrincipalCtx("acme", "alice", "otd_1", auth.ScopeRunsRead), `{"op":"open","name":"w"}`); !isErr || !strings.Contains(text, "not_granted") {
		t.Errorf("without runs:create: %s", text)
	}
	if dirs := f.workspaceDirs(t); len(dirs) != 0 {
		t.Errorf("a refused call made workspaces %v", dirs)
	}
}

// A subject's workspace is reached by any session of that subject and by no
// other subject of the tenant; exec needs the operator to list it.
func TestWorkspaceMCP_TheSubjectOwnsItAndExecIsTheOperatorsToGrant(t *testing.T) {
	f := wsMCPFixture(t, config.WorkspaceMCPConfig{Enabled: true})
	alice := wsPrincipalCtx("acme", "alice", "otd_1", auth.ScopeRunsCreate)
	if text, isErr := wsMCPCall(t, f, alice, `{"op":"open","name":"w"}`); isErr {
		t.Fatalf("open: %s", text)
	}
	if text, isErr := wsMCPCall(t, f, alice, `{"op":"write","name":"w","path":"a.txt","content":"hello"}`); isErr {
		t.Fatalf("write: %s", text)
	}
	// Another token of alice: the same workspace.
	if text, isErr := wsMCPCall(t, f, wsPrincipalCtx("acme", "alice", "otd_2", auth.ScopeRunsCreate), `{"op":"read","name":"w","path":"a.txt"}`); isErr || !strings.Contains(text, "hello") {
		t.Errorf("alice's other token: %s", text)
	}
	// Bob of the same tenant, and alice of another tenant: not found.
	for name, ctx := range map[string]context.Context{
		"bob":           wsPrincipalCtx("acme", "bob", "otd_3", auth.ScopeRunsCreate),
		"other tenant":  wsPrincipalCtx("other", "alice", "otd_4", auth.ScopeRunsCreate),
		"a tenant op":   wsPrincipalCtx("acme", "carol", "otd_5", auth.ScopeTenant, auth.ScopeRunsCreate),
		"another admin": wsPrincipalCtx("acme", "root", "otd_6", auth.ScopeAdmin),
	} {
		if text, isErr := wsMCPCall(t, f, ctx, `{"op":"read","name":"w","path":"a.txt"}`); !isErr || !strings.Contains(text, "not_found") || strings.Contains(text, "hello") {
			t.Errorf("%s: %s, want not_found", name, text)
		}
		if text, _ := wsMCPCall(t, f, ctx, `{"op":"list"}`); strings.Contains(text, `"w"`) {
			t.Errorf("%s lists alice's workspace: %s", name, text)
		}
	}
	// exec is not in the default ops.
	if text, isErr := wsMCPCall(t, f, alice, `{"op":"exec","name":"w","command":"echo hi"}`); !isErr || !strings.Contains(text, "not_granted") {
		t.Errorf("exec with the default ops: %s", text)
	}
	// Ops of a run are refused by name.
	for _, in := range []string{`{"op":"share","name":"w","with":"x","access":"read"}`, `{"op":"run_cell","name":"w","code":"1"}`, `{"op":"read","name":"w@someone","path":"a.txt"}`} {
		if text, isErr := wsMCPCall(t, f, alice, in); !isErr || !(strings.Contains(text, "not_over_mcp") || strings.Contains(text, "not_granted")) {
			t.Errorf("%s: %s, want refused", in, text)
		}
	}
	execOK := wsMCPFixture(t, config.WorkspaceMCPConfig{Enabled: true, Ops: []string{"read", "exec"}})
	if text, isErr := wsMCPCall(t, execOK, alice, `{"op":"open","name":"w"}`); isErr {
		t.Fatalf("open: %s", text)
	}
	if text, isErr := wsMCPCall(t, execOK, alice, `{"op":"exec","name":"w","command":"echo hi"}`); isErr || !strings.Contains(text, "hi") {
		t.Errorf("exec with exec listed: %s", text)
	}
	if text, isErr := wsMCPCall(t, execOK, alice, `{"op":"write","name":"w","path":"b.txt","content":"x"}`); !isErr || !strings.Contains(text, "not_granted") {
		t.Errorf("write without write listed: %s", text)
	}
}
