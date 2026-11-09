package extract

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
)

// Bounds on a zip archive (DOCX, XLSX, PPTX). An Office file is a zip of XML,
// and a zip can claim, or hold, far more than it weighs: a few kilobytes of
// deflate can expand to gigabytes. Every zip format reads through openZip, so
// the bounds hold for each of them and for any format added later.
const (
	// MaxZipEntries caps the entries an archive may list.
	MaxZipEntries = 10000
	// MaxZipExpansion caps what may be read out of an archive as a multiple
	// of its own size.
	MaxZipExpansion = 8
	// MaxZipUncompressed caps what may be read out of any one archive.
	MaxZipUncompressed = 256 << 20
	// minZipBudget is what any archive may expand to whatever its size. XML
	// compresses ten to twenty times, so a small document alone would fail
	// the 8x bound; 16 MiB of text is no bomb.
	minZipBudget = 16 << 20
	// MaxZipEntryRatio caps how far one entry may expand over its compressed
	// size, past the first maxRatioFree bytes.
	MaxZipEntryRatio = 100
	maxRatioFree     = 1 << 20
	// maxXMLNodes caps the elements one XML part may hold, and maxXMLDepth
	// how deep they may nest.
	maxXMLNodes = 1_000_000
	maxXMLDepth = 256
	// maxXMLAttrs caps the attributes one XML part (or HTML page) may hold,
	// and maxXMLAttrsPerElement those on one element. The decoder builds an
	// element's whole attribute list before it hands the element over, at
	// about ten times the bytes the attributes take in the file, so both are
	// counted on the raw bytes before decoding (countXMLAttrs). Office
	// elements carry a handful; a root with every namespace declared, a few
	// dozen.
	maxXMLAttrs           = 2_000_000
	maxXMLAttrsPerElement = 1024
	// maxXMLAttrBytes caps the bytes of attribute values a part's tree keeps,
	// as maxXMLText caps its character data.
	maxXMLAttrBytes = 64 << 20
	// maxXMLText caps the character data kept from one XML part.
	maxXMLText = 64 << 20
)

// zipLimits are the bounds above; tests use smaller ones.
type zipLimits struct {
	entries   int
	expansion int64
	maxTotal  int64
	floor     int64
	ratio     int64
	ratioFree int64
}

var defaultZipLimits = zipLimits{
	entries: MaxZipEntries, expansion: MaxZipExpansion, maxTotal: MaxZipUncompressed,
	floor: minZipBudget, ratio: MaxZipEntryRatio, ratioFree: maxRatioFree,
}

// RefusedError is an archive or part the extractor will not read, and why.
type RefusedError struct{ Reason string }

func (e *RefusedError) Error() string { return "extract: refused: " + e.Reason }

func refusedf(format string, args ...any) error {
	return &RefusedError{Reason: fmt.Sprintf(format, args...)}
}

// IsRefused reports whether err is a refusal of the file (as opposed to a
// failure reading it).
func IsRefused(err error) bool {
	var r *RefusedError
	return errors.As(err, &r)
}

// archive is an opened zip with what may still be read out of it. Entries
// are looked up by their name as a key, never opened as a path.
type archive struct {
	ctx     context.Context
	files   map[string]*zip.File
	lower   map[string]*zip.File
	budget  int64
	limits  zipLimits
	partCap int64
}

