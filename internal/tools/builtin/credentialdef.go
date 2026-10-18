package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
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
	// Audit records every crypto-key change (RFC EP). Nil: not recorded.
	Audit audit.Sink
	// KMS reaches external KMSs, for a crypto key held in one (shared with
	// the Crypto tool). Nil: such keys cannot be created.
	KMS *KMSAccess
}

// CredentialToolName is the in-band tool name (tools:[CredentialDef]).
const CredentialToolName = "CredentialDef"

// CredentialValueField is the create-op input field carrying the plaintext
// secret. The transcript redactor masks it so a create tool-call never persists
// the value (the tool-call event is stored before the tool runs, so masking —
// not post-hoc registration — is the timing-safe fix).
const CredentialValueField = "value"

func (c *CredentialDef) Name() string { return CredentialToolName }

func (c *CredentialDef) Description() string {
	return "Store and manage encrypted API credentials (see Context op=help topic=credentials). Named secrets scoped tenant | user | agent, " +
		"encrypted at rest, that other config references as $cred:<name> and the runtime binds server-side — the " +
		"model never sees the value. user scope keys on YOUR subject (per-user tokens, e.g. a personal Telegram/Slack " +
		"bot token); tenant scope is shared across the tenant. Ops: create (store/rotate a value), get + list (metadata " +
		"only, never the secret), delete. allowed_urls on create restricts where the value may be sent: a restricted " +
		"credential is given only to a caller that names a matching destination, and an empty list means it is sent nowhere. " +
		"Requires LOOMCYCLE_SECRET_KEY for the inline backend. It also holds crypto keys for the Crypto tool (usage crypto): " +
		"generate makes one nobody ever sees, rotate adds a version, set_min_version retires old ones. A crypto key can also " +
		"reference a key held in Vault, AWS KMS or Google Cloud KMS (create with backend and external). Inside a run, crypto " +
		"keys are made and changed in agent scope only."
}

