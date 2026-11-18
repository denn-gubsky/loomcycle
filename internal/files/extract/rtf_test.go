package extract

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/text/encoding/charmap"
)

// rtfHex writes s as \'hh bytes of windows-1251, the way Word writes
// Cyrillic in a document of that code page.
func rtfHex(t testing.TB, s string) string {
	t.Helper()
	b, err := charmap.Windows1251.NewEncoder().Bytes([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for _, c := range b {
		if c < 0x80 {
			out.WriteByte(c)
			continue
		}
		fmt.Fprintf(&out, `\'%02x`, c)
	}
	return out.String()
}

// rtfText is a Word-style document with everything the reader handles:
// a code page with Cyrillic in \'hh bytes, a font table with charsets, a
// stylesheet naming a heading style, \info metadata, a header and other
// destinations that are skipped, a heading by outline level and one by
// style, bold and italic runs, \uN escapes with their replacements, a
// footnote, a field, a table, a PNG picture in hex beside its WMF
// fallback, and a hard line break.
func rtfText(t testing.TB) string {
	return `{\rtf1\ansi\ansicpg1251\deff0\uc1
{\fonttbl{\f0\froman\fcharset204 Times New Roman;}{\f1\fswiss\fcharset0 Arial;}{\f2\fnil\fcharset161 Greek;}}
{\colortbl;\red0\green0\blue0;}
{\stylesheet{\ql \f0 Normal;}{\s2\ql\b\f1 heading 2,H2;}{\*\cs10 \additive Default Paragraph Font;}}
{\*\generator Riched20 10.0;}
{\info{\title ` + rtfHex(t, "Мой документ") + `}{\author Ivan Petrov}{\subject Reports}{\keywords alpha, beta; gamma}{\doccomm A comment.}{\creatim\yr2019\mo5\dy7\hr10\min3}{\revtim\yr2021\mo1\dy1}}
{\header \pard\plain Header text\par}
\pard\plain\outlinelevel0\f0 ` + rtfHex(t, "Глава") + ` 1\par
\pard\plain\f0 ` + rtfHex(t, "Привет") + `, \b bold\b0  and \i italic\i0  text.\par
\pard\plain Unicode: \u1046\'3f and \u8212\'97 end.\par
\pard\plain\s2 Styled heading\par
\pard\plain See the note{\cs16\super\chftn}{\footnote\pard\plain{\cs16\super\chftn} The note text.}.\par
\pard\plain A {\field{\*\fldinst {HYPERLINK "http://example.com"}}{\fldrslt {link text}}} here.{\*\userprops secret}\par
\trowd\cellx1000\cellx2000\pard\plain\intbl Name\cell Value\cell\row
\trowd\cellx1000\cellx2000\pard\plain\intbl a|b\cell 1\cell\row
\pard\plain After the table.\par
{\*\shppict{\pict\pngblip\picw2\pich2 ` + hex.EncodeToString(tinyPNG(t)) + `}}{\nonshppict{\pict\wmetafile8 0102}}\par
\pard\plain # not a heading\line second line\par
}`
}

const rtfWant = `# Мой документ

## Глава 1

Привет, **bold** and *italic* text.

Unicode: Ж and — end.

### Styled heading

See the note[1].

A link text here.

| Name | Value |
| --- | --- |
| a\|b | 1 |

After the table.

![](volume:inbox:extracted/doc/img-1.png)

\# not a heading\
second line

## Notes

[1] The note text.`

var rtfWantMeta = &BookMetadata{
	Title:        "Мой документ",
	Authors:      []string{"Ivan Petrov"},
	Date:         "2019-05-07",
	Year:         2019,
	Subjects:     []string{"Reports", "alpha", "beta", "gamma"},
	Description:  "A comment.",
	SourceFormat: "rtf",
}

func TestRTF_DocumentReadsAsMarkdownAndMetadata(t *testing.T) {
	s, rep := run(t, RTF{}, []byte(rtfText(t)), Options{Name: "report.rtf", Images: true})
	if got := s.md.String(); got != rtfWant {
		t.Errorf("markdown:\n%s\n--- want ---\n%s", got, rtfWant)
	}
	if !reflect.DeepEqual(rep.Metadata, rtfWantMeta) {
		t.Errorf("metadata:\n%+v\nwant\n%+v", rep.Metadata, rtfWantMeta)
	}
	if rep.Format != "rtf" || rep.Images != 1 || rep.Tables != 1 || len(s.images) != 1 || len(rep.Notes) != 0 {
		t.Errorf("report %+v, %d images", rep, len(s.images))
	}
	if len(s.images) == 1 && (s.images[0].MediaType != "image/png" || !bytes.Equal(s.images[0].Data, tinyPNG(t))) {
		t.Errorf("picture: %s, %d bytes", s.images[0].MediaType, len(s.images[0].Data))
	}
}

func TestRTF_ImagesOffWritesAltTextAndKeepsNothing(t *testing.T) {
	s, rep := run(t, RTF{}, []byte(rtfText(t)), Options{Name: "report.rtf"})
	if len(s.images) != 0 || rep.Images != 0 || !strings.Contains(s.md.String(), "\n\nAfter the table.\n\n[image]\n\n") {
		t.Errorf("images off:\n%s", s.md.String())
	}
}

func TestRTF_TitleIsTheFileNameWithoutInfo(t *testing.T) {
	s, rep := run(t, RTF{}, []byte(`{\rtf1\ansi Hello.\par}`), Options{Name: "notes.rtf"})
	if s.md.String() != "# notes\n\nHello." || rep.Metadata != nil {
		t.Errorf("got %q, metadata %+v", s.md.String(), rep.Metadata)
	}
}

func TestRTF_UnknownCodePageIsRefusedByName(t *testing.T) {
	for word, name := range map[string]string{`\ansicpg932`: "932", `\pc`: "437", `\ansicpg1253`: "1253"} {
		_, err := extractBytes(RTF{}, "a.rtf", []byte(`{\rtf1\ansi`+word+` text \'82\'a0\par}`))
		if !IsRefused(err) || !strings.Contains(err.Error(), "code page "+name) {
			t.Errorf("%s: err = %v, want a refusal naming code page %s", word, err, name)
		}
	}
	// A code page is refused only when a byte needs it: text of \uN
	// escapes and ASCII reads.
	s, _ := run(t, RTF{}, []byte(`{\rtf1\ansi\ansicpg932\uc2 ok \u12354\'82\'a0\par}`), Options{Name: "a.rtf"})
	if s.md.String() != "# a\n\nok あ" {
		t.Errorf("got %q", s.md.String())
	}
}

func TestRTF_CodePagesOfTheListAreRead(t *testing.T) {
	for word, c := range map[string]struct {
		cm   *charmap.Charmap
		text string
	}{
		`\ansicpg1250`:  {charmap.Windows1250, "Łódź"},
		`\ansicpg866`:   {charmap.CodePage866, "Київ"},
		`\ansicpg20866`: {charmap.KOI8R, "Москва"},
		`\ansicpg21866`: {charmap.KOI8U, "Київ"},
		`\mac`:          {charmap.Macintosh, "Café"},
	} {
		b, err := c.cm.NewEncoder().Bytes([]byte(c.text))
		if err != nil {
			t.Fatal(err)
		}
		var hexed strings.Builder
		for _, x := range b {
			fmt.Fprintf(&hexed, `\'%02x`, x)
		}
		s, _ := run(t, RTF{}, []byte(`{\rtf1`+word+` `+hexed.String()+`\par}`), Options{Name: "a.rtf"})
		if s.md.String() != "# a\n\n"+c.text {
			t.Errorf("%s: got %q, want %q", word, s.md.String(), c.text)
		}
	}
}

// A font's charset chooses its code page; one not read falls back to the
// document's code page and is counted.
func TestRTF_FontCharsetsChooseTheCodePage(t *testing.T) {
	doc := `{\rtf1\ansi\ansicpg1252{\fonttbl{\f0\fcharset0 Arial;}{\f1\fcharset204 Arial Cyr;}{\f2\fcharset161 Arial Greek;}}` +
		`\f0 caf\'e9 {\f1 ` + rtfHex(t, "Київ") + `} \f2 \'e9\par}`
	s, rep := run(t, RTF{}, []byte(doc), Options{Name: "a.rtf"})
	if s.md.String() != "# a\n\ncafé Київ é" || len(rep.Notes) != 1 || !strings.Contains(rep.Notes[0], "character set") {
		t.Errorf("got %q, notes %v", s.md.String(), rep.Notes)
	}
}

func TestRTF_DeepGroupsAreRefused(t *testing.T) {
	deep := `{\rtf1` + strings.Repeat("{", maxRTFDepth+1) + "x" + strings.Repeat("}", maxRTFDepth+1) + "}"
	if _, err := extractBytes(RTF{}, "a.rtf", []byte(deep)); !IsRefused(err) || !strings.Contains(err.Error(), "deeper") {
		t.Fatalf("err = %v, want a refusal", err)
	}
	ok := `{\rtf1` + strings.Repeat("{", maxRTFDepth-1) + "x" + strings.Repeat("}", maxRTFDepth-1) + "}"
	if s, _ := run(t, RTF{}, []byte(ok), Options{Name: "a.rtf"}); s.md.String() != "# a\n\nx" {
		t.Errorf("at the depth cap: %q", s.md.String())
	}
}

func TestRTF_ControlWordsPastTheirBoundsAreRefused(t *testing.T) {
	for name, doc := range map[string]string{
		"long word":    `{\rtf1\` + strings.Repeat("a", maxRTFWord+1) + ` x}`,
		"long number":  `{\rtf1\b12345678901 x}`,
		"out of range": `{\rtf1\b2147483648 x}`,
		"negative":     `{\rtf1\b-2147483649 x}`,
		"wraps int64":  `{\rtf1\b18446744073709551617 x}`,
	} {
		if _, err := extractBytes(RTF{}, "a.rtf", []byte(doc)); !IsRefused(err) {
			t.Errorf("%s: err = %v, want a refusal", name, err)
		}
	}
	for _, doc := range []string{`{\rtf1\` + strings.Repeat("a", maxRTFWord) + ` x}`, `{\rtf1\b-2147483648 x}`, `{\rtf1\b2147483647 x}`} {
		if _, err := extractBytes(RTF{}, "a.rtf", []byte(doc)); err != nil {
			t.Errorf("%.40s: %v", doc, err)
		}
	}
}

func TestRTF_FileOverTheCapIsRefusedUnread(t *testing.T) {
	r := &countingReaderAt{r: bytes.NewReader([]byte(`{\rtf1 x}`))}
	_, err := RTF{}.Extract(context.Background(), r, maxRTFBytes+1, &memSink{}, Options{})
	if !IsRefused(err) || r.n.Load() != 0 {
		t.Errorf("err = %v, %d bytes read", err, r.n.Load())
	}
}

func TestRTF_NotRTFIsRefused(t *testing.T) {
	if _, err := extractBytes(RTF{}, "a.rtf", []byte(`<html>x</html>`)); !IsRefused(err) {
		t.Errorf("err = %v, want a refusal", err)
	}
	// A byte order mark before the header is allowed.
	if s, _ := run(t, RTF{}, []byte("\xEF\xBB\xBF{\\rtf1 x}"), Options{Name: "a.rtf"}); s.md.String() != "# a\n\nx" {
		t.Errorf("after a BOM: %q", s.md.String())
	}
}

func TestRTF_BinLongerThanTheFileIsRefused(t *testing.T) {
	for _, doc := range []string{`{\rtf1{\pict\pngblip\bin1000 abc}}`, `{\rtf1{\*\skipme\bin1000 abc}}`, `{\rtf1{\pict\pngblip\bin-1 abc}}`} {
		if _, err := extractBytes(RTF{}, "a.rtf", []byte(doc)); !IsRefused(err) || !strings.Contains(err.Error(), `\bin`) {
			t.Errorf("%s: err = %v, want a refusal", doc, err)
		}
	}
	// Raw \bin data is read as data, braces and backslashes included.
	png := tinyPNG(t)
	doc := []byte(`{\rtf1 a{\pict\pngblip\bin` + fmt.Sprint(len(png)+3) + ` {}\` + string(png) + `}b{\*\x\bin4 }{\\}c}`)
	s, rep := run(t, RTF{}, doc, Options{Name: "a.rtf", Images: true})
	// The data is "{}\" before the PNG, so it is no image type: alt text
	// and a note, not a picture; and none of it was read as markup.
	if s.md.String() != "# a\n\na\n\n[image]\n\nbc" || rep.Images != 0 || len(rep.Notes) != 1 {
		t.Errorf("got %q, notes %v", s.md.String(), rep.Notes)
	}
	doc = []byte(`{\rtf1 a{\pict\pngblip\bin` + fmt.Sprint(len(png)) + ` ` + string(png) + `}b{\*\x\bin4 }{\\}c}`)
	s, rep = run(t, RTF{}, doc, Options{Name: "a.rtf", Images: true})
	if s.md.String() != "# a\n\na\n\n![](volume:inbox:extracted/doc/img-1.png)\n\nbc" || rep.Images != 1 {
		t.Errorf("got %q, report %+v", s.md.String(), rep)
	}
}

func rtfWithPicture(n int, bin bool) []byte {
	pic := make([]byte, n)
	_, _ = rand.Read(pic)
	if bin {
		return []byte(`{\rtf1 Before.{\pict\pngblip\bin` + fmt.Sprint(n) + ` ` + string(pic) + `}}`)
	}
	return []byte(`{\rtf1 Before.{\pict\pngblip ` + hex.EncodeToString(pic) + `}}`)
}

// An oversized picture is refused having held at most the cap: hex or
// \bin data is taken a byte at a time behind the check.
func TestRTF_PictureOverTheCapIsRefusedBeforeDecoding(t *testing.T) {
	const limit = 1024
	for _, bin := range []bool{false, true} {
		big := rtfWithPicture(3<<20, bin)
		var err error
		got := allocated(func() {
			_, err = RTF{}.Extract(context.Background(), bytes.NewReader(big), int64(len(big)), &memSink{}, Options{Images: true, rtfMaxPicture: limit})
		})
		if !IsRefused(err) || !strings.Contains(err.Error(), "picture") {
			t.Fatalf("bin %t, a 3 MiB picture: err = %v, want a refusal", bin, err)
		}
		if got > ceiling(1<<20) {
			t.Errorf("bin %t: refusing a 3 MiB picture allocated %d KiB", bin, got>>10)
		}
		just := rtfWithPicture(limit+1, bin)
		if _, err := (RTF{}).Extract(context.Background(), bytes.NewReader(just), int64(len(just)), &memSink{}, Options{Images: true, rtfMaxPicture: limit}); !IsRefused(err) {
			t.Errorf("bin %t, a picture just over the cap: err = %v, want a refusal", bin, err)
		}
		// At the cap it is read; random bytes are no image type, so it is
		// written as alt text and counted as left out.
		at := rtfWithPicture(limit, bin)
		s, rep := run(t, RTF{}, at, Options{Name: "p.rtf", Images: true, rtfMaxPicture: limit})
		if s.md.String() != "# p\n\nBefore.\n\n[image]" || len(rep.Notes) != 1 {
			t.Errorf("bin %t, a picture at the cap: %q, notes %v", bin, s.md.String(), rep.Notes)
		}
		// With pictures off nothing is held, and nothing is refused.
		if _, err := extractBytesOpts(RTF{}, big, Options{rtfMaxPicture: limit}); err != nil {
			t.Errorf("bin %t, pictures off: %v", bin, err)
		}
	}
}

func extractBytesOpts(e Extractor, data []byte, opts Options) (Report, error) {
	return e.Extract(context.Background(), bytes.NewReader(data), int64(len(data)), &memSink{}, opts)
}

func TestRTF_OtherPictureKindsAreCountedInANote(t *testing.T) {
	s, rep := run(t, RTF{}, []byte(`{\rtf1 a{\pict\wmetafile8 0102}b{\pict\emfblip 0304}\par}`), Options{Name: "a.rtf", Images: true})
	if s.md.String() != "# a\n\na[image]b[image]" || len(rep.Notes) != 1 || !strings.HasPrefix(rep.Notes[0], "2 pictures in WMF") {
		t.Errorf("got %q, notes %v", s.md.String(), rep.Notes)
	}
}

// A document cut at its output limit stops reading there: the rest of the
// file, its footnotes and pictures, is never read.
func TestRTF_MaxCharsStopsReading(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{\rtf1\ansi\ansicpg1251 `)
	for i := 0; i < 20000; i++ {
		fmt.Fprintf(&b, `\pard Paragraph %d of a long document, with a note{\footnote Note %d.}.\par`+"\n", i, i)
	}
	b.WriteString(`{\pict\pngblip ` + strings.Repeat("00", 1<<20) + `}}`)
	data := []byte(b.String())
	r := &countingReaderAt{r: bytes.NewReader(data)}
	s := &memSink{limit: 4 << 10}
	rep, err := RTF{}.Extract(context.Background(), r, int64(len(data)), s, Options{Name: "long.rtf", Images: true})
	if err != ErrSinkFull {
		t.Fatalf("err = %v, want ErrSinkFull", err)
	}
	if got := r.n.Load(); got > 64<<10 {
		t.Errorf("read %d of the file's %d bytes after a cut at 4 KiB", got, len(data))
	}
	if strings.Contains(s.md.String(), "## Notes") || rep.Chars == 0 {
		t.Errorf("chars %d, markdown:\n%s", rep.Chars, s.md.String())
	}
}

func TestRTF_FootnotesAreBoundedAndHiddenTextIsLeftOut(t *testing.T) {
	doc := `{\rtf1 a{\footnote ` + strings.Repeat("n", maxRTFNote+100) + `}b{\v hidden{\footnote x}}c{\footnote{\footnote inner}outer}\par}`
	s, rep := run(t, RTF{}, []byte(doc), Options{Name: "a.rtf"})
	md := s.md.String()
	if !strings.HasPrefix(md, "# a\n\na[1]bc[2]\n\n## Notes\n\n[1] nnn") || !strings.HasSuffix(md, "\n\n[2] outer") {
		t.Errorf("got %.80q ... %q", md, md[max(len(md)-40, 0):])
	}
	if n := strings.Count(md, "n"); n > maxRTFNote+10 || len(rep.Notes) != 0 {
		t.Errorf("a footnote kept %d bytes, notes %v", n, rep.Notes)
	}
}

func TestRTF_DecodedTextOverTheCapIsRefused(t *testing.T) {
	doc := []byte(`{\rtf1\ansi\ansicpg1251 ` + strings.Repeat(`\'e0`, 10000) + `}`)
	_, err := extractBytesOpts(RTF{}, doc, Options{rtfMaxDecoded: 10000})
	if !IsRefused(err) || !strings.Contains(err.Error(), "decodes to more than") {
		t.Errorf("err = %v, want a refusal", err)
	}
	if _, err := extractBytesOpts(RTF{}, doc, Options{}); err != nil {
		t.Errorf("under the default cap: %v", err)
	}
}

// Emphasis markers never let the document's own text start a block or
// end the emphasis early, and a heading or a cell drops them.
func TestRTF_RunsAreEscaped(t *testing.T) {
	doc := `{\rtf1 {\b # bold*star_} 1. after <b> ![x](y)\par\pard\outlinelevel1 {\b Head}ing\par\pard {\i -} item\par}`
	s, _ := run(t, RTF{}, []byte(doc), Options{Name: "a.rtf"})
	want := "# a\n\n**# bold\\*star\\_** 1. after \\<b> !\\[x](y)\n\n### Heading\n\n*-* item"
	if got := s.md.String(); got != want {
		t.Errorf("got:\n%s\n--- want ---\n%s", got, want)
	}
}

func TestRTF_UnicodeSurrogatesAndSkipCounts(t *testing.T) {
	// U+1F600 as a surrogate pair, \uc2 skipping two replacement
	// characters, \uc0 skipping none, and a control word counted as one.
	doc := `{\rtf1 \u-10179?\u-8704? {\uc2\u1046\'3f\'3fA}{\uc0\u1046 B}{\u1046\b C}\par}`
	s, _ := run(t, RTF{}, []byte(doc), Options{Name: "a.rtf"})
	if want := "# a\n\n😀 ЖAЖBЖC"; s.md.String() != want {
		t.Errorf("got %q, want %q", s.md.String(), want)
	}
}

func TestRTF_RTFWithoutClosingBraceStillReads(t *testing.T) {
	s, _ := run(t, RTF{}, []byte(`{\rtf1 {\b unclosed`), Options{Name: "a.rtf"})
	if s.md.String() != "# a\n\n**unclosed**" {
		t.Errorf("got %q", s.md.String())
	}
}
