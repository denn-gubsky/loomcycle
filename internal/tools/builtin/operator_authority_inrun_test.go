package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A run started with an admin bearer carries that principal into every tool
// call its model makes, but the model is not the operator: a prompt-injected
// run must not gain what only the operator may do. Operator authority counts
// only off-run. Each test drives one gate with an in-run admin context (the
// principal plus the run id every run path stamps on its tool calls) and
// checks the gate treats it like any other author, beside the off-run admin
// call that still passes.

// inAdminRun puts ctx inside a run, as the loop does for every tool call.
func inAdminRun(ctx context.Context) context.Context { return tools.WithRunID(ctx, "r_injected") }

func adminOf(ctx context.Context, tenant string) context.Context {
	return auth.WithPrincipal(ctx, auth.Principal{TenantID: tenant, Subject: "ops", Scopes: []string{auth.ScopeAdmin}})
}

func TestCallerHasOperatorAuthority_HeldOnlyOffRun(t *testing.T) {
	admin := adminOf(asTenant(context.Background(), "acme"), "acme")
	open := tools.WithUnauthenticatedOperator(asTenant(context.Background(), ""))
	// The substrate-operator marker is on every off-run HTTP call, a tenant
	// operator's included, so it is no operator authority on its own.
	tenantOp := auth.WithPrincipal(tools.WithSubstrateOperator(asTenant(context.Background(), "acme")),
		auth.Principal{TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeTenant}})
	for _, c := range []struct {
		name string
		ctx  context.Context
		want bool
	}{
		{"admin principal, off-run", admin, true},
		{"admin principal, in a run", inAdminRun(admin), false},
		{"unauthenticated operator, off-run", open, true},
		{"unauthenticated-operator marker, in a run", inAdminRun(open), false},
		{"tenant operator, off-run", tenantOp, false},
	} {
		if got := callerHasOperatorAuthority(c.ctx); got != c.want {
			t.Errorf("%s: callerHasOperatorAuthority = %t, want %t", c.name, got, c.want)
		}
	}
}

// HookDef: an admin may name a signing-secret credential not created yet; a
// model in an admin's run names one its tenant holds, like any author. The
// tool refuses every in-run call before it gets here, so the gate is checked
// directly: it must not lean on that refusal.
func TestHookDef_InRunAdminSigningSecretMustBeTheTenantsCredential(t *testing.T) {
	_, s := hookDefFixture(t)
	creds := peerCredsOver(t, s, tenantCred("acme", "hook_sign", "v1"))
	admin := adminOf(hookTenantCtx("acme"), "acme")
	body := hooks.DefBody{Kind: "http", URL: "https://hooks.example/gate", SigningSecret: "$cred:absent"}
	if err := requireAuthorBoundSigningSecret(inAdminRun(admin), creds, body); err == nil || !strings.Contains(err.Error(), "names no tenant-level credential") {
		t.Errorf("an in-run admin named a credential its tenant does not hold: %v", err)
	}
	if err := requireAuthorBoundSigningSecret(admin, creds, body); err != nil {
		t.Errorf("an off-run admin was refused a credential created later: %v", err)
	}
	tool, _ := hookDefFixture(t)
	if _, text, isErr := runHookDef(t, tool, inAdminRun(admin), `{"op":"list"}`); !isErr || !strings.Contains(text, "not available inside a run") {
		t.Errorf("HookDef answered an in-run admin: %s", text)
	}
}

// MemoryBackendDef / DocumentSourceDef: an admin keeps free api_key_env; a
// model in an admin's run names neither another tenant's env key nor a
// credential its tenant does not hold.
func TestRemotePeerAuthoring_InRunAdminApiKeyEnvIsBoundToTheTenant(t *testing.T) {
	fixtures := map[string]struct {
		kind    string
		fixture func(t *testing.T) (tools.Tool, context.Context, func())
	}{
		"MemoryBackendDef": {`"kind":"remote",`, func(t *testing.T) (tools.Tool, context.Context, func()) {
			tool, ctx, done := memoryBackendDefFixture(t)
			tool.PeerCredentials = peerCredsOver(t, tool.Store, tenantCred("acme", "peer_key", "v-acme"))
			return tool, ctx, done
		}},
		"DocumentSourceDef": {``, func(t *testing.T) (tools.Tool, context.Context, func()) {
			tool, ctx, done := documentSourceDefFixture(t)
			tool.PeerCredentials = peerCredsOver(t, tool.Store, tenantCred("acme", "peer_key", "v-acme"))
			return tool, ctx, done
		}},
	}
	for kind, f := range fixtures {
		for _, key := range []string{"LOOMCYCLE_PEER_KEY_GLOBEX", "$cred:absent"} {
			t.Run(kind+" "+key, func(t *testing.T) {
				tool, base, done := f.fixture(t)
				defer done()
				admin := adminOf(asTenant(base, "acme"), "acme")
				create := `{"op":"create","name":"peer","overlay":{` + f.kind + `"config":{"base_url":"https://peer.example.com","api_key_env":"` + key + `"}}}`
				if res := execDef(t, tool, inAdminRun(admin), create); !res.IsError {
					t.Errorf("an in-run admin was given free api_key_env: %s", res.Text)
				}
				if res := execDef(t, tool, admin, create); res.IsError {
					t.Errorf("an off-run admin was refused free api_key_env: %s", res.Text)
				}
			})
		}
	}
}

