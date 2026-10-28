package http

// oauth_clientdef.go — RFC FL P0b: app clients registered at runtime
// (ClientDef), beside the config-declared auth.clients (D2).
//
// A client's redirect URIs decide where a person's sign-in code goes, so a
// runtime client is an account-phishing path if an operator credential leaks.
// That is the risk the owner accepted; these are its fences:
//
//   - who: an admin for any tenant (or for every tenant, tenant ""), a tenant
//     operator for its own tenant only (D2a), each write also asking the RFC
//     EY verb of def:client/<name>; a config client is never altered here;
//   - what: the same validation as auth.clients (config.ResolveAppClient), so
//     a runtime client can never be looser than one an operator could write;
//   - whom: a tenant's client signs in only that tenant's people (D10,
//     config.AppClient.ServesTenant at authorize, exchange, refresh and on
//     every use of the token);
//   - after: every create, change and retire is audited with its author and
//     redirect URIs; a change voids the client's unused codes, and a retire
//     also revokes its sign-ins and retires their access tokens — in the
//     shared store, so on every replica, with a cache flush across the bus.
//
// One implementation serves every transport: POST /v1/_clientdef, the MCP
// clientdef tool and the gRPC ClientDef RPC all call Server.ClientDef with the
// caller's authenticated principal on ctx.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// maxClientDefDescription bounds a ClientDef's free-text description.
const maxClientDefDescription = 1024

// clientDefInput is the ClientDef request on every transport.
type clientDefInput struct {
	Op            string                   `json:"op"`
	Name          string                   `json:"name,omitempty"`
	DefID         string                   `json:"def_id,omitempty"`
	Client        *config.AuthClientConfig `json:"client,omitempty"`
	Description   string                   `json:"description,omitempty"`
	Tenant        *string                  `json:"tenant,omitempty"`
	ContentSHA256 string                   `json:"content_sha256,omitempty"`
}

// clientDefCaller is who is asking: an admin (every tenant), or a tenant
// operator confined to its own tenant.
type clientDefCaller struct {
	admin   bool
	tenant  string
	subject string
}

func clientDefCallerOf(ctx context.Context) (clientDefCaller, error) {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok {
		// No principal: open mode or the stdio MCP launcher — the operator.
		return clientDefCaller{admin: true, subject: "operator"}, nil
	}
	if auth.HasScope(p.Scopes, auth.ScopeAdmin) {
		return clientDefCaller{admin: true, tenant: p.TenantID, subject: p.Subject}, nil
	}
	if auth.HasScope(p.Scopes, auth.ScopeTenant) {
		return clientDefCaller{tenant: p.TenantID, subject: p.Subject}, nil
	}
	return clientDefCaller{}, errors.New("registering apps needs substrate:tenant (a tenant operator) or substrate:admin")
}

// sees reports whether the caller may read a client of tenant: an admin any,
// a tenant operator its own tenant's and the every-tenant ones (which serve
// its users).
func (c clientDefCaller) sees(tenant string) bool {
	return c.admin || tenant == c.tenant || tenant == ""
}

// alters reports whether the caller may change or retire a client of tenant.
func (c clientDefCaller) alters(tenant string) bool {
	return c.admin || (tenant == c.tenant && tenant != "")
}

func clientDefErr(msg string) connector.ToolResult {
	return connector.ToolResult{Text: msg, IsError: true}
}

func clientDefOK(v any, count int) (connector.ToolResult, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return connector.ToolResult{}, err
	}
	res := connector.ToolResult{Text: string(b)}
	if count >= 0 {
		res.Count = &count
	}
	return res, nil
}

