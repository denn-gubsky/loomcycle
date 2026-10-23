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
	d := xml.NewDecoder(bytes.NewReader(data))
	d.Strict = true
	type frame struct {
		n    *node
		text strings.Builder
	}
	root := &node{}
	stack := []*frame{{n: root}}
	count, kept := 0, 0
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
			cur.n.text = cur.text.String()
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
