package extract

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
)

// RTF reads a Rich Text Format document (RFC GV §4.9) with a bounded
// tokenizer of its own: control words with an optional signed parameter,
// control symbols, groups and text. No helper runs and nothing in the file
// is fetched, executed or used as a path.
//
// The document is read as a stream and written as it goes: each paragraph
// when it ends (a long one in pieces), each table row when it ends, each
// picture when its data ends. Only a footnote's text is held, under its own
// caps, to be listed under "Notes" at the end. A document cut at its output
// limit stops reading there.
type RTF struct{}

const rtfMediaType = "application/rtf"

// Bounds on a document.
const (
	// maxRTFBytes caps the file.
	maxRTFBytes = 64 << 20
	// maxRTFDepth caps group nesting; the stack of group states is the
	// only thing that grows with it.
	maxRTFDepth = 256
	// maxRTFWord caps a control word's letters and maxRTFDigits its
	// parameter's digits (a signed 32-bit value is ten).
	maxRTFWord   = 32
	maxRTFDigits = 10
	// maxRTFPicture caps one picture's data, maxRTFPictureBytes all of
	// them, and maxRTFPictures their number.
	maxRTFPicture      = 16 << 20
	maxRTFPictureBytes = 64 << 20
	maxRTFPictures     = 500
	// maxRTFNote caps the text kept of one footnote, maxRTFNoteBytes of all.
	maxRTFNote      = 64 << 10
	maxRTFNoteBytes = 8 << 20
	// maxRTFPara is the Markdown one paragraph collects before what it
	// holds is written as a paragraph of its own; maxRTFLine caps the text
	// of a heading, and the plain text a paragraph keeps beside its
	// Markdown.
	maxRTFPara = 1 << 20
	maxRTFLine = 64 << 10
	// maxRTFRun is the text of a plain run rendered at once: rendering
	// costs some times the text in garbage.
	maxRTFRun = 64 << 10
	// maxRTFMeta caps the text read for one metadata value, and
	// maxRTFComment for the document comment: Clean cuts them further, this
	// only stops a value of megabytes being built first.
	maxRTFMeta    = MaxMetaString * 4
	maxRTFComment = MaxMetaDescription * 4
	// maxRTFFonts and maxRTFStyles cap the font table and the stylesheet
	// entries kept; maxRTFStyleName caps one style's name.
	maxRTFFonts     = 4096
	maxRTFStyles    = 4096
	maxRTFStyleName = 256
	// maxRTFDecoded caps the text the document decodes to: a \'hh byte of
	// a legacy code page is up to three bytes of UTF-8, and a \uN escape up
	// to four.
	maxRTFDecoded = 3 * maxRTFBytes
)

func (RTF) Match(mediaType, name string) bool {
	return matchType(mediaType, name, []string{rtfMediaType, "text/rtf", "application/x-rtf"}, []string{".rtf"})
}

func (RTF) Extract(ctx context.Context, r io.ReaderAt, size int64, sink Sink, opts Options) (Report, error) {
	rep := Report{Format: "rtf"}
	if size > maxRTFBytes {
		return rep, refusedf("the document is over %d bytes", maxRTFBytes)
	}
	maxPic := maxRTFPicture
	if opts.rtfMaxPicture > 0 {
		maxPic = opts.rtfMaxPicture
	}
	d := &rtfDoc{
		w: newWriter(ctx, sink, opts), rep: &rep, opts: opts, maxPicture: maxPic,
		title: title(opts.Name, "Document"), maxDecoded: maxRTFDecoded,
		in:    bufio.NewReaderSize(&ctxReader{ctx: ctx, r: io.NewSectionReader(r, 0, size)}, 32<<10),
		size:  size,
		docCP: rtfCodePage{cm: charmap.Windows1252}, deff: -1,
		fonts: map[int]rtfCodePage{}, styles: map[int]int{},
		notes: map[int]string{},
	}
	if opts.rtfMaxDecoded > 0 {
		d.maxDecoded = opts.rtfMaxDecoded
	}
	err := d.read()
	d.noteSkips()
	rep.finish(d.w)
	return rep, err
}

// rtfCodePage is the code page \'hh bytes are read in: a charmap, or the
// number of one that is not read (bad), refused when a byte needs it.
type rtfCodePage struct {
	cm  *charmap.Charmap
	bad int
}

// rtfCodePages are the code pages a document may name (RFC GV §4.9): the
// ones a Ukrainian-, Russian- or Western-language library is written in.
var rtfCodePages = map[int]*charmap.Charmap{
	1250: charmap.Windows1250, 1251: charmap.Windows1251, 1252: charmap.Windows1252,
	866: charmap.CodePage866, 20866: charmap.KOI8R, 21866: charmap.KOI8U,
	10000: charmap.Macintosh,
}

func rtfCodePageOf(n int) rtfCodePage {
	if cm := rtfCodePages[n]; cm != nil {
		return rtfCodePage{cm: cm}
	}
	return rtfCodePage{bad: n}
}

// rtfCharsets maps a font's \fcharsetN to its code page. A charset not
// here (Greek 161, Turkish 162, the CJK ones, ...) is read in the
// document's code page and counted in a note: refusing a document for one
// font in a table that lists dozens it never uses would refuse most of
// what Word writes, and Word writes characters outside the code page as
// \uN escapes anyway, the \'hh bytes beside them skipped.
var rtfCharsets = map[int]int{0: 1252, 77: 10000, 204: 1251, 238: 1250}

