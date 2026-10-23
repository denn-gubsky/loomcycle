package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC EY P4 — the HTTP management surfaces: user groups, ACL rule lists,
// bindings, and the AuthZEN-shaped evaluate / explain / simulate questions.
//
// Every row is confined to the caller's tenant (admin may focus another with
// ?tenant=, the RFC AS posture); every write bumps the tenant's acl_version
// in the store, drops this replica's cached sets, tells the other replicas,
// and is audited with names and ids, never a bearer.

const aclBodyCap = 1 << 20

// aclTenant is the tenant a management call acts in.
func (s *Server) aclTenant(r *http.Request) string {
	return s.adminFocusedTenant(r.Context(), r.URL.Query().Get("tenant"))
}

func (s *Server) aclStoreReady(w http.ResponseWriter) bool {
	if s.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "store_unavailable", "store not configured; access control requires a persistent store")
		return false
	}
	return true
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

func writeACLStoreError(w http.ResponseWriter, err error, kind string) {
	var nf *store.ErrNotFound
	var conflict *store.ErrConflict
	switch {
	case errors.As(err, &nf):
		writeJSONError(w, http.StatusNotFound, kind+"_not_found", kind+" not found")
	case errors.As(err, &conflict):
		writeJSONError(w, http.StatusConflict, kind+"_exists", err.Error())
	default:
		writeJSONError(w, http.StatusBadRequest, kind+"_invalid", err.Error())
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
	rows, err := s.store.GroupList(r.Context(), s.aclTenant(r))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "groups_list_failed", err.Error())
		return
	}
	writeJSONOK(w, map[string]any{"groups": rows})
}

func (s *Server) handleUpsertGroup(w http.ResponseWriter, r *http.Request) {
	if !s.aclStoreReady(w) {
		return
	}
	var req groupRequest
	if !decodeACLBody(w, r, &req) {
		return
	}
	tenant := s.aclTenant(r)
	row, err := s.store.GroupPut(r.Context(), store.GroupRow{TenantID: tenant, Name: strings.TrimSpace(req.Name),
		Description: req.Description, ExternalID: req.ExternalID, CreatedBy: principalSubject(r.Context())})
	if err != nil {
		writeACLStoreError(w, err, "group")
		return
	}
	s.aclChanged(r.Context(), tenant, "group_written", row.Name)
	writeJSON(w, http.StatusCreated, row)
}

func (s *Server) handleGetGroup(w http.ResponseWriter, r *http.Request) {
	if !s.aclStoreReady(w) {
		return
	}
	tenant, name := s.aclTenant(r), r.PathValue("name")
	row, err := s.store.GroupGet(r.Context(), tenant, name)
	if err != nil {
		writeACLStoreError(w, err, "group")
		return
	}
	members, err := s.store.GroupMembers(r.Context(), tenant, name)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "group_members_failed", err.Error())
		return
	}
	graph, err := s.store.GroupGraph(r.Context(), tenant)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "group_graph_failed", err.Error())
		return
	}
	// contained_in: every group a member of this one is also in.
	writeJSONOK(w, map[string]any{"group": row, "members": members, "contained_in": authz.Closure(graph, []string{name})})
}

func (s *Server) handleDeleteGroup(w http.ResponseWriter, r *http.Request) {
	if !s.aclStoreReady(w) {
		return
	}
	tenant, name := s.aclTenant(r), r.PathValue("name")
	ok, err := s.store.GroupDelete(r.Context(), tenant, name)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "group_delete_failed", err.Error())
		return
	}
	if !ok {
		writeJSONError(w, http.StatusNotFound, "group_not_found", "group not found")
		return
	}
	s.aclChanged(r.Context(), tenant, "group_deleted", name)
	writeJSONOK(w, map[string]any{"deleted": name})
}

