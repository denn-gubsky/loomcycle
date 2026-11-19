package extract

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// ---- a DjVu container built in code ----

func iffChunk(id string, data []byte) []byte {
	b := append([]byte(id), 0, 0, 0, 0)
	binary.BigEndian.PutUint32(b[4:], uint32(len(data)))
	b = append(b, data...)
	if len(data)%2 == 1 {
		b = append(b, 0)
	}
	return b
}

func iffForm(kind string, body ...[]byte) []byte {
	return iffChunk("FORM", append([]byte(kind), bytes.Join(body, nil)...))
}

// djvuDoc is a bundled document of len(text) pages, page i carrying a text
// chunk when text[i]; the chunks' bodies are not real DjVu data, which the
// walk never decodes. outline adds a NAVM chunk, anno an ANTz in page 1.
func djvuDoc(text []bool, outline, anno bool) []byte {
	return djvuDocIncluding(text, outline, anno, "")
}

// djvuDocIncluding is djvuDoc with page 1 including the component named
// incl (an INCL chunk), when it is not "".
func djvuDocIncluding(text []bool, outline, anno bool, incl string) []byte {
	var comps [][]byte
	for i, t := range text {
		parts := [][]byte{iffChunk("INFO", make([]byte, 10)), iffChunk("Sjbz", []byte("jb2"))}
		if t {
			parts = append(parts, iffChunk("TXTz", []byte("bzz")))
		}
		if anno && i == 0 {
			parts = append(parts, iffChunk("ANTz", []byte("bzz")))
		}
		if incl != "" && i == 0 {
			parts = append(parts, iffChunk("INCL", []byte(incl)))
		}
		comps = append(comps, iffForm("DJVU", parts...))
	}
	dirmLen := 3 + 4*len(comps) + 5
	off := 4 + 12 + 8 + dirmLen + dirmLen%2
	var navm []byte
	if outline {
		navm = iffChunk("NAVM", []byte("bzz"))
		off += len(navm)
	}
	dirm := []byte{0x81, byte(len(comps) >> 8), byte(len(comps))}
	for _, c := range comps {
		dirm = binary.BigEndian.AppendUint32(dirm, uint32(off))
		off += len(c)
	}
	dirm = append(dirm, "zzzzz"...)
	body := append([][]byte{iffChunk("DIRM", dirm), navm}, comps...)
	return append([]byte("AT&T"), iffForm("DJVM", body...)...)
}

func readFixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "djvu", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ---- helpers that answer as djvutxt and djvused would ----

type fakeDjVu struct {
	text    map[int]string // a page's text; a page not here has none: ()
	outline string
	meta    string
	fail    map[int]bool // pages whose presence in a call fails it
	missing bool         // the helpers are not available
	// render answers Render for a page as the rasterize helper would; nil
	// is no rasterize helper (Renders is false).
	render func(page, maxSide int) ([]byte, error)

	mu    sync.Mutex
	opens int
	calls []string
}

func (f *fakeDjVu) Open(ctx context.Context, r io.ReaderAt, size int64) (DjVuSession, error) {
	f.mu.Lock()
	f.opens++
	f.mu.Unlock()
	return f, nil
}

func (f *fakeDjVu) Close() error { return nil }

func (f *fakeDjVu) record(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakeDjVu) Text(ctx context.Context, spec string, maxBytes int) ([]byte, bool, error) {
	f.record("text " + spec)
	if f.missing {
		return nil, false, ErrHelperUnavailable
	}
	var b bytes.Buffer
	for _, n := range parseSpec(spec) {
		if f.fail[n] {
			return nil, false, errors.New("djvutxt: exit 10")
		}
		if t, ok := f.text[n]; ok {
			fmt.Fprintf(&b, "(page 0 0 100 100 %s)\n", strconv.Quote(t))
		} else {
			b.WriteString("()\n")
		}
	}
	if b.Len() > maxBytes {
		return b.Bytes()[:maxBytes], true, nil
	}
	return b.Bytes(), false, nil
}