// rtfDest is what the text of a group goes to.
type rtfDest int

const (
	rtfBody      rtfDest = iota // the document's text
	rtfSkip                     // nothing: a destination not read
	rtfFontTable                // \fonttbl: fonts' charsets
	rtfStyles                   // \stylesheet: heading styles
	rtfInfo                     // \info
	rtfInfoField                // one \info value
	rtfFootnote                 // a footnote's text
	rtfPict                     // a picture's data
)

// rtfSkipped are the destinations whose content is never text.
var rtfSkipped = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`colortbl listtable listoverridetable revtbl rsidtbl filetbl
		header headerl headerr headerf footer footerl footerr footerf
		object objdata objclass objname themedata colorschememapping datastore xmlnstbl
		latentstyles pgdsctbl pgptbl protusertbl fldinst xe tc tcn bkmkstart bkmkend
		pn pntxta pntxtb annotation atnid atnauthor atnref atndate atrfstart atrfend
		shpinst nonshppict userprops docvar template listpicture mmathPr wgrffmtfilter
		ftnsep ftnsepc ftncn aftnsep aftnsepc aftncn private comment generator
		panose falt fontemb fontfile oldcprops oldpprops oldsprops oldtprops
		defchp defpap passwordhash do txe`) {
		m[w] = true
	}
	return m
}()

// rtfState is what a group sets; '{' saves it, '}' restores it.
type rtfState struct {
	dest    rtfDest
	field   string // the \info value being read
	opens   bool   // this group opened its destination
	b, i, v bool   // bold, italic, hidden
	uc      int    // the replacement characters after a \uN
	font    int
	outline int // \outlinelevelN, -1 none
	style   int // \sN
	intbl   bool
}

type rtfDoc struct {
	w          *writer
	rep        *Report
	opts       Options
	maxPicture int
	title      string
	in         *bufio.Reader
	size       int64
	pos        int64 // bytes read
	decoded    int64 // bytes of text decoded
	maxDecoded int64

	st     rtfState
	stack  []rtfState
	fresh  bool // no token yet in the group just opened
	ignore bool // the group began with \*
	skipN  int  // characters still to skip after a \uN
	high   rune // a UTF-16 high surrogate waiting for its low half

	docCP rtfCodePage
	deff  int
	fonts map[int]rtfCodePage
	font  int // the font table entry being read
	// styles maps a paragraph style's number to its heading level (1 for
	// "heading 1"); style is the entry being read.
	styles     map[int]int
	style      *rtfStyle
	styleDepth int // the stack's length while the stylesheet's group is open

	meta    BookMetadata
	metaBuf strings.Builder
	created struct{ yr, mo, dy int }

	started bool

	// The paragraph being read: md is its Markdown so far (runs already
	// closed), run the text of the run still open, with its style, and
	// plain its text without markup, for a heading or a table cell.
	md, run, plain strings.Builder
	runB, runI     bool
	headCut        bool

	// The table being read: the cells of the row, the paragraphs of the
	// cell, and the table's width and size when its first row is written.
	table     *rtfTable
	row, cell []string
	cellBytes int

	// Footnotes: their numbers in order, the text kept, and the one open.
	noteN     int
	notes     map[int]string
	noteBytes int
	note      strings.Builder

	pic                          *rtfPicture
	pictures, picBytes           int
	badPictures, droppedPictures int
	otherPictures, droppedNotes  int
	foreignBytes, cutHeads       int
}

type rtfStyle struct {
	num     int
	para    bool
	outline int
	name    strings.Builder
}

type rtfTable struct {
	width, rows, size int
	full              bool
}

type rtfPicture struct {
	kind    string // "png", "jpeg", "other", "" not yet known
	data    []byte
	keep    bool // the data is kept
	dropped bool // past the pictures' caps
	nibble  int  // a hex digit waiting for its pair, -1 none
}

// read tokenizes the document and acts on each token. It returns at the
// first block the sink refuses, reading nothing after it.
func (d *rtfDoc) read() error {
	if head, _ := d.in.Peek(3); string(head) == "\xEF\xBB\xBF" {
		_, _ = d.in.Discard(3)
		d.pos += 3
	}
	if head, _ := d.in.Peek(5); string(head) != `{\rtf` {
		return refusedf("not an RTF document: it does not begin with {\\rtf")
	}
	d.st = rtfState{uc: 1, outline: -1, font: -1}
	for i := 0; ; i++ {
		if i%4096 == 0 {
			if err := d.w.ctx.Err(); err != nil {
				return err
			}
		}
		c, err := d.byte()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		switch c {
		case '{':
			if err := d.push(); err != nil {
				return err
			}
		case '}':
			if err := d.pop(); err != nil {
				return err
			}
			if len(d.stack) == 0 {
				// The document's own group closed; what follows is not RTF.
				return d.finish()
			}
		case '\\':
			if err := d.control(); err != nil {
				return err
			}
		case '\r', '\n':
		default:
			if err := d.char(c); err != nil {
				return err
			}
		}
	}
	return d.finish()
}

func (d *rtfDoc) byte() (byte, error) {
	c, err := d.in.ReadByte()
	if err == nil {
		d.pos++
	}
	return c, err
}

