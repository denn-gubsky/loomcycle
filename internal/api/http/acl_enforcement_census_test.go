package http

import (
	"encoding/json"
	"sort"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// The route gate asks exactly the pairs the enforcement table says it asks
// (RFC FY Part E): a pair the table credits to the HTTP routes that no route
// asks would let a rule on it be stored as enforced, and a pair a route asks
// that the table does not list would refuse a rule that holds.
func TestACLEnforcement_HTTPRoutesAskWhatTheTableSays(t *testing.T) {
	asked := map[string]bool{}
	for _, r := range eqRoutes(t) {
		if s, ok := authz.HTTPSurface(r.method, r.path); ok {
			asked[s.Action+" "+s.Type] = true
		}
	}
	assertSiteAsks(t, authz.SiteHTTPRoute, asked)
}

// assertSiteAsks compares the pairs a site really asks with the table's.
func assertSiteAsks(t *testing.T, site authz.Site, asked map[string]bool) {
	t.Helper()
	if len(asked) == 0 {
		t.Fatalf("%s asked nothing: the enumeration is broken", site)
	}
	table := map[string]bool{}
	for _, p := range authz.PairsAskedAt(site) {
		table[p] = true
		if !asked[p] {
			t.Errorf("the enforcement table credits %q to %s, which never asks it", p, site)
		}
	}
	var missing []string
	for p := range asked {
		if !table[p] {
			missing = append(missing, p)
		}
	}
	sort.Strings(missing)
	for _, p := range missing {
		t.Errorf("%s asks %q, which the enforcement table does not credit to it", site, p)
	}
}

// The shared connector operations ask exactly the pairs the table credits to
// them.
func TestACLEnforcement_ConnectorOperationsAskWhatTheTableSays(t *testing.T) {
	asked := map[string]bool{}
	for _, s := range connectorQuestions {
		asked[s.Action+" "+s.Type] = true
	}
	for _, in := range []string{`{"op":"set"}`, `{"op":"sql_exec"}`} {
		if q := memoryOpQuestion([]byte(in)); q == nil || !asked[q.Action+" "+q.Type] {
			t.Errorf("Memory %s asks %+v, which connectorQuestions does not list", in, q)
		}
	}
	// Every op the Channel tool's schema offers is mapped to a question, so
	// no op reaches the tool unasked; the op list is the schema's own.
	var schema struct {
		Properties struct {
			Op struct {
				Enum []string `json:"enum"`
			} `json:"op"`
		} `json:"properties"`
	}
	if err := json.Unmarshal((&builtin.Channel{}).InputSchema(), &schema); err != nil || len(schema.Properties.Op.Enum) < 5 {
		t.Fatalf("the Channel schema's op enum could not be read: %v %v", err, schema.Properties.Op.Enum)
	}
	for _, op := range schema.Properties.Op.Enum {
		qs, err := channelOpQuestions([]byte(`{"op":"` + op + `","channel":"c","channels":["c"]}`))
		if err != nil || len(qs) == 0 {
			t.Errorf("Channel op %s asks no question (%v)", op, err)
		}
		for _, q := range qs {
			if !asked[q.Action+" "+q.Type] {
				t.Errorf("Channel op %s asks %s %s, which connectorQuestions does not list", op, q.Action, q.Type)
			}
		}
	}
	if _, err := channelOpQuestions([]byte(`{"op":"no_such_op","channel":"c"}`)); err == nil {
		t.Error("an op the table does not map was let through unasked")
	}
	assertSiteAsks(t, authz.SiteConnector, asked)
}
