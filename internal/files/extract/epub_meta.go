package extract

import (
	"bytes"
	"context"
	"path"
	"strconv"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// epubMetadata reads the package's Dublin Core metadata, with the EPUB 3
// refinements (<meta refines="#id" property="role">) and Calibre's series
// meta. Values are taken as written; Clean bounds them.
func epubMetadata(ctx context.Context, md *node) BookMetadata {
	var m BookMetadata
	if md == nil {
		return m
	}
	// refines["#id"]["role"] = "trl": EPUB 3 says in a separate <meta>
	// what EPUB 2 said in an attribute.
	refines := map[string]map[string]string{}
	for _, k := range md.kidsNamed("meta") {
		if r := strings.TrimSpace(k.attr("refines")); r != "" && k.attr("property") != "" {
			if refines[r] == nil {
				refines[r] = map[string]string{}
			}
			p := k.attr("property")
			if _, seen := refines[r][p]; !seen {
				refines[r][p] = strings.TrimSpace(k.text)
			}
		}
	}
	refined := func(n *node, prop string) string {
		if id := n.attr("id"); id != "" {
			return refines["#"+id][prop]
		}
		return ""
	}

	for _, k := range md.kidsNamed("title") {
		t := strings.TrimSpace(k.text)
		if t == "" {
			continue
		}
		// EPUB 3 may list several titles; the one refined as "main" wins.
		if m.Title == "" || refined(k, "title-type") == "main" {
			m.Title = t
		}
	}
	people := func(kind string, authorsToo bool) {
		for _, k := range md.kidsNamed(kind) {
			name := strings.TrimSpace(k.text)
			if name == "" {
				continue
			}
			role := strings.ToLower(strings.TrimSpace(k.attr("role")))
			if role == "" {
				role = strings.ToLower(refined(k, "role"))
			}
			switch {
			case role == "trl":
				m.Translators = append(m.Translators, name)
			case authorsToo && (role == "" || role == "aut"):
				m.Authors = append(m.Authors, name)
			}
		}
	}
	people("creator", true)
	people("contributor", false)

	if k := md.child("language"); k != nil {
		m.Language = strings.TrimSpace(k.text)
	}
	// EPUB 2 dates carry an opf:event: the publication date is the book's,
	// a modification date is the file's.
	dateRank := 0
	for _, k := range md.kidsNamed("date") {
		t := strings.TrimSpace(k.text)
		rank := 2
		switch strings.ToLower(k.attr("event")) {
		case "publication", "original-publication":
			rank = 3
		case "modification", "conversion":
			rank = 1
		}
		if t != "" && rank > dateRank {
			m.Date, dateRank = t, rank
		}
	}
	if k := md.child("publisher"); k != nil {
		m.Publisher = strings.TrimSpace(k.text)
	}
	for _, k := range md.kidsNamed("identifier") {
		if id := epubIdentifier(k, refined(k, "identifier-type")); id.Value != "" {
			m.Identifiers = append(m.Identifiers, id)
		}
	}
	for _, k := range md.kidsNamed("subject") {
		m.Subjects = append(m.Subjects, strings.TrimSpace(k.text))
	}
	if k := md.child("description"); k != nil {
		// A description is often HTML, escaped inside the XML: its text
		// is what a catalogue wants, not its tags.
		m.Description = htmlFragmentText(ctx, k.text)
	}
	for _, k := range md.kidsNamed("meta") {
		switch k.attr("name") {
		case "calibre:series":
			m.Series = k.attr("content")
		case "calibre:series_index":
			m.SeriesIndex = k.attr("content")
		}
		if k.attr("property") == "belongs-to-collection" && k.attr("refines") == "" {
			kind := refined(k, "collection-type")
			if m.Series == "" || kind == "series" {
				m.Series = strings.TrimSpace(k.text)
				m.SeriesIndex = refined(k, "group-position")
			}
		}
	}
	return m
}

// epubIdentifier is one dc:identifier: its scheme from opf:scheme, an EPUB 3
// identifier-type refinement, or a urn:isbn: / urn:uuid: prefix.
func epubIdentifier(k *node, refinedScheme string) BookIdentifier {
	v := strings.TrimSpace(k.text)
	scheme := strings.ToLower(strings.TrimSpace(k.attr("scheme")))
	if scheme == "" {
		scheme = strings.ToLower(refinedScheme)
	}
	lower := strings.ToLower(v)
	for _, p := range []string{"isbn", "uuid", "asin", "doi"} {
		for _, prefix := range []string{"urn:" + p + ":", p + ":"} {
			if strings.HasPrefix(lower, prefix) {
				return BookIdentifier{Scheme: p, Value: strings.TrimSpace(v[len(prefix):])}
			}
		}
	}
	// ONIX code list 5 numbers in an EPUB 3 refinement: 15 is ISBN-13.
	if scheme == "15" || scheme == "02" {
		scheme = "isbn"
	}
	return BookIdentifier{Scheme: scheme, Value: v}
}

// maxDescriptionHTML caps the description markup parsed as HTML: a blurb
// is a few KB, and Clean keeps MaxMetaDescription characters of its text.
const maxDescriptionHTML = MaxMetaDescription * 4

// htmlFragmentText is the text of an HTML fragment, its tags dropped. The
// description is the book's own markup, so it is held to the web page's
// bounds before the parser builds anything: a fragment past
// maxDescriptionHTML bytes, or one countHTML refuses, is not parsed; its
// tags are stripped by stripTags instead.
func htmlFragmentText(ctx context.Context, s string) string {
	if !strings.ContainsRune(s, '<') {
		return strings.TrimSpace(s)
	}
	if len(s) > maxDescriptionHTML || countHTML(ctx, &ctxReader{ctx: ctx, r: strings.NewReader(s)}) != nil {
		return stripTags(s)
	}
	nodes, err := html.ParseFragment(&ctxReader{ctx: ctx, r: strings.NewReader(s)}, &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div})
	if err != nil {
		return stripTags(s)
	}
	var b strings.Builder
	for _, n := range nodes {
		b.WriteString(textOf(n))
		b.WriteString(" ")
	}
	return strings.TrimSpace(b.String())
}

