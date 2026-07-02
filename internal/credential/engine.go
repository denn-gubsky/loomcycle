package credential

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Engine is the domain layer over the credential store + crypto — the single
// entry point the credentialdef tool and the runtime-side $cred: resolver share.
// Its metadata-returning ops (Get/List/PutInline) strip the sealed definition;
// only Resolve produces a plaintext, and Resolve is called ONLY by runtime
// binding code (MCP $cred: substitution, provider/tool injection) — never a
// model-facing tool path.
type Engine struct {
	store  store.Store
	sealer *Sealer
	inline inlineBackend
}

// NewEngine builds an Engine over a store and a Sealer (the Sealer may be
// disabled — no KEK — in which case PutInline fails and inline resolves error).
func NewEngine(st store.Store, sealer *Sealer) *Engine {
	return &Engine{store: st, sealer: sealer, inline: inlineBackend{sealer: sealer}}
}

// InlineEnabled reports whether inline (encrypted) credentials can be stored.
func (e *Engine) InlineEnabled() bool { return e.sealer.Enabled() }

// PutInline seals plaintext and upserts the row (create / update / rotate —
// rotation re-seals in place with a fresh nonce). Returns row METADATA only
// (Definition stripped). Fails with ErrNoKey when no KEK is configured.
func (e *Engine) PutInline(ctx context.Context, id Identity, plaintext string, expiresAt *time.Time) (store.CredentialDefRow, error) {
	if !e.sealer.Enabled() {
		return store.CredentialDefRow{}, ErrNoKey
	}
	def, err := e.inline.sealInline(plaintext, id)
	if err != nil {
		return store.CredentialDefRow{}, err
	}
	row, err := e.store.CredentialDefPut(ctx, store.CredentialDefRow{
		TenantID: id.TenantID, Scope: id.Scope, ScopeID: id.ScopeID, Name: id.Name,
		Backend: BackendInline, Definition: def, ExpiresAt: expiresAt,
	})
	if err != nil {
		return store.CredentialDefRow{}, err
	}
	return stripSecret(row), nil
}

// Get returns row metadata (Definition stripped). *ErrNotFound on a miss.
func (e *Engine) Get(ctx context.Context, id Identity) (store.CredentialDefRow, error) {
	row, err := e.store.CredentialDefGet(ctx, id.TenantID, id.Scope, id.ScopeID, id.Name)
	if err != nil {
		return store.CredentialDefRow{}, err
	}
	return stripSecret(row), nil
}

// List returns metadata for a single (tenant, scope, scope_id) bucket.
func (e *Engine) List(ctx context.Context, tenantID, scope, scopeID string) ([]store.CredentialDefRow, error) {
	rows, err := e.store.CredentialDefList(ctx, tenantID, scope, scopeID)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		rows[i] = stripSecret(rows[i])
	}
	return rows, nil
}

// Delete removes a credential row. Returns (found, error).
func (e *Engine) Delete(ctx context.Context, id Identity) (bool, error) {
	return e.store.CredentialDefDelete(ctx, id.TenantID, id.Scope, id.ScopeID, id.Name)
}

// Resolved is a successful credential resolution: the plaintext plus the scope
// it was found in (for audit + the redactor).
type Resolved struct {
	Value   string
	Scope   string
	ScopeID string
	Backend string
}

// Resolve finds a credential by NAME using scope precedence agent > user >
// tenant — the most specific wins, so a user's own token shadows a tenant
// default of the same name (per-user Telegram/Slack channels). It then decrypts
// via the row's backend. RUNTIME-SIDE ONLY: the returned plaintext must go
// straight to a header/env/provider, never into a transcript. agentName/userID
// may be "" (that scope is skipped). Returns (_, false, nil) when the name is
// absent from every visible scope.
func (e *Engine) Resolve(ctx context.Context, tenantID, agentName, userID, name string) (Resolved, bool, error) {
	buckets := []struct{ scope, id string }{
		{"agent", agentName},
		{"user", userID},
		{"tenant", ""},
	}
	for _, b := range buckets {
		if b.scope != "tenant" && b.id == "" {
			continue // no identity for this scope on the run
		}
		row, err := e.store.CredentialDefGet(ctx, tenantID, b.scope, b.id, name)
		if err != nil {
			var nf *store.ErrNotFound
			if errors.As(err, &nf) {
				continue // not in this scope — try the next
			}
			return Resolved{}, false, err
		}
		backend, err := e.backendFor(row.Backend)
		if err != nil {
			return Resolved{}, false, err
		}
		val, err := backend.Resolve(ctx, row.Definition, Identity{
			TenantID: tenantID, Scope: b.scope, ScopeID: b.id, Name: name,
		})
		if err != nil {
			return Resolved{}, false, err
		}
		return Resolved{Value: val, Scope: b.scope, ScopeID: b.id, Backend: row.Backend}, true, nil
	}
	return Resolved{}, false, nil
}

func (e *Engine) backendFor(name string) (Backend, error) {
	switch name {
	case BackendInline:
		return e.inline, nil
	case BackendVault, BackendAWSSecretsManager, BackendGCPSecretManager, BackendOnePassword:
		return externalStub{name: name}, nil
	default:
		return nil, fmt.Errorf("credential: unknown backend %q", name)
	}
}

// stripSecret returns a copy of the row with the sealed Definition removed — the
// only shape that ever leaves the engine toward a caller/log (metadata only).
func stripSecret(row store.CredentialDefRow) store.CredentialDefRow {
	row.Definition = nil
	return row
}
