package calc

import (
	"encoding/csv"
	"errors"
	"io"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// SecTables is the section of the table functions (help: Calc/tables).
const SecTables = "tables"

// The table functions take a rule (a lambda or a function) where they need
// one, as map and filter do: where and addcol call it with a row, groupby
// and pivot with the rows of one group as a table. Every function charges
// one step per cell it builds, on top of what its rules charge, and
// refuses a result over MaxTableCells before building it.
var tableFuncs = []builtin{
	{name: "csv", sig: "csv(text, sep?)", section: SecTables, min: 1, max: 2, fn: fnCSV,
		summary: "a table from CSV text; the first line names the columns"},
	{name: "select", sig: "select(t, column, ...)", section: SecTables, min: 2, max: -1, fn: fnSelect,
		summary: "the table with only these columns, in this order"},
	{name: "where", sig: "where(t, pred)", section: SecTables, min: 2, max: 2, fn: fnWhere,
		summary: "the rows r of t where pred(r) is true"},
	{name: "addcol", sig: "addcol(t, name, rule)", section: SecTables, min: 3, max: 3, fn: fnAddcol,
		summary: "t with a column from rule(r) per row, a list or one value; replaces a column of that name"},
	{name: "head", sig: "head(v, n?)", section: SecTables, min: 1, max: 2, fn: headTail(true),
		summary: "the first n rows of a table or elements of a list (default 5)"},
	{name: "tail", sig: "tail(v, n?)", section: SecTables, min: 1, max: 2, fn: headTail(false),
		summary: "the last n rows of a table or elements of a list (default 5)"},
	{name: "groupby", sig: "groupby(t, keys, name, rule, ...)", section: SecTables, min: 2, max: -1, fn: fnGroupby,
		summary: "one row per distinct key, with a column per name from rule(group)"},
	{name: "pivot", sig: "pivot(t, rows, columns, rule)", section: SecTables, min: 4, max: 4, fn: fnPivot,
		summary: "a row per value of one column, a column per value of another, rule(group) in each cell"},
	{name: "join", sig: "join(a, b, key)", section: SecTables, min: 3, max: 3, fn: joinFn(false),
		summary: "the rows of a and b that match on the key column(s)"},
	{name: "leftjoin", sig: "leftjoin(a, b, key)", section: SecTables, min: 3, max: 3, fn: joinFn(true),
		summary: "every row of a, with the matching rows of b or empty cells"},
	{name: "totals", sig: "totals(t)", section: SecTables, min: 1, max: 1, fn: fnTotals,
		summary: "t with a row of column sums added"},
	{name: "describe", sig: "describe(t)", section: SecTables, min: 1, max: 1, fn: fnDescribeTable,
		summary: "count, empty cells, mean, stdev, min, quartiles and max of each numeric column"},
	{name: "isempty", sig: "isempty(x)", section: SecTables, min: 1, max: 1, fn: fnIsEmpty,
		summary: "whether x is an empty table cell"},
}

func (a args) table(i int) (*tableVal, *Error) {
	t, ok := a.v[i].(*tableVal)
	if !ok {
		e := errf(codeType, a.pos, "%s: argument %d must be a table, got a %s", a.name, i+1, typeName(a.v[i]))
		if _, isList := a.v[i].([]val); isList {
			e.Hint = `a table comes from vars ({"columns": [...], "rows": [...]} or an array of objects) or csv("...")`
		}
		return nil, e
	}
	return t, nil
}

// columnsArg reads argument i as one column name or a list of them, each
// a column of t, and returns their indexes.
func (e *evaluator) columnsArg(a args, t *tableVal, i int) ([]int, []string, *Error) {
	var names []string
	switch v := a.v[i].(type) {
	case string:
		names = []string{v}
	case []val:
		for _, x := range v {
			s, ok := x.(string)
			if !ok {
				return nil, nil, errf(codeType, a.pos, "%s: argument %d must be column names, got a %s", a.name, i+1, typeName(x))
			}
			names = append(names, s)
		}
		if len(names) == 0 {
			return nil, nil, errf(codeColumn, a.pos, "%s: argument %d names no column", a.name, i+1)
		}
	default:
		return nil, nil, errf(codeType, a.pos, "%s: argument %d must be a column name or a list of names, got a %s", a.name, i+1, typeName(a.v[i]))
	}
	if err := e.charge(len(names), a.pos); err != nil {
		return nil, nil, err
	}
	idx := make([]int, len(names))
	for k, n := range names {
		if idx[k] = t.col(n); idx[k] < 0 {
			return nil, nil, unknownColumn(n, t.names, a.pos)
		}
	}
	return idx, names, nil
}

// cellResult checks that a rule gave a value a cell can hold.
func cellResult(v val, a args) (val, *Error) {
	if !isCell(v) {
		return nil, errf(codeType, a.pos, "%s: the rule must give one value per cell, got a %s", a.name, typeName(v)).
			withHint("aggregate a column inside the rule, e.g. g -> sum(g.amount)")
	}
	return v, nil
}

func fnCSV(e *evaluator, a args) (val, *Error) {
	text, err := a.str(0)
	if err != nil {
		return nil, err
	}
	sep := ','
	if len(a.v) == 2 {
		s, err := a.str(1)
		if err != nil {
			return nil, err
		}
		r, n := utf8.DecodeRuneInString(s)
		if n != len(s) || n == 0 || r == '"' || r == '\n' || r == '\r' || r == utf8.RuneError {
			return nil, errf(codeDomain, a.pos, "csv: the separator must be one character other than a quote or a line break, got %q", s).
				withHint(`csv(text, ";") or csv(text, "\t")`)
		}
		sep = r
	}
	r := csv.NewReader(strings.NewReader(text))
	r.Comma = sep
	header, rerr := r.Read()
	if rerr == io.EOF {
		return nil, errf(codeDomain, a.pos, "csv: the text is empty").withHint("the first line names the columns")
	}
	if rerr != nil {
		return nil, csvErr(rerr, a)
	}
	names := make([]string, len(header))
	for j, h := range header {
		names[j] = strings.TrimSpace(h)
	}
	cols := make([][]val, len(names))
	for {
		rec, rerr := r.Read()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, csvErr(rerr, a)
		}
		if (len(cols[0])+1)*len(names) > MaxTableCells {
			return nil, errf(codeListTooLong, a.pos, "csv: the table has more than %d cells", MaxTableCells)
		}
		for j, f := range rec {
			cols[j] = append(cols[j], csvCell(f))
		}
	}
	for j := range cols {
		if cols[j] == nil {
			cols[j] = []val{}
		}
	}
	return e.newTable(names, cols, a.pos)
}

