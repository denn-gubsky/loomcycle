package http

// users_tokens.go — RFC BX P2c: delegated per-user token minting. A
// substrate:tenant operator mints / lists / revokes bearer tokens for its OWN
// users (bounded delegation), reusing the RFC L OperatorTokenDef machinery
// (auth.MintToken + Store.OperatorTokenDefCreate / OperatorTokenDefSetRetiredAt)
// WITHOUT going through the operator-admin OperatorTokenDef tool (that tool is
// deny-by-default and mints arbitrary scopes for any tenant).
//
// TRUST BOUNDARY — a tenant minting bearer tokens for its users. Four
// server-derived properties, none of them from the request body/query:
//
//  1. The tenant is ALWAYS tenantFromCtx(ctx) (the bearer-derived principal),
//     FORCED onto every row. The mint body is decoded strictly into a struct
//     that has no tenant, subject or scope field, so there is no path by
//     which a body/query "tenant" can reach the stored row.
//  2. The granted scopes are DERIVED from the target user's access_mode
//     (auth.GrantableUserScopes) — never free-form from the request. A tenant
//     operator cannot mint a token more powerful than the member's dial, and
//     cannot mint an operator/admin scope at all. An "inherit" token (RFC EY
//     §8) re-derives them the same way on every request instead of once.
//  3. The route is substrate:tenant (the /v1/_users/ prefix gate in
//     requiredScopeFor), so a substrate:user member is auto-denied minting.
//  4. list / revoke operate ONLY on member tokens — a row that belongs to this
//     (tenant, subject) AND holds no admin/tenant scope (isDelegatedUserToken).
//     This is the anti-escalation guard: an operator-admin could mint a
//     substrate:admin/tenant token whose (tenant, subject) collides with a
//     member, and without this a tenant operator could list/revoke that
//     privileged token through the member surface.
//
// The plaintext token crosses the wire exactly once (on mint) and is NEVER
// logged — only its non-secret 6-char suffix + def_id are logged for
// correlation.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// mintUserTokenRequest is the OPTIONAL body of a user-token mint. Every field
// only narrows what the token is or how long it lives; none can name a tenant,
// a subject or a scope. It is decoded strictly: on a mint, a misspelled key
// that was silently dropped would hand back a token that outlives what the
// caller asked for.
type mintUserTokenRequest struct {
	// TTLSeconds is the token's lifetime. Absent = no expiry, unless the
	// operator set a maximum, in which case absent means that maximum.
	TTLSeconds *int64 `json:"ttl_seconds,omitempty"`
	// Name is a label for the token. Absent = derived from the subject. It is
	// stored behind the "u-" prefix every user token's name carries.
	Name string `json:"name,omitempty"`
	// MaxActive keeps at most this many of the user's tokens alive: after the
	// mint, the oldest beyond the count are retired.
	MaxActive *int `json:"max_active,omitempty"`
	// Agents limits the token to starting these agents as top-level runs, and
	// to the run endpoints. A pointer so "absent" (unrestricted) and "[]"
	// (refused) are told apart.
	Agents *[]string `json:"agents,omitempty"`
	// Overrides locks the per-run overrides for runs this token starts:
	// "none", or the list of overrides it may still set.
	Overrides *auth.OverridePolicy `json:"overrides,omitempty"`
	// Rate bounds how often the token may start runs.
	Rate *auth.RateLimit `json:"rate,omitempty"`
	// Retention: "none" makes every run the token starts a no-trace run.
	Retention string `json:"retention,omitempty"`
	// Authority is how the token's scopes are decided (RFC EY §8): "inherit"
	// (its user's CURRENT access_mode, re-read on every request) or "frozen"
	// (the grant of the access_mode at mint). Absent = the deployment's
	// default (LOOMCYCLE_TOKEN_AUTHORITY_INHERIT). Either way the ceiling is
	// the user's own access_mode grant — it names no scope.
	Authority string `json:"authority,omitempty"`
}

