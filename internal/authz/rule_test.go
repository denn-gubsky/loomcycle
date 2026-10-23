package authz

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const supportACL = `{
  "name": "support-tier-1",
  "description": "Support staff",
  "rules": [
    {"effect": "allow", "actions": ["run", "read"], "resources": ["agent:chat/*", "agent:support/**"]},
    {"effect": "allow", "actions": ["read"], "resources": ["volume:shared-ro"]},
    {"effect": "allow", "actions": ["read", "write"], "resources": ["volume:support-scratch"]},
    {"effect": "deny",  "actions": ["invoke"], "resources": ["tool:Bash", "tool:Bashbox"]},
    {"effect": "deny",  "actions": ["run"], "resources": ["agent:admin/**"]},
    {"effect": "allow", "actions": ["invoke"], "resources": ["tool:*"],
     "when": {"transport": ["webui", "http"], "time": {"days": ["mon","tue","wed","thu","fri"], "between": ["07:00","20:00"], "tz": "Europe/Kyiv"}}}
  ]
}`

func TestParseACL_AcceptsTheRFCExample(t *testing.T) {
	a, err := ParseACL([]byte(supportACL))
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "support-tier-1" || len(a.Rules) != 6 {
		t.Fatalf("parsed %+v", a)
	}
	if a.Rules[5].When == nil || a.Rules[5].When.Time == nil || a.Rules[5].When.Time.TZ != "Europe/Kyiv" {
		t.Fatalf("condition not parsed: %+v", a.Rules[5].When)
	}
}

func TestParseACL_RefusesUnknownFieldsAndBadRules(t *testing.T) {
	cases := map[string]string{
		`{"name":"x","rules":[{"effect":"allow","actions":["run"],"resources":["agent:*"],"extra":1}]}`:         "unknown field",
		`{"name":"x","rules":[{"effect":"allow","actions":["run"],"resources":["agent:*"],"when":{"nope":1}}]}`: "unknown field",
		`{"name":"x","rules":[{"effect":"permit","actions":["run"],"resources":["agent:*"]}]}`:                  "not allow or deny",
		`{"name":"x","rules":[{"effect":"allow","actions":[],"resources":["agent:*"]}]}`:                        "no actions",
		`{"name":"x","rules":[{"effect":"allow","actions":["run"],"resources":[]}]}`:                            "no resources",
		`{"name":"x","rules":[{"effect":"allow","actions":["read"],"resources":["tool:*"]}]}`:                   "not a verb of tool",
		`{"name":"x","rules":[{"effect":"allow","actions":["run"],"resources":["nosuch:*"]}]}`:                  "not a catalog type",
		`{"name":"x","rules":[{"effect":"allow","actions":["run","run"],"resources":["agent:*"]}]}`:             "listed twice",
		`{"name":"x","rules":[{"effect":"allow","actions":["run"],"resources":["agent:*","agent:*"]}]}`:         "listed twice",
		`{"name":"x","rules":[]}`: "no rules",
		`{"name":"bad name!","rules":[{"effect":"allow","actions":["run"],"resources":["agent:*"]}]}`:   "not a valid name",
		`{"name":"x","rules":[{"effect":"allow","actions":["run"],"resources":["agent:*"],"when":{}}]}`: "at least one field",
		`{"name":"x","rules":[{"effect":"allow","actions":["run"],"resources":["agent:*"]}]} trailing`:  "trailing",
	}
	for in, want := range cases {
		_, err := ParseACL([]byte(in))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseACL(%s) = %v, want error containing %q", in, err, want)
		}
	}
}

func TestRule_Bounds(t *testing.T) {
	r := Rule{Effect: Allow, Actions: []string{"run"}}
	for i := 0; i <= MaxResourcesPerRule; i++ {
		r.Resources = append(r.Resources, "agent:a"+strings.Repeat("x", i%5)+string(rune('a'+i%26))+"/"+strings.Repeat("y", i/26))
	}
	if err := r.Validate(); err == nil || !strings.Contains(err.Error(), "the limit is") {
		t.Errorf("want a resources bound error, got %v", err)
	}
	a := ACL{Name: "x"}
	for i := 0; i <= MaxRulesPerACL; i++ {
		a.Rules = append(a.Rules, Rule{Effect: Allow, Actions: []string{"run"}, Resources: []string{"agent:*"}})
	}
	if err := a.Validate(); err == nil || !strings.Contains(err.Error(), "the limit is") {
		t.Errorf("want a rules bound error, got %v", err)
	}
}

