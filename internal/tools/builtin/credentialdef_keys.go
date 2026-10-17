package builtin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/credential"
	"github.com/denn-gubsky/loomcycle/internal/cryptoops"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// CredentialPassphraseField is export_key / import_key's passphrase. Masked out
// of the persisted tool call like create's value: the event is stored before
// the tool runs.
const CredentialPassphraseField = "passphrase"

// Crypto keys (RFC EP) are credentials with usage "crypto": a key type fixed
// at creation, numbered versions, and a min_version below which versions are
// retired. Their material is never shown; the Crypto tool uses them.
//
// Who may do what:
//   - Inside a run, an agent creates, generates and changes crypto keys only in
//     its own agent scope. A key in tenant or user scope is an operator's or a
//     user's, made outside a run.
//   - export_key and import_key move a key between deployments. Only an
//     operator, outside any run: an exported key is the key itself, under a
//     passphrase.

// callInRun reports whether this call is a model's tool call inside a run.
func callInRun(ctx context.Context) bool { return tools.RunID(ctx) != "" }

// isOperatorAdmin reports whether the caller is an administrator: no
// principal (open mode, the stdio MCP), the legacy operator token, or a token
// with substrate:admin.
func isOperatorAdmin(ctx context.Context) bool {
	p, ok := auth.PrincipalFromContext(ctx)
	return !ok || p.Legacy || auth.HasScope(p.Scopes, auth.ScopeAdmin)
}

// keyScopeAllowed refuses a crypto-key change in tenant or user scope from
// inside a run.
func keyScopeAllowed(ctx context.Context, op, scope string) (tools.Result, bool) {
	if callInRun(ctx) && scope != "agent" {
		return errPermission(fmt.Sprintf("%s: inside a run, crypto keys are created and changed only in agent scope", op),
			"Pass scope: \"agent\". Tenant and user keys are made by an operator or the user, outside a run."), false
	}
	return tools.Result{}, true
}

func (c *CredentialDef) execCreateKey(ctx context.Context, tenantID, scope, scopeID string, in credentialDefInput) (tools.Result, error) {
	if res, ok := keyScopeAllowed(ctx, "create", scope); !ok {
		return res, nil
	}
	if !credNameRe.MatchString(in.Name) {
		return errResult("create: name must match ^[A-Za-z0-9_-]{1,128}$"), nil
	}
	if in.AllowedURLs != nil || in.Username != nil {
		return errValidation("create: allowed_urls and username do not apply to a crypto key", "A crypto key is never sent anywhere; drop them."), nil
	}
	kt, err := cryptoops.ParseKeyType(in.KeyType)
	if err != nil {
		return errValidation("create: "+err.Error(), "A crypto key needs key_type."), nil
	}
	if in.Value == "" {
		return errValidation("create: value is required (the key material to import)", "Use op generate to make a new key nobody sees."), nil
	}
	material, err := cryptoops.ParseMaterial(kt, in.Value, in.KeyEncoding)
	if err != nil {
		return errValidation(fmt.Sprintf("create: value is not a valid %s key", kt), "Name its key_encoding, or generate a key instead."), nil
	}
	expires, res, ok := parseExpiry("create", in.ExpiresAt)
	if !ok {
		return res, nil
	}
	return c.putKey(ctx, "crypto_key_create", credential.Identity{TenantID: tenantID, Scope: scope, ScopeID: scopeID, Name: in.Name}, kt, material, expires)
}