// storedAuthority is the authority column value a mint writes: "" for frozen
// (what every token minted before the mode existed holds) or
// auth.AuthorityInherit. requested is what the caller asked for ("" = the
// default); an unknown value is an error.
func (s *Server) storedAuthority(requested string) (string, error) {
	switch requested {
	case "":
		if c := s.cfg(); c != nil && c.Env.TokenAuthorityInherit {
			return auth.AuthorityInherit, nil
		}
		return "", nil
	case auth.AuthorityInherit:
		return auth.AuthorityInherit, nil
	case auth.AuthorityFrozen:
		return "", nil
	}
	return "", fmt.Errorf("authority must be %q or %q", auth.AuthorityInherit, auth.AuthorityFrozen)
}

// restrictions builds the token's restrictions record from the request.
func (r mintUserTokenRequest) restrictions() (auth.Restrictions, error) {
	var out auth.Restrictions
	if r.Agents != nil {
		out.Agents = *r.Agents
	}
	out.Overrides, out.Rate = r.Overrides, r.Rate
	retention, err := auth.ParseRetention(r.Retention)
	if err != nil {
		return out, err
	}
	out.Retention = retention
	return out, out.Validate(r.Agents != nil)
}

// userTokenLabelRe is what a caller may ask a user token to be called. The
// stored name is userTokenNamePrefix + the label, so the label is two
// characters shorter than the 64 a token name allows.
var userTokenLabelRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,62}$`)

// userTokenNamePrefix starts every delegated user token's name; see
// auth.UserTokenNamePrefix for why the namespace is fenced.
const userTokenNamePrefix = auth.UserTokenNamePrefix

// maxMintBodyBytes bounds the mint body: a few short fields and an agent
// allowlist of at most auth.MaxRestrictedAgents names.
const maxMintBodyBytes = 16 << 10

// decodeMintUserTokenRequest reads the optional body. An empty body is the
// zero request.
func decodeMintUserTokenRequest(r *http.Request) (mintUserTokenRequest, error) {
	var req mintUserTokenRequest
	if r.Body == nil {
		return req, nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxMintBodyBytes+1))
	if err != nil {
		return req, err
	}
	if len(raw) > maxMintBodyBytes {
		return req, fmt.Errorf("body too large")
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return req, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return req, err
	}
	return req, nil
}

// SetUserTokenMaxTTL caps the lifetime of a delegated user token
// (LOOMCYCLE_USER_TOKEN_MAX_TTL_SECONDS). Zero = no cap.
func (s *Server) SetUserTokenMaxTTL(d time.Duration) { s.userTokenMaxTTL = d }

// SetTokenAudit wires the sink user-token mints and revocations are recorded
// to. nil = not recorded.
func (s *Server) SetTokenAudit(sink audit.Sink) { s.tokenAudit = sink }

// recordTokenAudit writes one user-token audit record. Best-effort, like every
// other token audit write: it never blocks or fails the operation.
func (s *Server) recordTokenAudit(ctx context.Context, action string, row store.OperatorTokenDefRow) {
	if s.tokenAudit == nil {
		return
	}
	ev := audit.Event{
		Action: action, TargetDefID: row.DefID, TargetName: row.Name,
		TargetTenant: row.TenantID, TargetSubject: row.Subject,
		ActorTenant: tenantFromCtx(ctx), ActorSubject: principalSubject(ctx),
	}
	if action == "create" {
		ev.ScopesAfter = row.AllowedScopes
	}
	if err := s.tokenAudit.Record(ev); err != nil {
		slog.Error("users: token audit write failed", "err", err)
	}
}

// minterExpiry returns when the token that authenticated this request stops
// working; zero when it never does, or when the caller is not a minted token
// (a config-declared principal, the legacy token, open mode).
func (s *Server) minterExpiry(ctx context.Context) (time.Time, error) {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok || p.Legacy || p.TokenDefID == "" || strings.HasPrefix(p.TokenDefID, "cfg:") {
		return time.Time{}, nil
	}
	// A principal that names a token row which cannot be read — missing
	// included — is refused, not treated as unbounded: "I could not find out
	// how long you have" must not become "you have forever".
	row, err := s.store.OperatorTokenDefGet(ctx, p.TokenDefID)
	if err != nil {
		return time.Time{}, err
	}
	// The expiry, and not ValidUntil: a rotated-out token in its grace window
	// also has an end (retired_at), but that end revokes one secret, not the
	// principal — its successor carries on with the same authority. Bounding
	// by it would make a routine rotation refuse every mint the old secret
	// attempts, which is the zero-downtime roll the grace window exists for.
	// An expiry is different: a rotation copies it, so nothing extends it.
	return row.ExpiresAt, nil
}

// boundByMinter holds a new token's end under the minting token's own end.
// expires and minterEnds are zero for "never".
//
// A token must not be able to make a credential that outlives it. Without
// this, a tenant-operator token minted for an hour could spend that hour
// minting user tokens that never expire.
func boundByMinter(expires, minterEnds time.Time, asked bool) (time.Time, error) {
	if minterEnds.IsZero() {
		return expires, nil
	}
	if expires.IsZero() || expires.After(minterEnds) {
		if asked {
			return time.Time{}, errTTLOutlivesMinter
		}
		// Not asked for: the token gets as long as its minter has left.
		return minterEnds, nil
	}
	return expires, nil
}

// errTTLOutlivesMinter refuses a lifetime that would run past the end of the
// token that asked for it.
var errTTLOutlivesMinter = errors.New("ttl_seconds would outlive the token minting it")

// userTokenLifetime resolves a mint's lifetime against the operator's cap.
// Zero = no expiry.
func (s *Server) userTokenLifetime(asked *int64) (time.Duration, error) {
	limit := s.userTokenMaxTTL
	if asked == nil {
		// With a cap set, "no TTL" means the cap: otherwise leaving the field
		// out would be the way around it.
		return limit, nil
	}
	if *asked <= 0 {
		return 0, fmt.Errorf("ttl_seconds must be positive")
	}
	// Bounded before the multiply so a huge value cannot overflow a Duration.
	const maxSeconds = int64(100 * 365 * 24 * 3600)
	if *asked > maxSeconds {
		return 0, fmt.Errorf("ttl_seconds is too large")
	}
	d := time.Duration(*asked) * time.Second
	if limit > 0 && d > limit {
		return 0, fmt.Errorf("ttl_seconds exceeds the maximum of %d", int64(limit/time.Second))
	}
	return d, nil
}

// mintUserTokenResponse is the show-once mint result. `token` is the plaintext,
// returned exactly here and never retrievable again.
type mintUserTokenResponse struct {
	DefID       string    `json:"def_id"`
	Token       string    `json:"token"` // plaintext — shown ONCE
	TokenSuffix string    `json:"token_suffix"`
	Name        string    `json:"name"`
	Scopes      []string  `json:"scopes"`
	CreatedAt   time.Time `json:"created_at"`
	// ExpiresAt is set when the token has a fixed lifetime.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// Retired lists the older tokens of this user that max_active retired.
	Retired []string `json:"retired,omitempty"`
	// Restrictions is what narrows the token below its scopes, when anything
	// does.
	Restrictions *auth.Restrictions `json:"restrictions,omitempty"`
	// Authority is "inherit" when the token takes its user's current
	// access_mode on each request (Scopes is then the grant at mint);
	// absent for a frozen token.
	Authority string `json:"authority,omitempty"`
	Warning   string `json:"warning"`
}

// userTokenMeta is one token row in the list response — METADATA ONLY. It never
// carries the plaintext (unrecoverable after mint) nor the token_hash
// (json:"-" on the row, but re-stated here so the shape is auditable). Note
// there is no token_suffix: RFC L never persisted it (it is derived from the
// plaintext at mint time), so the list cannot show it — def_id is the stable
// handle for revoke, and the suffix appears only in the one-time mint modal.
type userTokenMeta struct {
	DefID     string     `json:"def_id"`
	Name      string     `json:"name"`
	Scopes    []string   `json:"scopes"`
	CreatedAt time.Time  `json:"created_at"`
	RetiredAt *time.Time `json:"retired_at,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// Restrictions is the token's restrictions record, when it has one.
	Restrictions *auth.Restrictions `json:"restrictions,omitempty"`
	// Authority is "inherit" for a token whose scopes follow its user's
	// current access_mode (Scopes is then the grant at mint); absent for a
	// frozen token.
	Authority string `json:"authority,omitempty"`
	// Active applies the SAME validity rule as the auth hot path (a row
	// authenticates iff it is not retired, or still inside its grace window,
	// and not expired) so the UI can gray out a dead token honestly.
	Active bool `json:"active"`
}

