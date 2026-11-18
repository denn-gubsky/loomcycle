package memory

// scheme_skos.go — a SKOS concept scheme in RDF/XML, read into a SchemeProposal (RFC GM
// P1.4, import_scheme). Standard library only (encoding/xml). Pure — no I/O.
//
// What is read: skos:ConceptScheme and skos:Concept nodes (as typed elements or as
// rdf:Description with rdf:type), nested node elements included; prefLabel, altLabel,
// definition, scopeNote, notation, broader, narrower, topConceptOf, hasTopConcept,
// inScheme and exactMatch. Everything else is skipped. Turtle and other syntaxes are not
// read.
//
// SAFE ON HOSTILE INPUT. encoding/xml expands no external entity and no entity a DTD
// declares: a reference to one is a syntax error, and a DOCTYPE is refused outright
// here, so a billion-laughs document fails before it expands anything. Nesting is
// bounded, and the caller bounds the input's size.

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

const (
	rdfNS  = "http://www.w3.org/1999/02/22-rdf-syntax-ns#"
	skosNS = "http://www.w3.org/2004/02/skos/core#"
	xmlNS  = "http://www.w3.org/XML/1998/namespace"
	// skosMaxNesting bounds how deep node and property elements may nest.
	skosMaxNesting = 64
)

type langText struct{ lang, text string }

type skosNode struct {
	id                                       string
	order                                    int
	concept, scheme                          bool
	pref, alt, def, note                     []langText
	notation                                 []string
	broader, narrower, topOf, hasTop, exacts []string
	inScheme                                 []string
}

type skosReader struct {
	dec   *xml.Decoder
	nodes map[string]*skosNode
	blank int
}

// ParseSKOS reads a SKOS RDF/XML document. lang picks the labels and notes: a value
// tagged with it (or a regional variant of it, en for en-GB) first, then an untagged one;
// a label in another language is used only when a concept has none of those, with a
// warning. scheme picks one skos:ConceptScheme by IRI or label when the file holds more
// than one. The proposal is not Checked here; its header is left to the caller.
func ParseSKOS(data []byte, lang, scheme string) (SchemeProposal, []string, error) {
	r := &skosReader{dec: xml.NewDecoder(bytes.NewReader(data)), nodes: map[string]*skosNode{}}
	r.dec.Strict = true
	if err := r.read(); err != nil {
		return SchemeProposal{}, nil, err
	}
	return r.proposal(lang, scheme)
}

func (r *skosReader) read() error {
	for {
		tok, err := r.dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("the SKOS file is not well-formed XML: %w", err)
		}
		switch t := tok.(type) {
		case xml.Directive:
			return fmt.Errorf("the SKOS file has a DOCTYPE or other declaration, which is not accepted")
		case xml.StartElement:
			if t.Name.Space == rdfNS && t.Name.Local == "RDF" {
				if err := r.nodeList(attrLang(t, ""), 1); err != nil {
					return err
				}
				continue
			}
			if _, err := r.node(t, "", 1); err != nil {
				return err
			}
		}
	}
}

// nodeList reads the node elements inside rdf:RDF up to its end.
func (r *skosReader) nodeList(lang string, depth int) error {
	for {
		tok, err := r.next()
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if _, err := r.node(t, lang, depth+1); err != nil {
				return err
			}
		case xml.EndElement:
			return nil
		}
	}
}

func (r *skosReader) next() (xml.Token, error) {
	tok, err := r.dec.Token()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("the SKOS file ends inside an element")
		}
		return nil, fmt.Errorf("the SKOS file is not well-formed XML: %w", err)
	}
	if _, ok := tok.(xml.Directive); ok {
		return nil, fmt.Errorf("the SKOS file has a DOCTYPE or other declaration, which is not accepted")
	}
	return tok, nil
}