func openZip(ctx context.Context, r io.ReaderAt, size int64, lim *zipLimits) (*archive, error) {
	l := defaultZipLimits
	if lim != nil {
		l = *lim
	}
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("extract: not a readable zip archive: %w", err)
	}
	if len(zr.File) > l.entries {
		return nil, refusedf("the archive lists %d entries, over the limit of %d", len(zr.File), l.entries)
	}
	budget := min(max(size*l.expansion, l.floor), l.maxTotal)
	a := &archive{ctx: ctx, files: map[string]*zip.File{}, lower: map[string]*zip.File{}, budget: budget, limits: l}
	var declared uint64
	for _, f := range zr.File {
		if _, dup := a.files[f.Name]; dup {
			return nil, refusedf("the archive lists %q twice", f.Name)
		}
		a.files[f.Name] = f
		if _, ok := a.lower[strings.ToLower(f.Name)]; !ok {
			a.lower[strings.ToLower(f.Name)] = f
		}
		declared += f.UncompressedSize64
		if declared > uint64(budget) {
			return nil, refusedf("the archive expands to more than %d bytes (%d× its size, at most %d)", budget, l.expansion, l.maxTotal)
		}
	}
	return a, nil
}

// has reports whether the archive holds name.
func (a *archive) has(name string) bool {
	_, ok := a.lookup(name)
	return ok
}

// lookup finds an entry by its part name. OOXML part names are compared
// without case, so an exact match is tried first and then a folded one.
func (a *archive) lookup(name string) (*zip.File, bool) {
	name = strings.TrimPrefix(name, "/")
	if f, ok := a.files[name]; ok {
		return f, true
	}
	f, ok := a.lower[strings.ToLower(name)]
	return f, ok
}

// read returns an entry's bytes, read under the archive's remaining budget
// and the entry's own expansion bound, both enforced on the bytes as they
// are inflated: the sizes an entry declares are not trusted.
func (a *archive) read(name string) ([]byte, error) {
	if err := a.ctx.Err(); err != nil {
		return nil, err
	}
	f, ok := a.lookup(name)
	if !ok {
		return nil, fmt.Errorf("extract: the archive has no %s", name)
	}
	if f.Mode().IsDir() {
		return nil, fmt.Errorf("extract: %s is a folder", name)
	}
	limit := a.budget
	if byRatio := int64(f.CompressedSize64)*a.limits.ratio + a.limits.ratioFree; byRatio < limit {
		limit = byRatio
	}
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("extract: cannot open %s in the archive: %w", name, err)
	}
	defer rc.Close()
	var buf bytes.Buffer
	n, err := io.Copy(&buf, &ctxReader{ctx: a.ctx, r: io.LimitReader(rc, limit+1)})
	if err != nil {
		if cerr := a.ctx.Err(); cerr != nil {
			return nil, cerr
		}
		return nil, fmt.Errorf("extract: cannot read %s in the archive: %w", name, err)
	}
	if n > limit {
		if limit < a.budget {
			return nil, refusedf("%s expands more than %d times its compressed size", name, a.limits.ratio)
		}
		return nil, refusedf("the archive expands to more than %d bytes", a.budget)
	}
	a.budget -= n
	return buf.Bytes(), nil
}

// readXML reads an entry and parses it into a tree.
func (a *archive) readXML(name string) (*node, error) {
	b, err := a.read(name)
	if err != nil {
		return nil, err
	}
	return parseXML(a.ctx, b)
}

// ctxReader stops a long read when the context ends.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// rel is one relationship of an OOXML part.
type rel struct {
	Type     string
	Target   string
	External bool
}

// rels reads the relationships of part (word/document.xml →
// word/_rels/document.xml.rels), with each internal target resolved to a part
// name. A target is only ever a key into the archive: one that climbs out of
// it ("../../x") resolves to a name the archive does not hold.
func (a *archive) rels(part string) (map[string]rel, error) {
	dir, file := path.Split(part)
	name := dir + "_rels/" + file + ".rels"
	out := map[string]rel{}
	if !a.has(name) {
		return out, nil
	}
	root, err := a.readXML(name)
	if err != nil {
		return nil, err
	}
	for _, r := range root.all("Relationship") {
		id := r.attr("Id")
		if id == "" {
			continue
		}
		t := rel{Type: r.attr("Type"), Target: r.attr("Target"), External: strings.EqualFold(r.attr("TargetMode"), "External")}
		if !t.External {
			t.Target = resolvePart(dir, t.Target)
		}
		out[id] = t
	}
	return out, nil
}