// ScheduleDef: operator_layer (a tenant-less consolidation fan-out sweeps every
// tenant) is the operator's, never a model's in an admin's run.
func TestScheduleDef_InRunAdminIsNotTheOperatorLayer(t *testing.T) {
	sd, base, done := scheduleDefFixture(t)
	defer done()
	admin := tenantlessOperatorAdmin(base)

	requireRefusedAsValidation(t, "in-run fan-out", execDef(t, sd, inAdminRun(admin), `{"op":"create","name":"sweep","overlay":`+fanoutOverlay+`}`))
	if res := execDef(t, sd, inAdminRun(admin), `{"op":"create","name":"plain","overlay":{"agent":"job-search-batch","schedule":"0 * * * *"}}`); res.IsError {
		t.Fatalf("in-run plain create: %s", res.Text)
	}
	if storedOperatorLayer(t, sd, "plain") {
		t.Error("an in-run admin's schedule was stored with operator_layer")
	}
	if res := execDef(t, sd, admin, `{"op":"create","name":"sweep","overlay":`+fanoutOverlay+`}`); res.IsError {
		t.Fatalf("off-run admin fan-out: %s", res.Text)
	}
	if !storedOperatorLayer(t, sd, "sweep") {
		t.Error("an off-run admin's fan-out was stored without operator_layer")
	}
}

// ScheduleDef / WebhookDef: only the operator points a trigger's runs at
// another tenant. Neither an admin principal nor the unauthenticated-operator
// marker grants that inside a run.
func TestTriggerDefs_InRunAdminMayNotNameAnotherTenant(t *testing.T) {
	for _, h := range triggerHarnesses(t) {
		admin := h.as("acme", true)
		res := h.call(inAdminRun(admin), "create", "grab", "globex")
		requireRefusedAsValidation(t, h.kind+" in-run admin", res)
		if got, ok := h.execTenant(t, "acme", "grab"); ok {
			t.Errorf("%s: an in-run admin stored a version executing in %q", h.kind, got)
		}
		open := tools.WithUnauthenticatedOperator(asTenant(h.base, ""))
		requireRefusedAsValidation(t, h.kind+" in-run operator marker", h.call(inAdminRun(open), "create", "grab", "acme"))
		if res := h.call(admin, "create", "ops", "globex"); res.IsError {
			t.Fatalf("%s: off-run admin naming another tenant: %s", h.kind, res.Text)
		}
	}
}

// WebhookDef / ScheduleDef user_bearer: a model in an admin's run names a
// tenant-level credential its tenant holds, like any author.
func TestTriggerDefs_InRunAdminUserBearerMustBeTheTenantsCredential(t *testing.T) {
	wh, whCtx, _ := storedSecretFixture(t)
	whAdmin := adminOf(whCtx, "acme")
	if text, isErr := webhookCreate(t, wh, inAdminRun(whAdmin), "w", bearerOverlay(`,"user_bearer":"$cred:gone"`)); !isErr || !strings.Contains(text, "names no tenant-level credential") {
		t.Errorf("WebhookDef: an in-run admin named a credential its tenant does not hold: %s", text)
	}
	if text, isErr := webhookCreate(t, wh, whAdmin, "w", bearerOverlay(`,"user_bearer":"$cred:gone"`)); isErr {
		t.Errorf("WebhookDef: an off-run admin was refused a credential created later: %s", text)
	}

	sd, sdCtx := scheduleBearerFixture(t)
	sdAdmin := adminOf(sdCtx, "acme")
	overlay := `{"agent":"digest","schedule":"0 6 * * *","user_bearer":"$cred:gone"}`
	if text, isErr := scheduleCreate(t, sd, inAdminRun(sdAdmin), "s", overlay); !isErr || !strings.Contains(text, "names no tenant-level credential") {
		t.Errorf("ScheduleDef: an in-run admin named a credential its tenant does not hold: %s", text)
	}
	if text, isErr := scheduleCreate(t, sd, sdAdmin, "s", overlay); isErr {
		t.Errorf("ScheduleDef: an off-run admin was refused a credential created later: %s", text)
	}
}

// WebhookDef auth.secret: a model in an admin's run names a tenant-level
// credential its tenant holds, like any author.
func TestWebhookDefTool_InRunAdminStoredSecretMustBeTheTenantsCredential(t *testing.T) {
	tool, ctx, _ := storedSecretFixture(t)
	admin := adminOf(ctx, "acme")
	if text, isErr := webhookCreate(t, tool, inAdminRun(admin), "in-run", secretOverlay(`{"secret":"$cred:not-yet"}`)); !isErr || !strings.Contains(text, "names no tenant-level credential") {
		t.Errorf("an in-run admin named a credential its tenant does not hold: %s", text)
	}
	if text, isErr := webhookCreate(t, tool, admin, "off-run", secretOverlay(`{"secret":"$cred:not-yet"}`)); isErr {
		t.Errorf("an off-run admin was refused a credential created later: %s", text)
	}
}

// ScheduleDef user_scope_credentials: an operator may have a schedule resolve
// $cred: in any user's scope; a model in an admin's run only in the scope of
// the admin's own subject, like any principal.
func TestScheduleDefTool_InRunAdminMayNotActAsAnotherUser(t *testing.T) {
	tool, ctx := scheduleBearerFixture(t)
	admin := adminOf(ctx, "acme") // subject "ops"
	overlay := `{"agent":"digest","schedule":"0 6 * * *","user_id":"alice"}`
	if text, isErr := scheduleCreate(t, tool, inAdminRun(admin), "in-run", overlay); isErr {
		t.Fatal(text)
	}
	if activeScheduleDef(t, tool, "in-run").UserScopeCredentials {
		t.Error("an in-run admin's schedule resolves $cred: in another user's scope")
	}
	if text, isErr := scheduleCreate(t, tool, admin, "off-run", overlay); isErr {
		t.Fatal(text)
	}
	if !activeScheduleDef(t, tool, "off-run").UserScopeCredentials {
		t.Error("an off-run admin's schedule lost the user's scope")
	}
}
