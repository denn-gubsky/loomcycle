package store

import "time"

// PoolStats is a store's database connection pool, read from the driver's
// in-memory counters without a query: what /metrics reports per database.
type PoolStats struct {
	Open, InUse, Idle int
	// WaitCount is the connection acquires that had to wait for a free
	// connection, and WaitDuration their total wait, since the pool opened.
	WaitCount    int64
	WaitDuration time.Duration
}
