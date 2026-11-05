package mcp

import (
	"sort"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// The meta-tool gate asks exactly the pairs the enforcement table credits to
// it (RFC FY Part E).
func TestACLEnforcement_MetaToolsAskWhatTheTableSays(t *testing.T) {
	asked := map[string]bool{}
	for name := range handlersByName {
		for _, s := range mcpToolQuestions(name) {
			asked[s.Action+" "+s.Type] = true
		}
	}
	if len(asked) == 0 {
		t.Fatal("no meta-tool asked anything")
	}
	table := map[string]bool{}
	for _, p := range authz.PairsAskedAt(authz.SiteMCPTool) {
		table[p] = true
		if !asked[p] {
			t.Errorf("the enforcement table credits %q to the meta-tools, which never ask it", p)
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
		t.Errorf("a meta-tool asks %q, which the enforcement table does not credit to the meta-tools", p)
	}
}

// Every meta-tool names its HTTP twin or why it has none, and a twin names a
// governed route: a meta-tool cannot land asking a weaker question than its
// route by omission.
func TestMCPToolTwins_EveryToolIsListed(t *testing.T) {
	for name := range handlersByName {
		tw, twinned := mcpToolTwins[name]
		_, without := mcpToolsWithoutTwin[name]
		if twinned == without {
			t.Errorf("%s: twinned=%v without=%v, want exactly one", name, twinned, without)
			continue
		}
		if twinned {
			if _, governed := authz.HTTPSurface(tw.Method, tw.Path); !governed {
				t.Errorf("%s: twin %s %s is not a governed route", name, tw.Method, tw.Path)
			}
		}
	}
	for name := range mcpToolTwins {
		if _, ok := handlersByName[name]; !ok {
			t.Errorf("mcpToolTwins lists %s, which is not a meta-tool", name)
		}
	}
	for name := range mcpToolsWithoutTwin {
		if _, ok := handlersByName[name]; !ok {
			t.Errorf("mcpToolsWithoutTwin lists %s, which is not a meta-tool", name)
		}
	}
}
