package teamgraph

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// RFC GS P1 — the graph checks a yaml team is held to after the config load.

func loadTeamsYAML(t *testing.T, body string) *config.Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return cfg
}

// A team in the yaml is the team the API would store from the same keys: its
// content hash is the one a create of the JSON gets, so verify-or-fork across
// deployments matches the two.
func TestCheckConfigTeams_AYAMLTeamSignsAsTheSameTeamSentAsJSON(t *testing.T) {
	cfg := loadTeamsYAML(t, `
teams:
  review:
    description: one reviewer
    entry: review
    max_iterations: 4
    vars: {tone: dry}
    local:
      agents:
        reviewer: {tier: middle, system_prompt: "Review it."}
    states:
      - {state: review, handler: {kind: agent, agent: ./reviewer}}
      - {state: done, handler: {kind: terminal}}
    transitions: [{from: review, to: done, on: success}]
`)
	if err := CheckConfigTeams(cfg); err != nil {
		t.Fatalf("CheckConfigTeams: %v", err)
	}
	fromYAML, err := Parse(cfg.Teams["review"].Definition)
	if err != nil {
		t.Fatal(err)
	}
	fromJSON, err := Parse([]byte(`{"entry":"review","max_iterations":4,"vars":{"tone":"dry"},
	  "local":{"agents":{"reviewer":{"tier":"middle","system_prompt":"Review it."}}},
	  "states":[{"state":"review","handler":{"kind":"agent","agent":"./reviewer"}},{"state":"done","handler":{"kind":"terminal"}}],
	  "transitions":[{"from":"review","to":"done","on":"success"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if a, b := Sign("review", fromYAML), Sign("review", fromJSON); a != b {
		t.Errorf("content hash from yaml %s != from the JSON a create takes %s", a, b)
	}
}

// Every structural problem a create would refuse in the definition is
// reported, each at its yaml path, every team's together.
func TestCheckConfigTeams_AStructurallyBrokenTeamIsRefusedAtItsPath(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		want       []string
	}{
		{"unknown entry state", `
teams:
  t1:
    entry: nowhere
    states: [{state: a, handler: {kind: terminal}}]
    transitions: []
`, []string{"teams.t1.entry", "nowhere"}},
		{"a misspelt key", `
teams:
  t1:
    entry: a
    hoks: {}
    states: [{state: a, handler: {kind: terminal}}]
    transitions: []
`, []string{"teams.t1.hoks"}},
		{"a key in the wrong case", `
teams:
  t1:
    Entry: a
    states: [{state: a, handler: {kind: terminal}}]
    transitions: []
`, []string{"teams.t1.Entry"}},
		{"an undeclared team-local agent", `
teams:
  t1:
    entry: a
    states: [{state: a, handler: {kind: agent, agent: ./ghost}}, {state: b, handler: {kind: terminal}}]
    transitions: [{from: a, to: b, on: success}]
`, []string{"teams.t1.states[0]", "ghost"}},
		{"two broken teams", `
teams:
  t1:
    entry: nowhere
    states: [{state: a, handler: {kind: terminal}}]
    transitions: []
  t2:
    entry: a
    hoks: {}
    states: [{state: a, handler: {kind: terminal}}]
    transitions: []
`, []string{"teams.t1", "teams.t2.hoks"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckConfigTeams(loadTeamsYAML(t, tc.yaml))
			if err == nil {
				t.Fatalf("a broken team passed")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q should mention %q", err, w)
				}
			}
		})
	}
}

// Members outside the team may be created over the API later, so a name the
// config does not hold is not a refusal.
func TestCheckConfigTeams_AMemberTheConfigDoesNotHoldPasses(t *testing.T) {
	cfg := loadTeamsYAML(t, `
teams:
  t1:
    entry: a
    states: [{state: a, handler: {kind: agent, agent: made-later}}, {state: b, handler: {kind: terminal}}]
    transitions: [{from: a, to: b, on: success}]
`)
	if err := CheckConfigTeams(cfg); err != nil {
		t.Errorf("CheckConfigTeams: %v", err)
	}
}
