package extract

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// FB2 reads a FictionBook 2 book (RFC GV §4.3), plain or as a zip of one
// .fb2 file: one XML file whose description carries the metadata, whose
// first unnamed body is the book and whose binaries hold its pictures as
// base64. The book is read as a stream, never as a tree: a book cut at its
// output limit stops reading there, and nothing past the cut is parsed.
//
// The description comes first, so the metadata is known before any text is
// written. The notes body and the binaries usually come after the book's
// text, so a note reference is written [n] as it is reached and the notes it
// named are listed under "Notes" at the end, and a picture whose bytes have
// not been seen yet is written "[picture n]" where it stands and placed
// under "Pictures" when its binary is reached. Only that keeps the main text
// streaming; buffering it until the binaries came would hold the whole book.
// A binary that comes before its reference (rare) is held, under the
// picture caps, and placed where it is referenced.
type FB2 struct{}

const (
	fb2MediaType    = "application/x-fictionbook+xml"
	fb2ZipMediaType = "application/x-zip-compressed-fb2"
)

// Bounds on a book beyond the XML ones.
const (
	// maxFB2Bytes caps the book's XML (an FB2 with pictures is rarely past
	// 30 MB).
	maxFB2Bytes = 64 << 20
	// maxFB2Picture caps one picture decoded, maxFB2PictureBytes all of
	// them, and maxFB2Pictures their number.
	maxFB2Picture      = 16 << 20
	maxFB2PictureBytes = 64 << 20
	maxFB2Pictures     = 500
	// maxFB2Note caps the text kept of one note, maxFB2NoteBytes of all.
	maxFB2Note      = 64 << 10
	maxFB2NoteBytes = 8 << 20
	// maxFB2MetaText caps the text read for one metadata value, and
	// maxFB2Annotation for the annotation: Clean cuts them further, this
	// only stops a value of megabytes being built first.
	maxFB2MetaText   = MaxMetaString * 4
	maxFB2Annotation = MaxMetaDescription * 4
	// maxFB2Decoded caps the text a legacy encoding or UTF-16 decodes to:
	// a byte of windows-1251 is up to three of UTF-8, so the file cap alone
	// does not bound it.
	maxFB2Decoded = 3 * maxFB2Bytes
	// maxFB2Span caps one run of the stream between markup boundaries (a
	// text node, a tag, a comment) as it streams in: the decoder builds each
	// into one token, so a single node of most of the file would cost its
	// decoded size at once. A paragraph is kilobytes.
	maxFB2Span = 4 << 20
	// maxFB2Quotes caps the block quote markers written before a line:
	// deeper nesting is read, and written at this depth, so a prefix never
	// multiplies a line by the nesting of a hostile book.
	maxFB2Quotes = 8
	// maxFB2Para is the text one paragraph collects before what it holds
	// is written as a paragraph of its own; maxFB2Line caps a heading, a
	// verse line or an attribution, which are kept whole on one line.
	maxFB2Para = 1 << 20
	maxFB2Line = 64 << 10
)

func (FB2) Match(mediaType, name string) bool {
	if matchType(mediaType, name, []string{fb2MediaType, fb2ZipMediaType}, []string{".fb2"}) {
		return true
	}
	// path.Ext of "book.fb2.zip" is ".zip".
	return strings.HasSuffix(strings.ToLower(name), ".fb2.zip")
}

func (FB2) Extract(ctx context.Context, r io.ReaderAt, size int64, sink Sink, opts Options) (Report, error) {
	rep := Report{Format: "fb2"}
	src, err := fb2Source(ctx, r, size, opts)
	if err != nil {
		return rep, err
	}
	defer src.Close()
	maxPic := maxFB2Picture
	if opts.fb2MaxPicture > 0 {
		maxPic = opts.fb2MaxPicture
	}
	b := &fb2Book{
		w: newWriter(ctx, sink, opts), rep: &rep, opts: opts, maxPicture: maxPic,
		title:   title(opts.Name, "Book"),
		noteNum: map[string]int{}, noteText: map[string]string{},
		pics: map[string]*fb2Pic{}, held: map[string][]byte{},
	}
	err = b.read(src)
	b.noteSkips()
	rep.finish(b.w)
	return rep, err
}

// fb2Source is the book's XML: the file itself, or the one entry of a zip,
// read through the archive's bounds.
func fb2Source(ctx context.Context, r io.ReaderAt, size int64, opts Options) (io.ReadCloser, error) {
	var magic [4]byte
	if n, _ := r.ReadAt(magic[:], 0); n < 4 || string(magic[:]) != "PK\x03\x04" {
		if size > maxFB2Bytes {
			return nil, refusedf("the book is over %d bytes", maxFB2Bytes)
		}
		return io.NopCloser(&ctxReader{ctx: ctx, r: io.NewSectionReader(r, 0, size)}), nil
	}
	a, err := openZip(ctx, r, size, opts.limits)
	if err != nil {
		return nil, err
	}
	var names []string
	for name, f := range a.files {
		if !f.Mode().IsDir() {
			names = append(names, name)
		}
	}
	switch {
	case len(names) == 0:
		return nil, refusedf("the zip holds no book")
	case len(names) > 1:
		return nil, refusedf("an archive of %d files; unpack it first", len(names))
	case !strings.HasSuffix(strings.ToLower(names[0]), ".fb2"):
		return nil, refusedf("the zip's one file is not an .fb2 book")
	}
	return a.open(names[0], maxFB2Bytes)
}