func TestBinding_ValidateAndAppliesTo(t *testing.T) {
	good := []Binding{
		{ACL: "a", SubjectKind: SubjectUser, SubjectID: "alice"},
		{ACL: "a", SubjectKind: SubjectGroup, SubjectID: "support"},
		{ACL: "a", SubjectKind: SubjectToken, SubjectID: "def-1"},
		{ACL: "a", SubjectKind: SubjectTenant},
	}
	for _, b := range good {
		if err := b.Validate(); err != nil {
			t.Errorf("%+v: %v", b, err)
		}
	}
	bad := []Binding{
		{ACL: "", SubjectKind: SubjectUser, SubjectID: "alice"},
		{ACL: "a", SubjectKind: SubjectUser},
		{ACL: "a", SubjectKind: SubjectGroup, SubjectID: "bad name!"},
		{ACL: "a", SubjectKind: SubjectTenant, SubjectID: "x"},
		{ACL: "a", SubjectKind: "role", SubjectID: "x"},
	}
	for _, b := range bad {
		if err := b.Validate(); err == nil {
			t.Errorf("%+v: want an error", b)
		}
	}
	s := Subject{TenantID: "t", ID: "alice", Groups: []string{"support", "all"}, TokenDefID: "def-1"}
	for _, b := range good {
		if !b.AppliesTo(s) {
			t.Errorf("%+v should apply to %+v", b, s)
		}
	}
	for _, b := range []Binding{
		{ACL: "a", SubjectKind: SubjectUser, SubjectID: "bob"},
		{ACL: "a", SubjectKind: SubjectGroup, SubjectID: "admins"},
		{ACL: "a", SubjectKind: SubjectToken, SubjectID: "def-2"},
	} {
		if b.AppliesTo(s) {
			t.Errorf("%+v should not apply", b)
		}
	}
	now := time.Now()
	if (Binding{ExpiresAt: now.Add(-time.Second)}).Active(now) {
		t.Error("an expired binding is active")
	}
	if !(Binding{ExpiresAt: now.Add(time.Second)}).Active(now) || !(Binding{}).Active(now) {
		t.Error("a live or unbounded binding is inactive")
	}
}

// A binding with no expiry marshals without one, as the store rows do: the
// explain and simulate answers carry bindings, and an unset time.Time used
// to read 0001-01-01T00:00:00Z.
func TestBinding_MarshalLeavesAnUnsetExpiryOut(t *testing.T) {
	raw, err := json.Marshal(Binding{ACL: "x", SubjectKind: SubjectUser, SubjectID: "u"})
	if err != nil || strings.Contains(string(raw), "expires_at") {
		t.Errorf("unset expiry: %s %v", raw, err)
	}
	at := time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)
	raw, _ = json.Marshal(Binding{ACL: "x", SubjectKind: SubjectUser, SubjectID: "u", ExpiresAt: at})
	var back Binding
	if err := json.Unmarshal(raw, &back); err != nil || !back.ExpiresAt.Equal(at) || !strings.Contains(string(raw), `"expires_at":"2027-01-02T03:04:05Z"`) {
		t.Errorf("set expiry: %s → %+v %v", raw, back, err)
	}
}

// A set expiry is written in UTC whatever zone it was read in.
func TestBinding_MarshalWritesTheExpiryInUTC(t *testing.T) {
	at := time.Date(2027, 1, 2, 6, 4, 5, 0, time.FixedZone("UTC+3", 3*3600))
	raw, _ := json.Marshal(Binding{ACL: "x", SubjectKind: SubjectUser, SubjectID: "u", ExpiresAt: at})
	if !strings.Contains(string(raw), `"expires_at":"2027-01-02T03:04:05Z"`) {
		t.Errorf("expiry not in UTC: %s", raw)
	}
}
