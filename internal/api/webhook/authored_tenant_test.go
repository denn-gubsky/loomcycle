package webhook

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// End to end, from the authoring tool to the spawned run: whatever a tenant
// operator in acme does with the WebhookDef tool — name another tenant in the
// overlay, or fork a shared webhook an admin pointed at another tenant — every
// webhook the acme route serves from acme's own definitions spawns its run in
// acme. Resolution and run input are the receiver's own (lookup.Webhook, then
// buildRunInput), the path a delivery to /v1/_webhooks/acme/{name} takes.
func TestReceiver_NonAdminAuthoredWebhookSpawnsInTheAuthorsTenant(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{}
	tool := &builtin.WebhookDef{Store: st, Cfg: cfg}

	as := func(tenant string, scope string) context.Context {
		ctx := tools.WithWebhookDefPolicy(context.Background(), tools.WebhookDefPolicyValue{Scopes: []string{"any"}, SelfName: "orchestrator"})
		ctx = tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_test", TenantID: tenant})
		return auth.WithPrincipal(ctx, auth.Principal{TenantID: tenant, Subject: "op-" + tenant, Scopes: []string{scope}})
	}
	body := `"delivery":"spawn","agent":"intake","auth":{"kind":"hmac","signing_secret_env":"LOOMCYCLE_S"}`
	exec := func(ctx context.Context, in string) {
		// Refusals are the point for some of these; the assertion is on what spawns.
		res, _ := tool.Execute(ctx, json.RawMessage(in))
		t.Logf("%s -> error=%v %s", in, res.IsError, res.Text)
	}

	acme := as("acme", auth.ScopeTenant)
	exec(acme, `{"op":"create","name":"foreign","overlay":{`+body+`,"tenant_id":"globex"}}`)
	exec(acme, `{"op":"create","name":"own","overlay":{`+body+`}}`)
	exec(acme, `{"op":"fork","name":"own","overlay":{"tenant_id":"globex"}}`)
	exec(as("", auth.ScopeAdmin), `{"op":"create","name":"shared","overlay":{`+body+`,"tenant_id":"globex"}}`)
	exec(acme, `{"op":"fork","name":"shared","overlay":{"agent":"intake2"}}`)

	names, err := st.WebhookDefListNames(context.Background())
	if err != nil {
		t.Fatalf("list names: %v", err)
	}
	spawned := 0
	for _, n := range names {
		// Only acme's own definitions: the acme route falls back to a shared
		// def when acme has none, and that one is the admin's to point anywhere.
		if n.TenantID != "acme" || n.ActiveDefID == "" {
			continue
		}
		wd, ok := lookup.Webhook(context.Background(), st, cfg, "acme", n.Name)
		if !ok {
			t.Fatalf("lookup.Webhook(acme, %s): not found", n.Name)
		}
		in := buildRunInput(wd, projectResult{}, map[string]bool{}, func(string) string { return "" }, nil)
		spawned++
		if in.TenantID != "acme" {
			t.Errorf("webhook %q, active in acme and authored by an acme tenant operator, spawns its run in tenant %q", n.Name, in.TenantID)
		}
	}
	if spawned == 0 {
		t.Fatal("no webhook is active in acme — the fixture spawned nothing, so the assertion is vacuous")
	}
}
