package credential

// Crypto-key credentials (RFC EP). A crypto key is a credential row whose
// definition holds numbered versions of key MATERIAL instead of one secret
// value. The material is opaque bytes here: what a key_type means, and whether
// the material fits it, is the Crypto tool's business. This package only seals
// it, resolves it to the one caller allowed to use it, and keeps it off every
// path that sends a credential somewhere.
//
// The separation is one way. An ordinary credential may serve as key material
// (ResolveKeyMaterial). A crypto key never resolves as a secret: Resolve,
// ResolveFor, Substitute ($cred:), ResolveToCompare, HasToCompare, HasKey and
// HasKeyFor all refuse it with ErrCryptoKey, and the refusal does not fall
// through to a same-named credential in a less specific scope.
//
// Stored definition (backend inline, in store.CredentialDefRow.Definition):
//
//	{"usage":"crypto","key_type":"ed25519","current":2,"min_version":1,
//	 "versions":[{"version":1,"created_at":"…","value":{key_id,nonce,ciphertext}},
//	             {"version":2,"created_at":"…","value":{…}}]}
//
// Each version is sealed with usage, key_type and its own version number in
// Identity.Bound (see cryptoBound), so editing any of them in the database
// leaves the key unopenable. current, min_version and created_at are NOT
// sealed: they are bookkeeping a database writer could change (lowering
// min_version would un-retire a version), the same trust level as deleting
// the row.

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// UsageCrypto is the definition's usage for a crypto-key credential.
const UsageCrypto = "crypto"

var (
	// ErrCryptoKey: a crypto key reached a path that hands a credential's
	// value to a header, env var, provider or comparison. Held, not missing.
	ErrCryptoKey = errors.New("credential: this credential is a crypto key, used only by the Crypto tool and never sent")
	// ErrNotCryptoKey: a crypto-key operation named an ordinary credential.
	ErrNotCryptoKey = errors.New("credential: this credential is not a crypto key")
	// ErrKeyExists: a create or import named a credential that already exists
	// in that scope (of any usage).
	ErrKeyExists = errors.New("credential: a credential with this name already exists in this scope")
	// ErrKeyInUse: a delete named a key or key version that is not retired.
	ErrKeyInUse = errors.New("credential: this crypto key version is not retired; raise min_version above it first")
	// ErrBadVersion: an unknown version, or a min_version that is lowered or
	// above the current version.
	ErrBadVersion = errors.New("credential: invalid crypto key version")
	// ErrUseKeyName: a crypto key was asked for as $cred: key material. It is
	// named directly, by its own name.
	ErrUseKeyName = errors.New("credential: a crypto key is named directly, not as $cred: key material")
	// ErrAmbiguousKey: a key name held in more than one scope the caller may
	// use keys from. Refused rather than resolved by precedence, so a key
	// stored in a narrower scope cannot stand in for a wider one's.
	ErrAmbiguousKey = errors.New("credential: the key name is held in more than one scope")
)

// MaxUsableKeyVersions caps a crypto key's unretired versions: each use opens
// them all, and verify tries each.
const MaxUsableKeyVersions = 32

var (
	// ErrKeyTransfer is the deliberately opaque failure to open a key
	// transfer: a wrong passphrase and a tampered blob look the same.
	ErrKeyTransfer = errors.New("credential: the key transfer did not open (wrong passphrase or damaged data)")
)

// KeyVersion describes one version of a crypto key (metadata only).
type KeyVersion struct {
	Version   int
	CreatedAt time.Time
	// Retired is Version < MinVersion: the version no longer resolves, and
	// may be deleted.
	Retired bool
}

// Scopes says which credential scopes a by-name crypto lookup may draw from.
// A name held only in a scope that is not allowed is simply not found.
type Scopes struct {
	Agent, User, Tenant bool
}

// CryptoKey is a resolved crypto key: its material by version, only versions
// >= MinVersion. RUNTIME-SIDE ONLY: the material must never reach a model, a
// transcript, a log or a stored event.
type CryptoKey struct {
	Name, Scope, ScopeID, KeyType string
	Current, MinVersion           int
	Versions                      map[int][]byte
}

