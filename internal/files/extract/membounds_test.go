package extract

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"runtime"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Heap ceilings on the inputs that made one file cost far more memory than
// its size: an HTML table or element flood, one element with millions of
// attributes, formatting elements the HTML parser clones on every paragraph,
// and the same attribute flood inside a DOCX part. Each is refused, and the
// refusal itself costs little: the count runs before any tree is built.
//
// allocated reports the bytes the Go heap handed out while fn ran. It is the
// cumulative count (TotalAlloc), an upper bound on the peak, so a ceiling on
// it holds the peak too. The inputs are built before it starts.

func allocated(fn func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// peakHeap reports the most live heap seen while fn ran, above what was live
// when it started, sampled every 100µs: for a refusal whose cost is garbage
// the collector takes back, the peak is the bound that matters.
func peakHeap(fn func()) uint64 {
	runtime.GC()
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	read := func() uint64 {
		metrics.Read(sample)
		return sample[0].Value.Uint64()
	}
	base := read()
	var peak atomic.Uint64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(100 * time.Microsecond)
		defer t.Stop()
		for {
			if v := read(); v > peak.Load() {
				peak.Store(v)
			}
			select {
			case <-stop:
				return
			case <-t.C:
			}
		}
	}()
	fn()
	close(stop)
	<-done
	if p := peak.Load(); p > base {
		return p - base
	}
	return 0
}

// raceSlack scales a heap ceiling under the race detector, whose bookkeeping
// and the CI runner's noise move the measured peak by up to about a quarter
// (a 205 MiB local peak read 256 MiB under -race in CI). Every ceiling still
// sits far under the unfixed cost it guards against.
const raceSlack = 2

// ceiling is n, scaled under the race detector.
func ceiling(n uint64) uint64 {
	if raceEnabled {
		return n * raceSlack
	}
	return n
}

func extractBytes(e Extractor, name string, data []byte) (Report, error) {
	return e.Extract(context.Background(), bytes.NewReader(data), int64(len(data)), &memSink{}, Options{Name: name, Images: true})
}

func wantRefusedUnder(t *testing.T, label string, limit uint64, fn func() error) {
	t.Helper()
	var err error
	got := allocated(func() { err = fn() })
	if !IsRefused(err) {
		t.Fatalf("%s: err = %v, want a refusal", label, err)
	}
	if ceiling := ceiling(limit); got > ceiling {
		t.Fatalf("%s: refusing it allocated %d MiB, want at most %d MiB", label, got>>20, ceiling>>20)
	}
	t.Logf("%s: refused (%v) after %d MiB allocated", label, err, got>>20)
}

func TestHTML_OverSizeCapRefused(t *testing.T) {
	// The reviewer's 31 MiB table flood: over the page cap, refused unread.
	page := []byte("<table><tr>" + strings.Repeat("<td>", 31<<20/4))
	wantRefusedUnder(t, "31 MiB table flood", 1<<20, func() error {
		_, err := extractBytes(HTML{}, "flood.html", page)
		return err
	})
	if maxHTMLBytes > 8<<20 {
		t.Fatalf("maxHTMLBytes = %d, want at most 8 MiB", maxHTMLBytes)
	}
}

func TestHTML_TableCellFloodRefusedBeforeParse(t *testing.T) {
	// Under the size cap, one row of two million cells: the table walk keeps
	// only the first 200 columns, so the cells past them were never counted
	// against the element cap while the parser built a node for each.
	page := []byte("<table><tr>" + strings.Repeat("<td>", (8<<20-64)/4))
	wantRefusedUnder(t, "table cell flood", 32<<20, func() error {
		_, err := extractBytes(HTML{}, "cells.html", page)
		return err
	})
}