func (d *rtfDoc) push() error {
	if len(d.stack) >= maxRTFDepth {
		return refusedf("the document nests groups deeper than %d", maxRTFDepth)
	}
	d.stack = append(d.stack, d.st)
	d.st.opens = false
	d.fresh, d.ignore, d.skipN = true, false, 0
	if d.st.dest == rtfStyles && len(d.stack) == d.styleDepth+1 {
		// A group directly inside the stylesheet is one style.
		d.style = &rtfStyle{outline: -1, para: true}
	}
	return nil
}

func (d *rtfDoc) pop() error {
	if len(d.stack) == 0 {
		return nil
	}
	closing := d.st
	d.st = d.stack[len(d.stack)-1]
	d.stack = d.stack[:len(d.stack)-1]
	d.fresh, d.ignore, d.skipN = false, false, 0
	if d.style != nil && len(d.stack) == d.styleDepth {
		d.endStyle()
	}
	if !closing.opens {
		return nil
	}
	switch closing.dest {
	case rtfInfoField:
		d.endInfoField(closing.field)
	case rtfInfo:
		d.rep.Metadata = d.metadata()
	case rtfFootnote:
		d.endFootnote()
	case rtfPict:
		return d.endPicture()
	}
	return nil
}

// control reads what follows a backslash.
func (d *rtfDoc) control() error {
	c, err := d.byte()
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return err
	}
	if !rtfLetter(c) {
		return d.symbol(c)
	}
	var word [maxRTFWord]byte
	n := 0
	for rtfLetter(c) {
		if n == maxRTFWord {
			return refusedf("a control word is longer than %d letters", maxRTFWord)
		}
		word[n] = c
		n++
		if c, err = d.byte(); err != nil {
			if err == io.EOF {
				return d.word(string(word[:n]), 0, false)
			}
			return err
		}
	}
	param, has, neg, digits := int64(0), false, false, 0
	if c == '-' {
		// A sign only before a digit. The "-" was read and a Peek follows,
		// so it cannot be unread: a "-" that is text is handled here.
		next, err := d.in.Peek(1)
		if err != nil || next[0] < '0' || next[0] > '9' {
			if err := d.word(string(word[:n]), 0, false); err != nil {
				return err
			}
			return d.char('-')
		}
		neg = true
		if c, err = d.byte(); err != nil {
			return err
		}
	}
	for c >= '0' && c <= '9' {
		if digits++; digits > maxRTFDigits {
			return refusedf("a control word's parameter is longer than %d digits", maxRTFDigits)
		}
		param, has = param*10+int64(c-'0'), true
		if c, err = d.byte(); err != nil {
			if err == io.EOF {
				c = ' '
				break
			}
			return err
		}
	}
	if neg {
		param = -param
	}
	if param > 1<<31-1 || param < -(1<<31) {
		return refusedf("a control word's parameter is out of range")
	}
	if c != ' ' {
		if err := d.in.UnreadByte(); err == nil {
			d.pos--
		}
	}
	return d.word(string(word[:n]), int(param), has)
}

func rtfLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// symbol handles a control symbol: a backslash and one character.
func (d *rtfDoc) symbol(c byte) error {
	if c == '*' {
		if d.fresh {
			d.ignore = true
		}
		return nil
	}
	d.fresh = false
	if c == '\'' {
		var hex [2]byte
		for k := range hex {
			b, err := d.byte()
			if err != nil {
				if err == io.EOF {
					return nil
				}
				return err
			}
			hex[k] = b
		}
		v, ok := rtfHexByte(hex[0], hex[1])
		if !ok || !d.textDest() || d.skipping() {
			return nil
		}
		return d.codeByte(v)
	}
	if !d.textDest() || d.skipping() {
		return nil
	}
	switch c {
	case '\\', '{', '}':
		return d.text(string(c))
	case '~':
		return d.text(" ")
	case '_':
		return d.text("-")
	case '\r', '\n':
		return d.word("par", 0, false)
	}
	return nil // \- an optional hyphen, \| \: index marks
}

// skipping consumes one of the replacement characters after a \uN.
func (d *rtfDoc) skipping() bool {
	if d.skipN > 0 {
		d.skipN--
		return true
	}
	return false
}

func rtfHexByte(a, b byte) (byte, bool) {
	h, ok1 := rtfHexDigit(a)
	l, ok2 := rtfHexDigit(b)
	return h<<4 | l, ok1 && ok2
}

