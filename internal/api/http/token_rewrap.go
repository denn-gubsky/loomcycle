package http

import (
	"context"
	"fmt"
	"log"
	"sort"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC ES #6 / F3 / C2 — moving tokens minted before the derived pepper onto it.
//
// A row minted with no pepper carries the unpeppered hash, which anyone who
// can write the database can compute for a token of their choosing. So such
// a row is never accepted as it is. The operator moves the genuine ones onto
// the pepper with `loomcycle tokens rewrap` (RewrapUntaggedTokenHashes): its
// stored hash h becomes w1 = HMAC(pepper, h) (auth.TokenHasher), which a
// lookup recomputes from the presented token. The token keeps working; the
// database stops holding anything a writer could have computed.
//
// Why a command, not a pass at boot (C2): nothing in the row says whether a
// server wrote it before the pepper existed or a database writer wrote it
// since, so the decision to rewrap belongs to the operator, who sees the
// def_ids first (--dry-run). A boot-time pass had to infer it from database
// state (a marker row, the tagged rows), and a database writer could delete
// those and have the next restart rewrap a row of their own. Boot now only
// reports the unpeppered rows; nothing a database writer does makes the
// server rewrap one.
//
// Rows under the PREVIOUS secret key's pepper cannot be rewrapped without
// the token, so they move onto the current form at their next login, as
// wrapped rows do.

// TokenHashReport is what a scan of the token table found, read against a
// derived pepper.
type TokenHashReport struct {
	// Untagged are the def_ids of rows on an unpeppered hash (or one under an
	// explicit pepper since removed), sorted. Not secret; listed so an
	// operator can spot a row they never minted. They cannot authenticate
	// until rewrapped.
	Untagged []string
	// Wrapped counts rows on the wrapped form. Each moves to the current form
	// at its next login.
	Wrapped int
	// Previous counts rows under the pepper LOOMCYCLE_SECRET_KEY_PREVIOUS
	// derives. Each moves to the current form at its next login.
	Previous int
	// OtherKey counts rows under a pepper neither secret key derives. They
	// cannot authenticate.
	OtherKey int
}

// scanTokenHashes reads every token row and sorts it by form under h,
// returning the untagged rows themselves for a rewrap. The table has no
// list-all; names then rows per name.
func scanTokenHashes(ctx context.Context, st store.Store, h auth.TokenHasher) (TokenHashReport, []store.OperatorTokenDefRow, error) {
	names, err := st.OperatorTokenDefListNames(ctx)
	if err != nil {
		return TokenHashReport{}, nil, err
	}
	var rep TokenHashReport
	var untagged []store.OperatorTokenDefRow
	for _, n := range names {
		rows, err := st.OperatorTokenDefListByName(ctx, n.Name)
		if err != nil {
			return TokenHashReport{}, nil, err
		}
		for _, r := range rows {
			switch h.Form(r.TokenHash) {
			case auth.FormUntagged:
				untagged = append(untagged, r)
				rep.Untagged = append(rep.Untagged, r.DefID)
			case auth.FormWrapped:
				rep.Wrapped++
			case auth.FormPrevious:
				rep.Previous++
			case auth.FormOtherKey:
				rep.OtherKey++
			}
		}
	}
	sort.Strings(rep.Untagged)
	return rep, untagged, nil
}

// ReportTokenHashes is the boot check under a derived pepper: it counts the
// token rows on each older form for the boot log, and changes nothing. Call
// once at boot, before serving. A no-op under an explicit pepper (its
// untagged rows are current) and with no pepper.
func (s *Server) ReportTokenHashes(ctx context.Context) (TokenHashReport, error) {
	h, err := s.hasher()
	if err != nil {
		return TokenHashReport{}, err
	}
	if s.store == nil || h.Source() != auth.PepperDerived {
		s.wrappedTokens.Store(false)
		return TokenHashReport{}, nil
	}
	// Until the scan knows better a wrapped row may exist, so an error below
	// leaves the lookup trying the wrapped form. Trying it is always safe:
	// it needs the pepper to forge.
	s.wrappedTokens.Store(true)
	rep, _, err := scanTokenHashes(ctx, s.store, h)
	if err != nil {
		return TokenHashReport{}, err
	}
	// An untagged row counts too: the rewrap command may move it onto the
	// wrapped form while this server runs, and its token should then log in
	// without a restart.
	s.wrappedTokens.Store(rep.Wrapped > 0 || len(rep.Untagged) > 0)
	return rep, nil
}

// TokenRewrapResult is what RewrapUntaggedTokenHashes found and did.
type TokenRewrapResult struct {
	// TokenHashReport is the scan before the rewrap.
	TokenHashReport
	// Rewrapped counts the rows this call moved onto the wrapped form. Fewer
	// than len(Untagged) when a row's hash changed between the scan and the
	// write — another rewrap running at the same time.
	// Always 0 on a dry run.
	Rewrapped int
}

// RewrapUntaggedTokenHashes moves every untagged token row onto the wrapped
// form under h's derived pepper, in one transaction; with dryRun it only
// scans. Refused unless h's pepper is derived: under an explicit pepper the
// untagged rows are current, and with no pepper there is nothing to wrap
// under. Safe beside running servers and a second run of itself: each row is
// a compare-and-set, so a row is wrapped once, never a wrap of the wrapped
// form.
func RewrapUntaggedTokenHashes(ctx context.Context, st store.Store, h auth.TokenHasher, dryRun bool) (TokenRewrapResult, error) {
	if h.Source() != auth.PepperDerived {
		return TokenRewrapResult{}, fmt.Errorf("token pepper is %s, not derived: there is nothing to rewrap under", h.Source())
	}
	rep, untagged, err := scanTokenHashes(ctx, st, h)
	if err != nil {
		return TokenRewrapResult{}, err
	}
	res := TokenRewrapResult{TokenHashReport: rep}
	if dryRun || len(untagged) == 0 {
		return res, nil
	}
	ops := make([]store.OperatorTokenRehashOp, 0, len(untagged))
	for _, r := range untagged {
		ops = append(ops, store.OperatorTokenRehashOp{DefID: r.DefID, OldHash: r.TokenHash, NewHash: h.Wrap(r.TokenHash)})
	}
	// One transaction: a run stopped part-way would leave the operator
	// guessing which rows moved.
	n, err := st.OperatorTokenRehashAll(ctx, ops)
	if err != nil {
		return TokenRewrapResult{}, err
	}
	res.Rewrapped = n
	return res, nil
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