// resolvePart resolves a relationship target against the folder of the part
// that names it. An absolute target is relative to the archive root.
func resolvePart(dir, target string) string {
	if strings.HasPrefix(target, "/") {
		return path.Clean(strings.TrimPrefix(target, "/"))
	}
	return path.Clean(path.Join(dir, target))
}

// relType reports whether a relationship type URI ends in kind ("image",
// "officeDocument", "notesSlide", ...).
func relType(t, kind string) bool {
	return strings.HasSuffix(t, "/"+kind)
}

// imageType sniffs an image's bytes: its media type and extension when it is
// a type a document store and a browser both take, "" otherwise (EMF, WMF and
// TIFF are common in Office files and are left out, not converted).
func imageType(data []byte) (string, string) {
	switch mt := http.DetectContentType(data); mt {
	case "image/png":
		return mt, ".png"
	case "image/jpeg":
		return mt, ".jpg"
	case "image/gif":
		return mt, ".gif"
	case "image/webp":
		return mt, ".webp"
	case "image/bmp":
		return mt, ".bmp"
	}
	return "", ""
}

// node is one element of a parsed XML part. Office text lives in leaf
// elements (w:t, a:t), so a node keeps the character data directly inside it
// and the order of its child elements; text interleaved with children is
// joined.
type node struct {
	name  string // local name
	space string
	attrs []xml.Attr
	kids  []*node
	text  string
}

// parseXML parses an XML part into a tree with encoding/xml in strict mode:
// no DTD is read, an entity other than the five predefined ones is an error,
// and a declared encoding other than UTF-8 is refused (no CharsetReader).
// The element count, the depth and the character data kept are capped.
func parseXML(ctx context.Context, data []byte) (*node, error) {
	return parseXMLCapped(ctx, data, maxXMLText)
}

// parseXMLCapped is parseXML with the character-data cap given. Each open
// element collects its text in a builder of its own, so a part of many
// small elements costs time in proportion to its size.
func parseXMLCapped(ctx context.Context, data []byte, textCap int) (*node, error) {
	return parseXMLTree(ctx, data, textCap, false)
}

// textNode names the pseudo-element that holds one run of character data in
// a tree parsed with parseXMLMixed. No XML element name starts with "#".
const textNode = "#text"

// parseXMLMixed parses a part whose text and elements interleave (ODF:
// <text:p>a<text:s/>b<text:span>c</text:span></text:p>). Each run of
// character data becomes a textNode child in document order, where the plain
// tree would join it into the parent's text apart from its siblings. Same
// decoder and caps as parseXML; text pseudo-nodes count toward the element
// cap.
func parseXMLMixed(ctx context.Context, data []byte) (*node, error) {
	return parseXMLTree(ctx, data, maxXMLText, true)
}

