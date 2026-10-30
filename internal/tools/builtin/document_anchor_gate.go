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
func (d *Document) anchorGrammarFor(ctx context.Context, key sqlmem.ScopeKey, rootID string) string {
	g, _, _ := d.classGrammar(ctx, key, rootID, "anchor_grammar")
	return g.Name
}

// classGrammar is the grammar the class of the document whose root is rootID names in
// directive (anchor_grammar or citation_grammar). class is that class ("" when the root's
// type is not a class in force); issue explains a named grammar that cannot be used, and
// the grammar is then zero.
//
// The class is the root chunk's type, resolved against the tenant's CONFIRMED classes;
// the grammar is the class's own or its nearest ancestor's. A grammar the ontology
// reports as broken is disabled, so it yields nothing rather than half-working matches.
func (d *Document) classGrammar(ctx context.Context, key sqlmem.ScopeKey, rootID, directive string) (g memrank.OntologyGrammar, class, issue string) {
	if d.Store == nil || d.SqlMem == nil || rootID == "" {
		return memrank.OntologyGrammar{}, "", ""
	}
	row, ok, err := d.getChunkRow(ctx, key, rootID)
	if err != nil || !ok || strings.TrimSpace(row.Type) == "" {
		return memrank.OntologyGrammar{}, "", ""
	}
	read, ok := d.tenantOntologyRead(ctx)
	if !ok || !read.Confirmed {
		return memrank.OntologyGrammar{}, "", ""
	}
	var term memrank.OntologyTerm
	found := false
	for _, c := range memrank.EffectiveClasses(read.Terms, true) {
		if strings.EqualFold(c.Name, row.Type) {
			term, found = c, true
			break
		}
	}
	if !found {
		return memrank.OntologyGrammar{}, "", ""
	}
	names := memrank.GrammarNames(memrank.EffectiveDirective(term, directive))
	if len(names) == 0 {
		return memrank.OntologyGrammar{}, term.Name, ""
	}
	name := names[0] // an anchor grammar is one; citations go through classGrammars
	g, usable := usableGrammar(read.Grammars, name)
	if !usable {
		if g.Issue != "" {
			return memrank.OntologyGrammar{}, term.Name, "grammar " + name + " is disabled: " + g.Issue
		}
		return memrank.OntologyGrammar{}, term.Name, "grammar " + name + " is not declared in the ontology"
	}
	if directive == "citation_grammar" {
		if issue := citationGrammarIssue(g); issue != "" {
			return memrank.OntologyGrammar{}, term.Name, issue
		}
	}
	return g, term.Name, ""
}

// citationGrammarIssue explains why a grammar that parsed cleanly still cannot cite:
// one that only keys anchors (anchor: lines, no pattern:) has nothing to find a
// citation with, and named as a citation grammar it would find none without a word.
func citationGrammarIssue(g memrank.OntologyGrammar) string {
	if len(g.Patterns) == 0 {
		return "grammar " + g.Name + " declares no pattern: line, so it finds no citations"
	}
	return ""
}

// anchorFor applies a grammar to a part's heading path; "" when grammar is "" or
// does not match, and the caller falls back to the heading path.
//
// The deepest heading is tried first — a part is keyed by its own heading ("Article 5")
// before its ancestors' — and the first matching pattern wins. The anchor is the
// grammar's `resolve:` line's `anchor=` template with each {group} filled from that
// match; a grammar whose resolve line names no anchor yields "".
//
// A grammar with scope: or anchor: lines composes the anchor instead: see scopedAnchor.
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
	if len(g.Scopes) > 0 || len(g.Anchors) > 0 {
		return scopedAnchor(g, tmpl, headingPath)
	}
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

// anchorRule is a compiled scope: or anchor: line.
type anchorRule struct {
	re        *regexp.Regexp
	templates []string
}

// compileAnchorRules compiles a grammar's rule lines. Compiled here rather than kept on
// the parsed grammar, as anchorFor does with patterns: the parse is shared through the
// ontology cache and stays plain data. The parser has already capped each pattern.
func compileAnchorRules(rules []memrank.GrammarRule) []anchorRule {
	out := make([]anchorRule, 0, len(rules))
	for _, r := range rules {
		if re, err := regexp.Compile(r.Pattern); err == nil {
			out = append(out, anchorRule{re: re, templates: r.Templates})
		}
	}
	return out
}

