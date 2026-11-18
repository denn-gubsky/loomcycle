package extract

import (
	"bytes"
	"context"
	"io"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// EPUB reads an e-book, EPUB 2 or 3 (RFC GV §4.2): a zip of XHTML documents
// in the order its package's spine gives. The book's title is the one "#"
// heading, each document goes through the HTML walker's body-only mode, a
// document with no heading of its own takes its contents label, footnotes
// are listed under "Notes" at the end, and the book's own metadata goes to
// Report.Metadata. A book with encrypted content (DRM) is refused; one whose
// only encrypted files are fonts is read, since fonts are never read.
//
// Every name inside the book is a key into the zip's own index: one that is
// absolute, climbs out of the package or names nothing is skipped, never
// opened as a path.
type EPUB struct{}

const epubMediaType = "application/epub+zip"

// Bounds on a book beyond the zip and XML ones.
const (
	maxEPUBSpine    = 5000
	maxEPUBNav      = 5000
	maxEPUBNavDepth = 16
	// maxEPUBPicture caps one picture read out of the book.
	maxEPUBPicture = 16 << 20
)

func (EPUB) Match(mediaType, name string) bool {
	return matchType(mediaType, name, []string{epubMediaType}, []string{".epub"})
}

func (EPUB) Extract(ctx context.Context, r io.ReaderAt, size int64, sink Sink, opts Options) (Report, error) {
	rep := Report{Format: "epub"}
	a, err := openZip(ctx, r, size, opts.limits)
	if err != nil {
		return rep, err
	}
	if err := epubDRM(a); err != nil {
		return rep, err
	}
	opfPath, err := epubRootfile(a)
	if err != nil {
		return rep, err
	}
	opf, err := a.readXML(opfPath)
	if err != nil {
		return rep, err
	}
	pkg := opf.child("package")
	if pkg == nil {
		return rep, refusedf("the book's package document has no <package>")
	}
	b := &epubBook{a: a, rep: &rep, opts: opts, dir: path.Dir(opfPath), notes: &epubNotes{num: map[string]int{}, text: map[int]string{}}}
	b.readManifest(pkg)
	meta := epubMetadata(ctx, pkg.child("metadata"))
	meta.SourceFormat = "epub"
	rep.Metadata = meta.Clean()
	b.labels = b.navLabels(pkg)

	b.w = newWriter(ctx, sink, opts)
	// The record a loomcycle render keeps in META-INF (RFC GR D21), handed
	// on before any text so the headings can name their anchors. One that
	// cannot be read is no record.
	if _, ok := sink.(RecordSink); ok && a.has(epubRecordName) {
		if rec, err := a.read(epubRecordName); err == nil {
			b.w.record(bytes.TrimSpace(rec))
		} else if IsRefused(err) || ctx.Err() != nil {
			return rep, err
		}
	}
	b.title = title(opts.Name, "Book")
	if rep.Metadata != nil && rep.Metadata.Title != "" {
		b.title = rep.Metadata.Title
	}
	err = b.run(pkg)
	b.noteSkips()
	rep.finish(b.w)
	return rep, err
}

// epubRecordName is where a loomcycle render keeps its record in an EPUB:
// the container's own folder (RFC GR D21).
const epubRecordName = "META-INF/loomcycle.json"

// epubItem is one file the package's manifest lists.
type epubItem struct {
	path       string // resolved inside the zip; "" when it points outside
	mediaType  string
	properties string
}

type epubBook struct {
	a      *archive
	w      *writer
	rep    *Report
	opts   Options
	dir    string // the package document's folder
	title  string
	items  map[string]epubItem // by manifest id
	byFile map[string]epubItem // by resolved path
	// manifest is the items in the order the package lists them.
	manifest []epubItem
	labels   map[string]string // contents label by document path
	notes    *epubNotes
	// cover is the cover picture's path once it has been written, so the
	// cover page that shows it again writes nothing.
	cover string
	// written says text has gone to the sink, so the next block is
	// separated from it.
	written bool
	// What was skipped, counted so a book of many broken parts gives one
	// note each, not one per part.
	skippedManifest, skippedSpine, skippedTypes, unreadable int
}

// noteSkips reports what the book's reading skipped.
func (b *epubBook) noteSkips() {
	if b.skippedManifest > 0 {
		b.rep.note("%d manifest items name a file outside the book, or one it does not hold, and were skipped", b.skippedManifest)
	}
	if b.skippedSpine > 0 {
		b.rep.note("%d spine items name no file of the book and were skipped", b.skippedSpine)
	}
	if b.skippedTypes > 0 {
		b.rep.note("%d spine items that are not XHTML or HTML were skipped", b.skippedTypes)
	}
	if b.unreadable > 0 {
		b.rep.note("%d content documents could not be read and were skipped", b.unreadable)
	}
}

// run writes the book: the title, the cover, the spine's linear documents,
// its non-linear ones under "Supplementary", then the notes. It stops at
// the first block the sink will not take, and reads nothing after it.
func (b *epubBook) run(pkg *node) error {
	if err := b.emit(heading(1, b.title)); err != nil {
		return err
	}
	if err := b.writeCover(pkg); err != nil {
		return err
	}
	spine := pkg.child("spine")
	var linear, extra []string
	refs := spine.kidsNamed("itemref")
	if len(refs) > maxEPUBSpine {
		b.rep.note("only the first %d of the book's %d spine items were read", maxEPUBSpine, len(refs))
		refs = refs[:maxEPUBSpine]
	}
	for _, ref := range refs {
		it, ok := b.items[ref.attr("idref")]
		if !ok || it.path == "" {
			b.skippedSpine++
			continue
		}
		if strings.EqualFold(strings.TrimSpace(ref.attr("linear")), "no") {
			extra = append(extra, it.path)
		} else {
			linear = append(linear, it.path)
		}
	}
	for _, p := range linear {
		if err := b.document(p); err != nil {
			return err
		}
	}
	for i, p := range extra {
		if i == 0 {
			if err := b.emit(heading(2, "Supplementary")); err != nil {
				return err
			}
		}
		if err := b.document(p); err != nil {
			return err
		}
	}
	return b.writeNotes()
}

// emit writes one block.
func (b *epubBook) emit(md string) error {
	if b.written {
		md = "\n\n" + md
	}
	b.written = true
	return b.w.text(md)
}

// document reads one content document of the spine through the HTML
// walker's body-only mode and writes it. A document that cannot be read is
// skipped with a note; the book goes on.
func (b *epubBook) document(p string) error {
	if err := b.w.ctx.Err(); err != nil {
		return err
	}
	it := b.byPath(p)
	if mt := strings.ToLower(it.mediaType); mt != "" && mt != "application/xhtml+xml" && mt != "text/html" {
		b.skippedTypes++
		return nil
	}
	// A refusal (a document past a bound, an entry that inflates past the
	// archive's) refuses the book, as it would any other zip format: the
	// file is hostile, not damaged. Any other failure skips the document.
	data, err := b.a.readMax(p, maxHTMLBytes)
	if err != nil {
		if b.w.ctx.Err() != nil || IsRefused(err) {
			return err
		}
		b.unreadable++
		return nil
	}
	doc, err := parseHTMLPage(b.w.ctx, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		if b.w.ctx.Err() != nil || IsRefused(err) {
			return err
		}
		b.unreadable++
		return nil
	}
	b.notes.register(doc, p)
	h := &htmlState{w: b.w, rep: b.rep, infer: b.opts.InferHeadings, body: &htmlBody{
		title:   b.title,
		label:   b.labels[p],
		picture: func(ref string) ([]byte, bool) { return b.picture(p, ref) },
		noteRef: func(n *html.Node) string { return b.notes.ref(n, p) },
		note:    func(n *html.Node) bool { return b.notes.take(n, p) },
	}}
	if err := h.page(doc, maxCSSWork); err != nil {
		return err
	}
	if len(h.blocks) == 0 {
		return nil
	}
	err = h.write(b.written)
	b.written = true
	return err
}

// byPath is the manifest item of a resolved path, zero when none lists it.
func (b *epubBook) byPath(p string) epubItem { return b.byFile[p] }

// picture reads a picture a document at doc names by ref. Pictures are read
// only when they are kept; an SVG file is never passed on (no image list
// takes SVG: it is markup); the cover, already written, writes nothing.
func (b *epubBook) picture(doc, ref string) ([]byte, bool) {
	if !b.opts.Images {
		return nil, false
	}
	p, ok := epubResolve(path.Dir(doc), ref)
	if !ok || !b.a.has(p) {
		return nil, false
	}
	if p == b.cover {
		return nil, true
	}
	if strings.EqualFold(path.Ext(p), ".svg") || strings.EqualFold(b.byPath(p).mediaType, "image/svg+xml") {
		return nil, false
	}
	data, err := b.a.readMax(p, maxEPUBPicture)
	if err != nil {
		return nil, false
	}
	return data, true
}

// writeCover writes the cover picture under the title: EPUB 3's cover-image
// item, else the item EPUB 2's <meta name="cover"> names.
func (b *epubBook) writeCover(pkg *node) error {
	if !b.opts.Images {
		return nil
	}
	var cover epubItem
	for _, it := range b.manifest {
		if hasToken(it.properties, "cover-image") {
			cover = it
			break
		}
	}
	if cover.path == "" {
		for _, m := range pkg.child("metadata").kidsNamed("meta") {
			if m.attr("name") == "cover" {
				cover = b.items[m.attr("content")]
				break
			}
		}
	}
	if cover.path == "" || strings.EqualFold(path.Ext(cover.path), ".svg") || !b.a.has(cover.path) {
		return nil
	}
	data, err := b.a.readMax(cover.path, maxEPUBPicture)
	if err != nil {
		if b.w.ctx.Err() != nil {
			return err
		}
		return nil
	}
	md, err := b.w.image(data, "Cover")
	if err != nil {
		return err
	}
	b.cover = cover.path
	return b.emit(md)
}

// writeNotes lists the footnotes the book's text referred to, in the order
// of their numbers.
func (b *epubBook) writeNotes() error {
	var nums []int
	for n, t := range b.notes.text {
		if t != "" {
			nums = append(nums, n)
		}
	}
	if len(nums) == 0 {
		return nil
	}
	sort.Ints(nums)
	if err := b.emit(heading(2, "Notes")); err != nil {
		return err
	}
	for _, n := range nums {
		if err := b.emit(paragraph("[" + strconv.Itoa(n) + "] " + b.notes.text[n])); err != nil {
			return err
		}
	}
	return nil
}

// readManifest reads the package's manifest: each item's file resolved
// against the package document's folder.
func (b *epubBook) readManifest(pkg *node) {
	b.items, b.byFile = map[string]epubItem{}, map[string]epubItem{}
	for _, it := range pkg.child("manifest").kidsNamed("item") {
		id := it.attr("id")
		if id == "" {
			continue
		}
		p, ok := epubResolve(b.dir, it.attr("href"))
		if ok && !b.a.has(p) {
			ok = false
		}
		if !ok {
			b.skippedManifest++
			p = ""
		}
		item := epubItem{path: p, mediaType: strings.TrimSpace(it.attr("media-type")), properties: it.attr("properties")}
		b.items[id] = item
		b.manifest = append(b.manifest, item)
		if _, dup := b.byFile[p]; p != "" && !dup {
			b.byFile[p] = item
		}
	}
}

// epubResolve resolves a reference inside the book against dir, the folder
// of the file that makes it: its fragment and query dropped, its
// percent-encoding decoded. A reference with a scheme, an absolute one, or
// one that climbs out of the zip is not resolved.
func epubResolve(dir, ref string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if i := strings.IndexAny(ref, "#?"); i >= 0 {
		ref = ref[:i]
	}
	if ref == "" || hasScheme(ref) || strings.HasPrefix(ref, "/") || strings.Contains(ref, "\\") {
		return "", false
	}
	if u, err := url.PathUnescape(ref); err == nil {
		ref = u
	}
	if strings.HasPrefix(ref, "/") || strings.ContainsRune(ref, 0) {
		return "", false
	}
	if dir == "." {
		dir = ""
	}
	p := path.Clean(path.Join(dir, ref))
	if p == "." || p == ".." || strings.HasPrefix(p, "../") || strings.HasPrefix(p, "/") {
		return "", false
	}
	return p, true
}

// epubRootfile is the package document container.xml names: the first
// rootfile of the OPF type.
func epubRootfile(a *archive) (string, error) {
	if !a.has("META-INF/container.xml") {
		return "", refusedf("the book has no META-INF/container.xml")
	}
	c, err := a.readXML("META-INF/container.xml")
	if err != nil {
		return "", err
	}
	for _, rf := range c.all("rootfile") {
		mt := strings.TrimSpace(rf.attr("media-type"))
		if mt != "" && mt != "application/oebps-package+xml" {
			continue
		}
		p, ok := epubResolve("", rf.attr("full-path"))
		if ok && a.has(p) {
			return p, nil
		}
	}
	return "", refusedf("the book's container names no package document it holds")
}

// Font obfuscation (IDPF, Adobe) only scrambles an embedded font's first
// bytes so it cannot be lifted out of the book; it is not DRM.
var epubFontExts = map[string]bool{".otf": true, ".ttf": true, ".woff": true, ".woff2": true, ".eot": true}

// epubDRM refuses a book whose META-INF/encryption.xml encrypts anything but
// fonts: its text cannot be read without the key (Adobe ADEPT, Readium LCP,
// Apple FairPlay). An encryption.xml that cannot be read refuses the book
// too: what it would have said is unknown.
func epubDRM(a *archive) error {
	if !a.has("META-INF/encryption.xml") {
		return nil
	}
	enc, err := a.readXML("META-INF/encryption.xml")
	if err != nil {
		if a.ctx.Err() != nil {
			return err
		}
		return refusedf("the book is encrypted (DRM): its encryption.xml cannot be read")
	}
	for _, ref := range enc.all("CipherReference") {
		target := strings.TrimSpace(ref.attr("URI"))
		if u, err := url.PathUnescape(target); err == nil {
			target = u
		}
		if !epubFontExts[strings.ToLower(path.Ext(target))] {
			return refusedf("the book is encrypted (DRM)")
		}
	}
	return nil
}

// hasToken reports whether a space-separated attribute value holds tok.
func hasToken(v, tok string) bool {
	for _, f := range strings.Fields(v) {
		if f == tok {
			return true
		}
	}
	return false
}
