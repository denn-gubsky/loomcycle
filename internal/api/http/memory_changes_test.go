package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/cdc"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// TestMemoryChanges_AuthGate pins the locked decision: both change feeds are
// substrate:tenant AND carved out of member access (not member-accessible).
func TestMemoryChanges_AuthGate(t *testing.T) {
	for _, p := range []string{"/v1/_memory/changes", "/v1/_document/changes"} {
		if got := requiredScopeFor(http.MethodGet, p); got != auth.ScopeTenant {
			t.Errorf("%s scope = %q, want %s", p, got, auth.ScopeTenant)
		}
		if tenantMemberAccessible(http.MethodGet, p) {
			t.Errorf("%s must NOT be member-accessible (memberCarveOut)", p)
		}
	}
}

// TestMemoryChanges_SSEStreamsAndFilters seeds one memory + one document change
// and asserts each feed streams only its own family (tenant-scoped).
func TestMemoryChanges_SSEStreamsAndFilters(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.AppendMemoryChange(ctx, store.MemoryChange{Type: store.MemoryChangeSet, Scope: store.MemoryScopeAgent, ScopeID: "a1", Key: "k1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendMemoryChange(ctx, store.MemoryChange{Type: store.DocumentChangeUpdated, Scope: store.MemoryScopeAgent, ScopeID: "a1", ChunkID: "CID"}); err != nil {
		t.Fatal(err)
	}

	srv := &Server{store: st, cfg: &config.Config{}}

	mem := runSSEOnce(t, srv.handleMemoryChanges, "/v1/_memory/changes")
	if !strings.Contains(mem, `"memory.set"`) || !strings.Contains(mem, `"k1"`) {
		t.Errorf("memory feed missing its change: %s", mem)
	}
	if strings.Contains(mem, `"document.chunk.updated"`) {
		t.Errorf("memory feed leaked a document change: %s", mem)
	}

	doc := runSSEOnce(t, srv.handleDocumentChanges, "/v1/_document/changes")
	if !strings.Contains(doc, `"document.chunk.updated"`) || !strings.Contains(doc, `"CID"`) {
		t.Errorf("document feed missing its change: %s", doc)
	}
	if strings.Contains(doc, `"memory.set"`) {
		t.Errorf("document feed leaked a memory change: %s", doc)
	}
}

// runSSEOnce invokes an SSE handler with a short-lived context: it drains the
// already-present changes on entry, then returns when the context times out.
func runSSEOnce(t *testing.T, h http.HandlerFunc, path string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec.Body.String()
}

// TestMemoryChanges_OpeningFrameSaysWhetherTheFeedIsOn.
//
// With LOOMCYCLE_MEMORY_CHANGES_ENABLED unset, nothing writes the change table, so
// the stream connects, keepalives flow, and no change frame ever arrives — which
// looks exactly like a healthy feed over a quiet store. A reader watching for "is
// the pass doing anything" would wait forever on a misconfiguration and conclude
// the answer was no.
//
// The answer comes from the STORE, not from re-reading the env var: the CDC
// decorator is in the write path exactly when the feed is on, so its presence
// cannot disagree with whether writes are actually captured.
func TestMemoryChanges_OpeningFrameSaysWhetherTheFeedIsOn(t *testing.T) {
	raw, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()

	for _, tc := range []struct {
		name  string
		store store.Store
		want  string
	}{
		// A plain store does not implement CapturesChanges at all.
		{"feed off", raw, `"enabled":false`},
		{"feed on", cdc.Wrap(raw, nil), `"enabled":true`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := &Server{store: tc.store, cfg: &config.Config{}}
			for path, h := range map[string]http.HandlerFunc{
				"/v1/_memory/changes":   srv.handleMemoryChanges,
				"/v1/_document/changes": srv.handleDocumentChanges,
			} {
				body := runSSEOnce(t, h, path)
				if !strings.Contains(body, "event: feed") {
					t.Errorf("%s: no opening feed frame — a reader cannot tell a disabled feed "+
						"from a quiet one:\n%s", path, body)
				}
				if !strings.Contains(body, tc.want) {
					t.Errorf("%s: opening frame missing %s:\n%s", path, tc.want, body)
				}
			}
		})
	}
}

// TestMemoryChanges_OpeningFrameEchoesTheCursor.
//
// A reader resuming with ?since=N needs to know the server accepted that cursor.
// Without it, a `since` the server silently ignored (unparseable, negative) looks
// like a feed with nothing new, and the reader re-reads from 0 or waits forever
// depending on which way it guessed.
func TestMemoryChanges_OpeningFrameEchoesTheCursor(t *testing.T) {
	raw, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	srv := &Server{store: cdc.Wrap(raw, nil), cfg: &config.Config{}}

	if body := runSSEOnce(t, srv.handleMemoryChanges, "/v1/_memory/changes?since=42"); !strings.Contains(body, `"since":42`) {
		t.Errorf("opening frame did not echo the accepted cursor:\n%s", body)
	}
	// A cursor the server REFUSED must report as the value actually in use, not as
	// the one that was asked for.
	if body := runSSEOnce(t, srv.handleMemoryChanges, "/v1/_memory/changes?since=-7"); !strings.Contains(body, `"since":0`) {
		t.Errorf("a rejected cursor must report the cursor in force (0):\n%s", body)
	}
}
