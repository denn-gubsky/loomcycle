package http

// scim.go — RFC FA P6: inbound SCIM 2.0 (an RFC 7643/7644 subset) as an
// external-gate-compatible sync. An identity provider holding a
// scim:provision token provisions the users and groups of ONE tenant — its
// token's — through the deployment's kind: scim gate, and every effect goes
// through the code an external gate's /v1/_auth/sync uses:
//
//   - a user is made by provisionOrRefuse: subject "<gate>:<SCIM id>" (the
//     server's id, never a userName or e-mail the provider may rename), the
//     identity map's new_user floor, a user_identities row at the gate;
//   - accessMode (the loomcycle extension) is honoured only on a trusted
//     gate, and asking for one on an untrusted gate is refused, not ignored;
//   - group membership is what the provider asserts: a SCIM group's members
//     are the gate's assertion for each member, reconciled by syncGroups into
//     the "<gate>:<group>" groups (added AND removed; local and other gates'
//     groups never touched), and only with identity_map.groups on a trusted
//     gate;
//   - DELETE and active:false are the removal policy (disable now, unbind
//     and erase later), only with identity_map.authoritative_for_users and
//     within max_sync_removals per 24 h; nothing is hard-deleted.
//
// Status rules: SCIM never moves a user from pending to active (approval is
// an operator's), and re-enables only a user whose disable this gate's SCIM
// caused; an operator's or another gate's disable stands.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authgate"
	"github.com/denn-gubsky/loomcycle/internal/authgate/identity"
	"github.com/denn-gubsky/loomcycle/internal/authgate/lifecycle"
	"github.com/denn-gubsky/loomcycle/internal/authgate/password"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

const (
	scimSchemaUser    = "urn:ietf:params:scim:schemas:core:2.0:User"
	scimSchemaGroup   = "urn:ietf:params:scim:schemas:core:2.0:Group"
	scimSchemaExt     = "urn:ietf:params:scim:schemas:extension:loomcycle:2.0:User"
	scimSchemaList    = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	scimSchemaError   = "urn:ietf:params:scim:api:messages:2.0:Error"
	scimSchemaSPC     = "urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"
	scimSchemaResType = "urn:ietf:params:scim:schemas:core:2.0:ResourceType"
	scimSchemaSchema  = "urn:ietf:params:scim:schemas:core:2.0:Schema"
	scimContentType   = "application/scim+json"

	scimMaxResults      = 100
	scimMaxBody         = 1 << 20
	scimMaxStringBytes  = 512
	scimMaxEmails       = 10
	scimMaxPatchOps     = 100
	scimMaxGroupMembers = 10000
	// scimMaxGroupKey bounds a group's provider name (its display name as a
	// slug), the part of "<gate>:<name>" the provider chooses.
	scimMaxGroupKey = 64
)

// scimError writes an RFC 7644 §3.12 error.
func scimError(w http.ResponseWriter, status int, scimType, detail string) {
	body := map[string]any{"schemas": []string{scimSchemaError}, "status": strconv.Itoa(status), "detail": detail}
	if scimType != "" {
		body["scimType"] = scimType
	}
	scimJSON(w, status, body)
}

func scimJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", scimContentType)
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Warn("scim: encode failed", "err", err)
	}
}

// scimCtx is one admitted SCIM request: the credential's tenant and the SCIM
// gate, its identity map pinned to that tenant.
type scimCtx struct {
	tenant string
	g      config.AuthGate
	p      auth.Principal
}

// scimCaller admits a request: a SCIM gate is configured, the principal is a
// scim:provision credential (the scope gate already reserved the route to
// one; open mode stamps none and is refused here), its tenant is a named one,
// and the gate serves it.
func (s *Server) scimCaller(w http.ResponseWriter, r *http.Request) (scimCtx, bool) {
	g, ok := s.cfg().ResolvedAuth.SCIMGate()
	if !ok || g.Identity == nil || g.SCIM == nil {
		scimError(w, http.StatusNotFound, "", "SCIM provisioning is not enabled on this server")
		return scimCtx{}, false
	}
	p, ok := auth.PrincipalFromContext(r.Context())
	if !ok || !auth.IsSCIMProvisioner(p) {
		scimError(w, http.StatusUnauthorized, "", "a "+auth.ScopeSCIMProvision+" token is required")
		return scimCtx{}, false
	}
	if p.TenantID == "" {
		// The shared tenant is never provisioned by an identity provider.
		scimError(w, http.StatusForbidden, "", "this token names no tenant")
		return scimCtx{}, false
	}
	if g.Identity.Tenant != "" && g.Identity.Tenant != p.TenantID {
		scimError(w, http.StatusForbidden, "", "this server's SCIM service does not serve this token's tenant")
		return scimCtx{}, false
	}
	if s.store == nil {
		scimError(w, http.StatusServiceUnavailable, "", "SCIM needs a persistent store")
		return scimCtx{}, false
	}
	m := *g.Identity
	m.Tenant = p.TenantID
	g.Identity = &m
	return scimCtx{tenant: p.TenantID, g: g, p: p}, true
}

func (s *Server) scimAudit(c scimCtx, action, subject, reason string) {
	s.recordAuthAudit(audit.Event{Action: action, AuthGate: c.g.Name, AuthReason: reason, TargetTenant: c.tenant,
		TargetSubject: subject, ActorTenant: c.p.TenantID, ActorSubject: c.p.Subject, ActorTokenSuffix: c.p.TokenSuffix})
}

// scimDecode reads a JSON body (application/scim+json or application/json),
// bounded.
func scimDecode(w http.ResponseWriter, r *http.Request, into any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, scimMaxBody)
	if err := json.NewDecoder(r.Body).Decode(into); err != nil {
		scimError(w, http.StatusBadRequest, "invalidSyntax", "invalid request body")
		return false
	}
	return true
}

func (s *Server) scimLocation(kind, id string) string {
	return strings.TrimRight(s.cfg().Env.PublicURL, "/") + scimRoutePrefix + kind + "/" + id
}

func newSCIMID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// scimSubject is the loomcycle subject of a SCIM user of the gate.
func scimSubject(c scimCtx, id string) string { return c.g.Name + ":" + id }

// scimIDOf is the SCIM id of a subject the gate made ("" for any other).
func scimIDOf(c scimCtx, subject string) string {
	id, ok := strings.CutPrefix(subject, c.g.Name+":")
	if !ok {
		return ""
	}
	return id
}

// ---- users ----

type scimName struct {
	Formatted  string `json:"formatted,omitempty"`
	GivenName  string `json:"givenName,omitempty"`
	FamilyName string `json:"familyName,omitempty"`
}

type scimEmail struct {
	Value   string `json:"value"`
	Type    string `json:"type,omitempty"`
	Primary bool   `json:"primary,omitempty"`
}

// scimUserAttrs is what the store keeps opaque for a user resource.
type scimUserAttrs struct {
	DisplayName string      `json:"displayName,omitempty"`
	Name        *scimName   `json:"name,omitempty"`
	Emails      []scimEmail `json:"emails,omitempty"`
}

type scimExt struct {
	AccessMode string `json:"accessMode,omitempty"`
}

// scimUserIn is a User resource as a client sends it. Read-only attributes
// (id, groups, meta) are ignored, as RFC 7643 §2.2 requires.
type scimUserIn struct {
	UserName    string      `json:"userName"`
	ExternalID  string      `json:"externalId"`
	DisplayName string      `json:"displayName"`
	Name        *scimName   `json:"name"`
	Emails      []scimEmail `json:"emails"`
	Active      *bool       `json:"active"`
	Ext         *scimExt    `json:"urn:ietf:params:scim:schemas:extension:loomcycle:2.0:User"`
}

// scimUserState is the mutable part of a user resource: what a PUT replaces
// and a PATCH edits. accessMode "" means "not asked".
type scimUserState struct {
	userName   string
	externalID string
	attrs      scimUserAttrs
	active     bool
	accessMode string
}

func boundStr(field, v string) error {
	if len(v) > scimMaxStringBytes {
		return fmt.Errorf("%s is longer than %d bytes", field, scimMaxStringBytes)
	}
	return nil
}