type listUserTokensResponse struct {
	Subject string          `json:"subject"`
	Tokens  []userTokenMeta `json:"tokens"`
}

// newTokenDefID mints a fresh opaque def_id for a token row (same shape as
// mintDefID in the builtin package — "def_" + 16 hex chars = 64 bits — but the
// http package can't reach that unexported helper, so it's restated here).
func newTokenDefID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "def_" + hex.EncodeToString(b[:])
}

// deriveTokenName produces an operatorTokenNameRe-valid label from a user
// subject. A P2a user subject is unconstrained (handleCreateUser validates only
// access_mode/status, not the subject charset), so a subject may contain
// characters the token-name charset forbids; we prefix "u-" and keep only
// [a-zA-Z0-9_-], truncating to the 64-char limit. The name is a human-facing
// LABEL, not a uniqueness key (operator_token_defs has UNIQUE(token_hash) only,
// no UNIQUE(name)), so a lossy sanitize that collides across two odd subjects is
// harmless — each mint is still a distinct def_id + token_hash, and every
// listing/revoke is keyed on (tenant, subject), never on name.
func deriveTokenName(subject string) string {
	var b strings.Builder
	b.WriteString(userTokenNamePrefix)
	for _, r := range subject {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		}
	}
	name := b.String()
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

// isDelegatedUserToken reports whether row is a token this member surface may
// manage: it must belong to the caller's tenant AND the named user, AND hold no
// privileged (admin/tenant) scope. The last clause is the anti-escalation guard
// (property 4 above). A member token only ever carries the bounded
// GrantableUserScopes set, so "no admin/tenant scope" confines list/revoke to
// exactly the tokens this surface can itself mint. auth.HasScope(scopes, X)
// treats substrate:admin as satisfying everything, so a token holding admin is
// caught by the ScopeAdmin check and a token holding tenant (or admin) by the
// ScopeTenant check.
func isDelegatedUserToken(row store.OperatorTokenDefRow, tenant, subject string) bool {
	if row.TenantID != tenant || row.Subject != subject {
		return false
	}
	return !auth.HasScope(row.AllowedScopes, auth.ScopeAdmin) &&
		!auth.HasScope(row.AllowedScopes, auth.ScopeTenant)
}

