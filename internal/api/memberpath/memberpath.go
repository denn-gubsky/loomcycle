// Package memberpath is the member path (RFC CB) shared by the transports:
// which tenant surfaces a NON-ISOLATED principal without substrate:tenant (a
// "tenant member") may reach, and which stay the tenant operator's.
//
// It exists so the HTTP route gate and the gRPC RPC gate answer that question
// from one source. The operator-only list (CarvedOut) is written once, in HTTP
// route terms, and the gRPC gate reaches it through each RPC's HTTP twin
// (Twins) rather than through a second hand-written list of RPC names — a list
// that would drift from the routes the first time someone carved out a new
// one.
//
// What this package does not decide: whether the caller is a member at all
// (auth.IsIsolated, read by the caller), the scope each transport requires of a
// route or an RPC (requiredScopeFor / grpcConsumerScopes), and the per-handler
// confinement a member is held to once admitted (tenant, own subject).
package memberpath

import (
	"net/http"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// Route reports whether the member path admits a request to a surface whose
// required scope is required: it opens exactly the substrate:tenant surfaces,
// minus the operator-control carve-outs. A surface at substrate:admin
// (cross-tenant enumeration, runtime admin, token minting) is not opened —
// a member is not an operator of anything.
func Route(required, method, path string) bool {
	return required == auth.ScopeTenant && !CarvedOut(method, path)
}

// CarvedOut lists the substrate:tenant routes that STAY operator-only (require
// a real substrate:tenant token) even for a non-isolated member — the operator
// CONTROLS, not member data (RFC CB §Scope). Opening these to a member would be
// a privilege / cost-control / destruction hazard:
//   - user create + roster mutation + user-token minting (/v1/_users*) — escalation
//   - per-subject erasure (/v1/_erasure) — destructive
//   - budget WRITES (PUT/DELETE /v1/_limits) — cost-control bypass (GET stays open)
func CarvedOut(method, path string) bool {
	switch {
	case path == "/v1/_users" && method == http.MethodPost:
		return true
	case strings.HasPrefix(path, "/v1/_users/"):
		return true
	case path == "/v1/_erasure":
		return true
	case path == "/v1/_limits" && (method == http.MethodPut || method == http.MethodDelete):
		return true
	// RFC EY: who may do what is the operator's to write and to read; a
	// member learns its own reach from /v1/_me and /v1/_authz/explain.
	case IsACLManagementPath(path):
		return true
	// Switching storage back on for a no-trace agent is the operator's call:
	// a member who could do it could read what its colleagues' runs said.
	// Reading the list stays operator-only too; it names what is being
	// debugged.
	case path == "/v1/_retention/overrides":
		return true
	// RFC CD Part C — the memory/document CHANGE FEED is substrate:tenant-only,
	// NOT member-accessible (locked decision): the feed exposes the tenant's
	// write pattern (a metadata side-channel) and is operator observability, not
	// member data. Both the /v1/_memory/ prefix (ScopeTenant) and the
	// /v1/_document/changes route (ScopeTenant via isTenantConfinedDefPath) would
	// otherwise be member-accessible; carve them out so a non-isolated member is
	// refused.
	case path == "/v1/_memory/changes" || path == "/v1/_document/changes":
		return true
	// Crypto with the tenant's keys: a member who could decrypt with them could
	// read whatever the tenant sealed, and sign as the tenant. Operators only.
	case path == "/v1/_crypto":
		return true
	}
	return false
}

// IsACLManagementPath matches the RFC EY management routes: groups, ACL rule
// lists and bindings.
func IsACLManagementPath(path string) bool {
	for _, fam := range []string{"/v1/_groups", "/v1/_acldef", "/v1/_aclbinding"} {
		if path == fam || strings.HasPrefix(path, fam+"/") {
			return true
		}
	}
	return false
}
