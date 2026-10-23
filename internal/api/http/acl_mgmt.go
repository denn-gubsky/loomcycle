package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC EY P4 — the HTTP management surfaces: user groups, ACL rule lists,
// bindings, and the AuthZEN-shaped evaluate / explain / simulate questions.
//
// Every row is confined to the caller's tenant (admin may focus another with
// ?tenant=, the RFC AS posture); every write bumps the tenant's acl_version
// in the store, drops this replica's cached sets, tells the other replicas,
// and is audited with names and ids, never a bearer.

const aclBodyCap = 1 << 20

// aclReply is one management call's answer in transport-neutral form: the
// HTTP status and the JSON body the route writes. The HTTP handlers write it
// with the helpers they always used, so the wire is unchanged; the gRPC and
// MCP twins carry the body as their result text (acl_twins.go).
type aclReply struct {
	status int
	body   any
}

// aclErrorBody is writeJSONError's body, field for field.
type aclErrorBody struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

func aclOK(body any) aclReply      { return aclReply{http.StatusOK, body} }
func aclCreated(body any) aclReply { return aclReply{http.StatusCreated, body} }
func aclFail(status int, code, msg string) aclReply {
	return aclReply{status, aclErrorBody{Code: code, Error: msg}}
}

// writeACLReply writes a reply the way the handlers wrote each case before
// they shared the logic: an error with writeJSONErrorBody, a 200 with
// writeJSONOK, anything else with writeJSON.
func writeACLReply(w http.ResponseWriter, r aclReply) {
	switch {
	case r.status >= http.StatusBadRequest:
		writeJSONErrorBody(w, r.status, r.body)
	case r.status == http.StatusOK:
		writeJSONOK(w, r.body)
	default:
		writeJSON(w, r.status, r.body)
	}
}

// aclTenant is the tenant a management call acts in.
func (s *Server) aclTenant(r *http.Request) string {
	return s.adminFocusedTenant(r.Context(), r.URL.Query().Get("tenant"))
}

func (s *Server) aclStoreReady(w http.ResponseWriter) bool {
	if r := s.aclNoStore(); r != nil {
		writeACLReply(w, *r)
		return false
	}
	return true
}

// aclEnter is the gate every management and authz operation passes first,
// whichever transport called it — the route, RPC and MCP-tool gates in front
// are not what holds these lines, because a transport that forgot one would
// otherwise reach the logic. It returns the refusal, or nil.
//
//   - Never from inside a run: this is an operator plane, not an agent's
//     tool, and a run's ctx carries the principal that started it.
//   - A credential limited to certain agents is held to the run plane.
//   - Groups, lists and bindings are the tenant operator's (admin and the
//     legacy token included); a member or an isolated user is refused. The
//     authz questions are open to any principal, which the operation then
//     confines to its own subject (authzSubjectSet).
//   - surface, when set, is the family's type-level question — the one its
//     HTTP route and its RPC ask — so the MCP twin answers it too. A write
//     then asks its own verb (aclAsk).
//
// No principal is open mode (or the stdio MCP launcher): the operator.
func (s *Server) aclEnter(ctx context.Context, management bool, surface *authz.Surface) *aclReply {
	if tools.RunID(ctx) != "" {
		r := aclFail(http.StatusForbidden, "operator_plane", "access-control management is an operator plane; it is not reachable from inside a run")
		return &r
	}
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok {
		return nil
	}
	if p.Restrictions.RunPlaneOnly() {
		r := aclFail(http.StatusForbidden, "restricted_token", "this token is limited to starting and following runs of its allowed agents")
		return &r
	}
	if management && !p.Legacy && !auth.HasScope(p.Scopes, auth.ScopeTenant) {
		r := aclReply{http.StatusForbidden, aclScopeBody{Code: "insufficient_scope",
			Error: "groups, access-control lists and bindings are the tenant operator's", RequiredScope: auth.ScopeTenant}}
		return &r
	}
	if surface == nil {
		return nil
	}
	return s.aclAsk(ctx, *surface)
}

// aclAsk asks the principal's set one question and returns the refusal, or
// nil. A question the HTTP route gate already asked and admitted is not
// asked again. No principal (open mode) has no set to ask.
//
// A write asks two: aclEnter the family's weakest verb (what the route and
// the RPC ask), then the write's own verb here, so a principal allowed the
// write but denied the read gets the same answer on every transport.
func (s *Server) aclAsk(ctx context.Context, surface authz.Surface) *aclReply {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok || routeGateAnswered(ctx, surface) {
		return nil
	}
	return s.gateReply(ctx, p, surface, authz.EvaluateSurface(ctx, p.Effective, surface, true))
}

// aclScopeBody is writeScopeError's body, field for field.
type aclScopeBody struct {
	Code          string `json:"code"`
	Error         string `json:"error"`
	RequiredScope string `json:"required_scope"`
}

// The operations' type-level questions: what the HTTP route of each asks
// (authz.HTTPSurface), so a twin is held to the same answer.
// TestACLRouteSurfaces_MatchTheRoutes holds them equal.
func aclReadGroups() *authz.Surface {
	return &authz.Surface{Action: "read", Type: "principal", Name: "group:*"}
}
func aclManageGroups() *authz.Surface {
	return &authz.Surface{Action: "manage", Type: "principal", Name: "group:*"}
}
func aclReadLists() *authz.Surface { return &authz.Surface{Action: "read", Type: "def", Name: "acl/*"} }
func aclAuthorLists() *authz.Surface {
	return &authz.Surface{Action: "author", Type: "def", Name: "acl/*"}
}

// routeGateKey marks the question the HTTP route gate asked and admitted.
type routeGateKey struct{}

// withRouteGate records that the route gate asked surface and let the
// request through, so an operation behind it does not ask the identical
// question twice (one engine call, and one audit row for a shadow
// divergence). Set only by aclGateHTTP, after it admitted.
func withRouteGate(ctx context.Context, surface authz.Surface) context.Context {
	return context.WithValue(ctx, routeGateKey{}, surface)
}

func routeGateAnswered(ctx context.Context, surface authz.Surface) bool {
	asked, ok := ctx.Value(routeGateKey{}).(authz.Surface)
	return ok && asked == surface
}

// aclNoStore is the refusal when no store is wired, or nil.
func (s *Server) aclNoStore() *aclReply {
	if s.store == nil {
		r := aclFail(http.StatusServiceUnavailable, "store_unavailable", "store not configured; access control requires a persistent store")
		return &r
	}
	return nil
}

