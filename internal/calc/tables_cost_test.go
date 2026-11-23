package calc

import (
	"context"
	"encoding/json"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A row whose cells are each far larger than the output cap must cost about
// the cap to render in every format: each cell is cut to the room left
// before it is escaped, quoted or joined, so the cells past the first are
// never touched.
func TestTables_RenderingAHugeRowAllocatesAboutTheCap(t *testing.T) {
	cell := strings.Repeat(`a|"b`, 250_000) // 1 MB; escaping and quoting double it
	names := make([]string, 20)
	cols := make([][]val, 20)
	for j := range cols {
		names[j] = "c" + strconv.Itoa(j)
		cols[j] = []val{cell}
	}
	tbl := &tableVal{names: names, cols: cols} // rendering reads no index
	for _, format := range []string{FormatJSON, FormatMarkdown, FormatCSV} {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		w := &capWriter{limit: MaxOutputBytes}
		renderResult(w, tbl, DefaultDigits, format)
		runtime.ReadMemStats(&after)
		if !w.over || w.b.Len() > MaxOutputBytes {
			t.Errorf("%s: over %v, %d bytes", format, w.over, w.b.Len())
		}
		// 20 cells of 1 MB would be 20-40 MB rendered whole.
		if got := after.TotalAlloc - before.TotalAlloc; got > 2<<20 {
			t.Errorf("%s: rendering allocated %d bytes, want about the %d-byte cap", format, got, MaxOutputBytes)
		}
	}
}

// A table with no rows still counts its columns against the cell limit.
func TestTables_ATableWithNoRowsCountsItsColumns(t *testing.T) {
	header := make([]string, MaxTableCells+1)
	for j := range header {
		header[j] = "c" + strconv.Itoa(j)
	}
	b, _ := json.Marshal(strings.Join(header, ","))
	_, err := evalWithin(context.Background(), Request{Expr: "csv(s)", Vars: map[string]json.RawMessage{"s": b}}, time.Minute)
	if err == nil || err.Code != string(codeListTooLong) {
		t.Fatalf("a header of %d columns and no rows: %v, want list_too_long", MaxTableCells+1, err)
	}
	b, _ = json.Marshal(strings.Join(header[:MaxTableCells], ","))
	if _, err := evalWithin(context.Background(), Request{Expr: "len(csv(s))", Vars: map[string]json.RawMessage{"s": b}}, time.Minute); err != nil {
		t.Fatalf("a header of %d columns: %v", MaxTableCells, err)
	}
}

// Wide tables: naming many columns, and joining two tables whose columns
// all clash, are linear in the columns. Done by scanning the names per
// column they took billions of comparisons, uncharged, for 50,000 columns.
// The bound is a wall time with a wide margin both ways.
func TestTables_WideTablesAreLinearInTheirColumns(t *testing.T) {
	const n = 49_999
	header := make([]string, n)
	for j := range header {
		header[j] = "c" + strconv.Itoa(j)
	}
	text, _ := json.Marshal("k," + strings.Join(header, ",") + "\n1," + strings.Repeat("2,", n-1) + "2")
	names, _ := json.Marshal(header)
	vars := map[string]json.RawMessage{"s": text, "names": names}
	for _, expr := range []string{
		`t = csv(s); size(join(t, t, "k"))`,
		`t = csv(s); [size(select(t, names)), size(select(t, reverse(names)))]`,
	} {
		start := time.Now()
		r, err := evalWithin(context.Background(), Request{Expr: expr, Vars: vars}, time.Minute)
		if err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		// Linear work takes about 0.3 s under -race; the quadratic form
		// took 4 to 5 s without it.
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("%s took %v (%s)", expr, d, r.Text)
		}
	}
}