// node reads one node element and its properties, returning its id.
func (r *skosReader) node(start xml.StartElement, lang string, depth int) (string, error) {
	if depth > skosMaxNesting {
		return "", fmt.Errorf("the SKOS file nests elements more than %d deep", skosMaxNesting)
	}
	lang = attrLang(start, lang)
	id := ""
	for _, a := range start.Attr {
		if a.Name.Space != rdfNS {
			continue
		}
		switch a.Name.Local {
		case "about":
			id = a.Value
		case "nodeID":
			id = "_:" + a.Value
		case "ID":
			id = "#" + a.Value
		}
	}
	if id == "" {
		r.blank++
		id = fmt.Sprintf("_:b%d", r.blank)
	}
	n := r.nodes[id]
	if n == nil {
		n = &skosNode{id: id, order: len(r.nodes)}
		r.nodes[id] = n
	}
	if start.Name.Space == skosNS {
		n.concept = n.concept || start.Name.Local == "Concept"
		n.scheme = n.scheme || start.Name.Local == "ConceptScheme"
	}
	for {
		tok, err := r.next()
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if err := r.property(n, t, lang, depth+1); err != nil {
				return "", err
			}
		case xml.EndElement:
			return id, nil
		}
	}
}

// property reads one property element of n: an object (rdf:resource, rdf:nodeID, or a
// nested node) or a literal.
func (r *skosReader) property(n *skosNode, el xml.StartElement, lang string, depth int) error {
	if depth > skosMaxNesting {
		return fmt.Errorf("the SKOS file nests elements more than %d deep", skosMaxNesting)
	}
	lang = attrLang(el, lang)
	object := ""
	for _, a := range el.Attr {
		if a.Name.Space == rdfNS && a.Name.Local == "resource" {
			object = a.Value
		}
		if a.Name.Space == rdfNS && a.Name.Local == "nodeID" {
			object = "_:" + a.Value
		}
	}
	var text strings.Builder
	for done := false; !done; {
		tok, err := r.next()
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.CharData:
			text.Write(t)
		case xml.StartElement:
			id, err := r.node(t, lang, depth+1)
			if err != nil {
				return err
			}
			object = id
		case xml.EndElement:
			done = true
		}
	}
	lit := langText{lang: lang, text: strings.TrimSpace(text.String())}
	if el.Name.Space == rdfNS && el.Name.Local == "type" {
		n.concept = n.concept || object == skosNS+"Concept"
		n.scheme = n.scheme || object == skosNS+"ConceptScheme"
		return nil
	}
	if el.Name.Space != skosNS {
		return nil
	}
	addLit := func(to *[]langText) {
		if object == "" && lit.text != "" {
			*to = append(*to, lit)
		}
	}
	addObj := func(to *[]string) {
		if object != "" {
			*to = append(*to, object)
		}
	}
	switch el.Name.Local {
	case "prefLabel":
		addLit(&n.pref)
	case "altLabel":
		addLit(&n.alt)
	case "definition":
		addLit(&n.def)
	case "scopeNote":
		addLit(&n.note)
	case "notation":
		if object == "" && lit.text != "" {
			n.notation = append(n.notation, lit.text)
		}
	case "broader":
		addObj(&n.broader)
	case "narrower":
		addObj(&n.narrower)
	case "topConceptOf":
		addObj(&n.topOf)
	case "hasTopConcept":
		addObj(&n.hasTop)
	case "inScheme":
		addObj(&n.inScheme)
	case "exactMatch":
		addObj(&n.exacts)
	}
	return nil
}

func attrLang(el xml.StartElement, inherited string) string {
	for _, a := range el.Attr {
		if a.Name.Space == xmlNS && a.Name.Local == "lang" {
			return a.Value
		}
	}
	return inherited
}

// skosWarnings counts each kind of loss once, with a few examples, so a scheme of 500
// concepts does not answer with 500 lines.
type skosWarnings struct {
	order    []string
	count    map[string]int
	examples map[string][]string
}

func (w *skosWarnings) add(kind, example string) {
	if w.count == nil {
		w.count, w.examples = map[string]int{}, map[string][]string{}
	}
	if w.count[kind] == 0 {
		w.order = append(w.order, kind)
	}
	w.count[kind]++
	if len(w.examples[kind]) < 3 {
		w.examples[kind] = append(w.examples[kind], example)
	}
}