func (s *Server) handleAddGroupMember(w http.ResponseWriter, r *http.Request) {
	if !s.aclStoreReady(w) {
		return
	}
	var req groupMemberRequest
	if !decodeACLBody(w, r, &req) {
		return
	}
	tenant, name := s.aclTenant(r), r.PathValue("name")
	if req.Kind == store.GroupMemberUser {
		if err := aclNoSelfElevation(r.Context(), string(authz.SubjectUser), req.ID); err != nil {
			// Adding yourself to a group is the membership form of self-binding.
			writeJSONError(w, http.StatusForbidden, "acl_self_elevation", "refused: adding yourself to a group")
			return
		}
	}
	err := s.store.GroupMemberAdd(r.Context(), store.GroupMemberRow{TenantID: tenant, GroupName: name, MemberKind: req.Kind, MemberID: strings.TrimSpace(req.ID), AddedBy: principalSubject(r.Context())})
	if err != nil {
		writeACLStoreError(w, err, "group_member")
		return
	}
	s.aclChanged(r.Context(), tenant, "group_member_added", name+"/"+req.Kind+":"+req.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"group": name, "kind": req.Kind, "id": req.ID})
}

func (s *Server) handleRemoveGroupMember(w http.ResponseWriter, r *http.Request) {
	if !s.aclStoreReady(w) {
		return
	}
	tenant, name, kind, id := s.aclTenant(r), r.PathValue("name"), r.PathValue("kind"), r.PathValue("id")
	ok, err := s.store.GroupMemberRemove(r.Context(), tenant, name, kind, id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "group_member_remove_failed", err.Error())
		return
	}
	if !ok {
		writeJSONError(w, http.StatusNotFound, "group_member_not_found", "no such membership")
		return
	}
	s.aclChanged(r.Context(), tenant, "group_member_removed", name+"/"+kind+":"+id)
	writeJSONOK(w, map[string]any{"group": name, "kind": kind, "id": id, "removed": true})
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
	ctx, tenant := r.Context(), s.aclTenant(r)
	if !s.aclListOpAllowed(ctx, w, req.Op, strings.TrimSpace(req.Name)) {
		return
	}
	switch req.Op {
	case "create", "update", "put":
		if len(req.Rules) == 0 {
			writeJSONError(w, http.StatusBadRequest, "missing_rules", "rules are required")
			return
		}
		row, err := s.store.AclDefPut(ctx, store.AclDefRow{TenantID: tenant, Name: strings.TrimSpace(req.Name), Rules: req.Rules, Description: req.Description, CreatedBy: principalSubject(ctx)})
		if err != nil {
			writeACLStoreError(w, err, "acl")
			return
		}
		s.aclChanged(ctx, tenant, "acl_written", row.Name)
		writeJSON(w, http.StatusCreated, row)
	case "validate":
		rules, err := authz.ParseRules(req.Rules)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "acl_invalid", err.Error())
			return
		}
		writeJSONOK(w, map[string]any{"valid": true, "rules": len(rules), "catalog_version": authz.CatalogVersion})
	case "get":
		row, err := s.store.AclDefGet(ctx, tenant, req.Name)
		if err != nil {
			writeACLStoreError(w, err, "acl")
			return
		}
		writeJSONOK(w, row)
	case "list":
		rows, err := s.store.AclDefList(ctx, tenant, req.IncludeRetired)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "acl_list_failed", err.Error())
			return
		}
		writeJSONOK(w, map[string]any{"acls": rows, "builtin": builtinACLs()})
	case "retire":
		ok, err := s.store.AclDefRetire(ctx, tenant, req.Name)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "acl_retire_failed", err.Error())
			return
		}
		if !ok {
			writeJSONError(w, http.StatusNotFound, "acl_not_found", "no live list of that name")
			return
		}
		s.aclChanged(ctx, tenant, "acl_retired", req.Name)
		writeJSONOK(w, map[string]any{"retired": req.Name})
	case "catalog":
		writeJSONOK(w, map[string]any{"catalog_version": authz.CatalogVersion, "types": catalogTypes(), "transports": authz.Transports})
	default:
		writeJSONError(w, http.StatusBadRequest, "unknown_op", "op must be one of create, get, list, retire, validate, catalog")
	}
}

