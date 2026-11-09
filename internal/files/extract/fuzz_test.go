package extract

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"
)

// Fuzzers for every extractor but PDF, which has its own (FuzzExtractPDF).
// Each feeds the bytes to one extractor as a whole file and holds it to what
// any caller relies on: it does not panic, it returns within its context
// (with a grace for the context check to be reached), it stops writing once
// the sink refuses and says the output was cut, and a zip format hands over
// no more image bytes than the archive's read budget allows. The seed
// corpora run as ordinary tests; the fuzz CI job runs each target for a
// bounded time.

const (
	// fuzzContext is the context each fuzzed extraction runs under, and
	// fuzzGrace how long past it the extractor may take to notice. Their sum
	// stays under the fuzzer's own hang detection.
	fuzzContext = 3 * time.Second
	fuzzGrace   = 3 * time.Second
	// fuzzOutput is the sink's output limit: small, so the fuzzer reaches
	// the cut from small inputs. Seeds stay a few KiB too: the fuzzer
	// minimizes every new input it keeps, and a large one stalls it.
	fuzzOutput = 2 << 10
	// fuzzAllocBase and fuzzAllocPerByte set the most heap one fuzzed
	// extraction may allocate: a base for the format's fixed costs (a zip
	// may expand to 16 MiB of XML whatever its size) and a multiple of the
	// input. Allocation is counted cumulatively, so this bounds the peak too;
	// it is what catches a few KiB of input that builds a tree of gigabytes.
	fuzzAllocBase    = 512 << 20
	fuzzAllocPerByte = 64
)

// fuzzSink refuses everything once it has refused once, and counts the
// writes an extractor attempts after that.
type fuzzSink struct {
	n, late    int
	refused    bool
	imageBytes int64
}

func (s *fuzzSink) Text(md string) error {
	if s.refused {
		s.late++
		return ErrSinkFull
	}
	if s.n+len(md) > fuzzOutput {
		s.refused = true
		return ErrSinkFull
	}
	s.n += len(md)
	return nil
}

func (s *fuzzSink) Image(img Image) (string, error) {
	if s.refused {
		s.late++
		return "", ErrSinkFull
	}
	s.imageBytes += int64(len(img.Data))
	return "volume:inbox:extracted/f/" + img.Name, nil
}

// fuzzExtract runs e on data and fails t on any property above broken.
func fuzzExtract(t *testing.T, e Extractor, name string, data []byte, zipped bool) {
	if err := fuzzCheck(e, name, data, zipped); err != nil {
		t.Fatal(err)
	}
}

// fuzzExtractBoth runs e on data with heading recovery off and on: the
// option's second pass over the document is held to the same properties.
func fuzzExtractBoth(t *testing.T, e Extractor, name string, data []byte, zipped bool) {
	for _, infer := range []bool{false, true} {
		if err := fuzzCheckOpts(e, data, zipped, Options{Name: name, Images: true, InferHeadings: infer}); err != nil {
			t.Fatalf("infer_headings %t: %v", infer, err)
		}
	}
}

// fuzzCheck runs e on data and reports the first property above it broke.
// zipped says the format is a zip archive, whose image bytes the read budget
// bounds.
func fuzzCheck(e Extractor, name string, data []byte, zipped bool) error {
	return fuzzCheckOpts(e, data, zipped, Options{Name: name, Images: true})
}

func fuzzCheckOpts(e Extractor, data []byte, zipped bool, opts Options) error {
	return fuzzCheckAlloc(e, data, zipped, opts, fuzzAllocBase+uint64(len(data))*fuzzAllocPerByte)
}

// fuzzCheckAlloc is fuzzCheckOpts with the allocation ceiling given.
func fuzzCheckAlloc(e Extractor, data []byte, zipped bool, opts Options, allocCeiling uint64) error {
	ctx, cancel := context.WithTimeout(context.Background(), fuzzContext)
	defer cancel()
	sink := &fuzzSink{}
	type outcome struct {
		err   error
		panic string
	}
	done := make(chan outcome, 1)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- outcome{panic: fmt.Sprintf("%v\n%s", p, debug.Stack())}
			}
		}()
		_, err := e.Extract(ctx, bytes.NewReader(data), int64(len(data)), sink, opts)
		done <- outcome{err: err}
	}()
	var o outcome
	select {
	case o = <-done:
	case <-time.After(fuzzContext + fuzzGrace):
		return fmt.Errorf("Extract did not return within %s of a %s context", fuzzGrace, fuzzContext)
	}
	runtime.ReadMemStats(&after)
	if o.panic != "" {
		return fmt.Errorf("Extract panicked: %s", o.panic)
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > allocCeiling {
		return fmt.Errorf("Extract allocated %d MiB for a %d-byte input, past its ceiling of %d MiB", got>>20, len(data), allocCeiling>>20)
	}
	if sink.late > 0 {
		return fmt.Errorf("Extract wrote %d more times after the sink refused", sink.late)
	}
	if sink.refused && !errors.Is(o.err, ErrSinkFull) && ctx.Err() == nil {
		return fmt.Errorf("the sink refused the output but Extract returned %v, not ErrSinkFull", o.err)
	}
	if zipped {
		if budget := min(max(int64(len(data))*MaxZipExpansion, minZipBudget), MaxZipUncompressed); sink.imageBytes > budget {
			return fmt.Errorf("handed over %d image bytes from a %d-byte archive, past its read budget of %d", sink.imageBytes, len(data), budget)
		}
	}
	return nil
}

