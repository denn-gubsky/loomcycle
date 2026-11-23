package calc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// sales is the table most tests read: three regions, an empty amount.
const sales = `t = csv("region,month,amount\nEU,jan,120.5\nUS,jan,300\nEU,feb,79.5\nUS,feb,\nAP,feb,10")`

func TestTables_CSVBuildsTypedCellsAndEmptyCells(t *testing.T) {
	checkTexts(t, []textCase{
		{sales + "; t", `{"columns": ["region", "month", "amount"], "rows": [["EU", "jan", 120.5], ["US", "jan", 300], ["EU", "feb", 79.5], ["US", "feb", null], ["AP", "feb", 10]]}`},
		{sales + "; t.amount", "[120.5, 300, 79.5, null, 10]"},
		{sales + "; t[2]", `{"region": "US", "month": "jan", "amount": 300}`},
		{sales + "; t[2].amount", "300"},
		{sales + `; t["region"]`, `["EU", "US", "EU", "US", "AP"]`},
		{sales + "; t[[5, 1]].region", `["AP", "EU"]`},
		{sales + "; [len(t), size(t), count(t), len(t[1])]", "[5, [5, 3], 5, 3]"},
		{`csv("a;b\n1;x y\n;\"q;r\"", ";")`, `{"columns": ["a", "b"], "rows": [[1, "x y"], [null, "q;r"]]}`},
		{`csv("a\tb\n1\t2", "\t").b`, "[2]"},
		{`csv("n\n0.1\n0.2").n[1] + csv("n\n0.1\n0.2").n[2]`, "0.3"},
		{`csv("h\n")`, `{"columns": ["h"], "rows": []}`},
	})
}

func TestTables_AggregatesSkipEmptyCellsAndKeepExactness(t *testing.T) {
	checkTexts(t, []textCase{
		{sales + "; sum(t.amount)", "510"},
		{sales + "; mean(t.amount)", "127.5"},
		{sales + "; count(t.amount)", "4"},
		{sales + "; len(t.amount)", "5"},
		{sales + "; [min(t.amount), max(t.amount), median(t.amount)]", "[10, 300, 100]"},
		{sales + "; t.amount * 2", "[241, 600, 159, null, 20]"},
		{sales + "; sum(t.amount .* t.amount) == sum(map(t.amount, x -> x^2))", "true"},
		{sales + "; round(t.amount / 3, 2)", "[40.17, 100, 26.5, null, 3.33]"},
		{sales + "; map(t.amount, isempty)", "[false, false, false, true, false]"},
		{sales + "; count(t.amount, x -> x > 50)", "3"},
		{sales + "; count(t.amount, isempty)", "1"},
		{`sum(csv("p\n0.10\n0.20\n0.30").p)`, "0.6"},
	})
}