// aclListOpSurface is the question an op of POST /v1/_acldef asks of the list
// it names: author for a write, retire for a retire, read for a get. The other
// ops name no list (list, validate, catalog) and the route's type-level read
// answers them; nil means there is nothing more to ask.
func aclListOpSurface(op, name string) *authz.Surface {
	var action string
	switch op {
	case "create", "update", "put":
		action = "author"
	case "retire":
		action = "retire"
	case "get":
		action = "read"
	default:
		return nil
	}
	return &authz.Surface{Action: action, Type: "def", Name: "acl/" + name}
}

// aclListOpAllowed asks the op's question of the named list, then refuses a
// write that would change the writer's own authority. It writes the refusal
// and returns false. No principal (open mode) has no set to ask.
func (s *Server) aclListOpAllowed(ctx context.Context, w http.ResponseWriter, op, name string) bool {
	p, ok := auth.PrincipalFromContext(ctx)
	surface := aclListOpSurface(op, name)
	if !ok || surface == nil {
		return true
	}
	if !s.applyGate(ctx, w, p, *surface, authz.EvaluateSurface(ctx, p.Effective, *surface, true)) {
		return false
	}
	if surface.Action == "read" {
		return true
	}
	if err := aclListNotSelfBound(p, name); err != nil {
		writeJSONError(w, http.StatusForbidden, "acl_self_elevation", "refused: "+err.Error()+" would change your own authority")
		return false
	}
	return true
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
	rows, err := s.store.AclDefList(r.Context(), s.aclTenant(r), false)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "acl_list_failed", err.Error())
		return
	}
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row.Name)
	}
	writeJSONOK(w, map[string]any{"names": names})
}

// ---- bindings ----

type bindingRequest struct {
	ACL         string    `json:"acl"`
	SubjectKind string    `json:"subject_kind"`
	SubjectID   string    `json:"subject_id,omitempty"`
	ExpiresAt   time.Time `json:"expires_at,omitempty"`
}

func (s *Server) handleListBindings(w http.ResponseWriter, r *http.Request) {
	if !s.aclStoreReady(w) {
		return
	}
	rows, err := s.store.AclBindingList(r.Context(), s.aclTenant(r), r.URL.Query().Get("include_retired") == "1")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "acl_bindings_list_failed", err.Error())
		return
	}
	writeJSONOK(w, map[string]any{"bindings": rows})
}

func (s *Server) handleCreateBinding(w http.ResponseWriter, r *http.Request) {
	if !s.aclStoreReady(w) {
		return
	}
	var req bindingRequest
	if !decodeACLBody(w, r, &req) {
		return
	}
	if err := aclNoSelfElevation(r.Context(), req.SubjectKind, req.SubjectID); err != nil {
		writeJSONError(w, http.StatusForbidden, "acl_self_elevation", "refused: "+err.Error()+" would change your own authority")
		return
	}
	tenant := s.aclTenant(r)
	row, err := s.store.AclBindingPut(r.Context(), store.AclBindingRow{TenantID: tenant, ACLName: req.ACL, SubjectKind: req.SubjectKind,
		SubjectID: req.SubjectID, ExpiresAt: req.ExpiresAt, CreatedBy: principalSubject(r.Context())})
	if err != nil {
		writeACLStoreError(w, err, "acl_binding")
		return
	}
	s.aclChanged(r.Context(), tenant, "binding_written", row.BindingID)
	writeJSON(w, http.StatusCreated, row)
}

func (s *Server) handleRetireBinding(w http.ResponseWriter, r *http.Request) {
	if !s.aclStoreReady(w) {
		return
	}
	tenant, id := s.aclTenant(r), r.PathValue("id")
	ok, err := s.store.AclBindingRetire(r.Context(), tenant, id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "acl_binding_retire_failed", err.Error())
		return
	}
	if !ok {
		writeJSONError(w, http.StatusNotFound, "acl_binding_not_found", "no live binding of that id")
		return
	}
	s.aclChanged(r.Context(), tenant, "binding_retired", id)
	writeJSONOK(w, map[string]any{"retired": id})
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
	eff, code, msg := s.authzSubjectSet(r.Context(), s.aclTenant(r), req.Subject)
	if code != 0 {
		writeJSONError(w, code, "authz_subject", msg)
		return
	}
	areq, err := authzRequestFrom(eff, req, r.Context())
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	writeJSONOK(w, decisionWire(r.Context(), eff.Decide(areq)))
}

