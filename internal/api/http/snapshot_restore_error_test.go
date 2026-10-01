package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/limits"
	"github.com/denn-gubsky/loomcycle/internal/snapshot"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// failedRestoreOnEveryCallSite restores an envelope expected to FAIL through
// the named transport and returns the error text the caller sees.
var failedRestoreOnEveryCallSite = []struct {
	name    string
	restore func(t *testing.T, srv *Server, envelope []byte) string
}{
	{"connector", func(t *testing.T, srv *Server, envelope []byte) string {
		_, err := srv.RestoreSnapshot(context.Background(), connector.RestoreSnapshotRequest{RawJSON: envelope})
		if err == nil {
			t.Fatal("RestoreSnapshot succeeded; the envelope was built to fail partway")
		}
		return err.Error()
	}},
	{"http", func(t *testing.T, srv *Server, envelope []byte) string {
		body, _ := json.Marshal(map[string]any{"json": json.RawMessage(envelope)})
		req := httptest.NewRequest("POST", "/v1/_snapshots/inline/restore", bytes.NewReader(body))
		req.SetPathValue("id", "inline")
		rec := httptest.NewRecorder()
		srv.handleRestoreSnapshot(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("restore status = %d, body = %s; want 500 restore_failed", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}},
}

// A restore that fails partway — past the version check, on a section that
// does not decode — has already written the budgets and usage that come
// first. Those bind at once, as after a successful restore: before the fix
// the error path skipped the refresh, and a retry found the rows present and
// had nothing to refresh, so the budget went unenforced until a restart. The
// warnings gathered before the stop reach the caller too.
func TestRestore_AFailedRestoreStillBindsTheBudgetItWrote(t *testing.T) {
	for _, tc := range failedRestoreOnEveryCallSite {
		t.Run(tc.name, func(t *testing.T) {
			src, err := storesqlite.Open(filepath.Join(t.TempDir(), "source.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = src.Close() }()
			seedBudget(t, src, 1000, 1000)
			_, good, err := snapshot.Capture(context.Background(), src, snapshot.CaptureOptions{})
			if err != nil {
				t.Fatal(err)
			}
			var env map[string]any
			if err := json.Unmarshal(good, &env); err != nil {
				t.Fatal(err)
			}
			sections := env["sections"].(map[string]any)
			// agent_defs keeps a readable version but its entries do not decode.
			sections["agent_defs"] = map[string]any{"version": "1.0", "entries": "not a list"}
			// A section this reader does not know: its warning is gathered
			// before the stop.
			sections["zz_future_section"] = map[string]any{"version": "1.0"}
			delete(env, "checksum")
			broken, err := json.Marshal(env)
			if err != nil {
				t.Fatal(err)
			}

			srv, _ := makeServer(t, completingProvider(), makeBaseConfig())
			if dec := srv.limits.Check("acme", "alice"); !dec.Allowed {
				t.Fatal("setup: acme/alice is refused before the restore")
			}

			msg := tc.restore(t, srv, broken)
			if !strings.Contains(msg, "agent_defs") {
				t.Errorf("error %q does not name the section that stopped the restore", msg)
			}
			if !strings.Contains(msg, "zz_future_section") {
				t.Errorf("error %q drops the warnings gathered before the stop", msg)
			}
			if got, _ := srv.store.TokenLimitsAll(context.Background()); len(got) != 1 {
				t.Fatalf("budgets written before the stop = %+v; the test needs the budget written", got)
			}
			if dec := srv.limits.Check("acme", "alice"); dec.Allowed {
				t.Error("the budget the failed restore wrote (1000 spent of 1000) is not enforced")
			}
		})
	}
}

// limitsReadFailStore fails the token_limits read the refresh makes.
type limitsReadFailStore struct{ store.Store }

func (limitsReadFailStore) TokenLimitsAll(context.Context) ([]store.TokenLimitRow, error) {
	return nil, errors.New("injected token_limits read failure")
}

// refreshHarness is a server with a live tracker over st.
func refreshHarness(t *testing.T) (*Server, store.Store) {
	t.Helper()
	srv, st, cleanup := minimalServerWithSnapshotStore(t)
	t.Cleanup(cleanup)
	srv.limits = limits.New(st)
	return srv, st
}

// The refresh pushes a restored budget as the store holds it when the refresh
// runs, not as the envelope carried it: a PUT /v1/_limits that landed between
// the restore's insert and the refresh is the operator's newer word. Before
// the fix the tracker held the envelope's 1000 while the store held 10.
func TestPostRestoreRefresh_UsesStoredValueNotEnvelope(t *testing.T) {
	srv, st := refreshHarness(t)
	ctx := context.Background()
	if err := st.TokenLimitPut(ctx, store.TokenLimitRow{TenantID: "acme", Scope: "tenant", HardLimit: i64p(10), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	srv.limits.Add("acme", "alice", 50)

	res := snapshot.RestoreResult{}
	res.Refresh.TokenLimits = []store.TokenLimitRow{{TenantID: "acme", Scope: "tenant", HardLimit: i64p(1000)}}
	srv.postRestoreRefresh(ctx, &res)

	if dec := srv.limits.Check("acme", "alice"); dec.Allowed {
		t.Error("50 spent is admitted: the tracker holds the envelope's 1000, not the stored 10")
	}
}

// A restored budget the store no longer holds — a DELETE /v1/_limits between
// the insert and the refresh — is dropped from the tracker, not cached as a
// phantom ceiling.
func TestPostRestoreRefresh_DropsABudgetDeletedSinceTheRestore(t *testing.T) {
	srv, _ := refreshHarness(t)
	ctx := context.Background()
	srv.limits.Add("acme", "bob", 50)

	res := snapshot.RestoreResult{}
	res.Refresh.TokenLimits = []store.TokenLimitRow{{TenantID: "acme", Scope: "user", ScopeID: "bob", HardLimit: i64p(1)}}
	srv.postRestoreRefresh(ctx, &res)

	if dec := srv.limits.Check("acme", "bob"); !dec.Allowed {
		t.Errorf("acme/bob refused by %+v: a ceiling the store no longer holds was cached", dec.Refusal)
	}
}

// When the re-read fails, the refresh falls back to the envelope rows, so a
// restored budget is never left unenforced by a transient store fault.
func TestPostRestoreRefresh_ReadFailurePushesTheRestoredRows(t *testing.T) {
	srv, st := refreshHarness(t)
	srv.store = limitsReadFailStore{st}
	ctx := context.Background()
	srv.limits.Add("acme", "alice", 50)

	res := snapshot.RestoreResult{}
	res.Refresh.TokenLimits = []store.TokenLimitRow{{TenantID: "acme", Scope: "tenant", HardLimit: i64p(10)}}
	srv.postRestoreRefresh(ctx, &res)

	if dec := srv.limits.Check("acme", "alice"); dec.Allowed {
		t.Error("50 spent is admitted: the restored hard=10 was not pushed when the re-read failed")
	}
}
