package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// OAuth credentials (RFC GB) are credentials with usage "oauth": the config
// of a connection to an OAuth authorization server, which its owner connects
// through the device grant and other config names as $oauth:<name>.
//
// Who may do what:
//   - Creating, connecting, checking and disconnecting one happen outside a
//     run: a connect needs a person to approve it, and a model must not
//     point a credential at a server of its choosing.
//   - Tenant scope: an operator, or a principal with substrate:tenant.
//     User scope: the caller's own (resolveScope stamps their subject; an
//     isolated user is pinned to it by the transport). Agent scope: an
//     operator only.

// oauthInput is the oauth object of a create with usage oauth.
type oauthInput struct {
	Resource string `json:"resource"`
	Issuer   string `json:"issuer"`
	Client   struct {
		Registration      string `json:"registration"`
		TokenEndpointAuth string `json:"token_endpoint_auth"`
	} `json:"client"`
	Scopes []string `json:"scopes"`
}

// oauthScopeAllowed refuses an OAuth credential op the caller may not make
// in scope.
func oauthScopeAllowed(ctx context.Context, op, scope string) (tools.Result, bool) {
	if callInRun(ctx) {
		return errPermission(fmt.Sprintf("%s: OAuth credentials are created, connected and disconnected outside a run", op),
			"Ask an operator or the credential's owner to do it from the Web UI, the API or the MCP server."), false
	}
	switch scope {
	case "user":
		return tools.Result{}, true
	case "tenant":
		if isOperatorAdmin(ctx) {
			return tools.Result{}, true
		}
		if p, ok := auth.PrincipalFromContext(ctx); ok && auth.HasScope(p.Scopes, auth.ScopeTenant) {
			return tools.Result{}, true
		}
		return errPermission(op+": a tenant OAuth credential is managed by a tenant operator",
			"Use scope user for your own connection."), false
	default:
		if isOperatorAdmin(ctx) {
			return tools.Result{}, true
		}
		return errPermission(op+": an agent OAuth credential is managed by an operator only",
			"Use scope user for your own connection."), false
	}
}

func (c *CredentialDef) execCreateOAuth(ctx context.Context, tenantID, scope, scopeID string, in credentialDefInput) (tools.Result, error) {
	if res, ok := oauthScopeAllowed(ctx, "create", scope); !ok {
		return res, nil
	}
	if !credNameRe.MatchString(in.Name) {
		return errResult("create: name must match ^[A-Za-z0-9_-]{1,128}$"), nil
	}
	if in.Value != "" || in.ExpiresAt != "" || in.Username != nil || in.KeyType != "" || len(in.External) > 0 {
		return errValidation("create: value, expires_at, username, key_type and external do not apply to an OAuth credential",
			"An OAuth credential holds no value of yours: connect it with oauth_connect_start."), nil
	}
	if in.Backend != "" && in.Backend != credential.BackendInline {
		return errValidation(fmt.Sprintf("create: backend %q is not one an OAuth credential can use", in.Backend), "Leave backend unset."), nil
	}
	if len(in.OAuth) == 0 {
		return errValidation("create: usage oauth needs an oauth object", `Pass oauth: {"resource": "https://…"}.`), nil
	}
	var oi oauthInput
	if err := json.Unmarshal(in.OAuth, &oi); err != nil {
		return errValidation("create: oauth is not a valid object", "See the oauth property's description."), nil
	}
	cfg := credential.OAuthConfig{Resource: oi.Resource, Issuer: oi.Issuer, Scopes: oi.Scopes,
		Client: credential.OAuthClientConfig{Registration: oi.Client.Registration, TokenEndpointAuth: oi.Client.TokenEndpointAuth}}
	if cfg.Client.Registration == "" {
		cfg.Client.Registration = credential.OAuthRegistrationDynamic
	}
	if cfg.Client.TokenEndpointAuth == "" {
		cfg.Client.TokenEndpointAuth = credential.OAuthAuthNone
	}
	if !c.Engine.InlineEnabled() {
		return errResult("create: inline credential storage is disabled — the operator must set LOOMCYCLE_SECRET_KEY"), nil
	}
	allowed, uerr := credential.NormalizeAllowedURLs(in.AllowedURLs)
	if uerr != nil {
		return errResult("create: " + uerr.Error()), nil
	}
	id := credential.Identity{TenantID: tenantID, Scope: scope, ScopeID: scopeID, Name: in.Name}
	meta, err := c.Engine.PutOAuth(ctx, id, cfg, allowed)
	c.recordOAuth(ctx, "oauth_create", id, cfg.Issuer, err)
	if err != nil {
		return errResult("create: " + err.Error()), nil
	}
	return jsonResult(credentialMeta(meta, "stored"))
}

