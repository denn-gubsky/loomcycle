package http

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// ReportUnenforceableACLs logs, once at boot, every live list — stored in any
// tenant, or declared in config — that still names a pair or a condition no
// surface evaluates (RFC FY Part E). Such a list was written before the write
// surfaces refused it, or names a verb since removed from the catalog; the
// rule does not do what it says (a pair decides nothing; an unfilled
// condition holds a deny everywhere and an allow nowhere), and an operator
// relying on it should know. The boot
// is not failed: the list's other rules still hold. Names and ids only. It
// returns what it logged, one "<source> <tenant>/<list>#<rule>: <reason>" each.
func (s *Server) ReportUnenforceableACLs(ctx context.Context) []string {
	var out []string
	if c := s.cfg(); c != nil && c.ResolvedACL != nil {
		names := make([]string, 0, len(c.ResolvedACL.ACLs))
		for name := range c.ResolvedACL.ACLs {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			out = append(out, reportUnenforceable("config", "", name, c.ResolvedACL.ACLs[name].Rules)...)
		}
	}
	if s.store == nil {
		return out
	}
	snap, err := s.store.SnapshotReadAcl(ctx)
	if err != nil {
		slog.Warn("authz: could not read the stored lists to check that each rule is enforced", "err", err)
		return out
	}
	for _, row := range snap.Defs {
		if row.Retired() {
			continue
		}
		rules, err := authz.ParseRules(row.Rules)
		if err != nil {
			continue // the resolver refuses the whole set for it, and says so
		}
		out = append(out, reportUnenforceable("store", row.TenantID, row.Name, rules)...)
	}
	return out
}

func reportUnenforceable(source, tenant, name string, rules []authz.Rule) []string {
	var out []string
	for i, r := range rules {
		if err := authz.RuleNotEnforceable(r); err != nil {
			out = append(out, fmt.Sprintf("%s %s/%s#%d: %v", source, tenant, name, i, err))
			// A pair no surface asks decides nothing; a condition no surface
			// fills reads as unknown, so a deny on it holds everywhere and an
			// allow nowhere. Either way the rule does not do what it says.
			slog.Warn("authz: a stored access-control rule names something no surface evaluates",
				"source", source, "tenant", tenant, "acl", name, "rule", i, "reason", err.Error())
		}
	}
	return out
}