func TestTables_FunctionsGiveTheirDocumentedTables(t *testing.T) {
	checkTexts(t, []textCase{
		{sales + `; select(t, "amount", "region")`, `{"columns": ["amount", "region"], "rows": [[120.5, "EU"], [300, "US"], [79.5, "EU"], [null, "US"], [10, "AP"]]}`},
		{sales + `; select(t, ["month"]).month`, `["jan", "jan", "feb", "feb", "feb"]`},
		{sales + `; where(t, r -> r.amount > 50).region`, `["EU", "US", "EU"]`},
		{sales + `; where(t, r -> isempty(r.amount)).region`, `["US"]`},
		{sales + `; where(t, r -> r.region == "EU" and r.month == "feb").amount`, `[79.5]`},
		{sales + `; addcol(t, "tax", r -> r.amount * 0.2).tax`, `[24.1, 60, 15.9, null, 2]`},
		{sales + `; addcol(t, "n", 1:5).n`, `[1, 2, 3, 4, 5]`},
		{sales + `; addcol(t, "cur", "EUR")[1]`, `{"region": "EU", "month": "jan", "amount": 120.5, "cur": "EUR"}`},
		{sales + `; addcol(t, "amount", r -> 0).amount`, `[0, 0, 0, 0, 0]`},
		{sales + `; sort(t, "amount", "desc").amount`, `[300, 120.5, 79.5, 10, null]`},
		{sales + `; sort(t, "amount").amount`, `[10, 79.5, 120.5, 300, null]`},
		{sales + `; sort(t, "region").month`, `["feb", "jan", "feb", "jan", "feb"]`},
		{sales + `; head(t, 2).region`, `["EU", "US"]`},
		{sales + `; tail(t, 2).region`, `["US", "AP"]`},
		{sales + `; len(head(t))`, `5`},
		{`[head(1:10, 3), tail(1:10, 3), head([1], 5), tail(1:3, 0)]`, `[[1, 2, 3], [8, 9, 10], [1], []]`},
		{sales + `; groupby(t, "region", "total", g -> sum(g.amount), "rows", g -> count(g))`,
			`{"columns": ["region", "total", "rows"], "rows": [["EU", 200, 2], ["US", 300, 2], ["AP", 10, 1]]}`},
		{sales + `; groupby(t, ["region", "month"], "n", count).region`, `["EU", "US", "EU", "US", "AP"]`},
		{sales + `; groupby(t, "month")`, `{"columns": ["month"], "rows": [["jan"], ["feb"]]}`},
		{sales + `; pivot(t, "region", "month", g -> sum(g.amount))`,
			`{"columns": ["region", "jan", "feb"], "rows": [["EU", 120.5, 79.5], ["US", 300, 0], ["AP", null, 10]]}`},
		{sales + `; totals(t)[6]`, `{"region": "total", "month": null, "amount": 510}`},
		{sales + `; describe(t)`,
			`{"columns": ["stat", "amount"], "rows": [["count", 4], ["empty", 1], ["mean", 127.5], ["stdev", 123.714052017815], ["min", 10], ["q1", 62.125], ["median", 100], ["q3", 165.375], ["max", 300]]}`},
	})
}

func TestTables_JoinsMatchOnKeysAndNameClashes(t *testing.T) {
	const two = `a = csv("id,name\n1,ann\n2,bob\n3,cy\n,nobody"); b = csv("id,score,name\n2,7,B\n1,5,A\n2,9,B2\n,1,ghost")`
	checkTexts(t, []textCase{
		{two + `; join(a, b, "id")`,
			`{"columns": ["id", "name", "score", "name_2"], "rows": [[1, "ann", 5, "A"], [2, "bob", 7, "B"], [2, "bob", 9, "B2"]]}`},
		{two + `; leftjoin(a, b, "id").score`, `[5, 7, 9, null, null]`},
		{two + `; join(a, b, ["id"]).name_2`, `["A", "B", "B2"]`},
		// 1 and 1.0 are one key; "1" (a string) is not.
		{`join(csv("k,x\n1,a"), csv("k,y\n1.0,b"), "k").y`, `["b"]`},
	})
}

func TestTables_VarsTakeBothShapes(t *testing.T) {
	vars := map[string]json.RawMessage{
		"t": json.RawMessage(`{"columns": ["sku", "price", "qty"], "rows": [["a", 19.99, 3], ["b", "0.10", null]]}`),
		"r": json.RawMessage(`[{"sku": "a", "price": 1.5}, {"price": 2, "qty": 4}]`),
	}
	cases := []textCase{
		{"sum(t.price .* t.qty)", "59.97"},
		{"t.price[2] * 3", "0.3"},
		{"r", `{"columns": ["sku", "price", "qty"], "rows": [["a", 1.5, null], [null, 2, 4]]}`},
	}
	for _, c := range cases {
		got, err := Eval(context.Background(), Request{Expr: c.expr, Vars: vars})
		if err != nil || got.Text != c.want {
			t.Errorf("%s = %q, %v; want %q", c.expr, got.Text, err, c.want)
		}
	}
	got, err := Eval(context.Background(), Request{Expr: "t", Vars: vars})
	if err != nil || got.Type != "table" || !equalInts(got.Shape, []int{2, 3}) || !got.Exact {
		t.Fatalf("t: type %s shape %v exact %v err %v", got.Type, got.Shape, got.Exact, err)
	}
	b, _ := json.Marshal(got.Result)
	if string(b) != `{"columns":["sku","price","qty"],"rows":[["a",19.99,3],["b",0.1,null]]}` {
		t.Errorf("result JSON = %s", b)
	}
	got, err = Eval(context.Background(), Request{Expr: "t[1]", Vars: vars})
	b, _ = json.Marshal(got.Result)
	if err != nil || got.Type != "row" || string(b) != `{"sku":"a","price":19.99,"qty":3}` {
		t.Errorf("t[1]: type %s result %s err %v", got.Type, b, err)
	}
}