func (c *CredentialDef) execKeyOp(ctx context.Context, tenantID, scope, scopeID string, in credentialDefInput) (tools.Result, error) {
	if in.Op != "export_key" && in.Op != "import_key" {
		if res, ok := keyScopeAllowed(ctx, in.Op, scope); !ok {
			return res, nil
		}
	}
	if !credNameRe.MatchString(in.Name) {
		return errResult(in.Op + ": name must match ^[A-Za-z0-9_-]{1,128}$"), nil
	}
	if !c.Engine.InlineEnabled() {
		return errResult(in.Op + ": crypto keys need the inline credential store — the operator must set LOOMCYCLE_SECRET_KEY"), nil
	}
	id := credential.Identity{TenantID: tenantID, Scope: scope, ScopeID: scopeID, Name: in.Name}
	switch in.Op {
	case "generate":
		kt, err := cryptoops.ParseKeyType(in.KeyType)
		if err != nil {
			return errValidation("generate: "+err.Error(), "Name the key_type."), nil
		}
		expires, res, ok := parseExpiry("generate", in.ExpiresAt)
		if !ok {
			return res, nil
		}
		material, err := cryptoops.Generate(kt)
		if err != nil {
			return errTransient("generate: could not make a key", "Try again."), nil
		}
		return c.putKey(ctx, "crypto_key_generate", id, kt, material, expires)

	case "rotate":
		meta, err := c.Engine.Get(ctx, id)
		if err != nil {
			return keyOpFailure("rotate", in.Name, scope, err), nil
		}
		if meta.Usage != credential.UsageCrypto {
			return errValidation(fmt.Sprintf("rotate: %q is not a crypto key", in.Name), "Rotate an ordinary credential by creating it again."), nil
		}
		kt, err := cryptoops.ParseKeyType(meta.KeyType)
		if err != nil {
			return errBusiness("rotate: the key has a type this build does not know", ""), nil
		}
		var material []byte
		if in.Value != "" {
			material, err = cryptoops.ParseMaterial(kt, in.Value, in.KeyEncoding)
			if err != nil {
				return errValidation(fmt.Sprintf("rotate: value is not a valid %s key", kt), "Name its key_encoding, or omit value to generate the new version."), nil
			}
		} else if material, err = cryptoops.Generate(kt); err != nil {
			return errTransient("rotate: could not make a key", "Try again."), nil
		}
		m, err := c.Engine.RotateCryptoKey(ctx, id, material)
		c.recordKey(ctx, "crypto_key_rotate", id, meta.KeyType, m.CurrentVersion, err)
		if err != nil {
			return keyOpFailure("rotate", in.Name, scope, err), nil
		}
		return jsonResult(credentialMeta(m, "rotated"))

	case "set_min_version":
		m, err := c.Engine.SetCryptoMinVersion(ctx, id, in.MinVersion)
		c.recordKey(ctx, "crypto_key_min_version", id, m.KeyType, in.MinVersion, err)
		if err != nil {
			return keyOpFailure("set_min_version", in.Name, scope, err), nil
		}
		return jsonResult(credentialMeta(m, "min_version set"))

	case "delete_version":
		m, err := c.Engine.DeleteCryptoKeyVersion(ctx, id, in.Version)
		c.recordKey(ctx, "crypto_key_delete_version", id, m.KeyType, in.Version, err)
		if err != nil {
			return keyOpFailure("delete_version", in.Name, scope, err), nil
		}
		return jsonResult(credentialMeta(m, "version deleted"))

	case "export_key", "import_key":
		if callInRun(ctx) || !isOperatorAdmin(ctx) {
			return errPermission(in.Op+": only an operator, outside a run, moves a crypto key between deployments", ""), nil
		}
		if len(in.Passphrase) < 16 {
			return errValidation(in.Op+": passphrase must be at least 16 characters", ""), nil
		}
		if in.Op == "export_key" {
			blob, err := c.Engine.ExportCryptoKey(ctx, id, in.Passphrase)
			c.recordKey(ctx, "crypto_key_export", id, "", 0, err)
			if err != nil {
				return keyOpFailure("export_key", in.Name, scope, err), nil
			}
			return jsonResult(map[string]any{"name": in.Name, "scope": scope, "blob": blob})
		}
		if in.Blob == "" {
			return errValidation("import_key: blob is required", "Pass what export_key returned."), nil
		}
		m, err := c.Engine.ImportCryptoKey(ctx, id, in.Blob, in.Passphrase)
		c.recordKey(ctx, "crypto_key_import", id, m.KeyType, m.CurrentVersion, err)
		if err != nil {
			return keyOpFailure("import_key", in.Name, scope, err), nil
		}
		return jsonResult(credentialMeta(m, "imported"))
	}
	return errResult("unknown op"), nil
}

func (c *CredentialDef) putKey(ctx context.Context, action string, id credential.Identity, kt cryptoops.KeyType, material []byte, expires *time.Time) (tools.Result, error) {
	if !c.Engine.InlineEnabled() {
		return errResult("crypto keys need the inline credential store — the operator must set LOOMCYCLE_SECRET_KEY"), nil
	}
	if res, ok := c.refuseShadow(ctx, action, id); !ok {
		return res, nil
	}
	m, err := c.Engine.PutCryptoKey(ctx, id, string(kt), material, expires)
	c.recordKey(ctx, action, id, string(kt), 1, err)
	if err != nil {
		return keyOpFailure(action, id.Name, id.Scope, err), nil
	}
	return jsonResult(credentialMeta(m, "stored"))
}

