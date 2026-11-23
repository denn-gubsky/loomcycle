package calc

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// MaxTableCells bounds one table: rows times columns. A table is built
// only through newTable (and tableFromJSON for vars), which check it, so
// every walk of a table — rendering, keys, joins — is bounded by it.
const MaxTableCells = 100_000

// tableVal is an ordered set of named columns of equal length, held by
// column so `t.price` is the column itself. Like a list it is never
// mutated once built; columns are shared freely between tables.
type tableVal struct {
	names []string
	cols  [][]val // cols[j][i] is row i of column j
}

func (t *tableVal) rows() int  { return len(t.cols[0]) }
func (t *tableVal) cells() int { return len(t.names) * t.rows() }

// col returns the index of the column named name, or -1.
func (t *tableVal) col(name string) int {
	for j, n := range t.names {
		if n == name {
			return j
		}
	}
	return -1
}

// rowVal is row i of a table: what t[i] gives and what a table rule
// (where, addcol, sort keys) receives. It holds the table, not a copy.
type rowVal struct {
	t *tableVal
	i int
}

func (r rowVal) cell(j int) val { return r.t.cols[j][r.i] }

// emptyVal is an empty cell (JSON null, an empty CSV field, a pivot cell
// with no rows). It stays empty through arithmetic and element functions,
// is skipped by the aggregates, and compares false with < > <= >=.
type emptyVal struct{}

var empty val = emptyVal{}

func isEmpty(v val) bool { _, ok := v.(emptyVal); return ok }

// isCell reports whether v may be a table cell: a scalar, never a list, a
// function, a row or a table.
func isCell(v val) bool {
	switch v.(type) {
	case num, cnum, string, bool, dateVal, timeVal, emptyVal:
		return true
	}
	return false
}

// checkTable validates a table's shape: at least one column, unique
// names, columns of one length, at most MaxTableCells cells. code is the
// error code for a name problem (bad_var for vars, column in a function).
func checkTable(names []string, cols [][]val, code errCode, pos int) *Error {
	if len(names) == 0 {
		return errf(code, pos, "a table needs at least one column")
	}
	seen := make(map[string]bool, len(names))
	for j, n := range names {
		if n == "" {
			return errf(code, pos, "column %d has no name", j+1)
		}
		if seen[n] {
			return errf(code, pos, "column %q appears twice", n).withHint("every column of a table needs its own name")
		}
		seen[n] = true
		if len(cols[j]) != len(cols[0]) {
			return errf(codeShape, pos, "column %q has %d rows, column %q has %d", n, len(cols[j]), names[0], len(cols[0]))
		}
	}
	if len(names)*len(cols[0]) > MaxTableCells {
		return errf(codeListTooLong, pos, "the table has %d rows of %d columns, %d cells; the limit is %d", len(cols[0]), len(names), len(names)*len(cols[0]), MaxTableCells).
			withHint("select fewer columns or filter rows first, or split the work into several calls")
	}
	return nil
}

// newTable builds a table, charging one step per cell.
func (e *evaluator) newTable(names []string, cols [][]val, pos int) (*tableVal, *Error) {
	if err := checkTable(names, cols, codeColumn, pos); err != nil {
		return nil, err
	}
	if err := e.charge(len(names)*len(cols[0]), pos); err != nil {
		return nil, err
	}
	return &tableVal{names: names, cols: cols}, nil
}

// unknownColumn is the refusal for a column name the table does not have.
func unknownColumn(name string, names []string, pos int) *Error {
	err := errf(codeColumn, pos, "the table has no column %q", name)
	if s := suggest(name, names); len(s) > 0 {
		return err.withHint("did you mean " + strings.Join(s, ", ") + "?")
	}
	return err.withHint("its columns are " + strings.Join(quoteAll(names, 10), ", "))
}

func quoteAll(names []string, limit int) []string {
	out := make([]string, 0, min(len(names), limit+1))
	for i, n := range names {
		if i == limit {
			out = append(out, "...")
			break
		}
		out = append(out, strconv.Quote(n))
	}
	return out
}

