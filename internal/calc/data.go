package calc

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"unicode/utf8"
)

// Data by reference (RFC GY §5): the Calc tool reads named data from a store
// (`from`) and writes the result to one (`to`). This file is the part that
// needs no store: turning bytes into a value, and a value into bytes. What is
// read is data — a table, a list or a scalar — and never expression text: no
// cell is parsed as anything but a number literal or kept as a string.

// FormatTSV is tab-separated text, read only. The write formats are the
// Request.Format ones.
const FormatTSV = "tsv"

// Data is one source read by reference: its bytes and how they are written,
// FormatCSV, FormatTSV, FormatJSON or FormatMarkdown.
type Data struct {
	Bytes  []byte
	Format string
}

// SniffFormat guesses the format of data whose name says nothing: JSON when
// it opens with `[` or `{`, a Markdown table when it opens with `|`, TSV when
// its first line holds a tab and no comma, else CSV.
func SniffFormat(b []byte) string {
	t := bytes.TrimLeft(bytes.TrimPrefix(b, utf8BOM), " \t\r\n")
	switch {
	case len(t) == 0:
		return FormatCSV
	case t[0] == '[' || t[0] == '{':
		return FormatJSON
	case t[0] == '|':
		return FormatMarkdown
	}
	line, _, _ := bytes.Cut(t, []byte{'\n'})
	if bytes.IndexByte(line, '\t') >= 0 && bytes.IndexByte(line, ',') < 0 {
		return FormatTSV
	}
	return FormatCSV
}

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// loadData binds every source in name order, so an error names the same
// source on every call. All of them together are held to MaxRefSources,
// MaxRefBytes (checked before anything is parsed) and MaxRefCells; parsing
// takes one slot of the process-wide gate, waiting under the evaluation's
// deadline, and is charged one step per cell on top of MaxSteps.
func (e *evaluator) loadData(data map[string]Data, vars map[string]json.RawMessage) *Error {
	if len(data) == 0 {
		return nil
	}
	if len(data) > MaxRefSources {
		return errf(codeBadRequest, -1, "from names %d sources; the limit is %d", len(data), MaxRefSources).
			withHint("read fewer sources per call, or combine them into one file")
	}
	names := make([]string, 0, len(data))
	size := 0
	for k, d := range data {
		names = append(names, k)
		size += len(d.Bytes)
	}
	sort.Strings(names)
	if size > MaxRefBytes {
		return errf(codeTooLarge, -1, "from reads %d bytes; the limit is %d", size, MaxRefBytes).
			withHint("read less data per call, or split the file")
	}
	for _, name := range names {
		if !isIdentifier(name) {
			return errf(codeBadVar, -1, "from name %q is not a valid name", name).withHint("names are letters, digits and _, starting with a letter or _")
		}
		if err := checkName(name, -1); err != nil {
			return err
		}
		if _, dup := vars[name]; dup {
			return errf(codeBadRequest, -1, "%s is named in both vars and from", name).withHint("give each name once")
		}
	}
	if e.gate != nil {
		if err := e.gate.Acquire(e.ctx, 1); err != nil {
			return ctxErr(err, -1)
		}
		defer e.gate.Release(1)
	}
	budget := MaxRefCells
	for _, name := range names {
		v, cells, err := e.loadOne(name, data[name], budget)
		if err != nil {
			return err
		}
		budget -= cells
		e.global.vars[name] = v
	}
	return nil
}

