package http

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC ES #6 / F3 / C2 — moving tokens minted before the derived pepper onto it.
//
// A row minted with no pepper carries the unpeppered hash, which anyone who
// can write the database can compute for a token of their choosing. So such
// a row is never accepted as it is. The operator moves the genuine ones onto
// the pepper with `loomcycle tokens rewrap`: the stored hash h becomes
// w1 = HMAC(pepper, h) (auth.TokenHasher), which a lookup recomputes from the
// presented token. The token keeps working; the database stops holding
// anything a writer could have computed.
//
// Why a command the operator confirms row by row, not a pass at boot (C2):
// nothing in a row says whether a server wrote it before the pepper existed
// or a database writer wrote it since. A boot-time pass had to infer that
// from database state (a marker row, the tagged rows), which a writer could
// delete and have the next restart rewrap a row of their own. A command that
// rewrapped every unpeppered row would only move the wait: insert, then wait
// for the operator to run it. So the command lists the rows first and writes
// only the ones the operator names — by def_id, or by a digest of the exact
// (def_id, token_hash) set it listed, which refuses if any row was added or
// changed since. Boot only reports. Nothing a database writer does makes
// loomcycle rewrap a row nobody named.
//
// What it cannot tell: a named row whose hash a writer replaced — before the
// review, or, when named by def_id, before the write. The listing shows what
// a row grants, not whose token it holds; that limit is documented.
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

// ListUntaggedTokenRows is the review half of `loomcycle tokens rewrap`: the
// scan of the token table under h's derived pepper, with the unpeppered rows
// themselves (sorted by def_id) for the operator to inspect. Refused unless
// the pepper is derived: under an explicit pepper the untagged rows are
// current, and with no pepper there is nothing to wrap under.
func ListUntaggedTokenRows(ctx context.Context, st store.Store, h auth.TokenHasher) (TokenHashReport, []store.OperatorTokenDefRow, error) {
	if h.Source() != auth.PepperDerived {
		return TokenHashReport{}, nil, fmt.Errorf("token pepper is %s, not derived: there is nothing to rewrap under", h.Source())
	}
	rep, rows, err := scanTokenHashes(ctx, st, h)
	if err != nil {
		return TokenHashReport{}, nil, err
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].DefID < rows[j].DefID })
	return rep, rows, nil
}

// UntaggedTokenSetDigest names an exact set of unpeppered rows: hex SHA-256
// over each (def_id, token_hash), sorted by def_id. The review prints it;
// confirming with it rewraps that set and nothing else — a row added, or a
// listed row's hash changed, since the review gives a different digest.
// The stored hashes are unpeppered SHA-256 of tokens; the digest reveals
// nothing about them a database reader does not already have.
func UntaggedTokenSetDigest(rows []store.OperatorTokenDefRow) string {
	sorted := append([]store.OperatorTokenDefRow(nil), rows...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].DefID < sorted[j].DefID })
	d := sha256.New()
	d.Write([]byte("loomcycle/token-rewrap-set/v1\n"))
	for _, r := range sorted {
		d.Write([]byte(r.DefID + "\x00" + r.TokenHash + "\n"))
	}
	return hex.EncodeToString(d.Sum(nil))
}

// SelectUntaggedTokenRows picks the rows the operator confirmed from the
// current unpeppered rows: exactly the named def_ids, or, with digest, the
// whole set if it still digests to what the review printed. Exactly one of
// the two must be given. Any named def_id that is not an unpeppered row now,
// or a digest that does not match, refuses the selection outright.
func SelectUntaggedTokenRows(rows []store.OperatorTokenDefRow, defIDs []string, digest string) ([]store.OperatorTokenDefRow, error) {
	switch {
	case len(defIDs) > 0 && digest != "":
		return nil, fmt.Errorf("name rows by def_id or by digest, not both")
	case digest != "":
		// The new digest is deliberately not reported: it would invite
		// confirming the changed set without looking at it.
		if UntaggedTokenSetDigest(rows) != digest {
			return nil, fmt.Errorf("the unpeppered rows no longer match the reviewed set: a row was added or changed since the review; run the review again")
		}
		return rows, nil
	case len(defIDs) > 0:
		byID := make(map[string]store.OperatorTokenDefRow, len(rows))
		for _, r := range rows {
			byID[r.DefID] = r
		}
		picked := make([]store.OperatorTokenDefRow, 0, len(defIDs))
		seen := make(map[string]bool, len(defIDs))
		var missing []string
		for _, id := range defIDs {
			if seen[id] {
				continue
			}
			seen[id] = true
			r, ok := byID[id]
			if !ok {
				missing = append(missing, id)
				continue
			}
			picked = append(picked, r)
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("not an unpeppered token row: %s", strings.Join(missing, ", "))
		}
		return picked, nil
	default:
		return nil, fmt.Errorf("no rows named")
	}
}

// RewrapTokenRows moves exactly the given rows onto the wrapped form under
// h's derived pepper, in one transaction, and reports how many it moved.
// Each write is a compare-and-set against the hash the row was listed with,
// so a row changed since (by another rewrap running at the same time, or a
// database writer) is left alone rather than wrapped as it now is. Safe
// beside running servers.
func RewrapTokenRows(ctx context.Context, st store.Store, h auth.TokenHasher, rows []store.OperatorTokenDefRow) (int, error) {
	if h.Source() != auth.PepperDerived {
		return 0, fmt.Errorf("token pepper is %s, not derived: there is nothing to rewrap under", h.Source())
	}
	if len(rows) == 0 {
		return 0, nil
	}
	ops := make([]store.OperatorTokenRehashOp, 0, len(rows))
	for _, r := range rows {
		ops = append(ops, store.OperatorTokenRehashOp{DefID: r.DefID, OldHash: r.TokenHash, NewHash: h.Wrap(r.TokenHash)})
	}
	return st.OperatorTokenRehashAll(ctx, ops)
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