func TestHTML_AttributeFloodRefusedBeforeParse(t *testing.T) {
	// One element with as many distinct attributes as the page cap allows
	// (repeated names are dropped by the tokenizer, so they cost nothing).
	// The count holds one tag's attributes while it reads them; what it
	// refuses is the parse, which would keep them all as nodes' attributes.
	var b strings.Builder
	b.WriteString("<p ")
	for i := 0; b.Len() < 8<<20-16; i++ {
		b.WriteString("a")
		b.WriteString(strconv.FormatInt(int64(i), 36))
		b.WriteString(" ")
	}
	b.WriteString(">")
	page := []byte(b.String())
	var err error
	peak := peakHeap(func() { _, err = extractBytes(HTML{}, "attrs.html", page) })
	if !IsRefused(err) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if c := ceiling(256 << 20); peak > c {
		t.Fatalf("refusing it held %d MiB at its peak, want at most %d MiB", peak>>20, c>>20)
	}
	t.Logf("attribute flood: refused (%v) at a peak of %d MiB", err, peak>>20)
}

func TestHTML_ReconstructedFormattingRefusedBeforeParse(t *testing.T) {
	// Formatting elements left open inside a paragraph are cloned by the
	// parser after every paragraph that closes them: 300 distinct <b>s and
	// ten thousand paragraphs are three million nodes from 200 KB of page.
	var b strings.Builder
	b.WriteString("<p>")
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&b, "<b class=c%d>", i)
	}
	b.WriteString(strings.Repeat("</p><p>x", 10000))
	page := []byte(b.String())
	wantRefusedUnder(t, "reconstructed formatting", 32<<20, func() error {
		_, err := extractBytes(HTML{}, "clones.html", page)
		return err
	})
}

// A book's content document is held to the same pre-parse bounds as a web
// page: the clone flood is refused before the parser builds it, and the
// book with it.
func TestEPUB_ReconstructedFormattingRefusedBeforeParse(t *testing.T) {
	var b strings.Builder
	b.WriteString("<p>")
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&b, "<b class=c%d>", i)
	}
	b.WriteString(strings.Repeat("</p><p>x", 10000))
	book := epubOf(t, epubContainer("c.opf"),
		xmlEntry("c.opf", `<package xmlns="http://www.idpf.org/2007/opf"><metadata/><manifest><item id="a" href="a.xhtml" media-type="application/xhtml+xml"/></manifest><spine><itemref idref="a"/></spine></package>`),
		entry{"a.xhtml", []byte(b.String())})
	wantRefusedUnder(t, "reconstructed formatting in a book", 32<<20, func() error {
		_, err := extractBytes(EPUB{}, "clones.epub", book)
		return err
	})
}

func TestHTML_PageWithFormattingStillExtracts(t *testing.T) {
	// The formatting count is not a refusal of formatting: closed tags, a few
	// left open, and the same tag repeated (the parser keeps three) pass.
	var b strings.Builder
	b.WriteString("<title>T</title><p><font face=x>")
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&b, "<p><b>bold %d</b> <i>it</i> <font color=red>red</font><a href=/x>l</a></p>", i)
	}
	for i := 0; i < 50; i++ {
		b.WriteString("<p><span><b>left open")
	}
	s, _ := run(t, HTML{}, []byte(b.String()), Options{Name: "ok.html"})
	if !strings.Contains(s.md.String(), "bold 1999") {
		t.Fatalf("the page was not extracted:\n%.300s", s.md.String())
	}
}

// cancelAfterReader serves r and cancels once more than after bytes have
// been read in all, counting what it serves after that: the extraction's
// deadline arrives while the file is being read.
type cancelAfterReader struct {
	r        io.ReaderAt
	after    int64
	cancel   func()
	read     atomic.Int64
	fired    atomic.Bool
	lateRead atomic.Int64
}

func (c *cancelAfterReader) ReadAt(p []byte, off int64) (int, error) {
	if c.fired.Load() {
		c.lateRead.Add(int64(len(p)))
	}
	if c.read.Add(int64(len(p))) > c.after && c.fired.CompareAndSwap(false, true) {
		c.cancel()
	}
	return c.r.ReadAt(p, off)
}

