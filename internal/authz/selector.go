package authz

import (
	"fmt"
	"strings"
)

// MaxSelectorBytes bounds one selector; a longer one is a typo or an attack
// on the matcher, not a policy.
const MaxSelectorBytes = 256

// selector is one parsed `<type>:<name>` entry of a rule.
//
// scope is the first component of a scoped or enum type's name (`*` = any);
// glob is the `/`-split pattern of the rest (nil = every instance of the
// scope, or, for a plain glob type, impossible). literal is set when the glob
// has no metacharacters, so an exact name can be matched by a map lookup.
type selector struct {
	typ     string
	raw     string // the name half of the selector, as written
	scope   string
	glob    []string
	all     bool // the name is `*` or `**` on a glob type: every instance
	literal string
}

// parseSelector validates sel against the catalog and returns its parsed
// form. Every refusal names the selector: an unenforceable entry must never
// be stored (RFC EY §3.2).
func parseSelector(sel string) (selector, error) {
	if len(sel) > MaxSelectorBytes {
		return selector{}, fmt.Errorf("selector is longer than %d bytes", MaxSelectorBytes)
	}
	typ, name, err := SplitSelector(sel)
	if err != nil {
		return selector{}, err
	}
	spec := catalogByName[typ]
	s := selector{typ: typ, raw: name}
	switch {
	case spec.isEnum():
		if !spec.validSelectorScope(name) {
			return selector{}, fmt.Errorf("selector %q: %q is not one of %s", sel, name, strings.Join(append(spec.Scopes, "*"), ", "))
		}
		s.scope = name
		return s, nil
	case spec.isScoped():
		scope, rest, hasRest := strings.Cut(name, string(spec.Sep))
		if !spec.validSelectorScope(scope) {
			return selector{}, fmt.Errorf("selector %q: scope %q is not one of %s", sel, scope, strings.Join(append(spec.Scopes, "*"), ", "))
		}
		s.scope = scope
		if !hasRest || rest == "" {
			return s, nil // every instance of the scope
		}
		g, lit, err := parseGlob(rest, spec.Sep)
		if err != nil {
			return selector{}, fmt.Errorf("selector %q: %w", sel, err)
		}
		s.glob, s.literal = g, lit
		return s, nil
	default:
		if name == "" {
			return selector{}, fmt.Errorf("selector %q: a name is required (use %s:* for every instance)", sel, typ)
		}
		if name == "*" || name == "**" {
			s.all = true
			return s, nil
		}
		g, lit, err := parseGlob(name, '/')
		if err != nil {
			return selector{}, fmt.Errorf("selector %q: %w", sel, err)
		}
		s.glob, s.literal = g, lit
		return s, nil
	}
}

// parseGlob splits a pattern on `/` and validates each segment. It returns
// the literal form when the pattern carries no metacharacter. A leading `/`
// (a document path) is dropped so `/kb/**` and `kb/**` are one pattern. A
// `..` segment is refused: a selector cannot climb (mirrors the Path tool).
func parseGlob(pattern string, sep byte) ([]string, string, error) {
	_ = sep // the glob part is `/`-segmented whatever separates it from its scope
	pattern = strings.TrimPrefix(pattern, "/")
	if pattern == "" {
		return nil, "", fmt.Errorf("empty pattern")
	}
	segs := strings.Split(pattern, "/")
	literal := true
	for _, seg := range segs {
		switch {
		case seg == "":
			return nil, "", fmt.Errorf("pattern %q has an empty segment", pattern)
		case seg == "..":
			return nil, "", fmt.Errorf("pattern %q climbs with ..", pattern)
		case strings.ContainsAny(seg, "*?"):
			literal = false
		}
		for i := 0; i < len(seg); i++ {
			c := seg[i]
			if c < 0x21 || c == 0x7f {
				return nil, "", fmt.Errorf("pattern %q has a control or space character", pattern)
			}
		}
	}
	if literal {
		return segs, strings.Join(segs, "/"), nil
	}
	return segs, "", nil
}

// splitName parses a resource NAME (not a selector) the way the type's
// selector shape does, so matching compares like with like.
func splitName(spec TypeSpec, name string) (scope string, segs []string) {
	switch {
	case spec.isEnum():
		return name, nil
	case spec.isScoped():
		scope, rest, _ := strings.Cut(name, string(spec.Sep))
		rest = strings.TrimPrefix(rest, "/")
		if rest == "" {
			return scope, nil
		}
		return scope, strings.Split(rest, "/")
	default:
		return "", strings.Split(strings.TrimPrefix(name, "/"), "/")
	}
}

// matches reports whether the selector names the resource name. The
// type-level name AnyInstance is matched only by a selector that covers
// every instance of the type.
func (s selector) matches(spec TypeSpec, name string) bool {
	if name == AnyInstance {
		if len(spec.Scopes) > 0 {
			return s.scope == "*" && s.glob == nil
		}
		return s.all
	}
	scope, segs := splitName(spec, name)
	if len(spec.Scopes) > 0 {
		if s.scope != "*" && s.scope != scope {
			return false
		}
		if spec.isEnum() {
			return true
		}
		if s.glob == nil {
			return true // every instance of the scope
		}
		return doublestarMatch(s.glob, segs)
	}
	if s.all {
		return true
	}
	return doublestarMatch(s.glob, segs)
}

// doublestarMatch matches a `/`-split pattern against a `/`-split path. `**`
// matches zero or more segments; every other segment is a single-segment
// glob. The same matcher the `skills:` allowlist uses (internal/skillmatch),
// reimplemented so this package imports nothing.
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

func segMatch(pat, seg string) bool {
	if !strings.ContainsAny(pat, "*?") {
		return pat == seg
	}
	return globSeg(pat, seg)
}

// globSeg is a small backtracking glob over a single segment: `*` any run,
// `?` any single character, everything else literal.
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
