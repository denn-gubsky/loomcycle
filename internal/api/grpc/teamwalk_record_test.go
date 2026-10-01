package grpc

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// grpcSoloTeam is one agent state and a terminal.
const grpcSoloTeam = `{"entry":"a","states":[` +
	`{"state":"a","handler":{"kind":"agent","agent":"agent"}},` +
	`{"state":"done","handler":{"kind":"terminal"}}],` +
	`"transitions":[{"from":"a","to":"done","on":"success"}]}`

// GetRun of a walk's run carries the team version the walk ran in its spec,
// against the REAL HTTP server as the connector, which is what records it.
func TestGrpcGetRun_AWalkRunCarriesTheTeamVersionItRan(t *testing.T) {
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "grpc-walk-record.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"agent": {Model: "stub-model", SystemPrompt: "hi"}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	httpSrv := lchttp.New(cfg, oneProvider{&answerProvider{}}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	httpSrv.SetTeamDefTool(&builtin.TeamDef{Store: st})
	adapter := New(Config{Store: st, CancelReg: cancel.NewRegistry(), Connector: httpSrv, Runner: httpSrv})

	ctx := context.Background()
	def, err := teamgraph.Parse([]byte(grpcSoloTeam))
	if err != nil {
		t.Fatal(err)
	}
	row, err := st.TeamDefCreate(ctx, store.TeamDefRow{
		DefID: "tdf_solo", Name: "solo", TenantID: "acme",
		Definition: json.RawMessage(grpcSoloTeam), ContentSHA256: teamgraph.Sign("solo", def),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.TeamDefSetActive(ctx, "acme", "solo", row.DefID, "a_test", store.TeamDefPromoter{}); err != nil {
		t.Fatal(err)
	}

	alice := auth.WithPrincipal(ctx, auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeTenant}})
	resp, err := adapter.TeamDef(alice, &loomcyclepb.SubstrateRequest{InputJson: []byte(`{"op":"run","name":"solo","input":"go"}`)})
	if err != nil || resp.GetIsError() {
		t.Fatalf("TeamDef run: err=%v resp=%s", err, resp.GetOutputJson())
	}
	var out struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(resp.GetOutputJson(), &out); err != nil || out.RunID == "" {
		t.Fatalf("no run_id in %s (%v)", resp.GetOutputJson(), err)
	}

	got, err := adapter.GetRun(alice, &loomcyclepb.GetRunRequest{RunId: out.RunID})
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	var spec struct {
		Team *struct {
			Name          string `json:"name"`
			DefID         string `json:"def_id"`
			Version       int    `json:"version"`
			ContentSHA256 string `json:"content_sha256"`
			ResolvedBy    string `json:"resolved_by"`
		} `json:"team"`
	}
	if err := json.Unmarshal(got.GetSpec(), &spec); err != nil || spec.Team == nil {
		t.Fatalf("GetRun spec = %s (%v), want a team record", got.GetSpec(), err)
	}
	if spec.Team.Name != "solo" || spec.Team.DefID != row.DefID || spec.Team.Version != row.Version ||
		spec.Team.ContentSHA256 != row.ContentSHA256 || spec.Team.ResolvedBy != "name" {
		t.Errorf("spec.team = %+v, want solo v%d (%s) resolved by name", *spec.Team, row.Version, row.DefID)
	}
}
