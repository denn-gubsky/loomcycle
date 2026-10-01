package builtin

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// Contexts for the env-reference rule, built from a bare background so none
// inherits the fixture's operator marker.

func mcpTenantOperatorCtx(tenant string) context.Context {
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a_tenant", TenantID: tenant})
	return auth.WithPrincipal(ctx, auth.Principal{TenantID: tenant, Subject: "op", Scopes: []string{auth.ScopeTenant}})
}

func mcpAdminCtx(tenant string) context.Context {
	ctx := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a_admin", TenantID: tenant})
	return auth.WithPrincipal(ctx, auth.Principal{TenantID: tenant, Subject: "root", Scopes: []string{auth.ScopeAdmin}})
}

func mcpOpenModeOperatorCtx() context.Context {
	return tools.WithUnauthenticatedOperator(
		tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a_op"}))
}

// mcpEnvRuleFixture is mcpServerDefFixture with dynamic stdio enabled, so the
// stdio fields are reachable too.
func mcpEnvRuleFixture(t *testing.T) (*MCPServerDef, func()) {
	t.Helper()
	tool, _, done := mcpServerDefFixture(t)
	tool.Cfg.Env.MCPAllowDynamicStdio = true
	return tool, done
}

func storedOverlay(t *testing.T, tool *MCPServerDef, tenant, name string) mcpServerOverlay {
	t.Helper()
	row, err := tool.Store.MCPServerDefGetActive(context.Background(), tenant, name)
	if err != nil {
		t.Fatalf("GetActive(%q,%q): %v", tenant, name, err)
	}
	var ov mcpServerOverlay
	if err := json.Unmarshal(row.Definition, &ov); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return ov
}

func TestMCPServerDefAuthoring_NonOperatorCannotReferenceServerEnv(t *testing.T) {
	t.Setenv("LOOMCYCLE_PEER_KEY_GLOBEX", "globex-secret-value")
	t.Setenv("LOOMCYCLE_HOST_X", "n8n.example.com")
	refused := map[string]string{
		"header": `{"transport":"http","url":"https://n8n.example.com/mcp","headers":{"Authorization":"Bearer ${LOOMCYCLE_PEER_KEY_GLOBEX}"}}`,
		"url":    `{"transport":"http","url":"https://${LOOMCYCLE_HOST_X}/mcp"}`,
		"nested default": `{"transport":"http","url":"https://n8n.example.com/mcp",` +
			`"headers":{"Authorization":"Bearer ${run.credentials.x:-${LOOMCYCLE_PEER_KEY_GLOBEX}}"}}`,
		"stdio env":     `{"transport":"stdio","command":"mcp-peer","env":{"TOKEN":"${LOOMCYCLE_PEER_KEY_GLOBEX}"}}`,
		"stdio args":    `{"transport":"stdio","command":"mcp-peer","args":["--key","${LOOMCYCLE_PEER_KEY_GLOBEX}"]}`,
		"stdio command": `{"transport":"stdio","command":"${LOOMCYCLE_HOST_X}"}`,
	}
	authors := map[string]context.Context{
		"tenant operator":                  mcpTenantOperatorCtx("acme"),
		"in-run author with no principal":  tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a_run", TenantID: "acme"}),
		"tenant-less author, no principal": tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{AgentID: "a_run"}),
	}
	for field, overlay := range refused {
		for who, ctx := range authors {
			t.Run(field+"/"+who, func(t *testing.T) {
				tool, done := mcpEnvRuleFixture(t)
				defer done()
				res := execDef(t, tool, ctx, `{"op":"create","name":"peer","overlay":`+overlay+`}`)
				if !res.IsError || !strings.Contains(res.Text, "may be set only by an admin") {
					t.Fatalf("create was not refused: %s", res.Text)
				}
				if strings.Contains(res.Text, "globex-secret-value") {
					t.Fatalf("refusal leaked the env value: %s", res.Text)
				}
				if names, _ := tool.Store.MCPServerDefListNames(context.Background()); len(names) != 0 {
					t.Fatalf("a refused create stored %d name(s)", len(names))
				}
			})
		}
	}
}

