package http

import (
	"sort"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
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
