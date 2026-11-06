package credential

// OAuth credentials (RFC GB). An OAuth credential is a credential row whose
// definition holds the configuration of a connection to an OAuth
// authorization server and, once connected, the grant: a refresh token and
// a short-lived access token the runtime keeps fresh. Other config names it
// as $oauth:<name>; the access token is substituted at send time, exactly
// where a $cred: value would be, and nothing else of it ever leaves.
//
// Stored definition (backend inline, in credentialdef.Row.Definition):
//
//	{"usage":"oauth",
//	 "config":{"resource":"https://…","issuer":"https://…",
//	           "client":{"registration":"dynamic","token_endpoint_auth":"none"},
//	           "scopes":["…"]},
//	 "allowed_urls":["https://…"],
//	 "status":{"state":"connected","connected_as":"…",…},
//	 "state_version":7,
//	 "lease":{"holder":"…","purpose":"refresh","until":"…"},
//	 "secret":{key_id,nonce,ciphertext}}
//
// The secret (oauthSecret) is sealed with the usage, a hash of the config and
// the URL restriction in Identity.Bound (oauthBound), so a row whose config
// or allowed_urls were edited in the database no longer opens. The
// discovered endpoints are inside the secret, not beside it: a database
// writer cannot point the token endpoint at a host of its choosing. The
// secret also carries the state_version it was written at, which must equal
// the clear one: an old ciphertext spliced under a newer header does not
// open. status, state_version and lease are bookkeeping in the clear, the
// same trust level as deleting the row.
//
// The store's conditional write (CredentialDefPutIf) is what keeps replicas
// from losing each other's refresh: every write is a compare-and-set against
// the definition the writer read.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/denn-gubsky/loomcycle/internal/credential/credentialdef"
	"github.com/denn-gubsky/loomcycle/internal/netguard"
)

// UsageOAuth is the definition's usage for an OAuth credential.
const UsageOAuth = "oauth"

// The states of an OAuth credential.
const (
	// OAuthPendingConnect: created, or a connect is under way; nothing to send.
	OAuthPendingConnect = "pending_connect"
	// OAuthConnected: a grant is held and its access token is sent.
	OAuthConnected = "connected"
	// OAuthNeedsReauth: the grant was lost (refused refresh, expired device
	// code, a failed write); its owner connects it again.
	OAuthNeedsReauth = "needs_reauth"
	// OAuthRevoked: disconnected by its owner.
	OAuthRevoked = "revoked"
)

// The client registration modes and token endpoint auth methods.
const (
	OAuthRegistrationDynamic          = "dynamic"
	OAuthRegistrationMetadataDocument = "metadata_document"
	OAuthRegistrationStatic           = "static"

	OAuthAuthNone              = "none"
	OAuthAuthClientSecretBasic = "client_secret_basic"
	OAuthAuthClientSecretPost  = "client_secret_post"
)

// maxOAuthScopes and maxOAuthScopeLen bound the scopes a config may ask for.
const (
	maxOAuthScopes   = 32
	maxOAuthScopeLen = 128
	maxOAuthURLLen   = 2048
)

var (
	// ErrOAuthCredential: an OAuth credential reached a path that hands out
	// a stored secret ($cred:, a provider key, key material). It is named as
	// $oauth:<name>; the refusal does not fall through to a same-named
	// credential further down.
	ErrOAuthCredential = errors.New("credential: this credential is connected through OAuth; name it as $oauth:<name>")
	// ErrNotOAuth: an $oauth: reference or an OAuth op named a credential
	// that is not an OAuth credential.
	ErrNotOAuth = errors.New("credential: this credential is not an OAuth credential")
	// ErrOAuthNotConnected: the OAuth credential holds no grant yet.
	ErrOAuthNotConnected = errors.New("credential: this OAuth credential is not connected; its owner connects it with oauth_connect_start")
	// ErrOAuthReauthRequired: the OAuth credential lost its grant and its
	// owner must connect it again. Returned wrapped in *OAuthReauthError.
	ErrOAuthReauthRequired = errors.New("credential: this OAuth credential must be connected again")
)

// OAuthReauthError says which OAuth credential must be connected again and
// why. It never carries token text: Reason is one of a few fixed phrases, or
// an authorization server's error code.
type OAuthReauthError struct {
	Name   string
	Scope  string
	Reason string
}