func parseXMLTree(ctx context.Context, data []byte, textCap int, mixed bool) (*node, error) {
	// The tree keeps every element's attributes, so the part's total counts
	// as well as each element's.
	if err := checkXMLAttrs(ctx, data, maxXMLAttrs); err != nil {
		return nil, err
	}
	d := xml.NewDecoder(bytes.NewReader(data))
	d.Strict = true
	type frame struct {
		n    *node
		text strings.Builder
	}
	root := &node{}
	stack := []*frame{{n: root}}
	count, kept := 0, 0
	// flush ends a run of text in mixed mode: the decoder may hand one run
	// over in pieces (CDATA sections), gathered in the frame's builder so a
	// run of many pieces costs time in proportion to its size.
	flush := func(f *frame) error {
		if !mixed || f.text.Len() == 0 {
			return nil
		}
		count++
		if count > maxXMLNodes {
			return refusedf("an XML part holds more than %d elements", maxXMLNodes)
		}
		f.n.kids = append(f.n.kids, &node{name: textNode, text: f.text.String()})
		f.text.Reset()
		return nil
	}
	for i := 0; ; i++ {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("extract: malformed XML: %w", err)
		}
		cur := stack[len(stack)-1]
		switch t := tok.(type) {
		case xml.StartElement:
			if err := flush(cur); err != nil {
				return nil, err
			}
			count++
			if count > maxXMLNodes {
				return nil, refusedf("an XML part holds more than %d elements", maxXMLNodes)
			}
			if len(stack) > maxXMLDepth {
				return nil, refusedf("an XML part nests deeper than %d", maxXMLDepth)
			}
			n := &node{name: t.Name.Local, space: t.Name.Space, attrs: t.Attr}
			cur.n.kids = append(cur.n.kids, n)
			stack = append(stack, &frame{n: n})
		case xml.EndElement:
			if len(stack) == 1 {
				return nil, errors.New("extract: malformed XML: unbalanced end element")
			}
			if mixed {
				if err := flush(cur); err != nil {
					return nil, err
				}
			} else {
				cur.n.text = cur.text.String()
			}
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 1 {
				kept += len(t)
				if kept > textCap {
					return nil, refusedf("an XML part holds more than %d bytes of text", textCap)
				}
				cur.text.Write(t)
			}
		}
	}
	if len(stack) != 1 {
		return nil, errors.New("extract: malformed XML: unclosed element")
	}
	return root, nil
}

// checkXMLAttrs refuses a part with an element of more than
// maxXMLAttrsPerElement attributes, or more than maxTotal in all (0: no total,
// for a streaming parse that keeps no element after it has read it; the
// bytes of attribute values are capped with the total).
func checkXMLAttrs(ctx context.Context, data []byte, maxTotal int) error {
	total, widest, valueBytes, err := countXMLAttrs(ctx, data)
	if err != nil {
		return err
	}
	if widest > maxXMLAttrsPerElement {
		return refusedf("an XML element holds more than %d attributes", maxXMLAttrsPerElement)
	}
	if maxTotal > 0 && total > maxTotal {
		return refusedf("an XML part holds more than %d attributes", maxTotal)
	}
	if maxTotal > 0 && valueBytes > maxXMLAttrBytes {
		return refusedf("an XML part holds more than %d bytes of attribute values", maxXMLAttrBytes)
	}
	return nil
}

// countXMLAttrs counts the attributes of a part's start tags, those of the
// widest one and the bytes of their quoted values, from the raw bytes and
// without allocating: each attribute is a '=' outside quotes inside a start
// tag, since a strict decoder takes no attribute without a quoted value.
//
// It is a second reader of the part, so it must never read less markup than
// encoding/xml does: comments, CDATA sections and processing instructions
// end where the decoder ends them, and a directive (<!DOCTYPE …>) is skipped
// by the decoder's own rules (xml.go, rawToken: quotes and nested <…> counted,
// a <!--…--> comment inside it skipped before any quote is honoured). And it
// fails closed: a construct it cannot see the end of is refused, never
// counted low. The decoder refuses such a part as malformed too, so nothing it
// would accept is lost. Only a start tag can carry attributes; an end tag or a
// malformed tag can only make the count come out high.
func countXMLAttrs(ctx context.Context, data []byte) (total, widest, valueBytes int, err error) {
	unterminated := refusedf("an XML part ends inside markup")
	// skipPast moves i past the first sep at or after it, or reports that the
	// part ends first.
	skipPast := func(i int, sep string) (int, bool) {
		if k := bytes.Index(data[i:], []byte(sep)); k >= 0 {
			return i + k + len(sep), true
		}
		return 0, false
	}
	nextCheck := 0
	for i := 0; i < len(data); {
		if i >= nextCheck {
			if err := ctx.Err(); err != nil {
				return 0, 0, 0, err
			}
			nextCheck = i + 1<<20
		}
		k := bytes.IndexByte(data[i:], '<')
		if k < 0 {
			break
		}
		i += k + 1
		rest := data[i:]
		ok := true
		switch {
		case bytes.HasPrefix(rest, []byte("!--")):
			i, ok = skipPast(i+3, "-->")
		case bytes.HasPrefix(rest, []byte("![CDATA[")):
			i, ok = skipPast(i+8, "]]>")
		case bytes.HasPrefix(rest, []byte("?")):
			i, ok = skipPast(i+1, "?>")
		case bytes.HasPrefix(rest, []byte("!")):
			i, ok = skipXMLDirective(data, i+1)
		case bytes.HasPrefix(rest, []byte("/")):
			i, ok = skipPast(i, ">")
		default:
			n := 0
			closed := false
		tag:
			for ; i < len(data); i++ {
				switch c := data[i]; c {
				case '"', '\'':
					q := bytes.IndexByte(data[i+1:], c)
					if q < 0 {
						break tag
					}
					valueBytes += q
					i += q + 1
				case '=':
					n++
				case '>':
					i++
					closed = true
					break tag
				}
			}
			total += n
			widest = max(widest, n)
			ok = closed
		}
		if !ok {
			return 0, 0, 0, unterminated
		}
	}
	return total, widest, valueBytes, nil
}