func decodeACLBody(w http.ResponseWriter, r *http.Request, into any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, aclBodyCap)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", "invalid request body: "+err.Error())
		return false
	}
	return true
}

// aclChanged is every write's epilogue: invalidate the cached sets here and
// on every replica, and record who changed what.
func (s *Server) aclChanged(ctx context.Context, tenant, action, targetName string) {
	if s.aclResolver != nil {
		aclres.Publish(ctx, s.backplane, s.aclResolver, tenant)
	}
	p, _ := auth.PrincipalFromContext(ctx)
	log.Printf("authz: %s tenant=%q target=%q by=%q", action, tenant, targetName, p.Subject)
	if s.tokenAudit == nil {
		return
	}
	ev := audit.Event{Action: action, ActorTenant: p.TenantID, ActorSubject: p.Subject, ActorTokenSuffix: p.TokenSuffix,
		TargetTenant: tenant, TargetName: targetName}
	if err := s.tokenAudit.Record(ev); err != nil {
		log.Printf("authz: audit write failed: %v", err)
	}
}

// aclNoSelfElevation refuses a binding a delegated writer would benefit
// from: to its own subject, its own token, or a group it is in (RFC EY §10
// invariant 4, in its practical form). A tenant operator or admin already
// holds the whole tenant's authority and is exempt; so is a tenant-wide
// binding, which an operator alone may write.
func aclNoSelfElevation(ctx context.Context, kind, id string) error {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok || aclWriterHoldsTenant(p) {
		return nil
	}
	switch authz.SubjectKind(kind) {
	case authz.SubjectUser:
		if id == p.Subject {
			return fmt.Errorf("a binding to your own subject")
		}
	case authz.SubjectToken:
		if id == p.TokenDefID {
			return fmt.Errorf("a binding to your own token")
		}
	case authz.SubjectGroup:
		for _, g := range p.Groups {
			if g == id {
				return fmt.Errorf("a binding to a group you are in")
			}
		}
	case authz.SubjectTenant:
		return fmt.Errorf("a tenant-wide binding")
	}
	return nil
}

// aclWriterHoldsTenant reports whether the writer already holds every
// ACL, group and binding authority in its tenant (RFC EY §3.8): admin, the
// legacy token and a tenant operator. The self-elevation guards bind the
// writers that hold only what an ACL delegates to them. An admin who wants a
// tenant operator unable to lift a list binds it a deny on `author
// def:acl/<name>`, which the per-op check honours.
func aclWriterHoldsTenant(p auth.Principal) bool {
	return p.Legacy || auth.HasScope(p.Scopes, auth.ScopeAdmin) || auth.HasScope(p.Scopes, auth.ScopeTenant)
}

func aclStoreError(err error, kind string) aclReply {
	var nf *store.ErrNotFound
	var conflict *store.ErrConflict
	switch {
	case errors.As(err, &nf):
		return aclFail(http.StatusNotFound, kind+"_not_found", kind+" not found")
	case errors.As(err, &conflict):
		return aclFail(http.StatusConflict, kind+"_exists", err.Error())
	default:
		return aclFail(http.StatusBadRequest, kind+"_invalid", err.Error())
	}
}

// ---- groups ----

type groupRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	ExternalID  string `json:"external_id"`
}

type groupMemberRequest struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

func (s *Server) handleListGroups(w http.ResponseWriter, r *http.Request) {
	if !s.aclStoreReady(w) {
		return
	}
	writeACLReply(w, s.aclGroupList(r.Context(), r.URL.Query().Get("tenant")))
}

func (s *Server) handleUpsertGroup(w http.ResponseWriter, r *http.Request) {
	if !s.aclStoreReady(w) {
		return
	}
	var req groupRequest
	if !decodeACLBody(w, r, &req) {
		return
	}
	writeACLReply(w, s.aclGroupPut(r.Context(), r.URL.Query().Get("tenant"), req))
}

func (s *Server) handleGetGroup(w http.ResponseWriter, r *http.Request) {
	if !s.aclStoreReady(w) {
		return
	}
	writeACLReply(w, s.aclGroupGet(r.Context(), r.URL.Query().Get("tenant"), r.PathValue("name")))
}

func (s *Server) handleGroupEffective(w http.ResponseWriter, r *http.Request) {
	if !s.aclStoreReady(w) {
		return
	}
	writeACLReply(w, s.aclGroupEffective(r.Context(), r.URL.Query().Get("tenant"), r.PathValue("name")))
}

func (s *Server) handleDeleteGroup(w http.ResponseWriter, r *http.Request) {
	if !s.aclStoreReady(w) {
		return
	}
	writeACLReply(w, s.aclGroupDelete(r.Context(), r.URL.Query().Get("tenant"), r.PathValue("name")))
}

func (s *Server) handleAddGroupMember(w http.ResponseWriter, r *http.Request) {
	if !s.aclStoreReady(w) {
		return
	}
	var req groupMemberRequest
	if !decodeACLBody(w, r, &req) {
		return
	}
	writeACLReply(w, s.aclGroupAddMember(r.Context(), r.URL.Query().Get("tenant"), r.PathValue("name"), req))
}

func (s *Server) handleRemoveGroupMember(w http.ResponseWriter, r *http.Request) {
	if !s.aclStoreReady(w) {
		return
	}
	writeACLReply(w, s.aclGroupRemoveMember(r.Context(), r.URL.Query().Get("tenant"), r.PathValue("name"), r.PathValue("kind"), r.PathValue("id")))
}

// The group operations. focus is the caller's `tenant` (the query string on
// HTTP, a field on the twins); adminFocusedTenant honours it for an admin
// only, so every transport confines the same way.