func (e *OAuthReauthError) Error() string {
	return fmt.Sprintf("%v (%q in %s scope: %s); its owner connects it again with oauth_connect_start", ErrOAuthReauthRequired, e.Name, e.Scope, e.Reason)
}

func (e *OAuthReauthError) Unwrap() error { return ErrOAuthReauthRequired }

// OAuthConfig is what an OAuth credential connects to. It is not secret and
// is shown by get and list.
type OAuthConfig struct {
	// Resource is the protected resource (an MCP server's URL) the grant is
	// for; sent as the resource parameter (RFC 8707).
	Resource string `json:"resource"`
	// Issuer is the authorization server, when the resource lists more than
	// one; "" lets discovery take the only one listed.
	Issuer string            `json:"issuer,omitempty"`
	Client OAuthClientConfig `json:"client"`
	Scopes []string          `json:"scopes,omitempty"`
}

// OAuthClientConfig is how the runtime is registered as a client.
type OAuthClientConfig struct {
	Registration      string `json:"registration"`
	TokenEndpointAuth string `json:"token_endpoint_auth"`
}

// OAuthStatus is the clear status of an OAuth credential. None of it is
// secret; it says what state the grant is in, never what it holds.
type OAuthStatus struct {
	State           string     `json:"state"`
	ConnectedAs     string     `json:"connected_as,omitempty"`
	ConnectedAt     *time.Time `json:"connected_at,omitempty"`
	LastRefreshAt   *time.Time `json:"last_refresh_at,omitempty"`
	AccessExpiresAt *time.Time `json:"access_expires_at,omitempty"`
	LastError       string     `json:"last_error,omitempty"`
}

// OAuthMeta is what Meta tells about an OAuth credential: its config and
// status, never its tokens or registration secrets.
type OAuthMeta struct {
	Config OAuthConfig
	Status OAuthStatus
}

// oauthLease marks a replica's refresh or connect under way, so the others
// wait for it instead of spending the refresh token twice.
type oauthLease struct {
	Holder  string    `json:"holder"`
	Purpose string    `json:"purpose"`
	Until   time.Time `json:"until"`
}

const (
	leaseRefresh = "refresh"
	leaseConnect = "connect"
)

// oauthDefinition is the stored definition of an OAuth credential.
type oauthDefinition struct {
	Usage        string      `json:"usage"`
	Config       OAuthConfig `json:"config"`
	AllowedURLs  *[]string   `json:"allowed_urls"`
	Status       OAuthStatus `json:"status"`
	StateVersion int64       `json:"state_version"`
	Lease        *oauthLease `json:"lease,omitempty"`
	Secret       *Sealed     `json:"secret,omitempty"`
}

// oauthEndpoints are the authorization server's endpoints, as discovered.
type oauthEndpoints struct {
	Issuer              string `json:"issuer"`
	Token               string `json:"token"`
	DeviceAuthorization string `json:"device_authorization"`
	Registration        string `json:"registration"`
	Revocation          string `json:"revocation,omitempty"`
	Userinfo            string `json:"userinfo,omitempty"`
}

// oauthSecret is the sealed part of an OAuth credential.
type oauthSecret struct {
	ClientID           string         `json:"client_id,omitempty"`
	ClientSecret       string         `json:"client_secret,omitempty"`
	RegistrationIssuer string         `json:"registration_issuer,omitempty"`
	Endpoints          oauthEndpoints `json:"endpoints"`
	RefreshToken       string         `json:"refresh_token,omitempty"`
	AccessToken        string         `json:"access_token,omitempty"`
	ExpiresAt          time.Time      `json:"expires_at"`
	IssuedAt           time.Time      `json:"issued_at"`
	DeviceCode         string         `json:"device_code,omitempty"`
	DeviceExpiresAt    time.Time      `json:"device_expires_at"`
	Interval           int            `json:"interval,omitempty"`
	StateVersion       int64          `json:"state_version"`
}

// registered reports whether the secret holds a client registration that
// belongs to issuer.
func (s *oauthSecret) registered(issuer string) bool {
	return s.ClientID != "" && s.RegistrationIssuer == issuer
}

// clearGrant drops the tokens and any device code, keeping the
// registration and endpoints.
func (s *oauthSecret) clearGrant() {
	s.RefreshToken, s.AccessToken = "", ""
	s.ExpiresAt, s.IssuedAt = time.Time{}, time.Time{}
	s.DeviceCode, s.DeviceExpiresAt, s.Interval = "", time.Time{}, 0
}