// fb2Encodings are the encodings a book may declare besides UTF-8 (RFC GV
// D3): the ones a Russian- or Ukrainian-language library is written in.
var fb2Encodings = map[string]encoding.Encoding{
	"windows-1250": charmap.Windows1250, "cp1250": charmap.Windows1250,
	"windows-1251": charmap.Windows1251, "cp1251": charmap.Windows1251,
	"windows-1252": charmap.Windows1252, "cp1252": charmap.Windows1252,
	"koi8-r": charmap.KOI8R, "koi8-u": charmap.KOI8U,
	"iso-8859-1": charmap.ISO8859_1, "iso-8859-2": charmap.ISO8859_2, "iso-8859-5": charmap.ISO8859_5,
	"ibm866": charmap.CodePage866, "cp866": charmap.CodePage866,
}

// fb2Pic is a picture the text referred to before its binary was read.
type fb2Pic struct {
	num    int
	alt    string
	placed bool
}

type fb2Book struct {
	w          *writer
	rep        *Report
	opts       Options
	maxPicture int
	title      string
	meta       BookMetadata
	cover      string // the coverpage's picture id
	written    bool
	started    bool // the title has been written
	bookSeen   bool // the book's body has begun
	bookDone   bool // and ended
	zone       fb2Zone

	// The book's structure where the stream is now.
	sections, quotes int
	tgt              *fb2Target
	skip             int // the depth of an element whose content is skipped; 0 none
	stanzaLines      int
	inStanza         bool
	poems            int
	eof              bool // the stream was read to its end
	table            *tableRows
	row              []string
	// The description being read.
	person   *fb2Person
	sequence bool

	noteNum   map[string]int    // by note id, in the order first referred to
	noteText  map[string]string // by note id
	noteBytes int
	pics      map[string]*fb2Pic // pictures referred to, by binary id
	picNum    int
	held      map[string][]byte // binaries read before their reference
	embeds    map[string]string // pictures written, by id: an embed to repeat
	bin       *fb2Binary
	pictures  int
	picBytes  int
	picHead   bool

	skippedBodies, badPictures, droppedPictures, droppedNotes int
}

type fb2Zone int

const (
	fb2None fb2Zone = iota
	fb2Desc
	fb2Body
	fb2Notes
)

// fb2Target is the text of one block being collected: a paragraph, a
// heading, a verse line, a table cell, a note, a metadata value.
type fb2Target struct {
	kind  string // the element that opened it
	depth int    // the stack depth it ends at
	buf   strings.Builder
	limit int // bytes kept; 0 is no bound beyond the file's
	id    string
}

func (t *fb2Target) add(s string) {
	if t.limit > 0 {
		if room := t.limit - t.buf.Len(); room < len(s) {
			room = max(room, 0)
			for room > 0 && !utf8.RuneStart(s[room]) {
				room--
			}
			s = s[:room]
		}
	}
	t.buf.WriteString(s)
}

type fb2Person struct{ first, middle, last, nick string }

func (p *fb2Person) name() string {
	if n := strings.Join(strings.Fields(p.first+" "+p.middle+" "+p.last), " "); n != "" {
		return n
	}
	return strings.TrimSpace(p.nick)
}

type fb2Binary struct {
	id  string
	buf []byte
}

