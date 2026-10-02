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
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// grpcFormTeam starts from an input state whose form requires chunk_id.
const grpcFormTeam = `{"entry":"form","states":[` +
	`{"state":"form","handler":{"kind":"input",` +
	`"schema":{"type":"object","required":["chunk_id"],"properties":{"chunk_id":{"type":"string"}}}}},` +
	`{"state":"a","handler":{"kind":"agent","agent":"agent"}},` +
	`{"state":"done","handler":{"kind":"terminal"}}],` +
	`"transitions":[{"from":"form","to":"a","on":"success"},{"from":"a","to":"done","on":"success"}]}`

// A form missing a field is refused over the TeamDef RPC the way every other
// op=run refusal is — is_error with a validation error_info — naming the
// field, before any member's model call; a filled-in form runs.
func TestGrpcTeamDefRun_InputFormMissingFieldRefusedAsValidation(t *testing.T) {
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "grpc-form.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"agent": {Model: "stub-model", SystemPrompt: "hi"}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	prov := &answerProvider{}
	httpSrv := lchttp.New(cfg, oneProvider{prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	httpSrv.SetTeamDefTool(&builtin.TeamDef{Store: st})
	adapter := New(Config{Store: st, CancelReg: cancel.NewRegistry(), Connector: httpSrv, Runner: httpSrv})

	ctx := context.Background()
	def, err := teamgraph.Parse([]byte(grpcFormTeam))
	if err != nil {
		t.Fatal(err)
	}
	row, err := st.TeamDefCreate(ctx, store.TeamDefRow{
		DefID: "tdf_form", Name: "form", TenantID: "acme",
		Definition: json.RawMessage(grpcFormTeam), ContentSHA256: teamgraph.Sign("form", def),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.TeamDefSetActive(ctx, "acme", "form", row.DefID, "a_test", store.TeamDefPromoter{}); err != nil {
		t.Fatal(err)
	}
	alice := auth.WithPrincipal(ctx, auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeTenant}})

	resp, err := adapter.TeamDef(alice, &loomcyclepb.SubstrateRequest{InputJson: []byte(`{"op":"run","name":"form","input":"{\"part\":\"cpu\"}"}`)})
	if err != nil {
		t.Fatalf("TeamDef run: %v", err)
	}
	if !resp.GetIsError() || !strings.Contains(string(resp.GetOutputJson()), `input field "chunk_id" is required`) {
		t.Fatalf("resp = is_error:%v %s, want the form refused naming chunk_id", resp.GetIsError(), resp.GetOutputJson())
	}
	if got := resp.GetErrorInfo().GetCategory(); got != "validation" {
		t.Errorf("error_info.category = %q, want validation", got)
	}
	if prov.last != nil {
		t.Error("the provider was called for a refused form")
	}

	resp, err = adapter.TeamDef(alice, &loomcyclepb.SubstrateRequest{InputJson: []byte(`{"op":"run","name":"form","input":"{\"chunk_id\":\"c\"}"}`)})
	if err != nil || resp.GetIsError() {
		t.Fatalf("filled-in form: err=%v resp=%s", err, resp.GetOutputJson())
	}
	if prov.last == nil {
		t.Error("a filled-in form never reached the member's model call")
	}
}