// validate bounds the state and returns its userName's case-folded key.
func (st *scimUserState) validate() (string, error) {
	key, err := password.NormalizeLogin(st.userName)
	if err != nil {
		return "", fmt.Errorf("userName: %w", err)
	}
	for f, v := range map[string]string{"externalId": st.externalID, "displayName": st.attrs.DisplayName} {
		if err := boundStr(f, v); err != nil {
			return "", err
		}
	}
	if n := st.attrs.Name; n != nil {
		for f, v := range map[string]string{"name.formatted": n.Formatted, "name.givenName": n.GivenName, "name.familyName": n.FamilyName} {
			if err := boundStr(f, v); err != nil {
				return "", err
			}
		}
	}
	if len(st.attrs.Emails) > scimMaxEmails {
		return "", fmt.Errorf("at most %d emails", scimMaxEmails)
	}
	for _, e := range st.attrs.Emails {
		if err := boundStr("emails.value", e.Value); err != nil {
			return "", err
		}
	}
	switch st.accessMode {
	case "", "isolated", "tenant":
	default:
		return "", fmt.Errorf("accessMode must be isolated or tenant")
	}
	return key, nil
}

func (st scimUserState) displayName() string {
	switch {
	case st.attrs.DisplayName != "":
		return st.attrs.DisplayName
	case st.attrs.Name != nil && st.attrs.Name.Formatted != "":
		return st.attrs.Name.Formatted
	case st.attrs.Name != nil && (st.attrs.Name.GivenName != "" || st.attrs.Name.FamilyName != ""):
		return strings.TrimSpace(st.attrs.Name.GivenName + " " + st.attrs.Name.FamilyName)
	}
	return st.userName
}

func (st scimUserState) primaryEmail() string {
	for _, e := range st.attrs.Emails {
		if e.Primary {
			return e.Value
		}
	}
	if len(st.attrs.Emails) > 0 {
		return st.attrs.Emails[0].Value
	}
	return ""
}

// state is the resource the request describes; an omitted active keeps
// defaultActive (on a PUT, the current value).
func (in scimUserIn) state(defaultActive bool) scimUserState {
	st := scimUserState{userName: in.UserName, externalID: in.ExternalID, active: defaultActive,
		attrs: scimUserAttrs{DisplayName: in.DisplayName, Name: in.Name, Emails: in.Emails}}
	if in.Active != nil {
		st.active = *in.Active
	}
	if in.Ext != nil {
		st.accessMode = in.Ext.AccessMode
	}
	return st
}

// assertion is what the provider asserts about a SCIM user, as an identity
// gate's assertion: the SCIM id as the external subject, the display data,
// the access mode asked for and the groups the user is a member of. An
// e-mail from SCIM is never verified.
func (st scimUserState) assertion(c scimCtx, id string, groups []string) authgate.Assertion {
	return authgate.Assertion{Subject: id, Email: st.primaryEmail(), DisplayName: st.displayName(),
		AccessMode: st.accessMode, Groups: groups, AuthTime: time.Now()}
}

// accessModeAllowed refuses an access mode an untrusted gate may not
// assert: anything but the floor on a create, anything but the current
// mode on an update.
func (c scimCtx) accessModeAllowed(asked, current string) bool {
	if asked == "" || c.g.Identity.Trusted {
		return true
	}
	if current == "" {
		current = c.g.Identity.NewUser.AccessMode
	}
	return asked == current
}

// handleSCIMCreateUser serves POST /scim/v2/Users.
func (s *Server) handleSCIMCreateUser(w http.ResponseWriter, r *http.Request) {
	c, ok := s.scimCaller(w, r)
	if !ok {
		return
	}
	var in scimUserIn
	if !scimDecode(w, r, &in) {
		return
	}
	st := in.state(true)
	key, err := st.validate()
	if err != nil {
		scimError(w, http.StatusBadRequest, "invalidValue", err.Error())
		return
	}
	if !st.active {
		scimError(w, http.StatusBadRequest, "invalidValue", "create users active; deactivate one with PATCH active:false")
		return
	}
	if !c.accessModeAllowed(st.accessMode, "") {
		scimError(w, http.StatusForbidden, "", "accessMode beyond the new-user floor needs a trusted SCIM gate")
		return
	}
	attrs, err := json.Marshal(st.attrs)
	if err != nil {
		scimError(w, http.StatusInternalServerError, "", "could not store the user")
		return
	}
	id, err := newSCIMID()
	if err != nil {
		scimError(w, http.StatusInternalServerError, "", "could not create the user")
		return
	}
	ctx := r.Context()
	// The create ceiling bounds how many users one provider adds per window.
	// It is read before the insert, so concurrent creates may pass it by the
	// requests in flight; the removal ceiling does not depend on it (its
	// base excludes the window's creates).
	created, err := s.store.SCIMUsersCreatedSince(ctx, c.g.Name, c.tenant, time.Now().UTC().Add(-config.SCIMRemovalWindow))
	if err != nil {
		scimError(w, http.StatusInternalServerError, "", "could not create the user")
		return
	}
	if limit := c.g.SCIM.CreateCeiling(); created >= limit {
		s.scimAudit(c, "auth_gate_sync_refused", "", fmt.Sprintf("create over the ceiling of %d in 24h", limit))
		scimError(w, http.StatusTooManyRequests, "", fmt.Sprintf(
			"this SCIM gate has created %d users in the last 24 hours, its ceiling (max_sync_creates); try again later", limit))
		return
	}
	// The resource is reserved first: its unique userName decides a race
	// between two creates before any user is made.
	row := store.SCIMUserRow{Gate: c.g.Name, TenantID: c.tenant, SCIMID: id, Subject: scimSubject(c, id), UserName: st.userName,
		UserNameKey: key, ExternalID: st.externalID, Attributes: string(attrs)}
	if err := s.store.SCIMUserCreate(ctx, row); err != nil {
		var conflict *store.ErrConflict
		if errors.As(err, &conflict) {
			scimError(w, http.StatusConflict, "uniqueness", "a user with this userName exists")
			return
		}
		scimError(w, http.StatusInternalServerError, "", "could not create the user")
		return
	}
	out, err := s.provisionOrRefuse(ctx, c.g, st.assertion(c, id, nil), s.clientSource(r))
	if err == nil && (out.TenantID != c.tenant || out.Subject != row.Subject) {
		err = fmt.Errorf("scim: provisioning resolved %s/%s, want %s/%s", out.TenantID, out.Subject, c.tenant, row.Subject)
	}
	if err != nil {
		if derr := s.store.SCIMUserDiscard(ctx, c.g.Name, c.tenant, id); derr != nil {
			slog.Error("scim: discarding a resource whose user was refused failed", "err", derr)
		}
		switch {
		case errors.Is(err, errIdentityRefused), errors.Is(err, errUserDisabled):
			scimError(w, http.StatusForbidden, "", "this gate's identity map refused the user")
		default:
			slog.Error("scim: provisioning a user failed", "gate", c.g.Name, "err", err)
			scimError(w, http.StatusInternalServerError, "", "could not create the user")
		}
		return
	}
	s.scimAudit(c, "auth_scim_user_created", row.Subject, "")
	s.writeSCIMUser(w, ctx, c, id, http.StatusCreated)
}

// scimUserOut is a user resource as the provider reads it back.
func (s *Server) scimUserOut(ctx context.Context, c scimCtx, row store.SCIMUserRow) (map[string]any, error) {
	u, err := s.store.UserGet(ctx, c.tenant, row.Subject)
	if err != nil {
		return nil, err
	}
	var attrs scimUserAttrs
	if err := json.Unmarshal([]byte(row.Attributes), &attrs); err != nil {
		return nil, fmt.Errorf("scim user attributes: %w", err)
	}
	groups, err := s.store.SCIMGroupsOfSubject(ctx, c.g.Name, c.tenant, row.Subject)
	if err != nil {
		return nil, err
	}
	gs := make([]map[string]string, 0, len(groups))
	for _, g := range groups {
		gs = append(gs, map[string]string{"value": g.SCIMID, "display": g.DisplayName, "$ref": s.scimLocation("Groups", g.SCIMID)})
	}
	out := map[string]any{
		"schemas": []string{scimSchemaUser, scimSchemaExt}, "id": row.SCIMID, "userName": row.UserName,
		// A pending user is not active yet: it signs in to nothing until an
		// operator approves it, whatever the provider sent.
		"active": u.Status == "active", "groups": gs,
		scimSchemaExt: map[string]string{"accessMode": u.AccessMode},
		"meta": map[string]any{"resourceType": "User", "created": row.CreatedAt.UTC(), "lastModified": row.UpdatedAt.UTC(),
			"location": s.scimLocation("Users", row.SCIMID), "version": fmt.Sprintf(`W/"%d"`, row.Version)},
	}
	if row.ExternalID != "" {
		out["externalId"] = row.ExternalID
	}
	if attrs.DisplayName != "" {
		out["displayName"] = attrs.DisplayName
	}
	if attrs.Name != nil {
		out["name"] = attrs.Name
	}
	if len(attrs.Emails) > 0 {
		out["emails"] = attrs.Emails
	}
	return out, nil
}

