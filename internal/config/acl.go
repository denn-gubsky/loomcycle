package config

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// RFC EY §9 — config-declared access control. The yaml carries the same
// shapes the HTTP surfaces take; the rules are validated by the engine's own
// strict parser at load, so an unenforceable entry fails the boot (and
// `loomcycle validate`) rather than silently granting nothing.
//
//	acls:
//	  support-tier-1:
//	    description: Support staff
//	    rules:
//	      - { effect: allow, actions: [run, read], resources: ["agent:chat/*"] }
//	      - { effect: deny,  actions: [invoke],    resources: ["tool:Bash"] }
//	groups:
//	  support:
//	    tenant: acme
//	    members: [alice, bob]
//	    groups: [support-contractors]
//	acl_bindings:
//	  - { acl: support-tier-1, group: support, tenant: acme }
//	  - { acl: read-only, tenant_wide: true, tenant: acme }

// ACLDefConfig is one declared rule list.
type ACLDefConfig struct {
	Description string           `yaml:"description"`
	Rules       []map[string]any `yaml:"rules"`
}

// GroupConfig is one declared group.
type GroupConfig struct {
	Tenant      string   `yaml:"tenant"`
	Description string   `yaml:"description"`
	ExternalID  string   `yaml:"external_id"`
	Members     []string `yaml:"members"`
	Groups      []string `yaml:"groups"`
}

// ACLBindingConfig is one declared binding. Exactly one of user, group,
// token or tenant_wide names the subject.
type ACLBindingConfig struct {
	Tenant     string    `yaml:"tenant"`
	ACL        string    `yaml:"acl"`
	User       string    `yaml:"user"`
	Group      string    `yaml:"group"`
	Token      string    `yaml:"token"`
	TenantWide bool      `yaml:"tenant_wide"`
	ExpiresAt  time.Time `yaml:"expires_at"`
}

// ResolveACLModes validates acl_modes (RFC EZ P5) against the deployment's
// LOOMCYCLE_ACL and builds Config.ResolvedACLModes. An entry may only raise
// a tenant's mode (authz.Mode.Strictness): one below the deployment's would
// drop denies the operator set fleet-wide, so it fails the load instead.
//
//	acl_modes:
//	  acme: enforce
func ResolveACLModes(c *Config) error {
	c.ResolvedACLModes = nil
	if len(c.ACLModes) == 0 {
		return nil
	}
	deployment, err := authz.ParseMode(c.Env.ACLMode)
	if err != nil {
		return err
	}
	tenants := make([]string, 0, len(c.ACLModes))
	for t := range c.ACLModes {
		tenants = append(tenants, t)
	}
	sort.Strings(tenants)
	out := make(map[string]authz.Mode, len(tenants))
	for _, t := range tenants {
		if strings.TrimSpace(t) == "" || strings.TrimSpace(t) != t {
			return fmt.Errorf("acl_modes: %q is not a tenant id", t)
		}
		raw := c.ACLModes[t]
		if strings.TrimSpace(raw) == "" {
			return fmt.Errorf("acl_modes.%s: a mode is required (compat, shadow or enforce)", t)
		}
		m, err := authz.ParseMode(raw)
		if err != nil {
			return fmt.Errorf("acl_modes.%s: %q is not one of compat, shadow, enforce", t, raw)
		}
		if m.Strictness() < deployment.Strictness() {
			return fmt.Errorf("acl_modes.%s: %s refuses less than the deployment's LOOMCYCLE_ACL=%s "+
				"(shadow < compat < enforce); a tenant's mode may only be stricter", t, m, deployment)
		}
		out[t] = m
	}
	c.ResolvedACLModes = out
	return nil
}

// ResolveACLConfig validates the acls / groups / acl_bindings blocks and
// builds Config.ResolvedACL. Every refusal names the entry. Exported so a
// test that builds a Config without Load can resolve it.
func ResolveACLConfig(c *Config) error {
	if len(c.ACLs) == 0 && len(c.Groups) == 0 && len(c.ACLBindings) == 0 {
		c.ResolvedACL = nil
		return nil
	}
	out := &authz.Static{ACLs: map[string]authz.ACL{}}
	names := make([]string, 0, len(c.ACLs))
	for name := range c.ACLs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		def := c.ACLs[name]
		if authz.IsBuiltin(name) {
			return fmt.Errorf("acls: %q is a built-in name", name)
		}
		raw, err := json.Marshal(def.Rules)
		if err != nil {
			return fmt.Errorf("acls.%s: %w", name, err)
		}
		rules, err := authz.ParseRules(raw)
		if err != nil {
			return fmt.Errorf("acls.%s: %w", name, err)
		}
		a := authz.ACL{Name: name, Description: def.Description, Rules: rules}
		if err := a.Validate(); err != nil {
			return fmt.Errorf("acls: %w", err)
		}
		out.ACLs[name] = a
	}
	gnames := make([]string, 0, len(c.Groups))
	for name := range c.Groups {
		gnames = append(gnames, name)
	}
	sort.Strings(gnames)
	graphs := map[string]authz.GroupGraph{}
	for _, name := range gnames {
		g := c.Groups[name]
		if !authz.ValidName(name) {
			return fmt.Errorf("groups: %q is not a valid name", name)
		}
		for _, m := range g.Groups {
			if m == name {
				return fmt.Errorf("groups.%s: lists itself as a member", name)
			}
			if _, ok := c.Groups[m]; !ok || c.Groups[m].Tenant != g.Tenant {
				return fmt.Errorf("groups.%s: member group %q is not declared in tenant %q", name, m, g.Tenant)
			}
		}
		if graphs[g.Tenant] == nil {
			graphs[g.Tenant] = authz.GroupGraph{}
		}
		graphs[g.Tenant][name] = append(graphs[g.Tenant][name], g.Groups...)
		out.Groups = append(out.Groups, authz.StaticGroup{Tenant: g.Tenant, Name: name, Description: g.Description, ExternalID: g.ExternalID,
			Members: append([]string(nil), g.Members...), Groups: append([]string(nil), g.Groups...)})
	}
	for tenant, graph := range graphs {
		if err := authz.ValidateGroupGraph(graph); err != nil {
			return fmt.Errorf("groups (tenant %q): %w", tenant, err)
		}
	}
	for i, b := range c.ACLBindings {
		bind := authz.Binding{ACL: b.ACL, ExpiresAt: b.ExpiresAt}
		set := 0
		if b.User != "" {
			bind.SubjectKind, bind.SubjectID = authz.SubjectUser, b.User
			set++
		}
		if b.Group != "" {
			bind.SubjectKind, bind.SubjectID = authz.SubjectGroup, b.Group
			set++
		}
		if b.Token != "" {
			bind.SubjectKind, bind.SubjectID = authz.SubjectToken, b.Token
			set++
		}
		if b.TenantWide {
			bind.SubjectKind = authz.SubjectTenant
			set++
		}
		if set != 1 {
			return fmt.Errorf("acl_bindings[%d]: exactly one of user, group, token or tenant_wide must be set", i)
		}
		if err := bind.Validate(); err != nil {
			return fmt.Errorf("acl_bindings[%d]: %w", i, err)
		}
		// A binding may name a list only the store has: the resolver reports
		// it missing until the list exists, and it matches nothing before.
		out.Bindings = append(out.Bindings, authz.StaticBinding{Tenant: b.Tenant, Binding: bind})
	}
	c.ResolvedACL = out
	return nil
}
