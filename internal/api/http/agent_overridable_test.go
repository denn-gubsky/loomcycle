package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

type overridableReport struct {
	Agent             string                      `json:"agent"`
	DefID             string                      `json:"def_id"`
	AllowOverrides    bool                        `json:"allow_overrides"`
	RestrictedByToken bool                        `json:"restricted_by_token"`
	Fields            map[string]overridableField `json:"fields"`
}

const (
	tenantOperatorBearer = "tok-tenant-operator"
	isolatedUserBearer   = "tok-isolated-user"
)

// overridableFixture is restrictedFixture plus four dynamic definitions —
// one per allow_overrides setting that matters, in acme, and one in another
// tenant — and two more tokens: acme's tenant operator and an isolated user.
//
// The static agents cover the yaml path: "other" says nothing about
// allow_overrides and "fixed" says false.
func overridableFixture(t *testing.T) (*httptest.Server, store.Store) {
	t.Helper()
	_, ts, st := restrictedFixtureServer(t)
	ctx := context.Background()
	defs := []struct{ id, name, tenant, body string }{
		{"def_opened", "opened", "acme", `{"provider":"scripted","model":"stub-model","system_prompt":"opened","tools":[],` +
			`"allow_overrides":true,"max_concurrent_children":3,"providers":["scripted","second"],` +
			`"models":{"middle":[{"provider":"scripted","model":"stub-model"},{"provider":"second","model":"model-b"}]}}`},
		{"def_sealed", "sealed", "acme", `{"provider":"scripted","model":"stub-model","system_prompt":"sealed","tools":[],"allow_overrides":false}`},
		{"def_foreign", "foreign", "globex", `{"provider":"scripted","model":"stub-model","system_prompt":"foreign","tools":[]}`},
	}
	for _, d := range defs {
		if _, err := st.AgentDefCreate(ctx, store.AgentDefRow{DefID: d.id, Name: d.name, Version: 1, TenantID: d.tenant,
			Definition: []byte(d.body), CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if err := st.AgentDefSetActive(ctx, d.tenant, d.name, d.id, ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, tok := range []struct {
		id, bearer string
		scopes     []string
	}{
		{"def_operator", tenantOperatorBearer, []string{auth.ScopeTenant}},
		{"def_isolated", isolatedUserBearer, []string{auth.ScopeUser}},
	} {
		if _, err := st.OperatorTokenDefCreate(ctx, store.OperatorTokenDefRow{
			DefID: tok.id, Name: "u-" + tok.id, TenantID: "acme", Subject: "dev",
			TokenHash: auth.HashToken(testTokenPepper, tok.bearer), AllowedScopes: tok.scopes,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return ts, st
}

func getOverridable(t *testing.T, ts *httptest.Server, bearer, agent, query string) (int, overridableReport, string) {
	t.Helper()
	code, body := bearerCall(t, ts, bearer, "GET", "/v1/_agentdef/"+agent+"/overridable"+query, "")
	var rep overridableReport
	if code == 200 {
		if err := json.Unmarshal([]byte(body), &rep); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
	}
	return code, rep, body
}

// runVerdict starts a run that sets one override and reports how the lock
// treated it. The lock answers before the run exists; an admitted run is
// abandoned as soon as its status line arrives, because what follows is the
// run, not the lock — and some overrides (interactive, review) make a run
// that waits for a person.
func runVerdict(t *testing.T, ts *httptest.Server, bearer, agent, field string) (refused bool, body string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", ts.URL+"/v1/runs", strings.NewReader(runBodyWith(agent, field, overrideSamples[field])))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/runs %s=%s: %v", field, overrideSamples[field], err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		return false, ""
	}
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode == 403 && strings.Contains(string(b), "override_not_allowed"), string(b)
}

// The report and the lock agree, field by field: for every caller and every
// kind of definition, the report says a field is overridable exactly when a
// run setting it is admitted by POST /v1/runs, and names the lock that
// refuses it otherwise.
func TestAgentOverridable_AgreesWithTheRunLockForEveryField(t *testing.T) {
	ts, _ := overridableFixture(t)
	for _, bearer := range []string{freeBearer, lockedBearer, effortOnlyBearer, legacyAdminBearer} {
		for _, agent := range []string{"other", "fixed", "opened", "sealed"} {
			code, rep, body := getOverridable(t, ts, bearer, agent, "")
			if code == 404 {
				// The legacy admin token lives in the shared tenant, where
				// acme's agents do not resolve; neither does its run.
				if _, runBody := runVerdict(t, ts, bearer, agent, "model"); !strings.Contains(runBody, "unknown agent") {
					t.Errorf("%s/%s: report 404 but the run did not say unknown agent: %s", bearer, agent, runBody)
				}
				continue
			}
			if code != 200 {
				t.Fatalf("%s/%s: status %d: %s", bearer, agent, code, body)
			}
			if len(rep.Fields) != len(auth.LockableOverrides) {
				t.Errorf("%s/%s: %d fields, want one per lockable override (%d)", bearer, agent, len(rep.Fields), len(auth.LockableOverrides))
			}
			for _, field := range auth.LockableOverrides {
				f, ok := rep.Fields[field]
				if !ok {
					t.Errorf("%s/%s: no entry for %q", bearer, agent, field)
					continue
				}
				refused, runBody := runVerdict(t, ts, bearer, agent, field)
				if f.Overridable == refused {
					t.Errorf("%s/%s %s: report says overridable=%v, the run was refused=%v: %s", bearer, agent, field, f.Overridable, refused, runBody)
					continue
				}
				switch {
				case f.Reason == overrideLockedByToken && !strings.Contains(runBody, "for this token"),
					f.Reason == overrideLockedByAgent && !strings.Contains(runBody, "allow_overrides is false"):
					t.Errorf("%s/%s %s: reason %q does not name the lock the run cited: %s", bearer, agent, field, f.Reason, runBody)
				}
			}
		}
	}
}

// What else the run path holds a field to: narrow-only fields and the
// fan-out ceiling, and the closed sets a definition declares.
func TestAgentOverridable_ReportsWhatTheRunPathAccepts(t *testing.T) {
	ts, _ := overridableFixture(t)
	code, rep, body := getOverridable(t, ts, freeBearer, "opened", "")
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	if rep.Agent != "opened" || rep.DefID != "def_opened" || !rep.AllowOverrides || rep.RestrictedByToken {
		t.Errorf("header = %+v, want opened / def_opened / allow_overrides / not restricted", rep)
	}
	want := map[string]overridableField{
		"max_concurrent_children": {Overridable: true, NarrowOnly: true, Max: 3},
		"interruption":            {Overridable: true, NarrowOnly: true},
		"effort":                  {Overridable: true, Values: []string{"high", "low", "medium"}},
		"provider":                {Overridable: true, Values: []string{"scripted", "second"}},
		"model":                   {Overridable: true, Values: []string{"model-b", "stub-model"}},
		"max_tokens":              {Overridable: true},
	}
	for field, w := range want {
		if got := rep.Fields[field]; !reflect.DeepEqual(got, w) {
			t.Errorf("%s = %+v, want %+v", field, got, w)
		}
	}

	// A static definition that declares no ceiling gets the Agent tool's
	// default, which is what a run's narrowing is checked against; it has
	// no version, and declares no provider or model set.
	_, rep, body = getOverridable(t, ts, freeBearer, "other", "")
	if strings.Contains(body, `"def_id"`) || rep.Fields["max_concurrent_children"].Max != builtin.DefaultMaxConcurrentChildren ||
		rep.Fields["model"].Values != nil || rep.Fields["provider"].Values != nil {
		t.Errorf("static agent: %+v", rep)
	}

	// The caller's own policy is folded in, and said to be.
	_, rep, _ = getOverridable(t, ts, effortOnlyBearer, "opened", "")
	if !rep.RestrictedByToken || !rep.Fields["effort"].Overridable || rep.Fields["model"].Overridable ||
		rep.Fields["model"].Reason != overrideLockedByToken || rep.Fields["model"].Values != nil {
		t.Errorf("effort-only token: restricted=%v effort=%+v model=%+v", rep.RestrictedByToken, rep.Fields["effort"], rep.Fields["model"])
	}
	_, rep, _ = getOverridable(t, ts, freeBearer, "sealed", "")
	if rep.AllowOverrides || rep.Fields["effort"].Overridable || rep.Fields["effort"].Reason != overrideLockedByAgent {
		t.Errorf("sealed agent: allow_overrides=%v effort=%+v", rep.AllowOverrides, rep.Fields["effort"])
	}
}

// An agent the caller cannot resolve — unknown, or another tenant's — is the
// same 404, so the report is no oracle for other tenants' agent names. Only
// an admin may look into another tenant, and only by saying so.
func TestAgentOverridable_UnknownAndForeignAgentsReadTheSame(t *testing.T) {
	ts, _ := overridableFixture(t)
	codeU, _, bodyU := getOverridable(t, ts, tenantOperatorBearer, "no-such", "")
	codeF, _, bodyF := getOverridable(t, ts, tenantOperatorBearer, "foreign", "")
	if codeU != 404 || codeF != 404 || !strings.Contains(bodyU, "agent_not_found") ||
		strings.ReplaceAll(bodyU, "no-such", "X") != strings.ReplaceAll(bodyF, "foreign", "X") {
		t.Errorf("unknown: %d %s / foreign: %d %s — want the same 404", codeU, bodyU, codeF, bodyF)
	}
	if code, _, body := getOverridable(t, ts, tenantOperatorBearer, "foreign", "?tenant=globex"); code != 404 {
		t.Errorf("a tenant operator's ?tenant= focus was honoured: %d %s", code, body)
	}
	if code, rep, body := getOverridable(t, ts, legacyAdminBearer, "foreign", "?tenant=globex"); code != 200 || rep.DefID != "def_foreign" {
		t.Errorf("admin focus on globex: %d %s", code, body)
	}
}

// Who may read it: the def plane's readers, plus a token limited to certain
// agents for exactly those agents.
func TestAgentOverridable_ScopeMatrix(t *testing.T) {
	path := "/v1/_agentdef/opened/overridable"
	if got := requiredScopeFor("GET", path); got != auth.ScopeTenant {
		t.Errorf("requiredScopeFor = %q, want %q", got, auth.ScopeTenant)
	}
	if !runPlaneRoute("GET", path) || runPlaneRoute("POST", path) {
		t.Error("the report must be on the run plane for GET only")
	}
	for _, p := range []string{"/v1/_agentdef/names", "/v1/_agentdef//overridable", "/v1/_agentdef/a/b/overridable", "/v1/_agentdef/a/overridable/x"} {
		if isAgentOverridablePath(p) {
			t.Errorf("%s matched the overridable route", p)
		}
	}

	ts, _ := overridableFixture(t)
	for _, c := range []struct {
		name, bearer, agent string
		code                int
		contains            string
	}{
		{"tenant operator", tenantOperatorBearer, "opened", 200, `"fields"`},
		{"non-isolated member", freeBearer, "opened", 200, `"fields"`},
		{"admin", legacyAdminBearer, "other", 200, `"fields"`},
		{"isolated user", isolatedUserBearer, "opened", 403, "insufficient scope"},
		{"agent-limited token, its agent", restrictedBearer, "allowed", 200, `"fields"`},
		{"agent-limited token, another agent", restrictedBearer, "other", 403, "agent_not_allowed"},
		{"agent-limited token, no such agent", restrictedBearer, "no-such", 403, "agent_not_allowed"},
		{"no bearer", "wrong", "opened", 401, "unauthorized"},
	} {
		code, _, body := getOverridable(t, ts, c.bearer, c.agent, "")
		if code != c.code || !strings.Contains(body, c.contains) {
			t.Errorf("%s: %d %s, want %d containing %q", c.name, code, body, c.code, c.contains)
		}
	}
	// The names listing three segments up is still the names listing.
	if code, body := bearerCall(t, ts, tenantOperatorBearer, "GET", "/v1/_agentdef/names", ""); code != 200 || strings.Contains(body, `"fields"`) {
		t.Errorf("/v1/_agentdef/names: %d %s", code, body)
	}
}

// One entry per run option the definition table lets a run set: every field
// agentDefOverridability marks overridable is reachable as a lockable
// override, except Tools, which only narrows and is never locked.
func TestAgentOverridable_CoversEveryOverridableDefField(t *testing.T) {
	reported := map[string]bool{}
	for _, wire := range auth.LockableOverrides {
		reported[lockableOverrideFields[wire]] = true
	}
	for name, kind := range agentDefOverridability {
		if kind != notOverridable && name != "Tools" && !reported[name] {
			t.Errorf("%s is overridable per agentDefOverridability but has no lockable run option, so the overridable report omits it", name)
		}
	}
	for name := range runOnlyOverridable {
		if !reported[name] {
			t.Errorf("run-only override %s is not reported", name)
		}
	}
}
