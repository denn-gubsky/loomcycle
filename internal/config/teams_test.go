package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// RFC GS P1 — the top-level `teams:` map. What the load decides is tested
// here; the graph checks, which sit above config, are teamgraph's
// (configteams_test.go there), with the content hash of a yaml team.

const marketingTeamYAML = `
teams:
  marketing:
    description: A writer drafts, an editor reviews.
    entry: draft
    max_iterations: 5
    local:
      agents:
        writer: {tier: middle, system_prompt: "Write the draft.", tools: [Read]}
    states:
      - state: draft
        handler: {kind: agent, agent: ./writer}
      - state: edit
        handler: {kind: agent, agent: marketing/editor}
      - state: done
        handler: {kind: terminal}
    transitions:
      - {from: draft, to: edit, on: success}
      - {from: edit, to: draft, on: "pushback:revise"}
      - {from: edit, to: done, on: success}
`

// The same team as the JSON TeamDef create would be sent.
const marketingTeamJSON = `{
  "entry": "draft", "max_iterations": 5,
  "local": {"agents": {"writer": {"tier": "middle", "system_prompt": "Write the draft.", "tools": ["Read"]}}},
  "states": [
    {"state": "draft", "handler": {"kind": "agent", "agent": "./writer"}},
    {"state": "edit", "handler": {"kind": "agent", "agent": "marketing/editor"}},
    {"state": "done", "handler": {"kind": "terminal"}}
  ],
  "transitions": [
    {"from": "draft", "to": "edit", "on": "success"},
    {"from": "edit", "to": "draft", "on": "pushback:revise"},
    {"from": "edit", "to": "done", "on": "success"}
  ]
}`

// graphOf decodes a team's graph for comparison: two teams with the same graph
// decode equal whatever their key order or source.
func graphOf(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("team graph is not JSON: %v\n%s", err, raw)
	}
	return m
}

// The entry's graph keys are the JSON a create would be sent; description is
// the entry's, not the graph's.
func TestTeams_AYAMLTeamIsTheJSONACreateTakes(t *testing.T) {
	cfg, err := loadYAMLString(t, marketingTeamYAML)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	team, ok := cfg.Teams["marketing"]
	if !ok {
		t.Fatalf("teams = %v, want marketing", cfg.Teams)
	}
	if team.Description != "A writer drafts, an editor reviews." {
		t.Errorf("description = %q", team.Description)
	}
	if got, want := graphOf(t, team.Definition), graphOf(t, []byte(marketingTeamJSON)); !reflect.DeepEqual(got, want) {
		t.Errorf("graph from yaml = %v\nwant the JSON a create takes = %v", got, want)
	}
}

func TestTeams_ADefinitionFileOfJSONOrYAMLIsReadBesideTheConfig(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, dir, "marketing.json", marketingTeamJSON)
	inline, err := loadYAMLString(t, marketingTeamYAML)
	if err != nil {
		t.Fatalf("load inline: %v", err)
	}
	// The graph keys of the inline entry, as a YAML file of their own.
	var graph []string
	for _, line := range strings.Split(marketingTeamYAML, "\n")[3:] {
		if strings.HasPrefix(line, "    description:") {
			continue
		}
		graph = append(graph, strings.TrimPrefix(line, "    "))
	}
	if err := os.MkdirAll(filepath.Join(dir, "teams"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeYAML(t, dir, "teams/marketing.yaml", strings.Join(graph, "\n"))
	want := graphOf(t, inline.Teams["marketing"].Definition)

	for _, file := range []string{"marketing.json", "teams/marketing.yaml"} {
		cfgPath := writeYAML(t, dir, "c.yaml", "teams:\n  marketing:\n    description: from a file\n    definition_file: "+file+"\n")
		cfg, err := Load(cfgPath)
		if err != nil {
			t.Fatalf("load with definition_file %s: %v", file, err)
		}
		team := cfg.Teams["marketing"]
		if got := graphOf(t, team.Definition); !reflect.DeepEqual(got, want) {
			t.Errorf("definition_file %s: graph %v, want the inline team's %v", file, got, want)
		}
		if team.Description != "from a file" || team.DefinitionFile != file {
			t.Errorf("definition_file %s: description %q, file %q", file, team.Description, team.DefinitionFile)
		}
	}
}

func TestTeams_ADefinitionFileBesideAnInlineGraphIsRefused(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, dir, "marketing.json", marketingTeamJSON)
	cfgPath := writeYAML(t, dir, "c.yaml", "teams:\n  marketing:\n    definition_file: marketing.json\n    entry: draft\n")
	_, err := Load(cfgPath)
	if err == nil || !strings.Contains(err.Error(), "teams.marketing") || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("load = %v, want teams.marketing refused for definition_file beside an inline graph", err)
	}
}

