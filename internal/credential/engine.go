package credential

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/netguard"
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

// CanResolve reports whether ANY credential could ever resolve in this build.
// The inline backend needs the Sealer enabled (a configured KEK); external
// backends are unimplemented stubs (RFC AR Phase 4), so nothing can resolve
// through them yet. When this is false, the $cred: / provider-key resolvers skip
// the per-call store reads entirely — with no way to resolve, the three scope
// lookups on every LLM call would only ever miss. Extend the disjunction here
// once an external backend is wired + configured.
func (e *Engine) CanResolve() bool { return e.sealer.Enabled() }

// Meta is what the engine tells a caller about a stored credential: the row
// with its sealed definition removed, and the URL restriction read out of it.
type Meta struct {
	store.CredentialDefRow
	// AllowedURLs is the credential's URL restriction: nil for none, empty
	// for "sent nowhere". Read from the stored row without opening the
	// secret, so it is what the row SAYS; a row whose list was changed
	// behind the engine's back shows the changed list and no longer resolves.
	AllowedURLs *[]string
}

// PutInline seals plaintext and upserts the row (create / update / rotate —
// rotation re-seals in place with a fresh nonce). allowedURLs is the URL
// restriction to store with it, already normalised (NormalizeAllowedURLs).
// Returns METADATA only. Fails with ErrNoKey when no KEK is configured.
//
// A nil allowedURLs means "the caller said nothing about it", and keeps the
// restriction the credential already has. Rotating a secret is the common
// second write, and a rotation that forgot the list must not quietly turn a
// restricted credential into one that can be sent anywhere. The restriction
// comes off by deleting the credential and creating it again.
func (e *Engine) PutInline(ctx context.Context, id Identity, plaintext string, expiresAt *time.Time, allowedURLs *[]string) (Meta, error) {
	if !e.sealer.Enabled() {
		return Meta{}, ErrNoKey
	}
	if allowedURLs == nil {
		kept, err := e.existingRestriction(ctx, id)
		if err != nil {
			return Meta{}, err
		}
		allowedURLs = kept
	}
	def, err := e.inline.sealInline(plaintext, id, allowedURLs)
	if err != nil {
		return Meta{}, err
	}
	row, err := e.store.CredentialDefPut(ctx, store.CredentialDefRow{
		TenantID: id.TenantID, Scope: id.Scope, ScopeID: id.ScopeID, Name: id.Name,
		Backend: BackendInline, Definition: def, ExpiresAt: expiresAt,
	})
	if err != nil {
		return Meta{}, err
	}
	return metaOf(row), nil
}

// existingRestriction returns the URL restriction of the credential stored
// under id, or nil when there is no such credential or it has none.
//
// The list is about to be sealed into the new value, so it has to be the one
// the old value was sealed with and not merely what the row says: the old
// value is opened to prove it. A restricted row that does not open is refused
// rather than carried over, since carrying it would turn an edited list into
// an authenticated one.
func (e *Engine) existingRestriction(ctx context.Context, id Identity) (*[]string, error) {
	row, err := e.store.CredentialDefGet(ctx, id.TenantID, id.Scope, id.ScopeID, id.Name)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return nil, nil
		}
		return nil, err
	}
	allowed := restrictionOf(row.Definition)
	if allowed == nil || row.Backend != BackendInline {
		return nil, nil
	}
	if _, err := e.inline.Resolve(ctx, row.Definition, id); err != nil {
		return nil, fmt.Errorf("the stored credential is restricted to URLs and could not be opened to carry the restriction over (%w); send allowed_urls again, or delete the credential first", err)
	}
	return allowed, nil
}

// Get returns a credential's metadata. *ErrNotFound on a miss.
func (e *Engine) Get(ctx context.Context, id Identity) (Meta, error) {
	row, err := e.store.CredentialDefGet(ctx, id.TenantID, id.Scope, id.ScopeID, id.Name)
	if err != nil {
		return Meta{}, err
	}
	return metaOf(row), nil
}

// List returns metadata for a single (tenant, scope, scope_id) bucket.
func (e *Engine) List(ctx context.Context, tenantID, scope, scopeID string) ([]Meta, error) {
	rows, err := e.store.CredentialDefList(ctx, tenantID, scope, scopeID)
	if err != nil {
		return nil, err
	}
	out := make([]Meta, len(rows))
	for i := range rows {
		out[i] = metaOf(rows[i])
	}
	return out, nil
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
	// AllowedURLs is the URL restriction the secret was stored with: nil for
	// none. Unlike Meta.AllowedURLs it is proven: the value would not have
	// opened with any other list.
	AllowedURLs *[]string
}

