package postgres

import "github.com/denn-gubsky/loomcycle/internal/store"

// PoolStats reports the pgx pool from pgxpool.Stat, which reads counters and
// runs no query. A wait is an acquire that found no idle connection
// (EmptyAcquireCount) and its time the time those acquires waited.
func (s *Store) PoolStats() store.PoolStats {
	st := s.pool.Stat()
	return store.PoolStats{Open: int(st.TotalConns()), InUse: int(st.AcquiredConns()), Idle: int(st.IdleConns()),
		WaitCount: st.EmptyAcquireCount(), WaitDuration: st.EmptyAcquireWaitTime()}
}