func (s *Server) writeSCIMUser(w http.ResponseWriter, ctx context.Context, c scimCtx, id string, status int) {
	row, err := s.store.SCIMUserGet(ctx, c.g.Name, c.tenant, id)
	if err != nil {
		s.scimNotFoundOr500(w, err, "User")
		return
	}
	out, err := s.scimUserOut(ctx, c, row)
	if err != nil {
		s.scimNotFoundOr500(w, err, "User")
		return
	}
	if status == http.StatusCreated {
		w.Header().Set("Location", s.scimLocation("Users", id))
	}
	scimJSON(w, status, out)
}

func (s *Server) scimNotFoundOr500(w http.ResponseWriter, err error, kind string) {
	if isNotFound(err) {
		scimError(w, http.StatusNotFound, "", kind+" not found")
		return
	}
	slog.Error("scim: store failed", "err", err)
	scimError(w, http.StatusInternalServerError, "", "the request could not be completed")
}

// handleSCIMGetUser serves GET /scim/v2/Users/{id}.
func (s *Server) handleSCIMGetUser(w http.ResponseWriter, r *http.Request) {
	c, ok := s.scimCaller(w, r)
	if !ok {
		return
	}
	s.writeSCIMUser(w, r.Context(), c, r.PathValue("id"), http.StatusOK)
}

var scimFilterRE = regexp.MustCompile(`^\s*([A-Za-z.]+)\s+eq\s+"((?:[^"\\]|\\.)*)"\s*$`)

// parseSCIMFilter accepts `<attribute> eq "<value>"` on userName or
// externalId (users), displayName or externalId (groups). Anything else is
// refused, not widened to "all".
func parseSCIMFilter(raw string, group bool) (store.SCIMFilter, error) {
	var f store.SCIMFilter
	if strings.TrimSpace(raw) == "" {
		return f, nil
	}
	if len(raw) > 2*scimMaxStringBytes {
		return f, errors.New("the filter is too long")
	}
	m := scimFilterRE.FindStringSubmatch(raw)
	if m == nil {
		return f, errors.New(`only filters of the form <attribute> eq "<value>" are supported`)
	}
	val, err := strconv.Unquote(`"` + m[2] + `"`)
	if err != nil {
		return f, errors.New("the filter value is not a valid string")
	}
	const noMatch = "\x00no-match"
	switch attr := strings.ToLower(m[1]); {
	case attr == "username" && !group:
		if f.UserNameKey, err = password.NormalizeLogin(val); err != nil {
			f.UserNameKey = noMatch // no resource can have it
		}
	case attr == "displayname" && group:
		if f.DisplayKey = scimGroupKey(val); f.DisplayKey == "" {
			f.DisplayKey = noMatch
		}
	case attr == "externalid":
		if f.ExternalID = val; f.ExternalID == "" {
			f.ExternalID = noMatch
		}
	default:
		return f, fmt.Errorf("filtering on %q is not supported", m[1])
	}
	return f, nil
}

// scimPage reads startIndex (1-based) and count, bounded.
func scimPage(r *http.Request) (offset, limit int) {
	start, count := 1, scimMaxResults
	if v, err := strconv.Atoi(r.URL.Query().Get("startIndex")); err == nil && v > 1 {
		start = min(v, 1<<30)
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("count")); err == nil && v >= 0 && v < count {
		count = v
	}
	return start - 1, count
}

func scimList(w http.ResponseWriter, total, offset int, resources []map[string]any) {
	scimJSON(w, http.StatusOK, map[string]any{"schemas": []string{scimSchemaList}, "totalResults": total,
		"startIndex": offset + 1, "itemsPerPage": len(resources), "Resources": resources})
}

// handleSCIMListUsers serves GET /scim/v2/Users[?filter=userName eq "x"].
func (s *Server) handleSCIMListUsers(w http.ResponseWriter, r *http.Request) {
	c, ok := s.scimCaller(w, r)
	if !ok {
		return
	}
	f, err := parseSCIMFilter(r.URL.Query().Get("filter"), false)
	if err != nil {
		scimError(w, http.StatusBadRequest, "invalidFilter", err.Error())
		return
	}
	offset, limit := scimPage(r)
	ctx := r.Context()
	rows, total, err := s.store.SCIMUserList(ctx, c.g.Name, c.tenant, f, offset, limit)
	if err != nil {
		s.scimNotFoundOr500(w, err, "User")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		u, err := s.scimUserOut(ctx, c, row)
		if err != nil {
			if isNotFound(err) {
				continue // the user was deleted under the resource
			}
			s.scimNotFoundOr500(w, err, "User")
			return
		}
		out = append(out, u)
	}
	scimList(w, total, offset, out)
}

// currentSCIMUser is a live resource, its user and its state as the provider
// last set it.
func (s *Server) currentSCIMUser(ctx context.Context, c scimCtx, id string) (store.SCIMUserRow, store.UserRow, scimUserState, error) {
	row, err := s.store.SCIMUserGet(ctx, c.g.Name, c.tenant, id)
	if err != nil {
		return row, store.UserRow{}, scimUserState{}, err
	}
	u, err := s.store.UserGet(ctx, c.tenant, row.Subject)
	if err != nil {
		return row, u, scimUserState{}, err
	}
	// The provider's last word on active is "not deprovisioned": a pending
	// user is one it wants active (it reads back active:false until an
	// operator approves), so a PUT that omits active never deactivates it.
	st := scimUserState{userName: row.UserName, externalID: row.ExternalID, active: u.Status != "disabled"}
	if err := json.Unmarshal([]byte(row.Attributes), &st.attrs); err != nil {
		return row, u, st, fmt.Errorf("scim user attributes: %w", err)
	}
	return row, u, st, nil
}

// handleSCIMReplaceUser serves PUT /scim/v2/Users/{id}. An omitted active
// keeps the current state, so a PUT that leaves it out changes no status.
func (s *Server) handleSCIMReplaceUser(w http.ResponseWriter, r *http.Request) {
	c, ok := s.scimCaller(w, r)
	if !ok {
		return
	}
	var in scimUserIn
	if !scimDecode(w, r, &in) {
		return
	}
	ctx := r.Context()
	row, u, cur, err := s.currentSCIMUser(ctx, c, r.PathValue("id"))
	if err != nil {
		s.scimNotFoundOr500(w, err, "User")
		return
	}
	s.applySCIMUser(w, r, c, row, u, cur, in.state(cur.active))
}

// scimPatch is an RFC 7644 §3.5.2 PATCH request.
type scimPatch struct {
	Schemas    []string `json:"schemas"`
	Operations []struct {
		Op    string          `json:"op"`
		Path  string          `json:"path"`
		Value json.RawMessage `json:"value"`
	} `json:"Operations"`
}

// handleSCIMPatchUser serves PATCH /scim/v2/Users/{id}.
func (s *Server) handleSCIMPatchUser(w http.ResponseWriter, r *http.Request) {
	c, ok := s.scimCaller(w, r)
	if !ok {
		return
	}
	var p scimPatch
	if !scimDecode(w, r, &p) {
		return
	}
	if len(p.Operations) == 0 || len(p.Operations) > scimMaxPatchOps {
		scimError(w, http.StatusBadRequest, "invalidValue", fmt.Sprintf("a PATCH needs 1 to %d operations", scimMaxPatchOps))
		return
	}
	ctx := r.Context()
	row, u, cur, err := s.currentSCIMUser(ctx, c, r.PathValue("id"))
	if err != nil {
		s.scimNotFoundOr500(w, err, "User")
		return
	}
	next := cur
	next.attrs.Emails = slices.Clone(cur.attrs.Emails)
	if cur.attrs.Name != nil {
		n := *cur.attrs.Name
		next.attrs.Name = &n
	}
	for _, op := range p.Operations {
		if err := patchSCIMUser(&next, strings.ToLower(op.Op), op.Path, op.Value); err != nil {
			scimError(w, http.StatusBadRequest, "invalidPath", err.Error())
			return
		}
	}
	s.applySCIMUser(w, r, c, row, u, cur, next)
}

