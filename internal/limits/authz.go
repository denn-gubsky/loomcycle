package limits

// authz.go holds the RFC AW limit-WRITE authorization + shape validation,
// extracted here so the HTTP (/v1/_limits) and gRPC (TokenLimit) transports
// enforce the IDENTICAL tenant confinement from one place. The rule is subtle
// and security-sensitive (a bug lets a tenant operator write a foreign tenant's
// or the operator-global budget), so it lives once and each transport maps the
// typed result to its own status vocabulary.

// WriteAuthzErr classifies a rejected limit write so each transport renders it
// natively: Forbidden → HTTP 403 / gRPC PermissionDenied (an authz failure);
// otherwise → HTTP 400 / gRPC InvalidArgument (a malformed request).
type WriteAuthzErr struct {
	Forbidden bool
	Msg       string
}

func (e *WriteAuthzErr) Error() string { return e.Msg }

// ValidScope reports whether scope is one of the closed set of budget axes.
func ValidScope(scope string) bool {
	switch scope {
	case "operator", "tenant", "user":
		return true
	}
	return false
}

// ResolveWrite validates a limit write (set/delete) and resolves the
// authoritative (tenantID, scopeID) under RFC AW tenant confinement.
//
//   - operator scope: admin-only (all==true); tenant + scope id forced to "".
//   - tenant scope: scopeID must be "" (the whole tenant).
//   - user scope: scopeID (the subject) required.
//
// callerTenant is the ctx principal's tenant; all reports full authority
// (admin / legacy / open mode). A caller with all==true may target any tenant
// via wireTenant; a scoped caller is confined to callerTenant and a disagreeing
// wireTenant is a cross-tenant attempt (never silently rewritten). The wire
// tenant is authoritative for confinement only for a full-authority caller.
func ResolveWrite(scope, wireTenant, scopeID, callerTenant string, all bool) (tenantID, resolvedScopeID string, err *WriteAuthzErr) {
	if !ValidScope(scope) {
		return "", "", &WriteAuthzErr{Msg: "scope must be one of: operator, tenant, user"}
	}
	switch scope {
	case "operator":
		if !all {
			return "", "", &WriteAuthzErr{Forbidden: true, Msg: "the operator-global budget is admin-only"}
		}
		// operator scope is a single global row; ignore any tenant/scope id.
		return "", "", nil
	case "tenant":
		if scopeID != "" {
			return "", "", &WriteAuthzErr{Msg: "scope_id must be empty for scope=tenant"}
		}
	case "user":
		if scopeID == "" {
			return "", "", &WriteAuthzErr{Msg: "scope_id (the user subject) is required for scope=user"}
		}
	}

	// tenant / user scope: resolve the authoritative tenant.
	if all {
		// Full authority may address any tenant; the wire tenant is authoritative.
		return wireTenant, scopeID, nil
	}
	// Scoped caller: confined to its own tenant. A wire tenant that disagrees is
	// a cross-tenant attempt → forbidden (never silently rewritten to a foreign id).
	if wireTenant != "" && wireTenant != callerTenant {
		return "", "", &WriteAuthzErr{Forbidden: true, Msg: "a tenant operator may only manage budgets in its own tenant"}
	}
	return callerTenant, scopeID, nil
}