// ClientDef is the ClientDef op dispatch, the one implementation every
// transport reaches.
func (s *Server) ClientDef(ctx context.Context, raw json.RawMessage) (connector.ToolResult, error) {
	if s.store == nil {
		return clientDefErr("ClientDef: this server has no store"), nil
	}
	var in clientDefInput
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return clientDefErr("invalid input JSON: " + err.Error()), nil
	}
	caller, err := clientDefCallerOf(ctx)
	if err != nil {
		return clientDefErr(err.Error()), nil
	}
	// RFC EZ §1.2: the op's own verb, asked of the client it names.
	if res, refused := tools.AuthorizeDefOp(tools.WithACLRecorder(ctx, s.recordToolACLEvent), "client", in.Op, in.Name); refused {
		return connector.ToolResult{Text: res.Text, IsError: true, ErrorInfo: res.Error}, nil
	}
	switch in.Op {
	case "create", "update":
		return s.clientDefWrite(ctx, caller, in)
	case "get":
		return s.clientDefGet(ctx, caller, in)
	case "list":
		return s.clientDefList(ctx, caller, in)
	case "retire":
		return s.clientDefRetire(ctx, caller, in)
	case "verify":
		return s.clientDefVerify(ctx, caller, in)
	case "":
		return clientDefErr("missing required field: op"), nil
	}
	return clientDefErr(fmt.Sprintf("unknown op %q (must be one of: create, update, get, list, retire, verify)", in.Op)), nil
}

// clientDefNotFound is the one answer for a client that does not exist and
// one the caller may not see, so the reply is no oracle for another tenant's
// clients.
func clientDefNotFound(op, name string) connector.ToolResult {
	return clientDefErr(fmt.Sprintf("%s: client %q not found", op, name))
}

func (s *Server) clientDefWrite(ctx context.Context, caller clientDefCaller, in clientDefInput) (connector.ToolResult, error) {
	op := in.Op
	if in.Name == "" || in.Client == nil {
		return clientDefErr(op + ": name and client are required"), nil
	}
	if _, ok := s.cfg().ResolvedAuth.Client(in.Name); ok {
		return clientDefErr(fmt.Sprintf("%s: %q is declared in the server's config, which is not changed through the API", op, in.Name)), nil
	}
	if len(in.Description) > maxClientDefDescription {
		return clientDefErr(fmt.Sprintf("%s: description is longer than %d bytes", op, maxClientDefDescription)), nil
	}
	cl, err := config.ResolveAppClient("client", in.Name, *in.Client)
	if err != nil {
		return clientDefErr(op + ": " + err.Error()), nil
	}
	cur, curErr := s.store.ClientDefGetActive(ctx, in.Name)
	if curErr != nil && !isNotFound(curErr) {
		return connector.ToolResult{}, curErr
	}
	exists := curErr == nil
	var tenant string
	switch {
	case op == "update":
		if !exists || !caller.sees(cur.TenantID) {
			return clientDefNotFound(op, in.Name), nil
		}
		if !caller.alters(cur.TenantID) {
			return clientDefErr(fmt.Sprintf("update: client %q serves every tenant; only an admin may change it", in.Name)), nil
		}
		if in.Tenant != nil && *in.Tenant != cur.TenantID {
			return clientDefErr("update: a client's tenant cannot change; retire it and create it again"), nil
		}
		tenant = cur.TenantID
	default:
		if exists {
			return clientDefErr(fmt.Sprintf("create: client %q already exists; use update to change it", in.Name)), nil
		}
		tenant = caller.tenant
		if caller.admin {
			// An admin names the tenant, "" for every tenant (the default).
			tenant = ""
			if in.Tenant != nil {
				tenant = *in.Tenant
			}
		} else if in.Tenant != nil && *in.Tenant != caller.tenant {
			return clientDefErr("create: a tenant operator registers apps for its own tenant only"), nil
		}
	}
	body, err := json.Marshal(in.Client)
	if err != nil {
		return connector.ToolResult{}, err
	}
	sum := sha256.Sum256(append([]byte(in.Name+"\x00"), body...))
	row, err := s.store.ClientDefCreate(ctx, store.ClientDefRow{
		DefID: "cdef_" + newTokenDefID()[len("def_"):], Name: in.Name, TenantID: tenant, Definition: string(body),
		ContentSHA256: hex.EncodeToString(sum[:]), Description: in.Description, CreatedBy: caller.subject,
		CreatedByTenant: caller.tenant,
	})
	if err != nil {
		var conflict *store.ErrConflict
		if errors.As(err, &conflict) {
			// The name is another tenant's (perhaps retired). Client ids
			// travel in every authorize URL, so saying so leaks nothing a
			// sign-in link would not.
			return clientDefErr(fmt.Sprintf("%s: the name %q is not available; choose another", op, in.Name)), nil
		}
		return connector.ToolResult{}, err
	}
	action := "client_def_created"
	if op == "update" {
		action = "client_def_changed"
		// Codes issued under the old definition go at once: a code bound to a
		// redirect URI the change removed must not still be exchangeable.
		if _, err := s.store.OAuthCodeInvalidateClient(ctx, in.Name, time.Now().UTC()); err != nil {
			slog.Error("clientdef: voiding a changed client's codes failed", "client", in.Name, "err", err)
		}
	}
	s.recordAuthEvent(audit.Event{Action: action, ActorTenant: caller.tenant, ActorSubject: caller.subject,
		TargetTenant: tenant, TargetName: in.Name, TargetDefID: row.DefID, AuthClient: in.Name, AuthRedirectURIs: cl.RedirectURIs})
	return clientDefOK(clientDefView(row, cl), -1)
}