// member is x.name: a table's column or a row's cell.
func (e *evaluator) member(x val, name string, pos int) (val, *Error) {
	switch x := x.(type) {
	case *tableVal:
		j := x.col(name)
		if j < 0 {
			return nil, unknownColumn(name, x.names, pos)
		}
		return x.cols[j], nil
	case rowVal:
		j := x.t.col(name)
		if j < 0 {
			return nil, unknownColumn(name, x.t.names, pos)
		}
		return x.cell(j), nil
	}
	return nil, errf(codeType, pos, "only a table or a row has columns; .%s was applied to a %s", name, typeName(x)).
		withHint("a number ends with digits, not a name: write `x*y`, not `x.y`")
}

// indexTable is t[i] (a row), t[[i, j, ...]] (a table of those rows) or
// t["name"] (a column); r[j] and r["name"] pick a cell of a row.
func (e *evaluator) indexTable(x val, idx val, pos int) (val, *Error) {
	if s, ok := idx.(string); ok {
		return e.member(x, s, pos)
	}
	if r, ok := x.(rowVal); ok {
		v, err := pick(r.t.cellsOf(r.i), idx, pos)
		return v, err
	}
	t := x.(*tableVal)
	if l, ok := idx.([]val); ok {
		if err := e.charge(len(l), pos); err != nil {
			return nil, err
		}
		order := make([]int, len(l))
		for k, iv := range l {
			i, err := rowIndex(t, iv, pos)
			if err != nil {
				return nil, err
			}
			order[k] = i
		}
		return e.takeRows(t, order, pos)
	}
	i, err := rowIndex(t, idx, pos)
	if err != nil {
		return nil, err
	}
	return rowVal{t: t, i: i}, nil
}

func (t *tableVal) cellsOf(i int) []val {
	out := make([]val, len(t.cols))
	for j := range t.cols {
		out[j] = t.cols[j][i]
	}
	return out
}

// rowIndex reads a 1-based row number of t as a 0-based index.
func rowIndex(t *tableVal, idx val, pos int) (int, *Error) {
	n, ok := idx.(num)
	if !ok {
		return 0, errf(codeType, pos, "a table index is a row number or a column name, got a %s", typeName(idx))
	}
	i, ok := smallInt(n, int64(MaxTableCells)+1)
	switch {
	case !ok:
		return 0, errf(codeIndex, pos, "row %s is not a whole number in range", fmtArg(n))
	case i == 0:
		return 0, errf(codeIndex, pos, "row 0 does not exist").withHint("rows are numbered from 1: t[1] is the first row")
	case i < 0 || int(i) > t.rows():
		return 0, errf(codeIndex, pos, "row %d is out of range for a table of %d rows", i, t.rows())
	}
	return int(i - 1), nil
}

// takeRows builds the table of t's rows in order (indexes may repeat).
func (e *evaluator) takeRows(t *tableVal, order []int, pos int) (*tableVal, *Error) {
	if err := e.charge(len(order)*len(t.names), pos); err != nil {
		return nil, err
	}
	if len(order)*len(t.names) > MaxTableCells {
		return nil, errf(codeListTooLong, pos, "the table would have %d cells; the limit is %d", len(order)*len(t.names), MaxTableCells)
	}
	cols := make([][]val, len(t.cols))
	for j, c := range t.cols {
		nc := make([]val, len(order))
		for k, i := range order {
			nc[k] = c[i]
		}
		cols[j] = nc
	}
	return &tableVal{names: t.names, cols: cols}, nil
}

// --- vars ---

// jsonObj is a JSON object with its keys in the order they were written,
// which a table built from an array of objects needs for its columns.
type jsonObj struct {
	keys []string
	m    map[string]any
}

// readJSON decodes one JSON value as decodeVars needs it: numbers as
// json.Number, objects as jsonObj (ordered), arrays as []any. Nesting
// past maxVarDepth is refused here, before the value is built.
func readJSON(dec *json.Decoder, depth int) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return tok, nil
	}
	if depth > maxVarDepth {
		return nil, errTooDeep
	}
	switch d {
	case '[':
		out := []any{}
		for dec.More() {
			v, err := readJSON(dec, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		_, err = dec.Token()
		return out, err
	case '{':
		obj := jsonObj{m: map[string]any{}}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			k := kt.(string) // the decoder only yields a string key here
			v, err := readJSON(dec, depth+1)
			if err != nil {
				return nil, err
			}
			if _, dup := obj.m[k]; !dup {
				obj.keys = append(obj.keys, k)
			}
			obj.m[k] = v
		}
		_, err = dec.Token()
		return obj, err
	}
	return nil, errBadDelim
}

