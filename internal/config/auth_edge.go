package config

import (
	"fmt"
	"net/netip"
	"regexp"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/credential"
)

// RFC FA P3: the edge gates.
//
//	auth:
//	  gates:
//	    cloudflare:
//	      kind: header
//	      preset: cloudflare-access         # | aws-alb | forward-auth-hmac
//	      team_domain: acme.cloudflareaccess.com
//	      audience: 9f3c…                   # the Access application's AUD tag
//	      trusted_proxy_cidrs: [172.18.0.0/16]
//	      identity_map: { tenant: acme, provision: known }
//	    ci:
//	      kind: awsiam
//	      server_id: loomcycle.acme.internal
//	      allowed_arns: ["arn:aws:iam::123456789012:role/ci-*"]
//	      identity_map: { tenant: acme, subject: arn, provision: auto }
//
// Both kinds verify an identity some other system authenticated, so both need
// an identity_map: the tenant and subject are never the request's to choose.

// Edge gate kinds and header presets.
const (
	AuthGateKindHeader = "header"
	AuthGateKindAWSIAM = "awsiam"

	HeaderPresetCloudflareAccess = "cloudflare-access"
	HeaderPresetAWSALB           = "aws-alb"
	HeaderPresetForwardAuthHMAC  = "forward-auth-hmac"
)

// AuthHeaderGate is a validated kind: header gate.
type AuthHeaderGate struct {
	Preset string
	// TrustedProxyCIDRs are the only peers whose assertion header is read.
	TrustedProxyCIDRs []netip.Prefix
	// cloudflare-access
	TeamDomain string
	Audience   string
	// aws-alb
	ALBARN    string
	ALBRegion string
	Issuer    string
	ClientID  string
	// forward-auth-hmac: the shared secret, by env-var name or as a stored
	// credential of the gate's tenant (never a literal).
	SecretEnv  string
	SecretCred string
}

// AuthAWSIAMGate is a validated kind: awsiam gate.
type AuthAWSIAMGate struct {
	ServerID    string
	AllowedARNs []string
	STSRegions  []string
}