// read parses the book token by token. It returns at the first block the
// sink refuses, reading nothing after it.
func (b *fb2Book) read(src io.Reader) error {
	br := bufio.NewReader(src)
	var in io.Reader = br
	var utf16, bom8 bool
	switch head, _ := br.Peek(3); {
	case bytes.HasPrefix(head, []byte("\xEF\xBB\xBF")):
		bom8 = true
		_, _ = br.Discard(3)
	case bytes.HasPrefix(head, []byte("\xFF\xFE")), bytes.HasPrefix(head, []byte("\xFE\xFF")):
		utf16 = true
		in = transform.NewReader(br, unicode.UTF16(unicode.LittleEndian, unicode.ExpectBOM).NewDecoder())
	}
	var fail error
	decoded := int64(maxFB2Decoded)
	if b.opts.fb2MaxDecoded > 0 {
		decoded = b.opts.fb2MaxDecoded
	}
	if utf16 {
		in = &fb2Decoded{r: in, max: decoded}
	}
	g := &fb2Guard{r: in, runCap: fb2EncodedCap(b.maxPicture), maxPicture: b.maxPicture}
	d := xml.NewDecoder(g)
	d.Strict = true
	d.CharsetReader = func(label string, in io.Reader) (io.Reader, error) {
		l := strings.ToLower(strings.TrimSpace(label))
		switch {
		case l == "utf-16" || l == "utf-16le" || l == "utf-16be":
			if utf16 {
				return in, nil // already decoded from its byte order mark
			}
			fail = refusedf("the book declares UTF-16 but has no byte order mark")
		case utf16 || bom8:
			fail = refusedf("the book declares the %q encoding but begins with a Unicode byte order mark", cutLabel(label))
		case fb2Encodings[l] != nil:
			return &fb2Decoded{r: fb2Encodings[l].NewDecoder().Reader(in), max: decoded}, nil
		default:
			fail = refusedf("the book is in the %q encoding, which is not read: UTF-8, UTF-16, windows-1250, windows-1251, windows-1252, koi8-r, koi8-u, iso-8859-1, iso-8859-2, iso-8859-5 and ibm866 are", cutLabel(label))
		}
		return nil, fail
	}
	depth, nodes, text := 0, 0, 0
	rooted := false
	for i := 0; ; i++ {
		if i%4096 == 0 {
			if err := b.w.ctx.Err(); err != nil {
				return err
			}
		}
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			switch {
			case b.w.ctx.Err() != nil:
				return b.w.ctx.Err()
			case fail != nil:
				return fail
			case g.err != nil:
				return g.err
			case IsRefused(err):
				return err
			}
			return fmt.Errorf("extract: malformed FB2: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			nodes++
			if nodes > maxXMLNodes {
				return refusedf("the book holds more than %d elements", maxXMLNodes)
			}
			if depth++; depth > maxXMLDepth {
				return refusedf("the book nests deeper than %d", maxXMLDepth)
			}
			if !rooted {
				if t.Name.Local != "FictionBook" {
					return refusedf("not an FB2 book: its root element is not FictionBook")
				}
				rooted = true
				continue
			}
			if err := b.start(t, depth); err != nil {
				return err
			}
		case xml.EndElement:
			if err := b.end(t, depth); err != nil {
				return err
			}
			depth--
		case xml.CharData:
			if text += len(t); text > maxXMLText {
				return refusedf("the book holds more than %d bytes of text", maxXMLText)
			}
			if err := b.chars(t); err != nil {
				return err
			}
		case xml.Directive:
			return refusedf("the book declares a DTD")
		}
	}
	if !rooted {
		return refusedf("not an FB2 book: it holds no FictionBook element")
	}
	b.eof = true
	if err := b.begin(); err != nil {
		return err
	}
	return b.finish()
}

// cutLabel is an encoding name from the file, short enough to quote.
func cutLabel(s string) string {
	if len(s) > 40 {
		return s[:40]
	}
	return s
}

// start handles an element opening at depth (the root is depth 1).
func (b *fb2Book) start(t xml.StartElement, depth int) error {
	if b.skip > 0 {
		return nil
	}
	name := t.Name.Local
	if b.bin != nil {
		// A binary holds only its base64: one with elements inside is not
		// a picture.
		b.bin = nil
		b.badPictures++
	}
	if depth == 2 {
		switch name {
		case "description":
			b.zone = fb2Desc
		case "body":
			switch nm := fb2Attr(t, "name"); {
			case nm == "notes" || nm == "comments":
				b.zone = fb2Notes
			case nm == "" && !b.bookSeen:
				b.bookSeen = true
				b.zone = fb2Body
				return b.begin()
			default:
				b.skippedBodies++
				b.skip = depth
			}
		case "binary":
			b.startBinary(t, depth)
		default:
			b.skip = depth
		}
		return nil
	}
	switch b.zone {
	case fb2Desc:
		b.startMeta(t, depth)
	case fb2Body:
		return b.startText(t, depth)
	case fb2Notes:
		b.startNote(t, depth)
	}
	return nil
}

// end handles the element at depth closing.
func (b *fb2Book) end(t xml.EndElement, depth int) error {
	if b.skip > 0 {
		if depth == b.skip {
			b.skip = 0
		}
		return nil
	}
	if depth == 2 {
		switch b.zone {
		case fb2Desc:
			b.rep.Metadata = b.metadata()
		case fb2Body:
			if err := b.flushStanza(); err != nil {
				return err
			}
			b.bookDone = true
		}
		b.zone = fb2None
		if t.Name.Local == "binary" && b.bin != nil {
			return b.endBinary()
		}
		return nil
	}
	switch b.zone {
	case fb2Desc:
		b.endMeta(t, depth)
	case fb2Body:
		return b.endText(t, depth)
	case fb2Notes:
		b.endNote(depth)
	}
	return nil
}

