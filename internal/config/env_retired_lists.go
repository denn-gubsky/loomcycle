package config

import (
	"fmt"
	"regexp"
	"strings"
)

// firstHookOwnerNotInYAML returns the first entry of a retired host-widen
// owners list naming a (tenant, owner) grant the yaml list lacks: that hook's
// allow_hosts would stop being honoured. Entries are read the way
// hooks.NewPermits reads them (a bare owner is the shared tenant; an entry
// with no owner grants nothing).
func firstHookOwnerNotInYAML(value string, c retiredEnvCtx) (string, bool) {
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
			return strings.TrimSpace(e), true
		}
	}
	return "", false
}

// firstHookPrivateHostNotInYAML returns the first entry of a retired hooks
// private-host list the yaml list lacks: a hook callback to that host would
// be refused by the SSRF guard. Entries compare the way the guard matches
// them (case and a trailing dot do not count); an entry is not judged covered
// by a shorter yaml suffix or a wider range, so the operator moves it
// knowingly.
func firstHookPrivateHostNotInYAML(value string, c retiredEnvCtx) (string, bool) {
	norm := func(e string) string {
		return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(e), "."))
	}
	listed := map[string]bool{}
	for _, e := range c.cfg.Hooks.PrivateHostAllowlist {
		listed[norm(e)] = true
	}
	for _, e := range strings.Split(value, ",") {
		if n := norm(e); n != "" && !listed[n] {
			return strings.TrimSpace(e), true
		}
	}
	return "", false
}

func hookOwnersNotInYAML(value string, c retiredEnvCtx) bool {
	_, lacking := firstHookOwnerNotInYAML(value, c)
	return lacking
}

func hookPrivateHostsNotInYAML(value string, c retiredEnvCtx) bool {
	_, lacking := firstHookPrivateHostNotInYAML(value, c)
	return lacking
}

// plainListEntryRE is what an owner name, a host name or a CIDR range is
// made of. The two hooks lists hold nothing else, and none of those is a
// secret, so a refusal may quote such an entry. An entry that is anything
// else (a URL pasted with its credentials) is not quoted.
var plainListEntryRE = regexp.MustCompile(`^[A-Za-z0-9._*/:\[\] -]{1,128}$`)

// missingEntry says which entry the yaml list lacks, quoting it when it is
// plainly a name, a host or a range.
func missingEntry(entry string) string {
	if !plainListEntryRE.MatchString(entry) {
		return "the yaml list lacks one of this variable's entries (not shown: it does not look like a name, a host or a range)"
	}
	return fmt.Sprintf("the yaml list lacks %q (the first of this variable's entries it lacks)", entry)
}

// hookOwnersInstead and hookPrivateHostsInstead are the refusal texts of the
// two retired hooks lists.
func hookOwnersInstead(value string, c retiredEnvCtx) string {
	entry, _ := firstHookOwnerNotInYAML(value, c)
	return "list each [tenant:]owner under hooks.permit_host_widen.owners in the yaml; " + missingEntry(entry) +
		". Entries are compared as written, tenant and owner: the same owner under another tenant is another entry. If the yaml already grants every hook the hosts it needs, remove the variable"
}

func hookPrivateHostsInstead(value string, c retiredEnvCtx) string {
	entry, _ := firstHookPrivateHostNotInYAML(value, c)
	return "list each host or CIDR range under hooks.private_host_allowlist in the yaml; " + missingEntry(entry) +
		". Entries are compared as written: a wider range or a shorter suffix in the yaml does not count for it. If the yaml already allows every address your hooks call, remove the variable"
}
