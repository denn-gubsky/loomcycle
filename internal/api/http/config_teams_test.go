package http

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// RFC GS P2: the boot's reconcile goes through the server, which gives it the
// operator plane's grants. A team with agents of its own needs them — its
// agents pass AgentDef create's gates — and a bare ctx has none.
func TestReconcileConfigTeams_ATeamWithItsOwnAgentsIsWrittenUnderTheOperatorsGrants(t *testing.T) {
	h := newTriggerWalkHarness(t)
	td := &builtin.TeamDef{
		Store:  h.st,
		Agents: &builtin.AgentDef{Store: h.st, Cfg: h.cfg, MaxDefinitionBytes: 131072, MaxDescriptionBytes: 8192, MaxCodeBytes: 262144},
	}
	h.srv.SetTeamDefTool(td)
	teams := map[string]config.Team{"own-agents": {Definition: json.RawMessage(localAgentTeam)}}

	bare, err := td.ReconcileConfigTeams(context.Background(), teams)
	if err != nil || len(bare.Refused) != 1 {
		t.Fatalf("reconcile with no grants = %+v (%v), want the team refused (the premise of this test)", bare, err)
	}

	rep, err := h.srv.ReconcileConfigTeams(context.Background(), teams)
	if err != nil {
		t.Fatalf("ReconcileConfigTeams: %v", err)
	}
	if !reflect.DeepEqual(rep.Written, []string{"own-agents"}) {
		t.Fatalf("report = %+v, want own-agents written", rep)
	}
	row, err := h.st.TeamDefGetActive(context.Background(), "", "own-agents")
	if err != nil || !row.BootstrappedFromStatic || !row.OperatorAuthored {
		t.Errorf("shared active = %+v (%v), want a config-written operator version", row, err)
	}
}

// A configured team whose entry is a channel Starter is armed by the
// reconcile — its active pointer is the arming — and disarmed when it leaves
// the yaml.
func TestReconcileConfigTeams_AConfiguredStarterTeamIsArmedAndDisarmedWithTheYAML(t *testing.T) {
	srv, cleanup := channelFanFixture(t)
	defer cleanup()
	srv.SetTeamDefTool(&builtin.TeamDef{Store: srv.store})
	ctx := context.Background()

	rep, err := srv.ReconcileConfigTeams(ctx, map[string]config.Team{"waves": {Definition: json.RawMessage(subStarterGraph)}})
	if err != nil || !reflect.DeepEqual(rep.Written, []string{"waves"}) {
		t.Fatalf("reconcile = %+v (%v), want waves written", rep, err)
	}
	subs, err := srv.listTeamSubscriptions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := subNames(subs); !reflect.DeepEqual(got, []string{"waves"}) {
		t.Fatalf("armed = %v, want the configured waves", got)
	}
	if p := subs[0].Promoter; p == nil || p.OperatorKeyRestricted || p.Isolated || p.ACLOwner != nil {
		t.Errorf("promoter = %+v, want unrestricted with no access-control owner", p)
	}

	if _, err := srv.ReconcileConfigTeams(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if subs, _ := srv.listTeamSubscriptions(ctx); len(subs) != 0 {
		t.Errorf("armed after the team left the yaml = %v, want none", subNames(subs))
	}
}
