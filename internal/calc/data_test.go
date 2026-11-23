package calc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"golang.org/x/sync/semaphore"
)

func evalData(t *testing.T, expr string, data map[string]Data) (Response, *Error) {
	t.Helper()
	return Eval(context.Background(), Request{Expr: expr, Data: data})
}

func mustData(t *testing.T, expr string, data map[string]Data) Response {
	t.Helper()
	resp, err := evalData(t, expr, data)
	if err != nil {
		t.Fatalf("%s: %v", expr, err)
	}
	return resp
}

func csvData(s string) Data { return Data{Bytes: []byte(s), Format: FormatCSV} }

// Each read format becomes the value it describes; number cells stay exact.
func TestData_EachFormatLoadsAsItsTable(t *testing.T) {
	for name, tc := range map[string]struct {
		data Data
		expr string
		want string
	}{
		"csv":                {csvData("region,amount\nEU,120.10\nUS,300\n"), "sum(t.amount)", "420.1"},
		"csv with a BOM":     {csvData("\xEF\xBB\xBFsku,qty\na,2\n"), "t[1].sku", "a"},
		"tsv":                {Data{Bytes: []byte("a\tb\n1\t2\n3\t4\n"), Format: FormatTSV}, "sum(t.b)", "6"},
		"json table":         {Data{Bytes: []byte(`{"columns":["a"],"rows":[[1],[2.5]]}`), Format: FormatJSON}, "sum(t.a)", "3.5"},
		"json records":       {Data{Bytes: []byte(`[{"a":1,"b":"x"},{"a":2}]`), Format: FormatJSON}, "count(t.b)", "1"},
		"json list":          {Data{Bytes: []byte(`[0.1, 0.2]`), Format: FormatJSON}, "sum(t)", "0.3"},
		"json matrix":        {Data{Bytes: []byte(`[[1,2],[3,4]]`), Format: FormatJSON}, "det(t)", "-2"},
		"json scalar":        {Data{Bytes: []byte(`"19.99"`), Format: FormatJSON}, "t * 3", "59.97"},
		"markdown in prose":  {Data{Bytes: []byte("Intro text.\n\n| item | cost |\n|:--|--:|\n| a \\| b | 1.10 |\n| c | 2 |\n\nAfter.\n| x | y |\n|---|---|\n| 9 | 9 |\n"), Format: FormatMarkdown}, "sum(t.cost)", "3.1"},
		"markdown escape":    {Data{Bytes: []byte("| item | cost |\n| --- | --- |\n| a \\| b | 1 |\n"), Format: FormatMarkdown}, "t[1].item", "a | b"},
		"markdown short row": {Data{Bytes: []byte("| a | b |\n|---|---|\n| 1 |\n"), Format: FormatMarkdown}, "isempty(t[1].b)", "true"},
	} {
		resp := mustData(t, tc.expr, map[string]Data{"t": tc.data})
		if resp.Text != tc.want {
			t.Errorf("%s: %s = %q, want %q", name, tc.expr, resp.Text, tc.want)
		}
	}
	if resp := mustData(t, "sum(t.amount)", map[string]Data{"t": csvData("amount\n0.1\n0.2\n")}); !resp.Exact {
		t.Error("a CSV column of decimals summed inexactly")
	}
}

// Data read by reference is data: a cell that looks like an expression, a
// formula or a name stays a string and is never evaluated.
func TestData_CellsAreNeverEvaluated(t *testing.T) {
	data := map[string]Data{"t": csvData("c\n1+1\n=sum(1:9)\npi\nf(x) = x\n")}
	resp := mustData(t, "t.c", data)
	if resp.Text != `["1+1", "=sum(1:9)", "pi", "f(x) = x"]` {
		t.Errorf("t.c = %s, want the cells as strings", resp.Text)
	}
	md := map[string]Data{"t": {Bytes: []byte("| c |\n|---|\n| 2^10 |\n"), Format: FormatMarkdown}}
	if resp := mustData(t, "t[1].c", md); resp.Type != "string" || resp.Text != "2^10" {
		t.Errorf("markdown cell = %s %q, want the string", resp.Type, resp.Text)
	}
	js := map[string]Data{"t": {Bytes: []byte(`["e", "1/0"]`), Format: FormatJSON}}
	if resp := mustData(t, "t", js); resp.Text != `["e", "1/0"]` {
		t.Errorf("json strings = %s", resp.Text)
	}
}

