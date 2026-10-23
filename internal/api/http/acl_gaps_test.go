package http

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Gaps the Web UI's Access console found in the management surface. Each
// holds on HTTP and, where the operation has a twin, on the twin too.

// An open-mode server reports its access-control mode like any other: the
// console reads it from /v1/_me to say how rules are applied.
func TestWhoami_OpenModeReportsTheACLMode(t *testing.T) {
	cfg := makeBaseConfig()
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "open.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, nil, []tools.Tool{}, concurrency.New(2, 2, time.Second), st)
	srv.SetACLResolver(aclres.New(st, authz.ModeShadow, 0))
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	code, body := bearerCall(t, ts, "", "GET", "/v1/_me", "")
	if code != 200 || !strings.Contains(body, `"open_mode":true`) || jsonField(t, body, "acl_mode") != "shadow" {
		t.Errorf("open-mode whoami: %d %s, want acl_mode shadow", code, body)
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// A time that was never set is left out, rather than written as year 1; a
// set one keeps its RFC 3339 form. "" asks for no expiry.
func TestACLRows_LeaveUnsetTimesOut(t *testing.T) {
	srv, ts, _ := aclMgmtServer(t, authz.ModeCompat)
	rules := `[{"effect":"deny","actions":["invoke"],"resources":["tool:Bash"]}]`
	code, body := bearerCall(t, ts, operatorBearer, "POST", "/v1/_acldef", `{"op":"create","name":"no-shell","rules":`+rules+`}`)
	if code != 201 || strings.Contains(body, "0001-01-01") || strings.Contains(body, "retired_at") {
		t.Errorf("a live list: %d %s, want no retired_at", code, body)
	}
	code, body = bearerCall(t, ts, operatorBearer, "POST", "/v1/_aclbinding", `{"acl":"no-shell","subject_kind":"user","subject_id":"alice","expires_at":""}`)
	if code != 201 || strings.Contains(body, "0001-01-01") || strings.Contains(body, "expires_at") || strings.Contains(body, "retired_at") {
		t.Fatalf("a binding with expires_at \"\": %d %s, want 201 and no times it does not have", code, body)
	}
	id, _ := jsonField(t, body, "binding_id").(string)
	until := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339)
	code, body = bearerCall(t, ts, operatorBearer, "POST", "/v1/_aclbinding", `{"acl":"no-shell","subject_kind":"user","subject_id":"bob","expires_at":"`+until+`"}`)
	got, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(jsonField(t, body, "expires_at")))
	want, _ := time.Parse(time.RFC3339, until)
	if code != 201 || !got.Equal(want) {
		t.Errorf("a binding with an expiry: %d %s, want expires_at %s", code, body, until)
	}
	if code, body := bearerCall(t, ts, operatorBearer, "GET", "/v1/_aclbinding", ""); code != 200 || strings.Contains(body, "0001-01-01") {
		t.Errorf("binding list: %d %s", code, body)
	}
	if code, _ := bearerCall(t, ts, operatorBearer, "DELETE", "/v1/_aclbinding/"+id, ""); code != 200 {
		t.Fatalf("retire: %d", code)
	}
	if code, body := bearerCall(t, ts, operatorBearer, "GET", "/v1/_aclbinding?include_retired=1", ""); code != 200 || !strings.Contains(body, `"retired_at":"`) || strings.Contains(body, "0001-01-01") {
		t.Errorf("binding list with the retired one: %d %s", code, body)
	}
	// The twin takes "" too.
	op := aclCaller(t, srv, "acme", "ops", auth.ScopeTenant)
	res := twinCall(t, srv, op, "aclbinding", `{"op":"create","acl":"no-shell","subject_kind":"user","subject_id":"carol","expires_at":""}`)
	if res.IsError || strings.Contains(res.Text, "expires_at") {
		t.Errorf("twin binding with expires_at \"\": %s", res.Text)
	}
}

