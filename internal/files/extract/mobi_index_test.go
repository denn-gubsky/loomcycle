package extract

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// testTag is a TAGX tag definition: the tag, its values per entry and the
// control byte bit that marks it.
type testTag struct{ tag, values, mask byte }

// fwd is v as a forward variable-width integer.
func fwd(v uint32) []byte {
	var b []byte
	for {
		b = append([]byte{byte(v & 0x7f)}, b...)
		if v >>= 7; v == 0 {
			break
		}
	}
	b[len(b)-1] |= 0x80
	return b
}

// testEntry is an index entry: its name, one control byte, its values.
func testEntry(name string, ctrl byte, vals ...uint32) []byte {
	e := append([]byte{byte(len(name))}, name...)
	e = append(e, ctrl)
	for _, v := range vals {
		e = append(e, fwd(v)...)
	}
	return e
}

func putU32(b []byte, off int, v int) { binary.BigEndian.PutUint32(b[off:], uint32(v)) }

// testIndex is an index as Calibre writes one: a header record with its
// TAGX section (one control byte), one data record, then the CNCX records.
func testIndex(tags []testTag, entries [][]byte, cncx ...[]byte) [][]byte {
	head := make([]byte, 192)
	copy(head, "INDX")
	putU32(head, 4, 192)
	putU32(head, 24, 1)
	putU32(head, 28, 65001)
	putU32(head, 36, len(entries))
	putU32(head, 52, len(cncx))
	putU32(head, 180, 192)
	tagx := []byte("TAGX")
	tagx = binary.BigEndian.AppendUint32(tagx, uint32(12+4*(len(tags)+1)))
	tagx = binary.BigEndian.AppendUint32(tagx, 1)
	for _, t := range tags {
		tagx = append(tagx, t.tag, t.values, t.mask, 0)
	}
	tagx = append(tagx, 0, 0, 0, 1)
	return append([][]byte{append(head, tagx...), testIndexData(entries)}, cncx...)
}

// testIndexData is one data record of entries, with its IDXT table.
func testIndexData(entries [][]byte) []byte {
	rec := make([]byte, 192)
	copy(rec, "INDX")
	putU32(rec, 4, 192)
	var offs []int
	for _, e := range entries {
		offs = append(offs, len(rec))
		rec = append(rec, e...)
	}
	putU32(rec, 20, len(rec))
	putU32(rec, 24, len(entries))
	rec = append(rec, "IDXT"...)
	for _, o := range offs {
		rec = binary.BigEndian.AppendUint16(rec, uint16(o))
	}
	return rec
}

// testCNCX is a CNCX record of labels and each label's offset.
func testCNCX(labels ...string) ([]byte, []uint32) {
	var rec []byte
	var offs []uint32
	for _, l := range labels {
		offs = append(offs, uint32(len(rec)))
		rec = append(rec, fwd(uint32(len(l)))...)
		rec = append(rec, l...)
	}
	return rec, offs
}

var (
	ncxTags  = []testTag{{1, 1, 1}, {3, 1, 2}, {4, 1, 4}}
	skelTags = []testTag{{1, 1, 1}, {6, 2, 2}}
	fragTags = []testTag{{6, 2, 1}}
)

// testNCX is a contents entry of a test book: the part it points into,
// its label and depth.
type testNCX struct {
	part  int
	label string
	depth uint32
}

var kf8RefRE = regexp.MustCompile(`@@(\w+)@@`)

