package http

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func residentScopeServer(t *testing.T) (*Server, *scopeRecorder) {
	t.Helper()
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"child": {Model: "stub-model", SystemPrompt: "be brief"}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8},
	}
	cfg.Env.AuthToken = ""
	prov := &scopeRecorder{Provider: &stubProvider{events: []providers.Event{
		{Type: providers.EventText, Text: "ok"},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}}}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "resident-scope.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(8, 8, 100*time.Millisecond), st)
	srv.SetSteerRegistry(steer.NewRegistry(0))
	return srv, prov
}

// A resident child alice's run opened keeps running across her run's turns.
// Once another user has sent her run a turn, what her run sends the child
// may carry that person's instruction, so the child takes it without alice's
// own credentials too — as does a child opened from then on.
func TestResidentChild_ATurnFromAWithheldParentWithholdsTheChild(t *testing.T) {
	for _, withheld := range []bool{false, true} {
		srv, prov := residentScopeServer(t)
		latch := new(atomic.Bool)
		parent := tools.WithRunIdentity(context.Background(), tools.RunIdentityValue{
			AgentID: "a_parent", TenantID: "acme", UserID: "alice", UserScopeWithheld: latch,
		})
		childID, _, _, err := srv.openResidentChild(parent, "child", "start", "", 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		// Another user's turn reached the parent after the child opened.
		latch.Store(withheld)
		if _, _, err := srv.sendResidentChild(parent, childID, "the next instruction", 0); err != nil {
			t.Fatalf("withheld=%t: send: %v", withheld, err)
		}
		calls := prov.calls()
		if len(calls) != 2 {
			t.Fatalf("withheld=%t: %d model calls, want the open's and the send's", withheld, len(calls))
		}
		if calls[0].noUserScope {
			t.Errorf("withheld=%t: the child's first turn, before the parent was withheld, lost the scope", withheld)
		}
		if calls[1].noUserScope != withheld {
			t.Errorf("withheld=%t: the child's turn after the parent's send ran with no-user-scope=%t", withheld, calls[1].noUserScope)
		}
		if row, _ := srv.store.GetRun(context.Background(), childID); row.CredentialsNoUserScope != withheld {
			t.Errorf("withheld=%t: the child's row credentials_no_user_scope = %t", withheld, row.CredentialsNoUserScope)
		}
		if _, _, _, err := srv.openResidentChild(parent, "child", "a second child", "", 0, 0); err != nil {
			t.Fatal(err)
		}
		if c := prov.calls(); c[len(c)-1].noUserScope != withheld {
			t.Errorf("withheld=%t: a child opened afterwards ran with no-user-scope=%t", withheld, c[len(c)-1].noUserScope)
		}
	}
}