var (
	edgeTeamDomainRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)
	edgeAudienceRE   = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,256}$`)
	edgeALBARNRE     = regexp.MustCompile(`^arn:aws(-[a-z]+)*:elasticloadbalancing:([a-z]{2}(-[a-z]+)+-[0-9]):[0-9]{12}:loadbalancer/app/[A-Za-z0-9-]{1,32}/[0-9a-f]{1,32}$`)
	edgeRegionRE     = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]$`)
	edgeServerIDRE   = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,256}$`)
	edgeARNPatternRE = regexp.MustCompile(`^arn:(aws|aws-cn|aws-us-gov):(iam|sts)::[0-9]{12}:[A-Za-z0-9+=,.@_/*-]{1,512}$`)
)

// hasEdgeOnlyKeys reports whether g sets a key only the edge gates take
// (preset, issuer and client_id are shared with kind: oidc).
func hasEdgeOnlyKeys(g AuthGateConfig) bool {
	return len(g.TrustedProxyCIDRs) > 0 || g.TeamDomain != "" || g.Audience != "" || g.ALBARN != "" ||
		g.SecretEnv != "" || g.Secret != "" || g.ServerID != "" || len(g.AllowedARNs) > 0 || len(g.STSRegions) > 0
}

// hasOIDCOnlyKeys reports whether g sets a key only kind: oidc takes.
func hasOIDCOnlyKeys(g AuthGateConfig) bool {
	return g.ClientSecretEnv != "" || g.ClientSecret != "" || g.DiscoveryURL != "" || len(g.AllowedDomains) > 0 ||
		len(g.AllowedTIDs) > 0 || len(g.Scopes) > 0 || g.EndSession || g.TokenAuth != ""
}

// headerPresetLabel is a header gate's login-page label when none is set: the
// login button reads "Continue as signed in by <label>".
func headerPresetLabel(preset, name string) string {
	switch preset {
	case HeaderPresetCloudflareAccess:
		return "Cloudflare Access"
	case HeaderPresetAWSALB:
		return "your AWS load balancer"
	}
	return name
}

// resolveEdgeGate validates one kind: header or kind: awsiam gate: its
// identity map (required) and its kind's own keys.
func resolveEdgeGate(c *Config, name, kind string, g AuthGateConfig, floor AuthNewUser) (*AuthIdentityMap, *AuthHeaderGate, *AuthAWSIAMGate, error) {
	if g.IdentityMap == nil {
		return nil, nil, nil, fmt.Errorf("auth.gates.%s: kind: %s needs an identity_map (at least tenant: <id>): the gate "+
			"verifies who someone is, never which tenant they belong to", name, kind)
	}
	if hasPasswordOnlyKeys(g) || len(g.AllowedReturnTo) > 0 {
		return nil, nil, nil, fmt.Errorf("auth.gates.%s: the password gate's keys (require_mfa, invites, resets, allowed_return_to, "+
			"password, new_user, manager_may_recover_owners) do not apply to kind: %s; use identity_map.new_user", name, kind)
	}
	idm, err := resolveIdentityMap(name, g.IdentityMap, floor, g.Trusted)
	if err != nil {
		return nil, nil, nil, err
	}
	if kind == AuthGateKindHeader {
		h, err := resolveHeaderGate(name, g)
		return idm, h, nil, err
	}
	a, err := resolveAWSIAMGate(c, name, g)
	return idm, nil, a, err
}

// resolveHeaderGate validates one kind: header gate.
func resolveHeaderGate(name string, g AuthGateConfig) (*AuthHeaderGate, error) {
	where := "auth.gates." + name
	h := &AuthHeaderGate{Preset: strings.TrimSpace(g.Preset)}
	if len(g.ServerID) > 0 || len(g.AllowedARNs) > 0 || len(g.STSRegions) > 0 {
		return nil, fmt.Errorf("%s: server_id, allowed_arns and sts_regions apply only to kind: awsiam", where)
	}
	if len(g.TrustedProxyCIDRs) == 0 {
		// Signed or not, an assertion header is read only from the proxy
		// that adds it: nothing else may reach the server with one.
		return nil, fmt.Errorf("%s: trusted_proxy_cidrs is required — the addresses of the proxy that adds the "+
			"assertion header (a tunnel container, the load balancer's subnets); a header from any other peer is refused", where)
	}
	for _, cidr := range g.TrustedProxyCIDRs {
		pfx, err := parsePrefixOrAddr(cidr)
		if err != nil {
			return nil, fmt.Errorf("%s.trusted_proxy_cidrs: %q is not a CIDR or an address", where, cidr)
		}
		if pfx.Bits() == 0 {
			return nil, fmt.Errorf("%s.trusted_proxy_cidrs: %q trusts every address", where, cidr)
		}
		h.TrustedProxyCIDRs = append(h.TrustedProxyCIDRs, pfx)
	}
	only := func(preset string, keys map[string]bool) error {
		for k, set := range keys {
			if set {
				return fmt.Errorf("%s: %s does not apply to preset %s", where, k, preset)
			}
		}
		return nil
	}
	switch h.Preset {
	case HeaderPresetCloudflareAccess:
		if err := only(h.Preset, map[string]bool{"alb_arn": g.ALBARN != "", "issuer": g.Issuer != "", "client_id": g.ClientID != "",
			"secret_env": g.SecretEnv != "", "secret": g.Secret != ""}); err != nil {
			return nil, err
		}
		h.TeamDomain, h.Audience = strings.ToLower(strings.TrimSpace(g.TeamDomain)), strings.TrimSpace(g.Audience)
		if !edgeTeamDomainRE.MatchString(h.TeamDomain) {
			return nil, fmt.Errorf("%s.team_domain: a host name like acme.cloudflareaccess.com is required (no scheme or path)", where)
		}
		if !edgeAudienceRE.MatchString(h.Audience) {
			return nil, fmt.Errorf("%s.audience: the Access application's AUD tag is required", where)
		}
	case HeaderPresetAWSALB:
		if err := only(h.Preset, map[string]bool{"team_domain": g.TeamDomain != "", "audience": g.Audience != "",
			"secret_env": g.SecretEnv != "", "secret": g.Secret != ""}); err != nil {
			return nil, err
		}
		h.ALBARN = strings.TrimSpace(g.ALBARN)
		m := edgeALBARNRE.FindStringSubmatch(h.ALBARN)
		if m == nil {
			// The signer check is the whole point (ALBeast): without the ARN
			// any ALB in the region could sign a header that verifies.
			return nil, fmt.Errorf("%s.alb_arn: the application load balancer's ARN is required "+
				"(arn:aws:elasticloadbalancing:<region>:<account>:loadbalancer/app/<name>/<id>)", where)
		}
		h.ALBRegion = m[2]
		h.Issuer, h.ClientID = strings.TrimSpace(g.Issuer), strings.TrimSpace(g.ClientID)
	case HeaderPresetForwardAuthHMAC:
		if err := only(h.Preset, map[string]bool{"team_domain": g.TeamDomain != "", "audience": g.Audience != "",
			"alb_arn": g.ALBARN != "", "issuer": g.Issuer != "", "client_id": g.ClientID != ""}); err != nil {
			return nil, err
		}
		env, sec := strings.TrimSpace(g.SecretEnv), strings.TrimSpace(g.Secret)
		switch {
		case env != "" && sec != "":
			return nil, fmt.Errorf("%s: set secret_env or secret, not both", where)
		case env != "":
			if !ExpandEnvAllowed(env) || envNameDenied(env) {
				return nil, fmt.Errorf("%s.secret_env: %q must be a LOOMCYCLE_*-prefixed (or allowlisted) name that is not one of "+
					"loomcycle's own infrastructure secrets or a principal's bearer", where, env)
			}
			h.SecretEnv = env
		case sec != "":
			ref, ok := credential.ParseRef(sec)
			if !ok {
				// Never quoted: a literal here is the secret.
				return nil, fmt.Errorf("%s.secret takes only a stored credential, written \"$cred:<name>\": a literal secret "+
					"would sit in the config file; use secret_env for an environment variable", where)
			}
			if g.IdentityMap == nil || strings.TrimSpace(g.IdentityMap.Tenant) == "" {
				// A stored credential belongs to a tenant; only a gate with
				// one fixed tenant names whose.
				return nil, fmt.Errorf("%s.secret: a $cred: secret is read from the gate's identity_map.tenant, so it needs a fixed tenant; "+
					"use secret_env with tenant_claim", where)
			}
			h.SecretCred = ref
		default:
			return nil, fmt.Errorf("%s: forward-auth-hmac needs the shared secret: secret_env: LOOMCYCLE_… or secret: \"$cred:<name>\"", where)
		}
	case "":
		return nil, fmt.Errorf("%s: preset is required (cloudflare-access, aws-alb or forward-auth-hmac)", where)
	default:
		return nil, fmt.Errorf("%s: unknown preset %q (want cloudflare-access, aws-alb or forward-auth-hmac)", where, g.Preset)
	}
	return h, nil
}

// resolveAWSIAMGate validates one kind: awsiam gate. server_id defaults to
// LOOMCYCLE_PUBLIC_URL's host: it only has to be a value no other service
// that accepts STS proofs uses.
func resolveAWSIAMGate(c *Config, name string, g AuthGateConfig) (*AuthAWSIAMGate, error) {
	where := "auth.gates." + name
	if g.Preset != "" || len(g.TrustedProxyCIDRs) > 0 || g.TeamDomain != "" || g.Audience != "" || g.ALBARN != "" ||
		g.Issuer != "" || g.ClientID != "" || g.SecretEnv != "" || g.Secret != "" {
		return nil, fmt.Errorf("%s: kind: awsiam takes server_id, allowed_arns, sts_regions and identity_map; "+
			"preset, trusted_proxy_cidrs and the header presets' keys apply only to kind: header", where)
	}
	a := &AuthAWSIAMGate{ServerID: strings.TrimSpace(g.ServerID)}
	if a.ServerID == "" {
		if host := publicURLHost(c.Env.PublicURL); host != "" {
			a.ServerID = host
		}
	}
	if !edgeServerIDRE.MatchString(a.ServerID) {
		return nil, fmt.Errorf("%s.server_id: required (or set LOOMCYCLE_PUBLIC_URL): the value clients sign into "+
			"X-Loomcycle-AWS-Server-ID, so a request signed for another service is refused here", where)
	}
	if len(g.AllowedARNs) == 0 {
		return nil, fmt.Errorf("%s.allowed_arns: at least one ARN pattern is required — STS answers for every AWS account", where)
	}
	for _, p := range g.AllowedARNs {
		p = strings.TrimSpace(p)
		if !edgeARNPatternRE.MatchString(p) {
			return nil, fmt.Errorf("%s.allowed_arns: %q must be an IAM ARN with a literal 12-digit account "+
				"(e.g. arn:aws:iam::123456789012:role/ci-*); * is allowed only after the account", where, p)
		}
		if strings.Contains(p, ":sts::") && strings.Contains(p, ":assumed-role/") {
			return nil, fmt.Errorf("%s.allowed_arns: %q: name the role (arn:aws:iam::<account>:role/<name>); "+
				"an assumed-role session is matched as its role", where, p)
		}
		a.AllowedARNs = append(a.AllowedARNs, p)
	}
	for _, r := range g.STSRegions {
		r = strings.TrimSpace(r)
		if !edgeRegionRE.MatchString(r) {
			return nil, fmt.Errorf("%s.sts_regions: %q is not an AWS region", where, r)
		}
		a.STSRegions = append(a.STSRegions, r)
	}
	return a, nil
}

func parsePrefixOrAddr(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if pfx, err := netip.ParsePrefix(s); err == nil {
		return netip.PrefixFrom(pfx.Addr().Unmap(), unmappedBits(pfx)).Masked(), nil
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	addr = addr.Unmap()
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

// unmappedBits keeps a v4-mapped prefix's length meaningful once unmapped.
func unmappedBits(p netip.Prefix) int {
	if p.Addr().Is4In6() {
		if b := p.Bits() - 96; b >= 0 {
			return b
		}
		return 0
	}
	return p.Bits()
}

func publicURLHost(u string) string {
	u = strings.TrimSpace(u)
	for _, scheme := range []string{"https://", "http://"} {
		if rest, ok := strings.CutPrefix(strings.ToLower(u), scheme); ok {
			host, _, _ := strings.Cut(rest, "/")
			return host
		}
	}
	return ""
}