// kf8Book is a KF8 book of parts, each a file's skeleton and its one
// fragment, with skeleton, fragment and contents indexes. A link's href
// written @@id@@ becomes the kindle:pos of the element with that id.
func kf8Book(t testing.TB, parts [][2]string, ncx []testNCX) mobiSpec {
	t.Helper()
	const dummy = "kindle:pos:fid:0000:off:0000000000"
	var names []string
	frags := make([]string, len(parts))
	for i, p := range parts {
		for _, m := range kf8RefRE.FindAllStringSubmatch(p[1], -1) {
			names = append(names, m[1])
		}
		frags[i] = kf8RefRE.ReplaceAllString(p[1], dummy)
	}
	pos := map[string]string{}
	for _, n := range names {
		for fid, f := range frags {
			if i := strings.Index(f, `id="`+n+`"`); i >= 0 {
				at := strings.LastIndexByte(f[:i], '<')
				pos[n] = fmt.Sprintf("kindle:pos:fid:%04s:off:%010s", strings.ToUpper(strconv.FormatInt(int64(fid), 32)), strings.ToUpper(strconv.FormatInt(int64(at), 32)))
			}
		}
		if pos[n] == "" {
			t.Fatalf("no element has id %q", n)
		}
	}
	k := 0
	var text strings.Builder
	var skels, fragEntries [][]byte
	var starts []int
	for i, p := range parts {
		f := kf8RefRE.ReplaceAllStringFunc(p[1], func(string) string { k++; return pos[names[k-1]] })
		starts = append(starts, text.Len())
		skels = append(skels, testEntry(fmt.Sprintf("SKEL%010d", i), 3, 1, uint32(text.Len()), uint32(len(p[0]))))
		text.WriteString(p[0])
		fragEntries = append(fragEntries, testEntry(strconv.Itoa(text.Len()), 1, 0, uint32(len(f))))
		text.WriteString(f)
	}
	var labels []string
	for _, e := range ncx {
		labels = append(labels, e.label)
	}
	cncx, offs := testCNCX(labels...)
	var ncxEntries [][]byte
	for i, e := range ncx {
		ncxEntries = append(ncxEntries, testEntry(fmt.Sprintf("%02d", i), 7, uint32(starts[e.part]+len(parts[e.part][0])), offs[i], e.depth))
	}
	fdst := []byte("FDST")
	fdst = binary.BigEndian.AppendUint32(fdst, 12)
	fdst = binary.BigEndian.AppendUint32(fdst, 1)
	fdst = binary.BigEndian.AppendUint32(fdst, 0)
	fdst = binary.BigEndian.AppendUint32(fdst, uint32(text.Len()))
	s := mobiSpec{text: []byte(text.String()), version: 8, recordSize: 4096, extraFlags: 3, fdstRecord: fdst,
		skelIndex: testIndex(skelTags, skels), fragIndex: testIndex(fragTags, fragEntries)}
	if len(ncx) > 0 {
		s.ncxIndex = testIndex(ncxTags, ncxEntries, cncx)
	}
	return s
}

const kf8Skel = `<?xml version="1.0"?><html><head><title>T</title></head><body></body></html>`

// kf8NotesBook is a KF8 book of three parts: one with a heading, a
// footnote reference and a cross-reference; one without a heading that
// holds the note; one with a heading of its own and a contents entry.
func kf8NotesBook(t testing.TB) mobiSpec {
	return kf8Book(t, [][2]string{
		{kf8Skel, `<h1>Test_Book</h1><h2>One</h2><p>A note<a id="r1" epub_type="noteref" href="@@n1@@"><sup>1</sup></a> here, and <a href="@@p2@@">2</a> there.</p>`},
		{kf8Skel, `<p id="p2">Untitled part text.</p><aside id="n1" epub_type="footnote"><p><a href="@@r1@@">1</a> The note text.</p></aside><aside epub_type="footnote"><p>Orphan aside.</p></aside>`},
		{kf8Skel, `<h2>Own heading</h2><p>More text.</p>`},
	}, []testNCX{{0, "One", 0}, {1, "Part Two", 0}, {2, "Unused label", 1}})
}

// A KF8 book's contents label is the heading of a part that has none, as
// an EPUB nav label is; a part with its own heading keeps it. A typed
// note a reference points at is written [n] and listed under Notes; a
// plain cross-reference stays text, and a typed note nothing refers to
// stays where it is.
func TestMOBI_KF8ContentsLabelsAndFootnotes(t *testing.T) {
	s, rep := run(t, MOBI{}, kf8NotesBook(t).build(), Options{Name: "b.azw3"})
	want := "# Test_Book\n\n### One\n\nA note[1] here, and 2 there.\n\n## Part Two\n\nUntitled part text.\n\nOrphan aside.\n\n### Own heading\n\nMore text.\n\n## Notes\n\n[1] The note text."
	if got := s.md.String(); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if len(rep.Notes) != 0 {
		t.Errorf("notes %v", rep.Notes)
	}
}