// storedZip repacks an archive with every entry stored and its CRC left
// zero, which archive/zip then does not check: a mutation of an entry's
// bytes reaches the extractor's parser instead of failing the checksum.
func storedZip(t testing.TB, data []byte) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		w, err := zw.CreateRaw(&zip.FileHeader{Name: f.Name, Method: zip.Store,
			CompressedSize64: uint64(len(body)), UncompressedSize64: uint64(len(body))})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// addZipSeeds adds each archive deflated as built, stored for mutation, and
// cut short.
func addZipSeeds(f *testing.F, archives ...[]byte) {
	for _, a := range archives {
		f.Add(a)
		s := storedZip(f, a)
		f.Add(s)
		f.Add(s[:len(s)/2])
	}
}

func FuzzExtractDOCX(f *testing.F) {
	png := tinyPNG(f)
	addZipSeeds(f, boardDOCX(f, png),
		docxWith(f, `<w:document `+nsW+`><w:body>`+strings.Repeat(wPara("", strings.Repeat("слово ", 10)), 20)+`</w:body></w:document>`),
		docxWith(f, `<w:document `+nsW+`><w:body><w:tbl><w:tr>`+wCell("a")+wCell("b")+`</w:tr><w:tr>`+wCell("1")+`</w:tr></w:tbl>`+
			wListItem("x")+`</w:body></w:document>`),
		inferDOCXOf(f, gdocBody()))
	f.Fuzz(func(t *testing.T, data []byte) { fuzzExtractBoth(t, DOCX{}, "f.docx", data, true) })
}

func FuzzExtractXLSX(f *testing.F) {
	// The last renders past the fuzz sink's limit, so the cut is seeded.
	addZipSeeds(f, budgetXLSX(f, 3), amplifiedXLSX(f, 30, 4, 3), amplifiedXLSX(f, 60, 40, 3))
	f.Fuzz(func(t *testing.T, data []byte) { fuzzExtract(t, XLSX{}, "f.xlsx", data, true) })
}

func FuzzExtractPPTX(f *testing.F) {
	addZipSeeds(f, boardPPTX(f, tinyPNG(f)))
	f.Fuzz(func(t *testing.T, data []byte) { fuzzExtract(t, PPTX{}, "f.pptx", data, true) })
}

func FuzzExtractODT(f *testing.F) {
	addZipSeeds(f, passportODT(f, tinyPNG(f)))
	f.Fuzz(func(t *testing.T, data []byte) { fuzzExtract(t, ODT{}, "f.odt", data, true) })
}

func FuzzExtractHTML(f *testing.F) {
	for _, s := range []string{
		`<!doctype html><html><head><title>Новини</title><script>x()</script></head><body><h1>Засідання</h1><p>Рішення: затвердити
   бюджет</p><h3>Деталі</h3><ul><li>Перше<ul><li>Вкладене</li></ul></li><li># не заголовок</li></ul>
<table><tr><th>Стаття</th><th colspan="2">Сума</th></tr><tr><td rowspan="3">a|b</td><td>1</td></tr></table>
<img src="https://example.com/logo (1).png" alt="Логотип"><pre>code ` + "```" + ` inside</pre><style>p{}</style></body></html>`,
		`<ol start="3"><li><a href="javascript:x">link</a></li></ol><blockquote><p>q</p></blockquote><h7>x</h7>`,
		`<table><tr><td><table><tr><td>nested</td></tr></table></td></tr></table>`,
		gdocHTML(),
		`<p style="font-size:1e308pt;color:rgb(300,-1,50%);font-weight:900">A</p><p style="font-size:200%"><span style="font-size:3em">B</span></p>`,
		gdocPage(gdocCSS+`@media x{.a{}`, `<p class="c1 c2"><span class="c3">A</span></p><style>}p.c2{font-size:2em/*</style><p class="c2">B</p>`),
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) { fuzzExtractBoth(t, HTML{}, "f.html", data, false) })
}

func FuzzExtractVTT(f *testing.F) {
	f.Add([]byte(teamsVTT))
	f.Add([]byte("1\r\n00:00:01,000 --> 00:00:02,500\r\n<i>Hello</i>\r\nthere\r\n\r\n2\r\n00:00:40,000 --> 00:00:41,000\r\n- Bye\r\n"))
	f.Add([]byte("WEBVTT\n\n00:00.000 --> 00:01.000\n<v A>a</v>\n\n00:00.000 --> 00:01.000\n<v B>b</v>\n"))
	f.Fuzz(func(t *testing.T, data []byte) { fuzzExtract(t, Captions{}, "f.vtt", data, false) })
}

