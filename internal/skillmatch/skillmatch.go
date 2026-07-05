// Package skillmatch implements the RFC BA ordered pattern allowlist that
// governs an agent's skill access — listing, using (invoking), and authoring
// (SkillDef create/fork). It also validates `/`-grouped skill names and the
// allowlist entries themselves.
//
// An allowlist is the agent's `skills:` YAML field (repurposed by RFC BA from
// an exact-name bundle list into a pattern allowlist). Each entry is:
//
//	pat  or  +pat   — a POSITIVE rule (allow names matching pat)
//	-pat            — a NEGATIVE rule (deny names matching pat)
//
// where pat is a `/`-segmented glob (`doc/*`, `marketing/**`, exact `seo`).
//
// Evaluation (order-independent — negatives always win):
//   - hasPositive = the list contains at least one positive entry.
//   - If hasPositive (WHITELIST mode): a name is allowed iff it matches ≥1
//     positive AND no negative.
//   - Else (only negatives, or empty — BLACKLIST mode): allowed iff it matches
//     no negative.
//   - An empty/absent list allows everything (the RFC BA default = all).
//   - `-*` (or `-**`) denies everything.
//
// Glob semantics: a lone `*` (or `**`) means "every name, including grouped
// ones" — so `-*` is deny-all and a bare `*`/`+*` is allow-all. WITHIN a path,
// `*` matches exactly one segment (`doc/*` matches `doc/redactor` but not
// `doc/a/b`) and `**` matches zero or more segments (`marketing/**` matches
// `marketing/seo` and `marketing/x/y`).
package skillmatch

import "strings"

// Allowed reports whether the given skill name is permitted by the ordered
// pattern allowlist. See the package doc for the exact semantics. An empty
// list allows every name.
func Allowed(patterns []string, name string) bool {
	hasPositive := false
	matchedPositive := false
	matchedNegative := false
	for _, entry := range patterns {
		neg, pat, ok := splitEntry(entry)
		if !ok {
			// Malformed entry (empty after sign strip). Config-load
			// validation rejects these; skip defensively here.
			continue
		}
		if neg {
			if matchPattern(pat, name) {
				matchedNegative = true
			}
			continue
		}
		hasPositive = true
		if matchPattern(pat, name) {
			matchedPositive = true
		}
	}
	// A negative match denies regardless of position or any positive match —
	// the deny rule is a hard floor.
	if matchedNegative {
		return false
	}
	if hasPositive {
		return matchedPositive
	}
	return true
}

// HasPositive reports whether the allowlist contains at least one positive
// (whitelist) entry. Used to decide whether to inject the system-prompt note
// naming the permitted patterns.
func HasPositive(patterns []string) bool {
	for _, entry := range patterns {
		neg, pat, ok := splitEntry(entry)
		if ok && !neg && pat != "" {
			return true
		}
	}
	return false
}

// DeniesAll reports whether the allowlist denies EVERY possible name — the one
// case (`skills: [-*]`) where the agent gets no skill access at all, so the
// Skill tool is NOT auto-added. A deny-all is any negative entry whose pattern
// matches everything (`-*` or `-**`). A whitelist that merely happens to match
// no existing skill (`[+doc/none]`) does NOT deny all — the policy still
// permits that name pattern, so the agent keeps the Skill tool.
func DeniesAll(patterns []string) bool {
	for _, entry := range patterns {
		neg, pat, ok := splitEntry(entry)
		if ok && neg && isMatchAll(pat) {
			return true
		}
	}
	return false
}

// splitEntry trims an allowlist entry, strips a leading sign, and returns
// (negative, pattern, ok). ok is false for an empty/sign-only entry.
func splitEntry(entry string) (neg bool, pat string, ok bool) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return false, "", false
	}
	switch entry[0] {
	case '-':
		neg, pat = true, entry[1:]
	case '+':
		neg, pat = false, entry[1:]
	default:
		neg, pat = false, entry
	}
	if pat == "" {
		return false, "", false
	}
	return neg, pat, true
}

// isMatchAll reports whether a (sign-stripped) pattern matches every name.
func isMatchAll(pat string) bool { return pat == "*" || pat == "**" }

// matchPattern matches a `/`-segmented glob pattern against a `/`-segmented
// skill name. A lone `*`/`**` matches every name (including grouped ones);
// otherwise `**` matches zero+ segments and any other segment is a single-
// segment glob.
func matchPattern(pat, name string) bool {
	if isMatchAll(pat) {
		return true
	}
	return doublestarMatch(strings.Split(pat, "/"), strings.Split(name, "/"))
}

// doublestarMatch matches a `/`-split pattern against a `/`-split path. `**`
// matches zero or more segments; every other segment is matched with segMatch
// (a single-segment `*`/`?`/literal glob). Backtracking DP over
// (pattern-index, path-index) — small enough for skill names (few segments).
// Reimplemented here (rather than importing internal/tools/builtin) to keep
// this package dependency-free.
func doublestarMatch(pat, path []string) bool {
	if len(pat) == 0 {
		return len(path) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(path); i++ {
			if doublestarMatch(pat[1:], path[i:]) {
				return true
			}
		}
		return false
	}
	if len(path) == 0 {
		return false
	}
	if !segMatch(pat[0], path[0]) {
		return false
	}
	return doublestarMatch(pat[1:], path[1:])
}

// segMatch matches one glob segment against one path segment. Supports `*`
// (any run, including empty) and `?` (any single char); every other char is a
// literal. No character classes — skill names are `[A-Za-z0-9_-]/`, so `*`
// and `?` are the only metacharacters worth supporting.
func segMatch(pat, seg string) bool {
	// Fast path: no metacharacters → literal compare.
	if !strings.ContainsAny(pat, "*?") {
		return pat == seg
	}
	return globSeg(pat, seg)
}

// globSeg is a small backtracking glob over a single segment.
func globSeg(pat, s string) bool {
	pi, si := 0, 0
	star, mark := -1, 0
	for si < len(s) {
		switch {
		case pi < len(pat) && (pat[pi] == '?' || pat[pi] == s[si]):
			pi++
			si++
		case pi < len(pat) && pat[pi] == '*':
			star = pi
			mark = si
			pi++
		case star != -1:
			pi = star + 1
			mark++
			si = mark
		default:
			return false
		}
	}
	for pi < len(pat) && pat[pi] == '*' {
		pi++
	}
	return pi == len(pat)
}