// The Calibre fixtures (testdata/mobi/src/book.html) mark three footnotes
// the EPUB 3 way and a fourth chapter with a styled paragraph for a title:
// MOBI 7 and KF8 both write the references [n] and list the notes once;
// the cross-references stay text; KF8 heads the fourth chapter with its
// contents label.
func TestMOBI_CalibreFootnotesAndLabels(t *testing.T) {
	for _, f := range []string{"book-mobi7.mobi", "book-kf8.azw3", "book-hybrid.mobi"} {
		data, err := os.ReadFile("testdata/mobi/" + f)
		if err != nil {
			t.Fatal(err)
		}
		s, _ := run(t, MOBI{}, data, Options{Name: f})
		md := s.md.String()
		for _, want := range []string{
			"lantern harbour[1] thread", "Thread signal[2] window", "Window winter[3] signal",
			"As chapter 3 tells, and the next chapter repeats, thread",
			"## Notes\n\n[1] The first note: a lantern kept in the harbour ledger.\n\n[2] The second note, with italic words and a café.\n\n[3] The third note, at the end of chapter three.",
		} {
			if !strings.Contains(md, want) {
				t.Errorf("%s: no %q in:\n%s", f, want, md)
			}
		}
		for _, once := range []string{"The first note", "The second note", "The third note"} {
			if c := strings.Count(md, once); c != 1 {
				t.Errorf("%s: %q appears %d times, want once (under Notes)", f, once, c)
			}
		}
		if label := strings.Contains(md, "## Chapter 4: Part Four\n"); label == (f == "book-mobi7.mobi") {
			t.Errorf("%s: the fourth chapter's contents label as a heading: %v", f, label)
		}
		if strings.Contains(md, "## Chapter 1") && !strings.Contains(md, "### Chapter 1") {
			t.Errorf("%s: a chapter with a heading of its own was given its label", f)
		}
	}
}

// A book cut at the output limit lists no notes: the notes are written
// once the text is read, and the text was not.
func TestMOBI_CutBookListsNoNotes(t *testing.T) {
	for _, f := range []string{"book-mobi7.mobi", "book-kf8.azw3"} {
		data, err := os.ReadFile("testdata/mobi/" + f)
		if err != nil {
			t.Fatal(err)
		}
		s := &memSink{limit: 1500}
		_, err = MOBI{}.Extract(context.Background(), bytes.NewReader(data), int64(len(data)), s, Options{Name: f})
		if err == nil {
			t.Fatalf("%s: not cut", f)
		}
		md := s.md.String()
		if !strings.Contains(md, "harbour[1]") || strings.Contains(md, "Notes") || strings.Contains(md, "[2]") || strings.Contains(md, "first note") {
			t.Errorf("%s: cut book:\n%s", f, md)
		}
	}
}

// mobi7Linked is MOBI 7 text with its links resolved: @name@ marks a place
// (and is removed), #name# becomes that place's offset, ten digits.
func mobi7Linked(t testing.TB, s string) string {
	t.Helper()
	at := map[string]int{}
	marks := regexp.MustCompile(`@(\w+)@`)
	refs := regexp.MustCompile(`#(\w+)#`)
	plain := refs.ReplaceAllString(s, "0000000000")
	for {
		m := marks.FindStringSubmatchIndex(plain)
		if m == nil {
			break
		}
		at[plain[m[2]:m[3]]] = m[0]
		plain = plain[:m[0]] + plain[m[1]:]
	}
	out := marks.ReplaceAllString(s, "")
	return refs.ReplaceAllStringFunc(out, func(r string) string {
		p, ok := at[strings.Trim(r, "#")]
		if !ok {
			t.Fatalf("no place %s", r)
		}
		return fmt.Sprintf("%010d", p)
	})
}

