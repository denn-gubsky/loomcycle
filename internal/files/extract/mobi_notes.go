package extract

import (
	"bytes"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
)

// A MOBI book's footnotes are links into its own text, by byte offset
// (RFC GV §4.4), and its contents are an index. What Calibre writes, from a
// source marked the EPUB 3 way (a noteref link, a footnote <aside> that
// links back), is what these rules follow:
//
//   - KF8 keeps the marks: <a epub_type="noteref" href="kindle:pos:fid:F:off:O">
//     points at <aside epub_type="footnote">. The link's fragment F and
//     offset O are resolved to a text offset through the skeleton and
//     fragment indexes.
//   - MOBI 7 keeps none: the reference is <sup><a filepos=N>1</a></sup>
//     and the note a paragraph further on whose first content is a link
//     back to the reference, <p><a filepos=R>1</a> The note.</p>. A
//     plain cross-reference (a link to a chapter, a contents entry) has no
//     <sup>, or a target that does not link back, and stays text.
//
// A reference is written [n], numbered by first reference, and the note it
// points at is taken out of the text when the reading reaches it; the
// notes are listed under "Notes" once the text is read, as EPUB and FB2
// list theirs. Only a note a written reference numbered is taken out.

// Bounds on a MOBI book's notes.
const (
	maxMOBINotes     = 100000
	maxMOBINote      = 64 << 10
	maxMOBINoteBytes = 8 << 20
	// maxMOBINoteMark is the longest text a MOBI 7 reference's link may
	// have, in characters: a number or a symbol, not a phrase.
	maxMOBINoteMark = 8
	// mobiBacklinkSpan is how far before its reference a MOBI 7 note's
	// link back may point: the start of the <sup> or paragraph around it.
	mobiBacklinkSpan = 256
	// maxMOBIMarks caps the positions marked in one block of text.
	maxMOBIMarks = 1 << 15
)

// mobiPosAttr carries a tag's offset in the book's text through the HTML
// parser: the reader writes it into the tags that references point at,
// and into MOBI 7's links, before a block is parsed.
const mobiPosAttr = "data-loomcycle-pos"

// kf8Tables are a KF8 book's skeleton and fragment indexes, resolved: the
// text offset each file starts and ends at, with its contents label, and
// each fragment's start and length.
type kf8Tables struct {
	files []kf8File
	frags []kf8Frag
}

type kf8File struct {
	start, end int64
	label      string
}

type kf8Frag struct{ start, length int64 }

// readKF8Tables reads the skeleton and fragment indexes. KF8 text holds
// each file's skeleton followed by its fragments, in order: a fragment
// starts where the one before it in its file ends, the first right after
// its skeleton.
func readKF8Tables(db *palmDB, skelAt, fragAt int) (*kf8Tables, error) {
	skel, err := readMOBIIndex(db, skelAt, maxKF8Parts)
	if err != nil {
		return nil, err
	}
	frag, err := readMOBIIndex(db, fragAt, maxKF8Parts)
	if err != nil {
		return nil, err
	}
	t := &kf8Tables{}
	next := 0
	end := int64(0)
	for _, e := range skel.entries {
		count, ok1 := e.value(1, 0)
		start, ok2 := e.value(6, 0)
		length, ok3 := e.value(6, 1)
		if !ok1 || !ok2 || !ok3 || int64(start) < end {
			return nil, refusedf("the book's skeleton index lists a file out of order or without its place")
		}
		if int64(count) > int64(len(frag.entries)-next) {
			return nil, refusedf("the book's skeleton index names more fragments than its fragment index holds")
		}
		at := int64(start) + int64(length)
		for k := 0; k < int(count); k++ {
			n, ok := frag.entries[next].value(6, 1)
			if !ok {
				return nil, refusedf("the book's fragment index lists a fragment without its length")
			}
			t.frags = append(t.frags, kf8Frag{start: at, length: int64(n)})
			at += int64(n)
			next++
		}
		t.files = append(t.files, kf8File{start: int64(start), end: at})
		end = at
	}
	return t, nil
}

