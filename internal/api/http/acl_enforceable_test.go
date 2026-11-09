package http

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A rule naming a pair no surface asks is refused at write, on the route and
// on its twin, with a code a client can branch on and a message naming the
// pair (RFC FY Part E). Before, it was stored and decided nothing.
func TestACLDef_ARuleOnAnUnenforcedPairIsRefused(t *testing.T) {
	srv, ts, _ := aclMgmtServer(t, authz.ModeCompat)
	inert := `[{"effect":"deny","actions":["mount"],"resources":["volume:data"]}]`
	for _, op := range []string{"create", "update", "put", "validate"} {
		code, body := bearerCall(t, ts, operatorBearer, "POST", "/v1/_acldef", `{"op":"`+op+`","name":"inert","rules":`+inert+`}`)
		if code != 400 || jsonField(t, body, "code") != "rule_not_enforceable" || !strings.Contains(body, "mount volume") {
			t.Errorf("%s: %d %s, want 400 rule_not_enforceable naming the pair", op, code, body)
		}
	}
	op := aclCaller(t, srv, "acme", "ops", auth.ScopeTenant)
	res := twinCall(t, srv, op, "acldef", `{"op":"create","name":"inert","rules":`+inert+`}`)
	if !res.IsError || textCode(res.Text) != "rule_not_enforceable" {
		t.Errorf("twin create: %s, want rule_not_enforceable", res.Text)
	}
	// An enforced pair is still written.
	if code, body := bearerCall(t, ts, operatorBearer, "POST", "/v1/_acldef", `{"op":"create","name":"live","rules":[{"effect":"deny","actions":["invoke"],"resources":["tool:Bash"]}]}`); code != 201 {
		t.Errorf("an enforced rule: %d %s", code, body)
	}
}

// A verb removed from the catalog is refused at write, left out of the
// catalog listing a rule picker is built from, and a question on it is
// refused by the simulator rather than answered.
func TestACLDef_ARemovedVerbIsRefusedAndUnlisted(t *testing.T) {
	_, ts, _ := aclMgmtServer(t, authz.ModeCompat)
	for _, rule := range []string{
		`{"effect":"deny","actions":["use"],"resources":["search:*"]}`,
		`{"effect":"deny","actions":["submit"],"resources":["evaluation:*"]}`,
		`{"effect":"deny","actions":["raise"],"resources":["interruption:*"]}`,
	} {
		code, body := bearerCall(t, ts, operatorBearer, "POST", "/v1/_acldef", `{"op":"create","name":"gone","rules":[`+rule+`]}`)
		if code != 400 || jsonField(t, body, "code") != "rule_not_enforceable" {
			t.Errorf("%s: %d %s, want rule_not_enforceable", rule, code, body)
		}
	}
	_, body := bearerCall(t, ts, operatorBearer, "POST", "/v1/_acldef", `{"op":"catalog"}`)
	var cat struct {
		Types []struct {
			Type  string   `json:"type"`
			Verbs []string `json:"verbs"`
		} `json:"types"`
	}
	if err := json.Unmarshal([]byte(body), &cat); err != nil || len(cat.Types) == 0 {
		t.Fatalf("catalog: %v %s", err, body)
	}
	for _, ty := range cat.Types {
		for _, v := range ty.Verbs {
			if !authz.IsEnforced(ty.Type, v) {
				t.Errorf("the catalog lists %s %s, which no surface asks", v, ty.Type)
			}
		}
		if ty.Type == "search" || ty.Type == "interruption" {
			t.Errorf("the catalog lists the removed type %s", ty.Type)
		}
	}
	code, body := bearerCall(t, ts, operatorBearer, "POST", "/v1/_authz/evaluation", `{"action":{"name":"use"},"resource":{"type":"search","id":"x"}}`)
	if code != 400 || jsonField(t, body, "code") != "rule_not_enforceable" {
		t.Errorf("evaluation of a removed verb: %d %s, want rule_not_enforceable", code, body)
	}
	code, body = bearerCall(t, ts, operatorBearer, "POST", "/v1/_authz/simulate",
		`{"acls":[{"name":"draft","rules":[{"effect":"deny","actions":["mount"],"resources":["volume:data"]}]}],"requests":[{"action":{"name":"read"},"resource":{"type":"run"}}]}`)
	if code != 400 || jsonField(t, body, "code") != "rule_not_enforceable" {
		t.Errorf("simulate with an unenforced draft rule: %d %s, want rule_not_enforceable", code, body)
	}
}