// MOBI 7 keeps no marks, so a reference is a superscript link forward to a
// block that links back to it. A link without <sup>, one pointing back, or
// one whose target does not link back is not a note: its text stays, and
// so does the target's.
func TestMOBI_Mobi7FootnotesNeedASupAndALinkBack(t *testing.T) {
	text := mobi7Linked(t, `<html><head><guide></guide></head><body>@top@<p>Intro<sup>@r1@<a filepos=#n1#>1</a></sup> text,`+
		` see <a filepos=#ch#>2</a>, <a filepos=#n1#>again</a>, <sup><a filepos=#n1#>a longer phrase</a></sup>, <sup><a filepos=#top#>0</a></sup> and <sup><a filepos=#nb#>9</a></sup>.</p>`+
		`@ch@<p><font size="5"><b>Chapter Two</b></font></p><p>Body.</p>`+
		`@n1@<p><a filepos=#r1#>1</a> Note one text.</p>`+
		`@nb@<p><a filepos=#ch#>x</a> Not a note: its link goes elsewhere.</p></body></html>`)
	s, _ := run(t, MOBI{}, mobiSpec{text: []byte(text), extraFlags: 3}.build(), Options{Name: "b.mobi"})
	want := "# Test_Book\n\nIntro[1] text, see 2, again, a longer phrase, 0 and [2].\n\n### Chapter Two\n\nBody.\n\nx Not a note: its link goes elsewhere.\n\n## Notes\n\n[1] Note one text."
	if got := s.md.String(); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// A note's text is capped at maxMOBINote bytes, cut at a character.
func TestMOBI_NoteTextIsBounded(t *testing.T) {
	long := strings.Repeat("é", maxMOBINote)
	text := mobi7Linked(t, `<html><body><p>A<sup>@r1@<a filepos=#n1#>1</a></sup>.</p>@n1@<p><a filepos=#r1#>1</a> `+long+`</p></body></html>`)
	s, _ := run(t, MOBI{}, mobiSpec{text: []byte(text), extraFlags: 3}.build(), Options{Name: "b.mobi"})
	_, note, ok := strings.Cut(s.md.String(), "[1] ")
	if !ok || len(note) > maxMOBINote || len(note) < maxMOBINote-4 || strings.ContainsRune(note, '�') {
		t.Errorf("note of %d bytes (ok %v), want at most %d, whole characters", len(note), ok, maxMOBINote)
	}
}

// The notes kept are capped at maxMOBINoteBytes in all: the notes past it
// are left out with a note, and the text is still read.
func TestMOBI_NotesPastTheByteCapAreLeftOut(t *testing.T) {
	const n = maxMOBINoteBytes/maxMOBINote + 2
	var refs, notes strings.Builder
	body := strings.Repeat("w", maxMOBINote-8)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&refs, "<sup>@r%d@<a filepos=#n%d#>%d</a></sup> ", i, i, i+1)
		fmt.Fprintf(&notes, "@n%d@<p><a filepos=#r%d#>%d</a> %s</p>", i, i, i+1, body)
	}
	text := mobi7Linked(t, "<html><body><p>Refs "+refs.String()+"end.</p>"+notes.String()+"</body></html>")
	s, rep := run(t, MOBI{}, mobiSpec{text: []byte(text), compression: palmStored, extraFlags: 3}.build(), Options{Name: "b.mobi"})
	md := s.md.String()
	kept := strings.Count(md, "\n\n[")
	if kept == 0 || kept >= n || !strings.Contains(strings.Join(rep.Notes, "\n"), "notes past 8388608 bytes of notes were left out") {
		t.Errorf("%d of %d notes listed; notes %v", kept, n, rep.Notes)
	}
}

// A block is marked at most maxMOBIMarks times: a block of nothing but
// links grows by a bounded amount before it is parsed.
func TestMOBI_MarksPerBlockAreCapped(t *testing.T) {
	seg := []byte(strings.Repeat("<a filepos=1>", maxMOBIMarks+100))
	out := newMOBINotes(nil).mark(seg, 0)
	if c := bytes.Count(out, []byte(mobiPosAttr)); c != maxMOBIMarks {
		t.Errorf("%d marks, want %d", c, maxMOBIMarks)
	}
}