var (
	keyTypeRe  = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
	cryptoName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
)

// cryptoDefinition is the stored definition of a crypto-key credential.
type cryptoDefinition struct {
	Usage      string          `json:"usage"`
	KeyType    string          `json:"key_type"`
	Current    int             `json:"current"`
	MinVersion int             `json:"min_version"`
	Versions   []cryptoVersion `json:"versions"`
}

type cryptoVersion struct {
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	Value     Sealed    `json:"value"`
}

// cryptoHeader is what tells a crypto-key definition apart from an ordinary
// one. An ordinary definition carries none of these fields, so ANY of them
// marks the row as crypto: a row whose usage was removed in the database is
// still kept off the secret paths (and no longer opens as a key).
type cryptoHeader struct {
	Usage    string          `json:"usage"`
	KeyType  string          `json:"key_type"`
	Versions json.RawMessage `json:"versions"`
}

// isCryptoDef reports whether a stored definition is a crypto key's.
func isCryptoDef(def json.RawMessage) bool {
	var h cryptoHeader
	if err := json.Unmarshal(def, &h); err != nil {
		return false
	}
	return h.Usage != "" || h.KeyType != "" || len(h.Versions) > 0
}

// cryptoBound is what each version is sealed together with. Its prefix
// differs from boundURLs' ("urls\x1e"), and parseCryptoDef only accepts a
// usage and key_type without the separator, so the encoding is unambiguous.
func cryptoBound(usage, keyType string, version int) []byte {
	return fmt.Appendf(nil, "crypto\x1e%s\x1e%s\x1e%d", usage, keyType, version)
}

// parseCryptoDef reads and checks a crypto definition. Any inconsistency is
// an error: the row is then unopenable, never half-used.
func parseCryptoDef(def json.RawMessage) (cryptoDefinition, error) {
	var d cryptoDefinition
	if err := json.Unmarshal(def, &d); err != nil {
		return d, err
	}
	if d.Usage != UsageCrypto {
		return d, errors.New("not a crypto-key definition")
	}
	if !keyTypeRe.MatchString(d.KeyType) {
		return d, errors.New("invalid key_type")
	}
	// min_version may be current+1: every version retired ("retire all").
	if d.Current < 1 || d.MinVersion < 1 || d.MinVersion > d.Current+1 {
		return d, errors.New("invalid min_version")
	}
	seen := map[int]bool{}
	for _, v := range d.Versions {
		if v.Version < 1 || v.Version > d.Current || seen[v.Version] {
			return d, errors.New("invalid version list")
		}
		seen[v.Version] = true
	}
	// A usable current version must be there; a retired one may have been
	// deleted.
	if d.Current >= d.MinVersion && !seen[d.Current] {
		return d, errors.New("current version missing")
	}
	return d, nil
}

// cryptoKeyErr is ErrCryptoKey naming the credential and its scope.
func cryptoKeyErr(name, scope string) error {
	return fmt.Errorf("%w (%q in %s scope)", ErrCryptoKey, name, scope)
}

// validCryptoIdentity checks the row a crypto operation writes.
func validCryptoIdentity(id Identity) error {
	if !cryptoName.MatchString(id.Name) {
		return fmt.Errorf("credential: invalid name %q (letters, digits, _ and -, at most 128)", id.Name)
	}
	switch id.Scope {
	case "tenant":
		if id.ScopeID != "" {
			return errors.New("credential: a tenant-scope credential has no scope_id")
		}
	case "user", "agent":
		if id.ScopeID == "" {
			return fmt.Errorf("credential: a %s-scope credential needs a scope_id", id.Scope)
		}
	default:
		return fmt.Errorf("credential: unknown scope %q", id.Scope)
	}
	return nil
}

func validKeyType(keyType string) error {
	if !keyTypeRe.MatchString(keyType) {
		return fmt.Errorf("credential: invalid key_type %q (lowercase letters, digits and -, at most 32)", keyType)
	}
	return nil
}

