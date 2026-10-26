// Package oauth is RFC FA's OAuthGate: sign-in through an OAuth 2.0 provider
// that is not an OpenID Provider (GitHub, GitLab, Gitea, or any provider with
// a userinfo endpoint). The authorization-code + PKCE flow is the shared
// codeflow machinery; what this package adds is the part OIDC gets from a
// signed ID token: after the code exchange it asks the provider's API who the
// person is, and turns the answer into an authgate.Assertion.
//
// Two rules make that answer safe to map to a loomcycle user:
//
//   - an e-mail address is reported only when the provider says it is
//     verified (GitHub: the primary AND verified entry of /user/emails), so an
//     address someone typed into their profile never reaches the identity map;
//   - an organisation, team or group becomes a group only when the gate is
//     configured to trust it (allowed_orgs). A gate with no allowlist asserts
//     no groups at all, so joining an arbitrary GitHub organisation never maps
//     to a loomcycle group.
//
// The subject is the provider's immutable numeric id, not the login name,
// which its owner can rename (and someone else can then claim).
package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/denn-gubsky/loomcycle/internal/authgate"
	"github.com/denn-gubsky/loomcycle/internal/authgate/codeflow"
)

// Kind is the gate kind in `auth.gates.<name>.kind`.
const Kind = "oauth"

// Presets.
const (
	PresetGitHub  = "github"
	PresetGitLab  = "gitlab"
	PresetGitea   = "gitea"
	PresetGeneric = "generic"
)

// AMROAuth is the method an OAuth login records. RFC 8176 has none for a
// delegated provider login; "oauth" says where the person was verified.
const AMROAuth = "oauth"

// Bounds on what the provider's API may send back.
const (
	maxResponseBytes = 1 << 20
	pageSize         = 100
	maxPages         = 10
	maxGroups        = 200
)

// ClaimMap names the userinfo fields of a generic provider.
type ClaimMap struct {
	Subject       string
	Email         string
	EmailVerified string
	Name          string
	Login         string
	Groups        string
}

// Config is one OAuth gate.
type Config struct {
	// Name is the configured gate name.
	Name   string
	Preset string
	// ClientID / ClientSecret are the gate's confidential client.
	ClientID     string
	ClientSecret string
	// BaseURL is the provider's web origin (GitHub Enterprise, a self-hosted
	// GitLab, a Gitea). Empty for github.com and gitlab.com.
	BaseURL string
	// The generic preset's endpoints (ignored by the others).
	AuthURL     string
	TokenURL    string
	UserinfoURL string
	Claims      ClaimMap
	// Scopes replaces the preset's scopes when set.
	Scopes []string
	// AllowedOrgs are the organisations (GitHub, Gitea), top-level groups
	// (GitLab) or group values (generic) whose membership the gate turns
	// into groups. Matched case-insensitively. Nothing else is asserted.
	AllowedOrgs []string
	// RequireOrg refuses a login whose person is in none of AllowedOrgs.
	RequireOrg bool
	// TrustEmail lets the gate assert the provider's verified e-mail at all.
	// Off, no e-mail is asserted: whether a self-managed instance or a
	// generic provider really confirms addresses is that instance's
	// configuration, which loomcycle cannot see.
	TrustEmail bool
	// AuthStyle is how the client authenticates at the token endpoint; zero
	// detects it.
	AuthStyle oauth2.AuthStyle
	// HTTP is used for the token exchange and the API calls (bounded
	// timeout); nil = a client with a 15 s timeout.
	HTTP *http.Client
}

// Gate is an OAuthGate. It satisfies authgate.Gate.
type Gate struct {
	cfg      Config
	client   *codeflow.Client
	provider provider
	allowed  map[string]bool
	now      func() time.Time
}

// provider is a preset's endpoints and how it reads a profile.
type provider struct {
	authURL, tokenURL string
	scopes            []string
	issuer            string
	profile           func(ctx context.Context, g *Gate, token string) (profile, error)
}

// profile is what a provider says about the signed-in person.
type profile struct {
	id, login, name string
	// email is set only when the provider reports it verified.
	email string
	// groups are the person's org / org/team / group names, unfiltered.
	groups []string
}