// loadOne reads one source of at most budget cells and returns its value
// and the cells it counts.
func (e *evaluator) loadOne(name string, d Data, budget int) (val, int, *Error) {
	what := "from " + name
	b := bytes.TrimPrefix(d.Bytes, utf8BOM)
	if !utf8.Valid(b) {
		return nil, 0, errf(codeBadData, -1, "%s is not UTF-8 text", what).withHint("save the file as UTF-8")
	}
	// tick charges each record as it is read, so a large load polls the
	// deadline and is counted, without spending the expression's steps.
	tick := func(n int) *Error {
		e.loadSteps += n
		return e.charge(n, -1)
	}
	var names []string
	var cols [][]val
	var err *Error
	switch d.Format {
	case FormatCSV, FormatTSV:
		sep := ','
		if d.Format == FormatTSV {
			sep = '\t'
		}
		names, cols, err = readCSV(bytes.NewReader(b), sep, budget, what, -1, tick)
	case FormatMarkdown:
		names, cols, err = readMarkdownTable(string(b), budget, what, tick)
	case FormatJSON:
		return e.loadJSON(name, b, budget, tick)
	default:
		return nil, 0, errf(codeBadRequest, -1, "%s: unknown format %q", what, d.Format)
	}
	if err != nil {
		return nil, 0, err
	}
	t, err := makeTable(names, cols, codeBadData, -1, budget)
	if err != nil {
		err.Detail = what + ": " + err.Detail
		return nil, 0, err
	}
	return t, cellCount(names, cols), nil
}

// loadJSON reads a table ({"columns", "rows"} or an array of objects), a
// list, a list of lists or a scalar, as a var of that shape would be read,
// with the by-reference cell limit and the bad_data code.
func (e *evaluator) loadJSON(name string, b []byte, budget int, tick func(int) *Error) (val, int, *Error) {
	what := "from " + name
	raw, jerr := decodeJSONVar(b)
	switch {
	case jerr == errTooDeep:
		return nil, 0, errf(codeBadData, -1, "%s nests deeper than %d levels", what, maxVarDepth)
	case jerr != nil:
		return nil, 0, errf(codeBadData, -1, "%s is not valid JSON: %v", what, jerr)
	}
	var v val
	var err *Error
	switch x := raw.(type) {
	case jsonObj:
		v, err = tableFromObject(name, x, budget)
	case []any:
		if allRecords(x) {
			v, err = tableFromRecords(name, x, budget)
			break
		}
		if n := leaves(x, budget); n > budget {
			return nil, 0, errf(codeListTooLong, -1, "%s holds more than %d values; the limit is %d", what, budget, budget)
		}
		v, err = fromJSON(name, x, 0)
	default:
		v, err = fromJSON(name, x, 0)
	}
	if err != nil {
		// The var readers name a var; this is data read by reference.
		if err.Code == string(codeBadVar) {
			err.Code = string(codeBadData)
		}
		err.Detail = strings.ReplaceAll(err.Detail, "var "+name, what)
		return nil, 0, err
	}
	if err := e.checkSize(v, -1); err != nil {
		return nil, 0, err
	}
	n := max(e.size(v), 1)
	if err := tick(n); err != nil {
		return nil, 0, err
	}
	return v, n, nil
}

// leaves counts the values nested in a decoded JSON array, stopping just
// past limit.
func leaves(x any, limit int) int {
	l, ok := x.([]any)
	if !ok {
		return 1
	}
	n := 0
	for _, it := range l {
		n += leaves(it, limit-n)
		if n > limit {
			return n
		}
	}
	return n
}

// readMarkdownTable reads the first pipe table in md: a header row, a
// delimiter row (`| --- | :-: |`), then body rows until a line that is blank
// or holds no `|`. A cell is a csvCell after `\|` is unescaped; a row with
// fewer cells than the header is padded with empty cells, one with more is
// refused, since dropping a cell would change the data silently.
func readMarkdownTable(md string, limit int, what string, tick func(int) *Error) ([]string, [][]val, *Error) {
	lines := strings.Split(md, "\n")
	start := -1
	for i := 0; i+1 < len(lines); i++ {
		if strings.Contains(lines[i], "|") && isDelimiterRow(lines[i+1]) {
			start = i
			break
		}
	}
	if start < 0 {
		return nil, nil, errf(codeBadData, -1, "%s holds no Markdown table", what).
			withHint("a Markdown table is a header row, a row of --- cells, then the rows, each cell between | characters")
	}
	names := markdownCells(lines[start])
	for j := range names {
		names[j] = strings.TrimSpace(names[j])
	}
	if got := len(markdownCells(lines[start+1])); got != len(names) {
		return nil, nil, errf(codeBadData, -1, "%s: the table's delimiter row has %d cells and its header %d", what, got, len(names))
	}
	cols := make([][]val, len(names))
	for j := range cols {
		cols[j] = []val{}
	}
	for i := start + 2; i < len(lines); i++ {
		ln := strings.TrimRight(lines[i], "\r")
		if strings.TrimSpace(ln) == "" || !strings.Contains(ln, "|") {
			break
		}
		cells := markdownCells(ln)
		if len(cells) > len(names) {
			return nil, nil, errf(codeShape, -1, "%s: line %d has %d cells, the header %d", what, i+1, len(cells), len(names)).
				withHint("escape a | inside a cell as \\|")
		}
		if (len(cols[0])+1)*len(names) > limit {
			return nil, nil, errf(codeListTooLong, -1, "%s: the table has more than %d cells", what, limit)
		}
		for j := range names {
			if j < len(cells) {
				cols[j] = append(cols[j], csvCell(cells[j]))
			} else {
				cols[j] = append(cols[j], empty)
			}
		}
		if err := tick(len(names)); err != nil {
			return nil, nil, err
		}
	}
	return names, cols, nil
}