// sealVersion seals one version's material for the row id.
func (e *Engine) sealVersion(id Identity, keyType string, version int, created time.Time, material []byte) (cryptoVersion, error) {
	id.Bound = cryptoBound(UsageCrypto, keyType, version)
	sealed, err := e.sealer.Seal(material, id)
	if err != nil {
		return cryptoVersion{}, err
	}
	return cryptoVersion{Version: version, CreatedAt: created.UTC(), Value: sealed}, nil
}

// openVersion opens one version of a parsed crypto definition stored under id.
func (e *Engine) openVersion(id Identity, d cryptoDefinition, v cryptoVersion) ([]byte, error) {
	id.Bound = cryptoBound(d.Usage, d.KeyType, v.Version)
	return e.sealer.Open(v.Value, id)
}

// getCryptoRow reads the row under id and parses it as a crypto key.
// *store.ErrNotFound on a miss, ErrNotCryptoKey for an ordinary credential,
// ErrUnopenable for a crypto row that does not parse.
func (e *Engine) getCryptoRow(ctx context.Context, id Identity) (store.CredentialDefRow, cryptoDefinition, error) {
	row, err := e.store.CredentialDefGet(ctx, id.TenantID, id.Scope, id.ScopeID, id.Name)
	if err != nil {
		return row, cryptoDefinition{}, err
	}
	if !isCryptoDef(row.Definition) {
		return row, cryptoDefinition{}, fmt.Errorf("%w (%q in %s scope)", ErrNotCryptoKey, id.Name, id.Scope)
	}
	d, err := parseCryptoDef(row.Definition)
	if err != nil {
		return row, d, unopenable(id.Name, id.Scope, err)
	}
	return row, d, nil
}

// putCryptoRow stores d under row's key, keeping the row's other fields.
func (e *Engine) putCryptoRow(ctx context.Context, row store.CredentialDefRow, d cryptoDefinition) (Meta, error) {
	def, err := json.Marshal(d)
	if err != nil {
		return Meta{}, err
	}
	row.Backend = BackendInline
	row.Definition = def
	out, err := e.store.CredentialDefPut(ctx, row)
	if err != nil {
		return Meta{}, err
	}
	return e.metaOf(out), nil
}

// PutCryptoKey creates a crypto key under id with material as version 1
// (current 1, min_version 1). keyType is opaque here ([a-z0-9-]{1,32}); the
// caller checks the material fits it. Refused with ErrKeyExists when the name
// already has a row in that scope (of any usage), ErrNoKey when no
// LOOMCYCLE_SECRET_KEY is configured. Returns metadata only.
func (e *Engine) PutCryptoKey(ctx context.Context, id Identity, keyType string, material []byte, expiresAt *time.Time) (Meta, error) {
	if !e.sealer.Enabled() {
		return Meta{}, ErrNoKey
	}
	if err := validCryptoIdentity(id); err != nil {
		return Meta{}, err
	}
	if err := validKeyType(keyType); err != nil {
		return Meta{}, err
	}
	if len(material) == 0 {
		return Meta{}, errors.New("credential: crypto key material is empty")
	}
	if err := e.checkExpiresAt(expiresAt); err != nil {
		return Meta{}, err
	}
	e.cryptoMu.Lock()
	defer e.cryptoMu.Unlock()
	if err := e.absent(ctx, id); err != nil {
		return Meta{}, err
	}
	v, err := e.sealVersion(id, keyType, 1, e.now(), material)
	if err != nil {
		return Meta{}, err
	}
	return e.putCryptoRow(ctx, store.CredentialDefRow{
		TenantID: id.TenantID, Scope: id.Scope, ScopeID: id.ScopeID, Name: id.Name, ExpiresAt: expiresAt,
	}, cryptoDefinition{Usage: UsageCrypto, KeyType: keyType, Current: 1, MinVersion: 1, Versions: []cryptoVersion{v}})
}