func TestTables_VarsRefuseMalformedTables(t *testing.T) {
	for name, raw := range map[string]string{
		"extra key":     `{"columns": ["a"], "rows": [[1]], "x": 1}`,
		"short row":     `{"columns": ["a", "b"], "rows": [[1]]}`,
		"dup column":    `{"columns": ["a", "a"], "rows": [[1, 2]]}`,
		"list cell":     `{"columns": ["a"], "rows": [[[1]]]}`,
		"no columns":    `{"columns": [], "rows": []}`,
		"numeric name":  `{"columns": [1], "rows": [[1]]}`,
		"nested object": `[[{"a": 1}]]`,
		"mixed records": `[{"a": 1}, 2]`,
	} {
		_, err := Eval(context.Background(), Request{Expr: "t", Vars: map[string]json.RawMessage{"t": json.RawMessage(raw)}})
		if err == nil || err.Code != string(codeBadVar) {
			t.Errorf("%s: got %v, want bad_var", name, err)
		}
	}
}

func TestTables_RefusalsNameTheFix(t *testing.T) {
	for _, c := range []struct {
		expr string
		code errCode
		hint string
	}{
		{sales + "; t.amout", codeColumn, "amount"},
		{sales + `; select(t, "nope")`, codeColumn, "region"},
		{sales + "; t[0]", codeIndex, "from 1"},
		{sales + "; t[9]", codeIndex, ""},
		{sales + "; t[1, 2]", codeIndex, "t[i].name"},
		{sales + "; (1:3).x", codeType, ""},
		{sales + "; t.", codeSyntax, "unit price"},
		{sales + `; addcol(t, "x", [1, 2])`, codeShape, ""},
		{sales + `; addcol(t, "x", r -> [1])`, codeType, "aggregate"},
		{sales + `; groupby(t, "region", "x")`, codeArity, "groupby(t"},
		{sales + `; sort(t)`, codeArity, "sort(t"},
		{sales + `; sort(t, "region", "up")`, codeDomain, ""},
		{`pivot(csv("k,v\nk,1"), "k", "k", count)`, codeColumn, ""},
		{`csv("")`, codeDomain, ""},
		{`csv("a,b\n1")`, codeShape, ""},
		{`csv("a\n\"x")`, codeSyntax, ""},
		{`csv("a,a\n1,2")`, codeColumn, ""},
		{`csv("a", "ab")`, codeDomain, ""},
		{`describe(csv("a\nx"))`, codeType, ""},
		{`select([1], "a")`, codeType, "csv"},
		{sales + "; wmean(t.amount, 1:5)", codeType, ""},
	} {
		err := mustFail(t, c.expr, c.code)
		if !strings.Contains(err.Hint, c.hint) {
			t.Errorf("%s: hint %q, want it to mention %q", c.expr, err.Hint, c.hint)
		}
	}
}

