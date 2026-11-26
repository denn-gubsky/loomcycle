package postgres

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/workspace/clustertest"
)

// RFC GW P12 plan §3.5, ruling Q7: the several-replica suite on Postgres,
// run by the Postgres job with the store contract.
func TestWorkspaceCluster(t *testing.T) {
	pool := newSchemaPool(t, pgDSNFromEnv(t))
	clustertest.Run(t, func(t *testing.T) store.Store {
		s, cleanup := pool.fixture(t, false)
		t.Cleanup(cleanup)
		return s
	})
}
