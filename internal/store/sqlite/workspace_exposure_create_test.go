package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestWorkspaceExposureCreate_ConcurrentCreatesAllSucceed — creating a page
// counts the workspace's live pages and then inserts. In a deferred
// transaction the count takes a read snapshot; when another write commits
// before the insert, SQLite cannot upgrade that snapshot and answers
// SQLITE_BUSY at once, without waiting out busy_timeout. Concurrent creates
// (or a create racing a run's own writes) then failed "database is locked".
// The create must take the write lock from the start.
func TestWorkspaceExposureCreate_ConcurrentCreatesAllSucceed(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	const workers, each = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, workers*each)
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				_, err := st.WorkspaceExposureCreate(ctx, store.WorkspaceExposure{
					ID: fmt.Sprintf("ex-%d-%d", w, i), TenantID: "acme", UserID: "alice",
					WorkspaceID: "ws1", Port: 1000 + w*each + i, Kind: store.WorkspaceExposurePage,
				}, 0)
				if err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("a concurrent create failed: %v", err)
	}
}
