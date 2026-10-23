package http

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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
// only the ones the operator names, each pinned to what was reviewed: by
// `<def_id>@<fingerprint>` (TokenRowFingerprint), or by a digest of the whole
// listed set. Either refuses if a named row changed since. Boot only
// reports. Nothing a database writer does makes loomcycle rewrap a row
// nobody named.
//
// Only a LIVE row is a candidate (store.OperatorTokenDefRow.ValidAt): a
// retired or expired row, once wrapped, would need only its retired_at
// cleared to become a working token, which defeats retiring a row as the
// review advises.
//
// What it cannot tell: a reviewed row whose hash a writer replaced before
// the review. The listing shows what a row grants, not whose token it holds;
// that limit is documented.
//
// Rows under the PREVIOUS secret key's pepper cannot be rewrapped without
// the token, so they move onto the current form at their next login, as
// wrapped rows do.

// TokenHashReport is what a scan of the token table found, read against a
// derived pepper. Every count but Dead is of live rows (not retired, not
// expired, a rotation grace window included).
type TokenHashReport struct {
	// Untagged are the def_ids of live rows on an unpeppered hash (or one
	// under an explicit pepper since removed), sorted. Not secret; listed so
	// an operator can spot a row they never minted. They cannot authenticate
	// until rewrapped.
	Untagged []string
	// Current counts rows on the current form.
	Current int
	// Wrapped counts rows on the wrapped form. Each moves to the current form
	// at its next login.
	Wrapped int
	// Previous counts rows under the pepper LOOMCYCLE_SECRET_KEY_PREVIOUS
	// derives. Each moves to the current form at its next login.
	Previous int
	// OtherKey counts rows under a pepper neither secret key derives. They
	// cannot authenticate.
	OtherKey int
	// UntaggedDead counts unpeppered rows that are retired or expired. They
	// can never authenticate and are never rewrapped.
	UntaggedDead int
}

// LikelyWrongKey reports a table that looks as if it was read with a secret
// key other than the one the server uses: rows under some other key, and
// none under this one. Wrapping under this pepper would then kill the tokens
// it wraps, so the rewrap refuses.
func (r TokenHashReport) LikelyWrongKey() bool {
	return r.OtherKey > 0 && r.Current+r.Wrapped == 0
}

// UntaggedTokenRows is the review of `loomcycle tokens rewrap`.
type UntaggedTokenRows struct {
	TokenHashReport
	// Live are the unpeppered rows that may be rewrapped, sorted by def_id.
	Live []store.OperatorTokenDefRow
	// Dead are the unpeppered rows that are retired or expired, sorted by
	// def_id. Listed apart; never rewrapped.
	Dead []store.OperatorTokenDefRow
}

// scanTokenHashes reads every token row and sorts it by form under h. The
// table has no list-all; names then rows per name. The names listing has one
// row per (tenant, name) while the by-name read returns every tenant's rows,
// so each name is read once.
func scanTokenHashes(ctx context.Context, st store.Store, h auth.TokenHasher) (UntaggedTokenRows, error) {
	names, err := st.OperatorTokenDefListNames(ctx)
	if err != nil {
		return UntaggedTokenRows{}, err
	}
	now := time.Now()
	var res UntaggedTokenRows
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		if seen[n.Name] {
			continue
		}
		seen[n.Name] = true
		rows, err := st.OperatorTokenDefListByName(ctx, n.Name)
		if err != nil {
			return UntaggedTokenRows{}, err
		}
		for _, r := range rows {
			form := h.Form(r.TokenHash)
			if !r.ValidAt(now) {
				if form == auth.FormUntagged {
					res.Dead = append(res.Dead, r)
					res.UntaggedDead++
				}
				continue
			}
			switch form {
			case auth.FormUntagged:
				res.Live = append(res.Live, r)
				res.Untagged = append(res.Untagged, r.DefID)
			case auth.FormCurrent:
				res.Current++
			case auth.FormWrapped:
				res.Wrapped++
			case auth.FormPrevious:
				res.Previous++
			case auth.FormOtherKey:
				res.OtherKey++
			}
		}
	}
	sort.Strings(res.Untagged)
	byID := func(rows []store.OperatorTokenDefRow) {
		sort.Slice(rows, func(i, j int) bool { return rows[i].DefID < rows[j].DefID })
	}
	byID(res.Live)
	byID(res.Dead)
	return res, nil
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
	res, err := scanTokenHashes(ctx, s.store, h)
	if err != nil {
		return TokenHashReport{}, err
	}
	// A live untagged row counts too: the rewrap command may move it onto
	// the wrapped form while this server runs, and its token should then log
	// in without a restart.
	s.wrappedTokens.Store(res.Wrapped > 0 || len(res.Untagged) > 0)
	return res.TokenHashReport, nil
}