// csvCell reads one field: blank is an empty cell, a number written as a
// var string would be ("19.99", "-1e3", "1_000") is that exact number,
// anything else is the text with its surrounding spaces trimmed.
func csvCell(f string) val {
	s := strings.TrimSpace(f)
	if s == "" {
		return empty
	}
	if n, ok := numericString(s); ok {
		return n
	}
	return s
}

func csvErr(err error, a args) *Error {
	var pe *csv.ParseError
	if errors.As(err, &pe) {
		if errors.Is(pe.Err, csv.ErrFieldCount) {
			return errf(codeShape, a.pos, "csv: line %d has a different number of fields than the header", pe.Line).
				withHint("every line needs one field per column; quote a field that contains the separator")
		}
		return errf(codeSyntax, a.pos, "csv: line %d: %v", pe.Line, pe.Err).
			withHint(`a quoted field starts and ends with ", and a quote inside one is written ""`)
	}
	return errf(codeSyntax, a.pos, "csv: %v", err)
}

func fnSelect(e *evaluator, a args) (val, *Error) {
	t, err := a.table(0)
	if err != nil {
		return nil, err
	}
	var names []string
	var cols [][]val
	for i := 1; i < len(a.v); i++ {
		idx, ns, err := e.columnsArg(a, t, i)
		if err != nil {
			return nil, err
		}
		for k, j := range idx {
			names = append(names, ns[k])
			cols = append(cols, t.cols[j])
		}
	}
	return e.newTable(names, cols, a.pos)
}