func TestMCPServerDefAuthoring_NonOperatorMayUseRunAndCredRefs(t *testing.T) {
	allowed := map[string]string{
		"run credential":         `{"transport":"http","url":"https://n8n.example.com/mcp","headers":{"Authorization":"Bearer ${run.credentials.x}"}}`,
		"run credential default": `{"transport":"http","url":"https://n8n.example.com/mcp","headers":{"Authorization":"Bearer ${run.credentials.x:-anonymous}"}}`,
		"run bearer and ids":     `{"transport":"http","url":"https://n8n.example.com/mcp","headers":{"Authorization":"Bearer ${run.user_bearer}","X-Tenant":"${run.tenant_id}"}}`,
		"stored credential":      `{"transport":"http","url":"https://n8n.example.com/mcp","headers":{"Authorization":"Bearer $cred:x"}}`,
	}
	for name, overlay := range allowed {
		t.Run(name, func(t *testing.T) {
			tool, done := mcpEnvRuleFixture(t)
			defer done()
			if res := execDef(t, tool, mcpTenantOperatorCtx("acme"), `{"op":"create","name":"peer","overlay":`+overlay+`}`); res.IsError {
				t.Fatalf("create refused: %s", res.Text)
			}
			if storedOverlay(t, tool, "acme", "peer").OperatorAuthored {
				t.Error("a tenant operator's def was stamped operator_authored")
			}
		})
	}
}

func TestMCPServerDefAuthoring_OperatorMayReferenceServerEnv(t *testing.T) {
	overlay := `{"transport":"http","url":"https://n8n.example.com/mcp","headers":{"Authorization":"Bearer ${run.credentials.x:-${LOOMCYCLE_PEER_KEY_GLOBEX}}"}}`
	for who, c := range map[string]struct {
		ctx    context.Context
		tenant string
	}{
		"admin":                    {mcpAdminCtx("acme"), "acme"},
		"open-mode/stdio operator": {mcpOpenModeOperatorCtx(), ""},
	} {
		t.Run(who, func(t *testing.T) {
			tool, done := mcpEnvRuleFixture(t)
			defer done()
			if res := execDef(t, tool, c.ctx, `{"op":"create","name":"peer","overlay":`+overlay+`}`); res.IsError {
				t.Fatalf("create refused: %s", res.Text)
			}
			if !storedOverlay(t, tool, c.tenant, "peer").OperatorAuthored {
				t.Error("an operator's def was not stamped operator_authored")
			}
		})
	}
}

func TestMCPServerDefAuthoring_OverlayCannotSetOperatorAuthored(t *testing.T) {
	tool, done := mcpEnvRuleFixture(t)
	defer done()
	res := execDef(t, tool, mcpTenantOperatorCtx("acme"),
		`{"op":"create","name":"peer","overlay":{"transport":"http","url":"https://n8n.example.com/mcp","operator_authored":true,"headers":{"A":"${LOOMCYCLE_PEER_KEY_GLOBEX}"}}}`)
	if !res.IsError {
		t.Fatalf("an overlay claiming operator_authored was accepted: %s", res.Text)
	}
	res = execDef(t, tool, mcpTenantOperatorCtx("acme"),
		`{"op":"create","name":"peer","overlay":{"transport":"http","url":"https://n8n.example.com/mcp","operator_authored":true}}`)
	if res.IsError {
		t.Fatalf("create: %s", res.Text)
	}
	if storedOverlay(t, tool, "acme", "peer").OperatorAuthored {
		t.Error("operator_authored was taken from the overlay")
	}
}