// New builds an OAuth gate from a validated config.
func New(cfg Config) (*Gate, error) {
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, errors.New("oauth: client_id and client_secret are required")
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 15 * time.Second}
	}
	p, err := presetProvider(cfg)
	if err != nil {
		return nil, err
	}
	if len(cfg.Scopes) > 0 {
		p.scopes = cfg.Scopes
	}
	if cfg.RequireOrg && len(cfg.AllowedOrgs) == 0 {
		return nil, errors.New("oauth: require_org needs allowed_orgs")
	}
	allowed := map[string]bool{}
	for _, o := range cfg.AllowedOrgs {
		if o = strings.ToLower(strings.TrimSpace(o)); o != "" {
			allowed[o] = true
		}
	}
	return &Gate{
		cfg: cfg, provider: p, allowed: allowed, now: time.Now,
		client: &codeflow.Client{
			ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret,
			AuthURL: p.authURL, TokenURL: p.tokenURL, Scopes: p.scopes, AuthStyle: cfg.AuthStyle, HTTP: cfg.HTTP,
		},
	}, nil
}

// webBase validates a configured provider origin: https, or http on loopback
// (a test or a provider on the same host), with no path, query or user info.
func webBase(raw, def string) (string, error) {
	if raw == "" {
		return def, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("oauth: base_url %q must be an absolute URL with no user info, query or fragment", raw)
	}
	loopback := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1"
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return "", fmt.Errorf("oauth: base_url %q must be https (http only on loopback)", raw)
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host+u.EscapedPath(), "/"), nil
}

func presetProvider(cfg Config) (provider, error) {
	switch cfg.Preset {
	case PresetGitHub:
		base, err := webBase(cfg.BaseURL, "https://github.com")
		if err != nil {
			return provider{}, err
		}
		api := "https://api.github.com"
		if base != "https://github.com" {
			api = base + "/api/v3" // GitHub Enterprise Server
		}
		return provider{
			authURL: base + "/login/oauth/authorize", tokenURL: base + "/login/oauth/access_token",
			scopes: []string{"read:user", "user:email", "read:org"}, issuer: base,
			profile: func(ctx context.Context, g *Gate, tok string) (profile, error) { return g.githubProfile(ctx, api, tok) },
		}, nil
	case PresetGitLab:
		base, err := webBase(cfg.BaseURL, "https://gitlab.com")
		if err != nil {
			return provider{}, err
		}
		scopes := []string{"read_user"}
		if len(cfg.AllowedOrgs) > 0 {
			// Group membership needs read_api; asked for only when groups
			// are wanted.
			scopes = append(scopes, "read_api")
		}
		return provider{
			authURL: base + "/oauth/authorize", tokenURL: base + "/oauth/token", scopes: scopes, issuer: base,
			profile: func(ctx context.Context, g *Gate, tok string) (profile, error) {
				return g.gitlabProfile(ctx, base, tok)
			},
		}, nil
	case PresetGitea:
		if cfg.BaseURL == "" {
			return provider{}, errors.New("oauth: the gitea preset needs base_url")
		}
		base, err := webBase(cfg.BaseURL, "")
		if err != nil {
			return provider{}, err
		}
		return provider{
			authURL: base + "/login/oauth/authorize", tokenURL: base + "/login/oauth/access_token",
			scopes: []string{"read:user", "read:organization"}, issuer: base,
			profile: func(ctx context.Context, g *Gate, tok string) (profile, error) { return g.giteaProfile(ctx, base, tok) },
		}, nil
	case PresetGeneric:
		for _, u := range []string{cfg.AuthURL, cfg.TokenURL, cfg.UserinfoURL} {
			if _, err := webBase(originOf(u), ""); err != nil || u == "" {
				return provider{}, fmt.Errorf("oauth: the generic preset needs https auth_url, token_url and userinfo_url (got %q)", u)
			}
		}
		issuer := originOf(cfg.AuthURL)
		return provider{
			authURL: cfg.AuthURL, tokenURL: cfg.TokenURL, scopes: nil, issuer: issuer,
			profile: func(ctx context.Context, g *Gate, tok string) (profile, error) { return g.genericProfile(ctx, tok) },
		}, nil
	}
	return provider{}, fmt.Errorf("oauth: unknown preset %q (want github, gitlab, gitea or generic)", cfg.Preset)
}

func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return u.Scheme + "://" + u.Host
}

// Kind implements authgate.Gate.
func (*Gate) Kind() string { return Kind }

