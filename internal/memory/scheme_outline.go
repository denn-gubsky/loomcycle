package memory

// scheme_outline.go — a concept scheme written as a Markdown outline, read into a
// SchemeProposal (RFC GM P1.4, import_scheme). Pure — no I/O.
//
// The form:
//
//	# Subject headings                      ← the scheme's name (optional; at most one, first)
//	Headings for the library catalogue.     ← the scheme's meaning: text before the first concept
//
//	- `QA` Mathematics                      ← a concept: an optional `notation`, then its name
//	  alt: Maths                            ← lines under a concept: alt, note, exact, notation
//	  note: The science of number and space.
//	  exact: http://id.loc.gov/authorities/classification/QA
//	  - `QA76` Computer science             ← indented deeper: a narrower concept
//	## Physics                              ← ## is a top concept, ### under it, and so on;
//	- `QC1` Mechanics                         bullets after a heading are narrower than it
//
// Any other text under a concept is added to its note. A ``` fence is skipped.

import (
	"fmt"
	"strings"
)

type outlineNode struct {
	c    ProposedConcept
	kids []*outlineNode
}

// ParseSchemeOutline reads an outline. name and meaning are the `#` title and the text
// before the first concept, "" when absent. The proposal is not Checked here.
func ParseSchemeOutline(md string) (SchemeProposal, error) {
	var p SchemeProposal
	var roots []*outlineNode
	var levels []*outlineNode // levels[i] is the last concept seen at level i+1
	var indents []int         // the bullet indents open below the last heading concept
	headingLevel := 0         // the level of the last heading concept, 0 above the first
	var cur *outlineNode
	var meaning []string
	inFence := false

	add := func(lineNo, level int, label string) error {
		notation, name := splitNotation(label)
		if name == "" {
			return fmt.Errorf("line %d: a concept has no name", lineNo)
		}
		if level > len(levels)+1 {
			return fmt.Errorf("line %d: %q is nested more than one level below the concept above it", lineNo, name)
		}
		n := &outlineNode{c: ProposedConcept{Name: name, Notation: notation}}
		levels = levels[:level-1]
		if level == 1 {
			roots = append(roots, n)
		} else {
			parent := levels[level-2]
			parent.kids = append(parent.kids, n)
		}
		levels = append(levels, n)
		cur = n
		return nil
	}

	for i, line := range strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n") {
		lineNo := i + 1
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") {
			inFence = !inFence
			continue
		}
		if inFence || t == "" {
			continue
		}
		if h := headingDepth(t); h > 0 {
			label := strings.TrimSpace(t[h:])
			indents = nil
			if h == 1 {
				if p.Name != "" || len(roots) > 0 {
					return p, fmt.Errorf("line %d: a `#` heading names the scheme, once, before any concept", lineNo)
				}
				p.Name = label
				continue
			}
			if err := add(lineNo, h-1, label); err != nil {
				return p, err
			}
			headingLevel = h - 1
			continue
		}
		if label, ok := bulletLabel(t); ok {
			w := len(line) - len(strings.TrimLeft(line, " \t"))
			for len(indents) > 0 && w < indents[len(indents)-1] {
				indents = indents[:len(indents)-1]
			}
			if len(indents) == 0 || w > indents[len(indents)-1] {
				indents = append(indents, w)
			}
			if err := add(lineNo, headingLevel+len(indents), label); err != nil {
				return p, err
			}
			continue
		}
		if cur == nil {
			meaning = append(meaning, t)
			continue
		}
		outlineAttribute(&cur.c, t)
	}
	if inFence {
		return p, fmt.Errorf("a ``` fence is not closed")
	}
	p.Meaning = strings.Join(meaning, " ")
	p.Concepts = toConcepts(roots)
	return p, nil
}

// outlineAttribute applies one line under a concept: `alt:`, `note:`, `exact:` or
// `notation:`, case ignored; anything else is more of the note.
func outlineAttribute(c *ProposedConcept, t string) {
	if k, v, ok := strings.Cut(t, ":"); ok {
		v = strings.TrimSpace(v)
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "alt":
			c.Alt = append(c.Alt, v)
			return
		case "note":
			c.Meaning = strings.TrimSpace(c.Meaning + " " + v)
			return
		case "exact":
			c.ExactMatch = v
			return
		case "notation":
			c.Notation = v
			return
		}
	}
	c.Meaning = strings.TrimSpace(c.Meaning + " " + t)
}

// headingDepth is the number of leading '#' of an ATX heading (1–6), 0 for any other line.
func headingDepth(t string) int {
	n := 0
	for n < len(t) && t[n] == '#' {
		n++
	}
	if n == 0 || n > 6 || (n < len(t) && t[n] != ' ' && t[n] != '\t') {
		return 0
	}
	return n
}

// bulletLabel is the text of a `- ` or `* ` list item.
func bulletLabel(t string) (string, bool) {
	for _, m := range []string{"- ", "* ", "+ "} {
		if strings.HasPrefix(t, m) {
			return strings.TrimSpace(t[len(m):]), true
		}
	}
	return "", false
}

// splitNotation splits "`QA76` Computer science" into its notation and name.
func splitNotation(label string) (notation, name string) {
	if strings.HasPrefix(label, "`") {
		if j := strings.Index(label[1:], "`"); j >= 0 {
			return strings.TrimSpace(label[1 : 1+j]), strings.TrimSpace(label[2+j:])
		}
	}
	return "", label
}

func toConcepts(ns []*outlineNode) []ProposedConcept {
	out := make([]ProposedConcept, 0, len(ns))
	for _, n := range ns {
		c := n.c
		c.Children = toConcepts(n.kids)
		out = append(out, c)
	}
	return out
}
