package builtin

import (
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// reportShellUse adds to op=self what the run holds of the shell tools a
// future release removes (RFC GW §9): shell_credentials, the credential names
// its shell receives, and deprecations, each deprecated tool or field the run
// was given with what replaces it (RFC GW P14, owner ruling Q5), so an agent
// can prefer the replacement before the removal reaches it. Only the run's
// own grant is read: never the deployment's environment, and never a
// credential's value. deprecations is absent when the run holds none.
func reportShellUse(out map[string]any, cfg *config.Config, p tools.RunPolicy) {
	if sc := shellCredentialsReport(cfg, p.ShellCreds); sc != nil {
		out["shell_credentials"] = sc
	}
	if d := selfDeprecations(p); len(d) > 0 {
		out["deprecations"] = d
	}
}

// selfDeprecations is one {name, replacement} per table row the run uses, in
// table order: a tool its effective list names, and shell_credentials when
// the run is under a narrowing. A sandbox server's tools are one row
// (mcp__sandbox__), as the run-time count reads them.
func selfDeprecations(p tools.RunPolicy) []map[string]string {
	used := map[string]bool{}
	for _, name := range p.Tools {
		if d, ok := config.DeprecatedTool(name); ok {
			used[d.Kind+"\x00"+d.Name] = true
		}
	}
	if p.ShellCreds.Set {
		used[config.DeprecatedKindField+"\x00shell_credentials"] = true
	}
	var out []map[string]string
	for _, d := range config.Deprecations() {
		if used[d.Kind+"\x00"+d.Name] {
			out = append(out, map[string]string{"name": d.Name, "replacement": d.Replacement})
		}
	}
	return out
}