func FuzzExtractCSV(f *testing.F) {
	f.Add([]byte("Стаття,Сума\n\"Оренда, склад\",120\nx|y,2\n"))
	f.Add([]byte("a\tb\n1\t2\n"))
	f.Add([]byte(strings.Repeat(strings.Repeat("значення,", 20)+"\n", 20)))
	f.Add([]byte("\xef\xbb\xbf\"a\"\"b\",c\n,,,,\n\"unterminated\n"))
	f.Fuzz(func(t *testing.T, data []byte) { fuzzExtract(t, CSV{}, "f.csv", data, false) })
}

func FuzzExtractText(f *testing.F) {
	f.Add([]byte("Звіт\n\n# not a heading\n- not a list\n<b>raw</b>\n"))
	f.Add([]byte("\xef\xbb\xbfline one\r\nline two\r\n\r\n\r\nthree"))
	f.Add([]byte("\xff\xfe not utf-8"))
	f.Fuzz(func(t *testing.T, data []byte) {
		fuzzExtract(t, Text{}, "f.txt", data, false)
		fuzzExtract(t, Text{}, "f.md", data, false)
	})
}

func FuzzExtractImage(f *testing.F) {
	f.Add(tinyPNG(f))
	f.Add([]byte("GIF89a"))
	f.Add([]byte("\xff\xd8\xff\xe0 not a whole jpeg"))
	f.Fuzz(func(t *testing.T, data []byte) { fuzzExtract(t, ImageFile{}, "f.png", data, false) })
}

// The stored, CRC-less repack the zip seeds use reads exactly as the archive
// it came from, so a fuzzer's mutation of it reaches the parser rather than
// stopping at the archive layer.
func TestStoredZip_ExtractsLikeTheOriginal(t *testing.T) {
	png := tinyPNG(t)
	for _, c := range []struct {
		e    Extractor
		data []byte
	}{{DOCX{}, boardDOCX(t, png)}, {XLSX{}, budgetXLSX(t, 3)}, {PPTX{}, boardPPTX(t, png)}, {ODT{}, passportODT(t, png)}} {
		want, _ := run(t, c.e, c.data, Options{Name: "f", Images: true})
		got, _ := run(t, c.e, storedZip(t, c.data), Options{Name: "f", Images: true})
		if got.md.String() != want.md.String() || len(got.images) != len(want.images) {
			t.Errorf("%T: the stored repack reads differently:\n%s\nwant:\n%s", c.e, got.md.String(), want.md.String())
		}
	}
}

// The fuzz harness's checks can fail: an extractor that writes on after the
// sink refused, or swallows the refusal, is reported.
func TestFuzzCheck_ReportsAnExtractorThatIgnoresTheSink(t *testing.T) {
	for _, c := range []struct {
		name string
		e    Extractor
	}{{"writes on", sinkIgnorer{swallow: false}}, {"swallows", sinkIgnorer{swallow: true}}} {
		if err := fuzzCheck(c.e, "f.txt", nil, false); err == nil {
			t.Errorf("%s: the harness passed an extractor that ignores the sink", c.name)
		}
	}
}

// The allocation ceiling can fail: an extractor that allocates past it is
// reported.
func TestFuzzCheck_ReportsAnExtractorThatAllocatesPastTheCeiling(t *testing.T) {
	if err := fuzzCheckAlloc(allocator{n: 64 << 20}, nil, false, Options{}, 16<<20); err == nil {
		t.Fatal("the harness passed an extractor that allocated 64 MiB against a 16 MiB ceiling")
	}
	if err := fuzzCheckAlloc(allocator{n: 1 << 20}, nil, false, Options{}, 16<<20); err != nil {
		t.Fatalf("the harness refused an extractor under its ceiling: %v", err)
	}
}

// allocator allocates n bytes in 1 MiB pieces it throws away.
type allocator struct{ n int }

var allocSink []byte

func (allocator) Match(string, string) bool { return false }

func (a allocator) Extract(context.Context, io.ReaderAt, int64, Sink, Options) (Report, error) {
	for i := 0; i < a.n; i += 1 << 20 {
		allocSink = make([]byte, 1<<20)
	}
	return Report{}, nil
}

// sinkIgnorer writes past the fuzz sink's limit twice, then returns nil when
// swallow is set and the sink's error otherwise.
type sinkIgnorer struct{ swallow bool }

func (sinkIgnorer) Match(string, string) bool { return false }

func (s sinkIgnorer) Extract(_ context.Context, _ io.ReaderAt, _ int64, sink Sink, _ Options) (Report, error) {
	big := strings.Repeat("x", fuzzOutput+1)
	err := sink.Text(big)
	if !s.swallow {
		err = sink.Text("more")
		return Report{}, err
	}
	return Report{}, nil
}
