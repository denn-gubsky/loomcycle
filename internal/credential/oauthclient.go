package credential

// The OAuth client (RFC GB): every network exchange an OAuth credential
// makes with its authorization server, and nothing else of the feature does
// network I/O. Responses are read through a size cap and parsed into fixed
// shapes; an error quotes only the server's error code and a scrubbed,
// truncated error_description — never a response body, which could echo a
// token.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// maxOAuthResponse caps what is read of any authorization server response.
const maxOAuthResponse = 1 << 20

// Grant types.
const (
	grantDeviceCode   = "urn:ietf:params:oauth:grant-type:device_code"
	grantRefreshToken = "refresh_token"
	grantAuthCode     = "authorization_code"
)

// oauthClientName is what the runtime registers itself as.
const oauthClientName = "loomcycle"

// oauthError is an authorization server's OAuth error response: its code and
// description only.
type oauthError struct {
	Code string
	Desc string
}

func (e *oauthError) Error() string {
	if e.Desc != "" {
		return fmt.Sprintf("authorization server error %s: %s", e.Code, e.Desc)
	}
	return "authorization server error " + e.Code
}

// oauthTransientError is a failure worth trying again later: the server
// could not be reached, answered 5xx or 429. It says nothing of the body.
type oauthTransientError struct {
	What string
}

func (e *oauthTransientError) Error() string { return "authorization server unavailable: " + e.What }

// isOAuthCode reports whether err is an OAuth error response with code.
func isOAuthCode(err error, code string) bool {
	var oe *oauthError
	return errors.As(err, &oe) && oe.Code == code
}

// isTransient reports whether err is an oauthTransientError.
func isTransient(err error) bool {
	var te *oauthTransientError
	return errors.As(err, &te)
}

// oauthClient talks to authorization servers. Its HTTP client must follow
// no redirect (production: a netguard client whose CheckRedirect returns
// http.ErrUseLastResponse), so a token request is never re-sent elsewhere.
type oauthClient struct {
	http *http.Client
	// now stamps the id_token key-set cache and the id_token's time checks
	// (nil: time.Now).
	now func() time.Time

	jwksMu sync.Mutex
	jwks   map[string]*jwksEntry // by issuer and jwks_uri; see idtoken.go
}

// tokenResponse is a token endpoint's successful answer.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	// IDToken is an OpenID Connect id_token, read only for a display name
	// (idTokenName) and never kept.
	IDToken string `json:"id_token"`
}

