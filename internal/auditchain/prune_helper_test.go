package auditchain_test

import (
	"context"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auditchain"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// pruneForTest prunes one chain the way the retention sweeper does.
func pruneForTest(ctx context.Context, s *sqlite.Store, tenant string, before time.Time) (int, error) {
	return auditchain.Prune(ctx, s, testKeys, tenant, before)
}
