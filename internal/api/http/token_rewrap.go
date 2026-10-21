package http

import (
	"context"
	"log"
	"sort"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC ES #6 / F3 — moving tokens minted before the derived pepper onto it.
//
// A row minted with no pepper carries the unpeppered hash, which anyone who
// can write the database can compute for a token of their choosing. So such
// a row is never accepted as it is. Instead, at boot under a derived pepper
// and before serving, every such row is WRAPPED: its stored hash h becomes
// w1 = HMAC(pepper, h) (auth.TokenHasher), which a lookup recomputes from the
// presented token. The token keeps working; the database stops holding
// anything a writer could have computed.
//
// Which untagged rows are genuine: every mint under a derived pepper writes a
// tagged hash, and the rewrap leaves none untagged. So an untagged row in a
// table that holds ANY tagged row (any form, any key) was not written by a
// server on a derived pepper. It is refused, never rewrapped — at this boot
// and at every later one, which a per-boot allow-list of rows could not
// promise. The rewrap happens only while the table holds no tagged row at
// all: the first boot after LOOMCYCLE_SECRET_KEY is set.
//
// The residual: a database writer can still get a token of their choosing
// rewrapped if, at a boot under a derived pepper, the table holds no tagged
// row — it is empty (no token was ever minted; the expiry sweep keeps each
// tenant's last row), or they delete or overwrite every tagged row, logging
// every token holder out. They then write an unpeppered row and wait for a
// restart. Closing that needs a marker stored outside the token table.
//
// Rows under the PREVIOUS secret key's pepper cannot be rewrapped without
// the token, so they move onto the current form at their next login, as
// wrapped rows do.

// TokenRewrapReport is what the boot rewrap found and did.
type TokenRewrapReport struct {
	// Rewrapped are the def_ids moved from an unpeppered hash onto the
	// wrapped form at this boot (by this replica or one booting beside it),
	// sorted. Not secret; listed so an operator can spot a row they never
	// minted.
	Rewrapped []string
	// Wrapped counts rows on the wrapped form after the pass, Rewrapped
	// included. Each moves to the current form at its next login.
	Wrapped int
	// Previous counts rows under the pepper LOOMCYCLE_SECRET_KEY_PREVIOUS
	// derives. Each moves to the current form at its next login.
	Previous int
	// OtherKey counts rows under a pepper neither secret key derives. They
	// cannot authenticate.
	OtherKey int
	// Refused are the def_ids of unpeppered rows left alone because the
	// table was already on a derived pepper, sorted. They cannot
	// authenticate.
	Refused []string
}

// RewrapLegacyTokenRows wraps every unpeppered token row under the derived
// pepper, if the table is not on a derived pepper yet. Call once at boot,
// before serving. A no-op under an explicit pepper (its untagged rows are
// current) and with no pepper. On an error nothing is rewrapped: the rows stay
// unpeppered and cannot authenticate until a later boot rewraps them.
func (s *Server) RewrapLegacyTokenRows(ctx context.Context) (TokenRewrapReport, error) {
	h, err := s.hasher()
	if err != nil {
		return TokenRewrapReport{}, err
	}
	if s.store == nil || h.Source() != auth.PepperDerived {
		s.wrappedTokens.Store(false)
		return TokenRewrapReport{}, nil
	}
	// Until the pass knows better a wrapped row may exist, so an error below
	// leaves the lookup trying the wrapped form. Trying it is always safe:
	// it needs the pepper to forge.
	s.wrappedTokens.Store(true)
	// The table has no list-all; names then rows per name, once, at boot.
	names, err := s.store.OperatorTokenDefListNames(ctx)
	if err != nil {
		return TokenRewrapReport{}, err
	}
	var rep TokenRewrapReport
	var untagged []store.OperatorTokenDefRow
	tagged := false
	for _, n := range names {
		rows, err := s.store.OperatorTokenDefListByName(ctx, n.Name)
		if err != nil {
			return TokenRewrapReport{}, err
		}
		for _, r := range rows {
			switch h.Form(r.TokenHash) {
			case auth.FormUntagged:
				untagged = append(untagged, r)
				continue
			case auth.FormWrapped:
				rep.Wrapped++
			case auth.FormPrevious:
				rep.Previous++
			case auth.FormOtherKey:
				rep.OtherKey++
			}
			tagged = true
		}
	}
	ids := make([]string, 0, len(untagged))
	for _, r := range untagged {
		ids = append(ids, r.DefID)
	}
	sort.Strings(ids)
	switch {
	case len(untagged) == 0:
	case tagged:
		rep.Refused = ids
	default:
		ops := make([]store.OperatorTokenRehashOp, 0, len(untagged))
		for _, r := range untagged {
			ops = append(ops, store.OperatorTokenRehashOp{DefID: r.DefID, OldHash: r.TokenHash, NewHash: h.Wrap(r.TokenHash)})
		}
		// One transaction: a pass stopped part-way would leave a table that
		// reads as already on the derived pepper, with the rest stranded.
		// Each op is a compare-and-set, so a replica booting beside this one
		// changes nothing the other already did.
		if _, err := s.store.OperatorTokenRehashAll(ctx, ops); err != nil {
			return TokenRewrapReport{}, err
		}
		rep.Rewrapped = ids
		rep.Wrapped += len(ids)
	}
	s.wrappedTokens.Store(rep.Wrapped > 0)
	return rep, nil
}

// lookupOlderTokenForm is the miss path of the token lookup under a derived
// pepper: try the wrapped form and the previous key's forms of bearer, and on
// a match move that row onto currentHash. Returns miss unchanged when nothing
// matches, and any store error other than not-found so the caller fails
// closed. Every form tried needs a pepper to compute, so a match is genuine.
//
// The caller does not cache a refusal from it: it may be the losing side of
// two first logins of the same token. That race is also why a miss here looks
// at currentHash once more before giving up — the other login may have moved
// the row onto it in between, and every move is onto currentHash.
func (s *Server) lookupOlderTokenForm(ctx context.Context, h auth.TokenHasher, bearer, currentHash string, miss error) (store.OperatorTokenDefRow, error) {
	var older []string
	if s.wrappedTokens.Load() {
		older = append(older, h.WrappedHash(bearer))
	}
	older = append(older, h.PreviousHashes(bearer)...)
	for _, old := range older {
		row, err := s.store.OperatorTokenDefGetByTokenHash(ctx, old)
		if isNotFound(err) {
			continue
		}
		if err != nil {
			return store.OperatorTokenDefRow{}, err
		}
		// The match authenticates whether or not the rewrite lands: losing
		// the compare-and-set means another login moved the row first.
		changed, rerr := s.store.OperatorTokenRehash(ctx, row.DefID, old, currentHash)
		switch {
		case rerr != nil:
			log.Printf("auth: token_def=%q: rehash onto the current pepper failed: %v", row.DefID, rerr)
		case changed && s.authVerbose():
			log.Printf("auth: token_def=%q rehashed onto the current pepper", row.DefID)
		}
		return row, nil
	}
	row, err := s.store.OperatorTokenDefGetByTokenHash(ctx, currentHash)
	if err == nil {
		return row, nil
	}
	if !isNotFound(err) {
		return store.OperatorTokenDefRow{}, err
	}
	return store.OperatorTokenDefRow{}, miss
}

// SetTokenHasher fixes the hasher every mint and lookup on this server uses.
// Call from main.go before serving, with the hasher the operator-token tool
// was given, so the two can never disagree. Unset (tests), it is built from
// the config on each use.
func (s *Server) SetTokenHasher(h auth.TokenHasher) { s.tokenHasher = &h }

func (s *Server) hasher() (auth.TokenHasher, error) {
	if s.tokenHasher != nil {
		return *s.tokenHasher, nil
	}
	return s.cfg().Env.TokenHasher()
}
