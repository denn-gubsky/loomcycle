// Package pdftest writes small PDFs for tests: text in a Type0 font with the
// Identity-H encoding and a ToUnicode CMap (the way Word and LibreOffice embed
// Cyrillic), pictures as JPEG (DCTDecode) or raw pixels (FlateDecode), and,
// through Writer, any object graph a test needs, malformed ones included.
//
// The output is deterministic: the same Doc always gives the same bytes.
package pdftest

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"sort"
	"strings"
)

// Writer assembles a PDF from numbered objects.
type Writer struct {
	objs []string
}

// Add adds an object (its body, without "n 0 obj") and returns its number.
func (w *Writer) Add(body string) int {
	w.objs = append(w.objs, body)
	return len(w.objs)
}

// Set replaces the body of object n.
func (w *Writer) Set(n int, body string) { w.objs[n-1] = body }

// Reserve adds an empty object, to be Set later, and returns its number.
func (w *Writer) Reserve() int { return w.Add("null") }

// Stream returns a stream object body: dict entries (without << >>) and the
// data, with its Length.
func Stream(dict string, data []byte) string {
	return fmt.Sprintf("<< %s /Length %d >>\nstream\n%s\nendstream", dict, len(data), data)
}

// Flate compresses data as FlateDecode does.
func Flate(data []byte) []byte {
	var b bytes.Buffer
	zw := zlib.NewWriter(&b)
	_, _ = zw.Write(data)
	_ = zw.Close()
	return b.Bytes()
}

