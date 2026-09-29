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
	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/runstate"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// A team walk started over the TeamDef RPC runs as the CALLER, not the
// synthetic grpc-admin user — against the REAL HTTP server as the connector,
// since that is where the walk's run and its members are filed.

// grpcWaveTeam is one starter state: its member carries the walk id, which is
// what a walk-filtered run-state stream matches on.
const grpcWaveTeam = `{"entry":"wave","states":[` +
	`{"state":"wave","handler":{"kind":"starter","source":{"channel":"in"},` +
	`"fanout":{"agent":"agent","per":"message","max":1},"sink":{"channel":"out"}}},` +
	`{"state":"done","handler":{"kind":"terminal"}}],` +
	`"transitions":[{"from":"wave","to":"done","on":"success"}],` +
	`"channels":{"publish":["out"],"subscribe":["in"]}}`

func TestGrpcTeamDef_RunWalksAsTheCaller(t *testing.T) {
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "grpc-walk.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:   map[string]config.AgentDef{"agent": {Model: "stub-model", SystemPrompt: "hi"}},
		Channels: map[string]config.Channel{
			"in":  {Scope: "tenant", Semantic: "queue", MaxMessages: 100},
			"out": {Scope: "tenant", Semantic: "queue", MaxMessages: 100},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	httpSrv := lchttp.New(cfg, oneProvider{&answerProvider{}}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	bus := runstate.NewBus()
	httpSrv.SetRunStateBus(bus)
	cbus := channels.NewBus()
	pub := &channels.StorePublisher{Store: st, Bus: cbus, Scheduler: channels.NewScheduler(cbus, 100)}
	httpSrv.SetSystemPublisher(pub)
	httpSrv.SetChannelBus(cbus)
	httpSrv.SetTeamDefTool(&builtin.TeamDef{Store: st})
	adapter := New(Config{Store: st, CancelReg: cancel.NewRegistry(), Connector: httpSrv, Runner: httpSrv})

	ctx := context.Background()
	def, err := teamgraph.Parse([]byte(grpcWaveTeam))
	if err != nil {
		t.Fatal(err)
	}
	row, err := st.TeamDefCreate(ctx, store.TeamDefRow{
		DefID: "tdf_wave", Name: "wave", TenantID: "acme",
		Definition: json.RawMessage(grpcWaveTeam), ContentSHA256: teamgraph.Sign("wave", def),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.TeamDefSetActive(ctx, "acme", "wave", row.DefID, "a_test", store.TeamDefPromoter{}); err != nil {
		t.Fatal(err)
	}
	if _, err := pub.PublishNow(ctx, "in", "acme", store.MemoryScopeTenant, "",
		json.RawMessage(`{"task":"write"}`), "_system", 0, 0); err != nil {
		t.Fatal(err)
	}

	// Alice watches her own run states.
	sctx, stop := context.WithCancel(t.Context())
	defer stop()
	events := make(chan connector.RunStateEvent, 64)
	go func() {
		_ = httpSrv.StreamUserRunStates(sctx, connector.StreamUserRunStatesRequest{UserID: "alice"},
			func(evt connector.RunStateEvent) error {
				events <- evt
				return nil
			})
	}()
	for deadline := time.Now().Add(2 * time.Second); bus.ActiveSubscriberCount() == 0; {
		if time.Now().After(deadline) {
			t.Fatal("stream never subscribed")
		}
		time.Sleep(2 * time.Millisecond)
	}

	alice := auth.WithPrincipal(ctx, auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeTenant}})
	resp, err := adapter.TeamDef(alice, &loomcyclepb.SubstrateRequest{InputJson: []byte(`{"op":"run","name":"wave","input":"go"}`)})
	if err != nil || resp.GetIsError() {
		t.Fatalf("TeamDef run: err=%v resp=%s", err, resp.GetOutputJson())
	}
	var out struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(resp.GetOutputJson(), &out); err != nil || out.RunID == "" {
		t.Fatalf("no run_id in %s (%v)", resp.GetOutputJson(), err)
	}

	walk, err := st.GetRun(ctx, out.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if walk.UserID != "alice" || walk.TenantID != "acme" {
		t.Errorf("walk run filed under (tenant %q, user %q), want (acme, alice)", walk.TenantID, walk.UserID)
	}
	runs, err := st.ListActiveRunsByUser(ctx, "", "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	memberID := ""
	for _, r := range runs {
		if r.ParentContext != nil && r.ParentContext.WalkID == out.RunID {
			memberID = r.ID
		}
	}
	if memberID == "" {
		t.Fatalf("no member run of walk %s under alice — the walk's agents ran as someone else", out.RunID)
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case evt := <-events:
			if evt.ParentContext != nil && evt.ParentContext.WalkID == out.RunID && evt.RunID == memberID {
				return
			}
		case <-deadline:
			t.Fatalf("alice's run-state stream never saw walk %s's member %s", out.RunID, memberID)
		}
	}
}
