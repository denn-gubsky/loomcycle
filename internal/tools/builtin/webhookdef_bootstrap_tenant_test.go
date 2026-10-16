package builtin

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/lookup"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A static webhook the boot did not seed is bootstrapped by the first fork of
// its name. It used to be bootstrapped into the FORKING caller's tenant, and
// promoted there, before the caller's scope was checked and before the fork's
// own checks ran: a tenant caller with no scope for the name, or whose fork
// was then refused, left an active row in its own tenant holding the
// operator's yaml — whose runs execute in the operator layer "" (a
// bootstrapped row keeps the yaml's empty tenant), and whose `$cred:` secret
// is read from that caller's tenant, where the caller can create it.
func TestWebhookDefTool_ForkBootstrapNeverLandsInTheCallersTenant(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()
	acme := tools.WithRunIdentity(ctx, tools.RunIdentityValue{AgentID: "a_acme", TenantID: "acme"})

	activeInAcme := func() bool {
		t.Helper()
		_, err := tool.Store.WebhookDefGetActive(ctx, "acme", "gh-push")
		var nf *store.ErrNotFound
		if err != nil && !errors.As(err, &nf) {
			t.Fatal(err)
		}
		return err == nil
	}

	// No scope for the name: refused, and nothing written.
	noScope := tools.WithWebhookDefPolicy(acme, tools.WebhookDefPolicyValue{Scopes: []string{"named:other"}})
	if res, _ := tool.Execute(noScope, json.RawMessage(`{"op":"fork","name":"gh-push","overlay":{"agent":"x"}}`)); !res.IsError {
		t.Fatalf("a fork outside the caller's scope was accepted: %s", res.Text)
	}
	if activeInAcme() {
		t.Error("a fork refused for scope left an active version in the caller's tenant")
	}

	// In scope, but the fork itself is refused (an unknown delivery).
	if res, _ := tool.Execute(acme, json.RawMessage(`{"op":"fork","name":"gh-push","overlay":{"delivery":"bogus"}}`)); !res.IsError {
		t.Fatalf("an invalid fork was accepted: %s", res.Text)
	}
	if activeInAcme() {
		t.Error("a refused fork left the bootstrapped yaml active in the caller's tenant")
	}

	// A fork that is stored: its version is the caller's, executing in the
	// caller's tenant, and the bootstrap it forked from is the shared one.
	res, _ := tool.Execute(acme, json.RawMessage(`{"op":"fork","name":"gh-push","promote":false,"overlay":{"agent":"y"}}`))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	if activeInAcme() {
		t.Error("an unpromoted fork left the bootstrapped yaml active in the caller's tenant")
	}
	shared, err := tool.Store.WebhookDefGetActive(ctx, "", "gh-push")
	if err != nil || !shared.BootstrappedFromStatic {
		t.Errorf("the bootstrap is not the shared tenant's (%+v, %v)", shared, err)
	}
	rows, err := tool.Store.WebhookDefListByName(ctx, "gh-push")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.BootstrappedFromStatic && r.TenantID != "" {
			t.Errorf("a bootstrapped row is stored under tenant %q", r.TenantID)
		}
	}
}

// A row bootstrapped from the yaml but stored under a tenant (as the fork
// above used to write) executes in that tenant, never in the operator layer.
func TestResolveWebhook_TenantBootstrappedRowRunsInItsTenant(t *testing.T) {
	tool, ctx, cleanup := webhookDefFixture(t)
	defer cleanup()
	body := json.RawMessage(`{"enabled":true,"delivery":"spawn","agent":"intake","auth":{"signing_secret_env":"LOOMCYCLE_S"}}`)
	row, err := tool.Store.WebhookDefCreate(ctx, store.WebhookDefRow{DefID: "wh_boot_acme", Name: "x", Definition: body, TenantID: "acme", BootstrappedFromStatic: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := tool.Store.WebhookDefSetActive(ctx, "acme", "x", row.DefID, "a"); err != nil {
		t.Fatal(err)
	}
	wd, ok := lookup.Webhook(ctx, tool.Store, nil, "acme", "x")
	if !ok {
		t.Fatal("did not resolve")
	}
	if wd.TenantID != "acme" {
		t.Errorf("a tenant's bootstrapped row executes in %q, want acme", wd.TenantID)
	}
}