// tokenActive applies the auth-layer validity rule: a row authenticates iff it
// was never retired, or its retired_at is still in the future (rotation
// grace), and it has not expired.
func tokenActive(row store.OperatorTokenDefRow) bool {
	return row.ValidAt(time.Now())
}

// retireUserTokensBeyond retires the oldest active delegated tokens of
// (tenant, subject) so that at most keep remain, and returns their def ids.
// The caller invalidates the token cache.
func (s *Server) retireUserTokensBeyond(ctx context.Context, tenant, subject string, keep int) ([]string, error) {
	rows, err := s.store.OperatorTokenDefListBySubject(ctx, tenant, subject)
	if err != nil {
		return nil, err
	}
	var active []store.OperatorTokenDefRow
	for _, row := range rows {
		if isDelegatedUserToken(row, tenant, subject) && tokenActive(row) {
			active = append(active, row)
		}
	}
	// Newest first, by creation time and then id: the listing's order is not
	// relied on, because which tokens die is decided here.
	sort.SliceStable(active, func(i, j int) bool {
		if !active[i].CreatedAt.Equal(active[j].CreatedAt) {
			return active[i].CreatedAt.After(active[j].CreatedAt)
		}
		return active[i].DefID > active[j].DefID
	})
	var retired []string
	now := time.Now().UTC()
	for i := keep; i < len(active); i++ {
		if err := s.store.OperatorTokenDefSetRetiredAt(ctx, active[i].DefID, now); err != nil {
			return retired, err
		}
		retired = append(retired, active[i].DefID)
		s.recordTokenAudit(ctx, "retire", active[i])
	}
	return retired, nil
}