// oauthBound is what an OAuth credential's secret is sealed together with:
// its usage, its config (hashed) and its URL restriction. The prefix differs
// from cryptoBound's and boundURLs', so no encoding collides.
func oauthBound(cfg OAuthConfig, allowed *[]string) []byte {
	raw, _ := json.Marshal(cfg) // a struct of strings: cannot fail
	sum := sha256.Sum256(raw)
	out := fmt.Appendf(nil, "oauth\x1e%s\x1e%x", UsageOAuth, sum)
	if b := boundURLs(allowed); b != nil {
		out = append(out, '\x1e')
		out = append(out, b...)
	}
	return out
}

// parseOAuthDef reads an OAuth definition. Any inconsistency is an error:
// the row is then unopenable, never half-used.
func parseOAuthDef(def json.RawMessage) (oauthDefinition, error) {
	var d oauthDefinition
	if err := json.Unmarshal(def, &d); err != nil {
		return d, errors.New("malformed OAuth definition")
	}
	if d.Usage != UsageOAuth {
		return d, errors.New("not an OAuth definition")
	}
	if d.AllowedURLs == nil {
		return d, errors.New("an OAuth definition without allowed_urls")
	}
	return d, nil
}

// validOAuthConfig checks a config before it is stored. Only https; only
// dynamic registration in this version.
func validOAuthConfig(cfg OAuthConfig) error {
	if err := validHTTPSURL("oauth.resource", cfg.Resource); err != nil {
		return err
	}
	if cfg.Issuer != "" {
		if err := validHTTPSURL("oauth.issuer", cfg.Issuer); err != nil {
			return err
		}
	}
	switch cfg.Client.Registration {
	case OAuthRegistrationDynamic:
	case OAuthRegistrationMetadataDocument, OAuthRegistrationStatic:
		return fmt.Errorf("oauth.client.registration %q is not supported yet; use dynamic", cfg.Client.Registration)
	default:
		return fmt.Errorf("oauth.client.registration must be dynamic (got %q)", cfg.Client.Registration)
	}
	switch cfg.Client.TokenEndpointAuth {
	case OAuthAuthNone, OAuthAuthClientSecretBasic, OAuthAuthClientSecretPost:
	default:
		return fmt.Errorf("oauth.client.token_endpoint_auth must be none, client_secret_basic or client_secret_post (got %q)", cfg.Client.TokenEndpointAuth)
	}
	if len(cfg.Scopes) > maxOAuthScopes {
		return fmt.Errorf("oauth.scopes: at most %d", maxOAuthScopes)
	}
	for _, s := range cfg.Scopes {
		if s == "" || len(s) > maxOAuthScopeLen || strings.IndexFunc(s, func(r rune) bool {
			// RFC 6749 §3.3 scope-token: printable ASCII but space, " and \.
			return r <= 0x20 || r >= 0x7f || r == '"' || r == '\\'
		}) >= 0 {
			return fmt.Errorf("oauth.scopes: %q is not a valid scope", s)
		}
	}
	return nil
}

// validHTTPSURL checks an absolute https URL with a host and no userinfo or
// fragment.
func validHTTPSURL(field, raw string) error {
	if raw == "" {
		return fmt.Errorf("%s is required", field)
	}
	if len(raw) > maxOAuthURLLen {
		return fmt.Errorf("%s is longer than %d bytes", field, maxOAuthURLLen)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return fmt.Errorf("%s must be an https URL with a host, no user info, query or fragment", field)
	}
	return nil
}

// oauthRow is a stored OAuth credential: the row as read (its Definition is
// the prev of the next conditional write), its parsed definition, and the
// identity its secret is sealed under.
type oauthRow struct {
	row credentialdef.Row
	def oauthDefinition
	id  Identity
}

// key names the row for the singleflight group and the poller map.
func (r oauthRow) key() string {
	return r.row.TenantID + "\x00" + r.row.Scope + "\x00" + r.row.ScopeID + "\x00" + r.row.Name
}

func (e *Engine) oauthRowOf(row credentialdef.Row) (oauthRow, error) {
	d, err := parseOAuthDef(row.Definition)
	if err != nil {
		return oauthRow{}, unopenable(row.Name, row.Scope, err)
	}
	return oauthRow{row: row, def: d, id: Identity{
		TenantID: row.TenantID, Scope: row.Scope, ScopeID: row.ScopeID, Name: row.Name,
		Bound: oauthBound(d.Config, d.AllowedURLs),
	}}, nil
}