// refuseShadow refuses a crypto key in agent or user scope under a name a
// wider scope the same runs read already holds as a crypto key. A key name
// resolves to the widest scope's key, so the new one would never be used;
// say so to the one creating it. (An ordinary credential of the name does not
// count: resolution skips it.)
func (c *CredentialDef) refuseShadow(ctx context.Context, action string, id credential.Identity) (tools.Result, bool) {
	wider := []credential.Identity{}
	switch id.Scope {
	case "agent":
		if uid := tools.RunIdentity(ctx).UserID; uid != "" {
			wider = append(wider, credential.Identity{TenantID: id.TenantID, Scope: "user", ScopeID: uid, Name: id.Name})
		}
		wider = append(wider, credential.Identity{TenantID: id.TenantID, Scope: "tenant", Name: id.Name})
	case "user":
		wider = append(wider, credential.Identity{TenantID: id.TenantID, Scope: "tenant", Name: id.Name})
	}
	for _, w := range wider {
		if m, err := c.Engine.Get(ctx, w); err == nil && m.Usage == credential.UsageCrypto {
			return errBusiness(fmt.Sprintf("%s: %q already names a crypto key in %s scope", action, id.Name, w.Scope),
				"Choose another name: a key name resolves to the widest scope's key, so this one would never be used."), false
		}
	}
	return tools.Result{}, true
}

func parseExpiry(op, s string) (*time.Time, tools.Result, bool) {
	if s == "" {
		return nil, tools.Result{}, true
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, errValidation(op+": expires_at must be RFC3339 (e.g. 2026-12-31T00:00:00Z)", ""), false
	}
	return &t, tools.Result{}, true
}

// keyOpFailure classifies an engine refusal of a key operation.
func keyOpFailure(op, name, scope string, err error) tools.Result {
	var nf *store.ErrNotFound
	switch {
	case errors.As(err, &nf):
		return errNotFound(fmt.Sprintf("%s: no credential %q in %s scope", op, name, scope), "Check the name and scope with op list.")
	case errors.Is(err, credential.ErrKeyExists):
		return errBusiness(fmt.Sprintf("%s: %q already exists in %s scope", op, name, scope), "Use op rotate to add a version, or another name.")
	case errors.Is(err, credential.ErrKeyInUse):
		return errBusiness(fmt.Sprintf("%s: %q still protects data", op, name),
			"Rewrap what it encrypted, raise min_version past the versions you want to remove, then delete those versions.")
	case errors.Is(err, credential.ErrBadVersion):
		return errValidation(op+": "+err.Error(), "")
	case errors.Is(err, credential.ErrNotCryptoKey):
		return errValidation(fmt.Sprintf("%s: %q is not a crypto key", op, name), "")
	case errors.Is(err, credential.ErrNoKey):
		return errBusiness(op+": inline credential storage is disabled", "The operator must set LOOMCYCLE_SECRET_KEY.")
	}
	return errResult(op + ": " + err.Error())
}

// recordKey writes the audit record of one key-management change.
func (c *CredentialDef) recordKey(ctx context.Context, action string, id credential.Identity, keyType string, version int, err error) {
	if c.Audit == nil {
		return
	}
	ident := tools.RunIdentity(ctx)
	ev := audit.Event{
		Action:        action,
		ActorTenant:   ident.TenantID,
		ActorSubject:  ident.UserID,
		TargetTenant:  id.TenantID,
		TargetName:    id.Name,
		CryptoScope:   id.Scope,
		CryptoKeyType: keyType,
		CryptoVersion: version,
		CryptoRunID:   tools.RunID(ctx),
		CryptoAgent:   tools.AgentName(ctx),
		CryptoOutcome: "ok",
	}
	if p, ok := auth.PrincipalFromContext(ctx); ok {
		ev.ActorTenant, ev.ActorSubject, ev.ActorTokenSuffix = p.TenantID, p.Subject, p.TokenSuffix
	}
	if err != nil {
		ev.CryptoOutcome = "refused"
	}
	_ = c.Audit.Record(ev)
}