// A list stored before the refusal, or declared in config, is reported at
// boot by class and still loads: an unenforced pair, an inert tier rule, a
// source_cidr deny whose meaning changed, and a transport http rule that no
// longer covers mcp or webui requests.
func TestReportUnenforceableACLs_NamesEachClass(t *testing.T) {
	srv, _, st := aclMgmtServerWith(t, authz.ModeCompat, func(c *config.Config) {
		c.ResolvedACL = &authz.Static{ACLs: map[string]authz.ACL{"yaml-old": {Name: "yaml-old", Rules: []authz.Rule{
			{Effect: authz.Deny, Actions: []string{"use"}, Resources: []string{"search:*"}},
		}}}}
	})
	rules := `[{"effect":"deny","actions":["invoke"],"resources":["tool:Bash"]},` +
		`{"effect":"deny","actions":["recall"],"resources":["memory:user"]},` +
		`{"effect":"deny","actions":["run"],"resources":["agent:*"],"when":{"user_tier":["free"]}},` +
		`{"effect":"deny","actions":["run"],"resources":["agent:*"],"when":{"source_cidr":["198.51.100.0/24"]}},` +
		`{"effect":"deny","actions":["run"],"resources":["agent:*"],"when":{"transport":["http"]}},` +
		`{"effect":"deny","actions":["run"],"resources":["agent:*"],"when":{"transport":["bridge"]}}]`
	if _, err := st.AclDefPut(context.Background(), store.AclDefRow{TenantID: "acme", Name: "old", Rules: json.RawMessage(rules)}); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(srv.ReportUnenforceableACLs(context.Background()), "\n")
	for _, want := range []string{
		"unenforced store acme/old#1: recall memory",
		"inert store acme/old#2",
		"source_cidr_enforced store acme/old#3",
		"transport_http store acme/old#4",
		`unenforced store acme/old#5: condition "transport"`,
		"unenforced config /yaml-old#0: use search",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "acme/old#0") || strings.Contains(got, "unenforced store acme/old#2") {
		t.Errorf("an enforced rule was reported, or an inert one as unenforced: %q", got)
	}
}

// A member reads a colleague's scope only through an explicit browse allow,
// so the operator must be able to write one: browse is asked, not refused as
// a pair no surface evaluates.
func TestACLDef_ABrowseAllowIsWritten(t *testing.T) {
	_, ts, _ := aclMgmtServer(t, authz.ModeCompat)
	for _, typ := range []string{"memory", "document"} {
		rules := `[{"effect":"allow","actions":["browse"],"resources":["` + typ + `:user"]}]`
		if code, body := bearerCall(t, ts, operatorBearer, "POST", "/v1/_acldef", `{"op":"create","name":"browse-`+typ+`","rules":`+rules+`}`); code != 201 {
			t.Errorf("a browse allow on %s: %d %s, want 201", typ, code, body)
		}
	}
}