func rtfHexDigit(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// textDest reports whether the group's text is read: the body, a
// footnote, an \info value or a style's name.
func (d *rtfDoc) textDest() bool {
	switch d.st.dest {
	case rtfBody, rtfFootnote, rtfInfoField, rtfStyles:
		return true
	}
	return false
}

// char handles a byte of text.
func (d *rtfDoc) char(c byte) error {
	d.fresh = false
	if d.st.dest == rtfPict {
		return d.pictHex(c)
	}
	if !d.textDest() || d.skipping() {
		return nil
	}
	if c == '\t' {
		return d.text(" ")
	}
	if c < 0x20 {
		return nil
	}
	if c >= 0x80 {
		return d.codeByte(c)
	}
	return d.text(d.asciiRun(c))
}

// asciiRun is c and the plain ASCII text buffered after it, up to a
// control, a group or a line end: text is handled a run at a time, not a
// byte at a time.
func (d *rtfDoc) asciiRun(c byte) string {
	buf, _ := d.in.Peek(min(d.in.Buffered(), 4096))
	n := 0
	for n < len(buf) && buf[n] >= 0x20 && buf[n] < 0x80 && buf[n] != '\\' && buf[n] != '{' && buf[n] != '}' {
		n++
	}
	if n == 0 {
		return string(c)
	}
	run := make([]byte, 0, n+1)
	run = append(append(run, c), buf[:n]...)
	_, _ = d.in.Discard(n)
	d.pos += int64(n)
	return string(run)
}

// codeByte decodes a byte in the code page of the font in use.
func (d *rtfDoc) codeByte(c byte) error {
	cp := d.docCP
	if f, ok := d.fonts[d.st.font]; ok {
		if f.bad != 0 {
			d.foreignBytes++
		} else {
			cp = f
		}
	}
	if cp.cm == nil {
		return refusedf("the document is in code page %d, which is not read: 1250, 1251, 1252, 866, KOI8-R (20866), KOI8-U (21866) and Mac Roman (10000) are", cp.bad)
	}
	return d.text(string(cp.cm.DecodeByte(c)))
}

// unicode handles \uN: a UTF-16 code unit, signed.
func (d *rtfDoc) unicode(n int) error {
	d.skipN = d.st.uc
	r := rune(uint16(int16(n)))
	if n > 0xFFFF || n < -0x8000 {
		r = utf8.RuneError
	}
	switch {
	case r >= 0xD800 && r < 0xDC00:
		d.high = r
		return nil
	case r >= 0xDC00 && r < 0xE000:
		if d.high == 0 {
			return nil
		}
		r = (d.high-0xD800)<<10 + (r - 0xDC00) + 0x10000
	}
	d.high = 0
	return d.text(string(r))
}

// word handles a control word.
func (d *rtfDoc) word(w string, n int, has bool) error {
	fresh, ignore := d.fresh, d.ignore
	d.fresh = false
	if w == "bin" {
		return d.bin(n)
	}
	if d.st.dest == rtfSkip {
		return nil
	}
	if fresh {
		if dest, ok := d.destination(w); ok {
			d.st.dest, d.st.opens = dest, dest != d.st.dest || dest == rtfFootnote
			return d.opened(w)
		}
		if ignore {
			d.st.dest = rtfSkip
			return nil
		}
	}
	switch {
	case w == "u":
		if !d.textDest() {
			return nil
		}
		return d.unicode(n)
	case w == "uc":
		d.st.uc = min(max(n, 0), 16)
		return nil
	case d.skipping():
		return nil
	}
	switch d.st.dest {
	case rtfFontTable:
		d.fontWord(w, n)
		return nil
	case rtfStyles:
		d.styleWord(w, n)
		return nil
	case rtfInfo, rtfInfoField:
		d.infoWord(w, n)
		return nil
	case rtfPict:
		d.pictWord(w)
		return nil
	}
	return d.textWord(w, n, has)
}

// destination maps a control word that opens a group to what the group's
// text goes to.
func (d *rtfDoc) destination(w string) (rtfDest, bool) {
	switch {
	case rtfSkipped[w]:
		return rtfSkip, true
	case w == "shppict" || w == "fldrslt" || w == "field":
		return d.st.dest, true // read through: the picture or the result
	}
	switch d.st.dest {
	case rtfBody:
		switch w {
		case "fonttbl":
			return rtfFontTable, true
		case "stylesheet":
			return rtfStyles, true
		case "info":
			return rtfInfo, true
		case "footnote":
			return rtfFootnote, true
		case "pict":
			return rtfPict, true
		}
	case rtfFootnote:
		switch w {
		case "footnote":
			return rtfSkip, true
		case "pict":
			return rtfPict, true
		}
	case rtfInfo:
		switch w {
		case "title", "author", "subject", "keywords", "doccomm", "creatim":
			return rtfInfoField, true
		}
	}
	return 0, false
}

// opened sets up a destination a group just opened.
func (d *rtfDoc) opened(w string) error {
	switch d.st.dest {
	case rtfInfoField:
		d.st.field = w
		d.metaBuf.Reset()
	case rtfFootnote:
		if d.st.v {
			d.st.dest = rtfSkip
			return nil
		}
		d.noteN++
		d.note.Reset()
		d.addPlain("[" + strconv.Itoa(d.noteN) + "]")
		return d.addRun("[" + strconv.Itoa(d.noteN) + "]")
	case rtfPict:
		d.pic = &rtfPicture{nibble: -1}
	case rtfStyles:
		d.styleDepth = len(d.stack)
	}
	return nil
}

// textWord handles a control word in the body or a footnote.
func (d *rtfDoc) textWord(w string, n int, has bool) error {
	on := !has || n != 0
	switch w {
	case "ansi":
		d.docCP = rtfCodePageOf(1252)
	case "mac":
		d.docCP = rtfCodePageOf(10000)
	case "pc":
		d.docCP = rtfCodePageOf(437)
	case "pca":
		d.docCP = rtfCodePageOf(850)
	case "ansicpg":
		d.docCP = rtfCodePageOf(n)
	case "deff":
		d.deff, d.st.font = n, n
	case "f":
		d.st.font = n
	case "plain":
		d.st.b, d.st.i, d.st.v, d.st.font = false, false, false, d.deff
	case "b":
		d.st.b = on
	case "i":
		d.st.i = on
	case "v":
		d.st.v = on
	case "pard":
		d.st.outline, d.st.style, d.st.intbl = -1, 0, false
	case "s":
		d.st.style = n
	case "outlinelevel":
		d.st.outline = n
	case "intbl":
		d.st.intbl = true
	case "par", "sect", "page":
		return d.endParagraph()
	case "line":
		return d.lineBreak()
	case "tab":
		return d.text(" ")
	case "emdash":
		return d.text("—")
	case "endash":
		return d.text("–")
	case "bullet":
		return d.text("•")
	case "lquote":
		return d.text("‘")
	case "rquote":
		return d.text("’")
	case "ldblquote":
		return d.text("“")
	case "rdblquote":
		return d.text("”")
	case "emspace", "enspace", "qmspace":
		return d.text(" ")
	case "cell", "nestcell":
		return d.endCell(w == "cell")
	case "row":
		return d.endRow()
	}
	return nil
}

// text adds decoded text where the group's text goes.
func (d *rtfDoc) text(s string) error {
	if d.decoded += int64(len(s)); d.decoded > d.maxDecoded {
		return refusedf("the document decodes to more than %d bytes of text", d.maxDecoded)
	}
	if d.st.v {
		return nil
	}
	s = rtfSpaces(s)
	switch d.st.dest {
	case rtfBody:
		d.addPlain(s)
		return d.addRun(s)
	case rtfFootnote:
		if d.note.Len()+len(s) <= maxRTFNote {
			d.note.WriteString(s)
		}
	case rtfInfoField:
		limit := maxRTFMeta
		if d.st.field == "doccomm" {
			limit = maxRTFComment
		}
		if d.metaBuf.Len()+len(s) <= limit {
			d.metaBuf.WriteString(s)
		}
	case rtfStyles:
		if d.style != nil && d.style.name.Len()+len(s) <= maxRTFStyleName {
			d.style.name.WriteString(s)
		}
	}
	return nil
}

// rtfSpaces maps control characters a \uN can carry to spaces: a newline
// in a run would end the line its markup is on.
func rtfSpaces(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == 0x85 || r == 0x2028 || r == 0x2029 {
			return ' '
		}
		return r
	}, s)
}

