package http

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// draftReadHookStore runs a hook, once, when a draft is next read: the moment
// between a start reading the run's row and reading the draft it starts.
type draftReadHookStore struct {
	store.Store
	mu   sync.Mutex
	hook func()
}

func (s *draftReadHookStore) GetRunDraft(ctx context.Context, runID string) (json.RawMessage, error) {
	s.mu.Lock()
	h := s.hook
	s.hook = nil
	s.mu.Unlock()
	if h != nil {
		h()
	}
	return s.Store.GetRunDraft(ctx, runID)
}

// A start decides the scope from the row it starts, not from an earlier read
// of it. Here alice's start reads the run row, then — before it reads the
// draft — bob edits the draft (marking the row, then writing his prompt). The
// start then reads bob's prompt and its compare-and-set on that draft
// succeeds, so the run that starts is running bob's words: it must run
// without alice's credentials, though the row read at the top said otherwise.
func TestConfiguredRun_AnEditRacingTheStartStillWithholds(t *testing.T) {
	st, err := storesqlite.Open(t.TempDir() + "/race.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	hooked := &draftReadHookStore{Store: st}
	cfg := resumeTreeConfig("scripted", "", "")
	cfg.Env.AuthToken = ouLegacySecret
	prov := &scopeRecorder{Provider: answeringProvider()}
	srv := New(cfg, &stubResolver{p: prov}, []tools.Tool{}, concurrency.New(8, 8, time.Second), hooked)

	alice := auth.WithPrincipal(context.Background(), ouAlice)
	draft, err := srv.CreateConfiguredRun(alice, connector.ConfiguredRunRequest{
		SpawnRunRequest: connector.SpawnRunRequest{Agent: "resumer", Segments: []loop.PromptSegment{
			{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "alice's prompt"}}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var editErr error
	hooked.mu.Lock()
	hooked.hook = func() {
		_, editErr = srv.UpdateConfiguredRun(auth.WithPrincipal(context.Background(), ouBob), draft.RunID, json.RawMessage(`{"prompt":"bob's prompt"}`))
	}
	hooked.mu.Unlock()

	if _, err := srv.StartConfiguredRun(alice, draft.RunID, connector.RunSecrets{}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if editErr != nil {
		t.Fatalf("bob's edit inside the start: %v", editErr)
	}
	calls := prov.calls()
	if len(calls) != 1 || !calls[0].noUserScope {
		t.Errorf("the started run (bob's prompt) ran with %+v, want alice's scope withheld", calls)
	}
}