func (s *Server) aclGroupList(ctx context.Context, focus string) aclReply {
	if r := s.aclEnter(ctx, true, aclReadGroups()); r != nil {
		return *r
	}
	if r := s.aclNoStore(); r != nil {
		return *r
	}
	tenant := s.adminFocusedTenant(ctx, focus)
	rows, err := s.store.GroupList(ctx, tenant)
	if err != nil {
		return aclFail(http.StatusInternalServerError, "groups_list_failed", err.Error())
	}
	out := make([]any, 0, len(rows))
	stored := map[string]bool{}
	for _, row := range rows {
		out = append(out, row)
		stored[row.Name] = true
	}
	// Config-declared groups of the tenant, read-only. One the store also
	// has is shown as the stored row (the editable one); the resolver
	// unions the two groups' members.
	if static := s.aclStatic(); static != nil {
		for _, g := range static.Groups {
			if g.Tenant == tenant && !stored[g.Name] {
				stored[g.Name] = true
				out = append(out, aclConfigGroup{TenantID: g.Tenant, Name: g.Name, Description: g.Description,
					ExternalID: g.ExternalID, Members: g.Members, Groups: g.Groups, Source: aclres.SourceConfig})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return aclEntryName(out[i]) < aclEntryName(out[j]) })
	return aclOK(map[string]any{"groups": out})
}

func (s *Server) aclGroupPut(ctx context.Context, focus string, req groupRequest) aclReply {
	if r := s.aclEnter(ctx, true, aclReadGroups()); r != nil {
		return *r
	}
	if r := s.aclAsk(ctx, *aclManageGroups()); r != nil {
		return *r
	}
	if r := s.aclNoStore(); r != nil {
		return *r
	}
	tenant := s.adminFocusedTenant(ctx, focus)
	if r := s.aclGroupExternal(ctx, tenant, strings.TrimSpace(req.Name)); r != nil {
		return *r
	}
	row, err := s.store.GroupPut(ctx, store.GroupRow{TenantID: tenant, Name: strings.TrimSpace(req.Name),
		Description: req.Description, ExternalID: req.ExternalID, CreatedBy: principalSubject(ctx)})
	if err != nil {
		return aclStoreError(err, "group")
	}
	s.aclChanged(ctx, tenant, "group_written", row.Name)
	return aclCreated(row)
}

func (s *Server) aclGroupGet(ctx context.Context, focus, name string) aclReply {
	if r := s.aclEnter(ctx, true, aclReadGroups()); r != nil {
		return *r
	}
	if r := s.aclNoStore(); r != nil {
		return *r
	}
	tenant := s.adminFocusedTenant(ctx, focus)
	row, err := s.store.GroupGet(ctx, tenant, name)
	if err != nil {
		return aclStoreError(err, "group")
	}
	members, err := s.store.GroupMembers(ctx, tenant, name)
	if err != nil {
		return aclFail(http.StatusInternalServerError, "group_members_failed", err.Error())
	}
	graph, err := s.store.GroupGraph(ctx, tenant)
	if err != nil {
		return aclFail(http.StatusInternalServerError, "group_graph_failed", err.Error())
	}
	// contained_in: every group a member of this one is also in.
	return aclOK(map[string]any{"group": row, "members": members, "contained_in": authz.Closure(graph, []string{name})})
}

// aclGroupEffective lists everyone the group holds: its user members and
// those of every group nested in it, at any depth, config-declared members
// included — the resolver merges the config's groups and nesting with the
// store's, so this does too. "groups" is the nested groups walked.
func (s *Server) aclGroupEffective(ctx context.Context, focus, name string) aclReply {
	if r := s.aclEnter(ctx, true, aclReadGroups()); r != nil {
		return *r
	}
	if r := s.aclNoStore(); r != nil {
		return *r
	}
	tenant := s.adminFocusedTenant(ctx, focus)
	static := s.aclStatic()
	staticMembers := map[string][]string{}
	if static != nil {
		for _, g := range static.Groups {
			if g.Tenant == tenant {
				staticMembers[g.Name] = append(staticMembers[g.Name], g.Members...)
			}
		}
	}
	if _, err := s.store.GroupGet(ctx, tenant, name); err != nil {
		var nf *store.ErrNotFound
		if !errors.As(err, &nf) {
			return aclFail(http.StatusInternalServerError, "group_get_failed", err.Error())
		}
		if _, declared := staticMembers[name]; !declared {
			return aclFail(http.StatusNotFound, "group_not_found", "group not found")
		}
	}
	graph, err := s.store.GroupGraph(ctx, tenant)
	if err != nil {
		return aclFail(http.StatusInternalServerError, "group_graph_failed", err.Error())
	}
	_, staticGraph := static.GroupsOf(tenant, "")
	graph = authz.MergeGraph(graph, staticGraph)
	// Walk parent → child. The store refuses cycles at write time; the
	// visited set and the depth cap hold the walk if a graph has one anyway.
	seen := map[string]bool{name: true}
	frontier := []string{name}
	for depth := 0; depth < authz.MaxGroupNestingDepth && len(frontier) > 0; depth++ {
		var next []string
		for _, g := range frontier {
			for _, child := range graph[g] {
				if !seen[child] {
					seen[child] = true
					next = append(next, child)
				}
			}
		}
		frontier = next
	}
	users := map[string]bool{}
	nested := make([]string, 0, len(seen))
	for g := range seen {
		if g != name {
			nested = append(nested, g)
		}
		members, err := s.store.GroupMembers(ctx, tenant, g)
		if err != nil {
			return aclFail(http.StatusInternalServerError, "group_members_failed", err.Error())
		}
		for _, m := range members {
			if m.MemberKind == store.GroupMemberUser {
				users[m.MemberID] = true
			}
		}
		for _, u := range staticMembers[g] {
			users[u] = true
		}
	}
	userList := make([]string, 0, len(users))
	for u := range users {
		userList = append(userList, u)
	}
	sort.Strings(userList)
	sort.Strings(nested)
	return aclOK(map[string]any{"group": name, "users": userList, "groups": nested})
}

// aclStatic is the config-declared access control of the live config (nil
// when none is declared).
func (s *Server) aclStatic() *authz.Static {
	if c := s.cfg(); c != nil {
		return c.ResolvedACL
	}
	return nil
}

func (s *Server) aclGroupDelete(ctx context.Context, focus, name string) aclReply {
	if r := s.aclEnter(ctx, true, aclReadGroups()); r != nil {
		return *r
	}
	if r := s.aclAsk(ctx, *aclManageGroups()); r != nil {
		return *r
	}
	if r := s.aclNoStore(); r != nil {
		return *r
	}
	tenant := s.adminFocusedTenant(ctx, focus)
	ok, err := s.store.GroupDelete(ctx, tenant, name)
	if err != nil {
		return aclFail(http.StatusInternalServerError, "group_delete_failed", err.Error())
	}
	if !ok {
		return aclFail(http.StatusNotFound, "group_not_found", "group not found")
	}
	s.aclChanged(ctx, tenant, "group_deleted", name)
	return aclOK(map[string]any{"deleted": name})
}

func (s *Server) aclGroupAddMember(ctx context.Context, focus, name string, req groupMemberRequest) aclReply {
	if r := s.aclEnter(ctx, true, aclReadGroups()); r != nil {
		return *r
	}
	if r := s.aclAsk(ctx, *aclManageGroups()); r != nil {
		return *r
	}
	if r := s.aclNoStore(); r != nil {
		return *r
	}
	tenant := s.adminFocusedTenant(ctx, focus)
	if r := s.aclGroupExternal(ctx, tenant, name); r != nil {
		return *r
	}
	if req.Kind == store.GroupMemberUser {
		if err := aclNoSelfElevation(ctx, string(authz.SubjectUser), req.ID); err != nil {
			// Adding yourself to a group is the membership form of self-binding.
			return aclFail(http.StatusForbidden, "acl_self_elevation", "refused: adding yourself to a group")
		}
	}
	err := s.store.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: tenant, GroupName: name, MemberKind: req.Kind, MemberID: strings.TrimSpace(req.ID), AddedBy: principalSubject(ctx)})
	if err != nil {
		return aclStoreError(err, "group_member")
	}
	s.aclChanged(ctx, tenant, "group_member_added", name+"/"+req.Kind+":"+req.ID)
	return aclCreated(map[string]any{"group": name, "kind": req.Kind, "id": req.ID})
}

