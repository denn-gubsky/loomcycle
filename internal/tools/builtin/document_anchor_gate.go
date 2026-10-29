package builtin

import (
	"context"
	"regexp"
	"strings"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
)

// document_anchor_gate.go — the ontology's hooks for part anchors (RFC FN P2 ↔
// RFC FO). A document class may declare an @anchor_grammar (AKN eId style) that
// keys its parts better than their heading path does. FO owns the bodies; until
// it fills them every document uses the heading-path anchor.

// anchorGrammarFor names the @anchor_grammar of the class of the document whose
// root is rootID ("" = none: heading-path anchors).
//
// The class is the root chunk's type, resolved against the tenant's CONFIRMED classes;
// the grammar is the class's own or its nearest ancestor's. A grammar the ontology
// reports as broken is disabled, so it yields "" rather than a half-working anchor.
func (d *Document) anchorGrammarFor(ctx context.Context, key sqlmem.ScopeKey, rootID string) string {
	if d.Store == nil || d.SqlMem == nil || rootID == "" {
		return ""
	}
	row, ok, err := d.getChunkRow(ctx, key, rootID)
	if err != nil || !ok || strings.TrimSpace(row.Type) == "" {
		return ""
	}
	read, ok := d.tenantOntologyRead(ctx)
	if !ok || !read.Confirmed {
		return ""
	}
	var class memrank.OntologyTerm
	found := false
	for _, c := range memrank.EffectiveClasses(read.Terms, true) {
		if strings.EqualFold(c.Name, row.Type) {
			class, found = c, true
			break
		}
	}
	if !found {
		return ""
	}
	name := memrank.EffectiveDirective(class, "anchor_grammar")
	if _, usable := usableGrammar(read.Grammars, name); !usable {
		return ""
	}
	return name
}

// anchorFor applies a grammar to a part's heading path; "" when grammar is "" or
// does not match, and the caller falls back to the heading path.
//
// The deepest heading is tried first — a part is keyed by its own heading ("Article 5")
// before its ancestors' — and the first matching pattern wins. The anchor is the
// grammar's `resolve:` line's `anchor=` template with each {group} filled from that
// match; a grammar whose resolve line names no anchor yields "".
func (d *Document) anchorFor(ctx context.Context, grammar string, headingPath []string) string {
	if grammar == "" || len(headingPath) == 0 || d.Store == nil || d.SqlMem == nil {
		return ""
	}
	read, ok := d.tenantOntologyRead(ctx)
	if !ok || !read.Confirmed {
		return ""
	}
	g, usable := usableGrammar(read.Grammars, grammar)
	if !usable {
		return ""
	}
	tmpl := resolveTemplate(g.Resolve, "anchor")
	if tmpl == "" {
		return ""
	}
	var res []*regexp.Regexp
	for _, p := range g.Patterns {
		// Compiled again here rather than kept on the parsed grammar: the parse is shared
		// through the ontology cache and stays plain data. RE2 — linear time, no
		// backtracking — and the parser has already capped each pattern's length.
		if re, err := regexp.Compile(p); err == nil {
			res = append(res, re)
		}
	}
	for i := len(headingPath) - 1; i >= 0; i-- {
		for _, re := range res {
			m := re.FindStringSubmatch(headingPath[i])
			if m == nil {
				continue
			}
			return fillTemplate(tmpl, re.SubexpNames(), m)
		}
	}
	return ""
}

// usableGrammar finds a named grammar that parsed cleanly.
func usableGrammar(grammars []memrank.OntologyGrammar, name string) (memrank.OntologyGrammar, bool) {
	if name == "" {
		return memrank.OntologyGrammar{}, false
	}
	for _, g := range grammars {
		if g.Name == name {
			return g, g.Issue == "" && len(g.Patterns) > 0
		}
	}
	return memrank.OntologyGrammar{}, false
}

// resolveTemplate returns the template for one key of a resolve line
// (`work_key=law:{number}, anchor=art{article}`), or "".
func resolveTemplate(resolve, field string) string {
	for _, part := range strings.Split(resolve, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && strings.TrimSpace(k) == field {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

var templateGroupRe = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// fillTemplate substitutes each {group} with that named group's match. A group that did
// not participate in the match makes the anchor unusable: "" rather than a key with a
// hole in it, which would collide with every other part missing the same group.
func fillTemplate(tmpl string, names, match []string) string {
	values := map[string]string{}
	for i, n := range names {
		if n != "" && i < len(match) {
			values[n] = match[i]
		}
	}
	missing := false
	out := templateGroupRe.ReplaceAllStringFunc(tmpl, func(g string) string {
		v := strings.TrimSpace(values[g[1:len(g)-1]])
		if v == "" {
			missing = true
		}
		return v
	})
	if missing {
		return ""
	}
	return out
}
