package http

// users_crud.go — RFC BX P2a: create / update / delete for the tenant-owned
// first-class `users` table. The GET list (handleListUsers) + per-subject
// inspect (handleInspectUser) live in users.go / directory.go.
//
// Trust boundary: the tenant a row belongs to is ALWAYS derived server-side
// from the principal (tenantFromCtx), NEVER from the request body or the URL.
// A tenant operator therefore manages ONLY its own tenant's users; a
// cross-tenant update/delete is an opaque 404, never an existence oracle.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// createUserRequest is the wire body of POST /v1/_users. `tenant` is
// deliberately NOT a field — the tenant is server-derived.
type createUserRequest struct {
	Subject     string `json:"subject"`
	DisplayName string `json:"display_name"`
	AccessMode  string `json:"access_mode"`
	Status      string `json:"status"`
}

// updateUserRequest is the wire body of PATCH /v1/_users/{subject}. Pointer
// fields so an omitted key leaves the column unchanged; a present key (even
// the empty string) is applied.
type updateUserRequest struct {
	DisplayName *string `json:"display_name"`
	AccessMode  *string `json:"access_mode"`
	Status      *string `json:"status"`
}

// wireUserRecord is the response shape of a single first-class user row.
type wireUserRecord struct {
	TenantID    string    `json:"tenant_id"`
	Subject     string    `json:"subject"`
	DisplayName string    `json:"display_name"`
	AccessMode  string    `json:"access_mode"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	CreatedBy   string    `json:"created_by"`
}

func toWireUserRecord(r store.UserRow) wireUserRecord {
	return wireUserRecord{
		TenantID:    r.TenantID,
		Subject:     r.Subject,
		DisplayName: r.DisplayName,
		AccessMode:  r.AccessMode,
		Status:      r.Status,
		CreatedAt:   r.CreatedAt,
		CreatedBy:   r.CreatedBy,
	}
}

// handleCreateUser serves POST /v1/_users. Registers a first-class user in
// the caller's own tenant. 409 on a duplicate (tenant, subject); 400 on a
// missing subject or a bad access_mode / status.
func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable",
			"store not configured; user management requires a persistent store")
		return
	}
	var req createUserRequest
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", "invalid request body: "+err.Error())
		return
	}
	subject := strings.TrimSpace(req.Subject)
	if subject == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_subject", "subject is required")
		return
	}
	tenant, caller, ok := s.userCrudTenant(w, r)
	if !ok {
		return
	}
	// Apply the RFC BX defaults BEFORE validating so an omitted dial means
	// "full whole-tenant collaboration" (today's behaviour), not a rejection.
	accessMode := strings.TrimSpace(req.AccessMode)
	if accessMode == "" {
		accessMode = "tenant"
		// A website's credential provisions at the floor unless it says
		// otherwise (and may say otherwise only for an empty tenant).
		if caller == gateCallerManager {
			accessMode = "isolated"
		}
	}
	status := strings.TrimSpace(req.Status)
	if status == "" {
		status = "active"
	}
	if !store.ValidUserAccessMode(accessMode) {
		writeJSONError(w, http.StatusBadRequest, "invalid_access_mode",
			"access_mode must be one of tenant|isolated")
		return
	}
	if !store.ValidUserStatus(status) {
		writeJSONError(w, http.StatusBadRequest, "invalid_status",
			"status must be one of active|disabled")
		return
	}
	if caller == gateCallerManager && accessMode == "tenant" && !s.managerMayCreateOwner(w, r, tenant) {
		return
	}
	row := store.UserRow{
		TenantID:    tenant, // the principal's, or the one an admin / auth:manage named
		Subject:     subject,
		DisplayName: strings.TrimSpace(req.DisplayName),
		AccessMode:  accessMode,
		Status:      status,
		CreatedAt:   time.Now().UTC(),
		CreatedBy:   principalSubject(r.Context()),
	}
	// A website's bootstrap create makes the tenant's owner: the user and its
	// tenant-operator binding go in one transaction, which re-checks that the
	// tenant never had a user (owner decision, Q6).
	owner := caller == gateCallerManager && accessMode == "tenant"
	var err error
	if owner {
		err = s.store.UserCreateOwner(r.Context(), row, ownerBinding(row.TenantID, row.Subject, "bootstrap:"+principalSubject(r.Context())))
	} else {
		err = s.store.UserCreate(r.Context(), row)
	}
	if err != nil {
		var conflict *store.ErrConflict
		if errors.As(err, &conflict) && conflict.Kind == "tenant_not_empty" {
			writeJSONError(w, http.StatusForbidden, "tenant_not_empty",
				"an "+auth.ScopeAuthManage+" token may create an access_mode tenant user only in a tenant that never had a user")
			return
		}
		if errors.As(err, &conflict) {
			writeJSONError(w, http.StatusConflict, "user_exists",
				"a user with that subject already exists in this tenant")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "user_create_failed", err.Error())
		return
	}
	if owner {
		s.aclChanged(r.Context(), row.TenantID, "auth_owner_bound", row.Subject)
	}
	// A USER COMING INTO EXISTENCE IS THE ESTABLISHMENT MOMENT, and this is where it
	// happens — not at token mint. Provisioning was originally hooked only to the
	// OperatorTokenDef substrate tool, so a user created through this surface (which is
	// what the Web UI drives) got no profile at all: the template that the Identity
	// section lives in never appeared, so nobody could declare their own names, so
	// placement could not tell a fact about them from a fact about a colleague.
	//
	// Best-effort and after the row lands: creating the user is the operation that must
	// succeed, and a template document is a convenience.
	s.ProvisionIdentityDocs(r.Context(), row.TenantID, row.Subject)
	writeJSON(w, http.StatusCreated, toWireUserRecord(row))
}

// userCrudTenant is the tenant a user create / update / delete acts on. Before
// RFC FA it was always the principal's own; an admin and an auth:manage
// credential may now name another with ?tenant= (auth:manage must). A tenant
// operator's ?tenant= is still ignored, as it always was.
func (s *Server) userCrudTenant(w http.ResponseWriter, r *http.Request) (string, gateCaller, bool) {
	ctx := r.Context()
	switch caller := callerOf(ctx); caller {
	case gateCallerManager, gateCallerAdmin:
		return s.manageTenant(w, ctx, r.URL.Query().Get("tenant"))
	default:
		return tenantFromCtx(ctx), caller, true
	}
}

// managerMayCreateOwner admits an auth:manage credential's access_mode
// tenant user only as the first user of a tenant that never had one, with no
// live owner invite (the same bootstrap rule as an invite: D16). Operator
// tokens alone do not bootstrap a tenant — legacy cloud tenants have them and
// no users row, and their owner is created this way, once.
func (s *Server) managerMayCreateOwner(w http.ResponseWriter, r *http.Request, tenant string) bool {
	has, err := s.tenantBootstrapped(r.Context(), tenant)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return false
	}
	if has {
		writeJSONError(w, http.StatusForbidden, "tenant_not_empty",
			"an "+auth.ScopeAuthManage+" token may create an access_mode tenant user only in a tenant that never had a user")
		return false
	}
	live, err := s.store.AuthInviteLiveBootstrap(r.Context(), tenant, time.Now())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return false
	}
	if live {
		writeJSONError(w, http.StatusConflict, "bootstrap_invite_live",
			"this tenant has a live owner invite; revoke it or let it be redeemed")
		return false
	}
	return true
}

// handleUpdateUser serves PATCH /v1/_users/{subject}. Patches mutable fields
// on a user in the caller's own tenant. A cross-tenant target is an opaque
// 404 (no existence oracle); a bad access_mode / status is a 400.
func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable",
			"store not configured; user management requires a persistent store")
		return
	}
	subject := strings.TrimSpace(r.PathValue("subject"))
	if subject == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_subject", "subject is required")
		return
	}
	var req updateUserRequest
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_body", "invalid request body: "+err.Error())
			return
		}
	}
	if req.AccessMode != nil && !store.ValidUserAccessMode(*req.AccessMode) {
		writeJSONError(w, http.StatusBadRequest, "invalid_access_mode",
			"access_mode must be one of tenant|isolated")
		return
	}
	if req.Status != nil && !store.ValidUserStatus(*req.Status) {
		writeJSONError(w, http.StatusBadRequest, "invalid_status",
			"status must be one of active|disabled")
		return
	}
	tenant, caller, ok := s.userCrudTenant(w, r)
	if !ok {
		return
	}
	// RFC FA D16: auth:manage acts only on users at the floor — it may not
	// re-mode, disable or re-enable a user above it (display_name stays its
	// to fix) — and never promotes. Raising a user's access_mode is the
	// tenant's own operator's call.
	if caller == gateCallerManager && (req.AccessMode != nil || req.Status != nil) {
		if _, ok := s.holdManagerToFloorTarget(w, r.Context(), caller, nil, tenant, subject, "update", false); !ok {
			return
		}
	}
	if caller == gateCallerManager && req.AccessMode != nil {
		cur, err := s.store.UserGet(r.Context(), tenant, subject)
		if err != nil {
			if isNotFound(err) {
				writeJSONError(w, http.StatusNotFound, "user_not_found", "no such user in this tenant")
			} else {
				writeJSONError(w, http.StatusInternalServerError, "user_update_failed", err.Error())
			}
			return
		}
		if accessRank(*req.AccessMode) > accessRank(cur.AccessMode) {
			writeJSONError(w, http.StatusForbidden, "promotion_refused",
				"an "+auth.ScopeAuthManage+" token cannot raise a user's access_mode")
			return
		}
	}
	patch := store.UserPatch{
		DisplayName: req.DisplayName,
		AccessMode:  req.AccessMode,
		Status:      req.Status,
	}
	if err := s.store.UserUpdate(r.Context(), tenant, subject, patch); err != nil {
		var notFound *store.ErrNotFound
		if errors.As(err, &notFound) {
			writeJSONError(w, http.StatusNotFound, "user_not_found", "no such user in this tenant")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "user_update_failed", err.Error())
		return
	}
	// Disabling a user must revoke its live bearer(s) — the auth path is
	// token-only and never checks users.status, so a status flip alone would
	// leave existing tokens working. (Re-enabling does not re-issue tokens; the
	// operator re-mints.)
	if req.Status != nil && *req.Status == "disabled" {
		if _, err := s.retireDelegatedUserTokens(r.Context(), tenant, subject); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "token_retire_failed", err.Error())
			return
		}
	}
	// Re-read so the response reflects the post-patch state.
	updated, err := s.store.UserGet(r.Context(), tenant, subject)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "user_reread_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toWireUserRecord(updated))
}

// handleDeleteUser serves DELETE /v1/_users/{subject}. Hard-deletes a user in
// the caller's own tenant; 404 if no such row. Owned data (runs / sessions /
// memory) is left intact — deleting the identity record is not erasure.
func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable",
			"store not configured; user management requires a persistent store")
		return
	}
	subject := strings.TrimSpace(r.PathValue("subject"))
	if subject == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_subject", "subject is required")
		return
	}
	tenant, caller, ok := s.userCrudTenant(w, r)
	if !ok {
		return
	}
	if _, ok := s.holdManagerToFloorTarget(w, r.Context(), caller, nil, tenant, subject, "delete", false); !ok {
		return
	}
	// Fail-safe ordering: retire the user's delegated tokens BEFORE removing the
	// identity row, so a deleted user's bearer can never outlive the record (the
	// auth path is token-only). A no-op when the user has no tokens / doesn't
	// exist; a retire error aborts the delete so the operator retries rather than
	// stranding live tokens under a gone identity.
	if _, err := s.retireDelegatedUserTokens(r.Context(), tenant, subject); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "token_retire_failed", err.Error())
		return
	}
	// The user's logins and password go first too: a login must never map to a
	// user that no longer exists, and a later user of the same name (an
	// operator-chosen subject) must not inherit the old password.
	if err := s.store.AuthSubjectDelete(r.Context(), tenant, subject); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "user_delete_failed", err.Error())
		return
	}
	removed, err := s.store.UserDelete(r.Context(), tenant, subject)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "user_delete_failed", err.Error())
		return
	}
	if !removed {
		writeJSONError(w, http.StatusNotFound, "user_not_found", "no such user in this tenant")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