// aclGroupExternal refuses a write to a group an identity provider owns
// (external_id set): its members are managed at the provider, and the store
// documents such a group as read-only locally. The refusal covers the
// membership writes and an upsert over the group, which could otherwise
// clear the mark and then edit the members. A group that does not exist is
// left to the operation, which reports it in its own words.
func (s *Server) aclGroupExternal(ctx context.Context, tenant, name string) *aclReply {
	row, err := s.store.GroupGet(ctx, tenant, name)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return nil
		}
		r := aclFail(http.StatusInternalServerError, "group_get_failed", err.Error())
		return &r
	}
	if row.ExternalID != "" {
		r := aclFail(http.StatusConflict, "group_external", fmt.Sprintf("group %q is synced from an identity provider (external_id %q); manage its members there", name, row.ExternalID))
		return &r
	}
	return nil
}

func (s *Server) aclGroupRemoveMember(ctx context.Context, focus, name, kind, id string) aclReply {
	if r := s.aclEnter(ctx, true, aclReadGroups()); r != nil {
		return *r
	}
	if r := s.aclAsk(ctx, *aclManageGroups()); r != nil {
		return *r
	}
	if r := s.aclNoStore(); r != nil {
		return *r
	}
	tenant := s.adminFocusedTenant(ctx, focus)
	if r := s.aclGroupExternal(ctx, tenant, name); r != nil {
		return *r
	}
	ok, err := s.store.GroupMemberRemove(ctx, tenant, name, kind, id)
	if err != nil {
		return aclFail(http.StatusInternalServerError, "group_member_remove_failed", err.Error())
	}
	if !ok {
		return aclFail(http.StatusNotFound, "group_member_not_found", "no such membership")
	}
	s.aclChanged(ctx, tenant, "group_member_removed", name+"/"+kind+":"+id)
	return aclOK(map[string]any{"group": name, "kind": kind, "id": id, "removed": true})
}

// ---- ACL definitions (the def-family dispatch shape) ----

type aclDefRequest struct {
	Op             string          `json:"op"`
	Name           string          `json:"name"`
	Rules          json.RawMessage `json:"rules,omitempty"`
	Description    string          `json:"description,omitempty"`
	IncludeRetired bool            `json:"include_retired,omitempty"`
}

func (s *Server) handleAclDef(w http.ResponseWriter, r *http.Request) {
	if !s.aclStoreReady(w) {
		return
	}
	var req aclDefRequest
	if !decodeACLBody(w, r, &req) {
		return
	}
	writeACLReply(w, s.aclDefOp(r.Context(), r.URL.Query().Get("tenant"), req))
}

// aclDefOp runs one op of the acldef family. The per-op question comes
// first and fails closed, so an op it does not know is asked author before
// it is refused as unknown.
func (s *Server) aclDefOp(ctx context.Context, focus string, req aclDefRequest) aclReply {
	if r := s.aclEnter(ctx, true, aclReadLists()); r != nil {
		return *r
	}
	if r := s.aclNoStore(); r != nil {
		return *r
	}
	tenant := s.adminFocusedTenant(ctx, focus)
	if r := s.aclListOpCheck(ctx, req.Op, strings.TrimSpace(req.Name)); r != nil {
		return *r
	}
	switch req.Op {
	case "create", "update", "put":
		if len(req.Rules) == 0 {
			return aclFail(http.StatusBadRequest, "missing_rules", "rules are required")
		}
		row, err := s.store.AclDefPut(ctx, store.AclDefRow{TenantID: tenant, Name: strings.TrimSpace(req.Name), Rules: req.Rules, Description: req.Description, CreatedBy: principalSubject(ctx)})
		if err != nil {
			return aclStoreError(err, "acl")
		}
		s.aclChanged(ctx, tenant, "acl_written", row.Name)
		return aclCreated(row)
	case "validate":
		rules, err := authz.ParseRules(req.Rules)
		if err != nil {
			return aclFail(http.StatusBadRequest, "acl_invalid", err.Error())
		}
		return aclOK(map[string]any{"valid": true, "rules": len(rules), "catalog_version": authz.CatalogVersion})
	case "get":
		row, err := s.store.AclDefGet(ctx, tenant, req.Name)
		if err != nil {
			if entry, ok, cerr := s.aclConfigList(ctx, tenant, req.Name); cerr == nil && ok {
				return aclOK(entry)
			}
			return aclStoreError(err, "acl")
		}
		return aclOK(row)
	case "list":
		rows, err := s.store.AclDefList(ctx, tenant, req.IncludeRetired)
		if err != nil {
			return aclFail(http.StatusInternalServerError, "acl_list_failed", err.Error())
		}
		configured, err := s.aclConfigLists(ctx, tenant)
		if err != nil {
			return aclFail(http.StatusServiceUnavailable, "acl_unavailable", "the config-declared lists could not be resolved; retry")
		}
		out := make([]any, 0, len(rows)+len(configured))
		for _, row := range rows {
			out = append(out, row)
		}
		for _, entry := range configured {
			out = append(out, entry)
		}
		sort.SliceStable(out, func(i, j int) bool { return aclEntryName(out[i]) < aclEntryName(out[j]) })
		return aclOK(map[string]any{"acls": out, "builtin": builtinACLs()})
	case "retire":
		ok, err := s.store.AclDefRetire(ctx, tenant, req.Name)
		if err != nil {
			return aclFail(http.StatusInternalServerError, "acl_retire_failed", err.Error())
		}
		if !ok {
			return aclFail(http.StatusNotFound, "acl_not_found", "no live list of that name")
		}
		s.aclChanged(ctx, tenant, "acl_retired", req.Name)
		return aclOK(map[string]any{"retired": req.Name})
	case "catalog":
		return aclOK(map[string]any{"catalog_version": authz.CatalogVersion, "types": catalogTypes(), "transports": authz.Transports})
	default:
		return aclFail(http.StatusBadRequest, "unknown_op", "op must be one of create, get, list, retire, validate, catalog")
	}
}