// patchSCIMUser applies one operation to st.
func patchSCIMUser(st *scimUserState, op, path string, value json.RawMessage) error {
	switch op {
	case "add", "replace", "remove":
	default:
		return fmt.Errorf("op %q is not add, replace or remove", op)
	}
	if path == "" {
		if op == "remove" {
			return errors.New("remove needs a path")
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(value, &obj); err != nil {
			return errors.New("a PATCH without a path needs an object value")
		}
		for k, v := range obj {
			if err := patchSCIMUser(st, op, k, v); err != nil {
				return err
			}
		}
		return nil
	}
	str := func() (string, error) {
		if op == "remove" {
			return "", nil
		}
		var v string
		if err := json.Unmarshal(value, &v); err != nil {
			return "", fmt.Errorf("%s needs a string", path)
		}
		return v, nil
	}
	name := func() *scimName {
		if st.attrs.Name == nil {
			st.attrs.Name = &scimName{}
		}
		return st.attrs.Name
	}
	var err error
	switch lp := strings.ToLower(path); lp {
	case "active":
		if op == "remove" {
			return errors.New("active cannot be removed")
		}
		// Some providers send "False" as a string.
		var b bool
		if json.Unmarshal(value, &b) != nil {
			var sv string
			if json.Unmarshal(value, &sv) != nil {
				return errors.New("active needs a boolean")
			}
			if b, err = strconv.ParseBool(sv); err != nil {
				return errors.New("active needs a boolean")
			}
		}
		st.active = b
	case "username":
		if op == "remove" {
			return errors.New("userName cannot be removed")
		}
		st.userName, err = str()
	case "externalid":
		st.externalID, err = str()
	case "displayname":
		st.attrs.DisplayName, err = str()
	case "name.givenname":
		name().GivenName, err = str()
	case "name.familyname":
		name().FamilyName, err = str()
	case "name.formatted":
		name().Formatted, err = str()
	case "name":
		if op == "remove" {
			st.attrs.Name = nil
			return nil
		}
		var n scimName
		if json.Unmarshal(value, &n) != nil {
			return errors.New("name needs an object")
		}
		st.attrs.Name = &n
	case "emails":
		if op == "remove" {
			st.attrs.Emails = nil
			return nil
		}
		var es []scimEmail
		if json.Unmarshal(value, &es) != nil {
			return errors.New("emails needs an array")
		}
		if op == "add" {
			st.attrs.Emails = append(st.attrs.Emails, es...)
		} else {
			st.attrs.Emails = es
		}
	case strings.ToLower(scimSchemaExt), strings.ToLower(scimSchemaExt + ":accessMode"):
		if op == "remove" {
			return errors.New("accessMode cannot be removed")
		}
		if lp == strings.ToLower(scimSchemaExt) {
			var e scimExt
			if json.Unmarshal(value, &e) != nil {
				return errors.New("the loomcycle extension needs an object")
			}
			st.accessMode = e.AccessMode
			return nil
		}
		st.accessMode, err = str()
	case "groups":
		return errors.New("groups is read-only; change memberships on the Group")
	default:
		// An attribute this service does not keep (a title, the enterprise
		// extension) is ignored, as the attributes of a POST are: nothing
		// here grants on an attribute it does not know.
	}
	return err
}

// scimRefusal is a refusal decided before any write, written as a SCIM error.
type scimRefusal struct {
	status   int
	scimType string
	detail   string
}

// applySCIMUser writes next over cur. Every refusal is decided before any
// attribute is written: an access mode an untrusted gate may not assert, a
// userName another resource holds, a removal the gate is not authoritative
// for or that is over its ceiling, a re-enable of a user this gate did not
// disable. The status change goes first and the attributes after it; only a
// userName taken by a concurrent request between the check and the write
// can still answer 409 after the status changed (the provider's retry then
// finds the status already applied).
func (s *Server) applySCIMUser(w http.ResponseWriter, r *http.Request, c scimCtx, row store.SCIMUserRow, u store.UserRow, cur, next scimUserState) {
	ctx := r.Context()
	key, err := next.validate()
	if err != nil {
		scimError(w, http.StatusBadRequest, "invalidValue", err.Error())
		return
	}
	if !c.accessModeAllowed(next.accessMode, u.AccessMode) {
		scimError(w, http.StatusForbidden, "", "changing accessMode needs a trusted SCIM gate")
		return
	}
	disable := !next.active && u.Status != "disabled"
	reactivate := next.active && u.Status == "disabled"
	if disable && !c.g.Identity.AuthoritativeForUsers {
		scimError(w, http.StatusForbidden, "", "this SCIM gate is not authoritative for users (identity_map.authoritative_for_users)")
		return
	}
	fail := func(err error) {
		slog.Error("scim: updating a user failed", "gate", c.g.Name, "err", err)
		scimError(w, http.StatusInternalServerError, "", "the request could not be completed; repeat it")
	}
	attrs, err := json.Marshal(next.attrs)
	if err != nil {
		scimError(w, http.StatusInternalServerError, "", "could not store the user")
		return
	}
	if key != row.UserNameKey {
		held, _, err := s.store.SCIMUserList(ctx, c.g.Name, c.tenant, store.SCIMFilter{UserNameKey: key}, 0, 1)
		if err != nil {
			fail(err)
			return
		}
		if len(held) > 0 && held[0].SCIMID != row.SCIMID {
			scimError(w, http.StatusConflict, "uniqueness", "a user with this userName exists")
			return
		}
	}
	if reactivate {
		// Back to the status it had when SCIM removed it: pending stays
		// pending (the one rule for every path: SCIM never approves).
		restored, ref := s.scimReactivate(ctx, c, row.Subject)
		if ref != nil {
			if ref.status == http.StatusForbidden {
				s.scimAudit(c, "auth_scim_status_refused", row.Subject, "re-enable of a user SCIM did not disable")
			}
			scimError(w, ref.status, ref.scimType, ref.detail)
			return
		}
		u.Status = restored
	}
	if disable {
		if ref := s.scimRemove(ctx, c, row.Subject); ref != nil {
			scimError(w, ref.status, ref.scimType, ref.detail)
			return
		}
	}
	upd := row
	upd.UserName, upd.UserNameKey, upd.ExternalID, upd.Attributes = next.userName, key, next.externalID, string(attrs)
	if err := s.store.SCIMUserUpdate(ctx, upd); err != nil {
		var conflict *store.ErrConflict
		if errors.As(err, &conflict) {
			scimError(w, http.StatusConflict, "uniqueness", "a user with this userName exists")
			return
		}
		fail(err)
		return
	}
	if dn := next.displayName(); dn != u.DisplayName {
		if err := s.store.UserUpdate(ctx, c.tenant, row.Subject, store.UserPatch{DisplayName: &dn}); err != nil {
			fail(err)
			return
		}
	}
	if next.active && u.Status == store.UserStatusPending {
		// Approval is an operator's: SCIM's active:true leaves a pending
		// user pending (the resource reads active:false until approved).
		s.scimAudit(c, "auth_scim_status_not_applied", row.Subject, "pending users are approved by an operator")
	}
	if u.Status != "disabled" && !disable {
		if err := s.scimSyncUser(ctx, c, row, next); err != nil {
			slog.Error("scim: syncing a user failed", "gate", c.g.Name, "err", err)
			scimError(w, http.StatusInternalServerError, "", "the change was applied in part; repeat it")
			return
		}
	}
	s.writeSCIMUser(w, ctx, c, row.SCIMID, http.StatusOK)
}

// scimSyncUser runs the user through the gate's provisioning again, as an
// external gate's sync does: a trusted gate's access mode, and the groups
// the user's SCIM memberships assert.
func (s *Server) scimSyncUser(ctx context.Context, c scimCtx, row store.SCIMUserRow, st scimUserState) error {
	groups, err := s.scimAssertedGroups(ctx, c, row.Subject)
	if err != nil {
		return err
	}
	_, err = s.provisionOrRefuse(ctx, c.g, st.assertion(c, row.SCIMID, groups), "")
	if errors.Is(err, errUserDisabled) {
		return nil
	}
	return err
}

// scimAssertedGroups is the provider group names (the SCIM groups' keys) a
// subject is a member of.
func (s *Server) scimAssertedGroups(ctx context.Context, c scimCtx, subject string) ([]string, error) {
	gs, err := s.store.SCIMGroupsOfSubject(ctx, c.g.Name, c.tenant, subject)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(gs))
	for _, g := range gs {
		out = append(out, g.DisplayKey)
	}
	if len(out) > identity.MaxGroups {
		slog.Warn("scim: a user is in more groups than one sync carries; the rest are not synced",
			"gate", c.g.Name, "groups", len(out), "max", identity.MaxGroups)
	}
	return out, nil
}

