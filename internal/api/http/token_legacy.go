package http

import (
	"context"
	"log"
	"sync"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC ES #6 / D2 — moving tokens minted before the derived pepper onto it.
//
// A token row minted with no pepper, or under the pepper an earlier
// LOOMCYCLE_SECRET_KEY derived, does not match the current hash. Such a row
// is still accepted under its legacy hash — and rewritten to the current one
// on that login — but ONLY if it was already on a legacy hash when this
// replica booted. That restriction is the point: the legacy forms are exactly
// what a database writer can compute without the pepper, so a row inserted
// (or edited onto a legacy hash) after boot must never match one.
//
// Which rows are on a legacy hash is read from the row, not guessed: a
// derived-pepper hash carries a tag naming the pepper (auth.TokenHasher), so
// the boot set holds exactly the rows still to migrate, and a row rehashed on
// a previous run is never a candidate again. A row leaves the set when it is
// rehashed or when it logs in under the current hash.
//
// The set is per replica and in memory. The residual window: a writer who
// edits one of these boot-time rows onto a legacy hash of their choosing
// before its owner logs in, or edits any row and waits for a restart, is
// trusted. LOOMCYCLE_TOKEN_LEGACY_HASH=off closes it.
type legacyTokenSet struct {
	mu  sync.Mutex
	ids map[string]struct{}
}

func (l *legacyTokenSet) replace(ids map[string]struct{}) {
	l.mu.Lock()
	l.ids = ids
	l.mu.Unlock()
}

func (l *legacyTokenSet) has(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.ids[id]
	return ok
}

func (l *legacyTokenSet) empty() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.ids) == 0
}

func (l *legacyTokenSet) forget(id string) {
	l.mu.Lock()
	delete(l.ids, id)
	l.mu.Unlock()
}

// SnapshotLegacyTokenRows records which token rows are on a legacy hash now.
// Call once at boot, before serving: only these rows may log in under a
// legacy hash. Returns how many rows are on one — reported even when the
// fallback is off, when those rows can no longer log in at all.
func (s *Server) SnapshotLegacyTokenRows(ctx context.Context) (int, error) {
	h, err := s.cfg().Env.TokenHasher()
	if err != nil {
		return 0, err
	}
	if s.store == nil || h.Source() != auth.PepperDerived {
		s.legacyTokens.replace(nil)
		return 0, nil
	}
	// The table has no list-all; names then rows per name, once, at boot.
	names, err := s.store.OperatorTokenDefListNames(ctx)
	if err != nil {
		return 0, err
	}
	ids := map[string]struct{}{}
	for _, n := range names {
		rows, err := s.store.OperatorTokenDefListByName(ctx, n.Name)
		if err != nil {
			return 0, err
		}
		for _, r := range rows {
			if !h.IsCurrent(r.TokenHash) {
				ids[r.DefID] = struct{}{}
			}
		}
	}
	count := len(ids)
	if !h.LegacyFallback() {
		ids = nil
	}
	s.legacyTokens.replace(ids)
	return count, nil
}

// lookupLegacyToken is the miss path of the token lookup: try each legacy
// hash of bearer, accept a match only for a row in the boot set, and rewrite
// that row to currentHash. Returns miss unchanged when nothing qualifies, and
// any store error other than not-found so the caller fails closed.
func (s *Server) lookupLegacyToken(ctx context.Context, h auth.TokenHasher, bearer, currentHash string, miss error) (store.OperatorTokenDefRow, error) {
	if !h.LegacyFallback() || s.legacyTokens.empty() {
		return store.OperatorTokenDefRow{}, miss
	}
	for _, old := range h.LegacyHashes(bearer) {
		row, err := s.store.OperatorTokenDefGetByTokenHash(ctx, old)
		if isNotFound(err) {
			continue
		}
		if err != nil {
			return store.OperatorTokenDefRow{}, err
		}
		if !s.legacyTokens.has(row.DefID) {
			// A legacy-hash match on a row that was not on a legacy hash at
			// boot is what a forged or tampered row looks like.
			log.Printf("auth: token_def=%q matched a legacy token hash but was not on one at boot; refused", row.DefID)
			return store.OperatorTokenDefRow{}, miss
		}
		// The match authenticates whether or not the rewrite lands: losing
		// the compare-and-set means another replica moved the row first.
		changed, rerr := s.store.OperatorTokenRehash(ctx, row.DefID, old, currentHash)
		switch {
		case rerr != nil:
			log.Printf("auth: token_def=%q: rehash onto the current pepper failed: %v", row.DefID, rerr)
		case changed:
			s.legacyTokens.forget(row.DefID)
			if s.authVerbose() {
				log.Printf("auth: token_def=%q rehashed onto the current pepper", row.DefID)
			}
		}
		return row, nil
	}
	return store.OperatorTokenDefRow{}, miss
}
