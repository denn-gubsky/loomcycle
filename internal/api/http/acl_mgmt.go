package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/connector"
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

// aclIntent records a management write before it is made (recordIntent):
// a write whose record cannot be written is refused.
func (s *Server) aclIntent(ctx context.Context, tenant, action, targetName string) (changeIntent, *aclReply) {
	ev, err := s.recordIntent(ctx, audit.Event{Action: action, TargetTenant: tenant, TargetName: targetName})
	if err != nil {
		r := aclAuditUnavailable()
		return ev, &r
	}
	return ev, nil
}

// aclPublished is a recorded management write's epilogue: invalidate the
// cached sets here and on every replica (its record was written ahead).
func (s *Server) aclPublished(ctx context.Context, tenant, action, targetName string) {
	if s.aclResolver != nil {
		aclres.Publish(ctx, s.backplane, s.aclResolver, tenant)
	}
	p, _ := auth.PrincipalFromContext(ctx)
	slog.Info("authz: "+action, "tenant", tenant, "target", targetName, "by", p.Subject)
}

// aclChanged is the epilogue of a write made as part of another operation
// (a sign-in's group sync, an invite, a user's removal): invalidate the
// cached sets and record who changed what, best-effort.
func (s *Server) aclChanged(ctx context.Context, tenant, action, targetName string) {
	s.aclPublished(ctx, tenant, action, targetName)
	p, _ := auth.PrincipalFromContext(ctx)
	if s.tokenAudit == nil {
		return
	}
	ev := audit.Event{Action: action, ActorTenant: p.TenantID, ActorSubject: p.Subject, ActorTokenSuffix: p.TokenSuffix,
		TargetTenant: tenant, TargetName: targetName}
	if err := s.tokenAudit.Record(ev); err != nil {
		slog.Error("authz: audit write failed", "err", err)
	}
}

// aclRetireLists asks retire def:acl/<name> of each list, the question the
// documented lock on a list answers (a deny on author/retire def:acl/<name>).
// A binding, a group and a membership in a group each carry their lists'
// rules to a principal, so dropping one lifts those rules as surely as
// retiring the list: the same lock holds it, for a tenant operator too. It
// replaces the self-elevation guards that exempted every tenant operator
// and so could never fire behind the management gate (re-audit B2, B15).
func (s *Server) aclRetireLists(ctx context.Context, names []string) *aclReply {
	for _, name := range names {
		if r := s.aclAsk(ctx, authz.Surface{Action: "retire", Type: "def", Name: "acl/" + name}); r != nil {
			return r
		}
	}
	return nil
}

// aclGroupLists names, sorted, the lists bound (stored and config-declared,
// live) to group or to any group that contains it, at any depth: a member
// that leaves group leaves each of those groups through it, and deleting
// group takes every member out of them. A binding to a group whose name is
// not in the graph still counts: the store keeps bindings to a deleted group.
func (s *Server) aclGroupLists(ctx context.Context, tenant, group string) ([]string, error) {
	graph, err := s.store.GroupGraph(ctx, tenant)
	if err != nil {
		return nil, fmt.Errorf("group graph: %w", err)
	}
	static := s.aclStatic()
	_, staticGraph := static.GroupsOf(tenant, "")
	holders := map[string]bool{group: true}
	for _, g := range authz.Closure(authz.MergeGraph(graph, staticGraph), []string{group}) {
		holders[g] = true
	}
	rows, err := s.store.AclBindingList(ctx, tenant, false)
	if err != nil {
		return nil, fmt.Errorf("bindings: %w", err)
	}
	seen := map[string]bool{}
	var names []string
	add := func(kind, id, acl string) {
		if authz.SubjectKind(kind) == authz.SubjectGroup && holders[id] && !seen[acl] {
			seen[acl] = true
			names = append(names, acl)
		}
	}
	for _, row := range rows {
		add(row.SubjectKind, row.SubjectID, row.ACLName)
	}
	for _, b := range static.BindingsIn(tenant) {
		add(string(b.SubjectKind), b.SubjectID, b.ACL)
	}
	sort.Strings(names)
	return names, nil
}