// scimRemove is a SCIM gate's removal of a user: the gate must be
// authoritative for its users; then one store operation, all or nothing and
// atomic under the gate's ceiling, disables the user and records the removal
// with the status it had. started is false when the user was disabled
// already (by SCIM before, or by someone else — then SCIM never owns that
// disable). The follow-ups (sessions end, delegated tokens retire) run
// whenever the user is disabled under this gate's removal, so a retry
// finishes what a failed request left.
func (s *Server) scimRemove(ctx context.Context, c scimCtx, subject string) *scimRefusal {
	if !c.g.Identity.AuthoritativeForUsers {
		return &scimRefusal{http.StatusForbidden, "", "this SCIM gate is not authoritative for users (identity_map.authoritative_for_users)"}
	}
	internal := &scimRefusal{http.StatusInternalServerError, "", "the user could not be deactivated; repeat it"}
	now := time.Now().UTC()
	since := now.Add(-config.SCIMRemovalWindow)
	// A percentage is of the users the gate had before the window: a
	// provider cannot raise its own ceiling by creating users first.
	base, err := s.store.SCIMRemovalBase(ctx, c.g.Name, c.tenant, since)
	if err != nil {
		return internal
	}
	rp := c.g.SCIM.Removal
	row := removalRow(lifecycle.Policy{UnbindAfter: rp.UnbindAfter, EraseAfter: rp.EraseAfter}.Start(now),
		c.tenant, subject, c.g.Name, store.RemovalReasonSCIM, "gate:"+c.g.Name)
	ceiling := c.g.SCIM.RemovalCeiling(base)
	started, err := s.store.SCIMRemoveUser(ctx, row, since, ceiling)
	switch {
	case errors.Is(err, store.ErrSCIMRemovalCeiling):
		s.scimAudit(c, "auth_gate_sync_refused", subject, fmt.Sprintf("removal over the ceiling of %d in 24h", ceiling))
		return &scimRefusal{http.StatusTooManyRequests, "", fmt.Sprintf(
			"this SCIM gate has removed %d users in the last 24 hours, its ceiling (max_sync_removals); try again later", ceiling)}
	case isNotFound(err):
		return nil // the user is gone already: nothing to disable
	case err != nil:
		slog.Error("scim: removing a user failed", "gate", c.g.Name, "err", err)
		return internal
	}
	// Fail closed: whatever the store answered, the user must be disabled
	// now, or the provider is not told it was deprovisioned.
	if u, err := s.store.UserGet(ctx, c.tenant, subject); err != nil || u.Status != "disabled" {
		if err == nil || !isNotFound(err) {
			slog.Error("scim: a removed user is not disabled", "gate", c.g.Name, "err", err)
			return internal
		}
		return nil
	}
	rm, err := s.store.AuthRemovalGet(ctx, c.tenant, subject)
	if err != nil && !isNotFound(err) {
		return internal
	}
	if err == nil && rm.Gate == c.g.Name && store.SCIMRemovalReason(rm.Reason) {
		retired, err := s.retireDelegatedUserTokens(ctx, c.tenant, subject)
		for _, t := range retired {
			s.recordTokenAudit(ctx, "retire", t)
		}
		if err == nil {
			_, err = s.store.UISessionRevokeSubject(ctx, c.tenant, subject, now)
		}
		if err != nil {
			slog.Error("scim: ending a removed user's credentials failed", "gate", c.g.Name, "err", err)
			return internal
		}
	}
	if started {
		s.recordLifecycleEvent("auth_user_disabled", c.tenant, subject, c.g.Name, rm.Reason, "gate:"+c.g.Name)
		s.scimAudit(c, "auth_scim_user_deactivated", subject, "")
	}
	return nil
}

// scimReactivate re-enables a user whose CURRENT removal this gate's SCIM
// made, back to the status it had then (pending stays pending), at the
// §3.7a floor. An operator's or another gate's removal, or a disable with no
// removal, stands: 403. Nothing that gives access happens before the
// re-enable is SCIM's for good: the removal is CLAIMED in one store
// transaction (only a removal of this gate's SCIM claims); the user, still
// disabled, is stripped (memberships, bindings, access mode down to the
// floor's) — which only ever takes away; a second transaction re-enables
// only while the claim stands; and only then does the user join the floor
// groups. An operator who takes the removal over in between keeps a
// disabled user with less than it had, never more. A failure part-way
// leaves the claim, and the provider's retry continues it.
func (s *Server) scimReactivate(ctx context.Context, c scimCtx, subject string) (string, *scimRefusal) {
	refused := &scimRefusal{http.StatusForbidden, "", "this user was disabled outside SCIM; an operator re-enables it"}
	internal := &scimRefusal{http.StatusInternalServerError, "", "the request could not be completed"}
	claimed, err := s.store.SCIMReactivateClaim(ctx, c.g.Name, c.tenant, subject)
	switch {
	case err != nil:
		slog.Error("scim: claiming a removal failed", "gate", c.g.Name, "err", err)
		return "", internal
	case !claimed:
		return "", refused
	}
	if err := s.stripRemovedUser(ctx, c.tenant, subject); err != nil {
		slog.Error("scim: resetting a user to the floor failed", "gate", c.g.Name, "err", err)
		return "", internal
	}
	restored, ok, err := s.store.SCIMReactivate(ctx, c.g.Name, c.tenant, subject)
	switch {
	case err != nil:
		return "", internal
	case !ok:
		return "", refused
	}
	if err := s.rejoinFloorGroups(ctx, c.tenant, subject, "gate:"+c.g.Name); err != nil {
		// The user is back with fewer groups than the floor: less, never
		// more. An operator adds them.
		slog.Error("scim: a re-enabled user did not join the floor groups", "gate", c.g.Name, "err", err)
		return "", internal
	}
	s.recordLifecycleEvent("auth_user_reactivated", c.tenant, subject, c.g.Name, store.RemovalReasonSCIM, "gate:"+c.g.Name)
	return restored, nil
}