// readTestIndex reads an index built as records 1.. of a database.
func readTestIndex(t testing.TB, recs [][]byte, max int) (*mobiIndex, error) {
	t.Helper()
	all := append([][]byte{[]byte("r0")}, recs...)
	data := palmDBOf("I", "BOOKMOBI", all...)
	db, err := openPalmDB(context.Background(), bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	return readMOBIIndex(db, 1, max)
}

func wantIndexRefused(t *testing.T, what string, recs [][]byte, max int, reason string) {
	t.Helper()
	_, err := readTestIndex(t, recs, max)
	if !IsRefused(err) || !strings.Contains(err.Error(), reason) {
		t.Errorf("%s: err = %v, want a refusal naming %q", what, err, reason)
	}
}

func TestMOBIIndex_EntriesAreRead(t *testing.T) {
	cncx, offs := testCNCX("One", "Two")
	ix, err := readTestIndex(t, testIndex(ncxTags, [][]byte{testEntry("0", 7, 10, offs[0], 0), testEntry("1", 3, 300, offs[1])}, cncx), maxMOBIContents)
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.entries) != 2 {
		t.Fatalf("%d entries", len(ix.entries))
	}
	off, _ := ix.entries[1].value(1, 0)
	l, _ := ix.entries[1].value(3, 0)
	label, err := ix.label(l)
	if _, has := ix.entries[1].value(4, 0); off != 300 || label != "Two" || err != nil || has {
		t.Errorf("entry 1: off %d label %q (%v), depth present %v", off, label, err, has)
	}
}

// An index is bounded in records, entries, tag definitions, control bytes,
// values and string lengths; one past a bound is refused, and one at it is
// read.
func TestMOBIIndex_BoundsAreEnforced(t *testing.T) {
	entries := func(n int) [][]byte {
		var es [][]byte
		for i := 0; i < n; i++ {
			es = append(es, testEntry("", 1, uint32(i)))
		}
		return es
	}
	one := []testTag{{1, 1, 1}}
	if _, err := readTestIndex(t, testIndex(one, entries(maxMOBIContents)), maxMOBIContents); err != nil {
		t.Errorf("%d entries: %v", maxMOBIContents, err)
	}
	wantIndexRefused(t, "entries", testIndex(one, entries(maxMOBIContents+1)), maxMOBIContents, "more than 5000 entries")

	recs := testIndex(one, entries(1))
	putU32(recs[0], 24, maxMOBIIndexRecords+1)
	wantIndexRefused(t, "data records", recs, maxMOBIContents, "data records")
	recs = testIndex(one, entries(1))
	putU32(recs[0], 52, maxMOBICNCXRecords+1)
	wantIndexRefused(t, "string records", recs, maxMOBIContents, "string records")
	recs = testIndex(one, entries(1))
	putU32(recs[0], 52, 2)
	wantIndexRefused(t, "records past the book", recs, maxMOBIContents, "run past the book")

	recs = testIndex(one, entries(1))
	putU32(recs[0], 192+8, maxMOBIControlBytes+1)
	wantIndexRefused(t, "control bytes", recs, maxMOBIContents, "declares 5 control bytes, not 1 to 4")
	many := make([]testTag, maxMOBITagX)
	for i := range many {
		many[i] = testTag{byte(i + 1), 1, 1}
	}
	wantIndexRefused(t, "tag definitions", testIndex(many, entries(1)), maxMOBIContents, "TAGX section")
	recs = testIndex(one, entries(1))
	putU32(recs[0], 192+8, 2) // two control bytes, one end marker
	wantIndexRefused(t, "end markers", recs, maxMOBIContents, "ends 1 control bytes, and declares 2")
	// A tag after the last end marker has no control byte to be read from
	// (found by FuzzMOBIIndex; testdata/fuzz/FuzzMOBIIndex keeps the input).
	recs = testIndex(one, entries(1))
	copy(recs[0][192+12:], []byte{0, 0, 0, 1, 1, 1, 1, 0})
	wantIndexRefused(t, "a tag after the end marker", recs, maxMOBIContents, "a tag after its last control byte")

	// IDXT offsets out of order: the second entry before the first.
	recs = testIndex(one, entries(2))
	d := recs[1]
	idxt := int(binary.BigEndian.Uint32(d[20:]))
	a, b := binary.BigEndian.Uint16(d[idxt+4:]), binary.BigEndian.Uint16(d[idxt+6:])
	binary.BigEndian.PutUint16(d[idxt+4:], b)
	binary.BigEndian.PutUint16(d[idxt+6:], a)
	wantIndexRefused(t, "entry offsets", recs, maxMOBIContents, "out of order")
	recs = testIndex(one, entries(1))
	binary.BigEndian.PutUint16(recs[1][binary.BigEndian.Uint32(recs[1][20:])+4:], 4)
	wantIndexRefused(t, "an entry in the header", recs, maxMOBIContents, "out of order")
	recs = testIndex(one, entries(1))
	putU32(recs[1], 20, len(recs[1]))
	wantIndexRefused(t, "IDXT outside", recs, maxMOBIContents, "IDXT table")
	recs = testIndex(one, entries(1))
	copy(recs[1][binary.BigEndian.Uint32(recs[1][20:]):], "IDXX")
	wantIndexRefused(t, "IDXT magic", recs, maxMOBIContents, "IDXT table")

	// Values: a count past the entry, a value of five bytes.
	wantIndexRefused(t, "value count", testIndex([]testTag{{1, 1, 3}}, [][]byte{testEntry("", 2, 1)}), maxMOBIContents, "values run past the entry")
	five := append(testEntry("", 1), 0x01, 0x01, 0x01, 0x01, 0x81)
	wantIndexRefused(t, "five-byte value", testIndex(one, [][]byte{five}), maxMOBIContents, "values run past the entry")
	sized := append(testEntry("", 3), 0x84, 0x81) // 4 bytes of values, 1 held
	wantIndexRefused(t, "value size", testIndex([]testTag{{1, 1, 3}}, [][]byte{sized}), maxMOBIContents, "an index entry's values run past the entry")
}