func TestHTML_ParseStopsAtTheDeadline(t *testing.T) {
	// Few elements (the page walk would never reach its context check) and
	// many bytes. The page is read once, to be decoded, and that read stops
	// within a read when the deadline comes; the count and the parse work on
	// the decoded copy and stop at the deadline too.
	page := []byte("<p>" + strings.Repeat("word ", (6<<20)/5) + "</p>")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := parseHTMLBytes(ctx, page); !errors.Is(err, context.Canceled) {
		t.Fatalf("parse after the deadline: err = %v, want the context's error", err)
	}
	for _, c := range []struct {
		name  string
		after int64
	}{{"while reading", 64 << 10}} {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := &cancelAfterReader{r: bytes.NewReader(page), after: c.after, cancel: cancel}
			start := time.Now()
			_, err := HTML{}.Extract(ctx, r, int64(len(page)), &memSink{}, Options{Name: "slow.html"})
			if !r.fired.Load() {
				t.Fatal("the deadline never came: the page was read only once")
			}
			if late := r.lateRead.Load(); late > 256<<10 {
				t.Fatalf("%d KiB were read after the deadline, want the read to stop within one", late>>10)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want the context's error", err)
			}
			t.Logf("stopped %v after the start, %d KiB read past the deadline", time.Since(start), r.lateRead.Load()>>10)
		})
	}
}

// attrFlood is one element carrying n distinct attributes.
func attrFlood(n int) string {
	var b strings.Builder
	b.Grow(n * 10)
	b.WriteString("<w:p ")
	for i := 0; i < n; i++ {
		b.WriteString("a")
		b.WriteString(strconv.FormatInt(int64(i), 36))
		b.WriteString(`="" `)
	}
	b.WriteString("/>")
	return b.String()
}

func TestParseXML_AttributeFloodRefusedBeforeDecode(t *testing.T) {
	part := []byte(`<w:document ` + nsW + `><w:body>` + attrFlood(2_000_000) + `</w:body></w:document>`)
	wantRefusedUnder(t, "one element, 2M attributes", 16<<20, func() error {
		_, err := parseXML(context.Background(), part)
		return err
	})
}

func TestParseXML_AttributeTotalCapped(t *testing.T) {
	// Many elements, each with an ordinary count, past the part's total.
	var b strings.Builder
	b.WriteString(`<w:document ` + nsW + `><w:body>`)
	el := attrFlood(1000)
	for i := 0; i <= maxXMLAttrs/1000; i++ {
		b.WriteString(el)
	}
	b.WriteString(`</w:body></w:document>`)
	part := []byte(b.String())
	wantRefusedUnder(t, "attribute total", 16<<20, func() error {
		_, err := parseXML(context.Background(), part)
		return err
	})
}

func TestParseXML_AttributeValueBytesCapped(t *testing.T) {
	big := strings.Repeat("v", 1<<20)
	var b strings.Builder
	b.WriteString(`<r>`)
	for i := 0; i <= maxXMLAttrBytes>>20; i++ {
		b.WriteString(`<e a="` + big + `"/>`)
	}
	b.WriteString(`</r>`)
	part := []byte(b.String())
	wantRefusedUnder(t, "attribute value bytes", 16<<20, func() error {
		_, err := parseXML(context.Background(), part)
		return err
	})
}

func TestHTML_TextRunsCountAsNodes(t *testing.T) {
	// An element and a text run each: the walk counts both, so the count does.
	page := []byte(strings.Repeat("<i>x</i>", maxXMLNodes/2+1000))
	wantRefusedUnder(t, "element and text flood", 32<<20, func() error {
		_, err := extractBytes(HTML{}, "e.html", page)
		return err
	})
}

func TestScanSheet_AttributeFloodRefused(t *testing.T) {
	sheet := []byte(`<worksheet><sheetData><row r="1">` + strings.Replace(attrFlood(1_000_000), "<w:p ", "<c ", 1) + `</row></sheetData></worksheet>`)
	wantRefusedUnder(t, "worksheet attribute flood", 16<<20, func() error {
		_, _, err := scanSheet(context.Background(), sheet, sharedTable{}, 10, func([]xlsxCell) error { return nil })
		return err
	})
	shared := []byte(`<sst>` + strings.Replace(attrFlood(1_000_000), "<w:p ", "<si ", 1) + `</sst>`)
	wantRefusedUnder(t, "shared strings attribute flood", 16<<20, func() error {
		_, err := sharedStrings(context.Background(), shared)
		return err
	})
}