// Unreadable data is refused with bad_data (or the shape code), naming the
// source, never as a var.
func TestData_UnreadableDataIsRefusedNamingTheSource(t *testing.T) {
	for name, tc := range map[string]struct {
		data Data
		code errCode
	}{
		"not utf-8":          {csvData("a\n\xff\n"), codeBadData},
		"bad json":           {Data{Bytes: []byte(`{"columns":`), Format: FormatJSON}, codeBadData},
		"json object":        {Data{Bytes: []byte(`{"a":1}`), Format: FormatJSON}, codeBadData},
		"json null":          {Data{Bytes: []byte(`null`), Format: FormatJSON}, codeBadData},
		"no markdown table":  {Data{Bytes: []byte("just | prose\n"), Format: FormatMarkdown}, codeBadData},
		"extra md cell":      {Data{Bytes: []byte("| a |\n|---|\n| 1 | 2 |\n"), Format: FormatMarkdown}, codeShape},
		"duplicate column":   {csvData("a,a\n1,2\n"), codeBadData},
		"ragged csv":         {csvData("a,b\n1\n"), codeShape},
		"empty csv":          {csvData(""), codeDomain},
		"unknown format":     {Data{Bytes: []byte("x"), Format: "xlsx"}, codeBadRequest},
		"json too deep":      {Data{Bytes: []byte(strings.Repeat("[", 40) + strings.Repeat("]", 40)), Format: FormatJSON}, codeBadData},
		"json object inside": {Data{Bytes: []byte(`[[{"a":1}]]`), Format: FormatJSON}, codeBadData},
	} {
		_, err := evalData(t, "t", map[string]Data{"t": tc.data})
		if err == nil || err.Code != string(tc.code) {
			t.Errorf("%s: %v, want %s", name, err, tc.code)
			continue
		}
		if strings.Contains(err.Detail, "var t") {
			t.Errorf("%s: %q names a var", name, err.Detail)
		}
	}
}

// A name is given once, and is a valid, unreserved name.
func TestData_NamesAreCheckedLikeVars(t *testing.T) {
	d := csvData("a\n1\n")
	if _, err := Eval(context.Background(), Request{Expr: "t", Vars: map[string]json.RawMessage{"t": json.RawMessage("1")}, Data: map[string]Data{"t": d}}); err == nil || err.Code != string(codeBadRequest) {
		t.Errorf("a name in vars and from: %v", err)
	}
	if _, err := evalData(t, "1", map[string]Data{"pi": d}); err == nil || err.Code != string(codeReservedName) {
		t.Errorf("reserved name: %v", err)
	}
	if _, err := evalData(t, "1", map[string]Data{"1x": d}); err == nil || err.Code != string(codeBadVar) {
		t.Errorf("invalid name: %v", err)
	}
	many := map[string]Data{}
	for i := 0; i <= MaxRefSources; i++ {
		many["t"+strings.Repeat("x", i)] = d
	}
	if _, err := evalData(t, "1", many); err == nil || err.Code != string(codeBadRequest) {
		t.Errorf("%d sources: %v", len(many), err)
	}
}

// The byte cap is checked over every source together, before anything is
// parsed: an over-cap source of invalid JSON is too_large, not bad_data.
func TestData_ByteCapIsCheckedBeforeParsing(t *testing.T) {
	half := Data{Bytes: []byte(strings.Repeat("{", MaxRefBytes/2+1)), Format: FormatJSON}
	_, err := evalData(t, "1", map[string]Data{"a": half, "b": half})
	if err == nil || err.Code != string(codeTooLarge) {
		t.Fatalf("two half-cap sources = %v, want too_large", err)
	}
}

