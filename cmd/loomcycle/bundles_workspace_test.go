package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// movedBundleAgents are the embedded bundle agents RFC GW P14 moved onto
// workspaces, with the shell tool each keeps until P15 (owner ruling Q4 a).
var movedBundleAgents = []struct {
	bundle, agent, keeps string
}{
	{"team-examples", "sdlc/coder", "Bashbox"},
	{"team-examples", "sdlc/reviewer", "Bashbox"},
	{"agent-teams", "team/orchestrator", "Bashbox"},
	{"chat", "chat/medium", "Bash"},
	{"chat", "chat/local", "Bash"},
	{"chat", "chat/local-small", "Bash"},
}

// Each moved agent holds Workspace with the narrowest grant (D-P14-6: no
// share, expose, templates or connect), keeps its old shell tool until P15,
// and its bundle loads on an install with workspaces off without an advisory
// naming it. The runtime suite test/runtime/workspace-bundles runs the
// documented tasks on a workspace with the shell tool withheld.
func TestBundles_MovedAgentsHoldANarrowWorkspaceGrantAndKeepTheirShellTool(t *testing.T) {
	t.Setenv("LOOMCYCLE_SKILLS_ROOT", "")
	for _, m := range movedBundleAgents {
		t.Run(m.agent, func(t *testing.T) {
			cfg, err := config.LoadLayers(bootStackFor(t, "base", m.bundle)...)
			if err != nil {
				t.Fatalf("load base,%s: %v", m.bundle, err)
			}
			a, ok := cfg.Agents[m.agent]
			if !ok {
				t.Fatalf("%s has no agent %s", m.bundle, m.agent)
			}
			if !slices.Contains(a.Tools, "Workspace") || !slices.Contains(a.Tools, m.keeps) {
				t.Errorf("tools = %v, want Workspace and %s", a.Tools, m.keeps)
			}
			w := a.Workspace
			if w == nil || !w.Open || !slices.Contains(w.Ops, "exec") {
				t.Fatalf("workspace = %+v, want open with exec", w)
			}
			if w.Share || w.Expose || w.Templates || w.Connect {
				t.Errorf("workspace = %+v, want no share, expose, templates or connect", w)
			}
			for _, warn := range cfg.Warnings {
				if strings.Contains(warn, `"`+m.agent+`"`) {
					t.Errorf("advisory names the bundle's own agent: %s", warn)
				}
			}
		})
	}
}