// absent returns ErrKeyExists when a row is stored under id, nil when none is.
func (e *Engine) absent(ctx context.Context, id Identity) error {
	_, err := e.store.CredentialDefGet(ctx, id.TenantID, id.Scope, id.ScopeID, id.Name)
	if err == nil {
		return fmt.Errorf("%w (%q in %s scope)", ErrKeyExists, id.Name, id.Scope)
	}
	var nf *store.ErrNotFound
	if errors.As(err, &nf) {
		return nil
	}
	return err
}

// RotateCryptoKey adds material as version current+1 and makes it current.
// The newest stored version is opened first: the new version is sealed with
// the stored key_type, and carrying an edited key_type into a fresh seal
// would authenticate the edit. A key whose versions were all retired and
// deleted has nothing left to prove its key_type with, and is refused
// (ErrBadVersion): delete it and create it again. On a key whose versions
// are all retired (min_version = current+1) the new version is usable.
// Expiry is kept as it is.
func (e *Engine) RotateCryptoKey(ctx context.Context, id Identity, material []byte) (Meta, error) {
	if !e.sealer.Enabled() {
		return Meta{}, ErrNoKey
	}
	if len(material) == 0 {
		return Meta{}, errors.New("credential: crypto key material is empty")
	}
	e.cryptoMu.Lock()
	defer e.cryptoMu.Unlock()
	row, d, err := e.getCryptoRow(ctx, id)
	if err != nil {
		return Meta{}, err
	}
	if len(d.Versions) == 0 {
		return Meta{}, fmt.Errorf("%w: %q has no stored version left to authenticate its key_type; delete it and create it again", ErrBadVersion, id.Name)
	}
	newest := slices.MaxFunc(d.Versions, func(a, b cryptoVersion) int { return a.Version - b.Version })
	if _, err := e.openVersion(id, d, newest); err != nil {
		return Meta{}, unopenable(id.Name, id.Scope, err)
	}
	// Every use opens each usable version, and verify tries each in turn, so
	// their number bounds what one call costs.
	usable := 0
	for _, v := range d.Versions {
		if v.Version >= d.MinVersion {
			usable++
		}
	}
	if usable >= MaxUsableKeyVersions {
		return Meta{}, fmt.Errorf("%w: %q already has %d usable versions; retire older ones (raise min_version) before rotating again",
			ErrBadVersion, id.Name, usable)
	}
	next := d.Current + 1
	v, err := e.sealVersion(id, d.KeyType, next, e.now(), material)
	if err != nil {
		return Meta{}, err
	}
	d.Versions = append(d.Versions, v)
	d.Current = next
	return e.putCryptoRow(ctx, row, d)
}

// SetCryptoMinVersion retires every version below minVersion. It only
// raises (an equal value is a no-op), up to current+1, which retires every
// version: the key then resolves with no usable version, and may be deleted.
// ErrBadVersion otherwise.
func (e *Engine) SetCryptoMinVersion(ctx context.Context, id Identity, minVersion int) (Meta, error) {
	e.cryptoMu.Lock()
	defer e.cryptoMu.Unlock()
	row, d, err := e.getCryptoRow(ctx, id)
	if err != nil {
		return Meta{}, err
	}
	switch {
	case minVersion < d.MinVersion:
		return Meta{}, fmt.Errorf("%w: min_version can only be raised (it is %d)", ErrBadVersion, d.MinVersion)
	case minVersion > d.Current+1:
		return Meta{}, fmt.Errorf("%w: min_version %d is above current+1 (%d retires every version)", ErrBadVersion, minVersion, d.Current+1)
	case minVersion == d.MinVersion:
		return e.metaOf(row), nil
	}
	d.MinVersion = minVersion
	return e.putCryptoRow(ctx, row, d)
}