// The operator's own connection fields stay usable by a tenant: the yaml
// entry's (a tenant create over a yaml name is the per-tenant override), and
// an operator-authored parent's on a fork. Changing any of them is refused.
func TestMCPServerDefAuthoring_TenantKeepsTheOperatorsPairing(t *testing.T) {
	hdr := map[string]string{"Authorization": "Bearer ${LOOMCYCLE_PEER_KEY_GLOBEX}"}
	t.Run("yaml entry", func(t *testing.T) {
		tool, done := mcpEnvRuleFixture(t)
		defer done()
		tool.Cfg.MCPServers["yaml-peer"] = config.MCPServer{Transport: "http", URL: "https://n8n.example.com/mcp", Headers: hdr}
		same := `{"transport":"http","url":"https://n8n.example.com/mcp","headers":{"Authorization":"Bearer ${LOOMCYCLE_PEER_KEY_GLOBEX}"}}`
		if res := execDef(t, tool, mcpTenantOperatorCtx("acme"), `{"op":"create","name":"yaml-peer","overlay":`+same+`}`); res.IsError {
			t.Fatalf("keeping the yaml pairing was refused: %s", res.Text)
		}
		moved := `{"transport":"http","url":"https://internal.example/mcp","headers":{"Authorization":"Bearer ${LOOMCYCLE_PEER_KEY_GLOBEX}"}}`
		if res := execDef(t, tool, mcpTenantOperatorCtx("globex"), `{"op":"create","name":"yaml-peer","overlay":`+moved+`}`); !res.IsError {
			t.Fatalf("re-aiming the yaml key at a new url was accepted: %s", res.Text)
		}
	})
	t.Run("operator-authored parent", func(t *testing.T) {
		tool, done := mcpEnvRuleFixture(t)
		defer done()
		if res := execDef(t, tool, mcpOpenModeOperatorCtx(),
			`{"op":"create","name":"shared","overlay":{"transport":"http","url":"https://n8n.example.com/mcp","headers":{"Authorization":"Bearer ${LOOMCYCLE_PEER_KEY_GLOBEX}"}}}`); res.IsError {
			t.Fatalf("operator create: %s", res.Text)
		}
		if res := execDef(t, tool, mcpTenantOperatorCtx("acme"), `{"op":"fork","name":"shared","overlay":{"description":"ours"},"promote":true}`); res.IsError {
			t.Fatalf("a fork keeping the operator's connection was refused: %s", res.Text)
		}
		if !storedOverlay(t, tool, "acme", "shared").OperatorAuthored {
			t.Error("the unchanged fork was not stamped operator_authored, so it would not dial")
		}
		if res := execDef(t, tool, mcpTenantOperatorCtx("globex"), `{"op":"fork","name":"shared","overlay":{"url":"https://internal.example/mcp"}}`); !res.IsError {
			t.Fatalf("a fork re-aiming the operator's header at a new url was accepted: %s", res.Text)
		}
		if res := execDef(t, tool, mcpTenantOperatorCtx("globex"), `{"op":"fork","name":"shared","overlay":{"headers":{"Authorization":"Bearer ${LOOMCYCLE_OTHER}"}}}`); !res.IsError {
			t.Fatalf("a fork naming a new env var was accepted: %s", res.Text)
		}
	})
	t.Run("non-operator parent", func(t *testing.T) {
		tool, done := mcpEnvRuleFixture(t)
		defer done()
		// A legacy row: stored before the rule, with no bit.
		plantMCPServerDef(t, tool, "acme", "legacy", `{"transport":"http","url":"https://n8n.example.com/mcp","headers":{"Authorization":"Bearer ${LOOMCYCLE_PEER_KEY_GLOBEX}"}}`)
		if res := execDef(t, tool, mcpTenantOperatorCtx("acme"), `{"op":"fork","name":"legacy","overlay":{"description":"x"}}`); !res.IsError {
			t.Fatalf("a fork of a non-operator row's env reference was accepted: %s", res.Text)
		}
	})
}

// An admin re-saving a non-operator row's exact content must mint a version
// stamped operator_authored: content_sha256 excludes the bit, so a dedup on
// the hash alone would return the old, undialable row.
func TestMCPServerDefCreate_AdminResaveIsNotDeduplicatedOntoANonOperatorRow(t *testing.T) {
	tool, done := mcpEnvRuleFixture(t)
	defer done()
	body := `{"transport":"http","url":"https://n8n.example.com/mcp","headers":{"Authorization":"Bearer ${LOOMCYCLE_PEER_KEY_GLOBEX}"}}`
	plantMCPServerDef(t, tool, "acme", "legacy", body)
	res := execDef(t, tool, mcpAdminCtx("acme"), `{"op":"create","name":"legacy","overlay":`+body+`}`)
	if res.IsError {
		t.Fatalf("admin re-save: %s", res.Text)
	}
	if decodeResult(t, res.Text)["deduplicated"] == true {
		t.Fatalf("the admin re-save was deduplicated onto the non-operator row: %s", res.Text)
	}
	if !storedOverlay(t, tool, "acme", "legacy").OperatorAuthored {
		t.Error("the admin re-save is not the active, operator-authored version")
	}
	// The same admin save again IS a no-op.
	res = execDef(t, tool, mcpAdminCtx("acme"), `{"op":"create","name":"legacy","overlay":`+body+`}`)
	if decodeResult(t, res.Text)["deduplicated"] != true {
		t.Errorf("an identical admin save was not deduplicated: %s", res.Text)
	}
}

// plantMCPServerDef writes a row straight into the store, as a version stored
// before the env rule existed (no operator_authored bit), and makes it active.
func plantMCPServerDef(t *testing.T, tool *MCPServerDef, tenant, name, body string) store.MCPServerDefRow {
	t.Helper()
	var ov mcpServerOverlay
	if err := json.Unmarshal([]byte(body), &ov); err != nil {
		t.Fatalf("plant body: %v", err)
	}
	row, err := tool.Store.MCPServerDefCreate(context.Background(), store.MCPServerDefRow{
		DefID: mintMCPServerDefID(), Name: name, Definition: json.RawMessage(body),
		ContentSHA256: signFromMCPServerOverlay(name, ov), TenantID: tenant,
	})
	if err != nil {
		t.Fatalf("plant: %v", err)
	}
	if err := tool.Store.MCPServerDefSetActive(context.Background(), tenant, name, row.DefID, "a_plant"); err != nil {
		t.Fatalf("plant active: %v", err)
	}
	return row
}