// heading is the heading level of the paragraph being read, 0 for none: its
// own outline level (0 is the first level), else its style's.
func (d *rtfDoc) heading() int {
	if d.st.outline >= 0 && d.st.outline < 9 {
		return d.st.outline + 1
	}
	if d.st.outline < 0 {
		return d.styles[d.st.style]
	}
	return 0
}

// inBody reports whether the paragraph being read is body text: not a
// heading, not a table cell.
func (d *rtfDoc) inBody() bool { return !d.st.intbl && d.heading() == 0 }

// addPlain adds text to the paragraph's plain text, kept for a heading or a
// cell, up to maxRTFLine.
func (d *rtfDoc) addPlain(s string) {
	if d.plain.Len()+len(s) <= maxRTFLine {
		d.plain.WriteString(s)
	} else {
		d.headCut = true
	}
}

// addRun adds text to the paragraph's Markdown in the current style. A
// paragraph whose Markdown reaches maxRTFPara is written as it stands and
// goes on as a paragraph of its own, so none is held whole.
func (d *rtfDoc) addRun(s string) error {
	if !d.inBody() {
		return nil
	}
	if d.run.Len() > 0 && (d.st.b != d.runB || d.st.i != d.runI) {
		d.closeRun()
	}
	d.runB, d.runI = d.st.b, d.st.i
	d.run.WriteString(s)
	if d.run.Len() >= maxRTFRun && !d.runB && !d.runI {
		// A plain run is rendered in pieces as it grows, which renders the
		// same; an emphasised one is not split (its markers would be), and
		// is bounded by the paragraph's cap below.
		d.closeRun()
	}
	if d.md.Len()+d.run.Len() < maxRTFPara {
		return nil
	}
	d.closeRun()
	md := d.paragraphMD()
	d.md.Reset()
	d.plain.Reset()
	d.headCut = false
	return d.emit(md)
}

// closeRun renders the open run into the paragraph's Markdown: its text
// escaped, bold or italic markers around it with its outer spaces outside
// them, and the block-start escape when it begins a line.
func (d *rtfDoc) closeRun() {
	text := d.run.String()
	d.run.Reset()
	core := rtfCollapse(text)
	atStart := d.md.Len() == 0 || strings.HasSuffix(d.md.String(), "\n")
	spaced := atStart || strings.HasSuffix(d.md.String(), " ")
	if core == "" {
		if text != "" && !spaced {
			d.md.WriteByte(' ')
		}
		return
	}
	if !spaced && rtfSpaceAt(text, true) {
		d.md.WriteByte(' ')
	}
	s := escapeInline(core)
	mark := ""
	if d.runB {
		mark = "**"
	}
	if d.runI {
		mark += "*"
	}
	if mark == "" {
		if atStart {
			s = escapeBlockStart(s)
		}
		d.md.WriteString(s)
	} else {
		d.md.WriteString(mark + rtfEmphasisEscaper.Replace(s) + mark)
	}
	if rtfSpaceAt(text, false) {
		d.md.WriteByte(' ')
	}
}

