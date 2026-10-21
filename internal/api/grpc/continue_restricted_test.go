package grpc

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

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

// principalContinueStream drives Continue as a given caller, without the auth
// interceptor.
type principalContinueStream struct {
	googlegrpc.ServerStream
	ctx  context.Context
	sent []*loomcyclepb.Event
}

func (s *principalContinueStream) Context() context.Context { return s.ctx }
func (s *principalContinueStream) Send(e *loomcyclepb.Event) error {
	s.sent = append(s.sent, e)
	return nil
}

const grpcOneStateTeam = `{"entry":"work","states":[` +
	`{"state":"work","handler":{"kind":"agent","agent":"secretagent"}},` +
	`{"state":"done","handler":{"kind":"terminal"}}],` +
	`"transitions":[{"from":"work","to":"done","on":"success"}]}`

// Continue on a session of a retired team version, by a token limited to
// agents other than the session's, says neither the session's agent nor its
// team. gRPC answers with the runner error's text, which used to be the
// retirement refusal: "unknown agent: secretagent: … team "squad" …".
func TestGrpcContinue_ARetiredTeamSessionTellsARestrictedTokenNothing(t *testing.T) {
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "grpc-retired.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"secretagent": {Model: "stub-model", SystemPrompt: "hi"}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4},
	}
	httpSrv := lchttp.New(cfg, oneProvider{&answerProvider{}}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	httpSrv.SetTeamDefTool(&builtin.TeamDef{Store: st})
	adapter := New(Config{Store: st, CancelReg: cancel.NewRegistry(), Connector: httpSrv, Runner: httpSrv})

	ctx := context.Background()
	def, err := teamgraph.Parse([]byte(grpcOneStateTeam))
	if err != nil {
		t.Fatal(err)
	}
	row, err := st.TeamDefCreate(ctx, store.TeamDefRow{
		DefID: "tdf_squad", Name: "squad", TenantID: "acme",
		Definition: json.RawMessage(grpcOneStateTeam), ContentSHA256: teamgraph.Sign("squad", def),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.TeamDefSetActive(ctx, "acme", "squad", row.DefID, "a_test", store.TeamDefPromoter{}); err != nil {
		t.Fatal(err)
	}
	alice := auth.WithPrincipal(ctx, auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeTenant}})
	resp, err := adapter.TeamDef(alice, &loomcyclepb.SubstrateRequest{InputJson: []byte(`{"op":"run","name":"squad","input":"go"}`)})
	if err != nil || resp.GetIsError() {
		t.Fatalf("TeamDef run: err=%v resp=%s", err, resp.GetOutputJson())
	}
	runs, err := st.ListActiveRunsByUser(ctx, "", "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	sessionID := ""
	for _, r := range runs {
		if r.Agent == "secretagent" {
			sessionID = r.SessionID
		}
	}
	if sessionID == "" {
		t.Fatalf("the walk left no run of secretagent: %+v", runs)
	}
	if err := st.TeamDefSetRetired(ctx, row.DefID, true); err != nil {
		t.Fatal(err)
	}

	restricted := auth.WithPrincipal(ctx, auth.Principal{
		TenantID: "acme", Subject: "alice", TokenDefID: "def_restricted",
		Scopes:       []string{auth.ScopeRunsCreate, auth.ScopeRunsRead},
		Restrictions: auth.Restrictions{Agents: []string{"helper"}},
	})
	stream := &principalContinueStream{ctx: restricted}
	err = adapter.Continue(&loomcyclepb.ContinueRequest{
		SessionId: sessionID,
		Segments: []*loomcyclepb.PromptSegment{{Role: "user", Content: []*loomcyclepb.PromptContentBlock{
			{Type: "trusted-text", Text: "go"},
		}}},
	}, stream)
	msg := status.Convert(err).Message()
	if status.Code(err) != codes.PermissionDenied || strings.Contains(msg, "secretagent") || strings.Contains(msg, "squad") {
		t.Errorf("Continue = %v, want PermissionDenied naming neither the agent nor the team", err)
	}
}