func fnWhere(e *evaluator, a args) (val, *Error) {
	t, err := a.table(0)
	if err != nil {
		return nil, err
	}
	f, err := a.fn(1)
	if err != nil {
		return nil, err
	}
	keep := []int{}
	for i := range t.rows() {
		ok, err := e.predicate(f, rowVal{t: t, i: i}, a)
		if err != nil {
			return nil, err
		}
		if ok {
			keep = append(keep, i)
		}
	}
	return e.takeRows(t, keep, a.pos)
}

func fnAddcol(e *evaluator, a args) (val, *Error) {
	t, err := a.table(0)
	if err != nil {
		return nil, err
	}
	name, err := a.str(1)
	if err != nil {
		return nil, err
	}
	n := t.rows()
	col := make([]val, n)
	switch x := a.v[2].(type) {
	case *fnVal:
		for i := range n {
			v, err := e.call(x, []val{rowVal{t: t, i: i}}, a.pos)
			if err != nil {
				return nil, err
			}
			if col[i], err = cellResult(v, a); err != nil {
				return nil, err
			}
		}
	case []val:
		if len(x) != n {
			return nil, errf(codeShape, a.pos, "addcol: the list has %d values for a table of %d rows", len(x), n)
		}
		for i, v := range x {
			if col[i], err = cellResult(v, a); err != nil {
				return nil, err
			}
		}
	default:
		if _, err := cellResult(x, a); err != nil {
			return nil, err
		}
		for i := range col {
			col[i] = x
		}
	}
	names := append([]string(nil), t.names...)
	cols := append([][]val(nil), t.cols...)
	if j := t.col(name); j >= 0 {
		cols[j] = col
	} else {
		names = append(names, name)
		cols = append(cols, col)
	}
	return e.newTable(names, cols, a.pos)
}

func headTail(head bool) func(*evaluator, args) (val, *Error) {
	return func(e *evaluator, a args) (val, *Error) {
		n := int64(5)
		if len(a.v) == 2 {
			var err *Error
			if n, err = a.small(1, MaxListLen); err != nil {
				return nil, err
			}
			if n < 0 {
				return nil, errf(codeDomain, a.pos, "%s: n must not be negative, got %d", a.name, n)
			}
		}
		var total int
		switch v := a.v[0].(type) {
		case *tableVal:
			total = v.rows()
		case []val:
			total = len(v)
		default:
			return nil, errf(codeType, a.pos, "%s needs a table or a list, got a %s", a.name, typeName(a.v[0]))
		}
		k := min(int(n), total)
		from := 0
		if !head {
			from = total - k
		}
		if l, ok := a.v[0].([]val); ok {
			if err := e.charge(k, a.pos); err != nil {
				return nil, err
			}
			return l[from : from+k : from+k], nil
		}
		order := make([]int, k)
		for i := range order {
			order[i] = from + i
		}
		return e.takeRows(a.v[0].(*tableVal), order, a.pos)
	}
}

// groupRows splits t's rows by the key columns, groups in the order their
// first row appears. An empty key cell is a key value like any other.
func (e *evaluator) groupRows(t *tableVal, keys []int, a args) ([][]int, *Error) {
	index := map[string]int{}
	var groups [][]int
	var k keyBuf
	for i := range t.rows() {
		k.b.Reset()
		for _, j := range keys {
			if err := e.valueKey(&k, t.cols[j][i], a); err != nil {
				return nil, err
			}
			if err := k.write("|", a); err != nil {
				return nil, err
			}
		}
		key := k.b.String()
		g, ok := index[key]
		if !ok {
			g = len(groups)
			index[key] = g
			groups = append(groups, nil)
		}
		groups[g] = append(groups[g], i)
	}
	return groups, nil
}