type jsonErr string

func (e jsonErr) Error() string { return string(e) }

const (
	errTooDeep  = jsonErr("nests too deep")
	errBadDelim = jsonErr("unexpected delimiter")
)

func decodeJSONVar(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := readJSON(dec, 0)
	if err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, jsonErr("more than one JSON value")
	}
	if _, err := dec.Token(); err == nil {
		return nil, jsonErr("more than one JSON value")
	}
	return v, nil
}

// tableFromObject reads {"columns": [...], "rows": [[...], ...]}.
func tableFromObject(name string, o jsonObj) (val, *Error) {
	cols, okC := o.m["columns"].([]any)
	rows, okR := o.m["rows"].([]any)
	if len(o.keys) != 2 || !okC || !okR {
		return nil, errf(codeBadVar, -1, "var %s is a JSON object, which has no meaning here", name).
			withHint(`a table is {"columns": ["a", "b"], "rows": [[1, 2], ...]} or an array of objects; otherwise pass a number, string, boolean or list`)
	}
	if len(cols)*max(len(rows), 1) > MaxTableCells {
		return nil, errf(codeListTooLong, -1, "var %s has %d rows of %d columns; the limit is %d cells", name, len(rows), len(cols), MaxTableCells)
	}
	names := make([]string, len(cols))
	for j, c := range cols {
		s, ok := c.(string)
		if !ok {
			return nil, errf(codeBadVar, -1, "var %s: column %d's name is not a string", name, j+1)
		}
		names[j] = s
	}
	data := make([][]val, len(names))
	for j := range data {
		data[j] = make([]val, len(rows))
	}
	for i, r := range rows {
		cells, ok := r.([]any)
		if !ok || len(cells) != len(names) {
			return nil, errf(codeBadVar, -1, "var %s: row %d is not a list of %d cells", name, i+1, len(names))
		}
		for j, c := range cells {
			v, err := cellFromJSON(name, c)
			if err != nil {
				return nil, err
			}
			data[j][i] = v
		}
	}
	if err := checkTable(names, data, codeBadVar, -1); err != nil {
		err.Detail = "var " + name + ": " + err.Detail
		return nil, err
	}
	return &tableVal{names: names, cols: data}, nil
}

// tableFromRecords reads an array of objects: the columns are the keys in
// the order they first appear, and a row missing a key has an empty cell.
func tableFromRecords(name string, recs []any) (val, *Error) {
	var names []string
	index := map[string]int{}
	for _, r := range recs {
		for _, k := range r.(jsonObj).keys {
			if _, ok := index[k]; !ok {
				index[k] = len(names)
				names = append(names, k)
			}
		}
	}
	if len(names)*len(recs) > MaxTableCells {
		return nil, errf(codeListTooLong, -1, "var %s has %d rows of %d columns; the limit is %d cells", name, len(recs), len(names), MaxTableCells)
	}
	data := make([][]val, len(names))
	for j := range data {
		data[j] = make([]val, len(recs))
		for i := range data[j] {
			data[j][i] = empty
		}
	}
	for i, r := range recs {
		o := r.(jsonObj)
		for _, k := range o.keys {
			v, err := cellFromJSON(name, o.m[k])
			if err != nil {
				return nil, err
			}
			data[index[k]][i] = v
		}
	}
	if err := checkTable(names, data, codeBadVar, -1); err != nil {
		err.Detail = "var " + name + ": " + err.Detail
		return nil, err
	}
	return &tableVal{names: names, cols: data}, nil
}

// cellFromJSON reads one table cell: a number (a numeric string is one, as
// in any var), a string, a boolean, or null for an empty cell.
func cellFromJSON(name string, x any) (val, *Error) {
	switch x := x.(type) {
	case nil:
		return empty, nil
	case json.Number, string, bool:
		return fromJSON(name, x, 0)
	}
	return nil, errf(codeBadVar, -1, "var %s: a table cell must be a number, a string, a boolean or null", name)
}

// allRecords reports a non-empty array whose every element is an object.
func allRecords(l []any) bool {
	if len(l) == 0 {
		return false
	}
	for _, x := range l {
		if _, ok := x.(jsonObj); !ok {
			return false
		}
	}
	return true
}