// Resolve finds a credential by NAME using scope precedence agent > user >
// tenant — the most specific wins, so a user's own token shadows a tenant
// default of the same name (per-user Telegram/Slack channels). It then decrypts
// via the row's backend. RUNTIME-SIDE ONLY: the returned plaintext must go
// straight to a header/env/provider, never into a transcript. agentName/userID
// may be "" (that scope is skipped). Returns (_, false, nil) when the name is
// absent from every visible scope.
//
// Resolve is for a caller that does not say where the secret is going. A
// credential that carries a URL restriction is therefore refused here with
// ErrRestricted: handing it out would be the very thing the restriction is
// there to stop. Use ResolveFor to name the destination.
func (e *Engine) Resolve(ctx context.Context, tenantID, agentName, userID, name string) (Resolved, bool, error) {
	return e.resolve(ctx, tenantID, agentName, userID, name, nil)
}

// ResolveFor is Resolve for a secret that is about to be sent to targetURL.
// A credential with a URL restriction resolves only when targetURL is under
// one of its entries, and is refused with ErrURLNotAllowed otherwise, before
// the secret is opened. An unrestricted credential resolves as with Resolve;
// a caller that requires a restriction checks Resolved.AllowedURLs.
//
// A restricted credential also pins the request: ctx must come from
// netguard.WithPinnable, and the request must be sent with it, so that its
// client follows no redirect away from targetURL. Without that it is refused
// with ErrNotPinnable.
func (e *Engine) ResolveFor(ctx context.Context, tenantID, agentName, userID, name, targetURL string) (Resolved, bool, error) {
	return e.resolve(ctx, tenantID, agentName, userID, name, &targetURL)
}

func (e *Engine) resolve(ctx context.Context, tenantID, agentName, userID, name string, target *string) (Resolved, bool, error) {
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
		// The restriction is decided on the row that the name resolves to,
		// and before the secret is opened. It does not fall through to a
		// less specific scope: a restricted credential must not be bypassed
		// by an unrestricted one of the same name further down.
		allowed := restrictionOf(row.Definition)
		if allowed != nil {
			if target == nil {
				return Resolved{}, false, fmt.Errorf("%w (%q)", ErrRestricted, name)
			}
			if !URLAllowed(allowed, *target) {
				return Resolved{}, false, fmt.Errorf("%w (%q)", ErrURLNotAllowed, name)
			}
			if !netguard.Pin(ctx) {
				return Resolved{}, false, fmt.Errorf("%w (%q)", ErrNotPinnable, name)
			}
		}
		backend, err := e.backendFor(row.Backend)
		if err != nil {
			return Resolved{}, false, err
		}
		// Opening proves the list just checked is the one the secret was
		// stored with: it is part of what the value was sealed with.
		val, err := backend.Resolve(ctx, row.Definition, Identity{
			TenantID: tenantID, Scope: b.scope, ScopeID: b.id, Name: name,
		})
		if err != nil {
			return Resolved{}, false, err
		}
		return Resolved{Value: val, Scope: b.scope, ScopeID: b.id, Backend: row.Backend, AllowedURLs: allowed}, true, nil
	}
	return Resolved{}, false, nil
}

// HasKey reports whether a credential named `name` exists in ANY scope visible
// to (tenantID, agentName, userID), using the SAME agent > user > tenant
// precedence as Resolve — but a METADATA-ONLY presence check: it calls
// CredentialDefGet and NEVER decrypts (no backend.Resolve), so RFC AX Layer-1
// credential-aware routing can test provider keyability without touching any
// plaintext. Respects CanResolve (no KEK ⇒ false ⇒ nothing is tenant-keyable,
// the correct restricted-run default). agentName / userID may be "" (that scope
// is skipped). Returns (false, nil) when absent from every visible scope; a
// store fault other than not-found propagates.
func (e *Engine) HasKey(ctx context.Context, tenantID, agentName, userID, name string) (bool, error) {
	if !e.CanResolve() {
		return false, nil
	}
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
			return false, err
		}
		// A URL-restricted credential is not a key a provider can be given:
		// Resolve refuses it, so reporting it here would promise a key the
		// run then does not get.
		return restrictionOf(row.Definition) == nil, nil // no decrypt needed
	}
	return false, nil
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

// metaOf reads what may be shown out of a row and drops the sealed Definition
// — the only shape that ever leaves the engine toward a caller or a log.
func metaOf(row store.CredentialDefRow) Meta {
	m := Meta{AllowedURLs: restrictionOf(row.Definition)}
	row.Definition = nil
	m.CredentialDefRow = row
	return m
}
