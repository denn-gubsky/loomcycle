package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// RFC GS P4: the team-examples bundle carries its starter teams as
// configuration. Stacked as the docs say to select it, its teams pass the
// graph checks every load runs, and the boot's reconcile writes both into the
// shared tier — none refused by a create gate, none in conflict.
func TestEmbedded_TeamExamplesTeamsLoadAndAreWrittenAtBoot(t *testing.T) {
	t.Setenv("LOOMCYCLE_SKILLS_ROOT", "")
	cfg, err := config.LoadLayers(layersFor(t, "base", "agent-teams", "team-examples")...)
	if err != nil {
		t.Fatalf("load base + agent-teams + team-examples: %v", err)
	}
	if err := teamgraph.CheckConfigTeams(cfg); err != nil {
		t.Fatalf("the bundle's teams fail the graph checks: %v", err)
	}
	names := make([]string, 0, len(cfg.Teams))
	for n := range cfg.Teams {
		names = append(names, n)
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"marketing", "sdlc"}) {
		t.Fatalf("configured teams = %v, want marketing and sdlc", names)
	}
	// Every member of every team is an agent the stack declares (RFC GS D9:
	// global, so the teams run in every tenant).
	for name, team := range cfg.Teams {
		def, err := teamgraph.Parse(team.Definition)
		if err != nil {
			t.Fatal(err)
		}
		for _, st := range def.States {
			if a := st.Handler.Agent; a != "" {
				if _, ok := cfg.Agents[a]; !ok {
					t.Errorf("team %s state %s runs agent %q, which the stack does not declare", name, st.ID, a)
				}
			}
		}
	}

	st, err := sqlite.Open(filepath.Join(t.TempDir(), "teams.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	td := &builtin.TeamDef{Store: st, MaxDefinitionBytes: builtin.TeamDefMaxDefinitionBytes, MaxDescriptionBytes: builtin.DefMaxDescriptionBytes}
	// The operator plane's grants, which the server's substrate admin ctx
	// stamps for the real boot (Server.ReconcileConfigTeams).
	ctx := tools.UpdateRunPolicy(context.Background(), func(rp *tools.RunPolicy) {
		rp.AgentDef = tools.AgentDefPolicyValue{Scopes: []string{"any"}}
		rp.Tools = []string{"*"}
	})
	rep, err := td.ReconcileConfigTeams(ctx, cfg.Teams)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !reflect.DeepEqual(rep.Written, []string{"marketing", "sdlc"}) || len(rep.Refused)+len(rep.Conflicts) != 0 {
		t.Errorf("reconcile = %+v, want both teams written, none refused or in conflict", rep)
	}
}

// The two team bundles exist twice — the readable source tree under bundles/
// and the flat file the binary go:embeds. Only the embedded copy ships, so the
// teams moved into config here must be in both, or the source tree goes on
// describing teams as JSON in a skill.
func TestTeamBundles_SourceMatchesEmbedded(t *testing.T) {
	for _, name := range []string{"team-examples", "agent-teams"} {
		src, err := os.ReadFile(filepath.Join("..", "..", "bundles", name, "loomcycle.yaml"))
		if err != nil {
			t.Fatalf("read bundle source %s: %v", name, err)
		}
		embeddedCopy, err := os.ReadFile(filepath.Join("..", "..", "internal", "embedded", "bundles", name+".yaml"))
		if err != nil {
			t.Fatalf("read embedded bundle %s: %v", name, err)
		}
		if !bytes.Equal(src, embeddedCopy) {
			t.Errorf("bundles/%s/loomcycle.yaml and internal/embedded/bundles/%s.yaml differ — only the embedded copy ships, so copy the source over it", name, name)
		}
	}
}
