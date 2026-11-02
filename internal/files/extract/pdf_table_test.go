package extract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/files/extract/pdftest"
)

// grid draws a table's borders the way Chrome and Word do: a thin filled
// box for every cell edge. Columns start at x with the widths given; rows
// go down from top with the heights given. open names the vertical edges
// left out, as {row, edge}: cells merged across columns.
func grid(x, top float64, widths, heights []float64, open map[[2]int]bool) string {
	return gridSpans(x, top, widths, heights, open, nil)
}

// gridSpans is grid that also leaves out the horizontal edges openH names,
// as {edge, column}: cells merged down rows. Edge 0 is the top border.
func gridSpans(x, top float64, widths, heights []float64, open, openH map[[2]int]bool) string {
	var b strings.Builder
	const w = 0.5
	y := top
	for r, h := range heights {
		cx := x
		for c := 0; c <= len(widths); c++ {
			if !open[[2]int{r, c}] {
				b.WriteString(pdftest.Rect(cx-w/2, y-h, w, h))
			}
			if c < len(widths) {
				if !openH[[2]int{r, c}] {
					b.WriteString(pdftest.Rect(cx, y-w/2, widths[c], w))
				}
				if !openH[[2]int{r + 1, c}] {
					b.WriteString(pdftest.Rect(cx, y-h-w/2, widths[c], w))
				}
				cx += widths[c]
			}
		}
		y -= h
	}
	return b.String()
}

// passportPage is a decision passport's options table: four columns, a
// cell that wraps onto a second line, and a cell merged across the last
// two columns, with text above and below it.
func passportPage(withBorders bool) pdftest.Page {
	p := pdftest.Page{Lines: []pdftest.Line{
		{Text: "Паспорт рішення", X: 40, Y: 780, Size: 10},
		// The header row: top 760, 20 high.
		{Text: "Варіант", X: 43, Y: 746, Size: 10},
		{Text: "Суть", X: 163, Y: 746, Size: 10},
		{Text: "Переваги", X: 303, Y: 746, Size: 10},
		{Text: "Ризики", X: 443, Y: 746, Size: 10},
		// Row 1: top 740, 40 high, its cells wrapping onto two lines.
		{Text: "1. Премія лише за", X: 43, Y: 726, Size: 10},
		{Text: "екстрарезультат", X: 43, Y: 714, Size: 10},
		{Text: "Оклад оплачує", X: 163, Y: 726, Size: 10},
		{Text: "Проста логіка;", X: 303, Y: 726, Size: 10},
		{Text: "чітко відділяє", X: 303, Y: 714, Size: 10},
		{Text: "Не вирішує", X: 443, Y: 726, Size: 10},
		{Text: "проблеми", X: 443, Y: 714, Size: 10},
		// Row 2: top 700, 40 high; its last two cells are one, whose
		// second line stands under the last column.
		{Text: "2. Змішана", X: 43, Y: 686, Size: 10},
		{Text: "Частина премії", X: 163, Y: 686, Size: 10},
		{Text: "Передбачуваність і контроль", X: 303, Y: 686, Size: 10},
		{Text: "для обох", X: 463, Y: 674, Size: 10},
		{Text: "Кінець розділу.", X: 40, Y: 630, Size: 10},
	}}
	if withBorders {
		p.Paths = grid(40, 760, []float64{120, 140, 140, 140}, []float64{20, 40, 40}, map[[2]int]bool{{2, 3}: true})
	}
	return p
}

const passportTable = "| Варіант | Суть | Переваги | Ризики |\n" +
	"| --- | --- | --- | --- |\n" +
	"| 1. Премія лише за екстрарезультат | Оклад оплачує | Проста логіка; чітко відділяє | Не вирішує проблеми |\n" +
	"| 2. Змішана | Частина премії | Передбачуваність і контроль для обох |   |"