// handleSCIMDeleteUser serves DELETE /scim/v2/Users/{id}: the removal
// policy's first stage (the user is disabled and kept), then the resource
// ends. Nothing is hard-deleted.
func (s *Server) handleSCIMDeleteUser(w http.ResponseWriter, r *http.Request) {
	c, ok := s.scimCaller(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	row, err := s.store.SCIMUserGet(ctx, c.g.Name, c.tenant, r.PathValue("id"))
	if err != nil {
		s.scimNotFoundOr500(w, err, "User")
		return
	}
	if ref := s.scimRemove(ctx, c, row.Subject); ref != nil {
		scimError(w, ref.status, ref.scimType, ref.detail)
		return
	}
	if _, err := s.store.SCIMUserMarkDeleted(ctx, c.g.Name, c.tenant, row.SCIMID, time.Now().UTC()); err != nil {
		s.scimNotFoundOr500(w, err, "User")
		return
	}
	s.scimAudit(c, "auth_scim_user_deleted", row.Subject, "")
	w.WriteHeader(http.StatusNoContent)
}

// ---- groups ----

type scimMember struct {
	Value string `json:"value"`
}

type scimGroupIn struct {
	DisplayName string       `json:"displayName"`
	ExternalID  string       `json:"externalId"`
	Members     []scimMember `json:"members"`
}

// scimGroupKey is a group's provider name: its display name as a slug
// (lower case, every run of other characters one '-'), so "Sales EMEA" is
// "sales-emea" in identity_map.groups.map and in its "<gate>:<key>"
// external id. "" when nothing usable is left.
func scimGroupKey(display string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(display)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '.' {
			b.WriteRune(r)
			dash = false
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	key := strings.TrimRight(b.String(), "-")
	if len(key) > scimMaxGroupKey {
		return ""
	}
	return key
}

// scimGroupsEnabled reports whether the gate syncs groups (identity_map.groups
// on a trusted gate); without it the Group endpoints refuse writes.
func (c scimCtx) scimGroupsEnabled() bool { return c.g.Identity.SyncGroups && c.g.Identity.Trusted }

func refuseSCIMGroups(w http.ResponseWriter) {
	scimError(w, http.StatusForbidden, "", "group provisioning needs identity_map.groups on a trusted SCIM gate")
}

func (s *Server) scimGroupOut(ctx context.Context, c scimCtx, g store.SCIMGroupRow, withMembers bool) (map[string]any, error) {
	out := map[string]any{"schemas": []string{scimSchemaGroup}, "id": g.SCIMID, "displayName": g.DisplayName,
		"meta": map[string]any{"resourceType": "Group", "created": g.CreatedAt.UTC(), "lastModified": g.UpdatedAt.UTC(),
			"location": s.scimLocation("Groups", g.SCIMID), "version": fmt.Sprintf(`W/"%d"`, g.Version)}}
	if g.ExternalID != "" {
		out["externalId"] = g.ExternalID
	}
	if withMembers {
		subs, err := s.store.SCIMGroupMembers(ctx, c.g.Name, c.tenant, g.SCIMID)
		if err != nil {
			return nil, err
		}
		ms := make([]map[string]string, 0, len(subs))
		for _, sub := range subs {
			if id := scimIDOf(c, sub); id != "" {
				ms = append(ms, map[string]string{"value": id, "$ref": s.scimLocation("Users", id)})
			}
		}
		out["members"] = ms
	}
	return out, nil
}

func excludesMembers(r *http.Request) bool {
	for _, a := range strings.Split(r.URL.Query().Get("excludedAttributes"), ",") {
		if strings.EqualFold(strings.TrimSpace(a), "members") {
			return true
		}
	}
	return false
}

func (s *Server) writeSCIMGroup(w http.ResponseWriter, r *http.Request, c scimCtx, id string, status int) {
	ctx := r.Context()
	g, err := s.store.SCIMGroupGet(ctx, c.g.Name, c.tenant, id)
	if err != nil {
		s.scimNotFoundOr500(w, err, "Group")
		return
	}
	out, err := s.scimGroupOut(ctx, c, g, !excludesMembers(r))
	if err != nil {
		s.scimNotFoundOr500(w, err, "Group")
		return
	}
	if status == http.StatusCreated {
		w.Header().Set("Location", s.scimLocation("Groups", id))
	}
	scimJSON(w, status, out)
}

// scimMemberSubjects resolves member values (SCIM user ids) to subjects, in
// one batched read. An unknown id — another tenant's, another gate's, a
// deleted one — is an error, never a member. budget is what is left of the
// request's member references (scimMaxGroupMembers across all of its
// operations); it is charged here, before any read.
func (s *Server) scimMemberSubjects(ctx context.Context, c scimCtx, members []scimMember, budget *int) ([]string, error) {
	if len(members) > *budget {
		return nil, fmt.Errorf("at most %d member references per request: %w", scimMaxGroupMembers, errSCIMMemberBudget)
	}
	*budget -= len(members)
	ids := make([]string, len(members))
	for i, m := range members {
		ids[i] = m.Value
	}
	rows, err := s.store.SCIMUsersGetMany(ctx, c.g.Name, c.tenant, ids)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(members))
	for _, id := range ids {
		row, ok := rows[id]
		if !ok {
			return nil, fmt.Errorf("member %q is not a user of this service: %w", id, errSCIMUnknownMember)
		}
		out = append(out, row.Subject)
	}
	return out, nil
}

// errSCIMUnknownMember: a member value that is not a live user resource of
// the gate in the tenant.
var errSCIMUnknownMember = errors.New("unknown member")

// errSCIMMemberBudget: a request that names more members than one request
// may resolve.
var errSCIMMemberBudget = errors.New("too many members")

// scimMemberError writes a member resolution failure: the caller's mistake
// is a 400, a store failure a 500.
func scimMemberError(w http.ResponseWriter, err error) {
	if errors.Is(err, errSCIMUnknownMember) || errors.Is(err, errSCIMMemberBudget) {
		scimError(w, http.StatusBadRequest, "invalidValue", err.Error())
		return
	}
	slog.Error("scim: resolving group members failed", "err", err)
	scimError(w, http.StatusInternalServerError, "", "could not read the members")
}

// scimResync re-runs the gate's group sync for subjects whose SCIM
// memberships changed. A disabled user is skipped: its removal policy owns
// its memberships (the unbind stage), and a sync must not undo it. The
// tenant's group list is read once for all of them (again only after a sync
// created a group), not once per member.
func (s *Server) scimResync(ctx context.Context, c scimCtx, subjects []string) error {
	var all []store.GroupRow
	stale := true
	for _, sub := range subjects {
		u, err := s.store.UserGet(ctx, c.tenant, sub)
		if err != nil {
			if isNotFound(err) {
				continue
			}
			return err
		}
		if u.Status == "disabled" {
			continue
		}
		groups, err := s.scimAssertedGroups(ctx, c, sub)
		if err != nil {
			return err
		}
		asserted := identity.AssertedGroups(c.g.Identity, authgate.Assertion{Groups: groups})
		if stale {
			if all, err = s.store.GroupList(ctx, c.tenant); err != nil {
				return err
			}
		}
		_, _, created, err := s.syncGroupsWith(ctx, c.g, c.tenant, sub, asserted, all)
		if err != nil {
			return err
		}
		stale = created
	}
	return nil
}

func validSCIMGroup(in *scimGroupIn) (string, error) {
	in.DisplayName = strings.TrimSpace(in.DisplayName)
	if in.DisplayName == "" {
		return "", errors.New("displayName is required")
	}
	if err := boundStr("displayName", in.DisplayName); err != nil {
		return "", err
	}
	if err := boundStr("externalId", in.ExternalID); err != nil {
		return "", err
	}
	key := scimGroupKey(in.DisplayName)
	if key == "" {
		return "", fmt.Errorf("displayName must have letters or digits and at most %d of them", scimMaxGroupKey)
	}
	return key, nil
}

// handleSCIMCreateGroup serves POST /scim/v2/Groups.
func (s *Server) handleSCIMCreateGroup(w http.ResponseWriter, r *http.Request) {
	c, ok := s.scimCaller(w, r)
	if !ok {
		return
	}
	if !c.scimGroupsEnabled() {
		refuseSCIMGroups(w)
		return
	}
	var in scimGroupIn
	if !scimDecode(w, r, &in) {
		return
	}
	key, err := validSCIMGroup(&in)
	if err != nil {
		scimError(w, http.StatusBadRequest, "invalidValue", err.Error())
		return
	}
	ctx := r.Context()
	budget := scimMaxGroupMembers
	subs, err := s.scimMemberSubjects(ctx, c, in.Members, &budget)
	if err != nil {
		scimMemberError(w, err)
		return
	}
	id, err := newSCIMID()
	if err != nil {
		scimError(w, http.StatusInternalServerError, "", "could not create the group")
		return
	}
	g := store.SCIMGroupRow{Gate: c.g.Name, TenantID: c.tenant, SCIMID: id, DisplayName: in.DisplayName, DisplayKey: key, ExternalID: in.ExternalID}
	if err := s.store.SCIMGroupCreate(ctx, g); err != nil {
		var conflict *store.ErrConflict
		if errors.As(err, &conflict) {
			scimError(w, http.StatusConflict, "uniqueness", "a group with this displayName exists")
			return
		}
		scimError(w, http.StatusInternalServerError, "", "could not create the group")
		return
	}
	added, err := s.store.SCIMGroupMembersAdd(ctx, c.g.Name, c.tenant, id, subs)
	if err == nil {
		err = s.scimResync(ctx, c, added)
	}
	if err != nil {
		slog.Error("scim: adding group members failed", "gate", c.g.Name, "err", err)
		scimError(w, http.StatusInternalServerError, "", "the group was created but its members were not all applied; repeat with PUT")
		return
	}
	s.scimAudit(c, "auth_scim_group_created", "", key)
	s.writeSCIMGroup(w, r, c, id, http.StatusCreated)
}

// handleSCIMGetGroup serves GET /scim/v2/Groups/{id}.
func (s *Server) handleSCIMGetGroup(w http.ResponseWriter, r *http.Request) {
	c, ok := s.scimCaller(w, r)
	if !ok {
		return
	}
	s.writeSCIMGroup(w, r, c, r.PathValue("id"), http.StatusOK)
}

// handleSCIMListGroups serves GET /scim/v2/Groups[?filter=displayName eq "x"].
func (s *Server) handleSCIMListGroups(w http.ResponseWriter, r *http.Request) {
	c, ok := s.scimCaller(w, r)
	if !ok {
		return
	}
	f, err := parseSCIMFilter(r.URL.Query().Get("filter"), true)
	if err != nil {
		scimError(w, http.StatusBadRequest, "invalidFilter", err.Error())
		return
	}
	offset, limit := scimPage(r)
	ctx := r.Context()
	rows, total, err := s.store.SCIMGroupList(ctx, c.g.Name, c.tenant, f, offset, limit)
	if err != nil {
		s.scimNotFoundOr500(w, err, "Group")
		return
	}
	withMembers := !excludesMembers(r)
	out := make([]map[string]any, 0, len(rows))
	for _, g := range rows {
		o, err := s.scimGroupOut(ctx, c, g, withMembers)
		if err != nil {
			s.scimNotFoundOr500(w, err, "Group")
			return
		}
		out = append(out, o)
	}
	scimList(w, total, offset, out)
}

// diffSubjects is what turns have into want.
func diffSubjects(have, want []string) (add, remove []string) {
	for _, s := range want {
		if !slices.Contains(have, s) && !slices.Contains(add, s) {
			add = append(add, s)
		}
	}
	for _, s := range have {
		if !slices.Contains(want, s) {
			remove = append(remove, s)
		}
	}
	return add, remove
}

// applySCIMGroup writes a group's new display name, external id and member
// set, then re-syncs everyone whose assertion changed: the added and removed
// members, and every member when the name (the provider group) changed.
func (s *Server) applySCIMGroup(w http.ResponseWriter, r *http.Request, c scimCtx, g store.SCIMGroupRow, next scimGroupIn, want []string) {
	ctx := r.Context()
	key, err := validSCIMGroup(&next)
	if err != nil {
		scimError(w, http.StatusBadRequest, "invalidValue", err.Error())
		return
	}
	if len(want) > scimMaxGroupMembers {
		scimError(w, http.StatusBadRequest, "invalidValue", fmt.Sprintf("at most %d members", scimMaxGroupMembers))
		return
	}
	have, err := s.store.SCIMGroupMembers(ctx, c.g.Name, c.tenant, g.SCIMID)
	if err != nil {
		s.scimNotFoundOr500(w, err, "Group")
		return
	}
	renamed := key != g.DisplayKey
	if next.DisplayName != g.DisplayName || next.ExternalID != g.ExternalID {
		upd := g
		upd.DisplayName, upd.DisplayKey, upd.ExternalID = next.DisplayName, key, next.ExternalID
		if err := s.store.SCIMGroupUpdate(ctx, upd); err != nil {
			var conflict *store.ErrConflict
			if errors.As(err, &conflict) {
				scimError(w, http.StatusConflict, "uniqueness", "a group with this displayName exists")
				return
			}
			s.scimNotFoundOr500(w, err, "Group")
			return
		}
	}
	add, remove := diffSubjects(have, want)
	removed, err := s.store.SCIMGroupMembersRemove(ctx, c.g.Name, c.tenant, g.SCIMID, remove)
	if err != nil {
		s.scimNotFoundOr500(w, err, "Group")
		return
	}
	added, err := s.store.SCIMGroupMembersAdd(ctx, c.g.Name, c.tenant, g.SCIMID, add)
	if err != nil {
		s.scimNotFoundOr500(w, err, "Group")
		return
	}
	affected := append(slices.Clone(removed), added...)
	if renamed {
		affected = append(affected, want...)
	}
	slices.Sort(affected)
	if err := s.scimResync(ctx, c, slices.Compact(affected)); err != nil {
		slog.Error("scim: re-syncing group members failed", "gate", c.g.Name, "err", err)
		scimError(w, http.StatusInternalServerError, "", "the change was applied in part; repeat it")
		return
	}
	s.writeSCIMGroup(w, r, c, g.SCIMID, http.StatusOK)
}

// handleSCIMReplaceGroup serves PUT /scim/v2/Groups/{id}.
func (s *Server) handleSCIMReplaceGroup(w http.ResponseWriter, r *http.Request) {
	c, ok := s.scimCaller(w, r)
	if !ok {
		return
	}
	if !c.scimGroupsEnabled() {
		refuseSCIMGroups(w)
		return
	}
	var in scimGroupIn
	if !scimDecode(w, r, &in) {
		return
	}
	ctx := r.Context()
	g, err := s.store.SCIMGroupGet(ctx, c.g.Name, c.tenant, r.PathValue("id"))
	if err != nil {
		s.scimNotFoundOr500(w, err, "Group")
		return
	}
	budget := scimMaxGroupMembers
	want, err := s.scimMemberSubjects(ctx, c, in.Members, &budget)
	if err != nil {
		scimMemberError(w, err)
		return
	}
	s.applySCIMGroup(w, r, c, g, in, want)
}

var memberFilterRE = regexp.MustCompile(`^(?i)members\[\s*value\s+eq\s+"([^"]*)"\s*\]$`)

// handleSCIMPatchGroup serves PATCH /scim/v2/Groups/{id}: displayName,
// externalId, and members add / replace / remove (by list, or one by
// `members[value eq "<id>"]`).
func (s *Server) handleSCIMPatchGroup(w http.ResponseWriter, r *http.Request) {
	c, ok := s.scimCaller(w, r)
	if !ok {
		return
	}
	if !c.scimGroupsEnabled() {
		refuseSCIMGroups(w)
		return
	}
	var p scimPatch
	if !scimDecode(w, r, &p) {
		return
	}
	if len(p.Operations) == 0 || len(p.Operations) > scimMaxPatchOps {
		scimError(w, http.StatusBadRequest, "invalidValue", fmt.Sprintf("a PATCH needs 1 to %d operations", scimMaxPatchOps))
		return
	}
	ctx := r.Context()
	g, err := s.store.SCIMGroupGet(ctx, c.g.Name, c.tenant, r.PathValue("id"))
	if err != nil {
		s.scimNotFoundOr500(w, err, "Group")
		return
	}
	members, err := s.store.SCIMGroupMembers(ctx, c.g.Name, c.tenant, g.SCIMID)
	if err != nil {
		s.scimNotFoundOr500(w, err, "Group")
		return
	}
	budget := scimMaxGroupMembers
	next := scimGroupIn{DisplayName: g.DisplayName, ExternalID: g.ExternalID}
	for _, op := range p.Operations {
		o, path := strings.ToLower(op.Op), strings.TrimSpace(op.Path)
		if o != "add" && o != "replace" && o != "remove" {
			scimError(w, http.StatusBadRequest, "invalidValue", fmt.Sprintf("op %q is not add, replace or remove", op.Op))
			return
		}
		switch {
		case path == "" && o != "remove":
			var in scimGroupIn
			if json.Unmarshal(op.Value, &in) != nil {
				scimError(w, http.StatusBadRequest, "invalidValue", "a PATCH without a path needs an object value")
				return
			}
			if in.DisplayName != "" {
				next.DisplayName = in.DisplayName
			}
			if in.ExternalID != "" {
				next.ExternalID = in.ExternalID
			}
			if in.Members != nil {
				subs, err := s.scimMemberSubjects(ctx, c, in.Members, &budget)
				if err != nil {
					scimMemberError(w, err)
					return
				}
				members = mergeMembers(members, subs, o == "replace")
			}
		case strings.EqualFold(path, "displayName") && o != "remove":
			if json.Unmarshal(op.Value, &next.DisplayName) != nil {
				scimError(w, http.StatusBadRequest, "invalidValue", "displayName needs a string")
				return
			}
		case strings.EqualFold(path, "externalId"):
			next.ExternalID = ""
			if o != "remove" && json.Unmarshal(op.Value, &next.ExternalID) != nil {
				scimError(w, http.StatusBadRequest, "invalidValue", "externalId needs a string")
				return
			}
		case strings.EqualFold(path, "members"):
			var ms []scimMember
			if len(op.Value) > 0 && string(op.Value) != "null" {
				if json.Unmarshal(op.Value, &ms) != nil {
					scimError(w, http.StatusBadRequest, "invalidValue", "members needs an array")
					return
				}
			}
			if o == "remove" && len(ms) == 0 {
				members = nil // remove every member
				continue
			}
			subs, err := s.scimMemberSubjects(ctx, c, ms, &budget)
			if err != nil {
				scimMemberError(w, err)
				return
			}
			if o == "remove" {
				members = slices.DeleteFunc(members, func(m string) bool { return slices.Contains(subs, m) })
			} else {
				members = mergeMembers(members, subs, o == "replace")
			}
		case o == "remove" && memberFilterRE.MatchString(path):
			sub := scimSubject(c, memberFilterRE.FindStringSubmatch(path)[1])
			members = slices.DeleteFunc(members, func(m string) bool { return m == sub })
		default:
			scimError(w, http.StatusBadRequest, "invalidPath", fmt.Sprintf("op %q on path %q is not supported", op.Op, op.Path))
			return
		}
		if len(members) > scimMaxGroupMembers {
			scimError(w, http.StatusBadRequest, "invalidValue", fmt.Sprintf("at most %d members", scimMaxGroupMembers))
			return
		}
	}
	s.applySCIMGroup(w, r, c, g, next, members)
}

func mergeMembers(cur, subs []string, replace bool) []string {
	if replace {
		return slices.Clone(subs)
	}
	out := slices.Clone(cur)
	for _, s := range subs {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// handleSCIMDeleteGroup serves DELETE /scim/v2/Groups/{id}: the resource and
// its memberships go, and its former members are re-synced (they leave the
// "<gate>:<key>" group). The loomcycle group itself stays, as with every
// gate: its bindings are the operator's.
func (s *Server) handleSCIMDeleteGroup(w http.ResponseWriter, r *http.Request) {
	c, ok := s.scimCaller(w, r)
	if !ok {
		return
	}
	if !c.scimGroupsEnabled() {
		refuseSCIMGroups(w)
		return
	}
	ctx := r.Context()
	id := r.PathValue("id")
	members, err := s.store.SCIMGroupMembers(ctx, c.g.Name, c.tenant, id)
	if err != nil {
		s.scimNotFoundOr500(w, err, "Group")
		return
	}
	ok, err = s.store.SCIMGroupDelete(ctx, c.g.Name, c.tenant, id)
	if err != nil {
		s.scimNotFoundOr500(w, err, "Group")
		return
	}
	if !ok {
		scimError(w, http.StatusNotFound, "", "Group not found")
		return
	}
	if err := s.scimResync(ctx, c, members); err != nil {
		slog.Error("scim: re-syncing a deleted group's members failed", "gate", c.g.Name, "err", err)
		scimError(w, http.StatusInternalServerError, "", "the group was deleted but its members were not all re-synced")
		return
	}
	s.scimAudit(c, "auth_scim_group_deleted", "", id)
	w.WriteHeader(http.StatusNoContent)
}

// ---- discovery ----

func (s *Server) handleSCIMServiceProviderConfig(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.scimCaller(w, r); !ok {
		return
	}
	scimJSON(w, http.StatusOK, map[string]any{
		"schemas":        []string{scimSchemaSPC},
		"patch":          map[string]bool{"supported": true},
		"bulk":           map[string]any{"supported": false, "maxOperations": 0, "maxPayloadSize": 0},
		"filter":         map[string]any{"supported": true, "maxResults": scimMaxResults},
		"changePassword": map[string]bool{"supported": false},
		"sort":           map[string]bool{"supported": false},
		"etag":           map[string]bool{"supported": false},
		"authenticationSchemes": []map[string]any{{"type": "oauthbearertoken", "name": "Bearer token",
			"description": "A loomcycle token holding only the " + auth.ScopeSCIMProvision + " scope", "primary": true}},
		"meta": map[string]string{"resourceType": "ServiceProviderConfig"},
	})
}

func scimResourceTypes() []map[string]any {
	return []map[string]any{
		{"schemas": []string{scimSchemaResType}, "id": "User", "name": "User", "endpoint": "/Users", "schema": scimSchemaUser,
			"schemaExtensions": []map[string]any{{"schema": scimSchemaExt, "required": false}},
			"meta":             map[string]string{"resourceType": "ResourceType"}},
		{"schemas": []string{scimSchemaResType}, "id": "Group", "name": "Group", "endpoint": "/Groups", "schema": scimSchemaGroup,
			"meta": map[string]string{"resourceType": "ResourceType"}},
	}
}

func scimAttr(name, typ string, extra map[string]any) map[string]any {
	a := map[string]any{"name": name, "type": typ, "multiValued": false, "required": false, "caseExact": false,
		"mutability": "readWrite", "returned": "default", "uniqueness": "none"}
	for k, v := range extra {
		a[k] = v
	}
	return a
}

func scimSchemas() []map[string]any {
	return []map[string]any{
		{"schemas": []string{scimSchemaSchema}, "id": scimSchemaUser, "name": "User", "attributes": []map[string]any{
			scimAttr("userName", "string", map[string]any{"required": true, "uniqueness": "server"}),
			scimAttr("externalId", "string", map[string]any{"caseExact": true}),
			scimAttr("displayName", "string", nil),
			scimAttr("name", "complex", map[string]any{"subAttributes": []map[string]any{
				scimAttr("formatted", "string", nil), scimAttr("givenName", "string", nil), scimAttr("familyName", "string", nil)}}),
			scimAttr("emails", "complex", map[string]any{"multiValued": true, "subAttributes": []map[string]any{
				scimAttr("value", "string", nil), scimAttr("type", "string", nil), scimAttr("primary", "boolean", nil)}}),
			scimAttr("active", "boolean", nil),
			scimAttr("groups", "complex", map[string]any{"multiValued": true, "mutability": "readOnly"}),
		}, "meta": map[string]string{"resourceType": "Schema"}},
		{"schemas": []string{scimSchemaSchema}, "id": scimSchemaGroup, "name": "Group", "attributes": []map[string]any{
			scimAttr("displayName", "string", map[string]any{"required": true, "uniqueness": "server"}),
			scimAttr("externalId", "string", map[string]any{"caseExact": true}),
			scimAttr("members", "complex", map[string]any{"multiValued": true, "subAttributes": []map[string]any{
				scimAttr("value", "string", map[string]any{"mutability": "immutable"})}}),
		}, "meta": map[string]string{"resourceType": "Schema"}},
		{"schemas": []string{scimSchemaSchema}, "id": scimSchemaExt, "name": "loomcycle user", "attributes": []map[string]any{
			scimAttr("accessMode", "string", map[string]any{"canonicalValues": []string{"isolated", "tenant"}}),
		}, "meta": map[string]string{"resourceType": "Schema"}},
	}
}

func (s *Server) handleSCIMResourceTypes(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.scimCaller(w, r); !ok {
		return
	}
	all := scimResourceTypes()
	if id := r.PathValue("id"); id != "" {
		for _, t := range all {
			if t["id"] == id {
				scimJSON(w, http.StatusOK, t)
				return
			}
		}
		scimError(w, http.StatusNotFound, "", "ResourceType not found")
		return
	}
	scimList(w, len(all), 0, all)
}

func (s *Server) handleSCIMSchemas(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.scimCaller(w, r); !ok {
		return
	}
	all := scimSchemas()
	if id := r.PathValue("id"); id != "" {
		for _, sc := range all {
			if sc["id"] == id {
				scimJSON(w, http.StatusOK, sc)
				return
			}
		}
		scimError(w, http.StatusNotFound, "", "Schema not found")
		return
	}
	scimList(w, len(all), 0, all)
}
