package credential

import (
	"context"
	"errors"
	"fmt"
	"math"
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
	// now is the clock expires_at is judged against; time.Now outside tests.
	now func() time.Time
}

// ErrExpired is returned when the credential a name resolves to has passed
// its expires_at. The wrapped message names the credential, its scope and
// when it expired — never its value.
var ErrExpired = errors.New("credential: this credential has expired")

// The span of times an int64 count of nanoseconds since 1970 can name.
var (
	minExpiry = time.Unix(0, math.MinInt64).UTC()
	maxExpiry = time.Unix(0, math.MaxInt64).UTC()
)

// NewEngine builds an Engine over a store and a Sealer (the Sealer may be
// disabled — no KEK — in which case PutInline fails and inline resolves error).
func NewEngine(st store.Store, sealer *Sealer) *Engine {
	return &Engine{store: st, sealer: sealer, inline: inlineBackend{sealer: sealer}, now: time.Now}
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
	// Username is the user name stored with the value ("" for none).
	Username string
	// Expired reports that ExpiresAt has passed: the credential no longer
	// resolves, and is kept only so it can be seen and rotated.
	Expired bool
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
//
// username, already checked (ValidUsername), is the user name to store with
// the value. nil keeps the one the credential has, so a rotation keeps it
// too; a pointer to "" removes it.
//
// expiresAt (nil for none) must be in the future and before 2262, which every
// store can hold. A new write replaces it, so a rotation without one clears it.
func (e *Engine) PutInline(ctx context.Context, id Identity, plaintext string, expiresAt *time.Time, allowedURLs *[]string, username *string) (Meta, error) {
	if !e.sealer.Enabled() {
		return Meta{}, ErrNoKey
	}
	// The SQLite store keeps the time as int64 nanoseconds, which wraps
	// outside this range: a "never, really" expiry of 2999 would read back
	// as a time long past and the credential would be refused from the day
	// it was stored.
	if expiresAt != nil && (expiresAt.Before(minExpiry) || expiresAt.After(maxExpiry)) {
		return Meta{}, fmt.Errorf("expires_at %s is outside the range a credential can store (%s to %s); leave it unset for no expiry",
			expiresAt.UTC().Format(time.RFC3339), minExpiry.Format(time.RFC3339), maxExpiry.Format(time.RFC3339))
	}
	// A credential created already expired could never be used: every lookup
	// would refuse it. That is always a mistake (a typo'd year, a stale
	// script), so say so at create rather than at the first failed send.
	if expiresAt != nil && !e.now().Before(*expiresAt) {
		return Meta{}, fmt.Errorf("expires_at %s is not in the future, so the credential could never be used; send a later time, or leave it unset for no expiry",
			expiresAt.UTC().Format(time.RFC3339))
	}
	if allowedURLs == nil {
		kept, err := e.existingRestriction(ctx, id)
		if err != nil {
			return Meta{}, err
		}
		allowedURLs = kept
	}
	name := ""
	if username != nil {
		name = *username
	} else {
		kept, err := e.existingUsername(ctx, id)
		if err != nil {
			return Meta{}, err
		}
		name = kept
	}
	def, err := e.inline.sealInline(plaintext, id, allowedURLs, name)
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
	return e.metaOf(row), nil
}

// existingUsername returns the user name of the credential stored under id,
// or "" when there is no such credential or it has none.
func (e *Engine) existingUsername(ctx context.Context, id Identity) (string, error) {
	row, err := e.store.CredentialDefGet(ctx, id.TenantID, id.Scope, id.ScopeID, id.Name)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return "", nil
		}
		return "", err
	}
	return usernameOf(row.Definition), nil
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
	return e.metaOf(row), nil
}

// List returns metadata for a single (tenant, scope, scope_id) bucket.
func (e *Engine) List(ctx context.Context, tenantID, scope, scopeID string) ([]Meta, error) {
	rows, err := e.store.CredentialDefList(ctx, tenantID, scope, scopeID)
	if err != nil {
		return nil, err
	}
	out := make([]Meta, len(rows))
	for i := range rows {
		out[i] = e.metaOf(rows[i])
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
	// Username is the user name stored with the value ("" for none). Not
	// sealed with it: a caller uses it only beside the value, to a
	// destination the value is already allowed to reach.
	Username string
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
	row, found, err := e.find(ctx, tenantID, agentName, userID, name)
	if err != nil || !found {
		return Resolved{}, false, err
	}
	// The restriction is decided on the row that the name resolves to, and
	// before the secret is opened. It does not fall through to a less
	// specific scope: a restricted credential must not be bypassed by an
	// unrestricted one of the same name further down.
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
	// Opening proves the list just checked is the one the secret was stored
	// with: it is part of what the value was sealed with.
	val, err := backend.Resolve(ctx, row.Definition, Identity{
		TenantID: tenantID, Scope: row.Scope, ScopeID: row.ScopeID, Name: name,
	})
	if err != nil {
		return Resolved{}, false, err
	}
	return Resolved{Value: val, Scope: row.Scope, ScopeID: row.ScopeID, Backend: row.Backend, AllowedURLs: allowed, Username: usernameOf(row.Definition)}, true, nil
}

// find returns the row a credential name resolves to for a run identity:
// the most specific of agent > user > tenant that holds the name.
// agentName / userID may be "" (that scope is skipped). found is false when
// the name is in no visible scope; a store fault other than not-found is
// returned. Every by-name lookup goes through here, so a rule about which
// row a name may resolve to is decided once.
//
// An expired row is found (found is true) and refused with ErrExpired. It
// does not fall through to a less specific scope, for the reason a
// restricted one does not: a user whose own token expired must be told so,
// not quietly sent under the tenant's token of the same name.
func (e *Engine) find(ctx context.Context, tenantID, agentName, userID, name string) (row store.CredentialDefRow, found bool, err error) {
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
			return store.CredentialDefRow{}, false, err
		}
		if err := e.expiredErr(row, name); err != nil {
			return row, true, err
		}
		return row, true, nil
	}
	return store.CredentialDefRow{}, false, nil
}

