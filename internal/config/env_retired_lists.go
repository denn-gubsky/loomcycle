package config

import "strings"

// hookOwnersNotInYAML reports a retired host-widen owners list naming a
// (tenant, owner) grant the yaml list lacks: that hook's allow_hosts would
// stop being honoured. Entries are read the way hooks.NewPermits reads them
// (a bare owner is the shared tenant; an entry with no owner grants nothing).
func hookOwnersNotInYAML(value string, c retiredEnvCtx) bool {
	grant := func(entry string) (string, bool) {
		tenant, owner := "", strings.TrimSpace(entry)
		if t, o, ok := strings.Cut(owner, ":"); ok {
			tenant, owner = strings.TrimSpace(t), strings.TrimSpace(o)
		}
		return tenant + ":" + owner, owner != ""
	}
	listed := map[string]bool{}
	for _, e := range c.cfg.Hooks.PermitHostWiden.Owners {
		if g, ok := grant(e); ok {
			listed[g] = true
		}
	}
	for _, e := range strings.Split(value, ",") {
		if g, ok := grant(e); ok && !listed[g] {
			return true
		}
	}
	return false
}

// hookPrivateHostsNotInYAML reports a retired hooks private-host list naming
// an entry the yaml list lacks: a hook callback to that host would be refused
// by the SSRF guard. Entries compare the way the guard matches them (case
// and a trailing dot do not count); an entry is not judged covered by a
// shorter yaml suffix, so the operator moves it knowingly.
func hookPrivateHostsNotInYAML(value string, c retiredEnvCtx) bool {
	norm := func(e string) string {
		return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(e), "."))
	}
	listed := map[string]bool{}
	for _, e := range c.cfg.Hooks.PrivateHostAllowlist {
		listed[norm(e)] = true
	}
	for _, e := range strings.Split(value, ",") {
		if e = norm(e); e != "" && !listed[e] {
			return true
		}
	}
	return false
}