const credentialDefInputSchema = `{
  "type": "object",
  "properties": {
    "op":         {"type": "string", "enum": ["create","get","list","delete","generate","rotate","set_min_version","delete_version","export_key","import_key"], "description": "The operation. generate, rotate, set_min_version, delete_version, export_key and import_key are for crypto keys (usage crypto), used by the Crypto tool."},
    "usage":      {"type": "string", "enum": ["secret","crypto"], "description": "create only: secret (default) stores a value other config sends as $cred:<name>; crypto stores a key for the Crypto tool, never sent anywhere. A crypto key needs key_type."},
    "key_type":   {"type": "string", "enum": ["aes-256-gcm","xchacha20-poly1305","hmac-sha256","hmac-sha512","ed25519","ecdsa-p256","totp","x25519","rsa-oaep-2048","rsa-oaep-3072","rsa-oaep-4096","rsa-pss-2048","rsa-pss-3072","rsa-pss-4096"], "description": "create (usage crypto) and generate: what kind of key. Fixed for the key's life. An rsa-oaep key encrypts and an rsa-pss key signs, never both."},
    "key_encoding": {"type": "string", "enum": ["raw","base64","hex","base32","pem"], "description": "create (usage crypto) and rotate with a value: how value is written (default by key_type: base64 for AEAD and HMAC, base32 for totp, pem for the asymmetric types)."},
    "min_version": {"type": "integer", "description": "set_min_version: the lowest version still used. Versions below it are retired: nothing decrypts or verifies with them, and only they can be deleted. It can only go up."},
    "version":    {"type": "integer", "description": "delete_version: the retired version to delete."},
    "passphrase": {"type": "string", "description": "export_key and import_key: at least 16 characters; the exported key is encrypted with it. Operators only, outside a run. Masked from the persisted transcript."},
    "blob":       {"type": "string", "description": "import_key: what export_key returned on the deployment the key comes from."},
    "name":       {"type": "string", "description": "Credential name (charset ^[A-Za-z0-9_-]{1,128}$). Referenced elsewhere as $cred:<name>. Required for create/get/delete."},
    "scope":      {"type": "string", "enum": ["tenant","user","agent"], "description": "Which bucket (default tenant). user = the calling principal's own subject (per-user tokens); agent = the calling agent. scope_id is derived from your identity, never supplied."},
    "value":      {"type": "string", "description": "create only: the plaintext secret to encrypt (inline backend). Masked from the persisted transcript; never returned."},
    "backend":    {"type": "string", "enum": ["inline","vault","aws_kms","gcp_kms"], "description": "create only: where it is held. inline (default) stores it here, encrypted. vault, aws_kms and gcp_kms are for a crypto key held in that external KMS (usage crypto, with external): only a reference is stored, every Crypto op is a call to the KMS, and the key is rotated and retired there. Such a key is added outside a run."},
    "external":   {"type": "object", "description": "create with backend vault, aws_kms or gcp_kms: the key in the KMS. vault: address (https URL; its host must be on the operator's host allowlist), mount (default transit), key (the Transit key name), namespace (optional), token. aws_kms: region, key_id (key id, ARN or alias/<name>), access_key_id, secret_access_key, session_token (optional). gcp_kms: key (projects/<p>/locations/<l>/keyRings/<r>/cryptoKeys/<k>, optionally /cryptoKeyVersions/<n>), service_account (a service-account key JSON). Each credential field (token, access_key_id, secret_access_key, session_token, service_account) is $cred:<name>, an ordinary credential in the same scope as the key.", "properties": {"address": {"type": "string"}, "mount": {"type": "string"}, "key": {"type": "string"}, "namespace": {"type": "string"}, "token": {"type": "string"}, "region": {"type": "string"}, "key_id": {"type": "string"}, "access_key_id": {"type": "string"}, "secret_access_key": {"type": "string"}, "session_token": {"type": "string"}, "service_account": {"type": "string"}}, "additionalProperties": false},
    "expires_at": {"type": "string", "description": "create only: optional RFC3339 time, in the future, when the value stops being sent. From then on it is refused wherever it is referenced, and nothing stands in for it (not the operator's provider key, not a host env var); get and list still show it, with expired: true. Rotate by creating it again with the same name (a create without expires_at clears it)."},
    "allowed_urls": {"type": "array", "items": {"type": "string"}, "description": "create only: the https URLs this value may be sent to, e.g. [\"https://github.com/acme/app\"]. An entry covers that URL and everything under it, by whole path segments. Omit for no restriction; pass [] for a value that is never sent anywhere. Omitting it when the credential already exists keeps the list it has, so rotating a value does not lift its restriction; to lift one, delete the credential and create it again. A restricted credential is used only where the destination is known and covered: an MCP server's header, a hook's webhook header, a remote memory or document peer's key, a git source. It is never a provider key or a Bash environment variable, and nothing stands in for it there: the provider call fails, and the variable is left unset."},
    "username":   {"type": "string", "description": "create only: optional user name sent with the value where the destination takes one: HTTP basic auth for a git source (default by host: oauth2 for GitLab, x-access-token otherwise). No ':' and no control characters. Omitting it when the credential already exists keeps the one it has; pass \"\" to remove it."}
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
	// A pointer, so an absent list (no restriction) and an empty one (sent
	// nowhere) stay different.
	AllowedURLs *[]string `json:"allowed_urls"`
	// A pointer, so an absent user name (keep the stored one) and an empty
	// one (remove it) stay different.
	Username *string `json:"username"`

	// Crypto keys (RFC EP).
	Usage       string `json:"usage"`
	KeyType     string `json:"key_type"`
	KeyEncoding string `json:"key_encoding"`
	MinVersion  int    `json:"min_version"`
	Version     int    `json:"version"`
	Passphrase  string `json:"passphrase"`
	Blob        string `json:"blob"`
	// External names a key held in an external KMS (backend vault, aws_kms
	// or gcp_kms); its fields depend on the backend.
	External json.RawMessage `json:"external"`
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
		if in.Usage == credential.UsageCrypto {
			return c.execCreateKey(ctx, tenantID, scope, scopeID, in)
		}
		if in.Usage != "" && in.Usage != "secret" {
			return errResult(fmt.Sprintf("create: unknown usage %q (secret | crypto)", in.Usage)), nil
		}
		return c.execCreate(ctx, tenantID, scope, scopeID, in)
	case "get":
		return c.execGet(ctx, tenantID, scope, scopeID, in)
	case "list":
		return c.execList(ctx, tenantID, scope, scopeID)
	case "delete":
		return c.execDelete(ctx, tenantID, scope, scopeID, in)
	case "generate", "rotate", "set_min_version", "delete_version", "export_key", "import_key":
		return c.execKeyOp(ctx, tenantID, scope, scopeID, in)
	case "":
		return errResult("missing required field: op"), nil
	default:
		return errResult(fmt.Sprintf("unknown op %q (must be one of: create, get, list, delete, generate, rotate, set_min_version, delete_version, export_key, import_key)", in.Op)), nil
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
	allowed, uerr := credential.NormalizeAllowedURLs(in.AllowedURLs)
	if uerr != nil {
		return errResult("create: " + uerr.Error()), nil
	}
	if in.Username != nil {
		if err := credential.ValidUsername(*in.Username); err != nil {
			return errResult("create: " + err.Error()), nil
		}
	}
	id := credential.Identity{TenantID: tenantID, Scope: scope, ScopeID: scopeID, Name: in.Name}
	row, err := c.Engine.PutInline(ctx, id, in.Value, expires, allowed, in.Username)
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
func credentialMeta(row credential.Meta, note string) map[string]any {
	m := map[string]any{
		"name":       row.Name,
		"scope":      row.Scope,
		"backend":    row.Backend,
		"created_at": row.CreatedAt,
		"updated_at": row.UpdatedAt,
	}
	if row.ExpiresAt != nil {
		m["expires_at"] = *row.ExpiresAt
		// An expired credential is kept so it can be seen and rotated, but
		// it no longer resolves anywhere; say so rather than make a reader
		// compare the time.
		m["expired"] = row.Expired
	}
	// Present only for a restricted credential; [] means "sent nowhere".
	if row.AllowedURLs != nil {
		m["allowed_urls"] = *row.AllowedURLs
	}
	if row.Username != "" {
		m["username"] = row.Username
	}
	// A crypto key (RFC EP): its type and versions, never its material.
	if row.Usage == credential.UsageCrypto {
		m["usage"] = row.Usage
		m["key_type"] = row.KeyType
		if row.KeyBackend != "" {
			// Held in an external KMS: its versions are the KMS's, not listed here.
			m["key_backend"] = row.KeyBackend
		}
		m["current_version"] = row.CurrentVersion
		m["min_version"] = row.MinVersion
		versions := make([]map[string]any, 0, len(row.Versions))
		for _, v := range row.Versions {
			versions = append(versions, map[string]any{"version": v.Version, "created_at": v.CreatedAt, "retired": v.Retired})
		}
		m["versions"] = versions
	}
	if note != "" {
		m["status"] = note
	}
	return m
}
