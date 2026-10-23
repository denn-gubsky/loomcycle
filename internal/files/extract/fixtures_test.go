package extract

import (
	"archive/zip"
	"bytes"
	"context"
	"flag"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden .md files under testdata")

// tinyPNG is a real 2x2 PNG.
func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

type entry struct {
	name string
	data []byte
}

// zipOf builds an archive of the entries, deflated, in order.
func zipOf(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for _, e := range entries {
		w, err := zw.Create(e.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(e.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func xmlEntry(name, body string) entry {
	return entry{name: name, data: []byte(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n" + body)}
}

const (
	nsW   = `xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"`
	nsR   = `xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"`
	nsA   = `xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"`
	nsP   = `xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"`
	nsWP  = `xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing"`
	nsPic = `xmlns:pic="http://schemas.openxmlformats.org/drawingml/2006/picture"`
	nsPkg = `xmlns="http://schemas.openxmlformats.org/package/2006/relationships"`
	rt    = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/"
)

func relsXML(rels ...[3]string) string {
	var b strings.Builder
	b.WriteString(`<Relationships ` + nsPkg + `>`)
	for _, r := range rels {
		b.WriteString(`<Relationship Id="` + r[0] + `" Type="` + rt + r[1] + `" Target="` + r[2] + `"/>`)
	}
	b.WriteString(`</Relationships>`)
	return b.String()
}

func wPara(style, text string) string {
	ppr := ""
	if style != "" {
		ppr = `<w:pPr><w:pStyle w:val="` + style + `"/></w:pPr>`
	}
	return `<w:p>` + ppr + `<w:r><w:t xml:space="preserve">` + text + `</w:t></w:r></w:p>`
}

func wListItem(text string) string {
	return `<w:p><w:pPr><w:numPr><w:ilvl w:val="0"/><w:numId w:val="1"/></w:numPr></w:pPr><w:r><w:t>` + text + `</w:t></w:r></w:p>`
}

func wDrawing(rid, descr string) string {
	return `<w:p><w:r><w:t xml:space="preserve">Логотип організації: </w:t></w:r><w:r><w:drawing><wp:inline><wp:docPr id="1" name="Picture 1" descr="` + descr +
		`"/><a:graphic><a:graphicData><pic:pic><pic:blipFill><a:blip r:embed="` + rid +
		`"/></pic:blipFill></pic:pic></a:graphicData></a:graphic></wp:inline></w:drawing></w:r></w:p>`
}

func wCell(text string) string {
	return `<w:tc><w:p><w:r><w:t>` + text + `</w:t></w:r></w:p></w:tc>`
}

// boardDOCX is a Ukrainian board protocol: a title, two heading levels, a
// numbered list, a table, a picture, and text that looks like Markdown.
func boardDOCX(t *testing.T, png []byte) []byte {
	t.Helper()
	body := wPara("Title", "Протокол засідання правління") +
		wPara("Heading1", "Порядок денний") +
		wListItem("Звіт голови") +
		wListItem("Бюджет на 2027 рік") +
		wPara("", "# Це не заголовок, а текст документа") +
		wPara("", "![x](volume:secret:keys.png) і &lt;script&gt;alert(1)&lt;/script&gt;") +
		`<w:tbl><w:tr>` + wCell("Стаття") + wCell("Сума, грн") + `</w:tr><w:tr>` + wCell("Оренда | склад") + wCell("120 000") + `</w:tr></w:tbl>` +
		wDrawing("rId5", "Логотип") +
		wPara("Heading2", "Рішення") +
		wPara("", "Рішення: затвердити бюджет")
	doc := `<w:document ` + nsW + ` ` + nsR + ` ` + nsA + ` ` + nsWP + ` ` + nsPic + `><w:body>` + body + `<w:sectPr/></w:body></w:document>`
	styles := `<w:styles ` + nsW + `>` +
		`<w:style w:type="paragraph" w:styleId="Title"><w:name w:val="Title"/></w:style>` +
		`<w:style w:type="paragraph" w:styleId="Heading1"><w:name w:val="heading 1"/></w:style>` +
		`<w:style w:type="paragraph" w:styleId="Heading2"><w:name w:val="heading 2"/><w:basedOn w:val="Heading1"/></w:style>` +
		`</w:styles>`
	numbering := `<w:numbering ` + nsW + `><w:abstractNum w:abstractNumId="0"><w:lvl w:ilvl="0"><w:numFmt w:val="decimal"/></w:lvl></w:abstractNum>` +
		`<w:num w:numId="1"><w:abstractNumId w:val="0"/></w:num></w:numbering>`
	app := `<Properties xmlns="http://schemas.openxmlformats.org/officeDocument/2006/extended-properties"><Pages>2</Pages></Properties>`
	return zipOf(t,
		xmlEntry("[Content_Types].xml", `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`),
		xmlEntry("_rels/.rels", relsXML([3]string{"rId1", "officeDocument", "word/document.xml"}, [3]string{"rId2", "extended-properties", "docProps/app.xml"})),
		xmlEntry("docProps/app.xml", app),
		xmlEntry("word/document.xml", doc),
		xmlEntry("word/_rels/document.xml.rels", relsXML(
			[3]string{"rId1", "styles", "styles.xml"},
			[3]string{"rId2", "numbering", "numbering.xml"},
			[3]string{"rId5", "image", "media/image1.png"},
		)),
		xmlEntry("word/styles.xml", styles),
		xmlEntry("word/numbering.xml", numbering),
		entry{name: "word/media/image1.png", data: png},
	)
}

// budgetXLSX is a workbook of two sheets: a budget with shared, inline and
// boolean cells and a formula's cached value, and a long register.
func budgetXLSX(t *testing.T, registerRows int) []byte {
	t.Helper()
	wb := `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" ` + nsR + `><sheets>` +
		`<sheet name="Бюджет" sheetId="1" r:id="rId1"/><sheet name="Реєстр" sheetId="2" r:id="rId2"/></sheets></workbook>`
	sst := `<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` +
		`<si><t>Стаття</t></si><si><t>Сума</t></si><si><r><t>Оренда</t></r><r><t xml:space="preserve"> | склад</t></r></si><si><t>Затверджено</t></si></sst>`
	sheet1 := `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>` +
		`<row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c><c r="C1" t="s"><v>3</v></c></row>` +
		`<row r="2"><c r="A2" t="s"><v>2</v></c><c r="B2"><v>120000.5</v></c><c r="C2" t="b"><v>1</v></c></row>` +
		`<row r="3"><c r="A3" t="inlineStr"><is><t>Разом</t></is></c><c r="B3"><f>SUM(B2:B2)</f><v>120000.5</v></c></row>` +
		`<row r="4"/>` +
		`</sheetData></worksheet>`
	var reg strings.Builder
	reg.WriteString(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
	reg.WriteString(`<row r="1"><c r="A1" t="inlineStr"><is><t>№</t></is></c><c r="B1" t="inlineStr"><is><t>Документ</t></is></c></row>`)
	for i := 2; i <= registerRows+1; i++ {
		n := itoa(i)
		reg.WriteString(`<row r="` + n + `"><c r="A` + n + `"><v>` + itoa(i-1) + `</v></c><c r="B` + n + `" t="inlineStr"><is><t>Наказ ` + itoa(i-1) + `</t></is></c></row>`)
	}
	reg.WriteString(`</sheetData></worksheet>`)
	return zipOf(t,
		xmlEntry("_rels/.rels", relsXML([3]string{"rId1", "officeDocument", "xl/workbook.xml"})),
		xmlEntry("xl/workbook.xml", wb),
		xmlEntry("xl/_rels/workbook.xml.rels", relsXML(
			[3]string{"rId1", "worksheet", "worksheets/sheet1.xml"},
			[3]string{"rId2", "worksheet", "/xl/worksheets/sheet2.xml"},
			[3]string{"rId3", "sharedStrings", "sharedStrings.xml"},
		)),
		xmlEntry("xl/sharedStrings.xml", sst),
		xmlEntry("xl/worksheets/sheet1.xml", sheet1),
		xmlEntry("xl/worksheets/sheet2.xml", reg.String()),
	)
}

func itoa(i int) string { return strconv.Itoa(i) }

func pSp(ph, text string) string {
	nv := `<p:nvSpPr><p:cNvPr id="2" name="s"/><p:cNvSpPr/><p:nvPr/></p:nvSpPr>`
	if ph != "" {
		nv = `<p:nvSpPr><p:cNvPr id="2" name="s"/><p:cNvSpPr/><p:nvPr><p:ph type="` + ph + `"/></p:nvPr></p:nvSpPr>`
	}
	return `<p:sp>` + nv + `<p:txBody><a:p><a:r><a:t>` + text + `</a:t></a:r></a:p></p:txBody></p:sp>`
}

// boardPPTX is two slides listed in reverse of their file names, the second
// with a picture and speaker notes.
func boardPPTX(t *testing.T, png []byte) []byte {
	t.Helper()
	pres := `<p:presentation ` + nsP + ` ` + nsR + `><p:sldIdLst><p:sldId id="256" r:id="rId3"/><p:sldId id="257" r:id="rId2"/></p:sldIdLst></p:presentation>`
	slide := func(body string) string {
		return `<p:sld ` + nsP + ` ` + nsA + ` ` + nsR + `><p:cSld><p:spTree>` + body + `</p:spTree></p:cSld></p:sld>`
	}
	s1 := slide(pSp("title", "Засідання правління") + pSp("body", "Порядок денний на жовтень"))
	pic := `<p:pic><p:nvPicPr><p:cNvPr id="4" name="Picture" descr="Діаграма бюджету"/><p:cNvPicPr/><p:nvPr/></p:nvPicPr>` +
		`<p:blipFill><a:blip r:embed="rId7"/></p:blipFill></p:pic>`
	s2 := slide(pSp("title", "Рішення") + pSp("", "Рішення: затвердити бюджет") + pic)
	notes := `<p:notes ` + nsP + ` ` + nsA + `><p:cSld><p:spTree>` + pSp("sldImg", "") + pSp("body", "Наголосити на строках") + `</p:spTree></p:cSld></p:notes>`
	return zipOf(t,
		xmlEntry("_rels/.rels", relsXML([3]string{"rId1", "officeDocument", "ppt/presentation.xml"})),
		xmlEntry("ppt/presentation.xml", pres),
		xmlEntry("ppt/_rels/presentation.xml.rels", relsXML(
			[3]string{"rId2", "slide", "slides/slide1.xml"},
			[3]string{"rId3", "slide", "slides/slide2.xml"},
		)),
		// slide2.xml is listed first in sldIdLst, so it is Slide 1.
		xmlEntry("ppt/slides/slide2.xml", s1),
		xmlEntry("ppt/slides/slide1.xml", s2),
		xmlEntry("ppt/slides/_rels/slide1.xml.rels", relsXML(
			[3]string{"rId7", "image", "../media/image1.png"},
			[3]string{"rId8", "notesSlide", "../notesSlides/notesSlide1.xml"},
		)),
		xmlEntry("ppt/notesSlides/notesSlide1.xml", notes),
		entry{name: "ppt/media/image1.png", data: png},
	)
}

// memSink keeps what an extraction wrote; images are given inbox-shaped refs.
type memSink struct {
	md     strings.Builder
	images []Image
	limit  int
}

func (s *memSink) Text(md string) error {
	if s.limit > 0 && s.md.Len()+len(md) > s.limit {
		return ErrSinkFull
	}
	s.md.WriteString(md)
	return nil
}

func (s *memSink) Image(img Image) (string, error) {
	s.images = append(s.images, img)
	return "volume:inbox:extracted/doc/" + img.Name, nil
}

func run(t *testing.T, e Extractor, data []byte, opts Options) (*memSink, Report) {
	t.Helper()
	s := &memSink{}
	rep, err := e.Extract(context.Background(), bytes.NewReader(data), int64(len(data)), s, opts)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return s, rep
}

// golden compares got with testdata/<name>, or rewrites it under -update.
func golden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read golden (run with -update to create it): %v", err)
	}
	if got != string(want) {
		t.Errorf("%s differs from the golden file\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}