// retireDelegatedUserTokens retires every currently-active DELEGATED member
// token for (tenant, subject) and invalidates the token cache. Called when a
// user is DELETED or DISABLED, or ERASED, so their bearer(s) stop
// authenticating immediately — the auth hot path (resolvePrincipalUncached) is
// token-only and never consults users.status, so without this a
// disabled/deleted user's token would keep working until its own retired_at.
// Only DELEGATED member tokens are touched (isDelegatedUserToken): an
// admin/tenant-operator token that happens to share the subject is a different
// role and is left to the operator-token surface. Returns the rows retired —
// on an error too, and the cache is flushed for those, so a retire that
// faulted part-way does not leave the ones it did retire authenticating from
// the cache.
func (s *Server) retireDelegatedUserTokens(ctx context.Context, tenant, subject string) ([]store.OperatorTokenDefRow, error) {
	rows, err := s.store.OperatorTokenDefListBySubject(ctx, tenant, subject)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	var retired []store.OperatorTokenDefRow
	defer func() {
		if len(retired) > 0 {
			s.invalidateTokenCache(ctx)
		}
	}()
	for _, row := range rows {
		if !isDelegatedUserToken(row, tenant, subject) || !tokenActive(row) {
			continue
		}
		if err := s.store.OperatorTokenDefSetRetiredAt(ctx, row.DefID, now); err != nil {
			return retired, err
		}
		retired = append(retired, row)
	}
	return retired, nil
}

// erasureSubjectTokens splits (tenant, subject)'s token rows by the rule
// retireDelegatedUserTokens applies: the member tokens an erasure revokes,
// active or not, and the rest, which it leaves.
func (s *Server) erasureSubjectTokens(ctx context.Context, tenant, subject string) (revocable, kept []store.OperatorTokenDefRow, err error) {
	rows, err := s.store.OperatorTokenDefListBySubject(ctx, tenant, subject)
	if err != nil {
		return nil, nil, err
	}
	for _, row := range rows {
		if isDelegatedUserToken(row, tenant, subject) {
			revocable = append(revocable, row)
		} else {
			kept = append(kept, row)
		}
	}
	return revocable, kept, nil
}