// ListUntaggedTokenRows is the review half of `loomcycle tokens rewrap`: the
// scan of the token table under h's derived pepper, with the unpeppered rows
// themselves for the operator to inspect. Refused unless the pepper is
// derived: under an explicit pepper the untagged rows are current, and with
// no pepper there is nothing to wrap under.
func ListUntaggedTokenRows(ctx context.Context, st store.Store, h auth.TokenHasher) (UntaggedTokenRows, error) {
	if h.Source() != auth.PepperDerived {
		return UntaggedTokenRows{}, fmt.Errorf("token pepper is %s, not derived: there is nothing to rewrap under", h.Source())
	}
	return scanTokenHashes(ctx, st, h)
}

// tokenRowReviewed is everything the review shows of a row, plus its hash:
// what a fingerprint or the set digest pins. JSON, so no two different rows
// encode alike whatever their fields hold.
func tokenRowReviewed(r store.OperatorTokenDefRow) []byte {
	at := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format(time.RFC3339Nano)
	}
	scopes := r.AllowedScopes
	if scopes == nil {
		scopes = []string{}
	}
	b, _ := json.Marshal([]any{r.DefID, r.TokenHash, r.TenantID, r.Subject, scopes, at(r.RetiredAt), at(r.ExpiresAt)})
	return b
}

// TokenRowFingerprint pins one reviewed row: hex SHA-256 over its def_id,
// token_hash, tenant, subject, scopes, retired_at and expires_at, cut to 16
// characters. `--def-id <def_id>@<fingerprint>` rewraps the row only if it is
// still exactly what the review showed. 64 bits, so a writer cannot search
// for a hash of their own token that keeps a reviewed row's fingerprint.
func TokenRowFingerprint(r store.OperatorTokenDefRow) string {
	sum := sha256.Sum256(append([]byte("loomcycle/token-rewrap-row/v1\n"), tokenRowReviewed(r)...))
	return hex.EncodeToString(sum[:8])
}

// UntaggedTokenSetDigest names an exact set of unpeppered rows: hex SHA-256
// over each row's reviewed fields (as TokenRowFingerprint pins them), sorted
// by def_id. The review prints it; confirming with it rewraps that set and
// nothing else — a row added, gone or changed since the review gives a
// different digest. The stored hashes are unpeppered SHA-256 of tokens; the
// digest reveals nothing about them a database reader does not already have.
func UntaggedTokenSetDigest(rows []store.OperatorTokenDefRow) string {
	sorted := append([]store.OperatorTokenDefRow(nil), rows...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].DefID < sorted[j].DefID })
	d := sha256.New()
	d.Write([]byte("loomcycle/token-rewrap-set/v2\n"))
	for _, r := range sorted {
		d.Write(tokenRowReviewed(r))
		d.Write([]byte("\n"))
	}
	return hex.EncodeToString(d.Sum(nil))
}

// mintedDefID is the shape every mint writes ("def_" + 16 lowercase hex).
var mintedDefID = regexp.MustCompile(`^def_[0-9a-f]{16}$`)

// SuspiciousTokenRow reports a row no mint writes and the review cannot show
// faithfully: a def_id not of the minted shape (which also catches blank-
// rendering and lookalike characters unicode.IsPrint lets through), or a
// name, tenant, subject or scope holding a non-printable character (a
// control character such as ESC or a newline, a format character such as a
// bidi override) or invalid UTF-8. Such a row can redraw an operator's
// terminal to hide itself or pass for another, so it is never rewrapped.
func SuspiciousTokenRow(r store.OperatorTokenDefRow) bool {
	if !mintedDefID.MatchString(r.DefID) {
		return true
	}
	fields := append([]string{r.Name, r.TenantID, r.Subject}, r.AllowedScopes...)
	for _, f := range fields {
		if !utf8.ValidString(f) {
			return true
		}
		for _, c := range f {
			if !unicode.IsPrint(c) {
				return true
			}
		}
	}
	return false
}

// TokenRowNonASCII reports a name, tenant or subject holding a non-ASCII
// character. Not refused (names may be in any script), but warned about in
// the review: it is where a lookalike would hide.
func TokenRowNonASCII(r store.OperatorTokenDefRow) bool {
	for _, f := range []string{r.Name, r.TenantID, r.Subject} {
		for i := 0; i < len(f); i++ {
			if f[i] >= utf8.RuneSelf {
				return true
			}
		}
	}
	return false
}

