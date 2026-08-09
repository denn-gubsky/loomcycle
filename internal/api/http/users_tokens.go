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
//     FORCED onto every row. The mint handler reads NO body, so there is no
//     path by which a body/query "tenant" can reach the stored row.
//  2. The granted scopes are DERIVED from the target user's access_mode
//     (auth.GrantableUserScopes) — never free-form from the request. A tenant
//     operator cannot mint a token more powerful than the member's dial, and
//     cannot mint an operator/admin scope at all.
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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// mintUserTokenResponse is the show-once mint result. `token` is the plaintext,
// returned exactly here and never retrievable again.
type mintUserTokenResponse struct {
	DefID       string    `json:"def_id"`
	Token       string    `json:"token"` // plaintext — shown ONCE
	TokenSuffix string    `json:"token_suffix"`
	Name        string    `json:"name"`
	Scopes      []string  `json:"scopes"`
	CreatedAt   time.Time `json:"created_at"`
	Warning     string    `json:"warning"`
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
	// Active applies the SAME validity rule as the auth hot path (a row
	// authenticates iff never retired or still inside its grace window) so the UI
	// can gray out a revoked token honestly.
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
	b.WriteString("u-")
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
// was never retired, or its retired_at is still in the future (rotation grace).
func tokenActive(row store.OperatorTokenDefRow) bool {
	return row.RetiredAt.IsZero() || time.Now().Before(row.RetiredAt)
}

// handleMintUserToken serves POST /v1/_users/{subject}/tokens. Mints a bearer
// token for one of the caller's OWN active users, with scopes derived from the
// user's access_mode. Reads NO request body — subject is in the path and every
// other field is server-derived — so a body/query "tenant" or "scopes" has no
// path to the stored row.
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
	row := store.OperatorTokenDefRow{
		DefID:    newTokenDefID(),
		Name:     deriveTokenName(subject),
		TenantID: tenant,  // FORCED — authoritative principal tenant
		Subject:  subject, // the path user
		// Hash with the SAME pepper the auth hot path uses (resolvePrincipal),
		// else the minted token would never resolve.
		TokenHash:     auth.HashToken(s.cfg.Env.OperatorTokenPepper, plaintext),
		AllowedScopes: scopes,
		CreatedAt:     now,
	}
	created, err := s.store.OperatorTokenDefCreate(r.Context(), row)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "token_create_failed", err.Error())
		return
	}
	// RFC L Decision 11: flush the per-replica auth cache (locally + cross-
	// replica) so the fresh token authenticates within one backplane round-trip.
	s.invalidateTokenCache(r.Context())

	// Non-secret correlation only — NEVER the plaintext. The suffix + def_id are
	// safe to log; the minting operator is attributed via principalSubject.
	log.Printf("users: minted token tenant=%q subject=%q def_id=%q suffix=%q scopes=%v by=%q",
		tenant, subject, created.DefID, suffix, scopes, principalSubject(r.Context()))

	writeJSON(w, http.StatusCreated, mintUserTokenResponse{
		DefID:       created.DefID,
		Token:       plaintext,
		TokenSuffix: suffix,
		Name:        created.Name,
		Scopes:      scopes,
		CreatedAt:   created.CreatedAt,
		Warning:     "store this token now — it is shown once and cannot be retrieved later",
	})
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
			Active:    tokenActive(row),
		}
		if !row.RetiredAt.IsZero() {
			ra := row.RetiredAt.UTC()
			m.RetiredAt = &ra
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
	log.Printf("users: revoked token tenant=%q subject=%q def_id=%q by=%q",
		tenant, subject, defID, principalSubject(r.Context()))
	writeJSON(w, http.StatusOK, map[string]any{
		"def_id":     defID,
		"retired_at": now.Format(time.RFC3339Nano),
	})
}