// deviceResponse is a device authorization endpoint's answer.
type deviceResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int64  `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// sameHost reports whether two URLs name the same host (and port).
func sameHost(a, b string) bool {
	ua, err1 := url.Parse(a)
	ub, err2 := url.Parse(b)
	return err1 == nil && err2 == nil && ua.Host != "" && strings.EqualFold(ua.Host, ub.Host)
}

// checkEndpoint refuses an advertised endpoint that is not https or not on
// the issuer's or the resource's host: an authorization server's metadata
// cannot send the runtime's tokens to a third host.
func checkEndpoint(field, endpoint, issuer, resource string) error {
	// An endpoint may carry a query (RFC 8414 allows it); never user info
	// or a fragment.
	u, err := url.Parse(endpoint)
	if err != nil || len(endpoint) > maxOAuthURLLen || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("authorization server metadata: %s is not an https URL", field)
	}
	if !sameHost(endpoint, issuer) && !sameHost(endpoint, resource) {
		return fmt.Errorf("authorization server metadata: %s is not on the issuer's or the resource's host", field)
	}
	return nil
}

// wellKnown builds the RFC 8414 / RFC 9728 well-known URL for base: the
// suffix inserted between the host and base's path.
func wellKnown(base, suffix string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	path := strings.TrimSuffix(u.Path, "/")
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/.well-known/" + suffix + path}).String(), nil
}

// sameResource compares resource identifiers, ignoring one trailing slash.
func sameResource(a, b string) bool {
	return strings.TrimSuffix(a, "/") == strings.TrimSuffix(b, "/")
}

// discover finds cfg's authorization server and its endpoints: the protected
// resource metadata (RFC 9728) at the resource's well-known URL, falling
// back to the host root; the issuer it lists (the configured one, which must
// be listed, or the only one); then the issuer's metadata (RFC 8414), whose
// issuer must match and whose endpoints must be https on the issuer's or
// the resource's host.
func (c *oauthClient) discover(ctx context.Context, cfg OAuthConfig) (oauthEndpoints, error) {
	var prm struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
	}
	prmURL, err := wellKnown(cfg.Resource, "oauth-protected-resource")
	if err != nil {
		return oauthEndpoints{}, err
	}
	status, err := c.getJSON(ctx, prmURL, &prm)
	if err == nil && status == http.StatusNotFound {
		u, _ := url.Parse(cfg.Resource)
		status, err = c.getJSON(ctx, (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/.well-known/oauth-protected-resource"}).String(), &prm)
	}
	if err != nil {
		return oauthEndpoints{}, fmt.Errorf("protected resource metadata: %w", err)
	}
	if status != http.StatusOK {
		return oauthEndpoints{}, fmt.Errorf("protected resource metadata: HTTP %d", status)
	}
	if !sameResource(prm.Resource, cfg.Resource) {
		return oauthEndpoints{}, errors.New("protected resource metadata names a different resource than the one configured")
	}
	issuer := cfg.Issuer
	switch {
	case issuer != "":
		listed := false
		for _, as := range prm.AuthorizationServers {
			if as == issuer {
				listed = true
			}
		}
		if !listed {
			return oauthEndpoints{}, errors.New("the configured issuer is not one the resource lists")
		}
	case len(prm.AuthorizationServers) == 1:
		issuer = prm.AuthorizationServers[0]
	case len(prm.AuthorizationServers) == 0:
		return oauthEndpoints{}, errors.New("the resource lists no authorization server")
	default:
		return oauthEndpoints{}, errors.New("the resource lists more than one authorization server; set oauth.issuer")
	}
	if err := validHTTPSURL("issuer", issuer); err != nil {
		return oauthEndpoints{}, fmt.Errorf("protected resource metadata: %w", err)
	}
	var meta struct {
		Issuer                      string `json:"issuer"`
		TokenEndpoint               string `json:"token_endpoint"`
		DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
		RegistrationEndpoint        string `json:"registration_endpoint"`
		RevocationEndpoint          string `json:"revocation_endpoint"`
		UserinfoEndpoint            string `json:"userinfo_endpoint"`
		JWKSURI                     string `json:"jwks_uri"`
	}
	asURL, err := wellKnown(issuer, "oauth-authorization-server")
	if err != nil {
		return oauthEndpoints{}, err
	}
	status, err = c.getJSON(ctx, asURL, &meta)
	if err != nil {
		return oauthEndpoints{}, fmt.Errorf("authorization server metadata: %w", err)
	}
	if status != http.StatusOK {
		return oauthEndpoints{}, fmt.Errorf("authorization server metadata: HTTP %d", status)
	}
	if meta.Issuer != issuer {
		return oauthEndpoints{}, errors.New("authorization server metadata names a different issuer")
	}
	ep := oauthEndpoints{Issuer: issuer, Token: meta.TokenEndpoint, DeviceAuthorization: meta.DeviceAuthorizationEndpoint,
		Registration: meta.RegistrationEndpoint, Revocation: meta.RevocationEndpoint, Userinfo: meta.UserinfoEndpoint}
	for _, f := range []struct {
		name, value string
		required    bool
	}{
		{"token_endpoint", ep.Token, true},
		{"device_authorization_endpoint", ep.DeviceAuthorization, true},
		{"registration_endpoint", ep.Registration, true},
		{"revocation_endpoint", ep.Revocation, false},
		{"userinfo_endpoint", ep.Userinfo, false},
	} {
		if f.value == "" {
			if f.required {
				return oauthEndpoints{}, fmt.Errorf("authorization server metadata has no %s", f.name)
			}
			continue
		}
		if err := checkEndpoint(f.name, f.value, issuer, cfg.Resource); err != nil {
			return oauthEndpoints{}, err
		}
	}
	// The key set only ever names the account (connected_as): one the
	// endpoint rule refuses is dropped rather than failing the connect, and
	// its id_tokens then go unverified and unused.
	if meta.JWKSURI != "" {
		if err := checkEndpoint("jwks_uri", meta.JWKSURI, issuer, cfg.Resource); err != nil {
			slog.DebugContext(ctx, "oauth: jwks_uri refused; id_tokens will not name the account", "issuer", issuer, "err", err)
		} else {
			ep.JWKS = meta.JWKSURI
		}
	}
	return ep, nil
}

// loopbackRedirect is a loopback redirect URI on a random port. It is never
// listened on: some authorization servers refuse to register a client that
// asks for the device grant alone, so the runtime registers as a native
// client that could also use the code grant, and never does.
func loopbackRedirect() string {
	n, err := rand.Int(rand.Reader, big.NewInt(65535-1024))
	port := int64(49152)
	if err == nil {
		port = 1024 + n.Int64()
	}
	return fmt.Sprintf("http://127.0.0.1:%d/callback", port)
}

// register registers the runtime as a client (RFC 7591).
func (c *oauthClient) register(ctx context.Context, ep oauthEndpoints, cfg OAuthConfig) (clientID, clientSecret string, err error) {
	body := map[string]any{
		"client_name":                oauthClientName,
		"grant_types":                []string{grantDeviceCode, grantRefreshToken, grantAuthCode},
		"response_types":             []string{"code"},
		"redirect_uris":              []string{loopbackRedirect()},
		"token_endpoint_auth_method": cfg.Client.TokenEndpointAuth,
	}
	if len(cfg.Scopes) > 0 {
		body["scope"] = strings.Join(cfg.Scopes, " ")
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.Registration, strings.NewReader(string(raw)))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	var out struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := c.do(req, &out, nil); err != nil {
		return "", "", fmt.Errorf("client registration: %w", err)
	}
	if out.ClientID == "" {
		return "", "", errors.New("client registration: no client_id in the answer")
	}
	if cfg.Client.TokenEndpointAuth != OAuthAuthNone && out.ClientSecret == "" {
		return "", "", errors.New("client registration: no client_secret in the answer, which the configured token_endpoint_auth needs")
	}
	return out.ClientID, out.ClientSecret, nil
}

// clientForm adds the client's authentication to a token-style request.
func clientForm(req *http.Request, form url.Values, cfg OAuthConfig, sec oauthSecret) {
	switch cfg.Client.TokenEndpointAuth {
	case OAuthAuthClientSecretBasic:
		req.SetBasicAuth(url.QueryEscape(sec.ClientID), url.QueryEscape(sec.ClientSecret))
	case OAuthAuthClientSecretPost:
		form.Set("client_id", sec.ClientID)
		form.Set("client_secret", sec.ClientSecret)
	default:
		form.Set("client_id", sec.ClientID)
	}
}

// postForm posts form to endpoint with the client's authentication and
// decodes a 2xx JSON answer into out. scrub lists the secrets the request
// carries: an error description that echoes one has it removed.
func (c *oauthClient) postForm(ctx context.Context, endpoint string, form url.Values, cfg OAuthConfig, sec oauthSecret, out any, scrub []string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return err
	}
	clientForm(req, form, cfg, sec)
	req.Body = io.NopCloser(strings.NewReader(form.Encode()))
	req.ContentLength = int64(len(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	return c.do(req, out, append(scrub, sec.ClientSecret))
}

// deviceAuthorize starts a device authorization (RFC 8628 §3.1).
func (c *oauthClient) deviceAuthorize(ctx context.Context, cfg OAuthConfig, sec oauthSecret) (deviceResponse, error) {
	form := url.Values{"resource": {cfg.Resource}}
	if len(cfg.Scopes) > 0 {
		form.Set("scope", strings.Join(cfg.Scopes, " "))
	}
	var out deviceResponse
	if err := c.postForm(ctx, sec.Endpoints.DeviceAuthorization, form, cfg, sec, &out, nil); err != nil {
		return deviceResponse{}, fmt.Errorf("device authorization: %w", err)
	}
	if out.DeviceCode == "" || out.UserCode == "" || out.VerificationURI == "" || out.ExpiresIn <= 0 {
		return deviceResponse{}, errors.New("device authorization: an incomplete answer")
	}
	return out, nil
}

// tokenDevice polls the token endpoint for a device grant (RFC 8628 §3.4).
func (c *oauthClient) tokenDevice(ctx context.Context, cfg OAuthConfig, sec oauthSecret) (tokenResponse, error) {
	form := url.Values{"grant_type": {grantDeviceCode}, "device_code": {sec.DeviceCode}, "resource": {cfg.Resource}}
	var out tokenResponse
	if err := c.postForm(ctx, sec.Endpoints.Token, form, cfg, sec, &out, []string{sec.DeviceCode}); err != nil {
		return tokenResponse{}, err
	}
	return out, checkTokenResponse(out)
}

// tokenRefresh exchanges the refresh token (RFC 6749 §6), naming the
// resource (RFC 8707): some authorization servers refuse a refresh without
// it.
func (c *oauthClient) tokenRefresh(ctx context.Context, cfg OAuthConfig, sec oauthSecret) (tokenResponse, error) {
	form := url.Values{"grant_type": {grantRefreshToken}, "refresh_token": {sec.RefreshToken}, "resource": {cfg.Resource}}
	var out tokenResponse
	if err := c.postForm(ctx, sec.Endpoints.Token, form, cfg, sec, &out, []string{sec.RefreshToken, sec.AccessToken}); err != nil {
		return tokenResponse{}, err
	}
	return out, checkTokenResponse(out)
}

// checkTokenResponse refuses a token answer the runtime cannot keep.
func checkTokenResponse(t tokenResponse) error {
	if t.AccessToken == "" {
		return errors.New("token endpoint: no access_token in the answer")
	}
	if t.TokenType != "" && !strings.EqualFold(t.TokenType, "bearer") {
		return errors.New("token endpoint: a token type other than Bearer")
	}
	return nil
}

// revoke revokes the refresh token (RFC 7009). Best effort: the caller
// clears its state whatever this answers.
func (c *oauthClient) revoke(ctx context.Context, cfg OAuthConfig, sec oauthSecret) error {
	if sec.Endpoints.Revocation == "" || sec.RefreshToken == "" {
		return nil
	}
	form := url.Values{"token": {sec.RefreshToken}, "token_type_hint": {"refresh_token"}}
	return c.postForm(ctx, sec.Endpoints.Revocation, form, cfg, sec, nil, []string{sec.RefreshToken})
}

// userinfo returns a display name for the account that connected: from the
// authorization server's userinfo endpoint, only when it advertised one and
// the grant includes openid. The access token is never decoded for it.
// "" when there is none; the caller then tries a verified id_token
// (idTokenName).
func (c *oauthClient) userinfo(ctx context.Context, sec oauthSecret, grantedScope string) string {
	if sec.Endpoints.Userinfo == "" || !hasScope(grantedScope, "openid") {
		return ""
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sec.Endpoints.Userinfo, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+sec.AccessToken)
	req.Header.Set("Accept", "application/json")
	var out struct {
		Email             string `json:"email"`
		PreferredUsername string `json:"preferred_username"`
		Name              string `json:"name"`
		Sub               string `json:"sub"`
	}
	if err := c.do(req, &out, []string{sec.AccessToken}); err != nil {
		return ""
	}
	for _, v := range []string{out.Email, out.PreferredUsername, out.Name, out.Sub} {
		if v = cleanConnectedAs(v); v != "" && v != sec.AccessToken {
			return v
		}
	}
	return ""
}

func hasScope(granted, want string) bool {
	for _, s := range strings.Fields(granted) {
		if s == want {
			return true
		}
	}
	return false
}

// getJSON GETs u and decodes a 200 answer into out; any other status is
// returned without reading the body.
func (c *oauthClient) getJSON(ctx context.Context, u string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, &oauthTransientError{What: "the request failed"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxOAuthResponse))
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			return resp.StatusCode, &oauthTransientError{What: fmt.Sprintf("HTTP %d", resp.StatusCode)}
		}
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxOAuthResponse)).Decode(out); err != nil {
		return resp.StatusCode, errors.New("the answer is not the expected JSON")
	}
	return resp.StatusCode, nil
}

// do sends req and decodes a 2xx answer into out (nil: ignore the body).
// A 4xx with an OAuth error body is an *oauthError; 5xx, 429 and a failed
// send are *oauthTransientError; anything else an error naming the status.
// No error ever carries the body.
func (c *oauthClient) do(req *http.Request, out any, scrub []string) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return &oauthTransientError{What: "the request failed"}
	}
	defer resp.Body.Close()
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, maxOAuthResponse))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		if rerr != nil {
			return &oauthTransientError{What: "the answer could not be read"}
		}
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(body, out); err != nil {
			return errors.New("the answer is not the expected JSON")
		}
		return nil
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		return &oauthTransientError{What: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return fmt.Errorf("HTTP %d: redirects are not followed", resp.StatusCode)
	}
	var oe struct {
		Error string `json:"error"`
		Desc  string `json:"error_description"`
	}
	if json.Unmarshal(body, &oe) == nil && validErrorCode(oe.Error) {
		return &oauthError{Code: oe.Error, Desc: scrubDescription(oe.Desc, scrub)}
	}
	return fmt.Errorf("HTTP %d", resp.StatusCode)
}

// validErrorCode accepts an OAuth error code as RFC 6749 defines its
// charset, bounded.
func validErrorCode(code string) bool {
	if code == "" || len(code) > 64 {
		return false
	}
	for _, r := range code {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

// maxErrorDescription bounds what is kept of an error_description.
const maxErrorDescription = 200

// scrubDescription keeps an error_description short, printable ASCII, and
// free of any secret the request carried.
func scrubDescription(desc string, scrub []string) string {
	for _, s := range scrub {
		if s != "" {
			desc = strings.ReplaceAll(desc, s, "[redacted]")
		}
	}
	desc = strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return -1
		}
		return r
	}, desc)
	if len(desc) > maxErrorDescription {
		desc = desc[:maxErrorDescription]
	}
	return desc
}

// deviceDeadline is when a device authorization answered at now expires.
func deviceDeadline(now time.Time, d deviceResponse) time.Time {
	return now.Add(time.Duration(d.ExpiresIn) * time.Second)
}
