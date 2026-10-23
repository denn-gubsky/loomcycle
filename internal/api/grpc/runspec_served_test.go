package grpc

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
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
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A run's spec on gRPC GetRun never carries whose access-control rules bound
// it: the record keeps acl_origin for the server, the wire does not.
func TestGrpcGetRun_SpecCarriesNoACLOrigin(t *testing.T) {
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "grpc-spec.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"agent": {Model: "stub-model", SystemPrompt: "hi"}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4},
	}
	httpSrv := lchttp.New(cfg, oneProvider{&answerProvider{}}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	adapter := New(Config{Store: st, CancelReg: cancel.NewRegistry(), Connector: httpSrv, Runner: httpSrv})
	ctx := context.Background()
	sess, err := st.CreateSession(ctx, "acme", "agent", "dev")
	if err != nil {
		t.Fatal(err)
	}
	rec := json.RawMessage(`{"sampling":{"temperature":0.2},"acl_origin":{"tenant_id":"acme","subject":"dev","token_def_id":"def_dev","class":"member"}}`)
	run, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_spec", TenantID: "acme", UserID: "dev", RunConfig: rec})
	if err != nil {
		t.Fatal(err)
	}
	admin := auth.WithPrincipal(ctx, auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeAdmin}})
	got, err := adapter.GetRun(admin, &loomcyclepb.GetRunRequest{RunId: run.ID})
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if spec := string(got.GetSpec()); strings.Contains(spec, "acl_origin") || strings.Contains(spec, "def_dev") || !strings.Contains(spec, `"temperature":0.2`) {
		t.Errorf("gRPC spec = %s; want the record without acl_origin", spec)
	}
}