// The cell cap holds per source and over every source together.
func TestData_CellCapHoldsAcrossSources(t *testing.T) {
	wide := csvData(strings.Repeat("a,", MaxRefCells) + "a\n")
	if _, err := evalData(t, "1", map[string]Data{"t": wide}); err == nil || err.Code != string(codeListTooLong) {
		t.Errorf("a header of %d columns = %v, want list_too_long", MaxRefCells+1, err)
	}
	long := Data{Bytes: []byte("[" + strings.Repeat("0,", MaxRefCells) + "0]"), Format: FormatJSON}
	if _, err := evalData(t, "1", map[string]Data{"t": long}); err == nil || err.Code != string(codeListTooLong) {
		t.Errorf("a list of %d values = %v, want list_too_long", MaxRefCells+1, err)
	}
	half := Data{Bytes: []byte("[" + strings.Repeat("0,", MaxRefCells/2) + "0]"), Format: FormatJSON}
	if _, err := evalData(t, "1", map[string]Data{"a": half, "b": half}); err == nil || err.Code != string(codeListTooLong) {
		t.Errorf("two sources of %d values = %v, want list_too_long", MaxRefCells/2+1, err)
	}
}

// A loaded table may pass MaxTableCells; a table the expression builds from
// it is held to MaxTableCells again, and its rows are indexable.
func TestData_ALoadedTableMayPassTheInlineCap(t *testing.T) {
	var b strings.Builder
	b.WriteString("a,b\n")
	rows := MaxTableCells/2 + 10
	for i := 0; i < rows; i++ {
		b.WriteString("1,2\n")
	}
	data := map[string]Data{"t": csvData(b.String())}
	resp := mustData(t, "[len(t), sum(t.a), t[len(t)].b]", data)
	if resp.Text != "[50010, 50010, 2]" {
		t.Errorf("loaded table = %s", resp.Text)
	}
	if _, err := evalData(t, "select(t, \"a\", \"b\")", data); err == nil || err.Code != string(codeListTooLong) {
		t.Errorf("a built copy past MaxTableCells = %v, want list_too_long", err)
	}
}

// A load is charged per cell on top of MaxSteps, so reading data does not
// take the expression's own budget.
func TestData_ALoadIsChargedOnTopOfTheStepBudget(t *testing.T) {
	e := &evaluator{ctx: context.Background(), funcs: builtinIndex, global: &scope{vars: map[string]val{}}, sizes: map[listKey]int{}, nextCheck: ctxCheckEvery}
	if err := e.loadData(map[string]Data{"t": csvData("a,b\n1,2\n3,4\n")}, nil); err != nil {
		t.Fatal(err)
	}
	if e.steps != 4 || e.loadSteps != 4 {
		t.Fatalf("steps %d, loadSteps %d, want 4 and 4", e.steps, e.loadSteps)
	}
	if err := e.charge(MaxSteps, -1); err != nil {
		t.Errorf("the expression's whole budget after a load: %v", err)
	}
	if err := e.charge(1, -1); err == nil || err.Code != string(codeStepLimit) {
		t.Errorf("one step past both: %v", err)
	}
}

// A load takes a slot of the gate and waits for one under the call's own
// deadline; a call with no data never waits.
func TestData_ALoadWaitsForAGateSlot(t *testing.T) {
	gate := semaphore.NewWeighted(1)
	if !gate.TryAcquire(1) {
		t.Fatal("gate")
	}
	req := Request{Expr: "len(t)", Data: map[string]Data{"t": csvData("a\n1\n")}, Gate: gate}
	if _, err := evalWithin(context.Background(), req, 50*time.Millisecond); err == nil || err.Code != string(codeTimeLimit) {
		t.Errorf("a load with the gate full = %v, want time_limit", err)
	}
	if _, err := evalWithin(context.Background(), Request{Expr: "1", Gate: gate}, 50*time.Millisecond); err != nil {
		t.Errorf("no data, gate full: %v", err)
	}
	gate.Release(1)
	if resp, err := evalWithin(context.Background(), req, time.Second); err != nil || resp.Text != "1" {
		t.Errorf("gate free: %v %v", resp.Text, err)
	}
	if !gate.TryAcquire(1) {
		t.Error("the load kept its gate slot")
	}
}