// aclListOpSurface is the question an op of POST /v1/_acldef asks of the list
// it names: read for a get, retire for a retire, and author for every other op
// — fail closed, so a write op added to the handler later is checked even if
// nobody adds it here. The three ops that name no list and write nothing
// (list, validate, catalog) return nil: the route's type-level read answered
// them.
func aclListOpSurface(op, name string) *authz.Surface {
	action := "author"
	switch op {
	case "list", "validate", "catalog":
		return nil
	case "get":
		action = "read"
	case "retire":
		action = "retire"
	}
	if name == "" {
		name = "*"
	}
	return &authz.Surface{Action: action, Type: "def", Name: "acl/" + name}
}

// aclListOpCheck asks the op's question of the named list, then refuses a
// write that would change the writer's own authority. It returns the
// refusal, or nil. No principal (open mode) has no set to ask.
func (s *Server) aclListOpCheck(ctx context.Context, op, name string) *aclReply {
	p, ok := auth.PrincipalFromContext(ctx)
	surface := aclListOpSurface(op, name)
	if !ok || surface == nil {
		return nil
	}
	if r := s.gateReply(ctx, p, *surface, authz.EvaluateSurface(ctx, p.Effective, *surface, true)); r != nil {
		return r
	}
	if surface.Action == "read" {
		return nil
	}
	if err := aclListNotSelfBound(p, name); err != nil {
		r := aclFail(http.StatusForbidden, "acl_self_elevation", "refused: "+err.Error()+" would change your own authority")
		return &r
	}
	return nil
}

// aclListNotSelfBound refuses a delegated writer a write or retire of a list
// that shapes its own set (RFC EY §10 invariant 4): one bound to its subject,
// its token, a group it is in or its tenant, which is what the compiled set's
// applied bindings are, and a name bound to it that no list has yet, which
// creating would bring into force. Rewriting or retiring such a list could
// drop the very denies that hold the writer back. Like the binding guard it
// leaves a writer that holds the tenant's authority alone (§3.8).
func aclListNotSelfBound(p auth.Principal, name string) error {
	if aclWriterHoldsTenant(p) || p.Effective == nil {
		return nil
	}
	for _, b := range p.Effective.Applied() {
		if b.ACL == name {
			return fmt.Errorf("list %q is bound to you (%s binding); changing it", name, b.SubjectKind)
		}
	}
	for _, missing := range p.Effective.Missing() {
		if missing == name {
			return fmt.Errorf("list %q is bound to you and not yet created; creating it", name)
		}
	}
	return nil
}

// The config-declared entries the management lists include (RFC EY §9):
// read-only, marked "source":"config", in the shape of the stored rows they
// sit beside. A stored row never carries "source".
type aclConfigList struct {
	TenantID string `json:"tenant_id,omitempty"`
	authz.ACL
	Source string `json:"source"`
}

type aclConfigGroup struct {
	TenantID    string   `json:"tenant_id,omitempty"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	ExternalID  string   `json:"external_id,omitempty"`
	Members     []string `json:"members,omitempty"`
	Groups      []string `json:"groups,omitempty"`
	Source      string   `json:"source"`
}

type aclConfigBinding struct {
	TenantID    string     `json:"tenant_id,omitempty"`
	ACL         string     `json:"acl"`
	SubjectKind string     `json:"subject_kind"`
	SubjectID   string     `json:"subject_id,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	Source      string     `json:"source"`
}

// aclEntryName is a list entry's name, stored or config, for sorting.
func aclEntryName(v any) string {
	switch e := v.(type) {
	case store.AclDefRow:
		return e.Name
	case store.GroupRow:
		return e.Name
	case aclConfigList:
		return e.Name
	case aclConfigGroup:
		return e.Name
	}
	return ""
}

// aclConfigLists is the config-declared lists a binding in tenant would
// reach, sorted by name: each the resolver resolves to the config (no live
// stored row of its name in the tenant or the shared tenant shadows it).
func (s *Server) aclConfigLists(ctx context.Context, tenant string) ([]aclConfigList, error) {
	static := s.aclStatic()
	if static == nil || s.aclResolver == nil {
		return nil, nil
	}
	names := make([]string, 0, len(static.ACLs))
	for name := range static.ACLs {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []aclConfigList
	for _, name := range names {
		entry, ok, err := s.aclConfigList(ctx, tenant, name)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, entry)
		}
	}
	return out, nil
}

// aclConfigList is the config-declared list name, when the resolver would
// resolve a binding in tenant to it.
func (s *Server) aclConfigList(ctx context.Context, tenant, name string) (aclConfigList, bool, error) {
	static := s.aclStatic()
	if static == nil || s.aclResolver == nil {
		return aclConfigList{}, false, nil
	}
	a, declared := static.ACLs[name]
	if !declared {
		return aclConfigList{}, false, nil
	}
	src, err := s.aclResolver.ACLSource(ctx, tenant, name)
	if err != nil || src != aclres.SourceConfig {
		return aclConfigList{}, false, err
	}
	return aclConfigList{ACL: a, Source: aclres.SourceConfig}, true, nil
}

func builtinACLs() []authz.ACL {
	bl := authz.Baselines()
	out := make([]authz.ACL, 0, len(bl))
	for _, name := range []string{authz.BaselineAdmin, authz.BaselineTenantOperator, authz.BaselineMember, authz.BaselineIsolated, authz.BaselineRunPlane} {
		out = append(out, bl[name])
	}
	return out
}

func catalogTypes() []map[string]any {
	var out []map[string]any
	for _, name := range authz.Types() {
		spec, _ := authz.TypeSpecOf(name)
		entry := map[string]any{"type": name, "verbs": spec.Verbs}
		if len(spec.Scopes) > 0 {
			entry["scopes"] = spec.Scopes
			if spec.Sep != 0 {
				entry["separator"] = string(spec.Sep)
			}
		}
		out = append(out, entry)
	}
	return out
}

func (s *Server) handleListAclDefNames(w http.ResponseWriter, r *http.Request) {
	if !s.aclStoreReady(w) {
		return
	}
	writeACLReply(w, s.aclDefNames(r.Context(), r.URL.Query().Get("tenant")))
}