// chars handles character data.
func (b *fb2Book) chars(c xml.CharData) error {
	if b.skip > 0 {
		return nil
	}
	if b.bin != nil {
		if len(b.bin.buf)+len(c) > fb2EncodedCap(b.maxPicture) {
			return b.pictureTooLarge()
		}
		b.bin.buf = append(b.bin.buf, c...)
		return nil
	}
	if b.tgt == nil {
		return nil
	}
	if b.tgt.kind != "p" {
		b.tgt.add(string(c))
		return nil
	}
	// A paragraph of megabytes is written as it grows, in pieces of
	// maxFB2Para cut on a character, so the output limit sees it before all
	// of it is held, and no piece costs more than its own size to render.
	for len(c) > 0 {
		n := min(len(c), max(maxFB2Para-b.tgt.buf.Len(), 1))
		for n < len(c) && n > 1 && !utf8.RuneStart(c[n]) {
			n--
		}
		b.tgt.add(string(c[:n]))
		c = c[n:]
		if b.tgt.buf.Len() >= maxFB2Para {
			text := oneLine(b.tgt.buf.String())
			b.tgt.buf.Reset()
			if err := b.emit(escapeLine(text)); err != nil {
				return err
			}
		}
	}
	return nil
}

// open starts collecting a block's text, unless one is being collected:
// then the element is part of it (a paragraph of a title, of a cell), set
// apart by a space.
func (b *fb2Book) open(kind string, depth, limit int) {
	if b.tgt != nil {
		b.tgt.add(" ")
		return
	}
	b.tgt = &fb2Target{kind: kind, depth: depth, limit: limit}
}

// closing returns the block that ends at depth, and stops collecting it.
func (b *fb2Book) closing(depth int) *fb2Target {
	if b.tgt == nil || b.tgt.depth != depth {
		return nil
	}
	t := b.tgt
	b.tgt = nil
	return t
}

// begin writes the title and the cover, once.
func (b *fb2Book) begin() error {
	if b.started {
		return nil
	}
	b.started = true
	if b.meta.Title != "" {
		if t := metaString(b.meta.Title, MaxMetaString); t != "" {
			b.title = t
		}
	}
	if err := b.emit(heading(1, b.title)); err != nil {
		return err
	}
	if b.cover == "" {
		return nil
	}
	return b.blockPicture(b.cover, "Cover")
}

// emit writes one block, inside the block quotes open where it stands.
//
// A quoted block is written a line at a time, each with its prefix, so the
// prefixes never build a second, larger copy of the block in memory.
func (b *fb2Book) emit(md string) error {
	if md == "" {
		return nil
	}
	lead := ""
	if b.written {
		lead = "\n\n"
	}
	b.written = true
	b.stanzaLines = 0 // a verse after another block starts a block of its own
	if b.quotes == 0 {
		return b.w.text(lead + md)
	}
	return b.quoted(lead, md)
}

// quoted writes md's lines inside the block quotes open where it stands,
// the first after lead.
func (b *fb2Book) quoted(lead, md string) error {
	prefix := strings.Repeat("> ", min(b.quotes, maxFB2Quotes))
	for {
		line, rest, more := strings.Cut(md, "\n")
		if err := b.w.text(lead + prefix + line); err != nil {
			return err
		}
		if !more {
			return nil
		}
		lead, md = "\n", rest
	}
}

// verse writes one verse line: the first of its stanza as a new block, each
// later one after a hard break, so a stanza of many lines is never held.
func (b *fb2Book) verse(line string) error {
	if b.stanzaLines == 0 {
		err := b.emit(line)
		b.stanzaLines = 1
		return err
	}
	if b.quotes == 0 {
		return b.w.text("\\\n" + line)
	}
	return b.quoted("\\\n", line)
}

// startText handles an element of the book's body.
func (b *fb2Book) startText(t xml.StartElement, depth int) error {
	name := t.Name.Local
	switch name {
	case "section":
		if err := b.flushStanza(); err != nil {
			return err
		}
		b.sections++
	case "epigraph", "cite", "annotation":
		b.quotes++
	case "title", "p", "subtitle", "text-author", "v", "date":
		b.open(name, depth, fb2Limit(name))
	case "poem":
		b.poems++
	case "stanza":
		if err := b.flushStanza(); err != nil {
			return err
		}
		b.inStanza = true
	case "table":
		if b.table != nil {
			b.skip = depth // a table in a table is read through its outer one
			return nil
		}
		b.table = b.w.newTable()
	case "tr":
		b.row = b.row[:0]
	case "td", "th":
		b.open("cell", depth, maxCellBytes)
	case "a":
		if b.tgt != nil && fb2Attr(t, "type") == "note" {
			if id, ok := fb2Local(fb2Attr(t, "href")); ok {
				b.tgt.add("[" + strconv.Itoa(b.noteRef(id)) + "]")
				b.skip = depth // the link's own text is the number again
			}
		}
	case "image":
		return b.image(t, depth)
	}
	return nil
}