// Write renders the whole result for storing, and the response carries its
// type and shape but none of its content.
func TestData_WriteRendersTheWholeResultAndReturnsNoContent(t *testing.T) {
	const expr = `t = csv("sku,price\na,19.99\nb,5"); secret = 42; t`
	for format, want := range map[string]string{
		FormatCSV:      "sku,price\na,19.99\nb,5\n",
		FormatMarkdown: "| sku | price |\n| --- | --- |\n| a | 19.99 |\n| b | 5 |\n",
		FormatJSON:     `{"columns":["sku","price"],"rows":[["a",19.99],["b",5]]}`,
	} {
		resp, err := Eval(context.Background(), Request{Expr: expr, Write: format})
		if err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		if string(resp.Written) != want {
			t.Errorf("%s wrote %q, want %q", format, resp.Written, want)
		}
		if resp.Result != nil || resp.Text != "" || resp.Assigned != nil || resp.Fraction != "" {
			t.Errorf("%s: the response carries content: %+v", format, resp)
		}
		if resp.Type != "table" || len(resp.Shape) != 2 || resp.Shape[0] != 2 || resp.Shape[1] != 2 || !resp.Exact {
			t.Errorf("%s: type %s shape %v exact %v", format, resp.Type, resp.Shape, resp.Exact)
		}
	}
	if resp, err := Eval(context.Background(), Request{Expr: "1/3", Write: FormatJSON}); err != nil || string(resp.Written) != "0.333333333333333" || resp.Fraction != "" {
		t.Errorf("1/3 as json: %q %+v %v", resp.Written, resp, err)
	}
	if _, err := Eval(context.Background(), Request{Expr: "[1, 2]", Write: FormatCSV}); err == nil || err.Code != string(codeType) {
		t.Errorf("a list as csv: %v, want type", err)
	}
	if _, err := Eval(context.Background(), Request{Expr: "1", Write: "xlsx"}); err == nil || err.Code != string(codeBadRequest) {
		t.Errorf("unknown write format: %v", err)
	}
}

// The written size is bounded before json.Marshal expands a shared value:
// one long string repeated past the cap is refused.
func TestData_WriteRefusesAResultPastTheByteCap(t *testing.T) {
	s, _ := json.Marshal(strings.Repeat("x", 900_000))
	req := Request{Expr: "map(1:20, k -> s)", Vars: map[string]json.RawMessage{"s": s}, Write: FormatJSON}
	if _, err := Eval(context.Background(), req); err == nil || err.Code != string(codeTooLarge) {
		t.Errorf("an 18 MB result = %v, want too_large", err)
	}
}

// The cell budget is enforced while the data is read, not counted after it
// is all built: each input is over the budget and then malformed, so a reader
// that checks only afterwards reaches the malformed part (bad_data, syntax or
// shape), having allocated everything before it.
func TestData_TheCellBudgetStopsTheReadBeforeTheRest(t *testing.T) {
	for name, d := range map[string]Data{
		"json values": {Bytes: []byte("[" + strings.Repeat("0,", 3*MaxRefCells+64) + "}"), Format: FormatJSON},
		"csv header":  csvData(strings.Repeat("a,", MaxRefCells) + "a\n\"x\"y\n"),
		"md header":   {Bytes: []byte(strings.Repeat("| a ", MaxRefCells+1) + "|\n|---|\n"), Format: FormatMarkdown},
	} {
		if _, err := evalData(t, "1", map[string]Data{"t": d}); err == nil || err.Code != string(codeListTooLong) {
			t.Errorf("%s: %v, want list_too_long before the rest is read", name, err)
		}
	}
}

func TestSniffFormat_GuessesFromTheFirstBytes(t *testing.T) {
	for in, want := range map[string]string{
		"  [1,2]":               FormatJSON,
		"\xEF\xBB\xBF{\"a\":1}": FormatJSON,
		"| a |\n|---|":          FormatMarkdown,
		"a\tb\n1\t2":            FormatTSV,
		"a,b\tc\n":              FormatCSV,
		"sku,price\na,1\n":      FormatCSV,
		"":                      FormatCSV,
		"single column\n1\n":    FormatCSV,
	} {
		if got := SniffFormat([]byte(in)); got != want {
			t.Errorf("SniffFormat(%q) = %s, want %s", in, got, want)
		}
	}
}