// DeleteCryptoKeyVersion removes one retired version (below min_version) and
// its material. An unknown version is ErrBadVersion, an unretired one
// ErrKeyInUse.
func (e *Engine) DeleteCryptoKeyVersion(ctx context.Context, id Identity, version int) (Meta, error) {
	e.cryptoMu.Lock()
	defer e.cryptoMu.Unlock()
	row, d, err := e.getCryptoRow(ctx, id)
	if err != nil {
		return Meta{}, err
	}
	i := slices.IndexFunc(d.Versions, func(v cryptoVersion) bool { return v.Version == version })
	if i < 0 {
		return Meta{}, fmt.Errorf("%w: %q has no version %d", ErrBadVersion, id.Name, version)
	}
	if version >= d.MinVersion {
		return Meta{}, fmt.Errorf("%w (%q version %d, min_version %d)", ErrKeyInUse, id.Name, version, d.MinVersion)
	}
	d.Versions = slices.Delete(d.Versions, i, i+1)
	return e.putCryptoRow(ctx, row, d)
}

// cryptoDeleteRefusal is why Delete must not remove a stored crypto row: any
// version >= min_version still protects data. nil only once every version is
// retired (min_version > current, SetCryptoMinVersion to current+1). A crypto
// row that does not parse is refused too: what it protects cannot be told.
func cryptoDeleteRefusal(row store.CredentialDefRow) error {
	var d cryptoDefinition
	if err := json.Unmarshal(row.Definition, &d); err != nil {
		return fmt.Errorf("%w (%q): its stored definition cannot be read", ErrKeyInUse, row.Name)
	}
	if d.MinVersion <= d.Current {
		return fmt.Errorf("%w (%q current version %d, min_version %d); retire every version (min_version %d) before deleting the key",
			ErrKeyInUse, row.Name, d.Current, d.MinVersion, d.Current+1)
	}
	for _, v := range d.Versions {
		if v.Version >= d.MinVersion {
			return fmt.Errorf("%w (%q version %d, min_version %d); a crypto key is deleted only when every version is retired",
				ErrKeyInUse, row.Name, v.Version, d.MinVersion)
		}
	}
	return nil
}

// findIn is find restricted to the scopes in allowed.
func (e *Engine) findIn(ctx context.Context, tenantID, agentName, userID, name string, allowed Scopes) (store.CredentialDefRow, bool, error) {
	if !allowed.Agent {
		agentName = ""
	}
	if !allowed.User {
		userID = ""
	}
	row, found, err := e.findScopes(ctx, tenantID, agentName, userID, name, allowed.Tenant)
	if err != nil || !found {
		return row, found, err
	}
	// A key is not chosen by precedence. For a $cred: secret a narrower scope
	// shadowing a wider one is the point (a user's own token over the
	// tenant's default); for a key it is a substitution: a user, or an agent
	// in a run, who stores a key under the tenant key's name would have every
	// run on their behalf sign, MAC or encrypt with a key they chose, and
	// verify or decrypt what they forged as if the tenant's key had made it.
	// So a name held in more than one of the allowed scopes is refused.
	n, err := e.holdersIn(ctx, tenantID, agentName, userID, name, allowed.Tenant)
	if err != nil {
		return store.CredentialDefRow{}, false, err
	}
	if n > 1 {
		return store.CredentialDefRow{}, false, fmt.Errorf("%w (%q is held in more than one scope this run may use keys from)", ErrAmbiguousKey, name)
	}
	return row, true, nil
}

// holdersIn counts the scopes, among those findScopes would look in, that
// hold a credential named name (any usage, expired ones too).
func (e *Engine) holdersIn(ctx context.Context, tenantID, agentName, userID, name string, withTenant bool) (int, error) {
	if userID == systemUserID {
		userID = ""
	}
	buckets := []struct{ scope, id string }{{"agent", agentName}, {"user", userID}, {"tenant", ""}}
	n := 0
	for _, b := range buckets {
		if (b.scope != "tenant" && b.id == "") || (b.scope == "tenant" && !withTenant) {
			continue
		}
		if _, err := e.store.CredentialDefGet(ctx, tenantID, b.scope, b.id, name); err != nil {
			var nf *store.ErrNotFound
			if errors.As(err, &nf) {
				continue
			}
			return 0, err
		}
		n++
	}
	return n, nil
}