// stripTags drops everything from a "<" to the next ">", in one pass and
// without building anything; only the first MaxMetaDescription*2 bytes of
// text are kept, more than Clean keeps. Entities stay as written.
func stripTags(s string) string {
	var b strings.Builder
	in := false
	for i := 0; i < len(s) && b.Len() < MaxMetaDescription*2; i++ {
		switch c := s[i]; {
		case c == '<':
			in = true
		case c == '>' && in:
			in = false
			b.WriteByte(' ')
		case !in:
			b.WriteByte(c)
		}
	}
	return strings.TrimSpace(b.String())
}

// navLabels maps each document of the book to the label its contents entry
// gives it: EPUB 3's nav document (nav epub:type="toc"), else EPUB 2's NCX.
// The first entry that points at a document names it. At most maxEPUBNav
// entries are read, maxEPUBNavDepth deep. A contents file that cannot be
// read gives no labels; the book is read without them.
func (b *epubBook) navLabels(pkg *node) map[string]string {
	labels := map[string]string{}
	var navPath, ncxPath string
	for _, it := range b.manifest {
		if it.path == "" {
			continue
		}
		if navPath == "" && hasToken(it.properties, "nav") {
			navPath = it.path
		}
		if ncxPath == "" && it.mediaType == "application/x-dtbncx+xml" {
			ncxPath = it.path
		}
	}
	if toc := pkg.child("spine").attr("toc"); toc != "" {
		if it, ok := b.items[toc]; ok && it.path != "" {
			ncxPath = it.path
		}
	}
	entries := 0
	add := func(dir, href, label string) bool {
		if entries++; entries > maxEPUBNav {
			return false
		}
		label = oneLine(label)
		if p, ok := epubResolve(dir, href); ok && label != "" {
			if _, seen := labels[p]; !seen {
				labels[p] = label
			}
		}
		return true
	}
	if navPath != "" {
		if data, err := b.a.readMax(navPath, maxHTMLBytes); err == nil {
			if doc, err := parseHTMLPage(b.a.ctx, bytes.NewReader(data), int64(len(data))); err == nil {
				if nav := findNavTOC(doc, 0); nav != nil {
					navList(nav, dirOf(navPath), add, 0)
					return labels
				}
			}
		}
	}
	if ncxPath != "" {
		if root, err := b.a.readXML(ncxPath); err == nil {
			ncxPoints(root.path("ncx", "navMap"), dirOf(ncxPath), add, 0)
		}
	}
	return labels
}

// dirOf is the folder of a path inside the book, "" at its root.
func dirOf(p string) string {
	if d := path.Dir(p); d != "." {
		return d
	}
	return ""
}

// findNavTOC is the <nav epub:type="toc"> of a nav document.
func findNavTOC(n *html.Node, depth int) *html.Node {
	if depth > maxXMLDepth {
		return nil
	}
	if n.Type == html.ElementNode && n.Data == "nav" && hasToken(epubType(n), "toc") {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if f := findNavTOC(c, depth+1); f != nil {
			return f
		}
	}
	return nil
}

// navList walks a nav's nested <ol>s, each <li>'s <a> an entry.
func navList(n *html.Node, dir string, add func(dir, href, label string) bool, depth int) bool {
	if depth > maxEPUBNavDepth*4 {
		return true
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode {
			continue
		}
		if c.Data == "a" {
			if !add(dir, attrOf(c, "href"), textOf(c)) {
				return false
			}
			continue
		}
		// An <ol> is one level of the contents, and an <li> holds one;
		// other elements only wrap them.
		if !navList(c, dir, add, depth+1) {
			return false
		}
	}
	return true
}

