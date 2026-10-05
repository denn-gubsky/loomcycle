package teamgraph

import "fmt"

// MaxNameLen bounds a team name. It matches the variable-name cap in this
// package: a team name is shown in run labels and log lines, never a path.
const MaxNameLen = 64

// ValidateName checks a team name: ONE segment of [A-Za-z0-9_-], 1..64
// characters. Unlike an agent or skill name it has no "/" grouping, because a
// team name is itself a component of two composites that must split
// unambiguously — a walk's run is labelled "team:<name>" (and a breakpoint spec
// is cut at its last ":"), and a team's own members are shown as
// "<team>/<name>". A "/" or ":" inside the name would make either one
// ambiguous.
//
// This is the grammar for a NEW name. Rows written before the rule existed may
// hold a name outside it; the caller decides whether a name is new.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("team name is required")
	}
	if len(name) > MaxNameLen {
		return fmt.Errorf("team name is %d bytes; the limit is %d", len(name), MaxNameLen)
	}
	for _, r := range name {
		ok := r == '_' || r == '-' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if !ok {
			return fmt.Errorf("team name %q has invalid character %q (allowed: A-Z a-z 0-9 _ - in one segment of at most %d characters; no \"/\", \":\", \".\" or spaces)", name, r, MaxNameLen)
		}
	}
	return nil
}
