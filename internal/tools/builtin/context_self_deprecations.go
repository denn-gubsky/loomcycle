package builtin

import (
	"slices"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// reportShellUse adds to op=self `deprecations`: each item v1.172.0 removed
// that the run's definition still names, with what replaces it (RFC GW P15
// Q3; the key keeps its P14 name). A tool listed there was not given to the
// run, and a field does nothing, so an agent can see why it lacks a shell and
// what to use instead. Only names are read: never a credential the removed
// field listed. Absent when the definition names none.
func reportShellUse(out map[string]any, p tools.RunPolicy) {
	if d := selfDeprecations(p); len(d) > 0 {
		out["deprecations"] = d
	}
}

// selfDeprecations is one {name, replacement} per removed item the run's
// definition names, in table order.
func selfDeprecations(p tools.RunPolicy) []map[string]string {
	var out []map[string]string
	for _, r := range config.Removals() {
		if slices.Contains(p.Removed, r.Name) {
			out = append(out, map[string]string{"name": r.Name, "replacement": r.Line(r.Name)})
		}
	}
	return out
}