// A contents label is at most maxMOBILabel bytes, and the contents nest at
// most maxMOBIContentsDepth deep; past either, the contents are refused.
func TestMOBIIndex_LabelLengthAndDepthAreCapped(t *testing.T) {
	label := func(l string, depth uint32) error {
		cncx, offs := testCNCX(l)
		recs := testIndex(ncxTags, [][]byte{testEntry("0", 7, 0, offs[0], depth)}, cncx)
		all := append([][]byte{[]byte("r0")}, recs...)
		data := palmDBOf("I", "BOOKMOBI", all...)
		db, err := openPalmDB(context.Background(), bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		tabs := &kf8Tables{files: []kf8File{{start: 0, end: 10}}}
		return tabs.addLabels(db, 1)
	}
	if err := label(strings.Repeat("x", maxMOBILabel), maxMOBIContentsDepth); err != nil {
		t.Errorf("a label at the caps: %v", err)
	}
	if err := label(strings.Repeat("x", maxMOBILabel+1), 0); !IsRefused(err) || !strings.Contains(err.Error(), "more than 1024") {
		t.Errorf("a label past the cap: %v", err)
	}
	if err := label("x", maxMOBIContentsDepth+1); !IsRefused(err) || !strings.Contains(err.Error(), "nest 17 deep") {
		t.Errorf("contents past the depth cap: %v", err)
	}
}

// An index the reader refuses leaves the book read without it, with a
// note: a malformed contents index loses the labels, a malformed fragment
// index the footnotes as well.
func TestMOBI_MalformedIndexesLeaveTheTextRead(t *testing.T) {
	spec := kf8NotesBook(t)
	putU32(spec.ncxIndex[0], 24, maxMOBIIndexRecords+1)
	s, rep := run(t, MOBI{}, spec.build(), Options{Name: "b.azw3"})
	if md := s.md.String(); strings.Contains(md, "Part Two") || !strings.Contains(md, "A note[1] here") || len(rep.Notes) != 1 || !strings.Contains(rep.Notes[0], "contents index was not read") {
		t.Errorf("bad contents index:\n%s\nnotes %v", md, rep.Notes)
	}
	spec = kf8NotesBook(t)
	spec.fragIndex[1] = spec.fragIndex[1][:200]
	s, rep = run(t, MOBI{}, spec.build(), Options{Name: "b.azw3"})
	if md := s.md.String(); strings.Contains(md, "Part Two") || strings.Contains(md, "[1]") || !strings.Contains(md, "The note text.") || len(rep.Notes) != 1 || !strings.Contains(rep.Notes[0], "fragment indexes were not read") {
		t.Errorf("bad fragment index:\n%s\nnotes %v", md, rep.Notes)
	}
}

// The skeleton index names its files in order, each with a count of the
// fragments that follow it; a count past the fragment index, or a file
// that starts before the last one ended, is refused.
func TestMOBI_KF8TablesRefuseAMismatchedSkeleton(t *testing.T) {
	tables := func(skel, frag [][]byte) error {
		recs := append([][]byte{[]byte("r0")}, testIndex(skelTags, skel)...)
		recs = append(recs, testIndex(fragTags, frag)...)
		data := palmDBOf("I", "BOOKMOBI", recs...)
		db, err := openPalmDB(context.Background(), bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		_, err = readKF8Tables(db, 1, 3)
		return err
	}
	frag := [][]byte{testEntry("1", 1, 0, 5)}
	if err := tables([][]byte{testEntry("S0", 3, 1, 0, 10)}, frag); err != nil {
		t.Errorf("a well-formed pair: %v", err)
	}
	if err := tables([][]byte{testEntry("S0", 3, 2, 0, 10)}, frag); !IsRefused(err) || !strings.Contains(err.Error(), "more fragments") {
		t.Errorf("two fragments named, one held: %v", err)
	}
	if err := tables([][]byte{testEntry("S0", 3, 1, 0, 10), testEntry("S1", 3, 0, 4, 10)}, frag); !IsRefused(err) || !strings.Contains(err.Error(), "out of order") {
		t.Errorf("a file inside the last: %v", err)
	}
}

// A kindle:pos link resolves through its fragment to a text offset; a
// fragment the book does not hold, an offset past it or a number past 32
// bits resolves to nothing.
func TestMOBI_KindlePosResolvesInsideItsFragment(t *testing.T) {
	tabs := &kf8Tables{frags: []kf8Frag{{start: 100, length: 50}, {start: 200, length: 10}}}
	for href, want := range map[string]int64{
		"kindle:pos:fid:0001:off:0000000009":        209,
		"KINDLE:POS:FID:0000:OFF:000000001A?mime=x": 142,
		"kindle:pos:fid:0001:off:000000000A":        -1,
		"kindle:pos:fid:0002:off:0000000000":        -1,
		"kindle:pos:fid:0000:off:VVVVVVVVVV":        -1,
		"kindle:pos:fid:0000":                       -1,
		"kindle:embed:0001":                         -1,
	} {
		got, ok := tabs.resolve(href)
		if !ok {
			got = -1
		}
		if got != want {
			t.Errorf("%s: %d, want %d", href, got, want)
		}
	}
}

// FuzzMOBIIndex fuzzes an index's header and data records and the
// contents labels read through them.
func FuzzMOBIIndex(f *testing.F) {
	cncx, offs := testCNCX("One", "Two")
	recs := testIndex(ncxTags, [][]byte{testEntry("0", 7, 10, offs[0], 0), testEntry("1", 7, 300, offs[1], 1)}, cncx)
	f.Add(recs[0], recs[1], recs[2])
	skel := testIndex(skelTags, [][]byte{testEntry("S", 3, 1, 0, 10)})
	f.Add(skel[0], skel[1], []byte{0})
	f.Fuzz(func(t *testing.T, head, data, strs []byte) {
		for _, r := range [][]byte{head, data, strs} {
			if len(r) == 0 {
				return // an empty record cannot be listed
			}
		}
		all := [][]byte{[]byte("r0"), head, data, strs}
		db := palmDBOf("I", "BOOKMOBI", all...)
		pdb, err := openPalmDB(context.Background(), bytes.NewReader(db), int64(len(db)))
		if err != nil {
			t.Fatal(err)
		}
		tabs := &kf8Tables{files: []kf8File{{start: 0, end: 1 << 20}}}
		if err := tabs.addLabels(pdb, 1); err != nil && !IsRefused(err) {
			t.Fatalf("not a refusal: %v", err)
		}
		if l := len(tabs.files[0].label); l > 4*MaxMetaString {
			t.Fatalf("label of %d bytes", l)
		}
		if ix, err := readMOBIIndex(pdb, 1, maxKF8Parts); err == nil && len(ix.entries) > maxKF8Parts {
			t.Fatalf("%d entries", len(ix.entries))
		}
	})
}
