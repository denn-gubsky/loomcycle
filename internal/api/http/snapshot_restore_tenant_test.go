package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/snapshot"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// capturePausedRun builds a snapshot envelope holding one paused run, taken on
// a store of its own — the instance the run was paused on — so a restore into
// the server's store is the cross-instance case.
func capturePausedRun(t *testing.T, identity store.RunIdentity, agent string, transcript func(st store.Store, runID string)) (runID string, envelope []byte) {
	t.Helper()
	ctx := context.Background()
	src, err := storesqlite.Open(filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	sess, err := src.CreateSession(ctx, identity.TenantID, agent, identity.UserID)
	if err != nil {
		t.Fatal(err)
	}
	run, err := src.CreateRun(ctx, sess.ID, identity)
	if err != nil {
		t.Fatal(err)
	}
	if transcript != nil {
		transcript(src, run.ID)
	}
	if err := src.SetRunPauseState(ctx, run.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}
	_, raw, err := snapshot.Capture(ctx, src, snapshot.CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return run.ID, raw
}

// A tenant principal reads its own run by id after the run was restored from a
// snapshot on another instance. The restore used to store the run under the
// shared tenant, so the tenant-scoped read folded it into a 404.
func TestRestoreSnapshot_TenantReadsItsRestoredRunByID(t *testing.T) {
	srv, _, cleanup := minimalServerWithSnapshotStore(t)
	defer cleanup()

	runID, raw := capturePausedRun(t, store.RunIdentity{AgentID: "a_acme", UserID: "alice", TenantID: "acme"}, "qa", nil)

	body, _ := json.Marshal(map[string]any{"json": json.RawMessage(raw)})
	req := httptest.NewRequest("POST", "/v1/_snapshots/inline/restore", bytes.NewReader(body))
	req.SetPathValue("id", "inline")
	rec := httptest.NewRecorder()
	srv.handleRestoreSnapshot(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var restored snapshotRestoreResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &restored)
	if restored.PausedRunsRestored != 1 {
		t.Fatalf("paused runs restored = %d, want 1", restored.PausedRunsRestored)
	}

	getAs := func(p auth.Principal) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/v1/runs/"+runID, http.NoBody)
		req.SetPathValue("run_id", runID)
		req = req.WithContext(auth.WithPrincipal(req.Context(), p))
		rec := httptest.NewRecorder()
		srv.handleGetRun(rec, req)
		return rec
	}
	if rec := getAs(auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeRunsCreate}}); rec.Code != http.StatusOK {
		t.Errorf("acme reading its restored run: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec := getAs(auth.Principal{TenantID: "globex", Subject: "alice", Scopes: []string{auth.ScopeRunsCreate}}); rec.Code != http.StatusNotFound {
		t.Errorf("globex reading acme's restored run: status = %d, want 404", rec.Code)
	}
}

// identityRecordingProvider is a scriptedProvider that records the run
// identity its calls execute under.
type identityRecordingProvider struct {
	*scriptedProvider
	mu   sync.Mutex
	seen []tools.RunIdentityValue
}

func (p *identityRecordingProvider) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	p.mu.Lock()
	p.seen = append(p.seen, tools.RunIdentity(ctx))
	p.mu.Unlock()
	return p.scriptedProvider.Call(ctx, req)
}

// resumeRestoredRun restores a one-run snapshot into a fresh server, resumes it
// to completion and returns the identities its provider calls ran under, plus
// whether the last call was allowed the operator's key.
func resumeRestoredRun(t *testing.T, identity store.RunIdentity) ([]tools.RunIdentityValue, bool) {
	t.Helper()
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"resumer": {Provider: "scripted", Model: "stub-model", SystemPrompt: "you resume work", Tools: []string{}},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	prov := &identityRecordingProvider{scriptedProvider: &scriptedProvider{
		defaultS: []providers.Event{
			{Type: providers.EventText, Text: "resumed and done"},
			{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}},
		},
	}}
	srv, _ := makeServer(t, prov, cfg)
	ctx := context.Background()

	runID, raw := capturePausedRun(t, identity, "resumer", func(st store.Store, runID string) {
		b, _ := json.Marshal([]loop.PromptSegment{
			{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "do the thing"}}},
		})
		if err := st.AppendEvent(ctx, runID, "user_input", b); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := snapshot.Restore(ctx, srv.store, raw, snapshot.RestoreOptions{}); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if n, warnings := srv.ResumePausedRuns(ctx); n != 1 {
		t.Fatalf("ResumePausedRuns re-dispatched %d, want 1 (warnings: %v)", n, warnings)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := srv.store.GetRun(ctx, runID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.Status == store.RunCompleted {
			break
		}
		if got.Status == store.RunFailed {
			t.Fatalf("resumed run failed: %s", got.ErrorMsg)
		}
		if time.Now().After(deadline) {
			t.Fatalf("resumed run did not complete (status=%q)", got.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}

	prov.mu.Lock()
	defer prov.mu.Unlock()
	if len(prov.seen) == 0 {
		t.Fatal("the provider was never called — the run was not resumed")
	}
	return append([]tools.RunIdentityValue(nil), prov.seen...), prov.lastOpKeyAllowed.Load()
}

// A run restored from a snapshot on another instance resumes under its own
// tenant: the resumed turn's calls carry that tenant, so its credentials,
// budget and memory resolve in it rather than in the shared tenant.
func TestResumePausedRuns_RestoredRunResumesUnderItsOwnTenant(t *testing.T) {
	seen, _ := resumeRestoredRun(t, store.RunIdentity{AgentID: "a_resume_acme", UserID: "alice", TenantID: "acme", Model: "stub-model"})
	for i, id := range seen {
		if id.TenantID != "acme" {
			t.Errorf("resumed call %d ran under tenant %q, want acme", i, id.TenantID)
		}
	}
}

// A restored run resumes as confined as it was paused: still denied the
// operator's provider key and still isolated.
func TestResumePausedRuns_RestoredRunKeepsItsConfinement(t *testing.T) {
	seen, opKeyAllowed := resumeRestoredRun(t, store.RunIdentity{
		AgentID: "a_resume_confined", UserID: "alice", TenantID: "acme", Model: "stub-model",
		OperatorKeyRestricted: true, Isolated: true,
	})
	if opKeyAllowed {
		t.Error("the resumed run's provider call was allowed the operator's key")
	}
	for i, id := range seen {
		if !id.OperatorKeyRestricted || !id.Isolated {
			t.Errorf("resumed call %d ran with restricted=%v isolated=%v, want both", i, id.OperatorKeyRestricted, id.Isolated)
		}
	}
}
