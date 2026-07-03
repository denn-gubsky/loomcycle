package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// CredentialDef is the RFC AR secure per-tenant credential store tool. It stores
// named secrets (API tokens, per-user Telegram/Slack bot tokens, …) encrypted
// at rest, scoped tenant | user | agent, that other Defs reference by name as
// $cred:<name> and the runtime binds server-side (the model never sees the
// value).
//
// Security invariants:
//   - scope_id is derived from the caller's authoritative identity, NEVER the
//     wire. user scope stamps the caller's OWN subject, so a user can only
//     author/read their own credentials; agent scope stamps the calling agent.
//   - get/list return METADATA ONLY — never a secret value.
//   - create's plaintext `value` is field-masked out of the persisted transcript
//     (see CredentialValueField + the redaction path in internal/api/http).
//   - the plaintext is sealed via the credential.Engine (AES-256-GCM under a
//     per-tenant HKDF key); nothing plaintext ever hits the store.
type CredentialDef struct {
	// Engine is the credential domain layer (store + crypto). Required.
	Engine *credential.Engine
}

// CredentialToolName is the in-band tool name (allowed_tools:[CredentialDef]).
const CredentialToolName = "CredentialDef"

// CredentialValueField is the create-op input field carrying the plaintext
// secret. The transcript redactor masks it so a create tool-call never persists
// the value (the tool-call event is stored before the tool runs, so masking —
// not post-hoc registration — is the timing-safe fix).
const CredentialValueField = "value"

func (c *CredentialDef) Name() string { return CredentialToolName }

func (c *CredentialDef) Description() string {
	return "Store and manage encrypted API credentials (RFC AR). Named secrets scoped tenant | user | agent, " +
		"encrypted at rest, that other config references as $cred:<name> and the runtime binds server-side — the " +
		"model never sees the value. user scope keys on YOUR subject (per-user tokens, e.g. a personal Telegram/Slack " +
		"bot token); tenant scope is shared across the tenant. Ops: create (store/rotate a value), get + list (metadata " +
		"only, never the secret), delete. Requires LOOMCYCLE_SECRET_KEY for the inline backend."
}

const credentialDefInputSchema = `{
  "type": "object",
  "properties": {
    "op":         {"type": "string", "enum": ["create","get","list","delete"], "description": "The operation."},
    "name":       {"type": "string", "description": "Credential name (charset ^[A-Za-z0-9_-]{1,128}$). Referenced elsewhere as $cred:<name>. Required for create/get/delete."},
    "scope":      {"type": "string", "enum": ["tenant","user","agent"], "description": "Which bucket (default tenant). user = the calling principal's own subject (per-user tokens); agent = the calling agent. scope_id is derived from your identity, never supplied."},
    "value":      {"type": "string", "description": "create only: the plaintext secret to encrypt (inline backend). Masked from the persisted transcript; never returned."},
    "backend":    {"type": "string", "enum": ["inline"], "description": "create only: storage backend (default inline; external backends are a future addition)."},
    "expires_at": {"type": "string", "description": "create only: optional RFC3339 soft-expiry (advisory rotation reminder)."}
  },
  "required": ["op"]
}`

func (c *CredentialDef) InputSchema() json.RawMessage {
	return json.RawMessage(credentialDefInputSchema)
}

type credentialDefInput struct {
	Op        string `json:"op"`
	Name      string `json:"name"`
	Scope     string `json:"scope"`
	Value     string `json:"value"`
	Backend   string `json:"backend"`
	ExpiresAt string `json:"expires_at"`
}

var credNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func (c *CredentialDef) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	if c.Engine == nil {
		return errResult("CredentialDef tool: not configured (no credential engine)"), nil
	}
	var in credentialDefInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errResult("invalid input JSON: " + err.Error()), nil
	}
	tenantID, scope, scopeID, serr := c.resolveScope(ctx, in.Scope)
	if serr != nil {
		return errResult(serr.Error()), nil
	}
	switch in.Op {
	case "create":
		return c.execCreate(ctx, tenantID, scope, scopeID, in)
	case "get":
		return c.execGet(ctx, tenantID, scope, scopeID, in)
	case "list":
		return c.execList(ctx, tenantID, scope, scopeID)
	case "delete":
		return c.execDelete(ctx, tenantID, scope, scopeID, in)
	case "":
		return errResult("missing required field: op"), nil
	default:
		return errResult(fmt.Sprintf("unknown op %q (must be one of: create, get, list, delete)", in.Op)), nil
	}
}

