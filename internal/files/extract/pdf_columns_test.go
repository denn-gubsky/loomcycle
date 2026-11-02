package extract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/files/extract/pdftest"
)

// The test font draws every glyph 6 pt wide at 10 pt, so a line's text
// ends 6 pt per character after its x.

// A real producer: the passport's options table printed by Chrome with no
// cell borders. Each line of the table is a row; a cell that wraps goes on
// in the row below, and the justified paragraph after it stays prose. See
// testdata/README.md.
func TestExtractPDF_ChromeBorderlessTableBecomesMarkdownTable(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "chrome-borderless.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	s, rep, err := extractPDF(t, data, Options{Name: "chrome-borderless.pdf"})
	if err != nil {
		t.Fatal(err)
	}
	md := s.md.String()
	golden(t, "chrome-borderless.pdf.md", md)
	for _, want := range []string{
		"Паспорт рішення. Розділ 3. Варіанти.\n\n| Варіант | Суть | Переваги | Ризики |\n| --- | --- | --- | --- |\n",
		"| 1. Премія лише за | Оклад оплачує всю нормальну | Проста логіка; чітко відділяє оклад | Не вирішує |\n",
		"| екстрарезультат | якісну роботу. Додаткова премія | від премії; стримує перетворення | поточні проблеми |\n",
		// The first column's cell ended a line earlier: its place is kept.
		"|   | лише за понадплан. | базової якості | підтримці |\n",
		"| 2. Змішана модель | Частина премії гарантована. | Передбачуваність | Розмиває |\n|   |   |   | мотивацію |\n\n",
		// A justified line's wide word spaces do not part cells.
		"Справедливий опис тексту з вирівнюванням по ширині для перевірки того, що широкі пробіли між словами у\nзвичайному абзаці",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in:\n%s", want, md)
		}
	}
	if rep.Tables != 1 || !hasNote(rep, "1 table was recovered from text aligned in columns") || hasNote(rep, "borders") {
		t.Errorf("report = %+v", rep)
	}
}