// Issuer is the provider's origin: with the gate and the provider's user id it
// keys the person's user_identities row.
func (g *Gate) Issuer() string { return g.provider.issuer }

// ErrReauthUnsupported: an OAuth provider cannot be made to authenticate the
// person afresh and say so (there is no auth_time or max_age), so a login that
// must prove a fresh authentication — an account link — is refused.
var ErrReauthUnsupported = errors.New("oauth: this provider cannot prove a fresh sign-in")

// AuthURL is the provider URL a login redirects the browser to.
func (g *Gate) AuthURL(_ context.Context, ls authgate.LoginState) (string, error) {
	if ls.Reauth {
		return "", ErrReauthUnsupported
	}
	return g.client.AuthCodeURL(ls, false, nil)
}

// EndSessionURL: OAuth providers have no RP-initiated logout; signing out of
// loomcycle ends only loomcycle's session.
func (*Gate) EndSessionURL(context.Context, string) string { return "" }

// Begin redirects the browser to the provider (authgate.Gate).
func (g *Gate) Begin(ctx context.Context, w http.ResponseWriter, r *http.Request, ls authgate.LoginState) error {
	u, err := g.AuthURL(ctx, ls)
	if err != nil {
		return err
	}
	http.Redirect(w, r, u, http.StatusFound)
	return nil
}

// Complete finishes the login at the callback (authgate.Gate): it checks the
// state in constant time, redeems the code with the login's PKCE verifier,
// reads the profile and builds the assertion.
func (g *Gate) Complete(ctx context.Context, r *http.Request, ls authgate.LoginState) (authgate.Assertion, error) {
	if ls.Reauth {
		return authgate.Assertion{}, ErrReauthUnsupported
	}
	code, err := codeflow.CheckCallback(r, ls)
	if err != nil {
		return authgate.Assertion{}, err
	}
	tok, err := g.client.Exchange(ctx, code, ls)
	if err != nil {
		return authgate.Assertion{}, fmt.Errorf("%w: code exchange failed", authgate.ErrInvalidCredential)
	}
	if tok.AccessToken == "" {
		return authgate.Assertion{}, fmt.Errorf("%w: the provider issued no access token", authgate.ErrInvalidCredential)
	}
	p, err := g.provider.profile(ctx, g, tok.AccessToken)
	if err != nil {
		return authgate.Assertion{}, err
	}
	return g.assertion(p)
}

// Verify: an OAuth gate authenticates only interactively.
func (*Gate) Verify(context.Context, *http.Request) (authgate.Assertion, error) {
	return authgate.Assertion{}, authgate.ErrNoCredential
}

// assertion turns a profile into the gate's assertion, keeping only the groups
// the gate trusts.
func (g *Gate) assertion(p profile) (authgate.Assertion, error) {
	if p.id == "" {
		return authgate.Assertion{}, fmt.Errorf("%w: the provider named no user id", authgate.ErrInvalidCredential)
	}
	if !g.cfg.TrustEmail {
		p.email = ""
	}
	groups := g.trustedGroups(p.groups)
	if g.cfg.RequireOrg && len(groups) == 0 {
		return authgate.Assertion{}, fmt.Errorf("%w: not a member of an allowed organisation", authgate.ErrInvalidCredential)
	}
	now := g.now()
	// Only what the user cannot rename into someone else is a claim an
	// identity map may read: the immutable id, a verified e-mail and the
	// gate's own org-filtered groups. The login and name are display only.
	claims := map[string]any{"sub": p.id, "groups": stringsAny(groups)}
	if p.email != "" {
		claims["email"] = p.email
		claims["email_verified"] = true
	}
	return authgate.Assertion{
		Subject: p.id, Issuer: g.provider.issuer, AMR: []string{AMROAuth}, AuthTime: now,
		Email: p.email, EmailVerified: p.email != "", DisplayName: firstNonEmpty(p.name, p.login),
		Groups: groups, Claims: claims, ExpiresAt: now.Add(5 * time.Minute),
	}, nil
}

// trustedGroups keeps the groups under an allowed organisation: the org
// itself and "org/team" (GitHub, Gitea), or a GitLab group path under an
// allowed top-level group. No allowlist: no groups.
func (g *Gate) trustedGroups(in []string) []string {
	if len(g.allowed) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, grp := range in {
		grp = strings.TrimSpace(grp)
		if grp == "" || len(grp) > 255 {
			continue
		}
		top, _, _ := strings.Cut(strings.ToLower(grp), "/")
		if !g.allowed[top] || seen[strings.ToLower(grp)] {
			continue
		}
		seen[strings.ToLower(grp)] = true
		out = append(out, grp)
		if len(out) >= maxGroups {
			break
		}
	}
	sort.Strings(out)
	return out
}

