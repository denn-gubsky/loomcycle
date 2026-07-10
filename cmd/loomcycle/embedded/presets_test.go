package embedded

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestUnits_RegistryShape: the embedded registry exposes the base/local presets
// and the document-agent bundle, each with a non-empty description and the right
// kind. Guards against a renamed/dropped embedded file.
func TestUnits_RegistryShape(t *testing.T) {
	byName := map[string]Unit{}
	for _, u := range Units() {
		byName[u.Name] = u
	}
	want := map[string]string{
		"base":           "preset",
		"local":          "preset",
		"oauth":          "preset",
		"document-agent": "bundle",
		"agent-teams":    "bundle",
		"team-examples":  "bundle",
	}
	for name, kind := range want {
		u, ok := byName[name]
		if !ok {
			t.Fatalf("embedded unit %q missing (have: %v)", name, unitNames())
		}
		if u.Kind != kind {
			t.Errorf("unit %q kind = %q, want %q", name, u.Kind, kind)
		}
		if strings.TrimSpace(u.Description) == "" {
			t.Errorf("unit %q has an empty description", name)
		}
		if len(u.Data) == 0 {
			t.Errorf("unit %q has empty Data", name)
		}
	}
}

// TestUnits_ParseAsYAML: every embedded unit must be valid YAML (a malformed
// preset/bundle is a build-time content bug we want caught by tests, not at boot).
func TestUnits_ParseAsYAML(t *testing.T) {
	for _, u := range Units() {
		var tree map[string]any
		if err := yaml.Unmarshal(u.Data, &tree); err != nil {
			t.Errorf("unit %q is not valid YAML: %v", u.Name, err)
		}
	}
}

// TestBundle_AgentTeamsHasOrchestrator guards the RFC BD additions: the
// agent-teams bundle must ship the team/orchestrator agent with
// unbounded_iterations (so a long-lived team lead isn't cut off mid-workflow —
// interactivity is a per-run flag, not an agent field) and the team/orchestrate +
// team/repo skills. A silent drop would leave the bundle unable to run a team.
func TestBundle_AgentTeamsHasOrchestrator(t *testing.T) {
	data, err := Show("agent-teams")
	if err != nil {
		t.Fatalf("Show(agent-teams): %v", err)
	}
	var tree struct {
		Agents map[string]struct {
			UnboundedIterations bool     `yaml:"unbounded_iterations"`
			Tools               []string `yaml:"tools"`
			Compaction          *struct {
				Enabled *bool `yaml:"enabled"`
			} `yaml:"compaction"`
		} `yaml:"agents"`
		Skills map[string]any `yaml:"skills"`
	}
	if err := yaml.Unmarshal(data, &tree); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	orch, ok := tree.Agents["team/orchestrator"]
	if !ok {
		t.Fatalf("agent-teams bundle missing team/orchestrator agent")
	}
	if !orch.UnboundedIterations {
		t.Errorf("team/orchestrator must set unbounded_iterations")
	}
	// It holds skills + a long multi-agent driving transcript, so auto-compaction
	// must be on to keep the session within the context window.
	if orch.Compaction == nil || orch.Compaction.Enabled == nil || !*orch.Compaction.Enabled {
		t.Errorf("team/orchestrator must enable compaction (long skills+driving transcript)")
	}
	hasTool := func(want string) bool {
		for _, tl := range orch.Tools {
			if tl == want {
				return true
			}
		}
		return false
	}
	// The driver + workspace tools it can't do its job without.
	for _, tl := range []string{"Agent", "Document", "TeamDef"} {
		if !hasTool(tl) {
			t.Errorf("team/orchestrator missing tool %q", tl)
		}
	}
	for _, s := range []string{"team/orchestrate", "team/repo"} {
		if _, ok := tree.Skills[s]; !ok {
			t.Errorf("agent-teams bundle missing skill %q", s)
		}
	}
}

// TestBundle_TeamExamplesHasStarters guards the RFC BD Phase 2 starter content:
// the team-examples bundle must ship the SDLC + marketing handler agents and the
// team/examples skill (the ready-to-create TeamDefs). Without these the
// orchestrator has nothing to drive out of the box.
func TestBundle_TeamExamplesHasStarters(t *testing.T) {
	data, err := Show("team-examples")
	if err != nil {
		t.Fatalf("Show(team-examples): %v", err)
	}
	var tree struct {
		Agents map[string]any `yaml:"agents"`
		Skills map[string]any `yaml:"skills"`
	}
	if err := yaml.Unmarshal(data, &tree); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, a := range []string{"sdlc/architect", "sdlc/coder", "sdlc/reviewer", "marketing/writer", "marketing/editor"} {
		if _, ok := tree.Agents[a]; !ok {
			t.Errorf("team-examples missing handler agent %q", a)
		}
	}
	if _, ok := tree.Skills["team/examples"]; !ok {
		t.Errorf("team-examples missing the team/examples skill")
	}
	// The skill must carry both starter TeamDefs so they're actually creatable.
	body, _ := Show("team-examples")
	for _, marker := range []string{"sdlc/architect", "marketing/writer", `"entry": "architecture"`, `"entry": "draft"`} {
		if !strings.Contains(string(body), marker) {
			t.Errorf("team/examples skill missing starter marker %q", marker)
		}
	}
}

// TestResolveUnits_OrderAndUnknown: selection order is preserved (it becomes the
// layer order), and an unknown name is a fatal error listing the available names.
func TestResolveUnits_OrderAndUnknown(t *testing.T) {
	got, err := ResolveUnits([]string{"document-agent", "base"})
	if err != nil {
		t.Fatalf("ResolveUnits: %v", err)
	}
	if len(got) != 2 || got[0].Name != "document-agent" || got[1].Name != "base" {
		t.Fatalf("ResolveUnits order not preserved: %v", got)
	}
	if _, err := ResolveUnits([]string{"base", "nope"}); err == nil {
		t.Errorf("ResolveUnits with an unknown name should error")
	} else if !strings.Contains(err.Error(), "available:") {
		t.Errorf("unknown-name error should list available units, got: %v", err)
	}
}

// TestShow: returns a unit's bytes; errors with the available list on a miss.
func TestShow(t *testing.T) {
	data, err := Show("base")
	if err != nil {
		t.Fatalf("Show(base): %v", err)
	}
	if !strings.Contains(string(data), "provider_priority") {
		t.Errorf("base preset should contain provider_priority")
	}
	if _, err := Show("does-not-exist"); err == nil {
		t.Errorf("Show with an unknown name should error")
	}
}

// TestEnvTemplate_NonEmpty: the embedded env.insecure.example is present.
func TestEnvTemplate_NonEmpty(t *testing.T) {
	if len(EnvTemplate()) == 0 {
		t.Fatal("EnvTemplate() is empty — env.insecure.example not embedded")
	}
	if !strings.Contains(string(EnvTemplate()), "LOOMCYCLE_") {
		t.Errorf("env template should mention LOOMCYCLE_ vars")
	}
}