func fnGroupby(e *evaluator, a args) (val, *Error) {
	t, err := a.table(0)
	if err != nil {
		return nil, err
	}
	keys, keyNames, err := e.columnsArg(a, t, 1)
	if err != nil {
		return nil, err
	}
	if (len(a.v)-2)%2 != 0 {
		return nil, errf(codeArity, a.pos, "groupby takes the keys, then pairs of a column name and a rule").
			withHint(`groupby(t, "region", "total", g -> sum(g.amount), "orders", g -> count(g))`)
	}
	names := append([]string(nil), keyNames...)
	var rules []*fnVal
	for i := 2; i < len(a.v); i += 2 {
		n, err := a.str(i)
		if err != nil {
			return nil, err
		}
		f, err := a.fn(i + 1)
		if err != nil {
			return nil, err
		}
		names = append(names, n)
		rules = append(rules, f)
	}
	groups, err := e.groupRows(t, keys, a)
	if err != nil {
		return nil, err
	}
	if len(groups)*len(names) > MaxTableCells {
		return nil, errf(codeListTooLong, a.pos, "groupby: %d groups of %d columns pass the %d-cell limit", len(groups), len(names), MaxTableCells)
	}
	cols := make([][]val, len(names))
	for c := range cols {
		cols[c] = make([]val, len(groups))
	}
	for g, rows := range groups {
		for k, j := range keys {
			cols[k][g] = t.cols[j][rows[0]]
		}
		if len(rules) == 0 {
			continue
		}
		sub, err := e.takeRows(t, rows, a.pos)
		if err != nil {
			return nil, err
		}
		for r, f := range rules {
			v, err := e.call(f, []val{sub}, a.pos)
			if err != nil {
				return nil, err
			}
			if cols[len(keys)+r][g], err = cellResult(v, a); err != nil {
				return nil, err
			}
		}
	}
	return e.newTable(names, cols, a.pos)
}

func fnPivot(e *evaluator, a args) (val, *Error) {
	t, err := a.table(0)
	if err != nil {
		return nil, err
	}
	rk, rn, err := e.columnsArg(a, t, 1)
	if err != nil {
		return nil, err
	}
	ck, _, err := e.columnsArg(a, t, 2)
	if err != nil {
		return nil, err
	}
	f, err := a.fn(3)
	if err != nil {
		return nil, err
	}
	if len(rk) != 1 || len(ck) != 1 {
		return nil, errf(codeType, a.pos, "pivot takes one column for the rows and one for the columns")
	}
	rowGroups, err := e.groupRows(t, rk, a)
	if err != nil {
		return nil, err
	}
	colGroups, err := e.groupRows(t, ck, a)
	if err != nil {
		return nil, err
	}
	if len(rowGroups)*(1+len(colGroups)) > MaxTableCells {
		return nil, errf(codeListTooLong, a.pos, "pivot: %d rows of %d columns pass the %d-cell limit", len(rowGroups), 1+len(colGroups), MaxTableCells)
	}
	rowOf := make([]int, t.rows())
	for g, rows := range rowGroups {
		for _, i := range rows {
			rowOf[i] = g
		}
	}
	names := []string{rn[0]}
	cols := make([][]val, 1+len(colGroups))
	cols[0] = make([]val, len(rowGroups))
	for g, rows := range rowGroups {
		cols[0][g] = t.cols[rk[0]][rows[0]]
	}
	for c, crows := range colGroups {
		names = append(names, pivotName(t.cols[ck[0]][crows[0]]))
		col := make([]val, len(rowGroups))
		for i := range col {
			col[i] = empty
		}
		// The rows of this column, split by their row group, in order.
		byRow := map[int][]int{}
		var order []int
		for _, i := range crows {
			g := rowOf[i]
			if _, ok := byRow[g]; !ok {
				order = append(order, g)
			}
			byRow[g] = append(byRow[g], i)
		}
		for _, g := range order {
			sub, err := e.takeRows(t, byRow[g], a.pos)
			if err != nil {
				return nil, err
			}
			v, err := e.call(f, []val{sub}, a.pos)
			if err != nil {
				return nil, err
			}
			if col[g], err = cellResult(v, a); err != nil {
				return nil, err
			}
		}
		cols[1+c] = col
	}
	return e.newTable(names, cols, a.pos)
}