func TestDOCX_AttributeFloodRefused(t *testing.T) {
	doc := docxWith(t, `<w:document `+nsW+`><w:body>`+attrFlood(2_000_000)+`</w:body></w:document>`)
	// The ceiling is the archive layer reading the 20 MiB part out of the zip
	// (about 64 MiB; ceiling doubles it under -race); the count adds nothing.
	wantRefusedUnder(t, "DOCX attribute flood", 96<<20, func() error {
		_, err := extractBytes(DOCX{}, "flood.docx", doc)
		return err
	})
}

func TestParseXML_QuoteInADoctypeCommentDoesNotHideAFlood(t *testing.T) {
	// A lone quote inside a comment of the internal subset: the decoder skips
	// the comment before it looks at quotes, so the count must too, or it
	// would read the rest of the part as a quoted string and miss the flood.
	part := []byte(`<!DOCTYPE r [<!-- it's -->]><r>` + attrFlood(2_000_000) + `</r>`)
	wantRefusedUnder(t, "flood after a doctype comment", 16<<20, func() error {
		_, err := parseXML(context.Background(), part)
		return err
	})
}

func TestCountXMLAttrs_UnterminatedMarkupFailsClosed(t *testing.T) {
	for _, part := range []string{
		`<r a="1`, `<r a="1"`, `<!-- open`, `<![CDATA[ x`, `<?pi`, `<!DOCTYPE r [ "x" `, `<!DOCTYPE r [<!-- x ]>`, `</r`,
	} {
		if _, _, _, err := countXMLAttrs(context.Background(), []byte(part)); !IsRefused(err) {
			t.Errorf("%q: err = %v, want a refusal", part, err)
		}
	}
}

func TestScanXMLAttrs_CountsOnlyRealAttributes(t *testing.T) {
	// '=' in text, comments, CDATA, processing instructions, a doctype and
	// quoted values is not an attribute; each real one is.
	part := []byte(`<?xml version="1.0"?><!DOCTYPE x [<!ENTITY e "a=b">]><!-- a=b c=d --><r x="1" y='a=b>c'>a=b<![CDATA[ c=d ]]><?pi a=b?><e z="2"/></r>`)
	n, per, vb, err := countXMLAttrs(context.Background(), part)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 || per != 2 || vb != len("1")+len("a=b>c")+len("2") {
		t.Fatalf("counted %d attributes, %d on the widest element, %d value bytes; want 3, 2 and 7", n, per, vb)
	}
}

func TestHTML_StrayEndTagsThatCreateElementsCounted(t *testing.T) {
	// In <body>, a stray </p> makes the parser create an empty <p> and a </br>
	// a <br>: end tags that build nodes count as nodes.
	for _, tag := range []string{"</p>", "</br>", "</P>"} {
		page := []byte("<body>" + strings.Repeat(tag, (8<<20-16)/len(tag)))
		wantRefusedUnder(t, tag+" flood", 32<<20, func() error {
			_, err := extractBytes(HTML{}, "stray.html", page)
			return err
		})
	}
}

func TestHTML_PeakAtTheNodeCapIsBounded(t *testing.T) {
	// The documented per-file HTML peak (SECURITY.md, limit.go: about 280
	// MiB): a page just under the node cap, built and walked.
	page := []byte(strings.Repeat("<i>x</i>", maxXMLNodes/2-1000))
	var err error
	peak := peakHeap(func() { _, err = extractBytes(HTML{}, "cap.html", page) })
	if err != nil {
		t.Fatalf("a page under the caps: %v", err)
	}
	if c := ceiling(320 << 20); peak > c {
		t.Fatalf("a page at the node cap held %d MiB at its peak, want at most %d MiB", peak>>20, c>>20)
	}
	t.Logf("peak %d MiB", peak>>20)
}

