package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// An operator's timed override stores the runs of ONE no-trace agent, in ONE
// tenant, until a time. These tests are about what it lifts, what it never
// lifts, and who may set one.

const (
	opsBearer    = "tok-ops"     // substrate:tenant, tenant acme
	globexBearer = "tok-globex"  // substrate:tenant, tenant globex
	adminBearer  = "tok-admin-x" // substrate:admin
)

func overrideFixture(t *testing.T, scripts ...[]providers.Event) (*Server, *httptest.Server, store.Store) {
	t.Helper()
	s, ts, st := restrictedFixtureServer(t, scripts...)
	for _, tok := range []struct {
		id, bearer, tenant, subject string
		scopes                      []string
	}{
		{"def_ops", opsBearer, "acme", "ops", []string{auth.ScopeTenant}},
		{"def_globex", globexBearer, "globex", "gops", []string{auth.ScopeTenant}},
		{"def_admin_x", adminBearer, "root", "root", []string{auth.ScopeAdmin}},
	} {
		if _, err := st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
			DefID: tok.id, Name: tok.id, TenantID: tok.tenant, Subject: tok.subject,
			TokenHash: auth.HashToken(testTokenPepper, tok.bearer), AllowedScopes: tok.scopes,
		}); err != nil {
			t.Fatal(err)
		}
	}
	markNoTrace(s, "other")
	return s, ts, st
}

func overrideBody(agent string, in time.Duration, extra string) string {
	return `{"agent":"` + agent + `","until":"` + time.Now().Add(in).UTC().Format(time.RFC3339) + `"` + extra + `}`
}

func runWithID(agent, agentID, extra string) string {
	return `{"agent":"` + agent + `","agent_id":"` + agentID + `"` + extra + `,"segments":[{"role":"user","content":[{"type":"trusted-text","text":"q ` + canary + `"}]}]}`
}

func retentionOfAgentID(t *testing.T, st store.Store, agentID string) string {
	t.Helper()
	run, err := st.GetRunByAgentID(context.Background(), agentID)
	if err != nil {
		t.Fatalf("run %s: %v", agentID, err)
	}
	return run.Retention
}