// ResolveCryptoKey finds a crypto key by name, agent > user > tenant, using
// only the scopes in allowed, and opens every version >= min_version. The
// winning row decides: expired → ErrExpired; an ordinary credential →
// ErrNotCryptoKey; not openable (no key, tampered usage/key_type/version, a
// corrupt seal) → ErrUnopenable. None of them falls through to a less
// specific scope. (_, false, nil) when the name is in no allowed scope. A key
// whose versions are all retired resolves (found, no error) with an empty
// Versions map: the caller refuses any operation on it.
func (e *Engine) ResolveCryptoKey(ctx context.Context, tenantID, agentName, userID, name string, allowed Scopes) (CryptoKey, bool, error) {
	row, found, err := e.findIn(ctx, tenantID, agentName, userID, name, allowed)
	if err != nil || !found {
		return CryptoKey{}, false, err
	}
	if !isCryptoDef(row.Definition) {
		return CryptoKey{}, false, fmt.Errorf("%w (%q in %s scope)", ErrNotCryptoKey, name, row.Scope)
	}
	d, err := parseCryptoDef(row.Definition)
	if err != nil {
		return CryptoKey{}, false, unopenable(name, row.Scope, err)
	}
	id := Identity{TenantID: tenantID, Scope: row.Scope, ScopeID: row.ScopeID, Name: name}
	versions := map[int][]byte{}
	for _, v := range d.Versions {
		if v.Version < d.MinVersion {
			continue
		}
		pt, err := e.openVersion(id, d, v)
		if err != nil {
			return CryptoKey{}, false, unopenable(name, row.Scope, err)
		}
		versions[v.Version] = pt
	}
	return CryptoKey{
		Name: name, Scope: row.Scope, ScopeID: row.ScopeID, KeyType: d.KeyType,
		Current: d.Current, MinVersion: d.MinVersion, Versions: versions,
	}, true, nil
}

// ResolveKeyMaterial resolves an ORDINARY credential's value for use as key
// material, agent > user > tenant within allowed. A crypto key is refused
// with ErrUseKeyName (it is named directly), a URL-restricted credential with
// ErrRestricted (its owner restricted where it goes, not agreed to its use as
// a key), an expired one with ErrExpired; none falls through.
func (e *Engine) ResolveKeyMaterial(ctx context.Context, tenantID, agentName, userID, name string, allowed Scopes) (Resolved, bool, error) {
	row, found, err := e.findIn(ctx, tenantID, agentName, userID, name, allowed)
	if found && isCryptoDef(row.Definition) {
		return Resolved{}, false, fmt.Errorf("%w (%q in %s scope)", ErrUseKeyName, name, row.Scope)
	}
	if err != nil || !found {
		return Resolved{}, false, err
	}
	if restrictionOf(row.Definition) != nil {
		return Resolved{}, false, fmt.Errorf("%w (%q)", ErrRestricted, name)
	}
	val, err := e.open(ctx, row, Identity{TenantID: tenantID, Scope: row.Scope, ScopeID: row.ScopeID, Name: name})
	if err != nil {
		return Resolved{}, false, err
	}
	return Resolved{Value: val, Scope: row.Scope, ScopeID: row.ScopeID, Backend: row.Backend, Username: usernameOf(row.Definition)}, true, nil
}

// ---- key transfer ----

const (
	transferFormat  = "lcx-keytransfer-1"
	transferKDF     = "pbkdf2-sha256"
	transferIter    = 600000
	transferMaxIter = 10_000_000
	transferAADBase = "loomcycle/crypto-key-transfer/v1"
	minPassphrase   = 16
)

type transferEnvelope struct {
	Format     string            `json:"format"`
	Name       string            `json:"name"`
	KeyType    string            `json:"key_type"`
	KDF        transferKDFParams `json:"kdf"`
	Nonce      string            `json:"nonce"`
	Ciphertext string            `json:"ciphertext"`
}

type transferKDFParams struct {
	Alg  string `json:"alg"`
	Iter int    `json:"iter"`
	Salt string `json:"salt"`
}