// aclGroupHandOver asks the group lock when an upsert would change who
// manages the group's membership: a new external_id hands it to an identity
// provider, whose sync and the removal policy then drop members while local
// edits are refused — leaving the group, by another route. A group that
// does not exist yet still asks: bindings to a deleted group's name stay.
func (s *Server) aclGroupHandOver(ctx context.Context, tenant, name, externalID string) *aclReply {
	current := ""
	row, err := s.store.GroupGet(ctx, tenant, name)
	switch {
	case err == nil:
		current = row.ExternalID
	case !isNotFound(err):
		r := aclFail(http.StatusInternalServerError, "group_get_failed", err.Error())
		return &r
	}
	if externalID == current {
		return nil
	}
	return s.aclGroupLock(ctx, tenant, name)
}

// aclGroupLock asks the retire question of every list group carries, or
// returns the refusal; a store fault refuses (fail closed).
func (s *Server) aclGroupLock(ctx context.Context, tenant, group string) *aclReply {
	names, err := s.aclGroupLists(ctx, tenant, group)
	if err != nil {
		r := aclFail(http.StatusInternalServerError, "group_lists_failed", err.Error())
		return &r
	}
	return s.aclRetireLists(ctx, names)
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
	if r := s.aclGroupHandOver(ctx, tenant, strings.TrimSpace(req.Name), req.ExternalID); r != nil {
		return *r
	}
	ev, refused := s.aclIntent(ctx, tenant, "group_written", strings.TrimSpace(req.Name))
	if refused != nil {
		return *refused
	}
	row, err := s.store.GroupPut(ctx, store.GroupRow{TenantID: tenant, Name: strings.TrimSpace(req.Name),
		Description: req.Description, ExternalID: req.ExternalID, CreatedBy: principalSubject(ctx)})
	if err != nil {
		s.recordFailed(ctx, ev)
		return aclStoreError(err, "group")
	}
	s.aclPublished(ctx, tenant, "group_written", row.Name)
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
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			if r := s.aclConfigGroupGet(ctx, tenant, name); r != nil {
				return *r
			}
		}
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

// aclConfigMember is one direct member of a config-declared group: the
// shape of a stored membership, without the store's added_at / added_by.
type aclConfigMember struct {
	TenantID   string `json:"tenant_id,omitempty"`
	GroupName  string `json:"group"`
	MemberKind string `json:"member_kind"`
	MemberID   string `json:"member_id"`
	Source     string `json:"source"`
}