// A binding to a list that no layer has (stored, shared or config-declared)
// applies nothing, and one that has already expired is never in force:
// both are refused rather than stored.
func TestACLBindings_RefuseAMissingListAndAPastExpiry(t *testing.T) {
	srv, ts, _ := aclMgmtServerWith(t, authz.ModeCompat, func(c *config.Config) {
		c.ResolvedACL = &authz.Static{ACLs: map[string]authz.ACL{"from-config": {Name: "from-config",
			Rules: []authz.Rule{{Effect: authz.Deny, Actions: []string{"invoke"}, Resources: []string{"tool:Bash"}}}}}}
	})
	rules := `[{"effect":"deny","actions":["invoke"],"resources":["tool:Bash"]}]`
	if code, body := bearerCall(t, ts, operatorBearer, "POST", "/v1/_acldef", `{"op":"create","name":"retired-one","rules":`+rules+`}`); code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}
	if code, _ := bearerCall(t, ts, operatorBearer, "POST", "/v1/_acldef", `{"op":"retire","name":"retired-one"}`); code != 200 {
		t.Fatal("retire")
	}
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	for _, c := range []struct{ body, want string }{
		{`{"acl":"nope","subject_kind":"user","subject_id":"alice"}`, "does not exist"},
		{`{"acl":"retired-one","subject_kind":"user","subject_id":"alice"}`, "does not exist"},
		{`{"acl":"builtin:admin","subject_kind":"user","subject_id":"alice"}`, "does not exist"},
		{`{"acl":"from-config","subject_kind":"user","subject_id":"alice","expires_at":"` + past + `"}`, "in the past"},
	} {
		code, body := bearerCall(t, ts, operatorBearer, "POST", "/v1/_aclbinding", c.body)
		if code != 400 || !strings.Contains(body, c.want) {
			t.Errorf("%s: %d %s, want 400 %q", c.body, code, body, c.want)
		}
	}
	if code, body := bearerCall(t, ts, operatorBearer, "POST", "/v1/_aclbinding", `{"acl":"from-config","subject_kind":"user","subject_id":"alice"}`); code != 201 {
		t.Errorf("a binding to a config-declared list: %d %s", code, body)
	}
	op := aclCaller(t, srv, "acme", "ops", auth.ScopeTenant)
	if res := twinCall(t, srv, op, "aclbinding", `{"op":"create","acl":"nope","subject_kind":"user","subject_id":"alice"}`); !res.IsError || !strings.Contains(res.Text, "does not exist") {
		t.Errorf("twin binding to a missing list: %s", res.Text)
	}
}

// A group an identity provider owns (external_id set) is read-only on the
// management surfaces: its members are managed at the provider, and an
// upsert that could clear the mark is refused too.
func TestACLGroups_AnIdPGroupIsReadOnlyLocally(t *testing.T) {
	srv, ts, _ := aclMgmtServer(t, authz.ModeCompat)
	if code, body := bearerCall(t, ts, operatorBearer, "POST", "/v1/_groups", `{"name":"okta-eng","external_id":"00g1"}`); code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/v1/_groups/okta-eng/members", `{"kind":"user","id":"alice"}`},
		{"DELETE", "/v1/_groups/okta-eng/members/user/alice", ""},
		{"POST", "/v1/_groups", `{"name":"okta-eng"}`},
	} {
		if code, body := bearerCall(t, ts, operatorBearer, c.method, c.path, c.body); code != 409 || !strings.Contains(body, "group_external") {
			t.Errorf("%s %s on an IdP group: %d %s, want 409 group_external", c.method, c.path, code, body)
		}
	}
	op := aclCaller(t, srv, "acme", "ops", auth.ScopeTenant)
	if res := twinCall(t, srv, op, "groupdef", `{"op":"add_member","name":"okta-eng","kind":"user","id":"alice"}`); !strings.Contains(res.Text, "group_external") {
		t.Errorf("twin add_member on an IdP group: %s", res.Text)
	}
}
