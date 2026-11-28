package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// movedBundleAgents are the embedded bundle agents RFC GW P14 moved onto
// workspaces. Since P15 (v1.172.0) a workspace is the only place they run a
// command: the shell tools they kept as a fallback are removed.
var movedBundleAgents = []struct {
	bundle, agent string
}{
	{"team-examples", "sdlc/coder"},
	{"team-examples", "sdlc/reviewer"},
	{"agent-teams", "team/orchestrator"},
	{"chat", "chat/medium"},
	{"chat", "chat/local"},
	{"chat", "chat/local-small"},
}

// Each moved agent holds Workspace with the narrowest grant (D-P14-6: no
// share, expose, templates or connect; RFC GW D22 adds expose for chat) and no removed shell tool, and its
// bundle loads on an install with workspaces off without an advisory naming
// it. The runtime suite test/runtime/workspace-bundles runs the documented
// tasks on a workspace.
func TestBundles_MovedAgentsHoldANarrowWorkspaceGrantAndNoShellTool(t *testing.T) {
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
			if !slices.Contains(a.Tools, "Workspace") {
				t.Errorf("tools = %v, want Workspace", a.Tools)
			}
			for _, removed := range []string{"Bash", "Bashbox", "VolumeDef"} {
				if slices.Contains(a.Tools, removed) {
					t.Errorf("tools = %v, want no %s", a.Tools, removed)
				}
			}
			w := a.Workspace
			if w == nil || !w.Open || !slices.Contains(w.Ops, "exec") {
				t.Fatalf("workspace = %+v, want open with exec", w)
			}
			if w.Share || w.Templates || w.Connect {
				t.Errorf("workspace = %+v, want no share, templates or connect", w)
			}
			// RFC GW D22: the chat bundle's agents carry expose, so a chat
			// can answer with an interactive page; no other moved agent does.
			if wantExpose := m.bundle == "chat"; w.Expose != wantExpose {
				t.Errorf("workspace expose = %v, want %v", w.Expose, wantExpose)
			}
			// team/repo (RFC GW P14 PR 8): the orchestrator opens the git
			// workspace and pushes its own branch.
			if m.agent == "team/orchestrator" && (!slices.Contains(a.GitRemote, "fetch") || !slices.Contains(a.GitRemote, "push")) {
				t.Errorf("git_remote = %v, want fetch and push", a.GitRemote)
			}
			for _, warn := range cfg.Warnings {
				if strings.Contains(warn, `"`+m.agent+`"`) {
					t.Errorf("advisory names the bundle's own agent: %s", warn)
				}
			}
		})
	}
}