// resolveScope derives the authoritative (tenant, scope, scope_id) from the run
// identity — NEVER the wire. Default scope is tenant. user stamps the caller's
// own subject; agent stamps the calling agent. This is the user/agent-scope
// isolation crux: a user can't address another user's credentials.
func (c *CredentialDef) resolveScope(ctx context.Context, requested string) (tenantID, scope, scopeID string, err error) {
	tenantID = tools.RunIdentity(ctx).TenantID
	if requested == "" {
		requested = "tenant"
	}
	switch requested {
	case "tenant":
		return tenantID, "tenant", "", nil
	case "user":
		uid := tools.RunIdentity(ctx).UserID
		if uid == "" {
			return "", "", "", errors.New("CredentialDef: scope=user requires a user_id on the run")
		}
		return tenantID, "user", uid, nil
	case "agent":
		name := tools.AgentName(ctx)
		if name == "" {
			return "", "", "", errors.New("CredentialDef: scope=agent requires a yaml-declared agent (no agent name on the run)")
		}
		return tenantID, "agent", name, nil
	default:
		return "", "", "", fmt.Errorf("CredentialDef: unknown scope %q (tenant | user | agent)", requested)
	}
}

func (c *CredentialDef) execCreate(ctx context.Context, tenantID, scope, scopeID string, in credentialDefInput) (tools.Result, error) {
	if !credNameRe.MatchString(in.Name) {
		return errResult("create: name must match ^[A-Za-z0-9_-]{1,128}$"), nil
	}
	backend := in.Backend
	if backend == "" {
		backend = credential.BackendInline
	}
	if backend != credential.BackendInline {
		return errResult(fmt.Sprintf("create: backend %q is not supported yet (use inline)", backend)), nil
	}
	if in.Value == "" {
		return errResult("create: value is required (the plaintext secret to encrypt)"), nil
	}
	if !c.Engine.InlineEnabled() {
		return errResult("create: inline credential storage is disabled — the operator must set LOOMCYCLE_SECRET_KEY"), nil
	}
	var expires *time.Time
	if in.ExpiresAt != "" {
		t, perr := time.Parse(time.RFC3339, in.ExpiresAt)
		if perr != nil {
			return errResult("create: expires_at must be RFC3339 (e.g. 2026-12-31T00:00:00Z)"), nil
		}
		expires = &t
	}
	id := credential.Identity{TenantID: tenantID, Scope: scope, ScopeID: scopeID, Name: in.Name}
	row, err := c.Engine.PutInline(ctx, id, in.Value, expires)
	if err != nil {
		return errResult("create: " + err.Error()), nil
	}
	return jsonResult(credentialMeta(row, "stored"))
}

func (c *CredentialDef) execGet(ctx context.Context, tenantID, scope, scopeID string, in credentialDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("get: name is required"), nil
	}
	row, err := c.Engine.Get(ctx, credential.Identity{TenantID: tenantID, Scope: scope, ScopeID: scopeID, Name: in.Name})
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return errResult(fmt.Sprintf("get: no credential %q in %s scope", in.Name, scope)), nil
		}
		return errResult("get: " + err.Error()), nil
	}
	return jsonResult(credentialMeta(row, ""))
}

func (c *CredentialDef) execList(ctx context.Context, tenantID, scope, scopeID string) (tools.Result, error) {
	rows, err := c.Engine.List(ctx, tenantID, scope, scopeID)
	if err != nil {
		return errResult("list: " + err.Error()), nil
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, credentialMeta(r, ""))
	}
	return jsonResult(map[string]any{"scope": scope, "credentials": out})
}

func (c *CredentialDef) execDelete(ctx context.Context, tenantID, scope, scopeID string, in credentialDefInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("delete: name is required"), nil
	}
	found, err := c.Engine.Delete(ctx, credential.Identity{TenantID: tenantID, Scope: scope, ScopeID: scopeID, Name: in.Name})
	if err != nil {
		return errResult("delete: " + err.Error()), nil
	}
	return jsonResult(map[string]any{"name": in.Name, "scope": scope, "deleted": found})
}

// credentialMeta renders a row as METADATA ONLY — never the sealed definition or
// the secret value. `note` is an optional status word (e.g. "stored").
func credentialMeta(row store.CredentialDefRow, note string) map[string]any {
	m := map[string]any{
		"name":       row.Name,
		"scope":      row.Scope,
		"backend":    row.Backend,
		"created_at": row.CreatedAt,
		"updated_at": row.UpdatedAt,
	}
	if row.ExpiresAt != nil {
		m["expires_at"] = *row.ExpiresAt
	}
	if note != "" {
		m["status"] = note
	}
	return m
}