// handleMintUserToken serves POST /v1/_users/{subject}/tokens. Mints a bearer
// token for one of the caller's OWN active users, with scopes derived from the
// user's access_mode. The subject is in the path and the tenant and scopes are
// server-derived; the optional body (mintUserTokenRequest) can only shorten
// the token's life, label it, and cap how many of the user's tokens stay
// alive.
func (s *Server) handleMintUserToken(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable",
			"store not configured; token minting requires a persistent store")
		return
	}
	subject := strings.TrimSpace(r.PathValue("subject"))
	if subject == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_subject", "subject is required")
		return
	}
	// tenant is FORCED from the authenticated principal — never the wire.
	tenant := tenantFromCtx(r.Context())
	// A token row needs a tenant, and a caller with none (open mode, or an
	// admin principal declared without one) has none to give it. The store
	// refused that as a 500; it is the caller's state, so it is a 400.
	if tenant == "" {
		writeJSONError(w, http.StatusBadRequest, "tenant_required",
			"user tokens are minted inside a tenant, and this caller has none; call with a token of the user's tenant")
		return
	}

	req, err := decodeMintUserTokenRequest(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", "invalid request body: "+err.Error())
		return
	}
	lifetime, err := s.userTokenLifetime(req.TTLSeconds)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_ttl", err.Error())
		return
	}
	name := deriveTokenName(subject)
	if req.Name != "" {
		if !userTokenLabelRe.MatchString(req.Name) {
			writeJSONError(w, http.StatusBadRequest, "invalid_name", "name must match [a-zA-Z0-9_-]{1,62}")
			return
		}
		name = userTokenNamePrefix + req.Name
	}
	if req.MaxActive != nil && *req.MaxActive < 1 {
		writeJSONError(w, http.StatusBadRequest, "invalid_max_active", "max_active must be at least 1")
		return
	}
	authority, err := s.storedAuthority(req.Authority)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_authority", err.Error())
		return
	}
	restrictions, err := req.restrictions()
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_restrictions", err.Error())
		return
	}
	// Held to the minting token's own restrictions, the stricter of each, as
	// its lifetime is held to the minter's (boundByMinter): a no-trace or
	// rate-limited operator token must not mint itself a way out.
	if p, ok := auth.PrincipalFromContext(r.Context()); ok {
		if restrictions, err = restrictions.Within(p.Restrictions); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_restrictions", err.Error())
			return
		}
	}
	restrictionsJSON, err := auth.MarshalRestrictions(restrictions)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "mint_failed", err.Error())
		return
	}

	// The target must be a registered member of THIS tenant. A cross-tenant (or
	// unregistered) subject is an opaque 404 — no existence oracle.
	user, err := s.store.UserGet(r.Context(), tenant, subject)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			writeJSONError(w, http.StatusNotFound, "user_not_found", "no such user in this tenant")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "user_lookup_failed", err.Error())
		return
	}
	if user.Status != "active" {
		writeJSONError(w, http.StatusConflict, "user_disabled",
			"cannot mint a token for a disabled user")
		return
	}

	// Scopes are DERIVED from the member's access_mode, never from the request.
	scopes, err := auth.GrantableUserScopes(user.AccessMode)
	if err != nil {
		// access_mode is enum-validated on every write, so reaching here is a
		// store invariant break, not a client error — fail closed (500), never
		// mint an unbounded token.
		writeJSONError(w, http.StatusInternalServerError, "grant_derivation_failed", err.Error())
		return
	}

	plaintext, suffix, err := auth.MintToken()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "mint_failed", err.Error())
		return
	}
	now := time.Now().UTC()
	// Read fresh, not from the principal: the bound has to hold against what
	// the store says now. A failed read refuses the mint rather than issuing a
	// token nothing bounds.
	minterEnds, err := s.minterExpiry(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "mint_failed", "reading the minting token: "+err.Error())
		return
	}
	var expiresAt time.Time
	if lifetime > 0 {
		expiresAt = now.Add(lifetime)
	}
	if expiresAt, err = boundByMinter(expiresAt, minterEnds, req.TTLSeconds != nil); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_ttl", err.Error())
		return
	}
	// Hash with the SAME hasher the auth hot path uses (resolvePrincipal),
	// else the minted token would never resolve.
	hasher, err := s.hasher()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "mint_failed", err.Error())
		return
	}
	row := store.OperatorTokenDefRow{
		DefID:         newTokenDefID(),
		Name:          name,
		TenantID:      tenant,  // FORCED — authoritative principal tenant
		Subject:       subject, // the path user
		TokenHash:     hasher.Hash(plaintext),
		AllowedScopes: scopes,
		CreatedAt:     now,
	}
	row.ExpiresAt = expiresAt
	row.RestrictionsJSON = restrictionsJSON
	row.Authority = authority
	created, err := s.store.OperatorTokenDefCreate(r.Context(), row)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "token_create_failed", err.Error())
		return
	}
	s.recordTokenAudit(r.Context(), "create", created)
	// After the create, so the new token is among the ones kept. A failure
	// here leaves a valid new token and too many old ones, and is reported:
	// the caller asked for a bound and did not get it.
	var retired []string
	if req.MaxActive != nil {
		retired, err = s.retireUserTokensBeyond(r.Context(), tenant, subject, *req.MaxActive)
		if err != nil {
			s.invalidateTokenCache(r.Context())
			writeJSONError(w, http.StatusInternalServerError, "token_retire_failed",
				"the token was minted (def_id "+created.DefID+") but older tokens could not be retired: "+err.Error())
			return
		}
	}
	// RFC L Decision 11: flush the per-replica auth cache (locally + cross-
	// replica) so the fresh token authenticates within one backplane round-trip.
	s.invalidateTokenCache(r.Context())

	// Also here, and not only in handleCreateUser. This route writes the token row
	// DIRECTLY rather than going through the OperatorTokenDef tool, so it shares no code
	// with the other mint path — and it is reachable for a user that already existed
	// before provisioning shipped, which is the case handleCreateUser cannot cover.
	// Idempotent, so the overlap with a just-created user costs one exists-check.
	s.ProvisionIdentityDocs(r.Context(), tenant, subject)

	// Non-secret correlation only — NEVER the plaintext. The suffix + def_id are
	// safe to log; the minting operator is attributed via principalSubject.
	slog.Info("users: minted token", "tenant", tenant, "subject", subject, "def_id", created.DefID, "suffix", suffix,
		"scopes", scopes, "inherit", created.Authority == auth.AuthorityInherit,
		"expires", !created.ExpiresAt.IsZero(), "restrictions", restrictions.Describe(),
		"retired", len(retired), "by", principalSubject(r.Context()))

	resp := mintUserTokenResponse{
		DefID:       created.DefID,
		Token:       plaintext,
		TokenSuffix: suffix,
		Name:        created.Name,
		Scopes:      scopes,
		CreatedAt:   created.CreatedAt,
		Retired:     retired,
		Authority:   created.Authority,
		Warning:     "store this token now — it is shown once and cannot be retrieved later",
	}
	if !created.ExpiresAt.IsZero() {
		ea := created.ExpiresAt.UTC()
		resp.ExpiresAt = &ea
	}
	if !restrictions.IsZero() {
		resp.Restrictions = &restrictions
	}
	writeJSON(w, http.StatusCreated, resp)
}