// PutOAuth creates an OAuth credential, pending connect. Create-only: a
// credential of the name in this bucket is ErrKeyExists (change the config
// by deleting and creating it again). allowedURLs, already normalised, nil
// for the default: the resource alone.
func (e *Engine) PutOAuth(ctx context.Context, id Identity, cfg OAuthConfig, allowedURLs *[]string) (Meta, error) {
	if !e.sealer.Enabled() {
		return Meta{}, ErrNoKey
	}
	if err := validOAuthConfig(cfg); err != nil {
		return Meta{}, err
	}
	if allowedURLs == nil {
		def := []string{cfg.Resource}
		norm, err := NormalizeAllowedURLs(&def)
		if err != nil {
			return Meta{}, fmt.Errorf("oauth.resource: %w", err)
		}
		allowedURLs = norm
	}
	if *allowedURLs == nil {
		allowedURLs = &[]string{}
	}
	e.cryptoMu.Lock()
	defer e.cryptoMu.Unlock()
	if err := e.absent(ctx, id); err != nil {
		return Meta{}, err
	}
	raw, err := json.Marshal(oauthDefinition{
		Usage: UsageOAuth, Config: cfg, AllowedURLs: allowedURLs,
		Status:       OAuthStatus{State: OAuthPendingConnect},
		StateVersion: 1,
	})
	if err != nil {
		return Meta{}, err
	}
	row, err := e.store.CredentialDefPut(ctx, credentialdef.Row{
		TenantID: id.TenantID, Scope: id.Scope, ScopeID: id.ScopeID, Name: id.Name,
		Backend: BackendInline, Definition: raw,
	})
	if err != nil {
		return Meta{}, err
	}
	return e.metaOf(row), nil
}

// getOAuth reads the OAuth credential stored exactly under id (no
// precedence): what the owner's connect, status and disconnect act on.
func (e *Engine) getOAuth(ctx context.Context, id Identity) (oauthRow, error) {
	row, err := e.store.CredentialDefGet(ctx, id.TenantID, id.Scope, id.ScopeID, id.Name)
	if err != nil {
		return oauthRow{}, err
	}
	if defKind(row.Definition) != kindOAuth {
		return oauthRow{}, fmt.Errorf("%w (%q in %s scope)", ErrNotOAuth, id.Name, id.Scope)
	}
	return e.oauthRowOf(row)
}

// findOAuth finds the OAuth credential an $oauth:<name> resolves to for a
// run identity: find's precedence (agent > user > tenant) and expiry, then
// the kind (an ordinary credential or a crypto key of the name is refused,
// without falling through), then the URL restriction for targetURL and the
// pin, as resolve does for a $cred:. Nothing is opened.
func (e *Engine) findOAuth(ctx context.Context, tenantID, agentName, userID, name, targetURL string) (oauthRow, bool, error) {
	row, found, err := e.find(ctx, tenantID, agentName, userID, name)
	if err != nil || !found {
		return oauthRow{}, found, err
	}
	if defKind(row.Definition) != kindOAuth {
		return oauthRow{}, true, fmt.Errorf("%w (%q in %s scope; name it as $cred:<name>)", ErrNotOAuth, name, row.Scope)
	}
	r, err := e.oauthRowOf(row)
	if err != nil {
		return oauthRow{}, true, err
	}
	if !URLAllowed(r.def.AllowedURLs, targetURL) {
		return oauthRow{}, true, fmt.Errorf("%w (%q)", ErrURLNotAllowed, name)
	}
	if !netguard.Pin(ctx) {
		return oauthRow{}, true, fmt.Errorf("%w (%q)", ErrNotPinnable, name)
	}
	return r, true, nil
}

// openOAuthSecret opens r's secret. A row with none yet opens to the zero
// secret. Any failure is ErrUnopenable.
func (e *Engine) openOAuthSecret(r oauthRow) (oauthSecret, error) {
	if r.def.Secret == nil {
		return oauthSecret{}, nil
	}
	pt, err := e.sealer.Open(*r.def.Secret, r.id)
	if err != nil {
		return oauthSecret{}, unopenable(r.id.Name, r.id.Scope, err)
	}
	var s oauthSecret
	if err := json.Unmarshal(pt, &s); err != nil {
		return oauthSecret{}, unopenable(r.id.Name, r.id.Scope, errors.New("malformed secret"))
	}
	// An old ciphertext spliced under a newer header (an earlier refresh
	// token, a revoked grant) does not open.
	if s.StateVersion != r.def.StateVersion {
		return oauthSecret{}, unopenable(r.id.Name, r.id.Scope, ErrDecrypt)
	}
	return s, nil
}

