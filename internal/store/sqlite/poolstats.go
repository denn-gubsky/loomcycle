package sqlite

import "github.com/denn-gubsky/loomcycle/internal/store"

// PoolStats reports the database handle's pool from sql.DB.Stats, which reads
// counters and runs no query.
func (s *Store) PoolStats() store.PoolStats {
	st := s.db.Stats()
	return store.PoolStats{Open: st.OpenConnections, InUse: st.InUse, Idle: st.Idle,
		WaitCount: st.WaitCount, WaitDuration: st.WaitDuration}
}