func (w *skosWarnings) list() []string {
	var out []string
	for _, k := range w.order {
		out = append(out, fmt.Sprintf("%d concept(s) %s (e.g. %s)", w.count[k], k, strings.Join(w.examples[k], "; ")))
	}
	return out
}

func (r *skosReader) proposal(lang, want string) (SchemeProposal, []string, error) {
	var w skosWarnings
	var schemes, concepts []*skosNode
	for _, n := range r.nodes {
		if n.scheme {
			schemes = append(schemes, n)
		}
		if n.concept {
			concepts = append(concepts, n)
		}
	}
	byOrder := func(ns []*skosNode) {
		sort.Slice(ns, func(i, j int) bool { return ns[i].order < ns[j].order })
	}
	byOrder(schemes)
	byOrder(concepts)

	var s *skosNode
	switch {
	case want != "":
		for _, sc := range schemes {
			if sc.id == want || anyLabel(sc.pref, want) {
				s = sc
			}
		}
		if s == nil {
			return SchemeProposal{}, nil, fmt.Errorf("the SKOS file has no concept scheme %q (it has: %s)", want, schemeNames(schemes))
		}
	case len(schemes) == 1:
		s = schemes[0]
	case len(schemes) > 1:
		return SchemeProposal{}, nil, fmt.Errorf("the SKOS file holds %d concept schemes (%s); pass scheme to pick one",
			len(schemes), schemeNames(schemes))
	}
	if len(concepts) == 0 {
		return SchemeProposal{}, nil, fmt.Errorf("the SKOS file has no skos:Concept")
	}
	// parentsOf lists each concept's broader concepts as the file states them, both
	// ways (broader on the child, narrower on the parent), in the file's order.
	parentsOf := map[string][]string{}
	for _, c := range concepts {
		for _, b := range c.broader {
			if b != c.id {
				parentsOf[c.id] = appendNew(parentsOf[c.id], b)
			}
		}
		for _, k := range c.narrower {
			if k != c.id {
				parentsOf[k] = appendNew(parentsOf[k], c.id)
			}
		}
	}
	in := r.members(s, concepts, parentsOf, len(schemes) > 1)
	if len(in) > SchemeProposalMaxConcepts {
		return SchemeProposal{}, nil, fmt.Errorf("%w: the scheme has %d concepts, over the limit of %d per proposal",
			ErrSchemeTooLarge, len(in), SchemeProposalMaxConcepts)
	}
	parent := map[string]string{}
	for _, c := range concepts {
		if !in[c.id] {
			continue
		}
		var cands []string
		for _, p := range parentsOf[c.id] {
			if in[p] {
				cands = append(cands, p)
			}
		}
		if len(cands) > 0 {
			parent[c.id] = cands[0]
			if len(cands) > 1 {
				w.add("have more than one broader concept and were placed under the first", r.label(c, lang, nil))
			}
		}
	}
	kids := map[string][]*skosNode{}
	var roots []*skosNode
	for _, c := range concepts {
		if !in[c.id] {
			continue
		}
		if p, ok := parent[c.id]; ok {
			kids[p] = append(kids[p], c)
		} else {
			roots = append(roots, c)
		}
	}
	placed := map[string]bool{}
	var build func(ns []*skosNode) []ProposedConcept
	build = func(ns []*skosNode) []ProposedConcept {
		var out []ProposedConcept
		for _, n := range ns {
			if placed[n.id] {
				continue
			}
			placed[n.id] = true
			pc := r.concept(n, lang, &w)
			pc.Children = build(kids[n.id])
			out = append(out, pc)
		}
		return out
	}
	p := SchemeProposal{Concepts: build(roots)}
	// A broader chain that loops never reaches a root: each such concept is put at the
	// top level rather than lost.
	for _, c := range concepts {
		if in[c.id] && !placed[c.id] {
			w.add("are in a loop of broader concepts and were put at the top level", r.label(c, lang, nil))
			p.Concepts = append(p.Concepts, build([]*skosNode{c})...)
		}
	}
	if s != nil {
		p.Name = r.label(s, lang, nil)
		p.Meaning = pickText(s.def, lang)
		if p.Meaning == "" {
			p.Meaning = pickText(s.note, lang)
		}
	}
	return p, w.list(), nil
}