func (s *Server) handleAuthzEvaluations(w http.ResponseWriter, r *http.Request) {
	var req authzEvaluationsRequest
	if !decodeACLBody(w, r, &req) {
		return
	}
	eff, code, msg := s.authzSubjectSet(r.Context(), s.aclTenant(r), req.Subject)
	if code != 0 {
		writeJSONError(w, code, "authz_subject", msg)
		return
	}
	if len(req.Evaluations) == 0 || len(req.Evaluations) > 256 {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "evaluations must list 1 to 256 entries")
		return
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
		areq, err := authzRequestFrom(eff, e, r.Context())
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("evaluations[%d]: %v", i, err))
			return
		}
		reqs = append(reqs, areq)
	}
	sem := authz.ExecuteAll
	if req.Options != nil && req.Options.Semantic != "" {
		sem = authz.Semantic(req.Options.Semantic)
	}
	out := make([]authzDecisionWire, 0, len(reqs))
	for _, d := range eff.DecideAll(reqs, sem) {
		out = append(out, decisionWire(r.Context(), d))
	}
	writeJSONOK(w, map[string]any{"evaluations": out})
}

func (s *Server) handleAuthzExplain(w http.ResponseWriter, r *http.Request) {
	var req authzEvaluationRequest
	if !decodeACLBody(w, r, &req) {
		return
	}
	eff, code, msg := s.authzSubjectSet(r.Context(), s.aclTenant(r), req.Subject)
	if code != 0 {
		writeJSONError(w, code, "authz_subject", msg)
		return
	}
	areq, err := authzRequestFrom(eff, req, r.Context())
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	ex := eff.Explain(areq)
	writeJSONOK(w, map[string]any{"decision": decisionWire(r.Context(), ex.Decision), "traces": ex.Traces, "missing_acls": ex.Missing, "applied_bindings": ex.Applied, "groups": eff.Subject.Groups})
}

// handleAuthzSimulate evaluates against a DRAFT: the stored lists and
// bindings the subject has now, with the draft's lists added or replaced by
// name and its bindings added. Nothing is written.
func (s *Server) handleAuthzSimulate(w http.ResponseWriter, r *http.Request) {
	var req authzSimulateRequest
	if !decodeACLBody(w, r, &req) {
		return
	}
	tenant := s.aclTenant(r)
	eff, code, msg := s.authzSubjectSet(r.Context(), tenant, req.Subject)
	if code != 0 {
		writeJSONError(w, code, "authz_subject", msg)
		return
	}
	if len(req.Requests) == 0 || len(req.Requests) > 256 {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "requests must list 1 to 256 entries")
		return
	}
	in, err := s.aclResolver.Input(r.Context(), eff.Subject)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "authz_subject", "the subject's access-control rows could not be read")
		return
	}
	for _, a := range req.ACLs {
		if err := a.Validate(); err != nil {
			writeJSONError(w, http.StatusBadRequest, "acl_invalid", err.Error())
			return
		}
		in.ACLs[a.Name] = authz.ACLVersion{ACL: a, Version: 0}
	}
	for _, b := range req.Bindings {
		if err := b.Validate(); err != nil {
			writeJSONError(w, http.StatusBadRequest, "acl_binding_invalid", err.Error())
			return
		}
		in.Bindings = append(in.Bindings, b)
	}
	draft, err := authz.Compile(in)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "acl_invalid", err.Error())
		return
	}
	out := make([]map[string]any, 0, len(req.Requests))
	for i, e := range req.Requests {
		areq, err := authzRequestFrom(draft, e, r.Context())
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("requests[%d]: %v", i, err))
			return
		}
		now, after := eff.Decide(areq), draft.Decide(areq)
		out = append(out, map[string]any{"request": e, "now": decisionWire(r.Context(), now), "with_draft": decisionWire(r.Context(), after), "changed": now.Outcome != after.Outcome})
	}
	writeJSONOK(w, map[string]any{"results": out, "missing_acls": draft.Missing()})
}