// rtfCollapse is oneLine for run text, which holds no line ends
// (rtfSpaces mapped them): its spaces trimmed and folded to one, without
// copying text that needs neither, the common case.
func rtfCollapse(s string) string {
	s = strings.TrimFunc(s, unicode.IsSpace)
	prev := false
	for _, r := range s {
		sp := unicode.IsSpace(r)
		if sp && (prev || r != ' ') {
			return strings.Join(strings.Fields(s), " ")
		}
		prev = sp
	}
	return s
}

// rtfEmphasisEscaper escapes what would end or start emphasis inside an
// emphasised run.
var rtfEmphasisEscaper = strings.NewReplacer("*", `\*`, "_", `\_`)

func rtfSpaceAt(s string, first bool) bool {
	var r rune
	if first {
		r, _ = utf8.DecodeRuneInString(s)
	} else {
		r, _ = utf8.DecodeLastRuneInString(s)
	}
	return unicode.IsSpace(r)
}

// paragraphMD is the paragraph's Markdown, its trailing breaks and spaces
// cut; its plain text, escaped, when no Markdown was collected.
func (d *rtfDoc) paragraphMD() string {
	md := d.md.String()
	for {
		t := strings.TrimRight(md, " ")
		if !strings.HasSuffix(t, "\\\n") {
			md = t
			break
		}
		md = t[:len(t)-2]
	}
	if md == "" {
		md = escapeLine(oneLine(d.plain.String()))
	}
	return md
}

// lineBreak handles \line: a hard break in a body paragraph, a space in
// anything else.
func (d *rtfDoc) lineBreak() error {
	if d.st.dest != rtfBody || !d.inBody() {
		return d.text(" ")
	}
	d.addPlain(" ")
	d.closeRun()
	// A space before the break stays: rewriting the paragraph to drop it
	// would copy it once a break.
	if md := d.md.String(); md != "" && !strings.HasSuffix(md, "\n") {
		d.md.WriteString("\\\n")
	}
	return nil
}

// endParagraph handles \par: a footnote's paragraphs are joined, a table
// cell's kept for the cell, a heading or a body paragraph written.
func (d *rtfDoc) endParagraph() error {
	switch d.st.dest {
	case rtfFootnote:
		return d.text(" ")
	case rtfBody:
	default:
		return nil
	}
	if d.st.intbl {
		d.cellParagraph()
		return nil
	}
	d.closeRun()
	defer d.resetParagraph()
	if lvl := d.heading(); lvl > 0 {
		text := oneLine(d.plain.String())
		if text == "" {
			return nil
		}
		if d.headCut {
			d.cutHeads++
		}
		return d.emit(heading(lvl+1, text))
	}
	return d.emit(d.paragraphMD())
}

func (d *rtfDoc) resetParagraph() {
	d.md.Reset()
	d.run.Reset()
	d.plain.Reset()
	d.headCut = false
}

// cellParagraph keeps the paragraph just ended for the cell, under the
// cell's cap.
func (d *rtfDoc) cellParagraph() {
	raw := d.plain.String()
	if room := maxCellBytes - d.cellBytes; len(raw) > room {
		// Cut before escaping, so no escape is split; escaping at most
		// doubles what is left.
		room = max(room, 0)
		for room > 0 && !utf8.RuneStart(raw[room]) {
			room--
		}
		raw = raw[:room]
		d.w.tableCut = true
	}
	text := cell(raw)
	d.resetParagraph()
	if text == "" {
		return
	}
	d.cellBytes += len(text)
	d.cell = append(d.cell, text)
}

// endCell handles \cell (and \nestcell, a cell of a table inside a cell,
// read as a paragraph of the outer cell).
func (d *rtfDoc) endCell(outer bool) error {
	if d.st.dest != rtfBody {
		return nil
	}
	d.cellParagraph()
	if !outer {
		return nil
	}
	if len(d.row) <= maxSheetColumns {
		d.row = append(d.row, cellParagraphs(d.cell))
	} else {
		d.w.tableWide = true
	}
	d.cell, d.cellBytes = d.cell[:0], 0
	return nil
}

// endRow writes the row as a line of the table, the first row as its
// header, under the table caps: a row past them is read and not written.
func (d *rtfDoc) endRow() error {
	if d.st.dest != rtfBody {
		return nil
	}
	if len(d.cell) > 0 {
		// A row ended with no \cell after its last paragraph.
		_ = d.endCell(true)
	}
	row := d.row
	d.row = d.row[:0]
	if len(row) == 0 {
		return nil
	}
	if len(row) > maxSheetColumns {
		row, d.w.tableWide = row[:maxSheetColumns], true
	}
	t := d.table
	first := t == nil
	if first {
		t = &rtfTable{width: len(row)}
	} else if t.full {
		return nil
	}
	line := rtfRowLine(row, t.width)
	if first {
		line += "\n|" + strings.Repeat(" --- |", t.width)
	}
	room, docBound := d.w.tableRoom()
	if (t.rows+1)*max(t.width, len(row)) > maxTableCells || t.size+len(line)+1 > room {
		t.full = true
		d.w.tableCut = true
		d.w.docTablesCut = d.w.docTablesCut || docBound
		return nil
	}
	if first {
		if err := d.begin(); err != nil {
			return err
		}
		d.table = t
		d.rep.Tables++
		line = "\n\n" + line
	} else {
		line = "\n" + line
	}
	t.rows++
	t.size += len(line)
	return d.w.text(line)
}