// Bytes writes the file: header, objects, cross-reference table and a
// trailer with /Root root and the extra trailer entries given.
func (w *Writer) Bytes(root int, trailer string) []byte {
	var b bytes.Buffer
	b.WriteString("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n")
	offs := make([]int, len(w.objs))
	for i, o := range w.objs {
		offs[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(w.objs)+1)
	for _, off := range offs {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root %d 0 R %s >>\nstartxref\n%d\n%%%%EOF\n", len(w.objs)+1, root, trailer, xref)
	return b.Bytes()
}

// Line is one line of text, at (X, Y) from the bottom left, in points.
type Line struct {
	Text string
	X, Y float64
	Size float64
	// Kern draws the spaces between words as TJ position adjustments
	// instead of space glyphs, as some producers do.
	Kern bool
}

// Image is a picture drawn on a page.
type Image struct {
	// JPEG is drawn as a DCTDecode image of W×H pixels.
	JPEG []byte
	// Pixels, when JPEG is nil, is drawn as raw RGB pixels (FlateDecode).
	Pixels []byte
	W, H   int
	// X, Y, Width, Height place it on the page, in points.
	X, Y, Width, Height float64
}

// Page is one page: its lines and pictures.
type Page struct {
	Lines  []Line
	Images []Image
}

// Doc is a document of pages, A4 portrait.
type Doc struct {
	Pages []Page
}

// Bytes writes the document.
func (d Doc) Bytes() []byte {
	w := &Writer{}
	catalog := w.Reserve()
	pages := w.Reserve()

	// One font for the whole document: CIDs numbered from 1 in the order
	// the characters first appear.
	cids := map[rune]int{}
	var order []rune
	for _, p := range d.Pages {
		for _, l := range p.Lines {
			for _, r := range l.Text {
				if _, ok := cids[r]; !ok {
					cids[r] = len(order) + 1
					order = append(order, r)
				}
			}
		}
	}
	font := w.Add(fmt.Sprintf("<< /Type /Font /Subtype /Type0 /BaseFont /AAAAAA+TestSans /Encoding /Identity-H /DescendantFonts [%d 0 R] /ToUnicode %d 0 R >>",
		w.Add(fmt.Sprintf("<< /Type /Font /Subtype /CIDFontType2 /BaseFont /AAAAAA+TestSans /CIDSystemInfo << /Registry (Adobe) /Ordering (Identity) /Supplement 0 >> /FontDescriptor %d 0 R /DW 1000 /W [1 [%s]] /CIDToGIDMap /Identity >>",
			w.Add("<< /Type /FontDescriptor /FontName /AAAAAA+TestSans /Flags 32 /FontBBox [0 -200 1000 900] /ItalicAngle 0 /Ascent 900 /Descent -200 /CapHeight 700 /StemV 80 >>"),
			strings.TrimSpace(strings.Repeat("600 ", len(order))))),
		w.Add(Stream("", toUnicode(order)))))

	var kids []string
	for _, p := range d.Pages {
		var c strings.Builder
		xobjects := map[string]int{}
		for i, img := range p.Images {
			name := fmt.Sprintf("Im%d", i+1)
			var body string
			if img.JPEG != nil {
				body = Stream(fmt.Sprintf("/Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /DCTDecode", img.W, img.H), img.JPEG)
			} else {
				body = Stream(fmt.Sprintf("/Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /FlateDecode", img.W, img.H), Flate(img.Pixels))
			}
			xobjects[name] = w.Add(body)
			fmt.Fprintf(&c, "q %g 0 0 %g %g %g cm /%s Do Q\n", img.Width, img.Height, img.X, img.Y, name)
		}
		for _, l := range p.Lines {
			fmt.Fprintf(&c, "BT\n/F1 %g Tf\n1 0 0 1 %g %g Tm\n%s TJ\nET\n", l.Size, l.X, l.Y, tj(l, cids))
		}
		content := w.Add(Stream("/Filter /FlateDecode", Flate([]byte(c.String()))))
		var xo strings.Builder
		names := make([]string, 0, len(xobjects))
		for n := range xobjects {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Fprintf(&xo, "/%s %d 0 R ", n, xobjects[n])
		}
		page := w.Add(fmt.Sprintf("<< /Type /Page /Parent %d 0 R /MediaBox [0 0 595 842] /Resources << /Font << /F1 %d 0 R >> /XObject << %s>> >> /Contents %d 0 R >>",
			pages, font, xo.String(), content))
		kids = append(kids, fmt.Sprintf("%d 0 R", page))
	}
	w.Set(pages, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(kids)))
	w.Set(catalog, fmt.Sprintf("<< /Type /Catalog /Pages %d 0 R >>", pages))
	return w.Bytes(catalog, "")
}

// tj is a line's TJ array: each character as its two-byte CID.
func tj(l Line, cids map[rune]int) string {
	var b strings.Builder
	b.WriteString("[<")
	for _, r := range l.Text {
		if r == ' ' && l.Kern {
			b.WriteString("> -300 <")
			continue
		}
		fmt.Fprintf(&b, "%04X", cids[r])
	}
	b.WriteString(">]")
	return b.String()
}

// toUnicode is the CMap for CIDs 1..n: the first half as bfchar entries,
// the rest as one-code bfrange entries, so both forms are read.
func toUnicode(order []rune) []byte {
	var b strings.Builder
	b.WriteString("/CIDInit /ProcSet findresource begin\n12 dict begin\nbegincmap\n/CIDSystemInfo << /Registry (Adobe) /Ordering (UCS) /Supplement 0 >> def\n/CMapName /Adobe-Identity-UCS def\n/CMapType 2 def\n1 begincodespacerange\n<0000> <FFFF>\nendcodespacerange\n")
	half := len(order) / 2
	if half > 0 {
		fmt.Fprintf(&b, "%d beginbfchar\n", half)
		for i, r := range order[:half] {
			fmt.Fprintf(&b, "<%04X> <%s>\n", i+1, utf16Hex(r))
		}
		b.WriteString("endbfchar\n")
	}
	if rest := order[half:]; len(rest) > 0 {
		fmt.Fprintf(&b, "%d beginbfrange\n", len(rest))
		for i, r := range rest {
			cid := half + i + 1
			fmt.Fprintf(&b, "<%04X> <%04X> <%s>\n", cid, cid, utf16Hex(r))
		}
		b.WriteString("endbfrange\n")
	}
	b.WriteString("endcmap\nCMapName currentdict /CMap defineresource pop\nend\nend\n")
	return []byte(b.String())
}

func utf16Hex(r rune) string {
	if r < 0x10000 {
		return fmt.Sprintf("%04X", r)
	}
	r -= 0x10000
	return fmt.Sprintf("%04X%04X", 0xD800+(r>>10), 0xDC00+(r&0x3FF))
}