// pivotName is the column name a pivot gives a value: its text, a string
// bare, an empty cell "(empty)".
func pivotName(v val) string {
	if isEmpty(v) {
		return "(empty)"
	}
	return cellText(v, DefaultDigits, MaxVarsBytes) // a name is never cut: every string here came in under the vars or expr bound
}

// joinFn joins a and b on key columns of the same name in both. Rows come
// in a's order, each with its matches in b's order; an empty key matches
// nothing. b's other columns follow a's, a clashing name gaining "_2".
func joinFn(left bool) func(*evaluator, args) (val, *Error) {
	return func(e *evaluator, a args) (val, *Error) {
		ta, err := a.table(0)
		if err != nil {
			return nil, err
		}
		tb, err := a.table(1)
		if err != nil {
			return nil, err
		}
		ka, keyNames, err := e.columnsArg(a, ta, 2)
		if err != nil {
			return nil, err
		}
		b := a
		b.v = []val{tb, tb, a.v[2]}
		kb, _, err := e.columnsArg(b, tb, 2)
		if err != nil {
			err.Detail = a.name + ": the second table: " + err.Detail
			return nil, err
		}
		// One keyBuf for the whole call, so the keys of both tables together
		// stay under maxKeyBytes however long their strings are.
		var k keyBuf
		keyOf := func(t *tableVal, cols []int, i int) (string, bool, *Error) {
			k.b.Reset()
			for _, j := range cols {
				if isEmpty(t.cols[j][i]) {
					return "", false, nil
				}
				if err := e.valueKey(&k, t.cols[j][i], a); err != nil {
					return "", false, err
				}
				if err := k.write("|", a); err != nil {
					return "", false, err
				}
			}
			return k.b.String(), true, nil
		}
		index := map[string][]int{}
		for i := range tb.rows() {
			k, ok, err := keyOf(tb, kb, i)
			if err != nil {
				return nil, err
			}
			if ok {
				index[k] = append(index[k], i)
			}
		}
		// Pair the rows first and check the size before building anything:
		// a join can produce a product of its inputs.
		var pa, pb []int
		for i := range ta.rows() {
			k, ok, err := keyOf(ta, ka, i)
			if err != nil {
				return nil, err
			}
			var m []int
			if ok {
				m = index[k]
			}
			if len(m) == 0 && left {
				pa, pb = append(pa, i), append(pb, -1)
			}
			for _, j := range m {
				pa, pb = append(pa, i), append(pb, j)
			}
			if len(pa) > MaxTableCells {
				break
			}
		}
		// Sets, not scans: both tables may be thousands of columns wide.
		if err := e.charge(len(ta.names)+len(tb.names), a.pos); err != nil {
			return nil, err
		}
		isKey := make(map[string]bool, len(keyNames))
		for _, n := range keyNames {
			isKey[n] = true
		}
		names := append([]string(nil), ta.names...)
		used := make(map[string]bool, len(ta.names)+len(tb.names))
		for _, n := range ta.names {
			used[n] = true
		}
		var bcols []int
		for j, n := range tb.names {
			if isKey[n] {
				continue
			}
			// A clash takes the first free suffix; every try is charged,
			// since a may already hold x_2, x_3, ...
			for s := 2; used[n]; s++ {
				if err := e.charge(1, a.pos); err != nil {
					return nil, err
				}
				n = tb.names[j] + "_" + strconv.Itoa(s)
			}
			used[n] = true
			names = append(names, n)
			bcols = append(bcols, j)
		}
		if len(pa)*len(names) > MaxTableCells {
			return nil, errf(codeListTooLong, a.pos, "%s: the result passes the %d-cell limit", a.name, MaxTableCells).
				withHint("a key repeated on both sides multiplies rows; filter or group first")
		}
		cols := make([][]val, len(names))
		for j := range ta.cols {
			c := make([]val, len(pa))
			for r, i := range pa {
				c[r] = ta.cols[j][i]
			}
			cols[j] = c
		}
		for k, j := range bcols {
			c := make([]val, len(pb))
			for r, i := range pb {
				if i < 0 {
					c[r] = empty
				} else {
					c[r] = tb.cols[j][i]
				}
			}
			cols[len(ta.cols)+k] = c
		}
		return e.newTable(names, cols, a.pos)
	}
}

