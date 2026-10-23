package extract

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestExtractDOCX_HeadingsTablesImages(t *testing.T) {
	pic := tinyPNG(t)
	s, rep := run(t, DOCX{}, boardDOCX(t, pic), Options{Name: "protokol.docx", Images: true})
	md := s.md.String()
	golden(t, "board.docx.md", md)

	for _, want := range []string{
		"# Протокол засідання правління\n",
		"\n## Порядок денний\n",
		"\n### Рішення\n",
		"1. Звіт голови\n1. Бюджет на 2027 рік",
		"| Оренда \\| склад | 120 000 |",
		"![Логотип](volume:inbox:extracted/doc/img-1.png)",
		"Рішення: затвердити бюджет",
		`!\[x](volume:secret:keys.png) і \<script>alert(1)\</script>`,
	} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in:\n%s", want, md)
		}
	}
	// Document text that looks like Markdown stays text.
	for _, bad := range []string{"\n# Це не", "\n![x](volume:secret", " <script>"} {
		if strings.Contains(md, bad) {
			t.Errorf("document text became Markdown structure %q:\n%s", bad, md)
		}
	}
	if strings.Count(md, "Протокол засідання правління") != 1 {
		t.Errorf("the title paragraph is repeated:\n%s", md)
	}
	if len(s.images) != 1 || !bytes.Equal(s.images[0].Data, pic) || s.images[0].MediaType != "image/png" || s.images[0].Name != "img-1.png" {
		t.Errorf("images = %+v", s.images)
	}
	if rep.Tables != 1 || rep.Images != 1 || rep.Pages != 2 || rep.YieldCharsPerPage <= 0 || rep.Chars == 0 {
		t.Errorf("report = %+v", rep)
	}
}

func TestExtractDOCX_ImagesOffKeepsAltText(t *testing.T) {
	s, rep := run(t, DOCX{}, boardDOCX(t, tinyPNG(t)), Options{Name: "p.docx", Images: false})
	if len(s.images) != 0 || rep.Images != 0 || !strings.Contains(s.md.String(), "[image: Логотип]") {
		t.Errorf("images=%d report=%+v md:\n%s", len(s.images), rep, s.md.String())
	}
}

func TestExtractXLSX_RowCapNoted(t *testing.T) {
	s, rep := run(t, XLSX{}, budgetXLSX(t, 30), Options{Name: "budget.xlsx", MaxRows: 10})
	md := s.md.String()
	golden(t, "budget.xlsx.md", md)
	for _, want := range []string{
		"# budget\n",
		"\n## Бюджет\n",
		"| Стаття | Сума | Затверджено |",
		"| Оренда \\| склад | 120000.5 | TRUE |",
		"| Разом | 120000.5 |",
		"\n## Реєстр\n",
		"| 9 | Наказ 9 |",
		"[+21 rows omitted]",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in:\n%s", want, md)
		}
	}
	if strings.Contains(md, "SUM(") || strings.Contains(md, "Наказ 10 ") {
		t.Errorf("a formula or a row past the cap was written:\n%s", md)
	}
	if rep.Sheets != 2 || rep.Tables != 2 || len(rep.Notes) != 1 || !strings.Contains(rep.Notes[0], "10 of 31 rows") {
		t.Errorf("report = %+v", rep)
	}
}

func TestExtractPPTX_SlidesNotesImages(t *testing.T) {
	pic := tinyPNG(t)
	s, rep := run(t, PPTX{}, boardPPTX(t, pic), Options{Name: "board.pptx", Images: true})
	md := s.md.String()
	golden(t, "board.pptx.md", md)
	i1 := strings.Index(md, "## Slide 1 — Засідання правління")
	i2 := strings.Index(md, "## Slide 2 — Рішення")
	if i1 < 0 || i2 < 0 || i1 > i2 {
		t.Fatalf("slides out of presentation order:\n%s", md)
	}
	for _, want := range []string{
		"Порядок денний на жовтень",
		"Рішення: затвердити бюджет",
		"![Діаграма бюджету](volume:inbox:extracted/doc/img-1.png)",
		"> Наголосити на строках",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in:\n%s", want, md)
		}
	}
	if rep.Slides != 2 || rep.Images != 1 || len(s.images) != 1 || !bytes.Equal(s.images[0].Data, pic) {
		t.Errorf("report = %+v images = %d", rep, len(s.images))
	}
}