func TestRetentionOverride_StoresTheRunsOfOneAgentUntilItRunsOut(t *testing.T) {
	_, ts, st := overrideFixture(t)

	// Before: the agent's runs are no-trace.
	if code, body := bearerCall(t, ts, freeBearer, "POST", "/v1/runs", runWithID("other", "a_before", "")); code != 200 {
		t.Fatalf("before: %d %s", code, body)
	}
	if got := retentionOfAgentID(t, st, "a_before"); got != store.RetentionNone {
		t.Fatalf("before any override: retention = %q, want none", got)
	}

	code, body := bearerCall(t, ts, opsBearer, "PUT", "/v1/_retention/overrides", overrideBody("other", time.Hour, ""))
	if code != 200 || !strings.Contains(body, `"tenant":"acme"`) || !strings.Contains(body, `"set_by":"ops"`) {
		t.Fatalf("set: %d %s", code, body)
	}
	// In the window: an ordinary stored run, content and all.
	if code, body := bearerCall(t, ts, freeBearer, "POST", "/v1/runs", runWithID("other", "a_during", "")); code != 200 {
		t.Fatalf("during: %d %s", code, body)
	}
	run, _ := st.GetRunByAgentID(context.Background(), "a_during")
	if run.Retention != "" {
		t.Errorf("in the window: retention = %q, want the default", run.Retention)
	}
	events, _ := st.GetTranscript(context.Background(), run.SessionID)
	stored := false
	for _, ev := range events {
		if strings.Contains(string(ev.Payload), canary) {
			stored = true
		}
	}
	if !stored {
		t.Error("in the window the run's content was not stored: the override did nothing useful")
	}
	// It lifts a request's "none" for that agent too, and nothing for another.
	if code, body := bearerCall(t, ts, freeBearer, "POST", "/v1/runs", runWithID("other", "a_req", `,"retention":"none"`)); code != 200 {
		t.Fatalf("request none: %d %s", code, body)
	}
	if got := retentionOfAgentID(t, st, "a_req"); got != "" {
		t.Errorf("a request for none in the window: retention = %q, want it lifted", got)
	}
	if code, body := bearerCall(t, ts, freeBearer, "POST", "/v1/runs", runWithID("allowed", "a_else", `,"retention":"none"`)); code != 200 {
		t.Fatalf("another agent: %d %s", code, body)
	}
	if got := retentionOfAgentID(t, st, "a_else"); got != store.RetentionNone {
		t.Errorf("another agent's run: retention = %q, want none — the override is for one agent", got)
	}

	// Cleared: back to no-trace.
	if code, body := bearerCall(t, ts, opsBearer, "DELETE", "/v1/_retention/overrides?agent=other", ""); code != 204 {
		t.Fatalf("clear: %d %s", code, body)
	}
	if code, body := bearerCall(t, ts, freeBearer, "POST", "/v1/runs", runWithID("other", "a_after", "")); code != 200 {
		t.Fatalf("after: %d %s", code, body)
	}
	if got := retentionOfAgentID(t, st, "a_after"); got != store.RetentionNone {
		t.Errorf("after the override was cleared: retention = %q, want none", got)
	}
	// One that has run out is the same as none at all.
	if err := st.RetentionOverridePut(context.Background(), store.RetentionOverrideRow{TenantID: "acme", Agent: "other", Until: time.Now().Add(-time.Minute)}, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if code, body := bearerCall(t, ts, freeBearer, "POST", "/v1/runs", runWithID("other", "a_expired", "")); code != 200 {
		t.Fatalf("expired: %d %s", code, body)
	}
	if got := retentionOfAgentID(t, st, "a_expired"); got != store.RetentionNone {
		t.Errorf("under an expired override: retention = %q, want none", got)
	}
}

// What an override never lifts: a token's own mode, another tenant's runs,
// and a child's inherited mode.
func TestRetentionOverride_NeverLiftsATokensModeOrAnotherTenants(t *testing.T) {
	s, ts, st := overrideFixture(t)
	if code, body := bearerCall(t, ts, opsBearer, "PUT", "/v1/_retention/overrides", overrideBody("other", time.Hour, "")); code != 200 {
		t.Fatalf("set: %d %s", code, body)
	}
	// The token whose runs are no-trace: still no-trace. A developer's own
	// runs stay unstored whatever an operator switches on.
	if code, body := bearerCall(t, ts, quietBearer, "POST", "/v1/runs", runWithID("other", "a_quiet_tok", "")); code != 200 {
		t.Fatalf("no-trace token: %d %s", code, body)
	}
	if got := retentionOfAgentID(t, st, "a_quiet_tok"); got != store.RetentionNone {
		t.Errorf("a no-trace token's run under an override: retention = %q, want none", got)
	}
	// Another tenant's run of an agent of the same name.
	if code, body := bearerCall(t, ts, globexBearer, "POST", "/v1/runs", runWithID("other", "a_globex", "")); code != 200 {
		t.Fatalf("other tenant: %d %s", code, body)
	}
	if got := retentionOfAgentID(t, st, "a_globex"); got != store.RetentionNone {
		t.Errorf("another tenant's run: retention = %q, want none — the override is for one tenant", got)
	}
	// A child of a no-trace run stays no-trace even when ITS agent is lifted.
	inside := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{TenantID: "acme", AgentID: "a_parent", Retention: store.RetentionNone})
	if got := s.resolveRetention(inside, "acme", "other", s.cfg().Agents["other"], ""); got != store.RetentionNone {
		t.Errorf("a lifted agent spawned by a no-trace parent: retention = %q, want none", got)
	}
	// The same agent started on its own in that tenant is lifted.
	if got := s.resolveRetention(context.Background(), "acme", "other", s.cfg().Agents["other"], ""); got != "" {
		t.Errorf("the lifted agent on its own: retention = %q, want the default", got)
	}
	// A store that cannot be read does not lift anything.
	broken := &Server{store: failingOverrideStore{Store: st}}
	if broken.retentionLifted(context.Background(), "acme", "other") {
		t.Error("an unreadable override was treated as in force")
	}
}

type failingOverrideStore struct{ store.Store }

func (failingOverrideStore) RetentionOverrideActive(context.Context, string, string, time.Time) (bool, error) {
	return true, errors.New("boom")
}

