package sqlite

import (
	"path/filepath"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auditchain/chaintest"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestAuditContract runs the audit stream contract with two handles on one
// database file — two processes sharing it, as far as SQLite's locking goes.
func TestAuditContract(t *testing.T) {
	chaintest.RunAudit(t, func(t *testing.T) (store.Store, store.Store, func()) {
		path := filepath.Join(t.TempDir(), "audit.db")
		a, err := Open(path)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		b, err := Open(path)
		if err != nil {
			_ = a.Close()
			t.Fatalf("Open (second handle): %v", err)
		}
		return a, b, func() { _ = b.Close(); _ = a.Close() }
	})
}