func (f *fakeDjVu) Inspect(ctx context.Context, script DjVuScript, maxBytes int) ([]byte, bool, error) {
	f.record("inspect " + string(script))
	if f.missing {
		return nil, false, ErrHelperUnavailable
	}
	switch script {
	case DjVuOutline:
		return []byte(f.outline), false, nil
	case DjVuMeta:
		return []byte(f.meta), false, nil
	}
	return nil, false, fmt.Errorf("a script that is not one of the two: %q", script)
}

func (f *fakeDjVu) textCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if s, ok := strings.CutPrefix(c, "text "); ok {
			out = append(out, s)
		}
	}
	return out
}

// parseSpec reads "1-3,7" back into pages.
func parseSpec(spec string) []int {
	var out []int
	for _, part := range strings.Split(spec, ",") {
		a, b, rng := strings.Cut(part, "-")
		lo, _ := strconv.Atoi(a)
		hi := lo
		if rng {
			hi, _ = strconv.Atoi(b)
		}
		for n := lo; n <= hi; n++ {
			out = append(out, n)
		}
	}
	return out
}

func allText(n int) []bool {
	t := make([]bool, n)
	for i := range t {
		t[i] = true
	}
	return t
}

func pageTexts(n int) map[int]string {
	m := map[int]string{}
	for i := 1; i <= n; i++ {
		m[i] = fmt.Sprintf("The text of page %d, long enough to count as a text layer of its own.", i)
	}
	return m
}

// djvuSink records the Markdown and where each page starts.
type djvuSink struct {
	b      strings.Builder
	starts map[int]int
	max    int
}

func (s *djvuSink) Text(md string) error {
	if s.max > 0 && s.b.Len()+len(md) > s.max {
		return ErrSinkFull
	}
	s.b.WriteString(md)
	return nil
}
func (s *djvuSink) Image(Image) (string, error) { return "", nil }
func (s *djvuSink) StartPage(n int) {
	if s.starts == nil {
		s.starts = map[int]int{}
	}
	s.starts[n] = s.b.Len()
}

func extractDjVu(t *testing.T, data []byte, h DjVuHelpers, sink Sink, opts Options) (Report, error) {
	t.Helper()
	if opts.Name == "" {
		opts.Name = "book.djvu"
	}
	return DjVu{Helpers: h}.Extract(context.Background(), bytes.NewReader(data), int64(len(data)), sink, opts)
}

// ---- the container ----

