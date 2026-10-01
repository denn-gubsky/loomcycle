package memory

import (
	"context"
	"time"
)

// PendingPruner is the one store method the drained-queue prune needs.
type PendingPruner interface {
	MemoryPendingPruneDrained(ctx context.Context, before time.Time, limit int) (int, error)
}

// PendingPruneBatch is how many drained rows one prune statement deletes. The
// prune loops until a batch comes back short, so this bounds how long one
// statement holds its locks, not how much one sweep removes.
const PendingPruneBatch = 1000

// PruneDrainedPending deletes every consolidation-queue row that was drained
// more than ttl before now, batch rows per statement, and returns how many it
// deleted. ttl <= 0 disables it: nothing is read or deleted.
//
// Why a drained row can go: the ack is the end of a row's useful life. A pass
// resolves `from_pending` provenance for the rows it is holding, before or right
// after it acks them, and a snapshot carries only undrained rows, so nothing
// reads a drained row a day later, let alone a week. Keeping it costs a scan on
// every consolidation tick and keeps raw chat payloads indefinitely.
func PruneDrainedPending(ctx context.Context, st PendingPruner, ttl time.Duration, now time.Time, batch int) (int, error) {
	if ttl <= 0 {
		return 0, nil
	}
	if batch <= 0 {
		batch = PendingPruneBatch
	}
	before := now.Add(-ttl)
	total := 0
	for {
		n, err := st.MemoryPendingPruneDrained(ctx, before, batch)
		total += n
		if err != nil {
			return total, err
		}
		if n < batch {
			return total, nil
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}
