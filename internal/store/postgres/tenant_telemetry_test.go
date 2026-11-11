package postgres

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/storetest"
)

func TestTenantTelemetryPolicyContract(t *testing.T) {
	pool := newSchemaPool(t, pgDSNFromEnv(t))
	storetest.RunTenantTelemetry(t, func(t *testing.T) (store.Store, func()) {
		return pool.fixture(t, false)
	})
}