// markdownCells splits a table row on the `|` that are not escaped, dropping
// the optional leading and trailing pipe, and unescapes `\|` in each cell.
func markdownCells(row string) []string {
	s := strings.TrimSpace(strings.TrimRight(row, "\r"))
	s = strings.TrimPrefix(s, "|")
	if strings.HasSuffix(s, "|") && !strings.HasSuffix(s, `\|`) {
		s = s[:len(s)-1]
	}
	var out []string
	var cur strings.Builder
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\\' && i+1 < len(s) && s[i+1] == '|':
			cur.WriteByte('|')
			i++
		case s[i] == '|':
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(s[i])
		}
	}
	return append(out, cur.String())
}

// isDelimiterRow reports a Markdown table's second row: cells of dashes,
// each optionally starting or ending with a colon.
func isDelimiterRow(row string) bool {
	if !strings.Contains(row, "-") {
		return false
	}
	for _, c := range markdownCells(row) {
		c = strings.TrimSpace(c)
		c = strings.TrimPrefix(c, ":")
		c = strings.TrimSuffix(c, ":")
		if c == "" || strings.Trim(c, "-") != "" {
			return false
		}
	}
	return true
}

// written renders the whole result in format for the caller to store (the
// tool's `to`). A table or a matrix is written in any format; any other
// value only as JSON. The response carries the result's type, exactness and
// shape, never its content.
func written(last val, digits int, format string) (Response, *Error) {
	resp := Response{Type: typeName(last), Exact: allExact(last), Shape: shape(last)}
	// The text rendering is bounded cell by cell, so it is the size probe:
	// a result sharing one long string a million times is refused here,
	// before json.Marshal would expand it.
	w := &capWriter{limit: MaxRefBytes}
	switch format {
	case FormatJSON:
		renderText(w, last, digits, false)
	case FormatMarkdown, FormatCSV:
		names, rows, ok := gridOf(last)
		if !ok {
			return Response{}, errf(codeType, -1, "to: a %s file holds a table or a matrix, and the result is a %s", format, typeName(last)).
				withHint("write it as json, or make the result a table")
		}
		if format == FormatMarkdown {
			renderMarkdown(w, names, rows, digits)
		} else {
			renderCSV(w, names, rows, digits)
		}
	}
	if w.over {
		return Response{}, errf(codeTooLarge, -1, "to: the result passes %d bytes", MaxRefBytes).
			withHint("write a smaller table, or split it with head or where")
	}
	if format != FormatJSON {
		resp.Written = []byte(w.b.String())
		return resp, nil
	}
	b, err := json.Marshal(toJSON(last, digits))
	if err != nil {
		// Unreachable: toJSON holds only JSON-safe values.
		return Response{}, errf(codeType, -1, "to: the result could not be written as JSON")
	}
	if len(b) > MaxRefBytes {
		return Response{}, errf(codeTooLarge, -1, "to: the result passes %d bytes", MaxRefBytes).
			withHint("write a smaller table, or split it with head or where")
	}
	resp.Written = b
	return resp, nil
}