// A selector naming some instances of a verb asked only of every instance
// (the run verbs, read agent, read skill, read evaluation) decides nothing as
// written — a deny refuses nothing, an allow admits every instance — so every
// write, the simulator's draft, and an evaluation or explanation of such a
// named instance refuse it (RFC GD D2). The same rule on every instance is
// written.
func TestACLDef_AnInstanceSelectorOnATypeLevelOnlyVerbIsRefused(t *testing.T) {
	srv, ts, _ := aclMgmtServer(t, authz.ModeCompat)
	for _, rule := range []string{
		`{"effect":"deny","actions":["read"],"resources":["run:tenant"]}`,
		`{"effect":"allow","actions":["cancel"],"resources":["run:own"]}`,
		`{"effect":"deny","actions":["read"],"resources":["agent:secret/*"]}`,
	} {
		for _, op := range []string{"create", "validate"} {
			code, body := bearerCall(t, ts, operatorBearer, "POST", "/v1/_acldef", `{"op":"`+op+`","name":"inert","rules":[`+rule+`]}`)
			if code != 400 || jsonField(t, body, "code") != "rule_not_enforceable" {
				t.Errorf("%s %s: %d %s, want 400 rule_not_enforceable", op, rule, code, body)
			}
		}
	}
	op := aclCaller(t, srv, "acme", "ops", auth.ScopeTenant)
	if res := twinCall(t, srv, op, "acldef", `{"op":"create","name":"inert","rules":[{"effect":"deny","actions":["read"],"resources":["run:tenant"]}]}`); !res.IsError || textCode(res.Text) != "rule_not_enforceable" {
		t.Errorf("twin create: %s, want rule_not_enforceable", res.Text)
	}
	code, body := bearerCall(t, ts, operatorBearer, "POST", "/v1/_authz/simulate",
		`{"acls":[{"name":"draft","rules":[{"effect":"deny","actions":["read"],"resources":["run:tenant"]}]}],"requests":[{"action":{"name":"read"},"resource":{"type":"run"}}]}`)
	if code != 400 || jsonField(t, body, "code") != "rule_not_enforceable" {
		t.Errorf("simulate with a draft deny on run:tenant: %d %s, want rule_not_enforceable", code, body)
	}
	for _, path := range []string{"/v1/_authz/evaluation", "/v1/_authz/explain"} {
		code, body := bearerCall(t, ts, operatorBearer, "POST", path, `{"action":{"name":"read"},"resource":{"type":"run","id":"tenant"}}`)
		if code != 400 || jsonField(t, body, "code") != "rule_not_enforceable" {
			t.Errorf("%s of read run:tenant: %d %s, want rule_not_enforceable", path, code, body)
		}
	}
	if code, body := bearerCall(t, ts, operatorBearer, "POST", "/v1/_acldef", `{"op":"create","name":"live","rules":[{"effect":"deny","actions":["read"],"resources":["run:*"]}]}`); code != 201 {
		t.Errorf("a deny on every run: %d %s, want 201", code, body)
	}
}

// One stored before the refusal is reported at boot in its own class, and
// explained: its trace says the selector does not do what it says.
func TestReportUnenforceableACLs_NamesAStoredInstanceSelectorOnATypeLevelOnlyVerb(t *testing.T) {
	srv, ts, st := aclMgmtServer(t, authz.ModeCompat)
	rules := `[{"effect":"deny","actions":["read"],"resources":["run:tenant"]}]`
	if _, err := st.AclDefPut(context.Background(), store.AclDefRow{TenantID: "acme", Name: "old-runs", Rules: json.RawMessage(rules)}); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(srv.ReportUnenforceableACLs(context.Background()), "\n")
	if !strings.Contains(got, "type_level_only store acme/old-runs#0: read run:tenant") {
		t.Errorf("report %q lacks the stored run:tenant deny", got)
	}
	if code, body := bearerCall(t, ts, operatorBearer, "POST", "/v1/_aclbinding", `{"acl":"old-runs","subject_kind":"user","subject_id":"ops"}`); code != 201 {
		t.Fatalf("bind: %d %s", code, body)
	}
	_, body := bearerCall(t, ts, operatorBearer, "POST", "/v1/_authz/explain", `{"action":{"name":"read"},"resource":{"type":"run"}}`)
	if !strings.Contains(body, "this deny refuses nothing") {
		t.Errorf("explain of read run:* does not flag the stored deny: %s", body)
	}
}
