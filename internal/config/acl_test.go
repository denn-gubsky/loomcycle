package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/denn-gubsky/loomcycle/internal/authz"
)

const aclYAML = `
acls:
  support-tier-1:
    description: Support staff
    rules:
      - { effect: allow, actions: [run, read], resources: ["agent:chat/*"] }
      - { effect: deny, actions: [invoke], resources: ["tool:Bash"],
          when: { transport: [http], time: { days: [mon, tue], between: ["07:00", "20:00"], tz: UTC } } }
groups:
  support:
    tenant: acme
    description: tier 1
    members: [alice, bob]
    groups: [support-contractors]
  support-contractors:
    tenant: acme
    members: [carol]
acl_bindings:
  - { acl: support-tier-1, group: support, tenant: acme }
  - { acl: read-only, tenant_wide: true, tenant: acme }
  - { acl: support-tier-1, user: dave }
`

func loadACL(t *testing.T, src string) (*Config, error) {
	t.Helper()
	var c Config
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	return &c, ResolveACLConfig(&c)
}

func TestResolveACLConfig_BuildsTheStaticSet(t *testing.T) {
	c, err := loadACL(t, aclYAML)
	if err != nil {
		t.Fatal(err)
	}
	st := c.ResolvedACL
	if st == nil || len(st.ACLs) != 1 || len(st.Groups) != 2 || len(st.Bindings) != 3 {
		t.Fatalf("resolved: %+v", st)
	}
	a := st.ACLs["support-tier-1"]
	if a.Name != "support-tier-1" || a.Description != "Support staff" || len(a.Rules) != 2 || a.Rules[1].When == nil || a.Rules[1].When.Time.TZ != "UTC" {
		t.Errorf("acl: %+v", a)
	}
	direct, graph := st.GroupsOf("acme", "carol")
	if len(direct) != 1 || direct[0] != "support-contractors" || len(graph["support"]) != 1 {
		t.Errorf("carol: direct=%v graph=%v", direct, graph)
	}
	if got := authz.Closure(graph, direct); len(got) != 2 || got[0] != "support" {
		t.Errorf("carol closure = %v", got)
	}
	if d, _ := st.GroupsOf("other", "alice"); len(d) != 0 {
		t.Errorf("another tenant sees alice in %v", d)
	}
	if b := st.BindingsIn("acme"); len(b) != 2 || b[1].SubjectKind != authz.SubjectTenant {
		t.Errorf("acme bindings = %+v", b)
	}
	if b := st.BindingsIn(""); len(b) != 1 || b[0].SubjectID != "dave" {
		t.Errorf("shared-tenant bindings = %+v", b)
	}
	// Nothing declared leaves nil.
	if c, err := loadACL(t, "agents: {}\n"); err != nil || c.ResolvedACL != nil {
		t.Errorf("empty: %+v %v", c.ResolvedACL, err)
	}
}

func TestResolveACLConfig_RefusesWhatTheEngineWould(t *testing.T) {
	cases := map[string]string{
		"bad verb":             "acls:\n  x:\n    rules:\n      - { effect: allow, actions: [read], resources: [\"tool:*\"] }\n",
		"unknown field":        "acls:\n  x:\n    rules:\n      - { effect: allow, actions: [run], resources: [\"agent:*\"], extra: 1 }\n",
		"unknown type":         "acls:\n  x:\n    rules:\n      - { effect: allow, actions: [run], resources: [\"nosuch:*\"] }\n",
		"builtin name":         "acls:\n  builtin:admin:\n    rules:\n      - { effect: allow, actions: [run], resources: [\"agent:*\"] }\n",
		"empty rules":          "acls:\n  x:\n    rules: []\n",
		"group cycle":          "groups:\n  a: { groups: [b] }\n  b: { groups: [a] }\n",
		"group self":           "groups:\n  a: { groups: [a] }\n",
		"group undeclared":     "groups:\n  a: { groups: [nosuch] }\n",
		"group cross-tenant":   "groups:\n  a: { tenant: t1, groups: [b] }\n  b: { tenant: t2 }\n",
		"binding two subjects": "acl_bindings:\n  - { acl: x, user: a, group: g }\n",
		"binding no subject":   "acl_bindings:\n  - { acl: x }\n",
		"binding bad acl name": "acl_bindings:\n  - { acl: \"bad name!\", user: a }\n",
	}
	for name, src := range cases {
		if _, err := loadACL(t, src); err == nil {
			t.Errorf("%s: loaded", name)
		} else if strings.Contains(err.Error(), "yaml") {
			t.Errorf("%s: a yaml error, not a validation one: %v", name, err)
		}
	}
}