// numericColumn reports whether every cell of c that is not empty is a
// real number, and at least one is.
func numericColumn(c []val) bool {
	seen := false
	for _, x := range c {
		if isEmpty(x) {
			continue
		}
		if _, ok := x.(num); !ok {
			return false
		}
		seen = true
	}
	return seen
}

// fnTotals adds a row: the sum of each numeric column, empty elsewhere, and
// "total" in the first column when that column is not numeric.
func fnTotals(e *evaluator, a args) (val, *Error) {
	t, err := a.table(0)
	if err != nil {
		return nil, err
	}
	cols := make([][]val, len(t.cols))
	for j, c := range t.cols {
		var cell val = empty
		switch {
		case numericColumn(c):
			if cell, err = e.callBuiltin(e.funcs["sum"], []val{c}, a.pos); err != nil {
				return nil, err
			}
		case j == 0:
			cell = "total"
		}
		cols[j] = append(append(make([]val, 0, len(c)+1), c...), cell)
	}
	return e.newTable(t.names, cols, a.pos)
}

var describeStats = []string{"count", "empty", "mean", "stdev", "min", "q1", "median", "q3", "max"}

// fnDescribeTable summarises each numeric column, one column per input column
// beside a "stat" column naming the rows. stdev is the sample one, empty
// for fewer than two values; q1 and q3 are quantile(v, 0.25) and 0.75.
func fnDescribeTable(e *evaluator, a args) (val, *Error) {
	t, err := a.table(0)
	if err != nil {
		return nil, err
	}
	stat := make([]val, len(describeStats))
	for i, s := range describeStats {
		stat[i] = s
	}
	names := []string{"stat"}
	cols := [][]val{stat}
	for j, c := range t.cols {
		if !numericColumn(c) {
			continue
		}
		col, err := e.describeColumn(c, a)
		if err != nil {
			return nil, err
		}
		names = append(names, t.names[j])
		cols = append(cols, col)
	}
	if len(cols) == 1 {
		return nil, errf(codeType, a.pos, "describe: the table has no numeric column")
	}
	for s := 1; contains(names[1:], "stat"); s++ {
		names[0] = "stat_" + strconv.Itoa(s)
		if !contains(names[1:], names[0]) {
			break
		}
	}
	return e.newTable(names, cols, a.pos)
}

