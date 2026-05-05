// Package policy enforces server-authoritative per-agent tool allowlists.
//
// The Apply pattern is: the agent definition (from YAML) lists allowed tools;
// the run options may further narrow that list; the dispatcher only sees the
// intersection. Glob suffixes ("mcp__brave-search__*") are supported.
package policy

import "strings"

// Apply returns the subset of available tool names that satisfy both the
// agent's allowlist and the caller's requested list. If callerAllowed is nil,
// the agent allowlist alone is used.
func Apply(available, agentAllowed, callerAllowed []string) []string {
	agentSet := make(map[string]bool, len(agentAllowed))
	for _, a := range agentAllowed {
		agentSet[a] = true
	}
	var callerSet map[string]bool
	if callerAllowed != nil {
		callerSet = make(map[string]bool, len(callerAllowed))
		for _, c := range callerAllowed {
			callerSet[c] = true
		}
	}

	out := make([]string, 0, len(available))
	for _, name := range available {
		if !Matches(name, agentSet) {
			continue
		}
		if callerSet != nil && !Matches(name, callerSet) {
			continue
		}
		out = append(out, name)
	}
	return out
}

// Matches checks if name is allowed by any rule in the set. Rules ending in
// "*" match by prefix; otherwise exact match. Exported so other packages
// (e.g. config) can perform the same subset check when validating layered
// allowlists.
func Matches(name string, set map[string]bool) bool {
	if set[name] {
		return true
	}
	for rule := range set {
		if strings.HasSuffix(rule, "*") && strings.HasPrefix(name, rule[:len(rule)-1]) {
			return true
		}
	}
	return false
}
