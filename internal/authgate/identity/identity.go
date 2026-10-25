// Package identity turns a gate's verified assertion into a loomcycle
// identity (RFC FA §3.3, §3.7): the tenant the identity map allows, the
// subject "<gate>:<value>", the provider groups it asserts, and the plan that
// reconciles those groups with the user's memberships. It is pure: the HTTP
// server's authgate_identity.go reads and writes the store around it, for
// every gate kind alike.
package identity

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"unicode"

	"github.com/denn-gubsky/loomcycle/internal/authgate"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Bounds on what an assertion may carry into the store.
const (
	MaxExternalSubject = 255
	MaxGroups          = 256
	MaxGroupClaim      = 128
	MaxDisplayName     = 128
)

var (
	// ErrTenantRefused: the identity map gives this identity no tenant (no
	// claim, or a value not in allowed_tenants). Never falls back to "".
	ErrTenantRefused = errors.New("identity: the identity maps to no allowed tenant")
	// ErrSubjectInvalid: the subject claim is missing or unusable.
	ErrSubjectInvalid = errors.New("identity: the identity has no usable subject")
)

// Target is a loomcycle (tenant, subject).
type Target struct {
	TenantID string
	Subject  string
}

// claimString reads a claim as a string; a JSON number reads as its literal.
func claimString(a authgate.Assertion, name string) (string, bool) {
	switch v := a.Claims[name].(type) {
	case string:
		return v, true
	case json.Number:
		return v.String(), true
	}
	return "", false
}

// cleanValue accepts an identifier from a provider: non-empty, bounded, no
// whitespace or control characters (it becomes part of a subject or a key).
func cleanValue(v string, max int) (string, bool) {
	if v == "" || len(v) > max {
		return "", false
	}
	for _, r := range v {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == unicode.ReplacementChar {
			return "", false
		}
	}
	return v, true
}

// ExternalSubject is the assertion's stable key at the gate: its Subject.
func ExternalSubject(a authgate.Assertion) (string, error) {
	v, ok := cleanValue(a.Subject, MaxExternalSubject)
	if !ok {
		return "", ErrSubjectInvalid
	}
	return v, nil
}

// Tenant resolves the identity's tenant.
func Tenant(m *config.AuthIdentityMap, a authgate.Assertion) (string, error) {
	if m == nil {
		return "", ErrTenantRefused
	}
	if m.Tenant != "" {
		return m.Tenant, nil
	}
	var v string
	if m.TenantClaim == config.EmailDomainClaim {
		// Only a verified address says anything about who controls the
		// domain; an unverified one is whatever the person typed.
		at := strings.LastIndexByte(a.Email, '@')
		if !a.EmailVerified || at < 1 || at == len(a.Email)-1 {
			return "", ErrTenantRefused
		}
		v = strings.ToLower(a.Email[at+1:])
	} else {
		s, ok := claimString(a, m.TenantClaim)
		if !ok {
			return "", ErrTenantRefused
		}
		v = s
	}
	t, ok := m.AllowedTenants[v]
	if !ok || t == "" {
		return "", ErrTenantRefused
	}
	return t, nil
}

// Subject builds the loomcycle subject "<gate>:<value>" (D5) from the
// configured subject claim (sub by default, read from the assertion's own
// Subject when the gate sent no claims).
func Subject(gate string, m *config.AuthIdentityMap, a authgate.Assertion) (string, error) {
	claim := "sub"
	if m != nil && m.SubjectClaim != "" {
		claim = m.SubjectClaim
	}
	v, ok := claimString(a, claim)
	if !ok && claim == "sub" {
		v, ok = a.Subject, true
	}
	if !ok {
		return "", ErrSubjectInvalid
	}
	v, ok = cleanValue(v, MaxExternalSubject)
	if !ok {
		return "", ErrSubjectInvalid
	}
	return gate + ":" + v, nil
}

// Resolve maps an assertion to the (tenant, subject) a NEW identity would get.
// An identity already linked keeps the subject its user_identities row names;
// the caller still checks the tenant against Tenant.
func Resolve(gate string, m *config.AuthIdentityMap, a authgate.Assertion) (Target, error) {
	t, err := Tenant(m, a)
	if err != nil {
		return Target{}, err
	}
	s, err := Subject(gate, m, a)
	if err != nil {
		return Target{}, err
	}
	return Target{TenantID: t, Subject: s}, nil
}