func TestDjVuIFF_ReadsPagesAndTextLayersWithoutDecoding(t *testing.T) {
	for name, want := range map[string]djvuInfo{
		"single.djvu": {pages: 1, pageText: []bool{true}},
		"book.djvu":   {pages: 3, pageText: []bool{true, false, true}, outline: true, anno: true},
		"scan.djvu":   {pages: 3, pageText: []bool{false, false, false}},
	} {
		data := readFixture(t, name)
		got, err := walkDjVu(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.pages != want.pages || !slices.Equal(got.pageText, want.pageText) || got.outline != want.outline || got.anno != want.anno || got.sharedText {
			t.Errorf("%s: %+v, want %+v", name, got, want)
		}
	}
	built := djvuDoc([]bool{false, true, false, true}, true, false)
	got, err := walkDjVu(bytes.NewReader(built), int64(len(built)))
	if err != nil || got.pages != 4 || !slices.Equal(got.pageText, []bool{false, true, false, true}) || !got.outline {
		t.Errorf("built: %+v, %v", got, err)
	}
}

// TestDjVu_IndirectDocumentIsRefused: an indirect document names its pages
// as other files; it is refused with that reason before any helper runs
// (D11), so a file can never make the runtime open a path.
func TestDjVu_IndirectDocumentIsRefused(t *testing.T) {
	h := &fakeDjVu{text: pageTexts(3)}
	_, err := extractDjVu(t, readFixture(t, "indirect.djvu"), h, &djvuSink{}, Options{})
	if !IsRefused(err) || !strings.Contains(err.Error(), "indirect") {
		t.Fatalf("err = %v, want a refusal naming the indirect document", err)
	}
	if h.opens != 0 {
		t.Errorf("the helpers were opened %d times for a refused file", h.opens)
	}
}

func TestDjVuIFF_RefusesWhatItCannotTrust(t *testing.T) {
	good := djvuDoc([]bool{true, true}, false, false)
	lying := slices.Clone(good)
	binary.BigEndian.PutUint32(lying[8:], uint32(len(good))) // FORM runs past the file
	dup := slices.Clone(good)
	// The second directory entry names the first page again.
	copy(dup[4+12+8+3+4:], dup[4+12+8+3:4+12+8+3+4])
	pm44 := append([]byte("AT&T"), iffForm("PM44", iffChunk("PM44", []byte("x")))...)
	nested := iffChunk("TXTz", nil)
	for range maxDjVuDepth + 1 {
		nested = iffForm("DJVU", nested)
	}
	deep := append([]byte("AT&T"), nested...)
	for name, data := range map[string][]byte{
		"empty":         nil,
		"not djvu":      []byte("%PDF-1.7 not a djvu file at all"),
		"cut short":     good[:len(good)-7],
		"lying length":  lying,
		"named twice":   dup,
		"an iw44 photo": pm44,
		"nested deep":   deep,
	} {
		if _, err := walkDjVu(bytes.NewReader(data), int64(len(data))); err == nil {
			t.Errorf("%s: walked, want refused", name)
		}
	}
}

// TestDjVuIFF_RefusesWhatWouldOpenAnotherFile: DjVuLibre opens the file a
// single page's INCL names beside it, so such a page, or one naming files
// in an NDIR, is refused before any helper runs. In a bundle an include is
// resolved among the bundle's own parts; its id must still be a plain name,
// never a path. Only a page (DJVU) or a bundle (DJVM) is a document: a
// shared part or a thumbnail file on its own is not.
func TestDjVuIFF_RefusesWhatWouldOpenAnotherFile(t *testing.T) {
	page := func(extra ...[]byte) []byte {
		return append([]byte("AT&T"), iffForm("DJVU", append([][]byte{iffChunk("INFO", make([]byte, 10))}, extra...)...)...)
	}
	refused := map[string][]byte{
		"page INCL":       page(iffChunk("INCL", []byte("shared_anno.iff"))),
		"page NDIR":       page(iffChunk("NDIR", []byte("bzz"))),
		"top DJVI":        append([]byte("AT&T"), iffForm("DJVI", iffChunk("ANTz", []byte("bzz")))...),
		"top THUM":        append([]byte("AT&T"), iffForm("THUM", iffChunk("TH44", []byte("x")))...),
		"bundle up":       djvuDocIncluding(allText(2), false, false, "../secret.iff"),
		"bundle slash":    djvuDocIncluding(allText(2), false, false, "dir/shared.iff"),
		"bundle bslash":   djvuDocIncluding(allText(2), false, false, `dir\shared.iff`),
		"bundle colon":    djvuDocIncluding(allText(2), false, false, "file:shared.iff"),
		"bundle nul":      djvuDocIncluding(allText(2), false, false, "shared\x00.iff"),
		"bundle dotdot":   djvuDocIncluding(allText(2), false, false, ".."),
		"bundle newline":  djvuDocIncluding(allText(2), false, false, "shared\n.iff"),
		"bundle too long": djvuDocIncluding(allText(2), false, false, strings.Repeat("a", maxDjVuIncludeID+1)),
	}
	for name, data := range refused {
		h := &fakeDjVu{text: pageTexts(2)}
		_, err := extractDjVu(t, data, h, &djvuSink{}, Options{})
		if err == nil {
			t.Errorf("%s: read, want refused", name)
		}
		if h.opens != 0 {
			t.Errorf("%s: the helpers were opened", name)
		}
	}
	if _, err := walkDjVu(bytes.NewReader(refused["page INCL"]), int64(len(refused["page INCL"]))); !IsRefused(err) || !strings.Contains(err.Error(), "includes other files") {
		t.Errorf("page INCL: %v, want a refusal saying why", err)
	}
	ok := djvuDocIncluding(allText(2), false, false, "shared_anno.iff")
	if _, err := walkDjVu(bytes.NewReader(ok), int64(len(ok))); err != nil {
		t.Errorf("a bundle including a part by its name: %v", err)
	}
}

// ---- the extractor ----

// TestDjVu_PagesAsksTheHelperForThosePagesOnly: pages limits the work, not
// only the output: the helper is asked for exactly the pages wanted that
// carry a text layer, and a page past the end (which djvutxt would clamp to
// the last page) never reaches it.
func TestDjVu_PagesAsksTheHelperForThosePagesOnly(t *testing.T) {
	text := allText(10)
	text[5] = false // page 6 has no text layer
	h := &fakeDjVu{text: pageTexts(10)}
	sink := &djvuSink{}
	rep, err := extractDjVu(t, djvuDoc(text, false, false), h, sink, Options{Pages: []int{2, 5, 6, 7, 40}})
	if err != nil {
		t.Fatal(err)
	}
	if got := h.textCalls(); !slices.Equal(got, []string{"2,5,7"}) {
		t.Errorf("text calls = %q, want [2,5,7]", got)
	}
	var yields []int
	for _, y := range rep.PageYields {
		yields = append(yields, y.Page)
	}
	if !slices.Equal(yields, []int{2, 5, 6, 7}) || rep.Pages != 10 {
		t.Errorf("yields for pages %v of %d, want 2, 5, 6, 7 of 10", yields, rep.Pages)
	}
	md := sink.b.String()
	for _, want := range []string{"## Page 2\n\nThe text of page 2", "## Page 6", "## Page 7\n\nThe text of page 7"} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in\n%s", want, md)
		}
	}
	if strings.Contains(md, "page 3,") || strings.Contains(md, "## Page 40") {
		t.Errorf("a page not asked for was written:\n%s", md)
	}
	if !strings.Contains(strings.Join(rep.Notes, "\n"), "does not have (it has 10)") {
		t.Errorf("notes = %q, want the page past the end named", rep.Notes)
	}
}