func (s *Server) clientDefGet(ctx context.Context, caller clientDefCaller, in clientDefInput) (connector.ToolResult, error) {
	if in.DefID != "" {
		row, err := s.store.ClientDefGet(ctx, in.DefID)
		if isNotFound(err) || err == nil && (!caller.sees(row.TenantID) || in.Name != "" && in.Name != row.Name) {
			return clientDefNotFound("get", in.DefID), nil
		}
		if err != nil {
			return connector.ToolResult{}, err
		}
		return clientDefOK(clientDefView(row, decodeClientDef(row)), -1)
	}
	if in.Name == "" {
		return clientDefErr("get: name or def_id is required"), nil
	}
	if cl, ok := s.cfg().ResolvedAuth.Client(in.Name); ok {
		return clientDefOK(configClientView(cl), -1)
	}
	row, err := s.store.ClientDefGetActive(ctx, in.Name)
	if isNotFound(err) || err == nil && !caller.sees(row.TenantID) {
		return clientDefNotFound("get", in.Name), nil
	}
	if err != nil {
		return connector.ToolResult{}, err
	}
	return clientDefOK(clientDefView(row, decodeClientDef(row)), -1)
}

// clientDefList lists the clients the caller may see: config clients (read
// only) and live ClientDefs. An admin may focus one tenant with tenant.
func (s *Server) clientDefList(ctx context.Context, caller clientDefCaller, in clientDefInput) (connector.ToolResult, error) {
	views, err := s.visibleClients(ctx, caller, in.Tenant)
	if err != nil {
		return connector.ToolResult{}, err
	}
	return clientDefOK(map[string]any{"clients": views}, len(views))
}

func (s *Server) visibleClients(ctx context.Context, caller clientDefCaller, focus *string) ([]map[string]any, error) {
	out := []map[string]any{}
	for _, cl := range s.cfg().ResolvedAuth.Clients {
		out = append(out, configClientView(cl))
	}
	rows, err := s.store.ClientDefList(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if !caller.sees(r.TenantID) || caller.admin && focus != nil && r.TenantID != *focus {
			continue
		}
		if _, shadowed := s.cfg().ResolvedAuth.Client(r.Name); shadowed {
			continue // a config client of the same name is the one in force
		}
		out = append(out, clientDefView(r, decodeClientDef(r)))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i]["name"].(string) < out[j]["name"].(string) })
	return out, nil
}

