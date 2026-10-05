package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// A server in open mode (no token configured) lists tenants as an admin
// would, and the runs it started — which carry no tenant — are the shared
// tenant's: the list answers 200 with them under "", not a 500.
func TestListTenants_OpenModeListsTheSharedTenantsRuns(t *testing.T) {
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "tenants.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	for _, tc := range []struct{ tenant, user string }{{"", "alice"}, {"", "bob"}, {"acme", "carol"}} {
		sess, err := st.CreateSession(ctx, tc.tenant, "chat", tc.user)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "chat", UserID: tc.user, TenantID: tc.tenant}); err != nil {
			t.Fatal(err)
		}
	}
	srv := New(&config.Config{}, &stubResolver{}, nil, concurrency.New(1, 1, time.Second), st)

	w := httptest.NewRecorder()
	srv.Mux().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/_tenants", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/_tenants in open mode: %d %s", w.Code, w.Body.String())
	}
	var out struct {
		Tenants []struct {
			Tenant string `json:"tenant"`
			Users  int    `json:"users"`
			Runs   int    `json:"runs"`
		} `json:"tenants"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	got := map[string][2]int{}
	for _, r := range out.Tenants {
		got[r.Tenant] = [2]int{r.Users, r.Runs}
	}
	if len(got) != 2 || got[""] != [2]int{2, 2} || got["acme"] != [2]int{1, 1} {
		t.Errorf("tenants = %s, want the shared tenant (2 users, 2 runs) and acme (1, 1)", w.Body.String())
	}
}