// A book's description is its own markup: a clone flood or a fragment of
// megabytes is not handed to the HTML parser, and the text kept is bounded
// (RFC GV §4.7).
func TestEPUB_DescriptionMarkupIsBoundedBeforeParse(t *testing.T) {
	var flood strings.Builder
	for i := 0; flood.Len() < maxDescriptionHTML/2; i++ {
		fmt.Fprintf(&flood, "<b class=c%d>", i)
	}
	for flood.Len() < maxDescriptionHTML-8 {
		flood.WriteString("</p><p>x")
	}
	for name, desc := range map[string]string{
		"clone flood": flood.String(),
		"oversized":   strings.Repeat("<p><i>a</i></p>", 2<<20/15),
		"deep":        strings.Repeat("<div>", 100000) + "x",
	} {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		got := htmlFragmentText(context.Background(), desc)
		runtime.ReadMemStats(&after)
		t.Logf("%s: %d bytes in, %d KiB allocated", name, len(desc), (after.TotalAlloc-before.TotalAlloc)>>10)
		if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 8<<20 {
			t.Errorf("%s: reading the description allocated %d MiB", name, alloc>>20)
		}
		if len(got) > MaxMetaDescription*2 {
			t.Errorf("%s: %d bytes of description text kept", name, len(got))
		}
		if strings.Contains(got, "<") {
			t.Errorf("%s: tags left in the text: %.80q", name, got)
		}
	}
	if got := htmlFragmentText(context.Background(), "<p>A <b>first</b> collection.</p>"); got != "A first collection." {
		t.Errorf("a small description = %q", got)
	}
}

