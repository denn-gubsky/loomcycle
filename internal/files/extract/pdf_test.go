package extract

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/files/extract/pdftest"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

// tinyJPEG is a real 8x8 JPEG.
func tinyJPEG(t testing.TB) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for x := range 8 {
		img.Set(x, x, color.RGBA{B: 255, A: 255})
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// boardPDF is a two-page board protocol: Cyrillic text in an Identity-H font
// with a ToUnicode CMap, a JPEG and a raw-pixel picture on page 1, and on
// page 2 document text that looks like Markdown.
func boardPDF(t testing.TB) ([]byte, []byte) {
	pic := tinyJPEG(t)
	doc := pdftest.Doc{Pages: []pdftest.Page{
		{
			Lines: []pdftest.Line{
				{Text: "Протокол засідання правління", X: 72, Y: 770, Size: 16},
				{Text: "Рішення: затвердити бюджет", X: 72, Y: 740, Size: 11, Kern: true},
				{Text: "на 2027 рік.", X: 72, Y: 726, Size: 11},
				{Text: "Голова: Іван Петренко", X: 72, Y: 650, Size: 11},
				{Text: "Ґанок, їжак, є.", X: 300, Y: 650, Size: 11},
			},
			Images: []pdftest.Image{
				{JPEG: pic, W: 8, H: 8, X: 72, Y: 500, Width: 100, Height: 100},
				{Pixels: bytes.Repeat([]byte{0x80}, 4*4*3), W: 4, H: 4, X: 200, Y: 500, Width: 40, Height: 40},
			},
		},
		{
			Lines: []pdftest.Line{
				{Text: "# не заголовок ![x](volume:secret:k.png) <script>", X: 72, Y: 770, Size: 11},
				{Text: "Голосували: «за» — 5, «проти» — 0.", X: 72, Y: 756, Size: 11},
			},
		},
	}}
	return doc.Bytes(), pic
}

// scanPDF is a page that is only a picture, as a scanner writes it.
func scanPDF(t testing.TB) ([]byte, []byte) {
	pic := tinyJPEG(t)
	doc := pdftest.Doc{Pages: []pdftest.Page{{
		Images: []pdftest.Image{{JPEG: pic, W: 8, H: 8, X: 0, Y: 0, Width: 595, Height: 842}},
	}}}
	return doc.Bytes(), pic
}

func extractPDF(t testing.TB, data []byte, opts Options) (*memSink, Report, error) {
	t.Helper()
	s := &memSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rep, err := PDF{}.Extract(ctx, bytes.NewReader(data), int64(len(data)), s, opts)
	return s, rep, err
}

func TestExtractPDF_CyrillicTextLayer(t *testing.T) {
	data, _ := boardPDF(t)
	s, rep, err := extractPDF(t, data, Options{Name: "protokol.pdf", Images: true})
	if err != nil {
		t.Fatal(err)
	}
	md := s.md.String()
	golden(t, "board.pdf.md", md)
	for _, want := range []string{
		"# protokol\n",
		"\n## Page 1\n\nПротокол засідання правління\n",
		// Words a producer separated by moving, not by a space glyph.
		"Рішення: затвердити бюджет\nна 2027 рік.",
		// Two runs on one baseline are one row, left to right.
		"Голова: Іван Петренко Ґанок, їжак, є.",
		"\n## Page 2\n",
		`\# не заголовок !\[x](volume:secret:k.png) \<script>`,
		"Голосували: «за» — 5, «проти» — 0.",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in:\n%s", want, md)
		}
	}
	if rep.Format != "pdf" || rep.Pages != 2 || rep.Chars == 0 {
		t.Errorf("report = %+v", rep)
	}
}

// A real producer: Chrome's PDF printer (Skia) writes Identity-H fonts with
// ToUnicode CMaps, one glyph per positioning operator, under a flipped
// coordinate system. See testdata/README.md.
func TestExtractPDF_CyrillicTextLayerFromChrome(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "chrome-board.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	s, rep, err := extractPDF(t, data, Options{Name: "chrome-board.pdf", Images: true})
	if err != nil {
		t.Fatal(err)
	}
	md := s.md.String()
	golden(t, "chrome-board.pdf.md", md)
	// Top to bottom, though the page's y axis points down.
	i1 := strings.Index(md, "Протокол засідання правління")
	i2 := strings.Index(md, "Рішення: затвердити бюджет на 2027 рік.")
	i3 := strings.Index(md, "Голова правління: Іван Петренко. Ґанок, їжак, є.")
	if i1 < 0 || i2 < i1 || i3 < i2 {
		t.Errorf("rows missing or out of order (%d %d %d):\n%s", i1, i2, i3, md)
	}
	if rep.Pages != 2 {
		t.Errorf("report = %+v", rep)
	}
}