func (s *Server) aclDefNames(ctx context.Context, focus string) aclReply {
	if r := s.aclEnter(ctx, true, aclReadLists()); r != nil {
		return *r
	}
	if r := s.aclNoStore(); r != nil {
		return *r
	}
	tenant := s.adminFocusedTenant(ctx, focus)
	rows, err := s.store.AclDefList(ctx, tenant, false)
	if err != nil {
		return aclFail(http.StatusInternalServerError, "acl_list_failed", err.Error())
	}
	configured, err := s.aclConfigLists(ctx, tenant)
	if err != nil {
		return aclFail(http.StatusServiceUnavailable, "acl_unavailable", "the config-declared lists could not be resolved; retry")
	}
	names := make([]string, 0, len(rows)+len(configured))
	for _, row := range rows {
		names = append(names, row.Name)
	}
	for _, entry := range configured {
		names = append(names, entry.Name)
	}
	sort.Strings(names)
	return aclOK(map[string]any{"names": names})
}

// ---- bindings ----

type bindingRequest struct {
	ACL         string          `json:"acl"`
	SubjectKind string          `json:"subject_kind"`
	SubjectID   string          `json:"subject_id,omitempty"`
	ExpiresAt   aclOptionalTime `json:"expires_at,omitempty"`
}

// aclOptionalTime is a request time that may be left empty: "" and null
// mean "not set" (a binding with no expiry), which time.Time refuses to
// decode. Any other value is RFC 3339, as before.
type aclOptionalTime struct{ time.Time }

func (t *aclOptionalTime) UnmarshalJSON(b []byte) error {
	if s := string(b); s == `""` || s == "null" {
		t.Time = time.Time{}
		return nil
	}
	return t.Time.UnmarshalJSON(b)
}

func (s *Server) handleListBindings(w http.ResponseWriter, r *http.Request) {
	if !s.aclStoreReady(w) {
		return
	}
	writeACLReply(w, s.aclBindingList(r.Context(), r.URL.Query().Get("tenant"), r.URL.Query().Get("include_retired") == "1"))
}

func (s *Server) handleCreateBinding(w http.ResponseWriter, r *http.Request) {
	if !s.aclStoreReady(w) {
		return
	}
	var req bindingRequest
	if !decodeACLBody(w, r, &req) {
		return
	}
	writeACLReply(w, s.aclBindingCreate(r.Context(), r.URL.Query().Get("tenant"), req))
}

func (s *Server) handleRetireBinding(w http.ResponseWriter, r *http.Request) {
	if !s.aclStoreReady(w) {
		return
	}
	writeACLReply(w, s.aclBindingRetire(r.Context(), r.URL.Query().Get("tenant"), r.PathValue("id")))
}

func (s *Server) aclBindingList(ctx context.Context, focus string, includeRetired bool) aclReply {
	if r := s.aclEnter(ctx, true, aclReadLists()); r != nil {
		return *r
	}
	if r := s.aclNoStore(); r != nil {
		return *r
	}
	tenant := s.adminFocusedTenant(ctx, focus)
	rows, err := s.store.AclBindingList(ctx, tenant, includeRetired)
	if err != nil {
		return aclFail(http.StatusInternalServerError, "acl_bindings_list_failed", err.Error())
	}
	out := make([]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, row)
	}
	// Config-declared bindings of the tenant, read-only; they apply beside
	// the stored ones (the resolver never shadows a binding).
	for _, b := range s.aclStatic().BindingsIn(tenant) {
		entry := aclConfigBinding{TenantID: tenant, ACL: b.ACL, SubjectKind: string(b.SubjectKind), SubjectID: b.SubjectID, Source: aclres.SourceConfig}
		if !b.ExpiresAt.IsZero() {
			t := b.ExpiresAt.UTC()
			entry.ExpiresAt = &t
		}
		out = append(out, entry)
	}
	return aclOK(map[string]any{"bindings": out})
}

func (s *Server) aclBindingCreate(ctx context.Context, focus string, req bindingRequest) aclReply {
	if r := s.aclEnter(ctx, true, aclReadLists()); r != nil {
		return *r
	}
	if r := s.aclAsk(ctx, *aclAuthorLists()); r != nil {
		return *r
	}
	if r := s.aclNoStore(); r != nil {
		return *r
	}
	if err := aclNoSelfElevation(ctx, req.SubjectKind, req.SubjectID); err != nil {
		return aclFail(http.StatusForbidden, "acl_self_elevation", "refused: "+err.Error()+" would change your own authority")
	}
	tenant := s.adminFocusedTenant(ctx, focus)
	if r := s.aclBindingTargetCheck(ctx, tenant, req); r != nil {
		return *r
	}
	row, err := s.store.AclBindingPut(ctx, store.AclBindingRow{TenantID: tenant, ACLName: req.ACL, SubjectKind: req.SubjectKind,
		SubjectID: req.SubjectID, ExpiresAt: req.ExpiresAt.Time, CreatedBy: principalSubject(ctx)})
	if err != nil {
		return aclStoreError(err, "acl_binding")
	}
	s.aclChanged(ctx, tenant, "binding_written", row.BindingID)
	return aclCreated(row)
}

// aclBindingTargetCheck refuses a binding that could never be in force: one
// whose expiry has passed, and one to a list no layer has — not stored live
// in the tenant or the shared tenant, not declared in config — which the
// resolver would report as missing and match nothing. The lookup is the
// resolver's own (ACLSource), so "missing" here and there cannot differ.
// With no resolver wired no binding is evaluated, and there is nothing to
// check against.
func (s *Server) aclBindingTargetCheck(ctx context.Context, tenant string, req bindingRequest) *aclReply {
	if !req.ExpiresAt.IsZero() && !req.ExpiresAt.After(time.Now()) {
		r := aclFail(http.StatusBadRequest, "acl_binding_invalid", fmt.Sprintf("expires_at %s is in the past: the binding would never be in force", req.ExpiresAt.Format(time.RFC3339)))
		return &r
	}
	if s.aclResolver == nil {
		return nil
	}
	src, err := s.aclResolver.ACLSource(ctx, tenant, strings.TrimSpace(req.ACL))
	if err != nil {
		r := aclFail(http.StatusServiceUnavailable, "acl_unavailable", "the list could not be looked up; retry")
		return &r
	}
	if src == "" {
		r := aclFail(http.StatusBadRequest, "acl_binding_invalid", fmt.Sprintf("list %q does not exist (or is retired): a binding to it would match nothing — create the list first", req.ACL))
		return &r
	}
	return nil
}

