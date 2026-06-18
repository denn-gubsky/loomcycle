package sqlmem

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// TestManager_ConcurrentDistinctScopes_NoEvictionRace stresses the LRU
// eviction path: far more concurrent distinct scopes than maxOpenHandles, each
// doing a quota-checked Exec then a Query. Before the in-use refcount, an
// eviction could close a handle mid-Exec (between its quota-check and write),
// surfacing as a spurious sql.ErrConnDone. With the refcount, an in-flight
// handle is never a victim. Run with -race to also catch data races on the
// open-handle map.
func TestManager_ConcurrentDistinctScopes_NoEvictionRace(t *testing.T) {
	mgr, err := New(Config{Root: t.TempDir(), QuotaBytes: 1 << 20, MaxRows: 1000})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	const (
		scopes     = maxOpenHandles * 3 // 192 distinct scope files — forces churn
		iterations = 4
	)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, scopes*iterations*3)

	for s := 0; s < scopes; s++ {
		wg.Add(1)
		go func(s int) {
			defer wg.Done()
			key := ScopeKey{Tenant: "default", Scope: "agent", ScopeID: fmt.Sprintf("a%d", s)}
			if _, err := mgr.Exec(ctx, key, "CREATE TABLE t (n INTEGER)", nil, 0); err != nil {
				errs <- fmt.Errorf("scope %d create: %w", s, err)
				return
			}
			for i := 0; i < iterations; i++ {
				if _, err := mgr.Exec(ctx, key, "INSERT INTO t (n) VALUES (1)", nil, 0); err != nil {
					errs <- fmt.Errorf("scope %d insert %d: %w", s, i, err)
					return
				}
				if _, err := mgr.Query(ctx, key, "SELECT count(*) FROM t", nil); err != nil {
					errs <- fmt.Errorf("scope %d query %d: %w", s, i, err)
					return
				}
			}
		}(s)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}

	// Each scope wrote `iterations` rows and they live in distinct files —
	// confirm isolation survived the churn for a sample scope.
	res, err := mgr.Query(ctx, ScopeKey{Tenant: "default", Scope: "agent", ScopeID: "a0"}, "SELECT count(*) FROM t", nil)
	if err != nil {
		t.Fatalf("final query: %v", err)
	}
	if len(res.Rows) != 1 || fmt.Sprint(res.Rows[0][0]) != fmt.Sprint(int64(iterations)) {
		t.Errorf("scope a0 row count = %v, want %d", res.Rows[0][0], iterations)
	}
}
