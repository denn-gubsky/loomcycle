package lifecycle_test

import (
	"path/filepath"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/workspace/clustertest"
)

// RFC GW P12 plan §3.5: two replicas over one SQLite store. The Postgres
// job runs the same suite (internal/store/postgres).
func TestCluster_SQLite(t *testing.T) {
	clustertest.Run(t, func(t *testing.T) store.Store {
		st, err := storesqlite.Open(filepath.Join(t.TempDir(), "ws.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		return st
	})
}
