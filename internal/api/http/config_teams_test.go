package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
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

// configTeamsGet calls GET /v1/_config/teams and decodes its rows by name.
func configTeamsGet(t *testing.T, srv *Server) (map[string]configTeamStatus, map[string]json.RawMessage) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.handleConfigTeams(rec, httptest.NewRequest(http.MethodGet, "/v1/_config/teams", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/_config/teams = %d: %s", rec.Code, rec.Body)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	var rows []configTeamStatus
	if err := json.Unmarshal(body["teams"], &rows); err != nil {
		t.Fatal(err)
	}
	byName := map[string]configTeamStatus{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	return byName, body
}

// The boot only logs a configured team whose name an API-made team holds; the
// endpoint the Settings health panel reads reports it, beside the teams the
// configuration did write.
func TestConfigTeams_ReportsAWrittenTeamAndANameConflict(t *testing.T) {
	h := newTriggerWalkHarness(t)
	ctx := context.Background()
	plain := `{"entry":"a","states":[{"state":"a","handler":{"kind":"agent","agent":"writer"}},` +
		`{"state":"done","handler":{"kind":"terminal"}}],"transitions":[{"from":"a","to":"done","on":"success"}]}`
	res, _ := h.srv.teamDefTool.Execute(substrateAdminCtx(ctx),
		json.RawMessage(`{"op":"create","name":"taken","overlay":`+plain+`}`))
	if res.IsError {
		t.Fatalf("create the API team: %s", res.Text)
	}
	h.cfg.Teams = map[string]config.Team{
		"fine":  {Definition: json.RawMessage(plain)},
		"taken": {Definition: json.RawMessage(plain)},
	}
	if _, err := h.srv.ReconcileConfigTeams(ctx, h.cfg.Teams); err != nil {
		t.Fatal(err)
	}

	rows, body := configTeamsGet(t, h.srv)
	if r := rows["fine"]; r.Status != configTeamWritten || r.ActiveDefID == "" {
		t.Errorf("fine = %+v, want written with its def id", r)
	}
	if r := rows["taken"]; r.Status != configTeamConflict || !strings.Contains(r.Detail, "over the API") {
		t.Errorf("taken = %+v, want a conflict saying the API version holds the name", r)
	}
	if _, ok := body["reconciled_at"]; !ok {
		t.Errorf("body = %s, want reconciled_at from this replica's reconcile", body)
	}
}

// A refused team has no live version. The reason is in the memory of the
// replica that ran the reconcile; any other replica says where to look.
func TestConfigTeams_ARefusedTeamCarriesItsReasonOnlyWhereTheReconcileRan(t *testing.T) {
	teams := map[string]config.Team{"broken": {}, "retired": {}}
	rows := []store.TeamDefNameSummary{
		{Name: "retired", ActiveDefID: "td_1", ActiveRetired: true, ActiveSource: store.TeamDefSourceConfig},
		{Name: "broken", TenantID: "acme", ActiveDefID: "td_2"}, // a tenant's own: not the shared tier
	}
	ran := &configTeamsRun{Report: builtin.ConfigTeamsReport{
		Refused: []builtin.ConfigTeamProblem{{Name: "broken", Detail: "agent ./w: tool Bash is not granted"}},
	}}

	statuses := func(body map[string]any) map[string]configTeamStatus {
		out := map[string]configTeamStatus{}
		for _, r := range body["teams"].([]configTeamStatus) {
			out[r.Name] = r
		}
		return out
	}
	here := statuses(configTeamsResponse(teams, rows, ran))
	if r := here["broken"]; r.Status != configTeamNotWritten || r.Detail != "agent ./w: tool Bash is not granted" {
		t.Errorf("broken here = %+v, want not_written with the refusal", r)
	}
	if r := here["retired"]; r.Status != configTeamNotWritten {
		t.Errorf("retired = %+v, want not_written: a retired active version is not in service", r)
	}

	elsewhere := statuses(configTeamsResponse(teams, rows, nil))
	if r := elsewhere["broken"]; r.Status != configTeamNotWritten || !strings.Contains(r.Detail, "did not run the boot reconcile") {
		t.Errorf("broken elsewhere = %+v, want not_written pointing at the other replica's log", r)
	}
}