// skipXMLDirective skips a directive whose "<!" ends just before data[i], the
// way encoding/xml's rawToken reads one: the first byte is taken as it is;
// then quotes, nested <…> and <!--…--> comments are tracked, a comment being
// recognised before a quote inside it could open a string. It returns where
// the directive ends, or false when the part ends first.
func skipXMLDirective(data []byte, i int) (int, bool) {
	if i >= len(data) {
		return 0, false
	}
	i++ // the decoder takes the byte after "<!" without looking at it
	var inquote byte
	depth := 0
	for {
		if i >= len(data) {
			return 0, false
		}
		b := data[i]
		i++
		if inquote == 0 && b == '>' && depth == 0 {
			return i, true
		}
	handle:
		switch {
		case b == inquote:
			inquote = 0
		case inquote != 0:
		case b == '\'' || b == '"':
			inquote = b
		case b == '>':
			depth--
		case b == '<':
			const open = "!--"
			for j := 0; j < len(open); j++ {
				if i >= len(data) {
					return 0, false
				}
				b = data[i]
				i++
				if b != open[j] {
					depth++
					goto handle
				}
			}
			k := bytes.Index(data[i:], []byte("-->"))
			if k < 0 {
				return 0, false
			}
			i += k + len("-->")
		}
	}
}

func (n *node) attr(local string) string {
	if n == nil {
		return ""
	}
	for _, a := range n.attrs {
		if a.Name.Local == local {
			return a.Value
		}
	}
	return ""
}

// attrRel is the relationship attribute local (r:embed, r:id): its namespace
// is the transitional or the strict relationships URI, or an undeclared "r".
func (n *node) attrRel(local string) string {
	if n == nil {
		return ""
	}
	for _, a := range n.attrs {
		if a.Name.Local == local && (strings.Contains(a.Name.Space, "relationships") || a.Name.Space == "r") {
			return a.Value
		}
	}
	return ""
}

// child returns the first child element called local.
func (n *node) child(local string) *node {
	if n == nil {
		return nil
	}
	for _, k := range n.kids {
		if k.name == local {
			return k
		}
	}
	return nil
}

// all returns every descendant element called local, in document order.
func (n *node) all(local string) []*node {
	var out []*node
	var walk func(*node, int)
	walk = func(m *node, depth int) {
		if depth > maxXMLDepth {
			return
		}
		for _, k := range m.kids {
			if k.name == local {
				out = append(out, k)
			}
			walk(k, depth+1)
		}
	}
	if n != nil {
		walk(n, 0)
	}
	return out
}

// path follows a chain of child element names.
func (n *node) path(names ...string) *node {
	for _, name := range names {
		n = n.child(name)
	}
	return n
}