// TestDjVu_MaxCharsStopsAskingTheHelper: once the sink refuses, no further
// batch is asked of the helper.
func TestDjVu_MaxCharsStopsAskingTheHelper(t *testing.T) {
	h := &fakeDjVu{text: pageTexts(40)}
	_, err := extractDjVu(t, djvuDoc(allText(40), false, false), h, &djvuSink{max: 300}, Options{djvuBatchSize: 4})
	if !errors.Is(err, ErrSinkFull) {
		t.Fatalf("err = %v, want ErrSinkFull", err)
	}
	if got := h.textCalls(); !slices.Equal(got, []string{"1-4"}) {
		t.Errorf("text calls = %q, want only the first batch", got)
	}
}

// A scan with no text chunk anywhere is read without the helpers at all,
// every page reported with a zero yield, so an OCR pass can choose them.
func TestDjVu_PagesWithoutATextChunkAreNeverAskedOfTheHelper(t *testing.T) {
	h := &fakeDjVu{}
	sink := &djvuSink{}
	rep, err := extractDjVu(t, readFixture(t, "scan.djvu"), h, sink, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if h.opens != 0 {
		t.Errorf("the helpers were opened %d times for a document with no text layer", h.opens)
	}
	if len(rep.PageYields) != 3 || rep.PageYields[2] != (PageYield{Page: 3}) {
		t.Errorf("yields = %+v", rep.PageYields)
	}
	if !strings.Contains(strings.Join(rep.Notes, "\n"), "likely a scan") {
		t.Errorf("notes = %q", rep.Notes)
	}
	if len(sink.starts) != 3 || sink.starts[1] >= sink.starts[2] {
		t.Errorf("page starts = %v", sink.starts)
	}
}

// The outline's entries are headings placed before the page each points to
// (after where the page starts, so an OCR splice keeps them with it); the
// metadata goes to the report and its title to the one # heading.
func TestDjVu_OutlineAndMetadata(t *testing.T) {
	h := &fakeDjVu{
		text:    map[int]string{1: "Page one text, long enough to be a text layer of its own, surely.", 3: "Page three."},
		outline: "(bookmarks\n (\"Chapter One\"\n  \"#1\" )\n (\"Chapter Two\"\n  \"#3\"\n  (\"Section 2.1\"\n   \"#3\" ) )\n (\"Elsewhere\" \"http://example.invalid/\") )\n",
		meta:    "publisher\t\"Loomcycle Test Press\"\nyear\t\"2019\"\nauthor\t\"Ada Example\"\ntitle\t\"A Synthetic \\\"Book\\\"\"\n",
	}
	sink := &djvuSink{}
	rep, err := extractDjVu(t, readFixture(t, "book.djvu"), h, sink, Options{})
	if err != nil {
		t.Fatal(err)
	}
	md := sink.b.String()
	want := "# A Synthetic \"Book\"\n\n## Chapter One\n\n## Page 1\n\nPage one text"
	if !strings.HasPrefix(md, want) {
		t.Errorf("markdown starts\n%q\nwant\n%q", md[:min(len(md), 120)], want)
	}
	if !strings.Contains(md, "## Page 2\n\n## Chapter Two\n\n### Section 2.1\n\n## Page 3\n\nPage three.") {
		t.Errorf("outline headings not before page 3:\n%s", md)
	}
	if !strings.HasPrefix(md[sink.starts[3]:], "\n\n## Chapter Two") {
		t.Errorf("page 3 starts at %q, want at its chapter heading", md[sink.starts[3]:])
	}
	m := rep.Metadata
	if m == nil || m.Title != `A Synthetic "Book"` || !slices.Equal(m.Authors, []string{"Ada Example"}) || m.Year != 2019 ||
		m.Publisher != "Loomcycle Test Press" || m.SourceFormat != "djvu" {
		t.Errorf("metadata = %+v", m)
	}
	if !strings.Contains(strings.Join(rep.Notes, "\n"), "1 outline entries that point to no page") {
		t.Errorf("notes = %q", rep.Notes)
	}
	if got := h.textCalls(); !slices.Equal(got, []string{"1,3"}) {
		t.Errorf("text calls = %q, want 1,3 (page 2 has no text layer)", got)
	}
}

// A batch the helper fails on is asked again a page at a time: one damaged
// page costs itself, reported unread, and the rest are read.
func TestDjVu_ABatchTheHelperFailsOnIsAskedPageByPage(t *testing.T) {
	h := &fakeDjVu{text: pageTexts(5), fail: map[int]bool{3: true}}
	rep, err := extractDjVu(t, djvuDoc(allText(5), false, false), h, &djvuSink{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := h.textCalls(); !slices.Equal(got, []string{"1-5", "1", "2", "3", "4", "5"}) {
		t.Errorf("text calls = %q", got)
	}
	if y := rep.PageYields[2]; !y.Unread || rep.PageYields[3].Chars == 0 {
		t.Errorf("yields = %+v, want page 3 unread and page 4 read", rep.PageYields)
	}
	if !strings.Contains(strings.Join(rep.Notes, "\n"), "text layer of page 3 could not be read") {
		t.Errorf("notes = %q", rep.Notes)
	}
}

// A helper that is not there fails the extraction at once, not page by page.
func TestDjVu_AnUnavailableHelperStopsTheExtraction(t *testing.T) {
	h := &fakeDjVu{missing: true}
	_, err := extractDjVu(t, djvuDoc(allText(5), false, false), h, &djvuSink{}, Options{})
	if !errors.Is(err, ErrHelperUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if n := len(h.textCalls()); n != 1 {
		t.Errorf("%d text calls, want 1", n)
	}
}

// Text from the file stays text: a line that reads as Markdown is escaped,
// DjVuLibre's separators become paragraph breaks, and control characters go.
func TestDjVu_TextIsWrittenAsText(t *testing.T) {
	h := &fakeDjVu{text: map[int]string{1: "# not a heading\n![x](volume:secret)\x1fnext paragraph\x01"}}
	sink := &djvuSink{}
	if _, err := extractDjVu(t, readFixture(t, "single.djvu"), h, sink, Options{}); err != nil {
		t.Fatal(err)
	}
	want := "## Page 1\n\n\\# not a heading\n!\\[x](volume:secret)\n\nnext paragraph"
	if !strings.HasSuffix(sink.b.String(), want) {
		t.Errorf("markdown =\n%q\nwant suffix\n%q", sink.b.String(), want)
	}
}

// ---- the helper's answers ----

func TestSexpr_ReadsDjVuLibreStrings(t *testing.T) {
	e, err := parseSexprs([]byte("(page 0 0 1 1 \"a\\\"b\\\\c\\nd\\037e\\x41\\321\\202\") ()\n; comment\nkey \"v\""))
	if err != nil {
		t.Fatal(err)
	}
	if len(e) != 4 {
		t.Fatalf("%d expressions", len(e))
	}
	if got, ok := djvuPageText(e[0]); !ok || got != "a\"b\\c\nd\x1feAт" {
		t.Errorf("page text %q, %v", got, ok)
	}
	if got, ok := djvuPageText(e[1]); !ok || got != "" {
		t.Errorf("empty page %q, %v", got, ok)
	}
	if e[2].atom != "key" || e[3].str != "v" {
		t.Errorf("pairs = %+v", e[2:])
	}
	for _, bad := range []string{"(page", "\"open", ")", "(a \"b\\", strings.Repeat("(", maxSexprDepth+2) + strings.Repeat(")", maxSexprDepth+2)} {
		if _, err := parseSexprs([]byte(bad)); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	many := bytes.Repeat([]byte("()"), maxSexprNodes+1)
	if _, err := parseSexprs(many); err == nil {
		t.Error("more nodes than the cap parsed")
	}
}

func TestSexpr_FinerDetailReadsAsLines(t *testing.T) {
	e, err := parseSexprs([]byte(`(page 0 0 9 9 (line 0 0 9 9 (word 0 0 1 1 "one") (word 1 1 2 2 "two")) (line 0 0 9 9 "three"))`))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := djvuPageText(e[0]); got != "one two \nthree\n" {
		t.Errorf("%q", got)
	}
}

func TestPageSpec_WritesRanges(t *testing.T) {
	for _, c := range []struct {
		in   []int
		want string
	}{{[]int{1}, "1"}, {[]int{1, 2, 3, 7}, "1-3,7"}, {[]int{2, 4, 5, 9, 10, 11}, "2,4-5,9-11"}} {
		if got := pageSpec(c.in); got != c.want {
			t.Errorf("pageSpec(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---- fuzz ----

func FuzzDjVuIFF(f *testing.F) {
	for _, name := range []string{"single.djvu", "book.djvu", "scan.djvu", "indirect.djvu"} {
		f.Add(readFixture(f, name))
	}
	f.Add(djvuDoc([]bool{true, false, true}, true, true))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = walkDjVu(bytes.NewReader(data), int64(len(data)))
		h := &fakeDjVu{text: pageTexts(3), outline: `(bookmarks ("A" "#1"))`, meta: `title "T"`}
		fuzzExtract(t, DjVu{Helpers: h}, "f.djvu", data, false)
	})
}

func FuzzDjVuTextSexpr(f *testing.F) {
	f.Add([]byte("(page 0 0 100 69 \n  \"First page \\nSecond\\037\" )\n()\n"))
	f.Add([]byte(`(bookmarks ("Chapter One" "#1") ("Two" "#3" ("2.1" "#3")))`))
	f.Add([]byte("title\t\"A\\\"B\"\nyear\t\"2019\"\n"))
	f.Add([]byte(`(page 0 0 9 9 (line 0 0 9 9 (word 0 0 1 1 "one")))`))
	f.Fuzz(func(t *testing.T, data []byte) {
		exprs, err := parseSexprs(data)
		if err != nil {
			return
		}
		for _, e := range exprs {
			text, _ := djvuPageText(e)
			if len(text) > len(data) {
				t.Fatalf("%d bytes of text from %d bytes of input", len(text), len(data))
			}
			_, _, _ = djvuMarkdown(text)
		}
		_ = djvuMetadata(data)
	})
}