// addLabels names each file by the first contents entry inside it (the
// NCX offset of an entry is a place in the file's assembled text, which
// covers the same span of the book's text as the file).
func (t *kf8Tables) addLabels(db *palmDB, ncxAt int) error {
	ncx, err := readMOBIIndex(db, ncxAt, maxMOBIContents)
	if err != nil {
		return err
	}
	for _, e := range ncx.entries {
		if d, ok := e.value(4, 0); ok && d > maxMOBIContentsDepth {
			return refusedf("the book's contents nest %d deep, more than %d", d, maxMOBIContentsDepth)
		}
		off, ok1 := e.value(1, 0)
		at, ok2 := e.value(3, 0)
		if !ok1 || !ok2 {
			continue
		}
		label, err := ncx.label(at)
		if err != nil {
			return err
		}
		f := t.file(int64(off))
		if label = metaString(label, MaxMetaString); f >= 0 && label != "" && t.files[f].label == "" {
			t.files[f].label = label
		}
	}
	return nil
}

// file is the index of the file holding text offset off, -1 for none.
func (t *kf8Tables) file(off int64) int {
	i := sort.Search(len(t.files), func(i int) bool { return t.files[i].start > off }) - 1
	if i < 0 || off >= t.files[i].end {
		return -1
	}
	return i
}

// resolve is the text offset of kindle:pos:fid:F:off:O, false for a
// fragment the book does not hold or an offset past it.
func (t *kf8Tables) resolve(href string) (int64, bool) {
	const prefix = "kindle:pos:fid:"
	if len(href) <= len(prefix) || !strings.EqualFold(href[:len(prefix)], prefix) {
		return 0, false
	}
	fid, rest, ok := strings.Cut(href[len(prefix):], ":")
	if !ok || len(rest) < 4 || !strings.EqualFold(rest[:4], "off:") {
		return 0, false
	}
	off := rest[4:]
	if i := strings.IndexAny(off, "?#"); i >= 0 {
		off = off[:i]
	}
	// 32 bits each: a fragment number and an offset past them are no
	// place in a book whose text is capped well below 4 GiB.
	f, err1 := strconv.ParseUint(fid, 32, 32)
	o, err2 := strconv.ParseUint(off, 32, 32)
	if err1 != nil || err2 != nil || f >= uint64(len(t.frags)) || int64(o) >= t.frags[f].length {
		return 0, false
	}
	return t.frags[f].start + int64(o), true
}

// mobiNotes are the footnotes of the book being read.
type mobiNotes struct {
	// kf8 resolves KF8 links; nil for a MOBI 7 book.
	kf8 *kf8Tables
	// num numbers each target by its text offset, in the order the
	// references are reached; refAt is where its first reference is.
	num   map[int64]int
	refAt map[int64]int64
	text  map[int]string
	bytes int
	// dropped counts the notes past maxMOBINoteBytes.
	dropped int
}

func newMOBINotes(kf8 *kf8Tables) *mobiNotes {
	return &mobiNotes{kf8: kf8, num: map[int64]int{}, refAt: map[int64]int64{}, text: map[int]string{}}
}

// mark writes the text offset into the tags of a block that a note may be
// found at: each place a link in the block points at, each target already
// numbered, and (MOBI 7) each link, whose own place decides whether it is a
// reference. Offsets are the block's raw bytes from start, before any
// decoding; the attribute is ASCII, so it reads the same in either of the
// book's encodings. At most maxMOBIMarks tags are marked.
func (n *mobiNotes) mark(seg []byte, start int64) []byte {
	cands := n.linkTargets(seg)
	var out []byte
	last, marks := 0, 0
	for i := 0; i+1 < len(seg) && marks < maxMOBIMarks; i++ {
		if seg[i] != '<' || !isASCIILetter(seg[i+1]) {
			continue
		}
		p := start + int64(i)
		_, numbered := n.num[p]
		if !numbered && !cands[p] && (n.kf8 != nil || !fileposLink(seg[i:])) {
			continue
		}
		j := i + 1
		for j < len(seg) && j-i <= 32 && (isASCIILetter(seg[j]) || seg[j] >= '0' && seg[j] <= '9' || seg[j] == ':' || seg[j] == '_' || seg[j] == '-') {
			j++
		}
		out = append(out, seg[last:j]...)
		out = append(out, ` `+mobiPosAttr+`="`+strconv.FormatInt(p, 10)+`"`...)
		last = j
		marks++
	}
	if out == nil {
		return seg
	}
	return append(out, seg[last:]...)
}