// endText handles the end of an element of the book's body.
func (b *fb2Book) endText(t xml.EndElement, depth int) error {
	switch name := t.Name.Local; name {
	case "section":
		if err := b.flushStanza(); err != nil {
			return err
		}
		b.sections--
	case "epigraph", "cite", "annotation":
		if err := b.flushStanza(); err != nil {
			return err
		}
		b.quotes--
	case "poem":
		b.poems--
	case "stanza":
		return b.flushStanza()
	case "tr":
		if b.table != nil && len(b.row) > 0 {
			b.table.add(append([]string(nil), b.row...))
		}
	case "table":
		if b.table == nil {
			return nil
		}
		rows := b.table
		rows.done()
		b.table = nil
		if len(rows.rows) == 0 {
			return nil
		}
		b.rep.Tables++
		return b.emit(table(rows.rows))
	}
	tgt := b.closing(depth)
	if tgt == nil {
		return nil
	}
	text := oneLine(tgt.buf.String())
	switch tgt.kind {
	case "cell":
		if b.table != nil && len(b.row) <= maxSheetColumns {
			b.row = append(b.row, cell(text))
		}
		return nil
	case "v":
		if text != "" && b.inStanza {
			return b.verse(escapeLine(text))
		}
	}
	if text == "" {
		return nil
	}
	switch tgt.kind {
	case "title":
		if b.sections == 0 && b.quotes == 0 {
			// The body's own title: often the author and the title the
			// "#" already says.
			if text == oneLine(b.title) {
				return nil
			}
			return b.emit(escapeLine(text))
		}
		if b.sections > 0 && b.quotes == 0 && b.poems == 0 {
			return b.emit(heading(b.sections+1, text))
		}
		return b.emit("**" + headingText(text) + "**")
	case "subtitle":
		return b.emit("**" + headingText(text) + "**")
	case "text-author":
		return b.emit("— " + headingText(text))
	}
	return b.emit(escapeLine(text))
}

// flushStanza writes the verse lines collected, each kept on its own line
// with a hard break.
func (b *fb2Book) flushStanza() error {
	b.inStanza, b.stanzaLines = false, 0
	return nil
}

// fb2Limit is the text a block of the body collects: a paragraph is written
// in pieces past maxFB2Para (see chars), every other block is one line.
func fb2Limit(kind string) int {
	if kind == "p" {
		return 0
	}
	return maxFB2Line
}

// image handles a picture reference in the book's body: inside a block's
// text it is a marker (or, held already, an embed of its own block); as a
// block, the picture.
func (b *fb2Book) image(t xml.StartElement, depth int) error {
	b.skip = depth
	id, local := fb2Local(fb2Attr(t, "href"))
	alt := fb2Attr(t, "alt")
	if b.tgt == nil {
		return b.blockPicture(id, alt)
	}
	if !local || !b.opts.Images || b.tgt.kind == "cell" || b.tgt.kind == "title" {
		b.tgt.add(fb2AltText(alt))
		return nil
	}
	md, ok, err := b.placeNow(id, alt)
	if err != nil {
		return err
	}
	if ok {
		// An embed is a block of its own: the paragraph so far is
		// written, and goes on after it.
		tgt := b.tgt
		text := oneLine(tgt.buf.String())
		if text != "" && (tgt.kind == "p" || tgt.kind == "v") {
			if err := b.emit(escapeLine(text)); err != nil {
				return err
			}
			tgt.buf.Reset()
		}
		return b.emit(md)
	}
	b.tgt.add(b.pending(id, alt))
	return nil
}

// blockPicture writes a picture that stands as a block.
func (b *fb2Book) blockPicture(id, alt string) error {
	if !b.opts.Images || id == "" {
		return b.emit(escapeLine(fb2AltText(alt)))
	}
	md, ok, err := b.placeNow(id, alt)
	if err != nil {
		return err
	}
	if ok {
		return b.emit(md)
	}
	return b.emit(escapeLine(b.pending(id, alt)))
}

// placeNow is the embed of a picture whose binary has been read: written
// once, repeated as written.
func (b *fb2Book) placeNow(id, alt string) (string, bool, error) {
	if md, ok := b.embeds[id]; ok {
		return md, true, nil
	}
	data, ok := b.held[id]
	if !ok {
		return "", false, nil
	}
	delete(b.held, id)
	md, err := b.w.image(data, alt)
	if err != nil {
		return "", false, err
	}
	if b.embeds == nil {
		b.embeds = map[string]string{}
	}
	b.embeds[id] = md
	return md, true, nil
}

// pending numbers a picture the stream has not reached yet and returns the
// marker the text carries for it.
func (b *fb2Book) pending(id, alt string) string {
	p, ok := b.pics[id]
	if !ok {
		if len(b.pics) >= maxFB2Pictures {
			return fb2AltText(alt)
		}
		b.picNum++
		p = &fb2Pic{num: b.picNum, alt: cleanAlt(alt)}
		b.pics[id] = p
	}
	return "[picture " + strconv.Itoa(p.num) + "]"
}

func fb2AltText(alt string) string {
	if alt = cleanAlt(alt); alt != "" {
		return "[image: " + alt + "]"
	}
	return "[image]"
}

