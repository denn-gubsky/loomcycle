package config

import (
	"strings"
	"testing"
)

// RFC GW P14 (owner ruling Q4 a): a bundle's agents gain the Workspace tool
// and a grant while keeping their old shell tool, so an install that has not
// turned workspaces on is not told, once per bundle agent, that the grant
// reaches nothing. What an operator changes in such a grant is the
// operator's, and is still advised on.
func TestWorkspaceWarnings_ABundlesOwnGrantIsNotCalledInertWhileWorkspacesAreOff(t *testing.T) {
	bundle := Layer{Name: "team-examples", Embedded: true, Data: []byte(`agents:
  sdlc/coder:
    tools: [Read, Bashbox, Workspace]
    workspace: {ops: [read, write, exec], open: true}
`)}
	inert := func(cfg *Config) []string {
		var out []string
		for _, w := range cfg.Warnings {
			if strings.Contains(w, "workspace.mode is off") {
				out = append(out, w)
			}
		}
		return out
	}
	cfg, err := LoadLayers(bundle, Layer{Name: "t.yaml", Data: []byte(plainYAML)})
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	if got := inert(cfg); len(got) != 0 {
		t.Errorf("a bundle's own grant was called inert: %q", got)
	}
	if got := cfg.InertWorkspaceGrants(); len(got) != 0 {
		t.Errorf("InertWorkspaceGrants = %v, want none for a bundle's own grant", got)
	}

	overlay := Layer{Name: "t.yaml", Data: []byte(plainYAML + `agents:
  sdlc/coder:
    workspace: {egress: [pypi]}
`)}
	cfg, err = LoadLayers(bundle, overlay)
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	if got := inert(cfg); len(got) != 1 || !strings.Contains(got[0], `"sdlc/coder"`) {
		t.Errorf("an operator's change to the grant: inert advisories = %q, want one naming sdlc/coder", got)
	}
	if got := cfg.InertWorkspaceGrants(); len(got) != 1 {
		t.Errorf("InertWorkspaceGrants = %v, want sdlc/coder", got)
	}
}