// members is the concepts of scheme s. With one scheme (or none) in the file, every
// concept; with several, those declared in s, at its top, or narrower than one that is.
func (r *skosReader) members(s *skosNode, concepts []*skosNode, parentsOf map[string][]string, several bool) map[string]bool {
	in := map[string]bool{}
	for _, c := range concepts {
		if !several || s == nil || containsString(c.inScheme, s.id) || containsString(c.topOf, s.id) || containsString(s.hasTop, c.id) {
			in[c.id] = true
		}
	}
	if !several || s == nil {
		return in
	}
	isConcept := map[string]bool{}
	childrenOf := map[string][]string{}
	for _, c := range concepts {
		isConcept[c.id] = true
		for _, p := range parentsOf[c.id] {
			childrenOf[p] = append(childrenOf[p], c.id)
		}
	}
	queue := make([]string, 0, len(in))
	for id := range in {
		queue = append(queue, id)
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, k := range childrenOf[id] {
			if isConcept[k] && !in[k] {
				in[k] = true
				queue = append(queue, k)
			}
		}
	}
	return in
}

func (r *skosReader) concept(n *skosNode, lang string, w *skosWarnings) ProposedConcept {
	pc := ProposedConcept{Name: r.label(n, lang, w)}
	for _, a := range n.alt {
		if langMatches(a.lang, lang) || a.lang == "" {
			pc.Alt = append(pc.Alt, a.text)
		}
	}
	pc.Meaning = pickText(n.def, lang)
	if pc.Meaning == "" {
		pc.Meaning = pickText(n.note, lang)
	}
	if len(n.notation) > 0 {
		pc.Notation = n.notation[0]
		if len(n.notation) > 1 {
			w.add("have more than one notation; the first was kept", pc.Name)
		}
	}
	if len(n.exacts) > 0 {
		pc.ExactMatch = n.exacts[0]
		if len(n.exacts) > 1 {
			w.add("have more than one exact match; the first was kept", pc.Name)
		}
	}
	return pc
}

// label is n's preferred label in lang, falling back to an untagged one, any one, its
// notation, then the last segment of its IRI. w, when set, records a fallback.
func (r *skosReader) label(n *skosNode, lang string, w *skosWarnings) string {
	if t := pickText(n.pref, lang); t != "" {
		return t
	}
	note := func(kind, v string) string {
		if w != nil {
			w.add(kind, v)
		}
		return v
	}
	if len(n.pref) > 0 {
		return note("have no label in "+lang+"; one in another language was used", n.pref[0].text)
	}
	if len(n.notation) > 0 {
		return note("have no label; the notation was used as the name", n.notation[0])
	}
	id := strings.TrimRight(n.id, "/#")
	if i := strings.LastIndexAny(id, "/#:"); i >= 0 {
		id = id[i+1:]
	}
	return note("have no label or notation; the end of the IRI was used as the name", id)
}

// pickText is the text in lang (or a regional variant of it), else an untagged one, else "".
func pickText(ts []langText, lang string) string {
	for _, t := range ts {
		if langMatches(t.lang, lang) {
			return t.text
		}
	}
	for _, t := range ts {
		if t.lang == "" {
			return t.text
		}
	}
	return ""
}

func langMatches(tag, want string) bool {
	if want == "" || tag == "" {
		return false
	}
	tag, want = strings.ToLower(tag), strings.ToLower(want)
	return tag == want || strings.HasPrefix(tag, want+"-")
}

func anyLabel(ts []langText, v string) bool {
	for _, t := range ts {
		if strings.EqualFold(t.text, v) {
			return true
		}
	}
	return false
}

func schemeNames(ss []*skosNode) string {
	names := make([]string, 0, len(ss))
	for _, s := range ss {
		names = append(names, s.id)
	}
	return strings.Join(names, ", ")
}

func appendNew(xs []string, v string) []string {
	if containsString(xs, v) {
		return xs
	}
	return append(xs, v)
}