// linkTargets are the offsets the links of a block point at, read from its
// raw bytes: filepos=N (MOBI 7) or kindle:pos hrefs (KF8).
func (n *mobiNotes) linkTargets(seg []byte) map[int64]bool {
	out := map[int64]bool{}
	marker := "filepos="
	if n.kf8 != nil {
		marker = "kindle:pos:fid:"
	}
	for i := indexFold(seg, 0, marker); i >= 0 && len(out) < maxMOBIMarks; i = indexFold(seg, i+len(marker), marker) {
		if n.kf8 != nil {
			end := i
			for end < len(seg) && end-i < 64 && seg[end] != '"' && seg[end] != '\'' && seg[end] != '>' && seg[end] > ' ' {
				end++
			}
			if p, ok := n.kf8.resolve(string(seg[i:end])); ok {
				out[p] = true
			}
			continue
		}
		v := seg[i+len(marker):]
		v = bytes.TrimLeft(v[:min(len(v), 12)], `"'`)
		k := 0
		for k < len(v) && k < 10 && v[k] >= '0' && v[k] <= '9' {
			k++
		}
		if p, err := strconv.ParseInt(string(v[:k]), 10, 32); err == nil {
			out[p] = true
		}
	}
	return out
}

// fileposLink reports whether b starts an <a> tag that carries filepos,
// looking no further than the tag's end or the next tag's start.
func fileposLink(b []byte) bool {
	if len(b) < 3 || b[1]|0x20 != 'a' || (b[2] != ' ' && b[2] != '\t' && b[2] != '\n' && b[2] != '\r') {
		return false
	}
	end := len(b)
	if i := bytes.IndexAny(b[1:], "<>"); i >= 0 {
		end = i + 1
	}
	return indexFold(b[:end], 0, "filepos") >= 0
}

func isASCIILetter(c byte) bool { return c|0x20 >= 'a' && c|0x20 <= 'z' }

// posOf is the text offset the reader marked el with.
func posOf(el *html.Node) (int64, bool) {
	v := attrOf(el, mobiPosAttr)
	if v == "" {
		return 0, false
	}
	p, err := strconv.ParseInt(v, 10, 32)
	return p, err == nil
}

// fileposOf is a MOBI 7 link's target.
func fileposOf(a *html.Node) (int64, bool) {
	v := strings.Trim(strings.TrimSpace(attrOf(a, "filepos")), `"'`)
	if v == "" {
		return 0, false
	}
	p, err := strconv.ParseInt(v, 10, 32)
	return p, err == nil && p >= 0
}

// kf8Type is an element's epub:type, which KF8 keeps as epub_type.
func kf8Type(n *html.Node) string {
	if t := epubType(n); t != "" {
		return t
	}
	return attrOf(n, "epub_type")
}

// refTarget is the text offset of the note a link refers to, false for a
// link that is not a note reference.
func (n *mobiNotes) refTarget(a *html.Node) (int64, int64, bool) {
	if n.kf8 != nil {
		if !hasToken(kf8Type(a), "noteref") {
			return 0, 0, false
		}
		t, ok := n.kf8.resolve(strings.TrimSpace(attrOf(a, "href")))
		return t, -1, ok
	}
	t, ok1 := fileposOf(a)
	r, ok2 := posOf(a)
	if !ok1 || !ok2 || t <= r || !nearSup(a) {
		return 0, 0, false
	}
	mark := oneLine(textOf(a))
	if c := utf8.RuneCountInString(mark); c == 0 || c > maxMOBINoteMark {
		return 0, 0, false
	}
	return t, r, true
}

// nearSup reports whether a MOBI 7 link is set as a superscript: a <sup>
// around it or inside it, within three levels.
func nearSup(a *html.Node) bool {
	p := a.Parent
	for k := 0; p != nil && k < 3; k++ {
		if p.Type == html.ElementNode && p.Data == "sup" {
			return true
		}
		p = p.Parent
	}
	var in func(n *html.Node, depth int) bool
	in = func(n *html.Node, depth int) bool {
		for c := n.FirstChild; c != nil && depth < 3; c = c.NextSibling {
			if c.Type == html.ElementNode && (c.Data == "sup" || in(c, depth+1)) {
				return true
			}
		}
		return false
	}
	return in(a, 0)
}

// number is target t's note number, given one at its first reference r;
// 0 past maxMOBINotes.
func (n *mobiNotes) number(t, r int64) int {
	if k, ok := n.num[t]; ok {
		return k
	}
	if len(n.num) >= maxMOBINotes {
		return 0
	}
	k := len(n.num) + 1
	n.num[t], n.refAt[t] = k, r
	return k
}