// startBinary begins reading a picture's base64, when the picture is one
// to keep: referred to and not yet placed, or read before the book's text
// ended and so perhaps referred to later.
func (b *fb2Book) startBinary(t xml.StartElement, depth int) {
	id := fb2Attr(t, "id")
	p := b.pics[id]
	wanted := b.opts.Images && id != "" &&
		((p != nil && !p.placed) || (p == nil && !b.bookDone && b.held[id] == nil && b.embeds[id] == ""))
	if !wanted {
		b.skip = depth
		return
	}
	b.bin = &fb2Binary{id: id}
}

// endBinary decodes a picture: its decoded size is checked from the encoded
// length first, so an oversized one is refused before any of it is decoded.
func (b *fb2Book) endBinary() error {
	bin := b.bin
	b.bin = nil
	data := bin.buf[:0]
	for _, c := range bin.buf {
		if c != ' ' && c != '\t' && c != '\r' && c != '\n' {
			data = append(data, c)
		}
	}
	data = bytes.TrimRight(data, "=")
	n := len(data) / 4 * 3
	if r := len(data) % 4; r > 1 {
		n += r - 1
	}
	if n > b.maxPicture {
		return b.pictureTooLarge()
	}
	if b.pictures >= maxFB2Pictures || b.picBytes+n > maxFB2PictureBytes {
		b.droppedPictures++
		return nil
	}
	out := make([]byte, n)
	k, err := base64.RawStdEncoding.Decode(out, data)
	if err != nil || k == 0 {
		b.badPictures++
		return nil
	}
	b.pictures++
	b.picBytes += k
	out = out[:k]
	p := b.pics[bin.id]
	if p == nil {
		b.held[bin.id] = out
		return nil
	}
	p.placed = true
	alt := "Picture " + strconv.Itoa(p.num)
	if p.alt != "" {
		alt += ": " + p.alt
	}
	md, err := b.w.image(out, alt)
	if err != nil {
		return err
	}
	if !b.picHead {
		b.picHead = true
		if err := b.emit(heading(2, "Pictures")); err != nil {
			return err
		}
	}
	return b.emit(md)
}

func (b *fb2Book) pictureTooLarge() error {
	return refusedf("a picture in the book is over %d bytes", b.maxPicture)
}

// fb2EncodedCap is the most base64 text a picture of n bytes is written in,
// with room for its line breaks and indentation.
func fb2EncodedCap(n int) int {
	enc := (n + 2) / 3 * 4
	return enc + enc/4 + 4096
}

// noteRef numbers a note reference in the order the text reaches them.
func (b *fb2Book) noteRef(id string) int {
	if n, ok := b.noteNum[id]; ok {
		return n
	}
	n := len(b.noteNum) + 1
	b.noteNum[id] = n
	return n
}

// startNote handles an element of the notes body: each section with an id
// is one note. A note is kept only when the text referred to it, or when the
// notes come before the text (then every note is kept, under the notes cap).
func (b *fb2Book) startNote(t xml.StartElement, depth int) {
	name := t.Name.Local
	switch {
	case name == "section" && b.tgt == nil:
		id := fb2Attr(t, "id")
		if _, refd := b.noteNum[id]; id == "" || (!refd && b.bookSeen) {
			b.skip = depth
			return
		}
		b.open("note", depth, maxFB2Note)
		b.tgt.id = id
	case name == "title" || name == "image":
		// A note's title is its number again; its pictures are not read.
		b.skip = depth
	case b.tgt != nil && (name == "p" || name == "v" || name == "subtitle" || name == "text-author" || name == "td"):
		b.tgt.add(" ")
	}
}

func (b *fb2Book) endNote(depth int) {
	t := b.closing(depth)
	if t == nil {
		return
	}
	text := oneLine(t.buf.String())
	if text == "" {
		return
	}
	if b.noteBytes+len(text) > maxFB2NoteBytes {
		b.droppedNotes++
		return
	}
	b.noteBytes += len(text)
	b.noteText[t.id] = text
}