// ncxPoints walks an NCX navMap's nested navPoints.
func ncxPoints(n *node, dir string, add func(dir, href, label string) bool, depth int) bool {
	if n == nil || depth >= maxEPUBNavDepth {
		return true
	}
	for _, p := range n.kidsNamed("navPoint") {
		label := ""
		if l := p.path("navLabel", "text"); l != nil {
			label = l.text
		}
		if c := p.child("content"); c != nil {
			if !add(dir, c.attr("src"), label) {
				return false
			}
		}
		if !ncxPoints(p, dir, add, depth+1) {
			return false
		}
	}
	return true
}

// epubType is an element's epub:type: the HTML parser keeps the prefixed
// name as the attribute's key.
func epubType(n *html.Node) string {
	for _, a := range n.Attr {
		if a.Key == "epub:type" || (a.Namespace == "epub" && a.Key == "type") {
			return a.Val
		}
	}
	return ""
}

// epubNotes are the book's footnotes: each note is numbered by its first
// reference in reading order (or, unreferenced, when it is found), its
// text kept for the Notes section. A note's key is its document's path and
// its element id.
type epubNotes struct {
	num  map[string]int
	text map[int]string
}

// maxEPUBNotes bounds the notes one book numbers.
const maxEPUBNotes = 100000

func isNoteType(t string) bool {
	return hasToken(t, "footnote") || hasToken(t, "endnote") || hasToken(t, "rearnote")
}

func isNotesType(t string) bool {
	return hasToken(t, "footnotes") || hasToken(t, "endnotes") || hasToken(t, "rearnotes")
}

// number is the note key's number, given one if it has none yet; 0 past
// maxEPUBNotes.
func (e *epubNotes) number(key string) int {
	if n, ok := e.num[key]; ok {
		return n
	}
	if len(e.num) >= maxEPUBNotes {
		return 0
	}
	n := len(e.num) + 1
	e.num[key] = n
	return n
}

// register numbers the notes a document refers to, in the order it refers
// to them, before the document is walked: a note placed before its
// reference is then still known as one.
func (e *epubNotes) register(doc *html.Node, docPath string) {
	budget := maxXMLNodes
	var walk func(n *html.Node, depth int)
	walk = func(n *html.Node, depth int) {
		if depth > maxXMLDepth {
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if budget--; budget < 0 {
				return
			}
			if c.Type != html.ElementNode {
				continue
			}
			if c.Data == "a" && hasToken(epubType(c), "noteref") {
				if key := noteKey(docPath, attrOf(c, "href")); key != "" {
					e.number(key)
				}
			}
			walk(c, depth+1)
		}
	}
	walk(doc, 0)
}

// noteKey is the key of the note an href points at: "doc#id".
func noteKey(docPath, href string) string {
	href = strings.TrimSpace(href)
	i := strings.IndexByte(href, '#')
	if i < 0 || i == len(href)-1 {
		return ""
	}
	target := docPath
	if i > 0 {
		p, ok := epubResolve(dirOf(docPath), href[:i])
		if !ok {
			return ""
		}
		target = p
	}
	return target + "#" + href[i+1:]
}

// ref is "[n]" for a link to a note, "" for any other link.
func (e *epubNotes) ref(a *html.Node, docPath string) string {
	if !hasToken(epubType(a), "noteref") {
		return ""
	}
	key := noteKey(docPath, attrOf(a, "href"))
	if key == "" {
		return ""
	}
	if n := e.number(key); n > 0 {
		return "[" + strconv.Itoa(n) + "]"
	}
	return ""
}

// take takes a note out of the flow and keeps its text: an element typed
// footnote, endnote or rearnote, an <aside> a reference points at, or every
// note in a footnotes / endnotes section.
func (e *epubNotes) take(el *html.Node, docPath string) bool {
	if el.Type != html.ElementNode {
		return false
	}
	t := epubType(el)
	if isNotesType(t) {
		e.collect(el, docPath, 0)
		return true
	}
	id := attrOf(el, "id")
	key := ""
	if id != "" {
		key = docPath + "#" + id
	}
	_, referred := e.num[key]
	if !isNoteType(t) && (el.Data != "aside" || key == "" || !referred) {
		return false
	}
	e.keep(key, el)
	return true
}

// collect keeps the notes of a footnotes / endnotes section: the elements
// typed as notes, or those a reference points at.
func (e *epubNotes) collect(n *html.Node, docPath string, depth int) {
	if depth > maxXMLDepth {
		return
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode {
			continue
		}
		key := ""
		if id := attrOf(c, "id"); id != "" {
			key = docPath + "#" + id
		}
		if _, referred := e.num[key]; isNoteType(epubType(c)) || (key != "" && referred) {
			e.keep(key, c)
			continue
		}
		e.collect(c, docPath, depth+1)
	}
}

// keep stores a note's text under its number; a note with no id is
// numbered when it is found.
func (e *epubNotes) keep(key string, el *html.Node) {
	if key == "" {
		key = "#" + strconv.Itoa(len(e.num)+1) + "\x00"
	}
	n := e.number(key)
	if n == 0 {
		return
	}
	if _, ok := e.text[n]; !ok {
		e.text[n] = oneLine(textOf(el))
	}
}
