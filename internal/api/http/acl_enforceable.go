package http

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// aclReportTimeout bounds the report: it reads every tenant's lists, and a
// slow store must not hold the boot or a restore.
const aclReportTimeout = 30 * time.Second

// aclReportMaxLines caps the WARN lines per class; a summary line counts the
// rest.
const aclReportMaxLines = 100

// The classes the report sorts a stored rule into (RFC FY Part E). A rule can
// be in more than one.
const (
	// aclReportUnenforced: a pair or a transport value no surface asks or
	// stamps — the rule decides nothing.
	aclReportUnenforced = "unenforced"
	// aclReportInert: a condition on the model or user tier, which no
	// surface vouches for — the rule is skipped whole.
	aclReportInert = "inert"
	// aclReportSourceCIDR: a source_cidr deny, which before this release
	// never applied and now does.
	aclReportSourceCIDR = "source_cidr_enforced"
	// aclReportTransportHTTP: a rule on transport http, which no longer
	// covers a request to /v1/_mcp (now mcp) or from the Web UI (now webui).
	aclReportTransportHTTP = "transport_http"
	// aclReportTypeLevelOnly: a selector naming some instances of a verb asked
	// only of every instance (RFC GD D2) — a deny refuses nothing, an allow
	// admits every instance. Writes refuse it now.
	aclReportTypeLevelOnly = "type_level_only"
)

var aclReportText = map[string]string{
	aclReportUnenforced:    "authz: a stored access-control rule names something no surface evaluates; it decides nothing",
	aclReportInert:         "authz: a stored access-control rule is inert: names a fact no surface vouches for",
	aclReportSourceCIDR:    "authz: a stored access-control source_cidr deny changed meaning: now enforced",
	aclReportTransportHTTP: "authz: a stored access-control rule on transport http no longer covers mcp or webui requests",
	aclReportTypeLevelOnly: "authz: a stored access-control rule names some instances of a verb asked only of every instance: a deny refuses nothing, an allow admits every instance",
}

// ReportUnenforceableACLs logs every live list — stored in any tenant, or
// declared in config — with a rule whose meaning this release changed or
// that does not do what it says (the classes above). It runs at boot and
// after a snapshot restore and never fails either: the list's other rules
// still hold. Bounded by aclReportTimeout (logged and skipped on expiry) and
// by aclReportMaxLines per class. Names and ids only. It returns every
// finding, uncapped, one "<class> <source> <tenant>/<list>#<rule>: <detail>"
// each.
func (s *Server) ReportUnenforceableACLs(ctx context.Context) []string {
	ctx, cancel := context.WithTimeout(ctx, aclReportTimeout)
	defer cancel()
	var out []string
	if c := s.cfg(); c != nil && c.ResolvedACL != nil {
		names := make([]string, 0, len(c.ResolvedACL.ACLs))
		for name := range c.ResolvedACL.ACLs {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			out = append(out, classifyACLRules("config", "", name, c.ResolvedACL.ACLs[name].Rules)...)
		}
	}
	if s.store != nil {
		snap, err := s.store.SnapshotReadAcl(ctx)
		switch {
		case ctx.Err() != nil:
			slog.Warn("authz: the stored-rule report timed out and was skipped", "timeout", aclReportTimeout)
		case err != nil:
			slog.Warn("authz: could not read the stored lists to check each rule", "err", err)
		default:
			for _, row := range snap.Defs {
				if row.Retired() {
					continue
				}
				rules, err := authz.ParseRules(row.Rules)
				if err != nil {
					continue // the resolver refuses the whole set for it, and says so
				}
				out = append(out, classifyACLRules("store", row.TenantID, row.Name, rules)...)
			}
		}
	}
	logACLReport(out)
	return out
}

// classifyACLRules is one list's findings.
func classifyACLRules(source, tenant, name string, rules []authz.Rule) []string {
	var out []string
	add := func(class string, i int, detail string) {
		out = append(out, fmt.Sprintf("%s %s %s/%s#%d: %s", class, source, tenant, name, i, detail))
	}
	for i, r := range rules {
		w := r.When
		inert := w != nil && (len(w.Tier) > 0 || len(w.UserTier) > 0)
		if inert {
			add(aclReportInert, i, "tier or user_tier")
		} else if err := authz.RuleNotEnforceable(r); err != nil {
			class := aclReportUnenforced
			var ne *authz.NotEnforceableError
			if errors.As(err, &ne) && ne.Selector != "" {
				class = aclReportTypeLevelOnly
			}
			add(class, i, err.Error())
		}
		if w == nil || inert {
			continue
		}
		if r.Effect == authz.Deny && len(w.SourceCIDR) > 0 {
			add(aclReportSourceCIDR, i, fmt.Sprintf("source_cidr %v", w.SourceCIDR))
		}
		if slices.Contains(w.Transport, "http") && !(slices.Contains(w.Transport, "mcp") && slices.Contains(w.Transport, "webui")) {
			add(aclReportTransportHTTP, i, fmt.Sprintf("transport %v", w.Transport))
		}
	}
	return out
}

// logACLReport writes the findings, at most aclReportMaxLines per class,
// then one summary per class.
func logACLReport(findings []string) {
	counts := map[string]int{}
	for _, f := range findings {
		class, detail, _ := strings.Cut(f, " ")
		counts[class]++
		if counts[class] <= aclReportMaxLines {
			slog.Warn(aclReportText[class], "finding", detail)
		}
	}
	classes := make([]string, 0, len(counts))
	for c := range counts {
		classes = append(classes, c)
	}
	sort.Strings(classes)
	for _, c := range classes {
		slog.Warn("authz: stored-rule report", "class", c, "rules", counts[c], "logged", min(counts[c], aclReportMaxLines))
	}
}
