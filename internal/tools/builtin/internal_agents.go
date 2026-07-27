package builtin

import (
	"sort"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// withInternalAgents returns the set of agent names whose sessions a caller
// should look past: every agent the operator declared `internal:` (loomcycle's
// own maintenance plumbing — see config.AgentDef.Internal) plus whatever extra
// names the caller adds, deduplicated and sorted.
//
// It is a package-level function rather than a method because the two callers
// hold the config on different tools (Memory for the consolidation scan, History
// for the chat listing) and neither owns the notion.
//
// A nil Cfg yields just the extras. That is the pre-wiring path — a tool
// constructed with only a Store, as every unit test does — and it degrades to
// exactly the old behaviour rather than to a panic or an empty set.
func withInternalAgents(cfg *config.Config, extra ...string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(n string) {
		if n == "" || seen[n] {
			return
		}
		seen[n] = true
		out = append(out, n)
	}
	for _, n := range cfg.InternalAgentNames() {
		add(n)
	}
	for _, n := range extra {
		add(n)
	}
	sort.Strings(out)
	return out
}