// SelectUntaggedTokenRows picks the rows the operator confirmed from the live
// unpeppered rows: each named `<def_id>@<fingerprint>`, or, with digest, the
// whole set if it still digests to what the review printed. Exactly one of
// the two must be given. A name without a fingerprint, a def_id that is not
// a live unpeppered row now, a fingerprint or digest that no longer matches,
// or a suspicious row (SuspiciousTokenRow) refuses the selection outright.
// Neither a new fingerprint nor a new digest is reported: either would
// invite confirming a changed row without looking at it.
func SelectUntaggedTokenRows(live []store.OperatorTokenDefRow, named []string, digest string) ([]store.OperatorTokenDefRow, error) {
	picked, err := selectUntaggedTokenRows(live, named, digest)
	if err != nil {
		return nil, err
	}
	var suspicious []string
	for _, r := range picked {
		if SuspiciousTokenRow(r) {
			suspicious = append(suspicious, strconv.Quote(r.DefID))
		}
	}
	if len(suspicious) > 0 {
		return nil, fmt.Errorf("refusing to rewrap a row no mint writes (an odd def_id, or control or non-printable characters): %s; retire it, and it leaves the rewrap set", strings.Join(suspicious, ", "))
	}
	return picked, nil
}

func selectUntaggedTokenRows(live []store.OperatorTokenDefRow, named []string, digest string) ([]store.OperatorTokenDefRow, error) {
	switch {
	case len(named) > 0 && digest != "":
		return nil, fmt.Errorf("name rows by def_id or by digest, not both")
	case digest != "":
		if UntaggedTokenSetDigest(live) != digest {
			return nil, fmt.Errorf("the unpeppered rows no longer match the reviewed set: a row was added, retired or changed since the review; run the review again")
		}
		return live, nil
	case len(named) > 0:
		byID := make(map[string]store.OperatorTokenDefRow, len(live))
		for _, r := range live {
			byID[r.DefID] = r
		}
		picked := make([]store.OperatorTokenDefRow, 0, len(named))
		seen := make(map[string]bool, len(named))
		var bare, missing, changed []string
		for _, n := range named {
			at := strings.LastIndex(n, "@")
			if at < 0 {
				bare = append(bare, strconv.Quote(n))
				continue
			}
			id, fp := n[:at], n[at+1:]
			if seen[id] {
				continue
			}
			seen[id] = true
			r, ok := byID[id]
			switch {
			case !ok:
				missing = append(missing, strconv.Quote(id))
			case TokenRowFingerprint(r) != fp:
				changed = append(changed, strconv.Quote(id))
			default:
				picked = append(picked, r)
			}
		}
		switch {
		case len(bare) > 0:
			return nil, fmt.Errorf("name each row as <def_id>@<fingerprint>, copied from the review: %s", strings.Join(bare, ", "))
		case len(missing) > 0:
			return nil, fmt.Errorf("not a live unpeppered token row: %s", strings.Join(missing, ", "))
		case len(changed) > 0:
			return nil, fmt.Errorf("changed since the review (the fingerprint no longer matches): %s; run the review again", strings.Join(changed, ", "))
		}
		return picked, nil
	default:
		return nil, fmt.Errorf("no rows named")
	}
}

// RewrapTokenRows moves exactly the given rows onto the wrapped form under
// h's derived pepper, in one transaction, and returns which moved and which
// did not. Each write is a compare-and-set against the hash the row was
// listed with, so a row changed since (by another rewrap running at the same
// time, or a database writer) is left alone rather than wrapped as it now
// is. Safe beside running servers.
func RewrapTokenRows(ctx context.Context, st store.Store, h auth.TokenHasher, rows []store.OperatorTokenDefRow) (moved, notMoved []store.OperatorTokenDefRow, err error) {
	if h.Source() != auth.PepperDerived {
		return nil, nil, fmt.Errorf("token pepper is %s, not derived: there is nothing to rewrap under", h.Source())
	}
	if len(rows) == 0 {
		return nil, nil, nil
	}
	ops := make([]store.OperatorTokenRehashOp, 0, len(rows))
	for _, r := range rows {
		ops = append(ops, store.OperatorTokenRehashOp{DefID: r.DefID, OldHash: r.TokenHash, NewHash: h.Wrap(r.TokenHash)})
	}
	changed, err := st.OperatorTokenRehashAll(ctx, ops)
	if err != nil {
		return nil, nil, err
	}
	for i, r := range rows {
		if i < len(changed) && changed[i] {
			moved = append(moved, r)
		} else {
			notMoved = append(notMoved, r)
		}
	}
	return moved, notMoved, nil
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
			slog.Warn("auth: token rehash onto the current pepper failed", "token_def", row.DefID, "err", rerr)
		case changed && s.authVerbose():
			slog.Info("auth: token rehashed onto the current pepper", "token_def", row.DefID)
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