// clientDefRetire takes a client out of service: its versions retire, its
// unused codes are voided, its sign-ins revoked and their access tokens
// retired, and every replica's token cache flushed.
func (s *Server) clientDefRetire(ctx context.Context, caller clientDefCaller, in clientDefInput) (connector.ToolResult, error) {
	if in.Name == "" {
		return clientDefErr("retire: name is required"), nil
	}
	if _, ok := s.cfg().ResolvedAuth.Client(in.Name); ok {
		return clientDefErr(fmt.Sprintf("retire: %q is declared in the server's config, which is not changed through the API", in.Name)), nil
	}
	row, err := s.store.ClientDefGetActive(ctx, in.Name)
	if isNotFound(err) || err == nil && !caller.sees(row.TenantID) {
		return clientDefNotFound("retire", in.Name), nil
	}
	if err != nil {
		return connector.ToolResult{}, err
	}
	if !caller.alters(row.TenantID) {
		return clientDefErr(fmt.Sprintf("retire: client %q serves every tenant; only an admin may retire it", in.Name)), nil
	}
	now := time.Now().UTC()
	if _, err := s.store.ClientDefRetire(ctx, in.Name, now); err != nil {
		return connector.ToolResult{}, err
	}
	s.endClientSignIns(ctx, in.Name, now)
	s.recordAuthEvent(audit.Event{Action: "client_def_retired", ActorTenant: caller.tenant, ActorSubject: caller.subject,
		TargetTenant: row.TenantID, TargetName: in.Name, TargetDefID: row.DefID, AuthClient: in.Name,
		AuthRedirectURIs: decodeClientDef(row).RedirectURIs})
	return clientDefOK(map[string]any{"name": in.Name, "retired": true}, -1)
}

// endClientSignIns ends everything a retired client issued. The rows are in
// the shared store, so this holds on every replica; the cache flush goes
// across the bus.
func (s *Server) endClientSignIns(ctx context.Context, client string, now time.Time) {
	if _, err := s.store.OAuthCodeInvalidateClient(ctx, client, now); err != nil {
		slog.Error("clientdef: voiding a retired client's codes failed", "client", client, "err", err)
	}
	if _, err := s.store.OAuthFamilyRevokeClient(ctx, client, oauthRevokeClientRetired, now); err != nil {
		slog.Error("clientdef: revoking a retired client's sign-ins failed", "client", client, "err", err)
	}
	if _, err := s.store.OAuthTokenRetire(ctx, client, "", now); err != nil {
		slog.Error("clientdef: retiring a retired client's tokens failed", "client", client, "err", err)
	}
	s.invalidateTokenCache(ctx)
}

func (s *Server) clientDefVerify(ctx context.Context, caller clientDefCaller, in clientDefInput) (connector.ToolResult, error) {
	if in.Name == "" {
		return clientDefErr("verify: name is required"), nil
	}
	row, err := s.store.ClientDefGetActive(ctx, in.Name)
	if isNotFound(err) || err == nil && !caller.sees(row.TenantID) {
		return clientDefOK(map[string]any{"name": in.Name, "deployed": false, "matches": false}, -1)
	}
	if err != nil {
		return connector.ToolResult{}, err
	}
	return clientDefOK(map[string]any{"name": in.Name, "deployed": true, "current_def_id": row.DefID,
		"current_sha256": row.ContentSHA256, "version": row.Version,
		"matches": in.ContentSHA256 != "" && in.ContentSHA256 == row.ContentSHA256}, -1)
}

// decodeClientDef reads a stored definition back into a validated client. A
// definition this build cannot read or no longer accepts is a client that does
// not sign anyone in (zero RedirectURIs).
func decodeClientDef(row store.ClientDefRow) config.AppClient {
	var cc config.AuthClientConfig
	if err := json.Unmarshal([]byte(row.Definition), &cc); err != nil {
		slog.Warn("clientdef: a stored client does not decode", "client", row.Name, "def_id", row.DefID, "err", err)
		return config.AppClient{}
	}
	cl, err := config.ResolveAppClient("client_def", row.Name, cc)
	if err != nil {
		slog.Warn("clientdef: a stored client no longer validates", "client", row.Name, "def_id", row.DefID, "err", err)
		return config.AppClient{}
	}
	cl.TenantID, cl.DefID = row.TenantID, row.DefID
	return cl
}