func TestRetentionOverride_WhoMaySetOneAndForWhom(t *testing.T) {
	_, ts, st := overrideFixture(t)
	ctx := context.Background()

	// A tenant operator's override is for its own tenant, whatever it names.
	if code, body := bearerCall(t, ts, opsBearer, "PUT", "/v1/_retention/overrides", overrideBody("other", time.Hour, `,"tenant":"globex"`)); code != 200 || !strings.Contains(body, `"tenant":"acme"`) {
		t.Errorf("a tenant operator naming another tenant: %d %s, want its own tenant", code, body)
	}
	if ok, _ := st.RetentionOverrideActive(ctx, "globex", "other", time.Now()); ok {
		t.Error("a tenant operator set an override in another tenant")
	}
	// An admin names the tenant.
	if code, body := bearerCall(t, ts, adminBearer, "PUT", "/v1/_retention/overrides", overrideBody("other", time.Hour, `,"tenant":"globex"`)); code != 200 || !strings.Contains(body, `"tenant":"globex"`) {
		t.Errorf("an admin: %d %s", code, body)
	}
	// A member, and a token limited to certain agents, may not — to set, to
	// clear, or to see what is being debugged.
	for _, bearer := range []string{freeBearer, restrictedBearer, quietBearer} {
		for _, call := range [][2]string{{"PUT", overrideBody("other", time.Hour, "")}, {"DELETE", ""}, {"GET", ""}} {
			path := "/v1/_retention/overrides"
			if call[0] == "DELETE" {
				path += "?agent=other"
			}
			if code, body := bearerCall(t, ts, bearer, call[0], path, call[1]); code != 403 {
				t.Errorf("%s %s by %s: status %d, want 403: %s", call[0], path, bearer, code, body)
			}
		}
	}
	if ok, _ := st.RetentionOverrideActive(ctx, "acme", "other", time.Now()); !ok {
		t.Error("a refused DELETE cleared the override")
	}

	// Each operator sees its own; an admin sees all, or one tenant's.
	list := func(bearer, query string) []retentionOverrideResponse {
		t.Helper()
		code, body := bearerCall(t, ts, bearer, "GET", "/v1/_retention/overrides"+query, "")
		if code != 200 {
			t.Fatalf("list by %s: %d %s", bearer, code, body)
		}
		var out struct {
			Overrides []retentionOverrideResponse `json:"overrides"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatal(err)
		}
		return out.Overrides
	}
	if got := list(opsBearer, ""); len(got) != 1 || got[0].Tenant != "acme" {
		t.Errorf("acme's operator sees %+v, want its own one", got)
	}
	if got := list(opsBearer, "?tenant=globex"); len(got) != 1 || got[0].Tenant != "acme" {
		t.Errorf("acme's operator asking for globex sees %+v, want its own one", got)
	}
	if got := list(adminBearer, ""); len(got) != 2 {
		t.Errorf("an admin sees %+v, want both", got)
	}
	if got := list(adminBearer, "?tenant=globex"); len(got) != 1 || got[0].Tenant != "globex" {
		t.Errorf("an admin asking for globex sees %+v", got)
	}
	// The retention report carries them too, for the console's banner.
	if code, body := bearerCall(t, ts, opsBearer, "GET", "/v1/_retention", ""); code != 200 || !strings.Contains(body, `"no_trace_overrides":[{"tenant":"acme","agent":"other"`) {
		t.Errorf("retention report: %d %s", code, body)
	}

	// A bad request sets nothing.
	for what, body := range map[string]string{
		"no until":         `{"agent":"other"}`,
		"in the past":      overrideBody("other", -time.Minute, ""),
		"more than a day":  overrideBody("other", 25*time.Hour, ""),
		"no agent":         overrideBody("", time.Hour, ""),
		"an unknown field": overrideBody("other", time.Hour, `,"forever":true`),
	} {
		if code, resp := bearerCall(t, ts, adminBearer, "PUT", "/v1/_retention/overrides", body); code != 400 {
			t.Errorf("%s: status %d, want 400: %s", what, code, resp)
		}
	}
}

func TestRetentionOverride_IsAuditedAndRefusedWhenItCannotBe(t *testing.T) {
	s, ts, st := overrideFixture(t)
	sink := &recordingSink{}
	s.SetTokenAudit(sink)

	if code, body := bearerCall(t, ts, opsBearer, "PUT", "/v1/_retention/overrides", overrideBody("other", time.Hour, "")); code != 200 {
		t.Fatalf("set: %d %s", code, body)
	}
	if code, _ := bearerCall(t, ts, opsBearer, "DELETE", "/v1/_retention/overrides?agent=other", ""); code != 204 {
		t.Fatalf("clear: %d", code)
	}
	// Clearing what is not there is not an event.
	if code, _ := bearerCall(t, ts, opsBearer, "DELETE", "/v1/_retention/overrides?agent=other", ""); code != 204 {
		t.Fatalf("second clear: %d", code)
	}
	if len(sink.events) != 2 {
		t.Fatalf("audit events = %+v, want a set and a clear", sink.events)
	}
	set, clear := sink.events[0], sink.events[1]
	if set.Action != "retention_override_set" || set.TargetTenant != "acme" || set.TargetName != "other" || set.ActorSubject != "ops" || set.RetentionUntil == "" {
		t.Errorf("set record = %+v", set)
	}
	if clear.Action != "retention_override_clear" || clear.TargetName != "other" || clear.ActorSubject != "ops" {
		t.Errorf("clear record = %+v", clear)
	}

	// An override that cannot be audited is not set.
	s.SetTokenAudit(failingSink{err: errors.New("disk full")})
	if code, body := bearerCall(t, ts, opsBearer, "PUT", "/v1/_retention/overrides", overrideBody("other", time.Hour, "")); code != 503 || !strings.Contains(body, "audit_unavailable") {
		t.Errorf("set with a failing audit log: %d %s, want 503 audit_unavailable", code, body)
	}
	if ok, _ := st.RetentionOverrideActive(context.Background(), "acme", "other", time.Now()); ok {
		t.Error("an override was set although its audit record could not be written")
	}
}