// The yield that chooses pages for OCR counts the same characters whether
// the text is read as a table or not.
func TestExtractPDF_BorderlessTableKeepsThePageYield(t *testing.T) {
	page := passportPage(false)
	_, table, err := extractPDF(t, pdftest.Doc{Pages: []pdftest.Page{page}}.Bytes(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	// The same text, each line moved so no two lines share a column start.
	for i := range page.Lines {
		page.Lines[i].X += float64(i) * 3
	}
	_, prose, err := extractPDF(t, pdftest.Doc{Pages: []pdftest.Page{page}}.Bytes(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if table.Tables != 1 || prose.Tables != 0 || table.PageYields[0].Chars != prose.PageYields[0].Chars {
		t.Errorf("tables %d/%d, yields %+v / %+v", table.Tables, prose.Tables, table.PageYields, prose.PageYields)
	}
}

// A cell merged across two columns has no border to say so: its text runs
// over a column's start, which ends the table there. What follows is text.
func TestExtractPDF_BorderlessTableEndsAtACellOverTwoColumns(t *testing.T) {
	s, _, err := extractPDF(t, pdftest.Doc{Pages: []pdftest.Page{passportPage(false)}}.Bytes(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := "Паспорт рішення\n\n" +
		"| Варіант | Суть | Переваги | Ризики |\n| --- | --- | --- | --- |\n" +
		"| 1. Премія лише за | Оклад оплачує | Проста логіка; | Не вирішує |\n" +
		"| екстрарезультат |   | чітко відділяє | проблеми |\n\n" +
		"2\\. Змішана Частина премії Передбачуваність і контроль\nдля обох\n\nКінець розділу."
	if !strings.Contains(s.md.String(), want) {
		t.Errorf("want\n%s\nin\n%s", want, s.md.String())
	}
}

// A form of labels and values: two columns, a value that wraps.
func TestExtractPDF_LabelValueFormIsTwoColumns(t *testing.T) {
	data := pdftest.Doc{Pages: []pdftest.Page{{Lines: []pdftest.Line{
		{Text: "Ініціатор", X: 40, Y: 760, Size: 10}, {Text: "Департамент HR", X: 200, Y: 760, Size: 10},
		{Text: "Дата", X: 40, Y: 746, Size: 10}, {Text: "02.11.2026", X: 200, Y: 746, Size: 10},
		{Text: "Підстава", X: 40, Y: 732, Size: 10}, {Text: "Протокол Правління №111", X: 200, Y: 732, Size: 10},
		{Text: "та рішення №150", X: 200, Y: 718, Size: 10},
		{Text: "Сума", X: 40, Y: 704, Size: 10}, {Text: "120 000 грн", X: 200, Y: 704, Size: 10},
	}}}}.Bytes()
	s, rep, err := extractPDF(t, data, Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := "| Ініціатор | Департамент HR |\n| --- | --- |\n| Дата | 02.11.2026 |\n" +
		"| Підстава | Протокол Правління №111 |\n|   | та рішення №150 |\n| Сума | 120 000 грн |"
	if !strings.Contains(s.md.String(), want) || rep.Tables != 1 {
		t.Errorf("want\n%s\nin\n%s\nreport %+v", want, s.md.String(), rep)
	}
}

// A numbered list whose text hangs a little right of its numbers: the gap
// after a number is under a column gap, so the list stays a list.
func TestExtractPDF_HangingIndentListIsNotATable(t *testing.T) {
	var lines []pdftest.Line
	y := 760.0
	for _, item := range [][2]string{{"1.", "Затвердити бюджет"}, {"2.", "Призначити відповідального"}, {"3.", "Звітувати щокварталу"}} {
		// "1." ends at 52: the text at 58 stands 6 pt off it.
		lines = append(lines, pdftest.Line{Text: item[0], X: 40, Y: y, Size: 10}, pdftest.Line{Text: item[1], X: 58, Y: y, Size: 10})
		lines = append(lines, pdftest.Line{Text: "з урахуванням зауважень", X: 58, Y: y - 12, Size: 10})
		y -= 24
	}
	s, rep, err := extractPDF(t, pdftest.Doc{Pages: []pdftest.Page{{Lines: lines}}}.Bytes(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Tables != 0 || strings.Contains(s.md.String(), "| --- |") || !strings.Contains(s.md.String(), "1\\. Затвердити бюджет") {
		t.Errorf("tables %d:\n%s", rep.Tables, s.md.String())
	}
}

// Wide gaps that do not line up from one line to the next are not columns.
func TestExtractPDF_WideGapsThatDoNotLineUpStayProse(t *testing.T) {
	var lines []pdftest.Line
	for i, xs := range [][]float64{{40, 100, 172}, {40, 124, 208}, {40, 88, 154}, {40, 112, 190}} {
		y := 760 - float64(i)*12
		for k, x := range xs {
			lines = append(lines, pdftest.Line{Text: []string{"слово", "інше", "третє"}[k], X: x, Y: y, Size: 10})
		}
	}
	s, rep, err := extractPDF(t, pdftest.Doc{Pages: []pdftest.Page{{Lines: lines}}}.Bytes(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Tables != 0 || strings.Contains(s.md.String(), "| --- |") {
		t.Errorf("tables %d:\n%s", rep.Tables, s.md.String())
	}
}

// Two columns of prose side by side are read as a two-column table: their
// lines line up like a table's, and nothing on the page says otherwise.
// Each line of each column stays whole in its cell, so the columns' text
// no longer runs together as it does read as plain lines.
func TestExtractPDF_ProseInTwoColumnsKeepsItsColumnsApart(t *testing.T) {
	var lines []pdftest.Line
	for i := range 5 {
		y := 760 - float64(i)*12
		lines = append(lines,
			pdftest.Line{Text: "ліва колонка рядок " + itoa(i+1), X: 40, Y: y, Size: 10},
			pdftest.Line{Text: "права колонка рядок " + itoa(i+1), X: 300, Y: y, Size: 10})
	}
	s, rep, err := extractPDF(t, pdftest.Doc{Pages: []pdftest.Page{{Lines: lines}}}.Bytes(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.md.String(), "| ліва колонка рядок 3 | права колонка рядок 3 |") || rep.Tables != 1 {
		t.Errorf("tables %d:\n%s", rep.Tables, s.md.String())
	}
}

// A table from borders and one from aligned columns on one page, with text
// between them, come out in page order and are reported apart.
func TestExtractPDF_RuledAndAlignedTablesOnOnePage(t *testing.T) {
	data := pdftest.Doc{Pages: []pdftest.Page{{
		Paths: grid(40, 760, []float64{100, 100}, []float64{20, 20}, nil),
		Lines: []pdftest.Line{
			{Text: "А1", X: 43, Y: 746, Size: 10}, {Text: "А2", X: 143, Y: 746, Size: 10},
			{Text: "А3", X: 43, Y: 726, Size: 10}, {Text: "А4", X: 143, Y: 726, Size: 10},
			{Text: "Між таблицями", X: 40, Y: 690, Size: 10},
			{Text: "Б1", X: 40, Y: 660, Size: 10}, {Text: "Б2", X: 140, Y: 660, Size: 10},
			{Text: "Б3", X: 40, Y: 648, Size: 10}, {Text: "Б4", X: 140, Y: 648, Size: 10},
			{Text: "Б5", X: 40, Y: 636, Size: 10}, {Text: "Б6", X: 140, Y: 636, Size: 10},
			{Text: "Висновок", X: 40, Y: 600, Size: 10},
		},
	}}}.Bytes()
	s, rep, err := extractPDF(t, data, Options{})
	if err != nil {
		t.Fatal(err)
	}
	md := s.md.String()
	last := -1
	for _, w := range []string{"| А1 | А2 |", "| А3 | А4 |", "Між таблицями", "| Б1 | Б2 |", "| Б5 | Б6 |", "Висновок"} {
		i := strings.Index(md, w)
		if i <= last {
			t.Fatalf("%q missing or out of order in\n%s", w, md)
		}
		last = i
	}
	if rep.Tables != 2 || !hasNote(rep, "1 table was recovered from the borders") || !hasNote(rep, "1 table was recovered from text aligned in columns") {
		t.Errorf("report = %+v", rep)
	}
}

// Past the page's work budget the text is read as text, and the report says
// where columns were not looked for.
func TestExtractPDF_ColumnSearchWorkBudgetFallsBackToText(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "chrome-borderless.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	s, rep, err := extractPDF(t, data, Options{pdfGridWork: 100})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Tables != 0 || strings.Contains(s.md.String(), "| --- |") || !strings.Contains(s.md.String(), "Варіант Суть Переваги Ризики") {
		t.Errorf("tables %d:\n%s", rep.Tables, s.md.String())
	}
	if !hasNote(rep, "text aligned in columns was not looked for on page 1") {
		t.Errorf("notes = %q", rep.Notes)
	}
}
