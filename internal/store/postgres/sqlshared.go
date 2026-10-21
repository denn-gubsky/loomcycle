package postgres

import "github.com/denn-gubsky/loomcycle/internal/store/sqlshared"

// shared is this store's pool as the sqlshared layer sees it. Domains whose
// queries moved there call through it.
func (s *Store) shared() sqlshared.DB { return sqlshared.PostgresDB(s.pool) }