func (s *Server) aclBindingRetire(ctx context.Context, focus, id string) aclReply {
	if r := s.aclEnter(ctx, true, aclReadLists()); r != nil {
		return *r
	}
	if r := s.aclAsk(ctx, *aclAuthorLists()); r != nil {
		return *r
	}
	if r := s.aclNoStore(); r != nil {
		return *r
	}
	tenant := s.adminFocusedTenant(ctx, focus)
	ok, err := s.store.AclBindingRetire(ctx, tenant, id)
	if err != nil {
		return aclFail(http.StatusInternalServerError, "acl_binding_retire_failed", err.Error())
	}
	if !ok {
		return aclFail(http.StatusNotFound, "acl_binding_not_found", "no live binding of that id")
	}
	s.aclChanged(ctx, tenant, "binding_retired", id)
	return aclOK(map[string]any{"retired": id})
}

// ---- evaluate / explain / simulate (AuthZEN 1.0 shapes) ----

type authzSubjectRef struct {
	Type string `json:"type,omitempty"`
	ID   string `json:"id,omitempty"`
}

type authzResourceRef struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	Owner string `json:"owner,omitempty"`
}

type authzActionRef struct {
	Name string `json:"name"`
}

type authzContextRef struct {
	Transport   string `json:"transport,omitempty"`
	Interactive bool   `json:"interactive,omitempty"`
	Tier        string `json:"tier,omitempty"`
	UserTier    string `json:"user_tier,omitempty"`
	Agent       string `json:"agent,omitempty"`
	Depth       int    `json:"depth,omitempty"`
}

type authzEvaluationRequest struct {
	Subject  *authzSubjectRef  `json:"subject,omitempty"`
	Action   *authzActionRef   `json:"action,omitempty"`
	Resource *authzResourceRef `json:"resource,omitempty"`
	Context  *authzContextRef  `json:"context,omitempty"`
}

type authzEvaluationsRequest struct {
	authzEvaluationRequest
	Evaluations []authzEvaluationRequest `json:"evaluations"`
	Options     *struct {
		Semantic string `json:"evaluations_semantic,omitempty"`
	} `json:"options,omitempty"`
}

type authzSimulateRequest struct {
	Subject  *authzSubjectRef         `json:"subject,omitempty"`
	ACLs     []authz.ACL              `json:"acls,omitempty"`
	Bindings []authz.Binding          `json:"bindings,omitempty"`
	Requests []authzEvaluationRequest `json:"requests"`
}

type authzDecisionWire struct {
	Decision    bool           `json:"decision"`
	Outcome     string         `json:"outcome"`
	Reasons     []authz.Reason `json:"reasons,omitempty"`
	TenantFloor bool           `json:"tenant_floor,omitempty"`
	Mode        string         `json:"mode"`
}

// decisionWire is the AuthZEN decision: true only when a rule ALLOWED. A
// `no_match` is false here whatever the mode — in compat and shadow the
// scope gate decides such a request, which this surface cannot see, and
// `outcome` says so.
func decisionWire(ctx context.Context, d authz.Decision) authzDecisionWire {
	return authzDecisionWire{Decision: d.Outcome == authz.Allowed, Outcome: d.Outcome.String(), Reasons: d.Reasons, TenantFloor: d.TenantFloor, Mode: string(authz.ModeFrom(ctx))}
}

// authzSubjectSet returns the compiled set to evaluate: the caller's own, or
// — for a tenant operator / admin naming another subject of the tenant —
// that subject's, resolved with the class its user row gives it (member
// when it has none).
func (s *Server) authzSubjectSet(ctx context.Context, tenant string, ref *authzSubjectRef) (*authz.Effective, int, string) {
	p, ok := auth.PrincipalFromContext(ctx)
	if ref == nil || ref.ID == "" || (ok && ref.ID == p.Subject && tenant == p.TenantID) {
		if !ok || p.Effective == nil {
			return nil, http.StatusServiceUnavailable, "the caller's access-control set is not available"
		}
		return p.Effective, 0, ""
	}
	if ok && !p.Legacy && !auth.HasScope(p.Scopes, auth.ScopeTenant) {
		return nil, http.StatusForbidden, "naming another subject needs a tenant operator"
	}
	if s.aclResolver == nil {
		return nil, http.StatusServiceUnavailable, "no access-control resolver is wired"
	}
	scopes := []string{auth.ScopeRunsCreate, auth.ScopeRunsRead, auth.ScopeChannelPublish, auth.ScopeChannelRead}
	if row, err := s.store.UserGet(ctx, tenant, ref.ID); err == nil && row.AccessMode == "isolated" {
		scopes = []string{auth.ScopeUser}
	}
	other, err := s.aclResolver.Resolve(ctx, auth.Principal{TenantID: tenant, Subject: ref.ID, Scopes: scopes})
	if err != nil || other.Effective == nil {
		return nil, http.StatusServiceUnavailable, "the subject's access-control set could not be read"
	}
	return other.Effective, 0, ""
}

func authzRequestFrom(eff *authz.Effective, in authzEvaluationRequest, ctx context.Context) (authz.Request, error) {
	if in.Action == nil || in.Action.Name == "" || in.Resource == nil || in.Resource.Type == "" {
		return authz.Request{}, fmt.Errorf("action.name and resource.type are required")
	}
	if !authz.ValidVerb(in.Resource.Type, in.Action.Name) {
		return authz.Request{}, fmt.Errorf("%q is not a verb of %s", in.Action.Name, in.Resource.Type)
	}
	name := in.Resource.ID
	if name == "" {
		name = authz.AnyInstance
	}
	c := authz.Context{Transport: authz.TransportFrom(ctx), Now: time.Now()}
	if in.Context != nil {
		if in.Context.Transport != "" {
			c.Transport = in.Context.Transport
		}
		c.Interactive, c.Tier, c.UserTier, c.Agent, c.Depth = in.Context.Interactive, in.Context.Tier, in.Context.UserTier, in.Context.Agent, in.Context.Depth
	}
	return authz.Request{Subject: eff.Subject, Action: in.Action.Name, Resource: authz.Resource{Type: in.Resource.Type, Name: name, TenantID: eff.Subject.TenantID, Owner: in.Resource.Owner}, Context: c}, nil
}

func (s *Server) handleAuthzEvaluation(w http.ResponseWriter, r *http.Request) {
	var req authzEvaluationRequest
	if !decodeACLBody(w, r, &req) {
		return
	}
	writeACLReply(w, s.authzEvaluation(r.Context(), r.URL.Query().Get("tenant"), req))
}