func TestExtractPDF_YieldPerPageReported(t *testing.T) {
	text, _ := boardPDF(t)
	_, rep, err := extractPDF(t, text, Options{Name: "board.pdf"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.YieldCharsPerPage < LowYieldChars {
		t.Errorf("a text PDF reports a yield of %.1f, want at least %d: %+v", rep.YieldCharsPerPage, LowYieldChars, rep)
	}
	for _, n := range rep.Notes {
		if strings.Contains(n, "scanned") {
			t.Errorf("a text PDF is reported as scanned: %q", n)
		}
	}

	scan, _ := scanPDF(t)
	_, rep, err = extractPDF(t, scan, Options{Name: "scan.pdf", Images: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Pages != 1 || rep.YieldCharsPerPage >= 1 {
		t.Errorf("a scan reports %+v, want a near-zero yield", rep)
	}
	if !hasNote(rep, "likely scanned") {
		t.Errorf("a scan has no low-yield note: %q", rep.Notes)
	}

	// A text page among scans: only the scans are listed.
	mixed := pdftest.Doc{Pages: []pdftest.Page{
		{Images: []pdftest.Image{{JPEG: tinyJPEG(t), W: 8, H: 8, Width: 595, Height: 842}}},
		{Lines: []pdftest.Line{{Text: strings.Repeat("Рішення правління про бюджет. ", 4), X: 72, Y: 700, Size: 11}}},
		{Images: []pdftest.Image{{JPEG: tinyJPEG(t), W: 8, H: 8, Width: 595, Height: 842}}},
		{},
	}}.Bytes()
	_, rep, err = extractPDF(t, mixed, Options{Name: "mixed.pdf"})
	if err != nil {
		t.Fatal(err)
	}
	if !hasNote(rep, "likely scanned: 1, 3–4") {
		t.Errorf("notes = %q, want pages 1, 3–4 listed", rep.Notes)
	}
}

func hasNote(rep Report, sub string) bool {
	for _, n := range rep.Notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}

func TestExtractPDF_JPEGCopiedAsIs(t *testing.T) {
	data, pic := boardPDF(t)
	s, rep, err := extractPDF(t, data, Options{Name: "board.pdf", Images: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.images) != 1 {
		t.Fatalf("images = %d, want the JPEG only:\n%s", len(s.images), s.md.String())
	}
	img := s.images[0]
	if !bytes.Equal(img.Data, pic) {
		t.Error("the JPEG's bytes were changed")
	}
	if img.MediaType != "image/jpeg" || img.Name != "img-1.jpg" || img.Page != 1 || img.Slide != 0 {
		t.Errorf("image = {%s %s page %d slide %d}", img.Name, img.MediaType, img.Page, img.Slide)
	}
	if !strings.Contains(s.md.String(), "![](volume:inbox:extracted/doc/img-1.jpg)") || rep.Images != 1 {
		t.Errorf("report %+v, md:\n%s", rep, s.md.String())
	}
	// The raw-pixel picture is counted, not converted.
	if !hasNote(rep, "1 images stored as raw pixels") {
		t.Errorf("notes = %q", rep.Notes)
	}

	// The scan's page picture is kept with its page.
	scan, spic := scanPDF(t)
	s, _, err = extractPDF(t, scan, Options{Name: "scan.pdf", Images: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.images) != 1 || !bytes.Equal(s.images[0].Data, spic) || s.images[0].Page != 1 {
		t.Errorf("scan images = %+v", s.images)
	}

	// Without images, nothing is handed over.
	s, _, err = extractPDF(t, data, Options{Name: "board.pdf"})
	if err != nil || len(s.images) != 0 {
		t.Errorf("images off: err=%v images=%d", err, len(s.images))
	}
}

func TestExtractPDF_EncryptedRefused(t *testing.T) {
	plain, _ := boardPDF(t)
	for _, tc := range []struct{ name, user, owner string }{
		{"user password", "secret", "owner"},
		// Opens without a password, but its owner restricted it.
		{"owner password only", "", "owner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conf := model.NewAESConfiguration(tc.user, tc.owner, 256)
			conf.Offline = true
			var enc bytes.Buffer
			if err := api.Encrypt(context.Background(), bytes.NewReader(plain), &enc, conf); err != nil {
				t.Fatalf("encrypt the fixture: %v", err)
			}
			start := time.Now()
			s, _, err := extractPDF(t, enc.Bytes(), Options{Name: "x.pdf", Images: true})
			if !IsRefused(err) || !strings.Contains(err.Error(), "encrypted") {
				t.Fatalf("err = %v, want an encryption refusal", err)
			}
			if s.md.Len() != 0 || len(s.images) != 0 {
				t.Errorf("an encrypted PDF wrote %q and %d images", s.md.String(), len(s.images))
			}
			if d := time.Since(start); d > 5*time.Second {
				t.Errorf("the refusal took %s", d)
			}
		})
	}
}

// malformedPDFs are files a parser must survive: each returns, with or
// without an error, without a panic and within the deadline.
func malformedPDFs(t testing.TB) map[string][]byte {
	good, _ := boardPDF(t)
	out := map[string][]byte{
		"truncated":    good[:len(good)/2],
		"header only":  []byte("%PDF-1.7\n"),
		"empty":        nil,
		"not a pdf":    []byte("PK\x03\x04 this is a zip"),
		"no startxref": bytes.ReplaceAll(good, []byte("startxref"), []byte("startxrex")),
	}
	// Garbage in place of object bodies.
	garbage := bytes.ReplaceAll(good, []byte("/Type /Page "), []byte("/Type (]]]>>>{{{ "))
	garbage = bytes.ReplaceAll(garbage, []byte("/DescendantFonts"), []byte("/DescendantFonts <<<<<<"))
	out["garbage objects"] = garbage

	// A cross-reference section that names itself as the previous one.
	{
		w := &pdftest.Writer{}
		cat := w.Reserve()
		pages := w.Add("<< /Type /Pages /Kids [] /Count 0 >>")
		w.Set(cat, "<< /Type /Catalog /Pages 2 0 R >>")
		b := w.Bytes(cat, "")
		i := bytes.LastIndex(b, []byte("startxref\n"))
		xref := string(b[i+len("startxref\n") : len(b)-len("\n%%EOF\n")])
		out["cyclic xref"] = bytes.Replace(b, []byte("/Root 1 0 R"), []byte("/Root 1 0 R /Prev "+xref), 1)
		_ = pages
	}
	// A page tree whose node is its own kid, with a huge declared count.
	{
		w := &pdftest.Writer{}
		cat := w.Reserve()
		pages := w.Reserve()
		w.Set(pages, "<< /Type /Pages /Kids [2 0 R 2 0 R] /Count 1000000000 >>")
		w.Set(cat, "<< /Type /Catalog /Pages 2 0 R >>")
		out["cyclic page tree"] = w.Bytes(cat, "")
	}
	// Content that nests arrays without end, an unterminated string, a
	// font that is not a dictionary, and a form that draws itself.
	{
		w := &pdftest.Writer{}
		cat := w.Reserve()
		pages := w.Reserve()
		form := w.Reserve()
		w.Set(form, pdftest.Stream("/Type /XObject /Subtype /Form /BBox [0 0 10 10] /Resources << /XObject << /Fm 3 0 R >> >>", []byte("/Fm Do /Fm Do")))
		content := w.Add(pdftest.Stream("", []byte(strings.Repeat("[", 100000)+" TJ BT /F1 12 Tf (unterminated Tj /Fm Do <<<< BI /W 1 ID \x00\x01 ")))
		page := w.Add("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 10 10] /Resources << /Font << /F1 42 >> /XObject << /Fm 3 0 R >> >> /Contents " + itoa(content) + " 0 R >>")
		w.Set(pages, "<< /Type /Pages /Kids ["+itoa(page)+" 0 R] /Count 1 >>")
		w.Set(cat, "<< /Type /Catalog /Pages 2 0 R >>")
		out["hostile content"] = w.Bytes(cat, "")
	}
	return out
}

// bombDecodeLimit is the decode cap the bomb is read under: the real cap,
// 64 MiB, is the same code path at a cost the race detector makes minutes.
const bombDecodeLimit = 1 << 20

// bombPDF is a page whose content stream inflates past bombDecodeLimit,
// about a thousand times its compressed size.
func bombPDF() []byte {
	w := &pdftest.Writer{}
	cat := w.Reserve()
	pages := w.Reserve()
	content := w.Add(pdftest.Stream("/Filter /FlateDecode", pdftest.Flate(bytes.Repeat([]byte(" "), 2*bombDecodeLimit))))
	page := w.Add("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 10 10] /Contents " + itoa(content) + " 0 R >>")
	w.Set(pages, "<< /Type /Pages /Kids ["+itoa(page)+" 0 R] /Count 1 >>")
	w.Set(cat, "<< /Type /Catalog /Pages 2 0 R >>")
	return w.Bytes(cat, "")
}

func TestExtractPDF_MalformedDoesNotPanic(t *testing.T) {
	bomb := bombPDF()
	cases := malformedPDFs(t)
	cases["inflation bomb"] = bomb
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			s, rep, err := extractPDF(t, data, Options{Name: "x.pdf", Images: true, pdfMaxDecode: bombDecodeLimit})
			if err != nil && strings.Contains(err.Error(), "could not be read: ") && strings.Contains(err.Error(), "runtime error") {
				t.Errorf("a runtime panic was recovered: %v", err)
			}
			if d := time.Since(start); d > 15*time.Second {
				t.Errorf("took %s", d)
			}
			t.Logf("err=%v notes=%q md=%d bytes", err, rep.Notes, s.md.Len())
		})
	}
	// The bomb's page is reported unread rather than inflated.
	_, rep, err := extractPDF(t, bomb, Options{Name: "x.pdf", pdfMaxDecode: bombDecodeLimit})
	if err != nil || !hasNote(rep, "could not be read") {
		t.Errorf("bomb: err=%v notes=%q", err, rep.Notes)
	}
}

// panicSink panics on the first text it is given.
type panicSink struct{ memSink }

func (s *panicSink) Text(string) error { panic("sink exploded") }

func TestExtractPDF_PanicIsAnError(t *testing.T) {
	data, _ := boardPDF(t)
	_, err := PDF{}.Extract(context.Background(), bytes.NewReader(data), int64(len(data)), &panicSink{}, Options{Name: "x.pdf"})
	if err == nil || !strings.Contains(err.Error(), "sink exploded") {
		t.Fatalf("err = %v, want the panic as an error", err)
	}
}

func TestExtractPDF_PageCapNoted(t *testing.T) {
	var pages []pdftest.Page
	for range 5 {
		pages = append(pages, pdftest.Page{Lines: []pdftest.Line{{Text: strings.Repeat("Сторінка протоколу правління. ", 3), X: 72, Y: 700, Size: 11}}})
	}
	data := pdftest.Doc{Pages: pages}.Bytes()
	s, rep, err := extractPDF(t, data, Options{Name: "long.pdf", pdfMaxPages: 3})
	if err != nil {
		t.Fatal(err)
	}
	md := s.md.String()
	if !strings.Contains(md, "## Page 3") || strings.Contains(md, "## Page 4") {
		t.Errorf("pages read:\n%s", md)
	}
	if rep.Pages != 5 || !hasNote(rep, "only the first 3 of 5 pages were read") {
		t.Errorf("report = %+v", rep)
	}
	// The yield is over the pages read.
	if rep.YieldCharsPerPage < LowYieldChars {
		t.Errorf("yield = %.1f", rep.YieldCharsPerPage)
	}
	if MaxPDFPages != 2000 {
		t.Errorf("MaxPDFPages = %d", MaxPDFPages)
	}
}

func TestExtractPDF_ContextCancelStops(t *testing.T) {
	data, _ := boardPDF(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := PDF{}.Extract(ctx, bytes.NewReader(data), int64(len(data)), &memSink{}, Options{Name: "x.pdf"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestExtractPDF_RegisteredForTypeAndExtension(t *testing.T) {
	for _, c := range [][2]string{{"application/pdf", ""}, {"", "Протокол.PDF"}, {"application/pdf; charset=binary", "x"}} {
		e, ok := Default().For(c[0], c[1])
		if _, isPDF := e.(PDF); !ok || !isPDF {
			t.Errorf("For(%q, %q) = %T", c[0], c[1], e)
		}
	}
}

func TestPDFGlyphRune_CyrillicNames(t *testing.T) {
	for name, want := range map[string]rune{
		"afii10017": 'А', "afii10022": 'Е', "afii10023": 'Ё', "afii10024": 'Ж', "afii10049": 'Я',
		"afii10065": 'а', "afii10071": 'ё', "afii10097": 'я', "afii10056": 'Ї', "afii10104": 'ї',
		"afii10050": 'Ґ', "afii10098": 'ґ', "afii10053": 'Є', "afii10055": 'І', "afii10145": 'Џ',
		"uni0416": 'Ж', "u1F600": '😀', "Zh": 0, "A": 'A', "endash": '–', "a.sc": 'a',
	} {
		if got := glyphRune(name); got != want {
			t.Errorf("glyphRune(%q) = %q, want %q", name, got, want)
		}
	}
}

func FuzzExtractPDF(f *testing.F) {
	board, _ := boardPDF(f)
	scan, _ := scanPDF(f)
	f.Add(board)
	f.Add(scan)
	for _, b := range malformedPDFs(f) {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := PDF{}.Extract(ctx, bytes.NewReader(data), int64(len(data)), &memSink{limit: 1 << 20}, Options{Name: "f.pdf", Images: true})
		// Extract recovers panics; under the fuzzer a recovered one is
		// still a bug to report.
		if err != nil && strings.Contains(err.Error(), "the PDF could not be read: ") {
			t.Fatalf("recovered panic: %v", err)
		}
	})
}