// What the load refuses itself stops it, each at the team's yaml path, and
// every team's problems are reported together.
func TestTeams_ABadNameOrNoGraphStopsTheLoadAtItsPath(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		want       []string
	}{
		{"a name outside the grammar", `
teams:
  "a.b":
    entry: a
    states: [{state: a, handler: {kind: terminal}}]
    transitions: []
`, []string{"teams.a.b", "invalid character"}},
		{"no graph", `
teams:
  t1:
    description: nothing else
`, []string{"teams.t1", "no graph"}},
		{"two broken teams", `
teams:
  t1: {}
  t2:
    description: nothing else
`, []string{"teams.t1", "teams.t2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadYAMLString(t, tc.yaml)
			if err == nil {
				t.Fatalf("load accepted a broken team")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("load error %q should mention %q", err, w)
				}
			}
		})
	}
}

func TestTeams_ADefinitionOverTheSizeCapStopsTheLoad(t *testing.T) {
	big := strings.Repeat("x", 1<<20)
	_, err := loadYAMLString(t, `
teams:
  t1:
    entry: a
    states: [{state: a, handler: {kind: agent, agent: helper, system_prompt: "`+big+`"}}]
    transitions: []
`)
	if err == nil || !strings.Contains(err.Error(), "exceeds max") {
		t.Fatalf("load = %v, want refused over the size cap", err)
	}
}

// A later layer merges into a team key by key, and replaces a list whole
// (RFC GS D7): an overlay can retarget one of the team's own agents without
// restating the graph, and an overlay `states` is the team's states.
func TestTeams_ALaterLayerMergesKeysAndReplacesLists(t *testing.T) {
	dir := t.TempDir()
	base := writeYAML(t, dir, "base.yaml", marketingTeamYAML)
	tier := writeYAML(t, dir, "tier.yaml", `
teams:
  marketing:
    max_iterations: 7
    local:
      agents:
        writer: {tier: high}
`)
	cfg, err := Load(base, tier)
	if err != nil {
		t.Fatalf("load base + overlay: %v", err)
	}
	g := graphOf(t, cfg.Teams["marketing"].Definition)
	if g["max_iterations"] != 7.0 || len(g["states"].([]any)) != 3 || g["entry"] != "draft" {
		t.Errorf("merged team = %v; want max_iterations 7, the base's 3 states, entry draft", g)
	}
	writer := g["local"].(map[string]any)["agents"].(map[string]any)["writer"].(map[string]any)
	if writer["tier"] != "high" || writer["system_prompt"] != "Write the draft." {
		t.Errorf("local writer = %v, want the overlay's tier beside the base's prompt", writer)
	}
	if cfg.Teams["marketing"].Description != "A writer drafts, an editor reviews." {
		t.Errorf("description = %q, want the base's", cfg.Teams["marketing"].Description)
	}

	states := writeYAML(t, dir, "states.yaml", `
teams:
  marketing:
    entry: only
    transitions: []
    states:
      - state: only
        handler: {kind: terminal}
`)
	cfg, err = Load(base, states)
	if err != nil {
		t.Fatalf("load base + states overlay: %v", err)
	}
	if states := graphOf(t, cfg.Teams["marketing"].Definition)["states"].([]any); len(states) != 1 || states[0].(map[string]any)["state"] != "only" {
		t.Errorf("states after an overlay list = %v, want only the overlay's", states)
	}
}

// The reload diff marshals the config; a team writes back as the team it
// loaded, which loads again unchanged.
func TestTeams_ATeamMarshalsBackToTheTeamItLoaded(t *testing.T) {
	cfg, err := loadYAMLString(t, marketingTeamYAML)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	secs, err := sectionBytes(cfg)
	if err != nil {
		t.Fatalf("sectionBytes: %v", err)
	}
	again, err := loadYAMLString(t, "teams:\n"+indent(secs["teams"], "  "))
	if err != nil {
		t.Fatalf("load the marshalled teams: %v\n%s", err, secs["teams"])
	}
	a := graphOf(t, cfg.Teams["marketing"].Definition)
	b := graphOf(t, again.Teams["marketing"].Definition)
	if !reflect.DeepEqual(a, b) || again.Teams["marketing"].Description != cfg.Teams["marketing"].Description {
		t.Errorf("marshalled team loads as a different team:\n%s", secs["teams"])
	}
}

func indent(s, pad string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = pad + l
	}
	return strings.Join(lines, "\n") + "\n"
}