// handleListUserTokens serves GET /v1/_users/{subject}/tokens. Returns METADATA
// ONLY for the caller's own tenant + the named user's member tokens — never any
// secret material.
func (s *Server) handleListUserTokens(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable",
			"store not configured; token listing requires a persistent store")
		return
	}
	subject := strings.TrimSpace(r.PathValue("subject"))
	if subject == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_subject", "subject is required")
		return
	}
	tenant := tenantFromCtx(r.Context())
	rows, err := s.store.OperatorTokenDefListBySubject(r.Context(), tenant, subject)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "token_list_failed", err.Error())
		return
	}
	out := make([]userTokenMeta, 0, len(rows))
	for _, row := range rows {
		// Show ONLY member tokens (property 4): a privileged token that collides
		// on (tenant, subject) is not this surface's to reveal.
		if !isDelegatedUserToken(row, tenant, subject) {
			continue
		}
		m := userTokenMeta{
			DefID:     row.DefID,
			Name:      row.Name,
			Scopes:    row.AllowedScopes,
			CreatedAt: row.CreatedAt.UTC(),
			Authority: row.Authority,
			Active:    tokenActive(row),
		}
		if !row.RetiredAt.IsZero() {
			ra := row.RetiredAt.UTC()
			m.RetiredAt = &ra
		}
		if !row.ExpiresAt.IsZero() {
			ea := row.ExpiresAt.UTC()
			m.ExpiresAt = &ea
		}
		// A record that cannot be read is shown as nothing here; the token
		// itself is refused at login (resolvePrincipalUncached).
		if rs, perr := auth.ParseRestrictions(row.RestrictionsJSON); perr == nil && !rs.IsZero() {
			m.Restrictions = &rs
		}
		out = append(out, m)
	}
	writeJSON(w, http.StatusOK, listUserTokensResponse{Subject: subject, Tokens: out})
}

// handleRevokeUserToken serves DELETE /v1/_users/{subject}/tokens/{def_id}.
// Retires (immediate) one of the caller's own users' member tokens. A def_id
// that is not this (tenant, subject)'s member token — cross-tenant, another
// user's, or a privileged token colliding on the pair — is an opaque 404
// (def_ids are returned to callers / logged, so not secret; the 404 gives no
// existence oracle and prevents guessing a privileged def_id to disable it).
func (s *Server) handleRevokeUserToken(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable",
			"store not configured; token revocation requires a persistent store")
		return
	}
	subject := strings.TrimSpace(r.PathValue("subject"))
	defID := strings.TrimSpace(r.PathValue("def_id"))
	if subject == "" || defID == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_param", "subject and def_id are required")
		return
	}
	tenant := tenantFromCtx(r.Context())
	row, err := s.store.OperatorTokenDefGet(r.Context(), defID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			writeJSONError(w, http.StatusNotFound, "token_not_found", "no such token for this user")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "token_lookup_failed", err.Error())
		return
	}
	if !isDelegatedUserToken(row, tenant, subject) {
		// Opaque — do not distinguish "wrong tenant" / "wrong user" / "privileged".
		writeJSONError(w, http.StatusNotFound, "token_not_found", "no such token for this user")
		return
	}
	now := time.Now().UTC()
	if err := s.store.OperatorTokenDefSetRetiredAt(r.Context(), defID, now); err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			writeJSONError(w, http.StatusNotFound, "token_not_found", "no such token for this user")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "token_revoke_failed", err.Error())
		return
	}
	// Stop the revoked token authenticating within one backplane round-trip.
	s.invalidateTokenCache(r.Context())
	s.recordTokenAudit(r.Context(), "retire", row)
	slog.Info("users: revoked token", "tenant", tenant, "subject", subject, "def_id", defID, "by", principalSubject(r.Context()))
	writeJSON(w, http.StatusOK, map[string]any{
		"def_id":     defID,
		"retired_at": now.Format(time.RFC3339Nano),
	})
}
