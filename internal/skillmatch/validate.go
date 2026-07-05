package skillmatch

import (
	"fmt"
	"strings"
)

// ValidateName checks a concrete `/`-grouped skill name (RFC BA). Grammar:
// one-or-more segments of [A-Za-z0-9_-]+ joined by `/`; no leading/trailing
// slash, no empty segment (`//`), no `.`/`..`. This is the name an operator
// gives a SKILL.md directory, an inline `skills:` map key, or a SkillDef
// create/fork target — NOT an allowlist pattern (use ValidatePattern for the
// agent's `skills:` entries, which may carry globs + a `+`/`-` sign).
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("empty skill name")
	}
	if strings.ContainsAny(name, "*?") {
		return fmt.Errorf("skill name %q must not contain glob characters (* ?)", name)
	}
	segs := strings.Split(name, "/")
	for _, seg := range segs {
		if err := validateNameSegment(seg, name); err != nil {
			return err
		}
	}
	return nil
}

func validateNameSegment(seg, full string) error {
	if seg == "" {
		return fmt.Errorf("skill name %q has an empty segment (no leading/trailing/double slash)", full)
	}
	if seg == "." || seg == ".." {
		return fmt.Errorf("skill name %q must not contain %q segments", full, seg)
	}
	for _, r := range seg {
		if !isNameRune(r) {
			return fmt.Errorf("skill name %q: segment %q has invalid character %q (allowed: A-Z a-z 0-9 _ -)", full, seg, r)
		}
	}
	return nil
}

// ValidatePattern checks one entry of an agent's `skills:` allowlist (RFC BA).
// An entry may carry a leading `+`/`-` sign; the remaining pattern is a
// `/`-segmented glob whose segments are `*`, `**`, or a single-segment glob of
// [A-Za-z0-9_-*?]. No empty entry/pattern/segment, no `.`/`..`.
func ValidatePattern(entry string) error {
	trimmed := strings.TrimSpace(entry)
	if trimmed == "" {
		return fmt.Errorf("empty skills entry")
	}
	pat := trimmed
	if pat[0] == '+' || pat[0] == '-' {
		pat = pat[1:]
	}
	if pat == "" {
		return fmt.Errorf("skills entry %q: missing pattern after the sign", entry)
	}
	for _, seg := range strings.Split(pat, "/") {
		if seg == "" {
			return fmt.Errorf("skills entry %q has an empty segment (no leading/trailing/double slash)", entry)
		}
		if seg == "." || seg == ".." {
			return fmt.Errorf("skills entry %q must not contain %q segments", entry, seg)
		}
		for _, r := range seg {
			if !isPatternRune(r) {
				return fmt.Errorf("skills entry %q: segment %q has invalid character %q (allowed: A-Z a-z 0-9 _ - * ?)", entry, seg, r)
			}
		}
	}
	return nil
}

func isNameRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') || r == '_' || r == '-'
}

func isPatternRune(r rune) bool {
	return isNameRune(r) || r == '*' || r == '?'
}