// clientDefAppClient resolves a runtime-registered client (a ClientDef). A
// stored client that no longer validates signs nobody in.
func (s *Server) clientDefAppClient(ctx context.Context, id string) (config.AppClient, bool, error) {
	if s.store == nil || id == "" {
		return config.AppClient{}, false, nil
	}
	row, err := s.store.ClientDefGetActive(ctx, id)
	if isNotFound(err) {
		return config.AppClient{}, false, nil
	}
	if err != nil {
		return config.AppClient{}, false, err
	}
	cl := decodeClientDef(row)
	if cl.ID == "" {
		return config.AppClient{}, false, nil
	}
	return cl, true, nil
}

// clientDefsAllowOrigin reports whether a live ClientDef lists origin.
func (s *Server) clientDefsAllowOrigin(ctx context.Context, origin string) bool {
	if s.store == nil {
		return false
	}
	rows, err := s.store.ClientDefList(ctx)
	if err != nil {
		slog.Warn("clientdef: listing clients for a preflight failed", "err", err)
		return false
	}
	for _, r := range rows {
		if decodeClientDef(r).OriginAllowed(origin) {
			return true
		}
	}
	return false
}

func clientDefView(row store.ClientDefRow, cl config.AppClient) map[string]any {
	var def any
	_ = json.Unmarshal([]byte(row.Definition), &def)
	return map[string]any{
		"name": row.Name, "source": "def", "def_id": row.DefID, "version": row.Version, "tenant_id": row.TenantID,
		"all_tenants": row.TenantID == "", "label": cl.Label, "redirect_uris": nonNil(cl.RedirectURIs),
		"client": def, "description": row.Description, "content_sha256": row.ContentSHA256,
		"created_at": row.CreatedAt.UTC().Format(time.RFC3339Nano), "created_by": row.CreatedBy,
		"created_by_tenant": row.CreatedByTenant, "read_only": false,
	}
}

func configClientView(cl config.AppClient) map[string]any {
	return map[string]any{
		"name": cl.ID, "source": "config", "label": cl.Label, "all_tenants": true, "tenant_id": "",
		"redirect_uris": nonNil(cl.RedirectURIs), "allowed_origins": nonNil(cl.AllowedOrigins),
		"access_token_ttl": cl.AccessTTL.String(), "refresh_idle": cl.RefreshIdle.String(),
		"refresh_absolute": cl.RefreshAbs.String(), "read_only": true,
	}
}

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// handleSubstrateClientDef serves POST /v1/_clientdef.
func (s *Server) handleSubstrateClientDef(w http.ResponseWriter, r *http.Request) {
	s.dispatchSubstrateCtx(w, r, "ClientDef", s.ClientDef, func(ctx context.Context) context.Context { return ctx })
}

// handleListClientDefNames serves GET /v1/_clientdef/names: the clients the
// caller may see, by name (admin: ?tenant= focuses one tenant).
func (s *Server) handleListClientDefNames(w http.ResponseWriter, r *http.Request) {
	caller, err := clientDefCallerOf(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusForbidden, "forbidden", err.Error())
		return
	}
	var focus *string
	if t := r.URL.Query().Get("tenant"); t != "" || r.URL.Query().Has("tenant") {
		focus = &t
	}
	views, err := s.visibleClients(r.Context(), caller, focus)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	names := make([]map[string]any, 0, len(views))
	for _, v := range views {
		names = append(names, map[string]any{"name": v["name"], "source": v["source"], "tenant_id": v["tenant_id"],
			"label": v["label"], "read_only": v["read_only"]})
	}
	writeJSON(w, http.StatusOK, map[string]any{"names": names})
}