func TestExtractPDF_RuledTableBecomesMarkdownTable(t *testing.T) {
	data := pdftest.Doc{Pages: []pdftest.Page{passportPage(true)}}.Bytes()
	s, rep, err := extractPDF(t, data, Options{Name: "passport.pdf"})
	if err != nil {
		t.Fatal(err)
	}
	md := s.md.String()
	want := "## Page 1\n\nПаспорт рішення\n\n" + passportTable + "\n\nКінець розділу."
	if !strings.Contains(md, want) {
		t.Errorf("want\n%s\nin\n%s", want, md)
	}
	if rep.Tables != 1 || !hasNote(rep, "1 table was recovered from the borders") {
		t.Errorf("report = %+v", rep)
	}
}

// A cell merged down two rows holds the text of both: the row below keeps
// its column, empty.
func TestExtractPDF_CellMergedDownRowsIsOneCell(t *testing.T) {
	data := pdftest.Doc{Pages: []pdftest.Page{{
		Paths: gridSpans(40, 760, []float64{120, 120, 120}, []float64{20, 20, 20}, nil, map[[2]int]bool{{2, 0}: true}),
		Lines: []pdftest.Line{
			{Text: "Етап", X: 43, Y: 746, Size: 10}, {Text: "Строк", X: 163, Y: 746, Size: 10}, {Text: "Відповідальний", X: 283, Y: 746, Size: 10},
			{Text: "Пілот", X: 43, Y: 726, Size: 10}, {Text: "Січень", X: 163, Y: 726, Size: 10}, {Text: "HR", X: 283, Y: 726, Size: 10},
			{Text: "у двох філіях", X: 43, Y: 706, Size: 10}, {Text: "Лютий", X: 163, Y: 706, Size: 10}, {Text: "Фінанси", X: 283, Y: 706, Size: 10},
		},
	}}}.Bytes()
	s, _, err := extractPDF(t, data, Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := "| Етап | Строк | Відповідальний |\n| --- | --- | --- |\n| Пілот у двох філіях | Січень | HR |\n|   | Лютий | Фінанси |"
	if !strings.Contains(s.md.String(), want) {
		t.Errorf("want\n%s\nin\n%s", want, s.md.String())
	}
}

// The borders change where the text goes, not how much of it a page has:
// the yield that chooses pages for OCR is the same with or without them.
func TestExtractPDF_RuledTableKeepsThePageYield(t *testing.T) {
	_, ruled, err := extractPDF(t, pdftest.Doc{Pages: []pdftest.Page{passportPage(true)}}.Bytes(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, plain, err := extractPDF(t, pdftest.Doc{Pages: []pdftest.Page{passportPage(false)}}.Bytes(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ruled.PageYields) != 1 || ruled.PageYields[0] != plain.PageYields[0] {
		t.Errorf("yields: ruled %+v, plain %+v", ruled.PageYields, plain.PageYields)
	}
	if plain.Tables != 0 {
		t.Errorf("a page with no borders has no tables: %+v", plain)
	}
}

// Borders drawn as stroked lines, each running the whole table, and an
// outline stroked as one rectangle.
func TestExtractPDF_StrokedBordersMakeATable(t *testing.T) {
	paths := "40 700 360 60 re S\n" +
		pdftest.Stroke(160, 700, 160, 760) + pdftest.Stroke(280, 700, 280, 760) +
		pdftest.Stroke(40, 740, 400, 740) + pdftest.Stroke(40, 720, 400, 720)
	data := pdftest.Doc{Pages: []pdftest.Page{{Paths: paths, Lines: []pdftest.Line{
		{Text: "Ризик", X: 43, Y: 746, Size: 10},
		{Text: "Ймовірність", X: 163, Y: 746, Size: 10},
		{Text: "Вплив", X: 283, Y: 746, Size: 10},
		{Text: "Відтік кадрів", X: 43, Y: 726, Size: 10},
		{Text: "Середня", X: 163, Y: 726, Size: 10},
		{Text: "Високий", X: 283, Y: 726, Size: 10},
		{Text: "Затримка", X: 43, Y: 706, Size: 10},
		{Text: "Низька", X: 163, Y: 706, Size: 10},
		{Text: "Помірний", X: 283, Y: 706, Size: 10},
	}}}}.Bytes()
	s, rep, err := extractPDF(t, data, Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := "| Ризик | Ймовірність | Вплив |\n| --- | --- | --- |\n| Відтік кадрів | Середня | Високий |\n| Затримка | Низька | Помірний |"
	if !strings.Contains(s.md.String(), want) || rep.Tables != 1 {
		t.Errorf("want\n%s\nin\n%s\nreport %+v", want, s.md.String(), rep)
	}
}

// A producer that writes a whole row as one TJ array, moving between the
// cells by adjustments: each cell's text still lands in its cell.
func TestExtractPDF_RowInOneTJArraySplitsIntoCells(t *testing.T) {
	data := pdftest.Doc{Pages: []pdftest.Page{{
		Paths: grid(40, 760, []float64{140, 140, 140}, []float64{20, 20}, nil),
		Lines: []pdftest.Line{
			{Text: "Колонка", X: 43, Y: 746, Size: 10},
			{Text: "Друга", X: 183, Y: 746, Size: 10},
			{Text: "Третя", X: 323, Y: 746, Size: 10},
			// Each word is 24 pt wide; 116 pt more reaches the next cell.
			{Text: "Альф\tБета\tГамм", X: 43, Y: 726, Size: 10, Jump: 11600},
		},
	}}}.Bytes()
	s, _, err := extractPDF(t, data, Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := "| Колонка | Друга | Третя |\n| --- | --- | --- |\n| Альф | Бета | Гамм |"
	if !strings.Contains(s.md.String(), want) {
		t.Errorf("want\n%s\nin\n%s", want, s.md.String())
	}
}

// Two tables and the text around them come out in the order they stand on
// the page.
func TestExtractPDF_TablesAndTextKeepTheirOrder(t *testing.T) {
	data := pdftest.Doc{Pages: []pdftest.Page{{
		Paths: grid(40, 760, []float64{100, 100}, []float64{20, 20}, nil) + grid(40, 640, []float64{100, 100}, []float64{20, 20}, nil),
		Lines: []pdftest.Line{
			{Text: "Вступ", X: 40, Y: 780, Size: 10},
			{Text: "А1", X: 43, Y: 746, Size: 10}, {Text: "А2", X: 143, Y: 746, Size: 10},
			{Text: "А3", X: 43, Y: 726, Size: 10}, {Text: "А4", X: 143, Y: 726, Size: 10},
			{Text: "Між таблицями", X: 40, Y: 680, Size: 10},
			{Text: "Б1", X: 43, Y: 626, Size: 10}, {Text: "Б2", X: 143, Y: 626, Size: 10},
			{Text: "Б3", X: 43, Y: 606, Size: 10}, {Text: "Б4", X: 143, Y: 606, Size: 10},
			{Text: "Висновок", X: 40, Y: 560, Size: 10},
		},
	}}}.Bytes()
	s, rep, err := extractPDF(t, data, Options{})
	if err != nil {
		t.Fatal(err)
	}
	md := s.md.String()
	order := []string{"Вступ", "| А1 | А2 |", "| А3 | А4 |", "Між таблицями", "| Б1 | Б2 |", "| Б3 | Б4 |", "Висновок"}
	last := -1
	for _, w := range order {
		i := strings.Index(md, w)
		if i <= last {
			t.Fatalf("%q missing or out of order in\n%s", w, md)
		}
		last = i
	}
	if rep.Tables != 2 || !hasNote(rep, "2 tables were recovered") {
		t.Errorf("report = %+v", rep)
	}
}

// Lines that are not a table's grid leave the text exactly as it reads
// without them: a box around a paragraph, an underline, a grid with text in
// only one cell, and an empty grid (a chart's).
func TestExtractPDF_FramesAndUnderlinesAreNotTables(t *testing.T) {
	lines := []pdftest.Line{
		{Text: "Рамка навколо абзацу", X: 50, Y: 760, Size: 10},
		{Text: "друга строка абзацу", X: 50, Y: 748, Size: 10},
		{Text: "Підкреслений текст", X: 50, Y: 700, Size: 10},
		{Text: "Одна клітинка", X: 53, Y: 646, Size: 10},
	}
	paths := "45 740 300 35 re S\n" + // the box
		pdftest.Rect(50, 697, 110, 0.5) + // the underline
		grid(50, 660, []float64{100, 100}, []float64{20, 20}, nil) + // text in one cell
		grid(300, 600, []float64{50, 50, 50}, []float64{30, 30, 30}, nil) // a chart's grid
	framed, rep, err := extractPDF(t, pdftest.Doc{Pages: []pdftest.Page{{Lines: lines, Paths: paths}}}.Bytes(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	plain, _, err := extractPDF(t, pdftest.Doc{Pages: []pdftest.Page{{Lines: lines}}}.Bytes(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if framed.md.String() != plain.md.String() || rep.Tables != 0 {
		t.Errorf("lines changed the text (tables %d):\n%s\n--- without them ---\n%s", rep.Tables, framed.md.String(), plain.md.String())
	}
}

// Text turned sideways inside a grid is not put into cells read for
// upright text: it stays as it was.
func TestExtractPDF_SidewaysTextInAGridStaysText(t *testing.T) {
	page := func(paths string) pdftest.Page {
		return pdftest.Page{Paths: paths, Lines: []pdftest.Line{
			{Text: "Вгору", X: 60, Y: 702, Size: 10, Rotate: true},
			{Text: "Також", X: 160, Y: 702, Size: 10, Rotate: true},
			{Text: "Ще", X: 60, Y: 652, Size: 10, Rotate: true},
		}}
	}
	ruled, rep, err := extractPDF(t, pdftest.Doc{Pages: []pdftest.Page{page(grid(40, 760, []float64{100, 100}, []float64{50, 50}, nil))}}.Bytes(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	plain, _, err := extractPDF(t, pdftest.Doc{Pages: []pdftest.Page{page("")}}.Bytes(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Tables != 0 || ruled.md.String() != plain.md.String() {
		t.Errorf("sideways text changed (tables %d):\n%s\n--- without borders ---\n%s", rep.Tables, ruled.md.String(), plain.md.String())
	}
}

// A page drawing more lines than the cap is not searched for tables, and
// the report says which page.
func TestExtractPDF_TooManyLinesSkipsTheTableSearch(t *testing.T) {
	page := passportPage(true)
	page.Paths = strings.Repeat(pdftest.Rect(10, 10, 0.5, 5), maxPDFPageRules) + page.Paths
	s, rep, err := extractPDF(t, pdftest.Doc{Pages: []pdftest.Page{page}}.Bytes(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Tables != 0 || strings.Contains(s.md.String(), "| --- |") {
		t.Errorf("a table was read on a page over the cap:\n%s", s.md.String())
	}
	if !hasNote(rep, "tables were not looked for on page 1") {
		t.Errorf("notes = %q", rep.Notes)
	}
	if !strings.Contains(s.md.String(), "Варіант Суть Переваги Ризики") {
		t.Errorf("the text is not read as before:\n%s", s.md.String())
	}
}

// A real producer: Chrome's PDF printer draws each cell border as a thin
// filled box under a flipped coordinate system, and positions each glyph
// separately. See testdata/README.md.
func TestExtractPDF_ChromeTableBecomesMarkdownTable(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "chrome-table.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	s, rep, err := extractPDF(t, data, Options{Name: "chrome-table.pdf"})
	if err != nil {
		t.Fatal(err)
	}
	md := s.md.String()
	golden(t, "chrome-table.pdf.md", md)
	for _, want := range []string{
		"| Варіант | Суть | Переваги | Ризики |",
		"| 1. Премія лише за екстрарезультат | Оклад оплачує всю нормальну якісну роботу. Додаткова премія лише за понадплан. | Проста логіка; чітко відділяє оклад від премії; стримує перетворення базової якості | Не вирішує поточні проблеми підтримці |",
		// The justified paragraph after the table stays prose.
		"Справедливий опис тексту з вирівнюванням по ширині",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in:\n%s", want, md)
		}
	}
	if rep.Tables != 1 {
		t.Errorf("report = %+v", rep)
	}
}

// A content stream of path operators that are never painted holds no more
// path points than the page budget: painting frees a path, but the count is
// the page's, so a stream of moves cannot grow memory without bound.
func TestPDFPath_PagePointBudgetBoundsWhatIsHeld(t *testing.T) {
	p := &pdfPage{}
	var pa pdfPath
	for range maxPDFPagePathPoints + 1000 {
		pa.moveTo(p, pdfPoint{})
	}
	if len(pa.subs) != maxPDFPagePathPoints || !p.rulesOver {
		t.Errorf("held %d subpaths (budget %d), over = %v", len(pa.subs), maxPDFPagePathPoints, p.rulesOver)
	}
	pa.reset()
	pa.rect(p, identity(), 0, 0, 1, 1)
	pa.lineTo(p, pdfPoint{})
	if len(pa.subs) != 0 {
		t.Errorf("a path past the page budget still grew: %d subpaths", len(pa.subs))
	}
}

// The same through the extractor: a page that builds more path points than
// the budget, even in paths it never paints, is read as text and noted.
func TestExtractPDF_ManyPathMovesSkipTheTableSearch(t *testing.T) {
	page := passportPage(true)
	page.Paths = strings.Repeat("0 0 m ", maxPDFPagePathPoints) + "n\n" + page.Paths
	s, rep, err := extractPDF(t, pdftest.Doc{Pages: []pdftest.Page{page}}.Bytes(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Tables != 0 || !hasNote(rep, "tables were not looked for on page 1") {
		t.Errorf("tables %d, notes %q:\n%s", rep.Tables, rep.Notes, s.md.String())
	}
}

// More grids on a page than the search takes: no table is read, and the
// page is noted.
func TestExtractPDF_ManyGridsSkipTheTableSearch(t *testing.T) {
	var paths strings.Builder
	var lines []pdftest.Line
	for i := range maxPDFPageGrids + 1 {
		x, top := 20+float64(i%25)*60, 20+float64(i/25+1)*40
		paths.WriteString(grid(x, top, []float64{20, 20}, []float64{10, 10}, nil))
		lines = append(lines,
			pdftest.Line{Text: "ab", X: x + 2, Y: top - 7, Size: 4},
			pdftest.Line{Text: "cd", X: x + 22, Y: top - 17, Size: 4})
	}
	s, rep, err := extractPDF(t, pdftest.Doc{Pages: []pdftest.Page{{Paths: paths.String(), Lines: lines}}}.Bytes(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Tables != 0 || strings.Contains(s.md.String(), "| --- |") || !hasNote(rep, "tables were not looked for on page 1") {
		t.Errorf("tables %d, notes %q", rep.Tables, rep.Notes)
	}
}

// Reading the text into a grid's cells is charged to the same work budget
// as joining its lines: when it runs out, no table is read.
func TestExtractPDF_GridWorkBudgetCoversReadingCells(t *testing.T) {
	data := pdftest.Doc{Pages: []pdftest.Page{passportPage(true)}}.Bytes()
	// Joining the passport's lines into its grid costs 71 units and reading
	// its runs and cells brings the page to 115: 90 is enough for the first
	// and not the second.
	_, rep, err := extractPDF(t, data, Options{pdfGridWork: 90})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Tables != 0 || !hasNote(rep, "tables were not looked for on page 1") {
		t.Errorf("tables %d, notes %q", rep.Tables, rep.Notes)
	}
	if _, rep, _ := extractPDF(t, data, Options{}); rep.Tables != 1 {
		t.Errorf("the default budget reads the table: %+v", rep)
	}
}
