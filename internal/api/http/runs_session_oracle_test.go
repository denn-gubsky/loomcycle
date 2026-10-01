package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// runsWithSession POSTs /v1/runs continuing sessionID as principal p, straight
// to the handler (the route's scope gate is not what is under test).
func runsWithSession(srv *Server, p auth.Principal, sessionID string) *httptest.ResponseRecorder {
	body := `{"agent":"default","session_id":"` + sessionID + `","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(auth.WithPrincipal(r.Context(), p))
	rr := httptest.NewRecorder()
	srv.handleRuns(rr, r)
	return rr
}

// A session the caller may not continue must read exactly like one that does
// not exist. The owner's run being in flight is the case that told them apart:
// the unowned session got past the existence check to the per-session lock and
// answered 409 session_busy, where an unknown id answered 404.
func TestHandleRuns_UnownedSessionReadsLikeMissingOne(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"default": {Model: "stub-model"},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	provider := &stubProvider{events: []providers.Event{
		{Type: providers.EventText, Text: "ok"},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}}
	srv := New(cfg, &stubResolver{p: provider}, nil, concurrency.New(4, 4, time.Second), st)

	sess, err := st.CreateSession(context.Background(), "acme", "default", "alice")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	const missingID = "s_does_not_exist_0001"

	probers := []struct {
		name string
		p    auth.Principal
	}{
		{"isolated member, same tenant", auth.Principal{TenantID: "acme", Subject: "mallory", Scopes: []string{auth.ScopeUser}}},
		{"tenant operator, other tenant", auth.Principal{TenantID: "evil", Subject: "op", Scopes: []string{auth.ScopeTenant}}},
	}
	// "owner idle" is the baseline a raw read already got right; "owner run in
	// flight" is the oracle.
	for _, ownerBusy := range []bool{false, true} {
		for _, pr := range probers {
			name := pr.name
			if ownerBusy {
				name += ", owner run in flight"
			}
			t.Run(name, func(t *testing.T) {
				if ownerBusy {
					release, ok := srv.trySessionLock(sess.ID)
					if !ok {
						t.Fatal("could not take the owner's session lock")
					}
					defer release()
				}
				unowned := runsWithSession(srv, pr.p, sess.ID)
				missing := runsWithSession(srv, pr.p, missingID)

				unownedBody := strings.ReplaceAll(unowned.Body.String(), sess.ID, "<ID>")
				missingBody := strings.ReplaceAll(missing.Body.String(), missingID, "<ID>")
				if unowned.Code != missing.Code || unownedBody != missingBody ||
					unowned.Header().Get("Content-Type") != missing.Header().Get("Content-Type") {
					t.Fatalf("unowned session is distinguishable from a missing one:\n unowned: %d %q %q\n missing: %d %q %q",
						unowned.Code, unowned.Header().Get("Content-Type"), unownedBody,
						missing.Code, missing.Header().Get("Content-Type"), missingBody)
				}
				if unowned.Code != http.StatusNotFound {
					t.Fatalf("status = %d, want 404; body=%s", unowned.Code, unowned.Body.String())
				}
			})
		}
	}

	// The owner still continues its own session.
	owner := runsWithSession(srv, auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeUser}}, sess.ID)
	if owner.Code != http.StatusOK {
		t.Fatalf("owner continuation: status = %d, want 200; body=%s", owner.Code, owner.Body.String())
	}
	if got := extractSessionID(owner.Body.String()); got != sess.ID {
		t.Fatalf("owner continuation ran in session %q, want %q; body=%s", got, sess.ID, owner.Body.String())
	}
}