// rtfRowLine renders one row, padded to width; a row wider than the
// header keeps its extra cells (the text is there to read, though a
// renderer shows only the header's columns).
func rtfRowLine(row []string, width int) string {
	var b strings.Builder
	b.WriteString("|")
	for i := 0; i < max(width, len(row)); i++ {
		c := " "
		if i < len(row) && row[i] != "" {
			c = row[i]
		}
		b.WriteString(" " + c + " |")
	}
	return b.String()
}

// endTable charges a table that ended to the document's table budget.
func (d *rtfDoc) endTable() {
	if d.table == nil {
		return
	}
	d.w.tableBytes += d.table.size
	d.table = nil
}

// begin writes the title, once.
func (d *rtfDoc) begin() error {
	if d.started {
		return nil
	}
	d.started = true
	if t := metaString(d.meta.Title, MaxMetaString); t != "" {
		d.title = t
	}
	return d.w.text(heading(1, d.title))
}

// emit writes one block after the title. Any paragraph, an empty one
// too, ends a table: a table's rows are the paragraphs marked \intbl.
func (d *rtfDoc) emit(md string) error {
	d.endTable()
	if md == "" {
		return nil
	}
	if err := d.begin(); err != nil {
		return err
	}
	return d.w.text("\n\n" + md)
}

// finish ends what is open and lists the footnotes the text referred to.
func (d *rtfDoc) finish() error {
	// A last paragraph with no \par after it, read as body text whatever
	// group the file ended in.
	d.st.dest, d.st.v, d.st.intbl = rtfBody, false, false
	if err := d.endParagraph(); err != nil {
		return err
	}
	if err := d.begin(); err != nil {
		return err
	}
	d.endTable()
	if len(d.notes) == 0 {
		return nil
	}
	nums := make([]int, 0, len(d.notes))
	for n := range d.notes {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	if err := d.emit(heading(2, "Notes")); err != nil {
		return err
	}
	for _, n := range nums {
		if err := d.emit(escapeLine("[" + strconv.Itoa(n) + "] " + d.notes[n])); err != nil {
			return err
		}
	}
	return nil
}

// endFootnote keeps a footnote's text under the notes' cap.
func (d *rtfDoc) endFootnote() {
	text := oneLine(d.note.String())
	d.note.Reset()
	if text == "" {
		return
	}
	if d.noteBytes+len(text) > maxRTFNoteBytes {
		d.droppedNotes++
		return
	}
	d.noteBytes += len(text)
	d.notes[d.noteN] = text
}

// fontWord reads the font table: each font's number and the code page its
// charset (or its own \cpgN) names.
func (d *rtfDoc) fontWord(w string, n int) {
	switch w {
	case "f":
		d.font = n
	case "fcharset", "cpg":
		if len(d.fonts) >= maxRTFFonts {
			return
		}
		if w == "cpg" {
			if cm := rtfCodePages[n]; cm != nil {
				d.fonts[d.font] = rtfCodePage{cm: cm}
			}
			return
		}
		switch cp, ok := rtfCharsets[n]; {
		case ok:
			d.fonts[d.font] = rtfCodePageOf(cp)
		case n == 1 || n == 2:
			// The default and the Symbol charsets: the document's code page.
		default:
			d.fonts[d.font] = rtfCodePage{bad: n}
		}
	}
}

// styleWord reads the stylesheet: which paragraph styles are headings.
func (d *rtfDoc) styleWord(w string, n int) {
	s := d.style
	if s == nil {
		return
	}
	switch w {
	case "s":
		s.num = n
	case "cs", "ds", "ts", "tsrowd":
		s.para = false
	case "outlinelevel":
		s.outline = n
	}
}

// endStyle keeps a heading style: one with an outline level, or named
// "heading N" (Word's own names, before any alias after a comma).
func (d *rtfDoc) endStyle() {
	s := d.style
	d.style = nil
	if !s.para || len(d.styles) >= maxRTFStyles {
		return
	}
	lvl := 0
	if s.outline >= 0 && s.outline < 9 {
		lvl = s.outline + 1
	}
	name, _, _ := strings.Cut(strings.TrimSuffix(strings.TrimSpace(s.name.String()), ";"), ",")
	if rest, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(name)), "heading "); ok && lvl == 0 {
		if k, err := strconv.Atoi(rest); err == nil && k >= 1 && k <= 9 {
			lvl = k
		}
	}
	if lvl > 0 {
		d.styles[s.num] = lvl
	}
}

// infoWord reads \info's control words: the creation year, month and day.
func (d *rtfDoc) infoWord(w string, n int) {
	if d.st.dest != rtfInfoField || d.st.field != "creatim" {
		return
	}
	switch w {
	case "yr":
		d.created.yr = n
	case "mo":
		d.created.mo = n
	case "dy":
		d.created.dy = n
	}
}

