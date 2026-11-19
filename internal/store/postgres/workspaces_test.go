package postgres

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/storetest"
)

func TestWorkspaceContract(t *testing.T) {
	pool := newSchemaPool(t, pgDSNFromEnv(t))
	storetest.RunWorkspaces(t, func(t *testing.T) (store.Store, func()) {
		return pool.fixture(t, false)
	})
}