// finish writes the notes the text referred to, in the order of their
// numbers.
func (b *fb2Book) finish() error {
	ids := make([]string, 0, len(b.noteNum))
	for id := range b.noteNum {
		if b.noteText[id] != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	sort.Slice(ids, func(i, j int) bool { return b.noteNum[ids[i]] < b.noteNum[ids[j]] })
	if err := b.emit(heading(2, "Notes")); err != nil {
		return err
	}
	for _, id := range ids {
		if err := b.emit(escapeLine("[" + strconv.Itoa(b.noteNum[id]) + "] " + b.noteText[id])); err != nil {
			return err
		}
	}
	return nil
}

// noteSkips reports what the reading left out.
func (b *fb2Book) noteSkips() {
	if b.skippedBodies > 0 {
		b.rep.note("%d bodies other than the book and its notes were skipped", b.skippedBodies)
	}
	if b.badPictures > 0 {
		b.rep.note("%d pictures could not be decoded and were left out", b.badPictures)
	}
	if b.droppedPictures > 0 {
		b.rep.note("%d pictures past %d pictures or %d bytes in all were left out", b.droppedPictures, maxFB2Pictures, maxFB2PictureBytes)
	}
	if b.droppedNotes > 0 {
		b.rep.note("%d notes past %d bytes of notes were left out", b.droppedNotes, maxFB2NoteBytes)
	}
	missing := 0
	for _, p := range b.pics {
		if !p.placed {
			missing++
		}
	}
	if missing > 0 && b.eof {
		b.rep.note("%d pictures the text refers to were not read", missing)
	}
}

// startMeta handles an element of the description. Only the first
// title-info and publish-info are read: src-title-info describes the
// original of a translation, document-info the FB2 file itself.
func (b *fb2Book) startMeta(t xml.StartElement, depth int) {
	name := t.Name.Local
	if depth == 3 {
		if name != "title-info" && name != "publish-info" {
			b.skip = depth
		}
		return
	}
	if b.tgt != nil {
		if b.tgt.kind == "annotation" && name != "image" {
			b.tgt.add(" ")
		}
		return
	}
	switch name {
	case "author", "translator":
		if depth == 4 {
			b.person = &fb2Person{}
		}
	case "first-name", "middle-name", "last-name", "nickname":
		if b.person != nil {
			b.open(name, depth, maxFB2MetaText)
		}
	case "book-title", "genre", "lang", "keywords", "publisher", "year", "isbn":
		if depth == 4 {
			b.open(name, depth, maxFB2MetaText)
		}
	case "date":
		if depth == 4 {
			if v := strings.TrimSpace(fb2Attr(t, "value")); v != "" && b.meta.Date == "" {
				b.meta.Date = v
			}
			b.open(name, depth, maxFB2MetaText)
		}
	case "annotation":
		if depth == 4 {
			b.open(name, depth, maxFB2Annotation)
		}
	case "sequence":
		if depth == 4 && !b.sequence {
			b.sequence = true
			b.meta.Series = fb2Attr(t, "name")
			b.meta.SeriesIndex = fb2Attr(t, "number")
		}
	case "image":
		if depth == 5 && b.cover == "" {
			if id, ok := fb2Local(fb2Attr(t, "href")); ok {
				b.cover = id
			}
		}
	}
}

func (b *fb2Book) endMeta(t xml.EndElement, depth int) {
	name := t.Name.Local
	if (name == "author" || name == "translator") && depth == 4 && b.person != nil {
		if n := b.person.name(); n != "" {
			if name == "author" {
				b.meta.Authors = append(b.meta.Authors, n)
			} else {
				b.meta.Translators = append(b.meta.Translators, n)
			}
		}
		b.person = nil
		return
	}
	tgt := b.closing(depth)
	if tgt == nil {
		return
	}
	v := strings.TrimSpace(tgt.buf.String())
	if p := b.person; p != nil {
		switch tgt.kind {
		case "first-name":
			p.first = v
		case "middle-name":
			p.middle = v
		case "last-name":
			p.last = v
		case "nickname":
			p.nick = v
		}
		return
	}
	set := func(f *string) {
		if *f == "" {
			*f = v
		}
	}
	switch tgt.kind {
	case "book-title":
		set(&b.meta.Title)
	case "genre":
		b.meta.Subjects = append(b.meta.Subjects, v)
	case "keywords":
		for _, k := range strings.Split(v, ",") {
			b.meta.Subjects = append(b.meta.Subjects, k)
		}
	case "lang":
		set(&b.meta.Language)
	case "date", "year":
		set(&b.meta.Date)
	case "publisher":
		set(&b.meta.Publisher)
	case "isbn":
		if v != "" {
			b.meta.Identifiers = append(b.meta.Identifiers, BookIdentifier{Scheme: "isbn", Value: v})
		}
	case "annotation":
		set(&b.meta.Description)
	}
}

// metadata is what the description said, cleaned.
func (b *fb2Book) metadata() *BookMetadata {
	m := b.meta
	m.SourceFormat = "fb2"
	return m.Clean()
}

// fb2Attr is an attribute's value by its local name, whatever its
// namespace (l:href, xlink:href).
func fb2Attr(t xml.StartElement, local string) string {
	for _, a := range t.Attr {
		if a.Name.Local == local {
			return a.Value
		}
	}
	return ""
}

// fb2Local reads a reference inside the book ("#id"): one to anything else
// is not followed (nothing is fetched).
func fb2Local(href string) (string, bool) {
	href = strings.TrimSpace(href)
	if !strings.HasPrefix(href, "#") || len(href) == 1 {
		return "", false
	}
	return href[1:], true
}

// fb2Guard reads the book's bytes ahead of the XML decoder and refuses, as
// they stream past, the two things the decoder would otherwise build in
// memory before handing over a token: an element with more than
// maxXMLAttrsPerElement attributes (the decoder holds them all, at about ten
// times their bytes), and a binary whose base64 is longer than a picture
// under the cap is written in. It also refuses any declaration (<!DOCTYPE)
// outright: the book needs none, and a DTD is how entities are declared.
//
// It must never see less markup than the decoder does: comments, CDATA
// sections and processing instructions end where the decoder ends them, and
// what it cannot read the end of only makes the decoder fail too.
type fb2Guard struct {
	r          io.Reader
	err        error
	st         int
	attrs      int
	bang       []byte
	name       []byte
	named      bool
	last       byte // the byte before this one inside markup
	prev       byte // and the one before it
	quote      byte
	binary     bool
	run        int
	runCap     int
	maxPicture int
	span       int // bytes since the last markup boundary
}

const (
	gText = iota
	gLt
	gBang
	gComment
	gCDATA
	gPI
	gEnd
	gStart
	gQuote
)

func (g *fb2Guard) Read(p []byte) (int, error) {
	if g.err != nil {
		return 0, g.err
	}
	n, err := g.r.Read(p)
	for _, c := range p[:n] {
		if g.err = g.step(c); g.err != nil {
			return 0, g.err
		}
	}
	return n, err
}

// step reads one byte: the markup state it moves to, then the run it
// belongs to. A run starts at each '<' and at the end of each piece of
// markup; base64 inside a binary is counted against the picture cap instead.
func (g *fb2Guard) step(c byte) error {
	old := g.st
	binaryText := g.binary && (old == gText || old == gCDATA)
	if err := g.parse(c); err != nil {
		return err
	}
	if g.st == gLt || (old != gText && g.st == gText) {
		g.span = 0
		return nil
	}
	if binaryText {
		return nil
	}
	if g.span++; g.span > maxFB2Span {
		return refusedf("a run of the book's text or markup is over %d bytes", maxFB2Span)
	}
	return nil
}

func (g *fb2Guard) parse(c byte) error {
	switch g.st {
	case gText:
		if c == '<' {
			g.st = gLt
		} else if g.binary {
			return g.count()
		}
	case gLt:
		g.last, g.prev = 0, 0
		switch c {
		case '!':
			g.st, g.bang = gBang, g.bang[:0]
		case '?':
			g.st = gPI
			return nil
		case '/':
			g.st, g.binary = gEnd, false
		default:
			g.st, g.attrs, g.named, g.binary = gStart, 0, false, false
			g.name = append(g.name[:0], c)
		}
	case gBang:
		g.bang = append(g.bang, c)
		switch s := string(g.bang); {
		case s == "--":
			// The decoder looks for "-->" only after the opening "--".
			g.st, g.last, g.prev = gComment, 0, 0
			return nil
		case s == "[CDATA[":
			g.st, g.last, g.prev = gCDATA, 0, 0
			return nil
		case strings.HasPrefix("--", s) || strings.HasPrefix("[CDATA[", s):
		default:
			return refusedf("the book declares a DTD")
		}
	case gComment:
		if c == '>' && g.last == '-' && g.prev == '-' {
			g.st = gText
		}
	case gCDATA:
		if c == '>' && g.last == ']' && g.prev == ']' {
			g.st = gText
		} else if g.binary {
			if err := g.count(); err != nil {
				return err
			}
		}
	case gPI:
		if c == '>' && g.last == '?' {
			g.st = gText
		}
	case gEnd:
		if c == '>' {
			g.st = gText
		}
	case gStart:
		if !g.named {
			if c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '/' || c == '>' {
				g.named = true
				local := g.name
				if i := bytes.LastIndexByte(local, ':'); i >= 0 {
					local = local[i+1:]
				}
				g.binary = string(local) == "binary"
			} else if len(g.name) < 64 {
				g.name = append(g.name, c)
			}
		}
		switch c {
		case '"', '\'':
			g.st, g.quote = gQuote, c
		case '=':
			if g.attrs++; g.attrs > maxXMLAttrsPerElement {
				return refusedf("an XML element holds more than %d attributes", maxXMLAttrsPerElement)
			}
		case '>':
			g.st = gText
			g.run = 0
			if g.last == '/' {
				g.binary = false // <binary/> holds nothing
			}
		}
	case gQuote:
		if c == g.quote {
			g.st = gStart
			g.prev, g.last = g.last, c
			return nil
		}
	}
	g.prev, g.last = g.last, c
	return nil
}

func (g *fb2Guard) count() error {
	if g.run++; g.run > g.runCap {
		return refusedf("a picture in the book is over %d bytes", g.maxPicture)
	}
	return nil
}

// fb2Decoded counts what a legacy encoding or UTF-16 decodes the book to,
// and refuses it past max.
type fb2Decoded struct {
	r   io.Reader
	n   int64
	max int64
}

func (d *fb2Decoded) Read(p []byte) (int, error) {
	n, err := d.r.Read(p)
	if d.n += int64(n); d.n > d.max {
		return 0, refusedf("the book decodes to more than %d bytes of text", d.max)
	}
	return n, err
}