// aclConfigGroupGet answers a get for a group only the config declares in
// tenant (nil when it declares none): the group, its direct members and the
// groups it is nested in, over the store's and the config's nesting merged
// as the resolver merges them.
func (s *Server) aclConfigGroupGet(ctx context.Context, tenant, name string) *aclReply {
	static := s.aclStatic()
	if static == nil {
		return nil
	}
	for _, g := range static.Groups {
		if g.Tenant != tenant || g.Name != name {
			continue
		}
		members := make([]aclConfigMember, 0, len(g.Members)+len(g.Groups))
		for _, m := range g.Members {
			members = append(members, aclConfigMember{TenantID: tenant, GroupName: name, MemberKind: store.GroupMemberUser, MemberID: m, Source: aclres.SourceConfig})
		}
		for _, m := range g.Groups {
			members = append(members, aclConfigMember{TenantID: tenant, GroupName: name, MemberKind: store.GroupMemberGroup, MemberID: m, Source: aclres.SourceConfig})
		}
		graph, err := s.store.GroupGraph(ctx, tenant)
		if err != nil {
			r := aclFail(http.StatusInternalServerError, "group_graph_failed", err.Error())
			return &r
		}
		_, staticGraph := static.GroupsOf(tenant, "")
		group := aclConfigGroup{TenantID: g.Tenant, Name: g.Name, Description: g.Description, ExternalID: g.ExternalID,
			Members: g.Members, Groups: g.Groups, Source: aclres.SourceConfig}
		r := aclOK(map[string]any{"group": group, "members": members, "contained_in": authz.Closure(authz.MergeGraph(graph, staticGraph), []string{name})})
		return &r
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
	if r := s.aclGroupLock(ctx, tenant, name); r != nil {
		return *r
	}
	ev, refused := s.aclIntent(ctx, tenant, "group_deleted", name)
	if refused != nil {
		return *refused
	}
	ok, err := s.store.GroupDelete(ctx, tenant, name)
	if err != nil {
		s.recordFailed(ctx, ev)
		return aclFail(http.StatusInternalServerError, "group_delete_failed", err.Error())
	}
	if !ok {
		s.recordFailed(ctx, ev)
		return aclFail(http.StatusNotFound, "group_not_found", "group not found")
	}
	s.aclPublished(ctx, tenant, "group_deleted", name)
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
	ev, refused := s.aclIntent(ctx, tenant, "group_member_added", name+"/"+req.Kind+":"+req.ID)
	if refused != nil {
		return *refused
	}
	err := s.store.GroupMemberAdd(ctx, store.GroupMemberRow{TenantID: tenant, GroupName: name, MemberKind: req.Kind, MemberID: strings.TrimSpace(req.ID), AddedBy: principalSubject(ctx)})
	if err != nil {
		s.recordFailed(ctx, ev)
		return aclStoreError(err, "group_member")
	}
	s.aclPublished(ctx, tenant, "group_member_added", name+"/"+req.Kind+":"+req.ID)
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
	if r := s.aclGroupLock(ctx, tenant, name); r != nil {
		return *r
	}
	ev, refused := s.aclIntent(ctx, tenant, "group_member_removed", name+"/"+kind+":"+id)
	if refused != nil {
		return *refused
	}
	ok, err := s.store.GroupMemberRemove(ctx, tenant, name, kind, id)
	if err != nil {
		s.recordFailed(ctx, ev)
		return aclFail(http.StatusInternalServerError, "group_member_remove_failed", err.Error())
	}
	if !ok {
		s.recordFailed(ctx, ev)
		return aclFail(http.StatusNotFound, "group_member_not_found", "no such membership")
	}
	s.aclPublished(ctx, tenant, "group_member_removed", name+"/"+kind+":"+id)
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
		// RFC FY Part E: a rule no surface evaluates would be stored and
		// decide nothing. A list that does not parse is left to the store,
		// which reports it as before.
		if rules, err := authz.ParseRules(req.Rules); err == nil {
			if r := notEnforceable(authz.RulesNotEnforceable(rules)); r != nil {
				return *r
			}
		}
		if r := s.aclNameReserved(ctx, tenant, strings.TrimSpace(req.Name)); r != nil {
			return *r
		}
		ev, refused := s.aclIntent(ctx, tenant, "acl_written", strings.TrimSpace(req.Name))
		if refused != nil {
			return *refused
		}
		row, err := s.store.AclDefPut(ctx, store.AclDefRow{TenantID: tenant, Name: strings.TrimSpace(req.Name), Rules: req.Rules, Description: req.Description, CreatedBy: principalSubject(ctx)})
		if err != nil {
			s.recordFailed(ctx, ev)
			return aclStoreError(err, "acl")
		}
		s.aclPublished(ctx, tenant, "acl_written", row.Name)
		if tenant == "" {
			return s.aclSharedWritten(ctx, row)
		}
		return aclCreated(row)
	case "validate":
		rules, err := authz.ParseRules(req.Rules)
		if err != nil {
			return aclFail(http.StatusBadRequest, "acl_invalid", err.Error())
		}
		if r := notEnforceable(authz.RulesNotEnforceable(rules)); r != nil {
			return *r
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
		ev, refused := s.aclIntent(ctx, tenant, "acl_retired", req.Name)
		if refused != nil {
			return *refused
		}
		ok, err := s.store.AclDefRetire(ctx, tenant, req.Name)
		if err != nil {
			s.recordFailed(ctx, ev)
			return aclFail(http.StatusInternalServerError, "acl_retire_failed", err.Error())
		}
		if !ok {
			s.recordFailed(ctx, ev)
			return aclFail(http.StatusNotFound, "acl_not_found", "no live list of that name")
		}
		s.aclPublished(ctx, tenant, "acl_retired", req.Name)
		return aclOK(map[string]any{"retired": req.Name})
	case "catalog":
		return aclOK(map[string]any{"catalog_version": authz.CatalogVersion, "types": catalogTypes(), "transports": authz.Transports})
	case "names":
		return s.aclDefNamesIn(ctx, tenant)
	default:
		// The refusal names the op table, the one list every transport
		// dispatches on (TestACLDefRoute_DispatchesEveryOpInTheTable holds
		// the cases above to it).
		return aclFail(http.StatusBadRequest, "unknown_op", "op must be one of "+strings.Join(connector.ACLManagementOps(connector.ACLFamilyAclDef), ", "))
	}
}

// aclNameReserved refuses a write that would put a list of tenant in force
// under a name a config-declared list holds, or (for a tenant other than
// the shared one) a live shared-tenant list holds. The resolver reads the
// tenant's row first (aclres lookupACL), so such a row would silently
// replace the operator's list for every binding in the tenant: a tenant
// operator could lift a deny the deployment declared (re-audit B3). The
// three write ops share the check because each is an upsert. A live row the
// tenant already holds stays writable: it is a shadow written before this
// check, which ReportACLShadows names at boot and resolution keeps until
// the operator renames it (RFC FY D4). The check and the write are not one
// transaction; a shared list created between them is named by the next boot
// or acls reload report (ReportACLShadows), and a binding of the name in
// the tenant is refused as ambiguous (aclBindingNameAmbiguous).
func (s *Server) aclNameReserved(ctx context.Context, tenant, name string) *aclReply {
	held, err := s.aclLiveList(ctx, tenant, name)
	if err != nil {
		return aclListLookupFault(ctx, tenant, name, err)
	}
	if held {
		return nil
	}
	if static := s.aclStatic(); static != nil {
		if _, declared := static.ACLs[name]; declared {
			r := aclFail(http.StatusConflict, "acl_name_reserved", fmt.Sprintf("list %q is declared in the server configuration; a stored list of that name would replace it, so choose another name", name))
			return &r
		}
	}
	if tenant == "" {
		return nil
	}
	shared, err := s.aclLiveList(ctx, "", name)
	if err != nil {
		return aclListLookupFault(ctx, "", name, err)
	}
	if shared {
		r := aclFail(http.StatusConflict, "acl_name_reserved", fmt.Sprintf("list %q is a shared list every tenant reads; a list of that name here would replace it, so choose another name", name))
		return &r
	}
	return nil
}

// aclSharedWritten answers a write of a shared-tenant list. A tenant that
// already holds a live list of the name keeps resolving its own (RFC FY D4),
// so the shared list does not reach it: each such tenant is warned of in
// the log, and the answer carries only how many there are
// ("shadowed_in_tenants"), never which — the admin renames, or a binding
// of the name in such a tenant is refused (acl_name_shadowed). The scan
// reads every tenant's lists; a shared-list write is an admin's, and rare.
func (s *Server) aclSharedWritten(ctx context.Context, row store.AclDefRow) aclReply {
	snap, err := s.store.SnapshotReadAcl(ctx)
	if err != nil {
		slog.WarnContext(ctx, "authz: tenants holding a shared list's name could not be checked", "name", row.Name, "err", err)
		return aclCreated(row)
	}
	n := 0
	for _, def := range snap.Defs {
		if def.TenantID == "" || def.Name != row.Name || def.Retired() {
			continue
		}
		slog.WarnContext(ctx, "authz: a tenant's list shadows the shared list of the same name; bindings there resolve to the tenant's",
			"tenant", def.TenantID, "name", def.Name)
		n++
	}
	body, err := aclRowWith(row, "shadowed_in_tenants", n)
	if err != nil {
		return aclCreated(row)
	}
	return aclCreated(body)
}

// aclRowWith is row's JSON with one more key. The row is kept as raw JSON
// so its int64 version never passes through a float.
func aclRowWith(row store.AclDefRow, key string, v any) (map[string]json.RawMessage, error) {
	raw, err := json.Marshal(row)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m[key], err = json.Marshal(v); err != nil {
		return nil, err
	}
	return m, nil
}

// aclLiveList reports whether tenant holds a live stored list of name.
func (s *Server) aclLiveList(ctx context.Context, tenant, name string) (bool, error) {
	row, err := s.store.AclDefGet(ctx, tenant, name)
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return !row.Retired(), nil
}

// aclListLookupFault is the refusal for a store fault while checking a
// name: fail closed, and retryable.
func aclListLookupFault(ctx context.Context, tenant, name string, err error) *aclReply {
	slog.ErrorContext(ctx, "authz: list name check failed", "tenant", tenant, "name", name, "err", err)
	r := aclFail(http.StatusServiceUnavailable, "acl_unavailable", "the list name could not be checked; retry")
	return &r
}

// ReportACLShadows warns once for every live stored list that shadows
// another list of the same name, and returns how many it found: a tenant's
// or a shared-tenant row over a config-declared list, and a tenant's row
// over a live shared-tenant list. Resolution keeps the nearer list first for
// now (RFC FY D4), so this is how an operator learns a guardrail it declared
// does not reach a tenant, and renames the list. Run at boot and on an acls
// reload; it reads every tenant's lists.
func (s *Server) ReportACLShadows(ctx context.Context, log *slog.Logger) int {
	if s.store == nil {
		return 0
	}
	snap, err := s.store.SnapshotReadAcl(ctx)
	if err != nil {
		log.Warn("authz: stored lists could not be checked for shadowing", "err", err)
		return 0
	}
	var declared map[string]authz.ACL
	if static := s.aclStatic(); static != nil {
		declared = static.ACLs
	}
	shared := map[string]bool{}
	for _, def := range snap.Defs {
		if def.TenantID == "" && !def.Retired() {
			shared[def.Name] = true
		}
	}
	n := 0
	for _, def := range snap.Defs {
		if def.Retired() {
			continue
		}
		if _, ok := declared[def.Name]; ok {
			log.Warn("authz: a stored list shadows the config-declared list of the same name; bindings resolve to the stored one",
				"tenant", def.TenantID, "name", def.Name)
			n++
			continue
		}
		if def.TenantID != "" && shared[def.Name] {
			log.Warn("authz: a tenant's list shadows the shared list of the same name; bindings there resolve to the tenant's",
				"tenant", def.TenantID, "name", def.Name)
			n++
		}
	}
	return n
}

// aclListOpSurface is the question an op of POST /v1/_acldef asks of the list
// it names: read for a get, retire for a retire, and author for every other op
// — fail closed, so a write op added to the handler later is checked even if
// nobody adds it here. The three ops that name no list and write nothing
// (list, validate, catalog, names) return nil: the route's type-level read
// answered them.
func aclListOpSurface(op, name string) *authz.Surface {
	action := "author"
	switch op {
	case "list", "validate", "catalog", "names":
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

// aclListOpCheck asks the op's question of the named list. It returns the
// refusal, or nil. No principal (open mode) has no set to ask.
func (s *Server) aclListOpCheck(ctx context.Context, op, name string) *aclReply {
	p, ok := auth.PrincipalFromContext(ctx)
	surface := aclListOpSurface(op, name)
	if !ok || surface == nil {
		return nil
	}
	return s.gateReply(ctx, p, *surface, authz.EvaluateSurface(ctx, p.Effective, *surface, true))
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
	for _, name := range []string{authz.BaselineAdmin, authz.BaselineTenantOperator, authz.BaselineMember, authz.BaselineIsolated, authz.BaselineRunPlane, authz.BaselineAuthManager, authz.BaselineSCIMProvisioner, authz.BaselineMetricsReader} {
		out = append(out, bl[name])
	}
	return out
}

// aclCatalogType is one type of the op=catalog answer.
type aclCatalogType struct {
	Type  string   `json:"type"`
	Verbs []string `json:"verbs"`
	// Scopes and Separator: present on a scoped or enum type; Separator only
	// on a scoped one.
	Scopes    []string `json:"scopes,omitempty"`
	Separator string   `json:"separator,omitempty"`
	// TypeLevelOnly: the verbs asked only of every instance, so a rule on
	// one of them must name every instance (`run:*`); one naming some is
	// refused at write. A picker offers only the every-instance selector for
	// them.
	TypeLevelOnly []string `json:"type_level_only,omitempty"`
}

func catalogTypes() []aclCatalogType {
	var out []aclCatalogType
	for _, name := range authz.Types() {
		spec, _ := authz.TypeSpecOf(name)
		// Only what a rule may be written with: a verb no surface asks is
		// refused at write (RFC FY Part E), so a picker built from this
		// listing never offers one.
		verbs := authz.EnforcedVerbs(name)
		if len(verbs) == 0 {
			continue
		}
		entry := aclCatalogType{Type: name, Verbs: verbs, TypeLevelOnly: authz.TypeLevelOnlyVerbs(name)}
		if len(spec.Scopes) > 0 {
			entry.Scopes = spec.Scopes
			if spec.Sep != 0 {
				entry.Separator = string(spec.Sep)
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
	return s.aclDefNamesIn(ctx, s.adminFocusedTenant(ctx, focus))
}

// aclDefNamesIn is the live lists' names in tenant, config-declared ones
// included: GET /v1/_acldef/names and the acldef op names.
func (s *Server) aclDefNamesIn(ctx context.Context, tenant string) aclReply {
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
	tenant := s.adminFocusedTenant(ctx, focus)
	if r := s.aclBindingTargetCheck(ctx, tenant, req); r != nil {
		return *r
	}
	// The binding's id is the store's to choose: the intent names the list
	// and whom it binds.
	ev, refused := s.aclIntent(ctx, tenant, "binding_written", req.ACL+"@"+req.SubjectKind+":"+req.SubjectID)
	if refused != nil {
		return *refused
	}
	row, err := s.store.AclBindingPut(ctx, store.AclBindingRow{TenantID: tenant, ACLName: req.ACL, SubjectKind: req.SubjectKind,
		SubjectID: req.SubjectID, ExpiresAt: req.ExpiresAt.Time, CreatedBy: principalSubject(ctx)})
	if err != nil {
		s.recordFailed(ctx, ev)
		return aclStoreError(err, "acl_binding")
	}
	s.aclPublished(ctx, tenant, "binding_written", row.BindingID)
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
	name := strings.TrimSpace(req.ACL)
	src, err := s.aclResolver.ACLSource(ctx, tenant, name)
	if err != nil {
		r := aclFail(http.StatusServiceUnavailable, "acl_unavailable", "the list could not be looked up; retry")
		return &r
	}
	if src == "" {
		r := aclFail(http.StatusBadRequest, "acl_binding_invalid", fmt.Sprintf("list %q does not exist (or is retired): a binding to it would match nothing — create the list first", req.ACL))
		return &r
	}
	return s.aclBindingNameAmbiguous(ctx, tenant, name)
}

// aclBindingNameAmbiguous refuses a binding in tenant to a name that tenant
// holds as a live list of its own AND a config-declared or shared-tenant
// list holds too. The binding would resolve to the tenant's list (RFC FY
// D4), so an admin binding a shared or config guardrail into a tenant that
// pre-created a list of the name would bind the tenant's own rules instead:
// create refuses the name in the other order (acl_name_reserved), this
// refuses it in this one. The way out is renaming one of the lists.
func (s *Server) aclBindingNameAmbiguous(ctx context.Context, tenant, name string) *aclReply {
	own, err := s.aclLiveList(ctx, tenant, name)
	if err != nil {
		return aclListLookupFault(ctx, tenant, name, err)
	}
	if !own {
		return nil
	}
	other := ""
	if static := s.aclStatic(); static != nil {
		if _, declared := static.ACLs[name]; declared {
			other = "the server configuration declares one"
		}
	}
	if other == "" && tenant != "" {
		shared, err := s.aclLiveList(ctx, "", name)
		if err != nil {
			return aclListLookupFault(ctx, "", name, err)
		}
		if shared {
			other = "a shared list has it"
		}
	}
	if other == "" {
		return nil
	}
	r := aclFail(http.StatusConflict, "acl_name_shadowed", fmt.Sprintf("list name %q is ambiguous here: this tenant has its own list of that name and %s, and a binding would resolve to this tenant's; rename one of them", name, other))
	return &r
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
	list, found, err := s.aclLiveBindingList(ctx, tenant, id)
	if err != nil {
		return aclFail(http.StatusInternalServerError, "acl_binding_retire_failed", err.Error())
	}
	if !found {
		return aclFail(http.StatusNotFound, "acl_binding_not_found", "no live binding of that id")
	}
	if r := s.aclRetireLists(ctx, []string{list}); r != nil {
		return *r
	}
	ev, refused := s.aclIntent(ctx, tenant, "binding_retired", id)
	if refused != nil {
		return *refused
	}
	ok, err := s.store.AclBindingRetire(ctx, tenant, id)
	if err != nil {
		s.recordFailed(ctx, ev)
		return aclFail(http.StatusInternalServerError, "acl_binding_retire_failed", err.Error())
	}
	if !ok {
		s.recordFailed(ctx, ev)
		return aclFail(http.StatusNotFound, "acl_binding_not_found", "no live binding of that id")
	}
	s.aclPublished(ctx, tenant, "binding_retired", id)
	return aclOK(map[string]any{"retired": id})
}

// aclLiveBindingList is the list a live binding of tenant names. The store
// has no lookup by id, so the tenant's live bindings are scanned; a tenant
// holds operator-authored bindings, few enough for that.
func (s *Server) aclLiveBindingList(ctx context.Context, tenant, id string) (string, bool, error) {
	rows, err := s.store.AclBindingList(ctx, tenant, false)
	if err != nil {
		return "", false, err
	}
	for _, row := range rows {
		if row.BindingID == id {
			return row.ACLName, true, nil
		}
	}
	return "", false, nil
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
	// SourceIP is the client address a source_cidr condition reads.
	SourceIP string `json:"source_ip,omitempty"`
}

// unenforcedField names a set field of c that no surface fills, or "".
func (c *authzContextRef) unenforcedField() string {
	set := map[string]bool{"interactive": c.Interactive, "tier": c.Tier != "", "user_tier": c.UserTier != "", "agent": c.Agent != "", "depth": c.Depth != 0}
	for _, f := range []string{"interactive", "tier", "user_tier", "agent", "depth"} {
		if set[f] && !authz.IsConditionEnforced(f) {
			return f
		}
	}
	return ""
}

// notEnforceable is the refusal of a rule or a question no surface
// evaluates (RFC FY Part E), or nil when err is nil.
func notEnforceable(err error) *aclReply {
	if err == nil {
		return nil
	}
	r := aclFail(http.StatusBadRequest, "rule_not_enforceable", err.Error())
	return &r
}

// authzRequestReply is the refusal of a malformed question: invalid_request,
// or rule_not_enforceable for one no surface asks.
func authzRequestReply(prefix string, err error) aclReply {
	var ne *authz.NotEnforceableError
	if errors.As(err, &ne) {
		return aclFail(http.StatusBadRequest, "rule_not_enforceable", prefix+err.Error())
	}
	return aclFail(http.StatusBadRequest, "invalid_request", prefix+err.Error())
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
	accessMode := ""
	if row, err := s.store.UserGet(ctx, tenant, ref.ID); err == nil {
		accessMode = row.AccessMode
	}
	other, err := s.aclResolver.Resolve(ctx, aclres.RowPrincipal(tenant, ref.ID, accessMode))
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
	// The question is answered as a gate would answer it, so one no gate
	// asks is refused, as a rule naming it is (RFC FY Part E).
	// A named instance of a pair asked only at the type level is a question
	// no surface asks either (RFC GD D2).
	if err := authz.QuestionNotEnforceable(in.Resource.Type, in.Action.Name, in.Resource.ID); err != nil {
		return authz.Request{}, err
	}
	if in.Context != nil {
		if f := in.Context.unenforcedField(); f != "" {
			return authz.Request{}, &authz.NotEnforceableError{Condition: f}
		}
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
		if in.Context.SourceIP != "" {
			if c.SourceIP = net.ParseIP(in.Context.SourceIP); c.SourceIP == nil {
				return authz.Request{}, fmt.Errorf("context.source_ip %q is not an address", in.Context.SourceIP)
			}
		}
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
		return authzRequestReply("", err)
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
			return authzRequestReply(fmt.Sprintf("evaluations[%d]: ", i), err)
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
	tenant := s.adminFocusedTenant(ctx, focus)
	eff, code, msg := s.authzSubjectSet(ctx, tenant, req.Subject)
	if code != 0 {
		return aclFail(code, "authz_subject", msg)
	}
	areq, err := authzRequestFrom(eff, req, ctx)
	if err != nil {
		return authzRequestReply("", err)
	}
	ex := eff.Explain(areq)
	out := map[string]any{"decision": decisionWire(ctx, ex.Decision), "traces": ex.Traces, "missing_acls": ex.Missing, "applied_bindings": ex.Applied, "groups": eff.Subject.Groups}
	// The notes name lists and where they are stored, which a member could
	// not otherwise learn: only a caller the list-read gate admits gets them.
	if s.store != nil && s.aclCallerMayReadLists(ctx) {
		s.explainLocks(ctx, eff.Subject.TenantID, tenant, ex.Applied, areq.Resource, out)
	}
	return aclOK(out)
}

// aclShadowNote names an applied list whose stored row shadows the
// config-declared list of the same name, and the tenant that row is in
// ("" for the shared tenant).
type aclShadowNote struct {
	ACL            string `json:"acl"`
	StoredInTenant string `json:"stored_in_tenant"`
	Shadows        string `json:"shadows"`
}

// explainLocks adds to an explain answer what the rule trace cannot show:
// "shadowed_lists", the applied lists a stored row resolves in place of the
// config-declared one (RFC FY D4 keeps that order for now), and, when the
// resource is one group, "governed_by": the lists whose retire question
// guards leaving or deleting it (aclGroupLists). A binding is not a
// resource a request can name, so it has no such entry; the list it names
// is the one that governs it. Both are best-effort: a store fault leaves
// the entry out, never the decision. The caller has passed the list-read
// gate. subjectTenant is where the explained set's lists resolve; focus is
// the tenant the request addresses (an admin's ?tenant=), which holds the
// group — the legacy admin's own set is in "default" whatever it focuses.
func (s *Server) explainLocks(ctx context.Context, subjectTenant, focus string, applied []authz.Binding, res authz.Resource, out map[string]any) {
	if static := s.aclStatic(); static != nil {
		var notes []aclShadowNote
		seen := map[string]bool{}
		for _, b := range applied {
			if _, declared := static.ACLs[b.ACL]; !declared || seen[b.ACL] {
				continue
			}
			seen[b.ACL] = true
			// The resolver's order (aclres storedACL): the tenant's row,
			// then the shared tenant's.
			order := []string{subjectTenant}
			if subjectTenant != "" {
				order = append(order, "")
			}
			for _, t := range order {
				live, err := s.aclLiveList(ctx, t, b.ACL)
				if err != nil {
					slog.WarnContext(ctx, "authz: explain could not check a list for shadowing", "tenant", t, "name", b.ACL, "err", err)
					break
				}
				if live {
					notes = append(notes, aclShadowNote{ACL: b.ACL, StoredInTenant: t, Shadows: aclres.SourceConfig})
					break
				}
			}
		}
		if len(notes) > 0 {
			out["shadowed_lists"] = notes
		}
	}
	group, isGroup := strings.CutPrefix(res.Name, "group:")
	if res.Type != "principal" || !isGroup || group == "" || strings.Contains(group, "*") {
		return
	}
	names, err := s.aclGroupLists(ctx, focus, group)
	if err != nil {
		slog.WarnContext(ctx, "authz: explain could not read the lists that govern a group", "tenant", focus, "group", group, "err", err)
		return
	}
	if names == nil {
		names = []string{}
	}
	out["governed_by"] = map[string]any{"group": group, "retire": names}
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
		if r := notEnforceable(authz.RulesNotEnforceable(a.Rules)); r != nil {
			return *r
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
			return authzRequestReply(fmt.Sprintf("requests[%d]: ", i), err)
		}
		now, after := eff.Decide(areq), draft.Decide(areq)
		out = append(out, map[string]any{"request": e, "now": decisionWire(ctx, now), "with_draft": decisionWire(ctx, after), "changed": now.Outcome != after.Outcome})
	}
	return aclOK(map[string]any{"results": out, "missing_acls": draft.Missing()})
}