// expired reports whether row has reached its expires_at. A credential is
// expired AT its expires_at, not after it: a value valid "until" a time is
// not handed out at that time.
func (e *Engine) expired(row store.CredentialDefRow) bool {
	return row.ExpiresAt != nil && !e.now().Before(*row.ExpiresAt)
}

// expiredErr is ErrExpired for an expired row, naming the credential, its
// scope and when it expired (never its value); nil otherwise.
func (e *Engine) expiredErr(row store.CredentialDefRow, name string) error {
	if !e.expired(row) {
		return nil
	}
	return fmt.Errorf("%w (%q in %s scope, expired %s); create it again with the same name to rotate it",
		ErrExpired, name, row.Scope, row.ExpiresAt.UTC().Format(time.RFC3339))
}

// ResolveToCompare returns the tenant-level credential name of tenantID for a
// caller that compares the value in process and sends it nowhere: a webhook's
// verification secret, checked against what a request carries. Only the
// tenant level is read — the secret belongs to whoever wrote the definition,
// not to a run's agent or user.
//
// A URL restriction (allowed_urls) says where a value may be sent. This one
// is sent nowhere, so a restricted credential resolves here as any other
// does; one restricted to no URL at all is the way to store a secret that
// must only ever be compared. Returns (_, false, nil) when the tenant has no
// such credential, or the engine cannot open any.
func (e *Engine) ResolveToCompare(ctx context.Context, tenantID, name string) (string, bool, error) {
	if !e.CanResolve() {
		return "", false, nil
	}
	row, err := e.store.CredentialDefGet(ctx, tenantID, "tenant", "", name)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return "", false, nil
		}
		return "", false, err
	}
	// An expired verification secret verifies nothing: a webhook whose
	// secret lapsed refuses its deliveries rather than accepting them.
	if err := e.expiredErr(row, name); err != nil {
		return "", false, err
	}
	backend, err := e.backendFor(row.Backend)
	if err != nil {
		return "", false, err
	}
	val, err := backend.Resolve(ctx, row.Definition, Identity{TenantID: tenantID, Scope: "tenant", Name: name})
	if err != nil {
		return "", false, err
	}
	return val, true, nil
}

// HasToCompare reports whether ResolveToCompare would find name in tenantID:
// metadata only, nothing is opened. An authoring check asks it before saving
// a definition that names the credential.
func (e *Engine) HasToCompare(ctx context.Context, tenantID, name string) (bool, error) {
	if !e.CanResolve() {
		return false, nil
	}
	row, err := e.store.CredentialDefGet(ctx, tenantID, "tenant", "", name)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return false, nil
		}
		return false, err
	}
	// Held, but ResolveToCompare would refuse it.
	if err := e.expiredErr(row, name); err != nil {
		return false, err
	}
	return true, nil
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
//
// An expired credential is (false, ErrExpired), not (false, nil): the run
// HAS a key of that name, it just may not be used, and a caller that treats
// "absent" as "use the operator's key instead" must not do so for it.
func (e *Engine) HasKey(ctx context.Context, tenantID, agentName, userID, name string) (bool, error) {
	if !e.CanResolve() {
		return false, nil
	}
	row, found, err := e.find(ctx, tenantID, agentName, userID, name)
	if err != nil || !found {
		return false, err
	}
	// A URL-restricted credential is not a key a provider can be given:
	// Resolve refuses it, so reporting it here would promise a key the run
	// then does not get.
	return restrictionOf(row.Definition) == nil, nil // no decrypt needed
}

// HasKeyFor is HasKey for a credential about to be sent to targetURL: it
// reports whether the name exists in a visible scope (found), and whether
// ResolveFor would hand it out for targetURL (usable) — unrestricted, or
// restricted to a list that covers targetURL. Metadata only, like HasKey: an
// authoring check can ask the question a send will ask, without decrypting.
// An expired credential is found and refused with ErrExpired, so the check
// can say why rather than blame its URL list.
func (e *Engine) HasKeyFor(ctx context.Context, tenantID, agentName, userID, name, targetURL string) (found, usable bool, err error) {
	if !e.CanResolve() {
		return false, false, nil
	}
	row, found, err := e.find(ctx, tenantID, agentName, userID, name)
	if err != nil || !found {
		return found, false, err
	}
	allowed := restrictionOf(row.Definition)
	return true, allowed == nil || URLAllowed(allowed, targetURL), nil
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
func (e *Engine) metaOf(row store.CredentialDefRow) Meta {
	m := Meta{AllowedURLs: restrictionOf(row.Definition), Username: usernameOf(row.Definition), Expired: e.expired(row)}
	row.Definition = nil
	m.CredentialDefRow = row
	return m
}