// ---- provider API calls ------------------------------------------------------

// getJSON GETs u with the access token and decodes a bounded JSON answer.
func (g *Gate) getJSON(ctx context.Context, u, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := g.cfg.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%w: the provider API could not be reached", authgate.ErrInvalidCredential)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || len(body) > maxResponseBytes {
		return fmt.Errorf("%w: the provider API answer is unreadable or too large", authgate.ErrInvalidCredential)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: the provider API answered %d", authgate.ErrInvalidCredential, resp.StatusCode)
	}
	// UseNumber: a numeric id decoded into an interface (the generic
	// userinfo) stays exact instead of rounding through float64, where two
	// ids beyond 2^53 would collide on one subject.
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%w: the provider API answer is not the expected JSON", authgate.ErrInvalidCredential)
	}
	return nil
}

// getPages follows ?per_page=100&page=N until a short page, at most maxPages.
func getPages[T any](ctx context.Context, g *Gate, base, token string) ([]T, error) {
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	var all []T
	for page := 1; page <= maxPages; page++ {
		var items []T
		if err := g.getJSON(ctx, fmt.Sprintf("%s%sper_page=%d&page=%d", base, sep, pageSize, page), token, &items); err != nil {
			return nil, err
		}
		all = append(all, items...)
		if len(items) < pageSize {
			break
		}
	}
	return all, nil
}

func (g *Gate) githubProfile(ctx context.Context, api, tok string) (profile, error) {
	var u struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
	}
	if err := g.getJSON(ctx, api+"/user", tok, &u); err != nil {
		return profile{}, err
	}
	p := profile{login: u.Login, name: u.Name}
	if u.ID > 0 {
		p.id = strconv.FormatInt(u.ID, 10)
	}
	// /user's `email` is the public profile address, which its owner types in
	// and nobody verifies; only the verified primary from /user/emails counts.
	type email struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	emails, err := getPages[email](ctx, g, api+"/user/emails", tok)
	if err != nil {
		return profile{}, err
	}
	for _, e := range emails {
		if e.Primary && e.Verified {
			p.email = e.Email
		}
	}
	if len(g.allowed) == 0 {
		return p, nil
	}
	type orgMembership struct {
		State        string `json:"state"`
		Organization struct {
			Login string `json:"login"`
		} `json:"organization"`
	}
	orgs, err := getPages[orgMembership](ctx, g, api+"/user/memberships/orgs?state=active", tok)
	if err != nil {
		return profile{}, err
	}
	for _, m := range orgs {
		if m.State == "active" && m.Organization.Login != "" {
			p.groups = append(p.groups, m.Organization.Login)
		}
	}
	type team struct {
		Slug         string `json:"slug"`
		Organization struct {
			Login string `json:"login"`
		} `json:"organization"`
	}
	teams, err := getPages[team](ctx, g, api+"/user/teams", tok)
	if err != nil {
		return profile{}, err
	}
	for _, t := range teams {
		if t.Slug != "" && t.Organization.Login != "" {
			p.groups = append(p.groups, t.Organization.Login+"/"+t.Slug)
		}
	}
	return p, nil
}