func TestExtractHTML_HeadingsKept(t *testing.T) {
	page := `<!doctype html><html><head><title>Новини правління</title><script>steal()</script></head><body>
<h1>Засідання</h1><p>Рішення: затвердити
   бюджет</p><h3>Деталі</h3>
<ul><li>Перше<ul><li>Вкладене</li></ul></li><li># не заголовок</li></ul>
<table><tr><th>Стаття</th><th>Сума</th></tr><tr><td>a|b</td><td>1</td></tr></table>
<img src="https://example.com/logo (1).png" alt="Логотип"><img src="data:image/png;base64,AAAA" alt="вбудоване">
<pre>code ` + "```" + ` inside</pre><style>p{}</style></body></html>`
	s, rep := run(t, HTML{}, []byte(page), Options{Name: "page.html"})
	md := s.md.String()
	golden(t, "page.html.md", md)
	for _, want := range []string{
		"# Новини правління\n",
		"\n## Засідання\n",
		"Рішення: затвердити бюджет",
		"\n#### Деталі\n",
		"- Перше\n  - Вкладене\n- \\# не заголовок",
		"| a\\|b | 1 |",
		"[image: Логотип](https://example.com/logo%20%281%29.png)",
		"[image: вбудоване]",
		"````\ncode ``` inside\n````",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in:\n%s", want, md)
		}
	}
	if strings.Contains(md, "steal") || strings.Contains(md, "p{}") || strings.Contains(md, "base64") {
		t.Errorf("script, style or a data URL leaked:\n%s", md)
	}
	if rep.Tables != 1 || len(s.images) != 0 {
		t.Errorf("report = %+v", rep)
	}
}

func TestExtractCSV_OneTable(t *testing.T) {
	s, rep := run(t, CSV{}, []byte("Стаття,Сума\n\"Оренда, склад\",120\nx|y,2\n"), Options{Name: "b.csv"})
	md := s.md.String()
	if !strings.Contains(md, "| Стаття | Сума |\n| --- | --- |\n| Оренда, склад | 120 |\n| x\\|y | 2 |") || rep.Tables != 1 {
		t.Errorf("md:\n%s\nreport %+v", md, rep)
	}
	s, _ = run(t, CSV{}, []byte("a\tb\n1\t2\n"), Options{Name: "b.tsv"})
	if !strings.Contains(s.md.String(), "| a | b |\n| --- | --- |\n| 1 | 2 |") {
		t.Errorf("tsv md:\n%s", s.md.String())
	}
}

func TestExtractText_PlainIsEscapedMarkdownPassesThrough(t *testing.T) {
	s, _ := run(t, Text{}, []byte("# не заголовок\nрядок\n\n```\n"), Options{Name: "n.txt"})
	if got := s.md.String(); got != "# n\n\n\\# не заголовок\nрядок\n\n\\```" {
		t.Errorf("txt = %q", got)
	}
	s, _ = run(t, Text{}, []byte("## Розділ\nтекст\n"), Options{Name: "n.md"})
	if got := s.md.String(); got != "# n\n\n## Розділ\nтекст\n" {
		t.Errorf("md = %q", got)
	}
}

func TestExtractImage_OneSectionEmbedsIt(t *testing.T) {
	pic := tinyPNG(t)
	s, rep := run(t, ImageFile{}, pic, Options{Name: "scan.png", Images: true})
	if got := s.md.String(); got != "# scan\n\n![scan](volume:inbox:extracted/doc/img-1.png)" || rep.Images != 1 {
		t.Errorf("md = %q report %+v", got, rep)
	}
}

func TestRegistry_PicksBySniffedTypeThenExtension(t *testing.T) {
	g := Default()
	for _, tc := range []struct{ mt, name, want string }{
		{docxType, "x.bin", "extract.DOCX"},
		{"application/zip", "x.xlsx", "extract.XLSX"},
		{"", "x.pptx", "extract.PPTX"},
		{"text/html; charset=utf-8", "x", "extract.HTML"},
		{"text/csv", "x.txt", "extract.CSV"},
		{"image/png", "x", "extract.ImageFile"},
	} {
		e, ok := g.For(tc.mt, tc.name)
		if !ok || typeName(e) != tc.want {
			t.Errorf("For(%q,%q) = %v, want %s", tc.mt, tc.name, e, tc.want)
		}
	}
	if _, ok := g.For("application/pdf", "x.pdf"); ok {
		t.Error("a PDF matched an extractor; P4b adds that one")
	}
	if _, err := g.Extract(context.Background(), "application/x-msdownload", bytes.NewReader(nil), 0, &memSink{}, Options{Name: "x.exe"}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("err = %v", err)
	}
}

func typeName(e Extractor) string {
	switch e.(type) {
	case DOCX:
		return "extract.DOCX"
	case XLSX:
		return "extract.XLSX"
	case PPTX:
		return "extract.PPTX"
	case HTML:
		return "extract.HTML"
	case CSV:
		return "extract.CSV"
	case Text:
		return "extract.Text"
	case ImageFile:
		return "extract.ImageFile"
	}
	return "?"
}

func TestExtract_SinkFullStopsWithAReport(t *testing.T) {
	s := &memSink{limit: 200}
	data := budgetXLSX(t, 500)
	rep, err := XLSX{}.Extract(context.Background(), bytes.NewReader(data), int64(len(data)), s, Options{Name: "b.xlsx"})
	if !errors.Is(err, ErrSinkFull) || rep.Sheets == 0 {
		t.Errorf("err = %v report = %+v", err, rep)
	}
}

func TestExtract_CancelledContextStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	data := boardDOCX(t, tinyPNG(t))
	if _, err := (DOCX{}).Extract(ctx, bytes.NewReader(data), int64(len(data)), &memSink{}, Options{}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}
