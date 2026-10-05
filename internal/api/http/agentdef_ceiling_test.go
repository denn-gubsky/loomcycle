package http

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// wideAgentOverlay grants capabilities the substrate plane itself does not
// stamp a policy for (volumes, interruptions) beside ones it does.
const wideAgentOverlay = `{"tools":["Read"],"volumes":["work"],"memory_scopes":["user","tenant"],
  "sql_scopes":["tenant"],"channels":{"publish":["ops"]},"interruption":{"enabled":true},
  "agent_def_scopes":["any"],"schedule_def_scopes":["any"],"volume_def_scopes":["any"]}`

// The operator authoring over the HTTP substrate plane — admin, open mode, or
// a tenant operator within its own tenant — is the root of authority and is
// not narrowed by an agent's policies: the capability ceiling recognises this
// plane by what substrateAdminCtx stamps. The same ctx inside a run is
// narrowed, so the posture cannot leak onto one.
func TestSubstrateAdminCtx_OperatorIsNotNarrowedByTheCapabilityCeiling(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{Volumes: map[string]config.Volume{"work": {Path: t.TempDir(), Mode: "rw"}}}
	tool := &builtin.AgentDef{Store: st, Cfg: cfg, MaxDefinitionBytes: 131072, MaxDescriptionBytes: 8192}
	create := func(ctx context.Context, name string) tools.Result {
		t.Helper()
		res, err := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"`+name+`","overlay":`+wideAgentOverlay+`}`))
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return res
	}
	for name, ctx := range map[string]context.Context{
		"open-mode":       substrateAdminCtx(context.Background()),
		"admin":           substrateAdminCtx(auth.WithPrincipal(context.Background(), auth.Principal{Subject: "root", Scopes: []string{auth.ScopeAdmin}})),
		"tenant-operator": substrateAdminCtx(tenantOperatorCtx("acme")),
	} {
		if res := create(ctx, "wide-"+name); res.IsError {
			t.Errorf("%s: the operator was refused: %s", name, res.Text)
		}
	}
	inRun := tools.WithRunID(substrateAdminCtx(tenantOperatorCtx("acme")), "run_1")
	if res := create(inRun, "wide-in-run"); !res.IsError {
		t.Errorf("the operator plane's ctx inside a run was not narrowed: %s", res.Text)
	}
}