func (g *Gate) gitlabProfile(ctx context.Context, base, tok string) (profile, error) {
	var u struct {
		ID          int64   `json:"id"`
		Username    string  `json:"username"`
		Name        string  `json:"name"`
		Email       string  `json:"email"`
		ConfirmedAt *string `json:"confirmed_at"`
		State       string  `json:"state"`
	}
	if err := g.getJSON(ctx, base+"/api/v4/user", tok, &u); err != nil {
		return profile{}, err
	}
	if u.State != "" && u.State != "active" {
		return profile{}, fmt.Errorf("%w: the provider account is %s", authgate.ErrInvalidCredential, u.State)
	}
	p := profile{login: u.Username, name: u.Name}
	if u.ID > 0 {
		p.id = strconv.FormatInt(u.ID, 10)
	}
	// GitLab's `email` is the primary address. The user's confirmed_at says
	// the ACCOUNT was confirmed once, not that the current primary was:
	// only /user/emails listing that address with its own confirmed_at does.
	if u.ConfirmedAt != nil && *u.ConfirmedAt != "" && u.Email != "" {
		type email struct {
			Email       string  `json:"email"`
			ConfirmedAt *string `json:"confirmed_at"`
		}
		emails, err := getPages[email](ctx, g, base+"/api/v4/user/emails", tok)
		if err != nil {
			return profile{}, err
		}
		for _, e := range emails {
			if strings.EqualFold(e.Email, u.Email) && e.ConfirmedAt != nil && *e.ConfirmedAt != "" {
				p.email = u.Email
			}
		}
	}
	if len(g.allowed) == 0 {
		return p, nil
	}
	type group struct {
		FullPath string `json:"full_path"`
	}
	groups, err := getPages[group](ctx, g, base+"/api/v4/groups?min_access_level=10", tok)
	if err != nil {
		return profile{}, err
	}
	for _, gr := range groups {
		p.groups = append(p.groups, gr.FullPath)
	}
	return p, nil
}

func (g *Gate) giteaProfile(ctx context.Context, base, tok string) (profile, error) {
	var u struct {
		ID       int64  `json:"id"`
		Login    string `json:"login"`
		FullName string `json:"full_name"`
	}
	if err := g.getJSON(ctx, base+"/api/v1/user", tok, &u); err != nil {
		return profile{}, err
	}
	p := profile{login: u.Login, name: u.FullName}
	if u.ID > 0 {
		p.id = strconv.FormatInt(u.ID, 10)
	}
	type email struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	emails, err := getPages[email](ctx, g, base+"/api/v1/user/emails", tok)
	if err != nil {
		return profile{}, err
	}
	for _, e := range emails {
		if e.Primary && e.Verified {
			p.email = e.Email
		}
	}
	if len(g.allowed) == 0 {
		return p, nil
	}
	type org struct {
		Username string `json:"username"`
	}
	orgs, err := getPages[org](ctx, g, base+"/api/v1/user/orgs", tok)
	if err != nil {
		return profile{}, err
	}
	for _, o := range orgs {
		p.groups = append(p.groups, o.Username)
	}
	type team struct {
		Name         string `json:"name"`
		Organization struct {
			Username string `json:"username"`
		} `json:"organization"`
	}
	teams, err := getPages[team](ctx, g, base+"/api/v1/user/teams", tok)
	if err != nil {
		return profile{}, err
	}
	for _, t := range teams {
		if t.Name != "" && t.Organization.Username != "" {
			p.groups = append(p.groups, t.Organization.Username+"/"+t.Name)
		}
	}
	return p, nil
}

func (g *Gate) genericProfile(ctx context.Context, tok string) (profile, error) {
	var info map[string]any
	if err := g.getJSON(ctx, g.cfg.UserinfoURL, tok, &info); err != nil {
		return profile{}, err
	}
	c := g.cfg.Claims
	p := profile{
		id:    claimString(info, firstNonEmpty(c.Subject, "sub")),
		login: claimString(info, firstNonEmpty(c.Login, "preferred_username")),
		name:  claimString(info, firstNonEmpty(c.Name, "name")),
	}
	// An address counts only when the provider says, as a JSON true, that it
	// is verified. No such claim: unverified.
	if v, ok := info[firstNonEmpty(c.EmailVerified, "email_verified")].(bool); ok && v {
		p.email = claimString(info, firstNonEmpty(c.Email, "email"))
	}
	if c.Groups != "" {
		if list, ok := info[c.Groups].([]any); ok {
			for _, v := range list {
				if s, ok := v.(string); ok {
					p.groups = append(p.groups, s)
				}
			}
		}
	}
	return p, nil
}

// claimString reads a string claim, or an integral number claim within int64,
// exactly. A fraction, an exponent or an out-of-range number is no id.
func claimString(m map[string]any, key string) string {
	switch v := m[key].(type) {
	case string:
		return v
	case json.Number:
		if n, err := strconv.ParseInt(v.String(), 10, 64); err == nil {
			return strconv.FormatInt(n, 10)
		}
	}
	return ""
}

func firstNonEmpty(a ...string) string {
	for _, s := range a {
		if s != "" {
			return s
		}
	}
	return ""
}

func stringsAny(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}
