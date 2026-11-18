package http

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// setTenantModes swaps the live config's resolved acl_modes, as a reload of a
// config carrying them would.
func setTenantModes(t *testing.T, srv *Server, modes map[string]authz.Mode) {
	t.Helper()
	c := srv.cfg()
	if c == nil {
		t.Fatal("fixture has no live config")
	}
	c.ResolvedACLModes = modes
}

// RFC EZ P5: on a shadow deployment a bound deny is only logged. Raising the
// tenant to compat in acl_modes makes the same request refused, for that
// tenant only. The legacy admin, who has no tenant, stays on the deployment's
// shadow. Dropping the entry from the live config lifts it on the next
// request.
func TestACLTenantMode_ARaisedTenantHonoursItsDeny(t *testing.T) {
	srv, url, st := aclFixture(t, authz.ModeShadow)
	putACL(t, st, "no-other", `[{"effect":"deny","actions":["run"],"resources":["agent:other"]}]`)
	bind(t, st, "no-other", "user", "dev")
	if code, body := bearerCall(t, httptestServerAt(url), freeBearer, "POST", "/v1/runs", runBody("other")); code != 200 {
		t.Fatalf("shadow deployment, no entry: status %d: %s", code, body)
	}

	setTenantModes(t, srv, map[string]authz.Mode{"acme": authz.ModeCompat})
	code, body := bearerCall(t, httptestServerAt(url), freeBearer, "POST", "/v1/runs", runBody("other"))
	if code != 403 || !strings.Contains(body, `"code":"acl_denied"`) {
		t.Fatalf("acme raised to compat: status %d: %s, want the deny honoured", code, body)
	}
	if code, body := bearerCall(t, httptestServerAt(url), freeBearer, "GET", "/v1/_me", ""); code != 200 || !strings.Contains(body, `"acl_mode":"compat"`) {
		t.Errorf("acme member whoami: %d %s, want acl_mode compat", code, body)
	}
	if code, body := bearerCall(t, httptestServerAt(url), "legacy-admin-secret", "GET", "/v1/_me", ""); code != 200 || !strings.Contains(body, `"acl_mode":"shadow"`) {
		t.Errorf("legacy admin whoami: %d %s, want the deployment's shadow", code, body)
	}

	setTenantModes(t, srv, nil)
	if code, body := bearerCall(t, httptestServerAt(url), freeBearer, "POST", "/v1/runs", runBody("other")); code != 200 {
		t.Errorf("entry removed: status %d: %s, want shadow again", code, body)
	}
}

// An entry below the deployment's mode is refused at config load; one that
// reaches the server anyway (a map built without validation) is not applied:
// the deny bound under a compat deployment still refuses.
func TestACLTenantMode_AnEntryBelowTheDeploymentIsNotApplied(t *testing.T) {
	srv, url, st := aclFixture(t, authz.ModeCompat)
	putACL(t, st, "no-other", `[{"effect":"deny","actions":["run"],"resources":["agent:other"]}]`)
	bind(t, st, "no-other", "user", "dev")
	setTenantModes(t, srv, map[string]authz.Mode{"acme": authz.ModeShadow})
	if code, body := bearerCall(t, httptestServerAt(url), freeBearer, "POST", "/v1/runs", runBody("other")); code != 403 {
		t.Errorf("acme lowered to shadow under compat: status %d: %s, want the deny still honoured", code, body)
	}
}

// A run that outlives its request (resumed, queued, trigger-started) is
// stamped with the mode of the tenant its origin record names, not the
// deployment's.
func TestACLTenantMode_ARecordedOriginRunsUnderItsTenantsMode(t *testing.T) {
	srv, _, _ := aclFixture(t, authz.ModeCompat)
	setTenantModes(t, srv, map[string]authz.Mode{"acme": authz.ModeEnforce})
	surface := authz.Surface{Action: "run", Type: "agent", Name: "allowed"}
	for tenant, want := range map[string]authz.Mode{"acme": authz.ModeEnforce, "beta": authz.ModeCompat} {
		o := &aclOriginRecord{TenantID: tenant, Subject: "dev", Class: aclres.ClassKey(auth.Principal{TenantID: tenant, Subject: "dev"})}
		ctx, _, err := srv.recordedCallerACLFor(context.Background(), o, surface)
		if err != nil {
			t.Fatalf("%s: %v", tenant, err)
		}
		if got := authz.ModeFrom(ctx); got != want {
			t.Errorf("origin tenant %s: mode %s, want %s", tenant, got, want)
		}
	}
}
