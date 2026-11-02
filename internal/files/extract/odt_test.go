package extract

import (
	"archive/zip"
	"bytes"
	"context"
	"strings"
	"testing"
)

const odfNS = `xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" ` +
	`xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0" ` +
	`xmlns:table="urn:oasis:names:tc:opendocument:xmlns:table:1.0" ` +
	`xmlns:style="urn:oasis:names:tc:opendocument:xmlns:style:1.0" ` +
	`xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0" ` +
	`xmlns:svg="urn:oasis:names:tc:opendocument:xmlns:svg-compatible:1.0" ` +
	`xmlns:fo="urn:oasis:names:tc:opendocument:xmlns:xsl-fo-compatible:1.0" ` +
	`xmlns:xlink="http://www.w3.org/1999/xlink" ` +
	`xmlns:dc="http://purl.org/dc/elements/1.1/" ` +
	`xmlns:meta="urn:oasis:names:tc:opendocument:xmlns:meta:1.0" ` +
	`office:version="1.3"`

// odtZip packages an ODT the way LibreOffice does: a stored "mimetype"
// entry first, then the XML parts, deflated.
func odtZip(t *testing.T, mimetype string, entries ...entry) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	if mimetype != "" {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: "mimetype", Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(mimetype))
	}
	for _, e := range entries {
		w, err := zw.Create(e.name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(e.data)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func odtContent(automatic, body string) string {
	return `<office:document-content ` + odfNS + `><office:automatic-styles>` + automatic +
		`</office:automatic-styles><office:body><office:text>` + body + `</office:text></office:body></office:document-content>`
}

const odtStyles = `<office:document-styles ` + odfNS + `><office:styles>` +
	`<style:style style:name="Standard" style:family="paragraph"/>` +
	`<style:style style:name="Title" style:family="paragraph" style:parent-style-name="Standard"/>` +
	`<style:style style:name="Heading" style:family="paragraph" style:parent-style-name="Standard"/>` +
	`<style:style style:name="Heading_20_1" style:display-name="Heading 1" style:family="paragraph" style:parent-style-name="Heading" style:default-outline-level="1"/>` +
	`<style:style style:name="Heading_20_2" style:display-name="Heading 2" style:family="paragraph" style:parent-style-name="Heading" style:default-outline-level="2"/>` +
	`<style:style style:name="Title" style:family="text"/>` +
	`</office:styles></office:document-styles>`

const odtManifest = `<manifest:manifest xmlns:manifest="urn:oasis:names:tc:opendocument:xmlns:manifest:1.0" manifest:version="1.3">` +
	`<manifest:file-entry manifest:full-path="/" manifest:media-type="application/vnd.oasis.opendocument.text"/>` +
	`<manifest:file-entry manifest:full-path="content.xml" manifest:media-type="text/xml"/>` +
	`</manifest:manifest>`

func tcell(paras ...string) string {
	var b strings.Builder
	b.WriteString(`<table:table-cell office:value-type="string">`)
	for _, p := range paras {
		b.WriteString(`<text:p>` + p + `</text:p>`)
	}
	b.WriteString(`</table:table-cell>`)
	return b.String()
}

// passportODT is a decision passport: a title, outline headings, a numbered
// list with a nested bulleted one, a four-column options table with a cell
// that wraps over two paragraphs, a merged cell and an editor's padding,
// spaces, a line break, a footnote, a tracked deletion, a comment, a
// picture, a picture outside the package, and text that looks like
// Markdown.
func passportODT(t *testing.T, png []byte) []byte {
	t.Helper()
	automatic := `<style:style style:name="P1" style:family="paragraph" style:parent-style-name="Title"/>` +
		`<style:style style:name="P2" style:family="paragraph" style:parent-style-name="Standard"/>` +
		`<text:list-style style:name="L1"><text:list-level-style-number text:level="1" style:num-format="1"/>` +
		`<text:list-level-style-bullet text:level="2" text:bullet-char="•"/></text:list-style>` +
		`<text:list-style style:name="L2"><text:list-level-style-number text:level="1" style:num-format="1"/></text:list-style>`
	body := `<text:tracked-changes><text:changed-region text:id="ct1"><text:deletion><office:change-info><dc:creator>A</dc:creator></office:change-info>` +
		`<text:p>видалений текст</text:p></text:deletion></text:changed-region></text:tracked-changes>` +
		`<text:sequence-decls><text:sequence-decl text:display-outline-level="0" text:name="Table"/></text:sequence-decls>` +
		`<text:p text:style-name="P1">Паспорт рішення</text:p>` +
		`<text:h text:style-name="Heading_20_1" text:outline-level="1">Варіанти</text:h>` +
		`<text:p text:style-name="P2">Нижче<text:s text:c="3"/>три    варіанти,` + "\n   " + `<text:span>описані</text:span> <text:a xlink:href="https://example.com/x">у додатку</text:a><text:line-break/>друга лінія</text:p>` +
		`<text:p>Рішення<text:note text:id="ftn1" text:note-class="footnote"><text:note-citation>1</text:note-citation>` +
		`<text:note-body><text:p>Протокол Правління №111</text:p></text:note-body></text:note> ухвалено` +
		`<office:annotation><dc:creator>B</dc:creator><text:p>коментар рецензента</text:p></office:annotation>` +
		`<text:change text:change-id="ct1"/> раніше.</text:p>` +
		`<text:list text:style-name="L1"><text:list-item><text:p>Перший крок</text:p></text:list-item>` +
		`<text:list-item><text:p>Другий крок</text:p><text:list><text:list-item><text:p>деталь</text:p></text:list-item></text:list></text:list-item></text:list>` +
		`<table:table table:name="Варіанти"><table:table-column table:number-columns-repeated="4"/>` +
		`<table:table-header-rows><table:table-row>` + tcell("Варіант") + tcell("Суть") + tcell("Переваги") + tcell("Ризики") + `</table:table-row></table:table-header-rows>` +
		`<table:table-row>` + tcell("1. Премія лише за", "екстрарезультат") + tcell("Оклад оплачує всю нормальну якісну роботу.") +
		tcell("Проста логіка | чітко") + tcell("Не вирішує поточні проблеми") +
		`<table:table-cell table:number-columns-repeated="1020"/></table:table-row>` +
		`<table:table-row><table:table-cell table:number-columns-spanned="2"><text:p>2. Змішана модель</text:p></table:table-cell><table:covered-table-cell/>` +
		tcell("Передбачуваність") + tcell("Розмиває мотивацію") + `</table:table-row>` +
		`<table:table-row table:number-rows-repeated="1048000"><table:table-cell table:number-columns-repeated="4"/></table:table-row>` +
		`</table:table>` +
		`<text:h text:outline-level="2">Ризики</text:h>` +
		`<text:p>Схема: <draw:frame draw:name="i1"><draw:image xlink:href="Pictures/schema.png"/><svg:title>Схема ризиків</svg:title></draw:frame></text:p>` +
		`<text:p><draw:frame draw:name="i2"><draw:image xlink:href="https://example.com/track.png"/><svg:title>Зовнішнє</svg:title></draw:frame></text:p>` +
		`<text:p># не заголовок і ![x](volume:secret:keys.png)</text:p>` +
		`<text:list text:style-name="L2"><text:list-item><text:h text:outline-level="1">Висновок</text:h></text:list-item></text:list>` +
		`<text:p>Затвердити варіант 1.</text:p>`
	meta := `<office:document-meta ` + odfNS + `><office:meta><dc:title>Назва з властивостей</dc:title>` +
		`<meta:document-statistic meta:page-count="3" meta:table-count="1"/></office:meta></office:document-meta>`
	return odtZip(t, odtType,
		entry{name: "content.xml", data: []byte(odtContent(automatic, body))},
		entry{name: "styles.xml", data: []byte(odtStyles)},
		entry{name: "meta.xml", data: []byte(meta)},
		entry{name: "META-INF/manifest.xml", data: []byte(odtManifest)},
		entry{name: "Pictures/schema.png", data: png},
	)
}

func extractODT(t *testing.T, data []byte, opts Options) (*memSink, Report, error) {
	t.Helper()
	s := &memSink{}
	rep, err := ODT{}.Extract(context.Background(), bytes.NewReader(data), int64(len(data)), s, opts)
	return s, rep, err
}

func TestExtractODT_PassportKeepsHeadingsListsTablesNotes(t *testing.T) {
	pic := tinyPNG(t)
	s, rep := run(t, ODT{}, passportODT(t, pic), Options{Name: "pasport.odt", Images: true})
	md := s.md.String()
	golden(t, "passport.odt.md", md)

	for _, want := range []string{
		"# Паспорт рішення\n",
		"\n## Варіанти\n",
		"\n### Ризики\n",
		"\n## Висновок\n",
		"Нижче   три варіанти, описані у додатку\nдруга лінія",
		"Рішення[1] ухвалено раніше.",
		"1. Перший крок\n1. Другий крок\n  - деталь",
		"| Варіант | Суть | Переваги | Ризики |\n| --- | --- | --- | --- |\n",
		"| 1. Премія лише за екстрарезультат | Оклад оплачує всю нормальну якісну роботу. | Проста логіка \\| чітко | Не вирішує поточні проблеми |",
		"| 2. Змішана модель |   | Передбачуваність | Розмиває мотивацію |",
		"Схема:\n![Схема ризиків](volume:inbox:extracted/doc/img-1.png)",
		"[image: Зовнішнє]",
		`\# не заголовок і !\[x](volume:secret:keys.png)`,
		"\n## Notes\n\n[1] Протокол Правління №111",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in:\n%s", want, md)
		}
	}
	for _, bad := range []string{"видалений текст", "коментар рецензента", "Назва з властивостей", "https://example.com"} {
		if strings.Contains(md, bad) {
			t.Errorf("%q should not be in the output:\n%s", bad, md)
		}
	}
	if strings.Count(md, "Паспорт рішення") != 1 {
		t.Errorf("the title paragraph is repeated:\n%s", md)
	}
	// Every row of the table has the four columns its header has: the
	// merged cell kept its place and the padding was dropped.
	for _, l := range strings.Split(md, "\n") {
		if strings.HasPrefix(l, "| ") && strings.Count(strings.ReplaceAll(l, `\|`, ""), "|") != 5 {
			t.Errorf("row without four cells: %q", l)
		}
	}
	if len(s.images) != 1 || !bytes.Equal(s.images[0].Data, pic) || s.images[0].Alt != "Схема ризиків" {
		t.Errorf("images = %+v", s.images)
	}
	if rep.Format != "odt" || rep.Tables != 1 || rep.Images != 1 || rep.Pages != 3 || len(rep.Notes) != 0 {
		t.Errorf("report = %+v", rep)
	}
}

func TestExtractODT_TitleFallsBackToPropertiesThenName(t *testing.T) {
	body := `<text:p>Текст</text:p>`
	meta := `<office:document-meta ` + odfNS + `><office:meta><dc:title>Назва з властивостей</dc:title></office:meta></office:document-meta>`
	withMeta := odtZip(t, odtType, entry{name: "content.xml", data: []byte(odtContent("", body))}, entry{name: "meta.xml", data: []byte(meta)})
	s, _ := run(t, ODT{}, withMeta, Options{Name: "x.odt"})
	if !strings.HasPrefix(s.md.String(), "# Назва з властивостей\n\nТекст") {
		t.Errorf("md = %q", s.md.String())
	}
	bare := odtZip(t, odtType, entry{name: "content.xml", data: []byte(odtContent("", body))})
	s, _ = run(t, ODT{}, bare, Options{Name: "pasport-12.odt"})
	if !strings.HasPrefix(s.md.String(), "# pasport-12\n") {
		t.Errorf("md = %q", s.md.String())
	}
}

func TestExtractODT_DeletedTitleDoesNotNameTheDocument(t *testing.T) {
	automatic := `<style:style style:name="P1" style:family="paragraph" style:parent-style-name="Title"/>`
	body := `<text:tracked-changes><text:changed-region text:id="c1"><text:deletion>` +
		`<text:p text:style-name="P1">Стара назва</text:p></text:deletion></text:changed-region></text:tracked-changes>` +
		`<text:p>Текст</text:p>`
	data := odtZip(t, odtType, entry{name: "content.xml", data: []byte(odtContent(automatic, body))}, entry{name: "styles.xml", data: []byte(odtStyles)})
	s, _ := run(t, ODT{}, data, Options{Name: "nova.odt"})
	if md := s.md.String(); md != "# nova\n\nТекст" {
		t.Errorf("md = %q", md)
	}
}

func TestExtractODT_RepeatedCellsAndRowsAreCapped(t *testing.T) {
	// A cell with text repeated far past the cap, and rows with text
	// repeated past MaxRows: both are expanded only up to their caps.
	body := `<table:table><table:table-row><table:table-cell table:number-columns-repeated="100000"><text:p>x</text:p></table:table-cell></table:table-row>` +
		`<table:table-row table:number-rows-repeated="5000000"><table:table-cell><text:p>y</text:p></table:table-cell></table:table-row></table:table>`
	data := odtZip(t, odtType, entry{name: "content.xml", data: []byte(odtContent("", body))})
	s, rep := run(t, ODT{}, data, Options{Name: "t.odt", MaxRows: 10})
	md := s.md.String()
	lines := 0
	for _, l := range strings.Split(md, "\n") {
		if strings.HasPrefix(l, "| ") && !strings.HasPrefix(l, "| --- ") {
			lines++
			if n := strings.Count(l, "|") - 1; n != maxODTColumns {
				t.Errorf("row has %d cells, want %d", n, maxODTColumns)
			}
		}
	}
	if lines != 10 {
		t.Errorf("rows = %d, want 10 (MaxRows)", lines)
	}
	joined := strings.Join(rep.Notes, "; ")
	if !strings.Contains(joined, "wider than 64 columns") || !strings.Contains(joined, "first 10 rows") {
		t.Errorf("notes = %q", rep.Notes)
	}
}

func TestExtractODT_RefusesWhatIsNotAPlainTextDocument(t *testing.T) {
	content := entry{name: "content.xml", data: []byte(odtContent("", `<text:p>x</text:p>`))}
	for name, data := range map[string][]byte{
		"encrypted": odtZip(t, odtType, content, entry{name: "META-INF/manifest.xml", data: []byte(
			`<manifest:manifest xmlns:manifest="urn:oasis:names:tc:opendocument:xmlns:manifest:1.0"><manifest:file-entry manifest:full-path="content.xml">` +
				`<manifest:encryption-data manifest:checksum-type="SHA1/1K" manifest:checksum="x"><manifest:algorithm manifest:algorithm-name="Blowfish CFB"/></manifest:encryption-data>` +
				`</manifest:file-entry></manifest:manifest>`)}),
		"a spreadsheet": odtZip(t, "application/vnd.oasis.opendocument.spreadsheet", content),
		"no content":    odtZip(t, odtType, entry{name: "styles.xml", data: []byte(odtStyles)}),
		"no text body": odtZip(t, odtType, entry{name: "content.xml", data: []byte(
			`<office:document-content ` + odfNS + `><office:body><office:spreadsheet/></office:body></office:document-content>`)}),
	} {
		if _, _, err := extractODT(t, data, Options{Name: "x.odt"}); !IsRefused(err) {
			t.Errorf("%s: err = %v, want a refusal", name, err)
		}
	}
}

func TestExtractODT_ZipBombRefused(t *testing.T) {
	// 4 MiB of spaces deflate to a few KiB: the per-entry ratio refuses it,
	// as it does for every zip format.
	body := odtContent("", `<text:p>`+strings.Repeat(" ", 4<<20)+`</text:p>`)
	_, _, err := extractODT(t, odtZip(t, odtType, entry{name: "content.xml", data: []byte(body)}), Options{Name: "bomb.odt"})
	if !IsRefused(err) || !strings.Contains(err.Error(), "times its compressed size") {
		t.Fatalf("err = %v, want a ratio refusal", err)
	}
}

func TestExtractODT_ImageHrefNeverLeavesThePackage(t *testing.T) {
	// A frame that names a part outside Pictures/ by climbing, or an
	// absolute path, gets its alt text; nothing outside the package is read.
	body := `<text:p><draw:frame><draw:image xlink:href="../../etc/passwd"/><svg:title>a</svg:title></draw:frame>` +
		`<draw:frame><draw:image xlink:href="/etc/passwd"/><svg:title>b</svg:title></draw:frame>` +
		`<draw:frame><draw:image xlink:href="file:///etc/passwd"/><svg:title>c</svg:title></draw:frame></text:p>`
	data := odtZip(t, odtType, entry{name: "content.xml", data: []byte(odtContent("", body))})
	s, rep := run(t, ODT{}, data, Options{Name: "x.odt", Images: true})
	if len(s.images) != 0 || rep.Images != 0 {
		t.Errorf("images = %+v", s.images)
	}
	for _, want := range []string{"[image: a]", "[image: b]", "[image: c]"} {
		if !strings.Contains(s.md.String(), want) {
			t.Errorf("missing %q in %q", want, s.md.String())
		}
	}
}

func TestParseXMLMixed_KeepsTextInDocumentOrder(t *testing.T) {
	data := []byte(`<p>Hello <span>big</span> world<s c="3"/><![CDATA[e]]><![CDATA[nd]]></p>`)
	root, err := parseXMLMixed(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, k := range root.child("p").kids {
		if k.name == textNode {
			got = append(got, "text:"+k.text)
		} else {
			got = append(got, k.name)
		}
	}
	want := "text:Hello |span|text: world|s|text:end"
	if g := strings.Join(got, "|"); g != want {
		t.Errorf("kids = %q, want %q", g, want)
	}
	// The plain parse is unchanged: text joined, children apart.
	plain, err := parseXML(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	if p := plain.child("p"); p.text != "Hello  worldend" || len(p.kids) != 2 {
		t.Errorf("plain p = %q with %d kids", p.text, len(p.kids))
	}
}

func TestParseXMLMixed_TextRunsCountTowardTheElementCap(t *testing.T) {
	// Text between empty elements doubles the nodes a part holds; the cap
	// counts both, so a mixed parse is bounded as the plain one is.
	var b strings.Builder
	b.WriteString("<p>")
	for range maxXMLNodes/2 + 10 {
		b.WriteString("a<s/>")
	}
	b.WriteString("</p>")
	if _, err := parseXMLMixed(context.Background(), []byte(b.String())); !IsRefused(err) {
		t.Errorf("err = %v, want an element-cap refusal", err)
	}
}