// keyHeading is the segment the first rule that keys heading gives. A rule keys it when
// its pattern matches and one of its alternatives fills — the first whose groups all
// took part in the match; a literal always does. A rule that matches but fills none
// does not key the heading, and the next rule is tried.
func keyHeading(rules []anchorRule, heading string) (string, bool) {
	for _, r := range rules {
		m := r.re.FindStringSubmatch(heading)
		if m == nil {
			continue
		}
		for _, t := range r.templates {
			if seg := fillTemplate(t, r.re.SubexpNames(), m); seg != "" {
				return seg, true
			}
		}
	}
	return "", false
}

// scopedAnchor composes a part's anchor from a grammar's scope: and anchor: lines.
//
// A heading a scope: line keys is a SCOPE heading (an annex, the resolution around the
// act it approves) and is never a leaf. The leaf is the deepest heading below the
// innermost scope that an anchor: line keys — or, after the anchor: lines, a pattern:
// line filling the resolve line's anchor= template (legacy). The anchor is every scope
// segment, outer to inner, then the leaf: `annex-2/point-3`.
//
// The leaf is never sought above the innermost scope: a table inside an annex nested in
// chapter 3 keyed as "chapter-3" would read as part of the chapter, outside its annex.
// Without a leaf, a part that is itself a scope heading is keyed by the scopes alone
// (`annex-2`); any other part gets "", and the caller falls back to its heading path.
func scopedAnchor(g memrank.OntologyGrammar, tmpl string, path []string) string {
	scopes := compileAnchorRules(g.Scopes)
	leaves := compileAnchorRules(g.Anchors)
	if tmpl != "" {
		for _, p := range g.Patterns {
			if re, err := regexp.Compile(p); err == nil {
				leaves = append(leaves, anchorRule{re: re, templates: []string{tmpl}})
			}
		}
	}
	var segs []string
	innermost := -1
	for i, h := range path {
		if seg, ok := keyHeading(scopes, h); ok {
			segs = append(segs, seg)
			innermost = i
		}
	}
	for i := len(path) - 1; i > innermost; i-- {
		if leaf, ok := keyHeading(leaves, path[i]); ok {
			return strings.Join(append(segs, leaf), "/")
		}
	}
	if innermost == len(path)-1 {
		return strings.Join(segs, "/")
	}
	return ""
}

// usableGrammar finds a named grammar that parsed cleanly. A grammar with anchor: lines
// and no pattern: line is usable for anchors; citation use checks citationGrammarIssue.
func usableGrammar(grammars []memrank.OntologyGrammar, name string) (memrank.OntologyGrammar, bool) {
	if name == "" {
		return memrank.OntologyGrammar{}, false
	}
	for _, g := range grammars {
		if g.Name == name {
			return g, g.Issue == "" && (len(g.Patterns) > 0 || len(g.Anchors) > 0)
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

// classGrammars is every citation grammar the document's class names, in the order
// named, with an issue for each that cannot be used. A class citing laws and a
// regulator's acts needs two grammars, since a grammar has one resolve line.
func (d *Document) classGrammars(ctx context.Context, key sqlmem.ScopeKey, rootID string) (gs []memrank.OntologyGrammar, class string, issues []string) {
	first, class, issue := d.classGrammar(ctx, key, rootID, "citation_grammar")
	if class == "" {
		return nil, "", nil
	}
	read, ok := d.tenantOntologyRead(ctx)
	if !ok || !read.Confirmed {
		return nil, class, nil
	}
	var term memrank.OntologyTerm
	for _, c := range memrank.EffectiveClasses(read.Terms, true) {
		if strings.EqualFold(c.Name, class) {
			term = c
		}
	}
	for i, name := range memrank.GrammarNames(memrank.EffectiveDirective(term, "citation_grammar")) {
		if i == 0 {
			if issue != "" {
				issues = append(issues, issue)
			} else if first.Name != "" {
				gs = append(gs, first)
			}
			continue
		}
		g, usable := usableGrammar(read.Grammars, name)
		switch {
		case usable && citationGrammarIssue(g) != "":
			issues = append(issues, citationGrammarIssue(g))
		case usable:
			gs = append(gs, g)
		case g.Issue != "":
			issues = append(issues, "grammar "+name+" is disabled: "+g.Issue)
		default:
			issues = append(issues, "grammar "+name+" is not declared in the ontology")
		}
	}
	return gs, class, issues
}
