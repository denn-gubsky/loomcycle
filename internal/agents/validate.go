package agents

import (
	"fmt"
	"strings"
)

// MaxNameLen bounds a `/`-grouped agent name. The name appears in log lines,
// yaml/error messages, and `mcp__loomcycle__spawn_run` parameters, and — for
// code-js agents — as a filesystem path under CodeRoot, so a sane cap is kept
// (128, matching the prior AgentDef floor).
const MaxNameLen = 128

// ValidateName checks a concrete `/`-grouped agent name. Grammar (mirrors the
// RFC BA skill-name grammar, internal/skillmatch.ValidateName): one-or-more
// segments of [A-Za-z0-9_-]+ joined by `/`; no leading/trailing/double slash,
// no empty segment, no `.`/`..` segment, no glob chars (`*?`). This is the name
// an operator gives a yaml `agents:` key or a create / fork / RegisterAgent
// target — NOT a `tools:`/`skills:` allowlist pattern.
//
// Agents and skills keep SEPARATE validators (no cross-package coupling) even
// though the grammar is identical today, so the two can diverge without a
// shared refactor.
//
// The `.`/`..` and separator rules double as a path-safety floor: a code-js
// agent's name becomes a path segment (agent_code/<name>/index.js), so
// forbidding `..` and a leading/trailing slash keeps a `/`-grouped name from
// escaping CodeRoot while still allowing nested dirs (doc/manager →
// agent_code/doc/manager/index.js).
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("empty agent name")
	}
	if len(name) > MaxNameLen {
		return fmt.Errorf("agent name %q is too long (max %d chars)", name, MaxNameLen)
	}
	if strings.ContainsAny(name, "*?") {
		return fmt.Errorf("agent name %q must not contain glob characters (* ?)", name)
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" {
			return fmt.Errorf("agent name %q has an empty segment (no leading/trailing/double slash)", name)
		}
		if seg == "." || seg == ".." {
			return fmt.Errorf("agent name %q must not contain %q segments", name, seg)
		}
		for _, r := range seg {
			if !isNameRune(r) {
				return fmt.Errorf("agent name %q: segment %q has invalid character %q (allowed: A-Z a-z 0-9 _ -)", name, seg, r)
			}
		}
	}
	return nil
}

func isNameRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') || r == '_' || r == '-'
}