// casOAuth writes def over r only while the stored definition is still the
// one r was read with. sec, when non-nil, is sealed as the new secret and
// the state_version advances; nil keeps the stored secret and version.
// Returns the row as written, and false when another writer got there first.
// *ErrNotFound (from the store) when the credential was deleted.
func (e *Engine) casOAuth(ctx context.Context, r oauthRow, def oauthDefinition, sec *oauthSecret) (oauthRow, bool, error) {
	def.Usage = UsageOAuth
	def.Config, def.AllowedURLs = r.def.Config, r.def.AllowedURLs
	if sec != nil {
		def.StateVersion = r.def.StateVersion + 1
		s := *sec
		s.StateVersion = def.StateVersion
		pt, err := json.Marshal(s)
		if err != nil {
			return oauthRow{}, false, err
		}
		sealed, err := e.sealer.Seal(pt, r.id)
		if err != nil {
			return oauthRow{}, false, err
		}
		def.Secret = &sealed
	} else {
		def.StateVersion, def.Secret = r.def.StateVersion, r.def.Secret
	}
	raw, err := json.Marshal(def)
	if err != nil {
		return oauthRow{}, false, err
	}
	next := r.row
	next.Definition = raw
	ok, err := e.store.CredentialDefPutIf(ctx, next, r.row.Definition)
	if err != nil || !ok {
		return oauthRow{}, false, err
	}
	// Re-read: the store may re-serialise the definition (Postgres jsonb),
	// and the next write's prev must be what it holds.
	got, err := e.store.CredentialDefGet(ctx, r.row.TenantID, r.row.Scope, r.row.ScopeID, r.row.Name)
	if err != nil {
		return oauthRow{}, false, err
	}
	nr, err := e.oauthRowOf(got)
	if err != nil {
		return oauthRow{}, false, err
	}
	return nr, true, nil
}

// HasOAuth reports the state of the OAuth credential name resolves to for
// a run identity (find's precedence), without opening anything: what a
// snapshot restore's scan asks. found is false when no credential of the
// name is held; a credential of the name that is not an OAuth one is
// (found, ErrNotOAuth).
func (e *Engine) HasOAuth(ctx context.Context, tenantID, agentName, userID, name string) (state string, found bool, err error) {
	row, found, err := e.find(ctx, tenantID, agentName, userID, name)
	if err != nil || !found {
		return "", found, err
	}
	if defKind(row.Definition) != kindOAuth {
		return "", true, fmt.Errorf("%w (%q in %s scope)", ErrNotOAuth, name, row.Scope)
	}
	d, err := parseOAuthDef(row.Definition)
	if err != nil {
		return "", true, unopenable(name, row.Scope, err)
	}
	return d.Status.State, true, nil
}

// oauthMeta fills m's OAuth fields from a stored definition, as the row says
// (unauthenticated, like Meta.AllowedURLs). A no-op for any other row.
func oauthMeta(m *Meta, def json.RawMessage) {
	if defKind(def) != kindOAuth {
		return
	}
	m.Usage = UsageOAuth
	d, err := parseOAuthDef(def)
	if err != nil {
		m.OAuth = &OAuthMeta{Status: OAuthStatus{State: "unreadable"}}
		return
	}
	m.OAuth = &OAuthMeta{Config: d.Config, Status: d.Status}
}

// oauthCredentialErr is ErrOAuthCredential naming the credential.
func oauthCredentialErr(name, scope string) error {
	return fmt.Errorf("%w (%q in %s scope)", ErrOAuthCredential, name, scope)
}

// cleanConnectedAs keeps a display name from an authorization server's
// userinfo to at most 256 printable characters.
func cleanConnectedAs(s string) string {
	s = strings.Map(func(r rune) rune {
		if !unicode.IsPrint(r) {
			return -1
		}
		return r
	}, s)
	if len(s) > 256 {
		s = s[:256]
	}
	return strings.TrimSpace(s)
}