// register numbers the references of a parsed block in reading order
// before it is walked, so a note the block holds is known as one when the
// walk reaches it, wherever the reference is.
func (n *mobiNotes) register(doc *html.Node) {
	budget := maxXMLNodes
	var walk func(m *html.Node, depth int)
	walk = func(m *html.Node, depth int) {
		if depth > maxXMLDepth {
			return
		}
		for c := m.FirstChild; c != nil; c = c.NextSibling {
			if budget--; budget < 0 {
				return
			}
			if c.Type != html.ElementNode {
				continue
			}
			if c.Data == "a" {
				if t, r, ok := n.refTarget(c); ok {
					n.number(t, r)
				}
			}
			walk(c, depth+1)
		}
	}
	walk(doc, 0)
}

// ref is "[n]" for a note reference, "" for any other link.
func (n *mobiNotes) ref(a *html.Node) string {
	t, r, ok := n.refTarget(a)
	if !ok {
		return ""
	}
	if k := n.number(t, r); k > 0 {
		return "[" + strconv.Itoa(k) + "]"
	}
	return ""
}

// take takes a note out of the text and keeps it: an element at a target
// a reference numbered that is a note by the book's own marks. A KF8 note
// is typed (footnote, endnote, rearnote) or an <aside>; a MOBI 7 note is a
// block whose first content is a link back to its reference.
func (n *mobiNotes) take(el *html.Node) bool {
	p, ok := posOf(el)
	if !ok {
		return false
	}
	k, numbered := n.num[p]
	if !numbered {
		return false
	}
	back := leadingLink(el)
	if n.kf8 != nil {
		if !isNoteType(kf8Type(el)) && el.Data != "aside" {
			return false
		}
		if back != nil && utf8.RuneCountInString(oneLine(textOf(back))) > maxMOBINoteMark {
			back = nil
		}
	} else {
		if !htmlBlock[el.Data] && el.Data != "li" || back == nil {
			return false
		}
		b, ok := fileposOf(back)
		r := n.refAt[p]
		if !ok || b > r || r-b > mobiBacklinkSpan {
			return false
		}
	}
	if _, kept := n.text[k]; kept {
		return true
	}
	t := oneLine(boundedText(el, back, maxMOBINote))
	if n.bytes+len(t) > maxMOBINoteBytes {
		n.dropped++
		return true
	}
	n.bytes += len(t)
	n.text[k] = t
	return true
}

// leadingLink is the <a> an element's content starts with, nil when text
// comes first. It looks at the first few nodes only.
func leadingLink(el *html.Node) *html.Node {
	budget := 64
	var find func(m *html.Node, depth int) (*html.Node, bool)
	find = func(m *html.Node, depth int) (*html.Node, bool) {
		for c := m.FirstChild; c != nil && depth < 16; c = c.NextSibling {
			if budget--; budget < 0 {
				return nil, true
			}
			switch c.Type {
			case html.TextNode:
				if strings.TrimSpace(c.Data) != "" {
					return nil, true
				}
				continue
			case html.ElementNode:
			default:
				continue
			}
			if c.Data == "a" {
				return c, true
			}
			if a, done := find(c, depth+1); done {
				return a, true
			}
		}
		return nil, false
	}
	a, _ := find(el, 0)
	return a
}

// boundedText is an element's text without skip's, at most limit bytes
// (cut at a character boundary).
func boundedText(el, skip *html.Node, limit int) string {
	var b strings.Builder
	var walk func(m *html.Node, depth int)
	walk = func(m *html.Node, depth int) {
		if depth > maxXMLDepth || b.Len() >= limit || m == skip {
			return
		}
		switch m.Type {
		case html.TextNode:
			b.WriteString(sourceSpace(m.Data[:min(len(m.Data), limit-b.Len()+utf8.UTFMax)]))
			return
		case html.ElementNode:
			if htmlSkip[m.Data] {
				return
			}
			if m.Data == "br" {
				b.WriteString("\n")
			}
		}
		for c := m.FirstChild; c != nil; c = c.NextSibling {
			walk(c, depth+1)
		}
	}
	walk(el, 0)
	s := b.String()
	if len(s) > limit {
		// The text is valid UTF-8, so only the cut can leave a broken
		// character, at the end: it is dropped.
		s = strings.ToValidUTF8(s[:limit], "")
	}
	return s
}

// list is the notes kept, in the order of their numbers.
func (n *mobiNotes) list() []int {
	var nums []int
	for k, t := range n.text {
		if t != "" {
			nums = append(nums, k)
		}
	}
	sort.Ints(nums)
	return nums
}
