package sqlite

import (
	"context"
	"path/filepath"
	"testing"
)

// PoolStats reads the handle's pool counters: after a query the pool holds an
// open connection and has given it back.
func TestStore_PoolStatsReportsTheHandlesPool(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "pool.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.CreateSession(context.Background(), "t", "a", "u"); err != nil {
		t.Fatal(err)
	}
	st := s.PoolStats()
	if st.Open < 1 || st.InUse != 0 || st.Idle != st.Open {
		t.Errorf("pool = %+v, want open >= 1, none in use, the rest idle", st)
	}
}