// A book's element with millions of attributes is refused while its bytes
// stream past, before the XML decoder builds the attribute list (about ten
// times its bytes); a book is read as a stream, so no count runs ahead of
// it over the whole file.
func TestFB2_AttributeFloodRefusedBeforeDecode(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<FictionBook><body><p`)
	for i := 0; b.Len() < 24<<20; i++ {
		fmt.Fprintf(&b, ` a%d=""`, i)
	}
	b.WriteString(`>x</p></body></FictionBook>`)
	book := []byte(b.String())
	wantRefusedUnder(t, "attribute flood in a book", 1<<20, func() error {
		_, err := extractBytes(FB2{}, "flood.fb2", book)
		return err
	})
}

// rtfDiscardSink takes everything and keeps nothing but its size and the
// largest write.
type rtfDiscardSink struct{ n, most int }

func (s *rtfDiscardSink) Text(md string) error {
	s.n += len(md)
	s.most = max(s.most, len(md))
	return nil
}

func (s *rtfDiscardSink) Image(Image) (string, error) { return "", nil }

// An RTF paragraph of tens of megabytes, plain, in runs of alternating
// bold or in hard-broken lines, is written in pieces as it grows: the
// reader holds at most a piece, never the paragraph, whatever the output
// limit. No write is larger than a piece; the heap peak, which counts
// garbage not yet collected and so varies run to run, stays well under
// the paragraph's own size (holding it would cost twice that).
func TestRTF_LongParagraphIsHeldInPiecesOnly(t *testing.T) {
	const size = 48 << 20
	for name, unit := range map[string]string{"plain": "word ", "runs": `\b a\b0 b`, "lines": `a\line `} {
		doc := &repeatReaderAt{head: `{\rtf1 `, unit: unit, size: size}
		s := &rtfDiscardSink{}
		var err error
		peak := peakHeap(func() {
			_, err = RTF{}.Extract(context.Background(), doc, size, s, Options{Name: "p.rtf"})
		})
		if err != nil || s.n < size/4 {
			t.Fatalf("%s: err %v, wrote %d bytes", name, err, s.n)
		}
		if s.most > maxRTFPara+8<<10 { // a piece ends after the text that reached the cap
			t.Errorf("%s: one write of %d bytes, want pieces of at most %d", name, s.most, maxRTFPara)
		}
		if c := ceiling(48 << 20); peak > c {
			t.Errorf("%s: a 48 MiB paragraph peaked at %d MiB, want at most %d MiB", name, peak>>20, c>>20)
		}
	}
}

// repeatReaderAt is a file of head and then unit over and over, made as it
// is read, so a test's own input is not on the heap it measures.
type repeatReaderAt struct {
	head, unit string
	size       int64
}

func (r *repeatReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n := 0
	for n < len(p) && off < r.size {
		if off < int64(len(r.head)) {
			p[n] = r.head[off]
		} else {
			p[n] = r.unit[(off-int64(len(r.head)))%int64(len(r.unit))]
		}
		n++
		off++
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// Footnotes are the one text an RTF document holds until its end: each is
// cut at maxRTFNote and all of them at maxRTFNoteBytes.
func TestRTF_FootnotesAreHeldUnderTheirCaps(t *testing.T) {
	unit := `x{\footnote ` + strings.Repeat("n", maxRTFNote) + `}`
	doc := &repeatReaderAt{head: `{\rtf1 `, unit: unit, size: int64(7 + 200*len(unit))}
	s := &rtfDiscardSink{}
	var rep Report
	var err error
	peak := peakHeap(func() {
		rep, err = RTF{}.Extract(context.Background(), doc, doc.size, s, Options{Name: "n.rtf"})
	})
	if err != nil || len(rep.Notes) != 1 || !strings.Contains(rep.Notes[0], "footnotes past") {
		t.Fatalf("err %v, notes %v", err, rep.Notes)
	}
	if c := ceiling(48 << 20); peak > c {
		t.Errorf("200 footnotes of 64 KiB peaked at %d MiB, want at most %d MiB", peak>>20, c>>20)
	}
}

// An RTF table is written a row at a time and stops at the table caps: a
// table of more rows than the cells cap writes no more than the cap.
func TestRTF_TableRowsStopAtTheTableCaps(t *testing.T) {
	doc := []byte(`{\rtf1 ` + strings.Repeat(`\trowd\intbl a\cell\row`, maxTableCells+5000) + `}`)
	s := &rtfDiscardSink{}
	rep, err := RTF{}.Extract(context.Background(), bytes.NewReader(doc), int64(len(doc)), s, Options{Name: "t.rtf"})
	if err != nil || rep.Tables != 1 || len(rep.Notes) != 1 || !strings.Contains(rep.Notes[0], "table was cut") {
		t.Fatalf("err %v, report %+v", err, rep)
	}
	if want := maxTableCells * len("\n| a |"); s.n > want+100 {
		t.Errorf("wrote %d bytes, want at most about %d", s.n, want)
	}
	// One cell of a megabyte is cut at the cell cap.
	big := []byte(`{\rtf1 \trowd\intbl ` + strings.Repeat("c", 1<<20) + `\cell\row}`)
	s = &rtfDiscardSink{}
	_, err = RTF{}.Extract(context.Background(), bytes.NewReader(big), int64(len(big)), s, Options{Name: "t.rtf"})
	if err != nil || s.n > maxCellBytes+200 {
		t.Errorf("a 1 MiB cell: err %v, wrote %d bytes", err, s.n)
	}
}

// huffBomb is a book whose one text record expands each phrase into two
// copies of the next, 30 deep, over a 1 KiB literal: 2^30 KiB unbounded.
func huffBomb() []byte {
	h := &testHuff{}
	const depth = 30
	for i := 0; i < depth; i++ {
		h.add(nil, false)
	}
	h.add(bytes.Repeat([]byte("a"), 1024), true)
	for i := 0; i < depth; i++ {
		h.phrases[i] = h.codes(i+1, i+1)
	}
	r := mobiSpec{compression: palmHuffman, huff: h, text: []byte("x")}.records()
	r[1] = h.codes(0)
	binary.BigEndian.PutUint32(r[0][4:], 4096)
	return palmDBOf("Bomb", "BOOKMOBI", r...)
}

func TestMOBI_HuffCDICExpansionBombIsRefused(t *testing.T) {
	bomb := huffBomb()
	wantRefusedUnder(t, "HUFF/CDIC expansion bomb", 4<<20, func() error {
		_, err := extractBytes(MOBI{}, "bomb.mobi", bomb)
		if err != nil && !strings.Contains(err.Error(), "decompresses past") {
			t.Errorf("refused for another reason: %v", err)
		}
		return err
	})
}

// A MOBI book's text is held a block at a time: 60 MiB of text, read to
// the end, peaks far under its own size.
func TestMOBI_TextIsHeldABlockAtATime(t *testing.T) {
	const records = 15 << 8 // 3,840 records of 16 KiB: 60 MiB
	unit := []byte(strings.Repeat("<p>"+strings.Repeat("word ", 800)+"</p>", 4))
	unit = append(unit, bytes.Repeat([]byte(" "), 16<<10-len(unit))...)
	book := newStoredMOBI(unit, records)
	s := &rtfDiscardSink{}
	var err error
	peak := peakHeap(func() {
		_, err = MOBI{}.Extract(context.Background(), book, book.size, s, Options{Name: "big.mobi"})
	})
	if err != nil || s.n < 50<<20 {
		t.Fatalf("err %v, wrote %d bytes", err, s.n)
	}
	if c := ceiling(32 << 20); peak > c {
		t.Errorf("60 MiB of text peaked at %d MiB, want at most %d MiB", peak>>20, c>>20)
	}
}

// storedMOBI is a MOBI book of n stored text records, each unit, made as it
// is read, so the test's own input is not on the heap it measures.
type storedMOBI struct {
	head []byte // the database header, record list and record 0
	unit []byte
	size int64
}

func newStoredMOBI(unit []byte, n int) *storedMOBI {
	r0 := mobiSpec{text: []byte("x"), compression: palmStored}.records()[0]
	binary.BigEndian.PutUint32(r0[4:], uint32(n*len(unit)))
	binary.BigEndian.PutUint16(r0[8:], uint16(n))
	binary.BigEndian.PutUint16(r0[10:], uint16(min(len(unit), 0xffff)))
	head := make([]byte, palmHeaderSize)
	copy(head, "Big")
	copy(head[60:], "BOOKMOBI")
	binary.BigEndian.PutUint16(head[76:], uint16(n+1))
	off := palmHeaderSize + 8*(n+1)
	for i := 0; i <= n; i++ {
		head = binary.BigEndian.AppendUint32(head, uint32(off))
		head = binary.BigEndian.AppendUint32(head, uint32(i))
		if i == 0 {
			off += len(r0)
		} else {
			off += len(unit)
		}
	}
	head = append(head, r0...)
	return &storedMOBI{head: head, unit: unit, size: int64(len(head) + n*len(unit))}
}

func (m *storedMOBI) ReadAt(p []byte, off int64) (int, error) {
	n := 0
	for n < len(p) && off < m.size {
		if off < int64(len(m.head)) {
			p[n] = m.head[off]
		} else {
			p[n] = m.unit[(off-int64(len(m.head)))%int64(len(m.unit))]
		}
		n++
		off++
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// A help file whose compressed section is an LZX bomb: each page of its
// contents sits at the end of its own 4 MiB reset interval of one repeated
// byte, so reading a page decompresses the interval before it. Reading
// stops at the extraction's decompression cap (lowered here to 16 MiB)
// with little held: a frame and the window, never an interval.
func TestCHM_LZXExpansionBombIsRefused(t *testing.T) {
	const interval = 4 << 20
	const pages = 9
	stream, frames := bombFramesLZX(15, pages*interval/lzxFrameSize, interval/lzxFrameSize)
	parts := []chmPart{{name: "/#SYSTEM", data: chmSystemFile("toc.hhc", "p0.htm", "Bomb", 0x409, "bomb")}}
	var names []string
	var extra []chmEntry
	for i := 0; i < pages; i++ {
		n := fmt.Sprintf("p%d.htm", i)
		names = append(names, n)
		extra = append(extra, chmEntry{name: "/" + n, section: 1, offset: uint64((i+1)*interval - 1024), length: 1024})
	}
	parts = append(parts, chmPart{name: "/toc.hhc", data: chmTOC(names...)})
	data := chmSpec{parts: parts, resetInterval: interval, lzx: stream, frames: frames, total: pages * interval, extra: extra}.build()
	wantRefusedUnder(t, "LZX bomb", 4<<20, func() error {
		_, err := CHM{}.Extract(context.Background(), bytes.NewReader(data), int64(len(data)), &memSink{}, Options{Name: "bomb.chm", chmMaxDecompressed: 16 << 20})
		if err != nil && !strings.Contains(err.Error(), errLZXBudget.Error()) {
			t.Errorf("refused for another reason: %v", err)
		}
		return err
	})
}

// A piece table may point every piece at the same bytes: 2,000 pieces of
// 64 KiB each would decode 128 MiB out of a file of 64 KiB of text. The
// text decoded in one pass is capped (maxDOCText), and the reader holds a
// paragraph piece at a time, not the text.
func TestDOC_OverlappingPiecesAreBounded(t *testing.T) {
	text := strings.Repeat("слово ", 32<<10/6)
	pieces := make([]docTestPiece, 2000)
	for i := range pieces {
		pieces[i] = docTestPiece{text: text}
	}
	data := docTestSpec{pieces: pieces, samePlace: true}.build(t)
	s := &rtfDiscardSink{}
	var err error
	peak := peakHeap(func() {
		_, err = DOC{}.Extract(context.Background(), bytes.NewReader(data), int64(len(data)), s, Options{Name: "o.doc"})
	})
	if !IsRefused(err) || !strings.Contains(err.Error(), "text is over") {
		t.Fatalf("err = %v, want the decoded text refused at its cap", err)
	}
	if s.most > maxDOCPara+8<<10 {
		t.Errorf("one write of %d bytes, want paragraph pieces of at most %d", s.most, maxDOCPara)
	}
	if c := ceiling(32 << 20); peak > c {
		t.Errorf("peaked at %d MiB, want at most %d MiB", peak>>20, c>>20)
	}
}

// A compound file whose directory chain loops is refused when it opens,
// for little: the chain walk allocates its visited set and stops.
func TestCFB_ChainLoopRefusalIsCheap(t *testing.T) {
	c := buildCFB(t, cfbStreamSpec{"WordDocument", make([]byte, 8192)})
	dirSec := uint32((c.dirAt - 512) / 512)
	c.fatEntry(dirSec, dirSec)
	wantRefusedUnder(t, "directory chain loop", 4<<20, func() error {
		_, err := extractBytes(DOC{}, "loop.doc", c.data)
		return err
	})
}

// Heading inference keeps each paragraph's runs of one formatting. A
// document whose bold turns on and off at every character makes a run per
// character, each costing far more than its one byte of text: the cap on
// what inference holds counts the runs too, so such a document ends the
// pass (no headings inferred) rather than holding a run per byte. The cap
// is lowered here so a small file reaches it.
func TestDOC_InferenceRunsCountAgainstItsCap(t *testing.T) {
	text := strings.Repeat(strings.Repeat("a", 2000)+"\r", 100) // 200,100 runs
	data := docTestSpec{pieces: []docTestPiece{{text: text, compressed: true}}, alternateBold: true}.build(t)
	var rep Report
	var err error
	peak := peakHeap(func() {
		rep, err = DOC{}.Extract(context.Background(), bytes.NewReader(data), int64(len(data)), &rtfDiscardSink{},
			Options{Name: "runs.doc", InferHeadings: true, docMaxInferText: 1 << 20})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(rep.Notes, "\n"), "headings were not inferred") {
		t.Fatalf("notes %v: 200,000 one-character runs within 200 KiB of text were all held, uncounted", rep.Notes)
	}
	if c := ceiling(16 << 20); peak > c {
		t.Errorf("peaked at %d MiB, want at most %d MiB", peak>>20, c>>20)
	}
	// The same text in one formatting is inferred as usual.
	plain := docTestSpec{pieces: []docTestPiece{{text: text, compressed: true}}}.build(t)
	rep, err = DOC{}.Extract(context.Background(), bytes.NewReader(plain), int64(len(plain)), &rtfDiscardSink{},
		Options{Name: "plain.doc", InferHeadings: true, docMaxInferText: 1 << 20})
	if err != nil || strings.Contains(strings.Join(rep.Notes, "\n"), "not inferred") {
		t.Fatalf("one formatting: err %v, notes %v", err, rep.Notes)
	}
}