func (c *CredentialDef) execOAuthOp(ctx context.Context, tenantID, scope, scopeID string, in credentialDefInput) (tools.Result, error) {
	if res, ok := oauthScopeAllowed(ctx, in.Op, scope); !ok {
		return res, nil
	}
	if !credNameRe.MatchString(in.Name) {
		return errResult(in.Op + ": name must match ^[A-Za-z0-9_-]{1,128}$"), nil
	}
	if c.OAuth == nil {
		return errResult(in.Op + ": OAuth credentials are not available on this server"), nil
	}
	id := credential.Identity{TenantID: tenantID, Scope: scope, ScopeID: scopeID, Name: in.Name}
	switch in.Op {
	case "oauth_connect_start":
		start, err := c.OAuth.ConnectStart(ctx, id)
		c.recordOAuth(ctx, "oauth_connect_start", id, "", err)
		if err != nil {
			return oauthOpErr(in.Op, in.Name, scope, err), nil
		}
		out := map[string]any{
			"name": in.Name, "scope": scope, "status": credential.OAuthPendingConnect,
			"user_code": start.UserCode, "verification_uri": start.VerificationURI,
			"expires_in": start.ExpiresIn, "interval": start.Interval,
			"next": "Open verification_uri, enter user_code and approve; then check with oauth_connect_status.",
		}
		if start.VerificationURIComplete != "" {
			out["verification_uri_complete"] = start.VerificationURIComplete
		}
		return jsonResult(out)
	case "oauth_connect_status":
		meta, err := c.OAuth.ConnectStatus(ctx, id)
		if err != nil {
			return oauthOpErr(in.Op, in.Name, scope, err), nil
		}
		return jsonResult(credentialMeta(meta, ""))
	default: // oauth_disconnect
		meta, err := c.OAuth.Disconnect(ctx, id)
		c.recordOAuth(ctx, "oauth_disconnect", id, "", err)
		if err != nil {
			return oauthOpErr(in.Op, in.Name, scope, err), nil
		}
		return jsonResult(credentialMeta(meta, ""))
	}
}

// oauthOpErr renders an OAuth op's failure. The engine's and the manager's
// errors carry no token text (they quote at most an authorization server's
// error code and a scrubbed description).
func oauthOpErr(op, name, scope string, err error) tools.Result {
	var nf *store.ErrNotFound
	switch {
	case errors.As(err, &nf):
		return errNotFound(fmt.Sprintf("%s: no credential %q in %s scope", op, name, scope), "Create it first with usage oauth.")
	case errors.Is(err, credential.ErrNotOAuth):
		return errValidation(op+": "+err.Error(), "These ops apply to a credential created with usage oauth.")
	}
	return errResult(op + ": " + err.Error())
}

// recordOAuth records an OAuth credential op by the caller.
func (c *CredentialDef) recordOAuth(ctx context.Context, action string, id credential.Identity, issuer string, err error) {
	if c.Audit == nil {
		return
	}
	ident := tools.RunIdentity(ctx)
	ev := audit.Event{
		Action: action, ActorTenant: ident.TenantID, ActorSubject: ident.UserID,
		TargetTenant: id.TenantID, TargetName: id.Name,
		OAuthScope: id.Scope, OAuthIssuer: issuer, OAuthOutcome: "ok",
	}
	if id.Scope == "user" {
		ev.TargetSubject = id.ScopeID
	}
	if p, ok := auth.PrincipalFromContext(ctx); ok {
		ev.ActorTenant, ev.ActorSubject, ev.ActorTokenSuffix = p.TenantID, p.Subject, p.TokenSuffix
	}
	if err != nil {
		ev.OAuthOutcome = "refused"
	}
	_ = c.Audit.Record(ev)
}

// oauthMetaFields renders an OAuth credential's config and status — never
// a token, a device code or a client secret (the meta holds none).
func oauthMetaFields(m map[string]any, o *credential.OAuthMeta) {
	oa := map[string]any{
		"resource":     o.Config.Resource,
		"registration": o.Config.Client.Registration,
		"state":        o.Status.State,
	}
	if o.Config.Issuer != "" {
		oa["issuer"] = o.Config.Issuer
	}
	if len(o.Config.Scopes) > 0 {
		oa["scopes"] = o.Config.Scopes
	}
	if o.Config.Client.TokenEndpointAuth != "" {
		oa["token_endpoint_auth"] = o.Config.Client.TokenEndpointAuth
	}
	if o.Status.ConnectedAs != "" {
		oa["connected_as"] = o.Status.ConnectedAs
	}
	if o.Status.ConnectedAt != nil {
		oa["connected_at"] = *o.Status.ConnectedAt
	}
	if o.Status.LastRefreshAt != nil {
		oa["last_refresh_at"] = *o.Status.LastRefreshAt
	}
	if o.Status.AccessExpiresAt != nil {
		oa["access_expires_at"] = *o.Status.AccessExpiresAt
	}
	if o.Status.LastError != "" {
		oa["last_error"] = o.Status.LastError
	}
	m["oauth"] = oa
}