func TestTables_FormatRendersTablesAndMatrices(t *testing.T) {
	for _, c := range []struct{ expr, format, want string }{
		{sales + "; head(t, 2)", "markdown", "| region | month | amount |\n| --- | --- | --- |\n| EU | jan | 120.5 |\n| US | jan | 300 |\n"},
		{sales + "; tail(t, 2)", "csv", "region,month,amount\nUS,feb,\nAP,feb,10\n"},
		{`csv("a\n\"x|y\"")`, "markdown", "| a |\n| --- |\n| x\\|y |\n"},
		{`csv("a\n\"x,y\"")`, "csv", "a\n\"x,y\"\n"},
		{"[[1, 2], [3, 4]]", "markdown", "| 1 | 2 |\n| --- | --- |\n| 1 | 2 |\n| 3 | 4 |\n"},
		{"[[1, 2], [3, 4]]", "csv", "1,2\n3,4\n"},
		{"[[1, 2], [3, 4]]", "json", "[[1, 2], [3, 4]]"},
		{"[1, 2]", "csv", "[1, 2]"},
		{"1/4", "markdown", "0.25"},
	} {
		r, err := Eval(context.Background(), Request{Expr: c.expr, Format: c.format})
		if err != nil || r.Text != c.want {
			t.Errorf("%s as %s = %q, %v; want %q", c.expr, c.format, r.Text, err, c.want)
		}
	}
	if _, err := Eval(context.Background(), Request{Expr: "1", Format: "xml"}); err == nil || err.Code != string(codeBadRequest) {
		t.Errorf("format xml: %v, want bad_request", err)
	}
}

func TestTables_CellLimitIsCheckedBeforeBuilding(t *testing.T) {
	// 400 rows joined to themselves on one key is 160,000 rows: refused from
	// the pair count, not after building them.
	ctx := context.Background()
	text := func(rows int) map[string]json.RawMessage {
		b, _ := json.Marshal("k\n" + strings.Repeat("1\n", rows))
		return map[string]json.RawMessage{"s": b}
	}
	r, err := evalWithin(ctx, Request{Expr: `t = csv(s); join(t, t, "k")`, Vars: text(400)}, time.Minute)
	if err == nil || err.Code != string(codeListTooLong) {
		t.Fatalf("self-join of 400 equal keys: %q, %v; want list_too_long", r.Text, err)
	}
	if _, err := evalWithin(ctx, Request{Expr: `t = csv(s); len(t)`, Vars: text(MaxTableCells)}, time.Minute); err != nil {
		t.Fatalf("csv at the cap: %v", err)
	}
	r, err = evalWithin(ctx, Request{Expr: `t = csv(s); len(t)`, Vars: text(MaxTableCells + 1)}, time.Minute)
	if err == nil || err.Code != string(codeListTooLong) {
		t.Fatalf("csv over the cap: %q, %v; want list_too_long", r.Text, err)
	}
	// A long string repeated down a key column costs few steps per row, but
	// the keys a join or a groupby builds are bounded in bytes.
	long, _ := json.Marshal(strings.Repeat("x", 900_000))
	for _, expr := range []string{
		`t = addcol(csv(s), "x", long); join(t, t, "x")`,
		`t = addcol(csv(s), "x", long); groupby(t, "x")`,
	} {
		vars := text(100)
		vars["long"] = long
		r, err = evalWithin(ctx, Request{Expr: expr, Vars: vars}, time.Minute)
		if err == nil || err.Code != string(codeTooLarge) {
			t.Errorf("%s: %d bytes of text, %v; want too_large", expr, len(r.Text), err)
		}
	}
	// Eleven tables of the cap each fit; a list holding them passes the
	// nested-size bound, which counts cells.
	r, err = evalWithin(ctx, Request{Expr: `t = csv(s); [t, t, t, t, t, t, t, t, t, t, t]`, Vars: text(MaxTableCells)}, time.Minute)
	if err == nil || err.Code != string(codeListTooLong) {
		t.Fatalf("list of eleven full tables: %q, %v; want list_too_long", r.Text, err)
	}
}