// endInfoField keeps one \info value. A subject and the keywords become
// subjects (a subject is a short phrase, not a summary); the document
// comment is the description.
func (d *rtfDoc) endInfoField(field string) {
	v := strings.TrimSpace(d.metaBuf.String())
	d.metaBuf.Reset()
	set := func(f *string) {
		if *f == "" {
			*f = v
		}
	}
	switch field {
	case "title":
		set(&d.meta.Title)
	case "author":
		if v != "" {
			d.meta.Authors = append(d.meta.Authors, v)
		}
	case "subject":
		d.meta.Subjects = append(d.meta.Subjects, v)
	case "keywords":
		d.meta.Subjects = append(d.meta.Subjects, strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ';' })...)
	case "doccomm":
		set(&d.meta.Description)
	case "creatim":
		c := d.created
		switch {
		case d.meta.Date != "" || c.yr < 1 || c.yr > 9999:
		case c.mo >= 1 && c.mo <= 12 && c.dy >= 1 && c.dy <= 31:
			d.meta.Date = fmt.Sprintf("%04d-%02d-%02d", c.yr, c.mo, c.dy)
		default:
			d.meta.Date = fmt.Sprintf("%04d", c.yr)
		}
	}
}

func (d *rtfDoc) metadata() *BookMetadata {
	m := d.meta
	m.SourceFormat = "rtf"
	return m.Clean()
}

// pictWord reads a picture's kind.
func (d *rtfDoc) pictWord(w string) {
	p := d.pic
	if p == nil || p.kind != "" {
		return
	}
	switch w {
	case "pngblip":
		p.kind = "png"
	case "jpegblip":
		p.kind = "jpeg"
	case "wmetafile", "emfblip", "macpict", "pmmetafile", "dibitmap", "wbitmap":
		p.kind = "other"
	default:
		return
	}
	p.keep = d.opts.Images && p.kind != "other"
}

// pictHex reads a hex digit of a picture's data. A picture kept is decoded
// a byte at a time, its size checked before each byte, so one over the cap
// is refused having held at most the cap.
func (d *rtfDoc) pictHex(c byte) error {
	p := d.pic
	if p == nil || !p.keep || p.dropped {
		return nil
	}
	v, ok := rtfHexDigit(c)
	if !ok {
		return nil
	}
	if p.nibble < 0 {
		p.nibble = int(v)
		return nil
	}
	b := byte(p.nibble)<<4 | v
	p.nibble = -1
	return d.pictByte(b)
}

func (d *rtfDoc) pictByte(b byte) error {
	p := d.pic
	if len(p.data) >= d.maxPicture {
		return refusedf("a picture in the document is over %d bytes", d.maxPicture)
	}
	if d.pictures >= maxRTFPictures || d.picBytes+len(p.data) >= maxRTFPictureBytes {
		p.dropped, p.data = true, nil
		return nil
	}
	p.data = append(p.data, b)
	return nil
}

// bin reads \binN: N bytes of raw data, refused when longer than what is
// left of the file. A picture kept is checked against its cap a byte at a
// time, as hex data is.
func (d *rtfDoc) bin(n int) error {
	if n < 0 || int64(n) > d.size-d.pos {
		return refusedf("a \\bin of %d bytes is longer than what is left of the document", n)
	}
	p := d.pic
	if d.st.dest != rtfPict || p == nil || !p.keep || p.dropped {
		k, err := d.in.Discard(n)
		d.pos += int64(k)
		return err
	}
	for range n {
		c, err := d.byte()
		if err != nil {
			return err
		}
		if err := d.pictByte(c); err != nil {
			return err
		}
	}
	return nil
}

// endPicture places a picture: as a block of its own in a body paragraph
// (the paragraph so far written first), as "[image]" in a heading, a cell,
// a footnote or with pictures off.
func (d *rtfDoc) endPicture() error {
	p := d.pic
	d.pic = nil
	if p == nil || d.st.v {
		return nil
	}
	switch {
	case p.kind == "other" || p.kind == "":
		d.otherPictures++
	case p.dropped:
		d.droppedPictures++
	case p.keep && len(p.data) == 0:
		d.badPictures++
	}
	placed := p.keep && !p.dropped && len(p.data) > 0
	if !placed || d.st.dest != rtfBody || !d.inBody() {
		return d.text("[image]")
	}
	d.pictures++
	d.picBytes += len(p.data)
	md, err := d.w.image(p.data, "")
	if err != nil {
		return err
	}
	d.closeRun()
	if para := d.paragraphMD(); para != "" {
		if err := d.emit(para); err != nil {
			return err
		}
	}
	d.resetParagraph()
	return d.emit(md)
}

// noteSkips reports what the reading left out.
func (d *rtfDoc) noteSkips() {
	if d.badPictures > 0 {
		d.rep.note("%d pictures held no data and were left out", d.badPictures)
	}
	if d.droppedPictures > 0 {
		d.rep.note("%d pictures past %d pictures or %d bytes in all were left out", d.droppedPictures, maxRTFPictures, maxRTFPictureBytes)
	}
	if d.otherPictures > 0 {
		d.rep.note("%d pictures in WMF, EMF or other formats than PNG and JPEG were left out", d.otherPictures)
	}
	if d.droppedNotes > 0 {
		d.rep.note("%d footnotes past %d bytes of notes were left out", d.droppedNotes, maxRTFNoteBytes)
	}
	if d.cutHeads > 0 {
		d.rep.note("%d headings were cut at %d bytes", d.cutHeads, maxRTFLine)
	}
	if d.foreignBytes > 0 {
		d.rep.note("%d characters in fonts of a character set that is not read were read in the document's code page", d.foreignBytes)
	}
}