func (e *evaluator) describeColumn(c []val, a args) ([]val, *Error) {
	var data []val
	for _, x := range c {
		if !isEmpty(x) {
			data = append(data, x)
		}
	}
	call := func(name string, extra ...val) (val, *Error) {
		return e.callBuiltin(e.funcs[name], append([]val{data}, extra...), a.pos)
	}
	out := []val{exactInt64(int64(len(data))), exactInt64(int64(len(c) - len(data)))}
	for _, s := range describeStats[2:] {
		var v val = empty
		var err *Error
		switch s {
		case "mean", "min", "median", "max":
			v, err = call(s)
		case "stdev":
			if len(data) > 1 {
				v, err = call("stdev")
			}
		case "q1":
			v, err = call("quantile", exactNum(big.NewRat(1, 4)))
		case "q3":
			v, err = call("quantile", exactNum(big.NewRat(3, 4)))
		}
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// countRows is count's table form: the rows, or the rows r where pred(r).
func countRows(e *evaluator, t *tableVal, a args) (val, *Error) {
	if len(a.v) == 1 {
		return exactInt64(int64(t.rows())), nil
	}
	f, err := a.fn(1)
	if err != nil {
		return nil, err
	}
	n := int64(0)
	for i := range t.rows() {
		hit, err := e.predicate(f, rowVal{t: t, i: i}, a)
		if err != nil {
			return nil, err
		}
		if hit {
			n++
		}
	}
	return exactInt64(n), nil
}

func fnIsEmpty(_ *evaluator, a args) (val, *Error) {
	return isEmpty(a.v[0]), nil
}

// sortTable is sort's table form: by one column, "asc" (the default) or
// "desc", stable, empty cells last either way.
func sortTable(e *evaluator, a args) (val, *Error) {
	t := a.v[0].(*tableVal)
	idx, _, err := e.columnsArg(a, t, 1)
	if err != nil {
		return nil, err
	}
	if len(idx) != 1 {
		return nil, errf(codeType, a.pos, "sort takes one column; for several, sort by the least important first (the sort is stable)")
	}
	desc := false
	if len(a.v) == 3 {
		o, err := a.str(2)
		if err != nil {
			return nil, err
		}
		switch o {
		case "asc":
		case "desc":
			desc = true
		default:
			return nil, errf(codeDomain, a.pos, "sort: order %q is not \"asc\" or \"desc\"", o)
		}
	}
	keys := t.cols[idx[0]]
	var present []val
	for _, k := range keys {
		if !isEmpty(k) {
			present = append(present, k)
		}
	}
	less, err := orderOf(present, a)
	if err != nil {
		return nil, err
	}
	if err := e.charge(sortCost(len(keys)), a.pos); err != nil {
		return nil, err
	}
	order := make([]int, len(keys))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(x, y int) bool {
		kx, ky := keys[order[x]], keys[order[y]]
		switch {
		case isEmpty(kx):
			return false
		case isEmpty(ky):
			return true
		case desc:
			return less(ky, kx)
		}
		return less(kx, ky)
	})
	return e.takeRows(t, order, a.pos)
}

// orderOf returns the comparison for keys that are all numbers, all
// strings, all dates or all datetimes, refusing any other mix.
func orderOf(keys []val, a args) (func(x, y val) bool, *Error) {
	allNum, allStr, allDate, allTime := true, true, true, true
	for _, k := range keys {
		if isComplex(k) {
			return nil, errf(codeType, a.pos, "sort: complex numbers have no order")
		}
		_, isNum := k.(num)
		_, isStr := k.(string)
		_, isDate := k.(dateVal)
		_, isTime := k.(timeVal)
		allNum, allStr, allDate, allTime = allNum && isNum, allStr && isStr, allDate && isDate, allTime && isTime
	}
	switch {
	case allNum:
		return func(x, y val) bool { return cmpNum(x.(num), y.(num)) < 0 }, nil
	case allStr:
		return func(x, y val) bool { return x.(string) < y.(string) }, nil
	case allDate:
		return func(x, y val) bool { return x.(dateVal).days < y.(dateVal).days }, nil
	case allTime:
		return func(x, y val) bool { return x.(timeVal).t.Before(y.(timeVal).t) }, nil
	}
	return nil, errf(codeType, a.pos, "sort: the column mixes kinds of value; it needs all numbers, all strings, all dates or all datetimes")
}