func (s *Server) handleAuthzEvaluations(w http.ResponseWriter, r *http.Request) {
	var req authzEvaluationsRequest
	if !decodeACLBody(w, r, &req) {
		return
	}
	writeACLReply(w, s.authzEvaluations(r.Context(), r.URL.Query().Get("tenant"), req))
}

func (s *Server) handleAuthzExplain(w http.ResponseWriter, r *http.Request) {
	var req authzEvaluationRequest
	if !decodeACLBody(w, r, &req) {
		return
	}
	writeACLReply(w, s.authzExplain(r.Context(), r.URL.Query().Get("tenant"), req))
}

func (s *Server) handleAuthzSimulate(w http.ResponseWriter, r *http.Request) {
	var req authzSimulateRequest
	if !decodeACLBody(w, r, &req) {
		return
	}
	writeACLReply(w, s.authzSimulate(r.Context(), r.URL.Query().Get("tenant"), req))
}

func (s *Server) authzEvaluation(ctx context.Context, focus string, req authzEvaluationRequest) aclReply {
	if r := s.aclEnter(ctx, false, nil); r != nil {
		return *r
	}
	eff, code, msg := s.authzSubjectSet(ctx, s.adminFocusedTenant(ctx, focus), req.Subject)
	if code != 0 {
		return aclFail(code, "authz_subject", msg)
	}
	areq, err := authzRequestFrom(eff, req, ctx)
	if err != nil {
		return aclFail(http.StatusBadRequest, "invalid_request", err.Error())
	}
	return aclOK(decisionWire(ctx, eff.Decide(areq)))
}

func (s *Server) authzEvaluations(ctx context.Context, focus string, req authzEvaluationsRequest) aclReply {
	if r := s.aclEnter(ctx, false, nil); r != nil {
		return *r
	}
	eff, code, msg := s.authzSubjectSet(ctx, s.adminFocusedTenant(ctx, focus), req.Subject)
	if code != 0 {
		return aclFail(code, "authz_subject", msg)
	}
	if len(req.Evaluations) == 0 || len(req.Evaluations) > 256 {
		return aclFail(http.StatusBadRequest, "invalid_request", "evaluations must list 1 to 256 entries")
	}
	reqs := make([]authz.Request, 0, len(req.Evaluations))
	for i, e := range req.Evaluations {
		if e.Action == nil {
			e.Action = req.Action
		}
		if e.Resource == nil {
			e.Resource = req.Resource
		}
		if e.Context == nil {
			e.Context = req.Context
		}
		areq, err := authzRequestFrom(eff, e, ctx)
		if err != nil {
			return aclFail(http.StatusBadRequest, "invalid_request", fmt.Sprintf("evaluations[%d]: %v", i, err))
		}
		reqs = append(reqs, areq)
	}
	sem := authz.ExecuteAll
	if req.Options != nil && req.Options.Semantic != "" {
		sem = authz.Semantic(req.Options.Semantic)
	}
	out := make([]authzDecisionWire, 0, len(reqs))
	for _, d := range eff.DecideAll(reqs, sem) {
		out = append(out, decisionWire(ctx, d))
	}
	return aclOK(map[string]any{"evaluations": out})
}

func (s *Server) authzExplain(ctx context.Context, focus string, req authzEvaluationRequest) aclReply {
	if r := s.aclEnter(ctx, false, nil); r != nil {
		return *r
	}
	eff, code, msg := s.authzSubjectSet(ctx, s.adminFocusedTenant(ctx, focus), req.Subject)
	if code != 0 {
		return aclFail(code, "authz_subject", msg)
	}
	areq, err := authzRequestFrom(eff, req, ctx)
	if err != nil {
		return aclFail(http.StatusBadRequest, "invalid_request", err.Error())
	}
	ex := eff.Explain(areq)
	return aclOK(map[string]any{"decision": decisionWire(ctx, ex.Decision), "traces": ex.Traces, "missing_acls": ex.Missing, "applied_bindings": ex.Applied, "groups": eff.Subject.Groups})
}

// authzSimulate evaluates against a DRAFT: the stored lists and bindings the
// subject has now, with the draft's lists added or replaced by name and its
// bindings added. Nothing is written.
func (s *Server) authzSimulate(ctx context.Context, focus string, req authzSimulateRequest) aclReply {
	if r := s.aclEnter(ctx, false, nil); r != nil {
		return *r
	}
	tenant := s.adminFocusedTenant(ctx, focus)
	eff, code, msg := s.authzSubjectSet(ctx, tenant, req.Subject)
	if code != 0 {
		return aclFail(code, "authz_subject", msg)
	}
	if len(req.Requests) == 0 || len(req.Requests) > 256 {
		return aclFail(http.StatusBadRequest, "invalid_request", "requests must list 1 to 256 entries")
	}
	if s.aclResolver == nil {
		return aclFail(http.StatusServiceUnavailable, "authz_subject", "no access-control resolver is wired")
	}
	in, err := s.aclResolver.Input(ctx, eff.Subject)
	if err != nil {
		return aclFail(http.StatusServiceUnavailable, "authz_subject", "the subject's access-control rows could not be read")
	}
	for _, a := range req.ACLs {
		if err := a.Validate(); err != nil {
			return aclFail(http.StatusBadRequest, "acl_invalid", err.Error())
		}
		in.ACLs[a.Name] = authz.ACLVersion{ACL: a, Version: 0}
	}
	for _, b := range req.Bindings {
		if err := b.Validate(); err != nil {
			return aclFail(http.StatusBadRequest, "acl_binding_invalid", err.Error())
		}
		in.Bindings = append(in.Bindings, b)
	}
	draft, err := authz.Compile(in)
	if err != nil {
		return aclFail(http.StatusBadRequest, "acl_invalid", err.Error())
	}
	out := make([]map[string]any, 0, len(req.Requests))
	for i, e := range req.Requests {
		areq, err := authzRequestFrom(draft, e, ctx)
		if err != nil {
			return aclFail(http.StatusBadRequest, "invalid_request", fmt.Sprintf("requests[%d]: %v", i, err))
		}
		now, after := eff.Decide(areq), draft.Decide(areq)
		out = append(out, map[string]any{"request": e, "now": decisionWire(ctx, now), "with_draft": decisionWire(ctx, after), "changed": now.Outcome != after.Outcome})
	}
	return aclOK(map[string]any{"results": out, "missing_acls": draft.Missing()})
}
