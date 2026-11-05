package postgres

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/storetest"
)

// TestAuditContract runs the audit stream contract with two Stores — two
// pools, as two replicas hold — on one schema.
func TestAuditContract(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	storetest.RunAudit(t, func(t *testing.T) (store.Store, store.Store, func()) {
		fx := freshSchema(t, dsn)
		b, err := Open(context.Background(), Config{DSN: fx.storeDSN, MaxOpenConns: 8})
		if err != nil {
			fx.cleanup()
			t.Fatalf("open the second replica's store: %v", err)
		}
		return fx.store, b, func() { _ = b.Close(); fx.cleanup() }
	})
}