type transferPayload struct {
	Versions   []transferVersion `json:"versions"`
	Current    int               `json:"current"`
	MinVersion int               `json:"min_version"`
	ExpiresAt  *time.Time        `json:"expires_at,omitempty"`
}

type transferVersion struct {
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	Material  []byte    `json:"material"` // base64 in JSON
}

func transferAAD(name, keyType string) []byte {
	return []byte(transferAADBase + "\x1f" + name + "\x1f" + keyType)
}

func transferAEAD(passphrase string, salt []byte, iter int) (cipher.AEAD, error) {
	k, err := pbkdf2.Key(sha256.New, passphrase, salt, iter, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// ExportCryptoKey opens every version of the crypto key under id (retired
// ones too) and wraps them, with current, min_version and expires_at, under a
// key derived from passphrase (PBKDF2-HMAC-SHA256, 600000 iterations, a
// random 16-byte salt; AES-256-GCM with the name and key_type as associated
// data). The result is a base64url string to hand to ImportCryptoKey on the
// target. passphrase must be at least 16 characters. Who may export is the
// caller's decision (admin only).
func (e *Engine) ExportCryptoKey(ctx context.Context, id Identity, passphrase string) (string, error) {
	if utf8.RuneCountInString(passphrase) < minPassphrase {
		return "", fmt.Errorf("credential: the transfer passphrase must be at least %d characters", minPassphrase)
	}
	row, d, err := e.getCryptoRow(ctx, id)
	if err != nil {
		return "", err
	}
	p := transferPayload{Current: d.Current, MinVersion: d.MinVersion, ExpiresAt: row.ExpiresAt}
	defer func() {
		for _, v := range p.Versions {
			clear(v.Material)
		}
	}()
	for _, v := range d.Versions {
		pt, err := e.openVersion(id, d, v)
		if err != nil {
			return "", unopenable(id.Name, id.Scope, err)
		}
		p.Versions = append(p.Versions, transferVersion{Version: v.Version, CreatedAt: v.CreatedAt, Material: pt})
	}
	plain, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	defer clear(plain)
	salt := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return "", err
	}
	aead, err := transferAEAD(passphrase, salt, transferIter)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	env, err := json.Marshal(transferEnvelope{
		Format: transferFormat, Name: id.Name, KeyType: d.KeyType,
		KDF:        transferKDFParams{Alg: transferKDF, Iter: transferIter, Salt: base64.StdEncoding.EncodeToString(salt)},
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(aead.Seal(nil, nonce, plain, transferAAD(id.Name, d.KeyType))),
	})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(env), nil
}

// ImportCryptoKey unwraps an ExportCryptoKey blob and stores the key under
// id, sealed for this deployment, with its versions, created_at, current,
// min_version and expires_at. Only when no row exists under id (of any
// usage): ErrKeyExists otherwise. id.Name must be the name the key was
// exported under. A wrong passphrase or a tampered blob is ErrKeyTransfer,
// which does not say which.
func (e *Engine) ImportCryptoKey(ctx context.Context, id Identity, blob, passphrase string) (Meta, error) {
	if !e.sealer.Enabled() {
		return Meta{}, ErrNoKey
	}
	if err := validCryptoIdentity(id); err != nil {
		return Meta{}, err
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(strings.TrimSpace(blob), "="))
	if err != nil {
		return Meta{}, errors.New("credential: the key transfer is not valid base64url")
	}
	var env transferEnvelope
	if err := json.Unmarshal(raw, &env); err != nil || env.Format != transferFormat {
		return Meta{}, errors.New("credential: not a loomcycle key transfer")
	}
	if env.KDF.Alg != transferKDF || env.KDF.Iter < transferIter || env.KDF.Iter > transferMaxIter {
		return Meta{}, errors.New("credential: the key transfer uses an unsupported key derivation")
	}
	if env.Name != id.Name {
		return Meta{}, fmt.Errorf("credential: the key transfer is for %q, not %q", env.Name, id.Name)
	}
	if err := validKeyType(env.KeyType); err != nil {
		return Meta{}, err
	}
	salt, err1 := base64.StdEncoding.DecodeString(env.KDF.Salt)
	nonce, err2 := base64.StdEncoding.DecodeString(env.Nonce)
	ct, err3 := base64.StdEncoding.DecodeString(env.Ciphertext)
	if err1 != nil || err2 != nil || err3 != nil {
		return Meta{}, ErrKeyTransfer
	}
	aead, err := transferAEAD(passphrase, salt, env.KDF.Iter)
	if err != nil || len(nonce) != aead.NonceSize() {
		return Meta{}, ErrKeyTransfer
	}
	plain, err := aead.Open(nil, nonce, ct, transferAAD(env.Name, env.KeyType))
	if err != nil {
		return Meta{}, ErrKeyTransfer
	}
	defer clear(plain)
	var p transferPayload
	if err := json.Unmarshal(plain, &p); err != nil {
		return Meta{}, ErrKeyTransfer
	}
	defer func() {
		for _, v := range p.Versions {
			clear(v.Material)
		}
	}()
	if err := validTransferPayload(p); err != nil {
		return Meta{}, err
	}
	if p.ExpiresAt != nil && (p.ExpiresAt.Before(minExpiry) || p.ExpiresAt.After(maxExpiry)) {
		return Meta{}, errors.New("credential: the key transfer's expires_at is outside the range a credential can store")
	}

	e.cryptoMu.Lock()
	defer e.cryptoMu.Unlock()
	if err := e.absent(ctx, id); err != nil {
		return Meta{}, err
	}
	d := cryptoDefinition{Usage: UsageCrypto, KeyType: env.KeyType, Current: p.Current, MinVersion: p.MinVersion}
	for _, v := range p.Versions {
		sv, err := e.sealVersion(id, env.KeyType, v.Version, v.CreatedAt, v.Material)
		if err != nil {
			return Meta{}, err
		}
		d.Versions = append(d.Versions, sv)
	}
	return e.putCryptoRow(ctx, store.CredentialDefRow{
		TenantID: id.TenantID, Scope: id.Scope, ScopeID: id.ScopeID, Name: id.Name, ExpiresAt: p.ExpiresAt,
	}, d)
}

// validTransferPayload applies parseCryptoDef's consistency rules to an
// unwrapped payload. It authenticated, so a failure here is a bad export,
// not an attack, and may say what is wrong.
func validTransferPayload(p transferPayload) error {
	if p.Current < 1 || p.MinVersion < 1 || p.MinVersion > p.Current+1 {
		return errors.New("credential: the key transfer has an invalid min_version")
	}
	seen := map[int]bool{}
	for _, v := range p.Versions {
		if v.Version < 1 || v.Version > p.Current || seen[v.Version] || len(v.Material) == 0 {
			return errors.New("credential: the key transfer has an invalid version list")
		}
		seen[v.Version] = true
	}
	if p.Current >= p.MinVersion && !seen[p.Current] {
		return errors.New("credential: the key transfer is missing its current version")
	}
	return nil
}

// cryptoMeta fills m's crypto fields from a stored definition, as the row
// says (unauthenticated, like Meta.AllowedURLs). A no-op for an ordinary row.
func cryptoMeta(m *Meta, def json.RawMessage) {
	if !isCryptoDef(def) {
		return
	}
	var d cryptoDefinition
	if err := json.Unmarshal(def, &d); err != nil {
		m.Usage = UsageCrypto
		return
	}
	m.Usage = d.Usage
	if m.Usage == "" {
		m.Usage = UsageCrypto
	}
	m.KeyType = d.KeyType
	m.CurrentVersion = d.Current
	m.MinVersion = d.MinVersion
	m.Versions = make([]KeyVersion, 0, len(d.Versions))
	for _, v := range d.Versions {
		m.Versions = append(m.Versions, KeyVersion{Version: v.Version, CreatedAt: v.CreatedAt, Retired: v.Version < d.MinVersion})
	}
}