func TestMCPServerDefOperatorEnvWarnings_ListsAStoredNonOperatorReference(t *testing.T) {
	t.Setenv("LOOMCYCLE_PEER_KEY_GLOBEX", "globex-secret-value")
	tool, done := mcpEnvRuleFixture(t)
	defer done()
	bad := plantMCPServerDef(t, tool, "acme", "leaky", `{"transport":"http","url":"https://n8n.example.com/mcp","headers":{"Authorization":"Bearer ${LOOMCYCLE_PEER_KEY_GLOBEX}"}}`)
	plantMCPServerDef(t, tool, "acme", "clean", `{"transport":"http","url":"https://n8n.example.com/mcp","headers":{"Authorization":"Bearer ${run.credentials.x}"}}`)
	if res := execDef(t, tool, mcpAdminCtx("acme"),
		`{"op":"create","name":"blessed","overlay":{"transport":"http","url":"https://n8n.example.com/mcp","headers":{"Authorization":"Bearer ${LOOMCYCLE_PEER_KEY_GLOBEX}"}}}`); res.IsError {
		t.Fatalf("admin create: %s", res.Text)
	}

	warns, err := tool.OperatorEnvWarnings(context.Background())
	if err != nil {
		t.Fatalf("OperatorEnvWarnings: %v", err)
	}
	if len(warns) != 1 {
		t.Fatalf("want exactly the one non-operator row, got %d: %q", len(warns), warns)
	}
	w := warns[0]
	for _, want := range []string{`"leaky"`, bad.DefID, `tenant "acme"`, "ACTIVE", "headers.Authorization"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning lacks %q: %s", want, w)
		}
	}
	if strings.Contains(w, "globex-secret-value") || strings.Contains(w, "Bearer") {
		t.Errorf("warning carries a value, not only a location: %s", w)
	}
	if !strings.Contains(w, "still dials this release") {
		t.Errorf("the default-mode warning does not say the def still dials: %s", w)
	}
	tool.Cfg.Env.MCPRefuseUnattributedEnv = true
	if warns, _ = tool.OperatorEnvWarnings(context.Background()); len(warns) != 1 || !strings.Contains(warns[0], "so it is not dialed") {
		t.Errorf("the strict-mode warning does not say the def is not dialed: %q", warns)
	}
}

// The bit's carry from the stored body to the spec the pool dials: overlay →
// specFromOverlay → registry (rehydrate). The last hop, the registry →
// lookup.MCPServerSpec view in main, is covered by the dial test there.
func TestMCPServerDefOperatorAuthored_CarriedIntoTheRehydratedRegistry(t *testing.T) {
	tool, done := mcpEnvRuleFixture(t)
	defer done()
	if res := execDef(t, tool, mcpOpenModeOperatorCtx(),
		`{"op":"create","name":"op-peer","overlay":{"transport":"http","url":"https://n8n.example.com/mcp","headers":{"A":"${LOOMCYCLE_X}"}}}`); res.IsError {
		t.Fatalf("operator create: %s", res.Text)
	}
	plantMCPServerDef(t, tool, "acme", "legacy", `{"transport":"http","url":"https://n8n.example.com/mcp"}`)

	reg := loommcp.NewDynamicRegistry() // a fresh boot's registry
	if _, err := RehydrateMCPRegistry(context.Background(), tool.Store, tool.Cfg.MCPServers, reg, t.Logf); err != nil {
		t.Fatalf("rehydrate: %v", err)
	}
	if spec, ok := reg.Get("", "op-peer"); !ok || !spec.OperatorAuthored {
		t.Errorf("operator-authored def rehydrated without the bit: ok=%v spec=%+v", ok, spec)
	}
	if spec, ok := reg.Get("acme", "legacy"); !ok || spec.OperatorAuthored {
		t.Errorf("legacy def rehydrated with the bit: ok=%v spec=%+v", ok, spec)
	}
}

// The write shape and the read shape of a stored MCP server def carry the
// same json fields (pairwise with lookup's own want-list drift test).
func TestMCPServerOverlay_JSONTagsMatchSubstrateMCPServer(t *testing.T) {
	tags := func(t reflect.Type) map[string]bool {
		out := map[string]bool{}
		for i := 0; i < t.NumField(); i++ {
			name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
			out[name] = true
		}
		return out
	}
	write, read := tags(reflect.TypeOf(mcpServerOverlay{})), tags(reflect.TypeOf(lookup.SubstrateMCPServer{}))
	if !reflect.DeepEqual(write, read) {
		t.Errorf("mcpServerOverlay json tags %v != lookup.SubstrateMCPServer %v", write, read)
	}
}
