package help

import (
	"regexp"
	"strings"
)

// argumentName is a top-level argument as an article names it: lowercase JSON
// keys, the only shape the builtin schemas use.
var argumentName = regexp.MustCompile("`([a-z_][a-z0-9_]*)`")

// Arguments returns the arguments an operation article documents, from its
// `## Arguments` section, and whether the section exists.
//
// Each top-level bullet names its arguments in backticks before the first
// " — ": "- `body` — Markdown text." or "- `type`, `status` — labels.". Nested
// bullets describe values, not arguments, and are skipped. A section with no
// bullets ("None besides `op`.") documents an op that takes no arguments, which
// is different from an article with no section at all (ok=false: nothing is
// known). `op` itself is never listed; every call carries it.
//
// The dispatcher uses this to refuse an argument that belongs to a SIBLING
// operation, which a schema shared by every op of a tool cannot express.
func (t *Topic) Arguments() ([]string, bool) {
	if t == nil {
		return nil, false
	}
	var (
		out       []string
		inSection bool
		found     bool
		inFence   bool
	)
	for _, ln := range strings.Split(t.Content, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if strings.HasPrefix(ln, "## ") {
			inSection = strings.TrimSpace(ln) == "## Arguments"
			found = found || inSection
			continue
		}
		if !inSection || !strings.HasPrefix(ln, "- `") {
			continue
		}
		head := ln
		if i := strings.Index(head, " — "); i >= 0 {
			head = head[:i]
		}
		for _, m := range argumentName.FindAllStringSubmatch(head, -1) {
			if m[1] != "op" {
				out = append(out, m[1])
			}
		}
	}
	return out, found
}