// AssertedGroups is the provider group list the identity map syncs: the
// configured claim, or the gate's own list. Deduplicated, sorted, bounded;
// an unusable entry is dropped.
func AssertedGroups(m *config.AuthIdentityMap, a authgate.Assertion) []string {
	if m == nil || !m.SyncGroups {
		return nil
	}
	var raw []string
	if m.GroupsClaim != "" {
		switch v := a.Claims[m.GroupsClaim].(type) {
		case string:
			raw = []string{v}
		case []any:
			for _, e := range v {
				if s, ok := e.(string); ok {
					raw = append(raw, s)
				}
			}
		}
	} else {
		raw = a.Groups
	}
	seen := map[string]bool{}
	var out []string
	for _, g := range raw {
		g, ok := cleanValue(strings.TrimSpace(g), MaxGroupClaim)
		if !ok || seen[g] {
			continue
		}
		seen[g] = true
		out = append(out, g)
		if len(out) == MaxGroups {
			break
		}
	}
	sort.Strings(out)
	return out
}

// ExternalID is the external_id of the group a gate manages for a provider
// group name.
func ExternalID(gate, providerGroup string) string { return gate + ":" + providerGroup }

// Plan reconciles a user's memberships with the groups a gate asserts.
type Plan struct {
	// Create are groups to provision (GroupCreate, insert-only).
	Create []store.GroupRow
	// Add and Remove are group names.
	Add    []string
	Remove []string
}

// SyncPlan computes a group sync (§3.7b, D6). Only groups whose external_id
// is "<gate>:<provider name>" are this gate's: they are added when asserted
// and removed when not. A local group (no external_id) and another gate's
// group are never added to or removed from, whatever is asserted — and a
// group the gate would provision under a name that is already taken is
// skipped rather than claimed.
//
// tenantGroups is every group of the tenant; current the user's direct
// memberships.
func SyncPlan(gate, tenant string, m *config.AuthIdentityMap, asserted []string, tenantGroups []store.GroupRow, current []string) Plan {
	var p Plan
	if m == nil || !m.SyncGroups || !m.Trusted {
		return p
	}
	prefix := gate + ":"
	byExt := map[string]string{}
	taken := map[string]bool{}
	managed := map[string]bool{}
	for _, g := range tenantGroups {
		taken[g.Name] = true
		if strings.HasPrefix(g.ExternalID, prefix) {
			byExt[g.ExternalID] = g.Name
			managed[g.Name] = true
		}
	}
	want := map[string]bool{}
	for _, pg := range asserted {
		ext := ExternalID(gate, pg)
		if name, ok := byExt[ext]; ok {
			want[name] = true
			continue
		}
		if !m.GroupsProvision {
			continue
		}
		name := pg
		if mapped, ok := m.GroupMap[pg]; ok {
			name = mapped
		}
		if !authz.ValidName(name) || taken[name] {
			continue
		}
		taken[name] = true
		byExt[ext] = name
		managed[name] = true
		want[name] = true
		p.Create = append(p.Create, store.GroupRow{TenantID: tenant, Name: name, ExternalID: ext,
			Description: fmt.Sprintf("synced from auth gate %s (%s)", gate, pg), CreatedBy: "gate:" + gate})
	}
	have := map[string]bool{}
	for _, c := range current {
		have[c] = true
		if managed[c] && !want[c] {
			p.Remove = append(p.Remove, c)
		}
	}
	for name := range want {
		if !have[name] {
			p.Add = append(p.Add, name)
		}
	}
	sort.Strings(p.Add)
	sort.Strings(p.Remove)
	return p
}

// DisplayName bounds a provider display name.
func DisplayName(a authgate.Assertion) string {
	n := strings.TrimSpace(a.DisplayName)
	if n == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range n {
		if unicode.IsControl(r) {
			continue
		}
		if b.Len()+len(string(r)) > MaxDisplayName {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// SafeReturnTo is where a login may send the browser afterwards: a path in
// this server's Web UI, or an absolute URL listed verbatim in allowed. It
// refuses anything else (an open redirect), and "" means the UI's home.
func SafeReturnTo(v string, allowed []string) (string, bool) {
	if v == "" {
		return "/ui/", true
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return "", false
		}
	}
	if strings.HasPrefix(v, "/") {
		// "//host" and "/\host" are scheme-relative to a browser.
		if strings.HasPrefix(v, "//") {
			return "", false
		}
		u, err := url.Parse(v)
		if err != nil || u.Scheme != "" || u.Host != "" {
			return "", false
		}
		if u.Path != "/ui" && !strings.HasPrefix(u.Path, "/ui/") {
			return "", false
		}
		if strings.Contains(u.Path, "/../") || strings.HasSuffix(u.Path, "/..") {
			return "", false
		}
		return v, true
	}
	for _, a := range allowed {
		if v == a {
			return v, true
		}
	}
	return "", false
}
