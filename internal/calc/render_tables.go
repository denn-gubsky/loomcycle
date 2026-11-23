package calc

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"strconv"
	"strings"
)

// The output formats of Request.Format. They change only how a table or a
// matrix is written in Response.Text; Result is always JSON.
const (
	FormatJSON     = "json"
	FormatMarkdown = "markdown"
	FormatCSV      = "csv"
)

func validFormat(f string) bool {
	return f == "" || f == FormatJSON || f == FormatMarkdown || f == FormatCSV
}

// renderResult writes the top-level result in format: a table or a matrix
// as a Markdown table or CSV when asked, everything else as renderText.
func renderResult(w *capWriter, v val, digits int, format string) {
	if format == FormatMarkdown || format == FormatCSV {
		if names, rows, ok := gridOf(v); ok {
			if format == FormatMarkdown {
				renderMarkdown(w, names, rows, digits)
			} else {
				renderCSV(w, names, rows, digits)
			}
			return
		}
	}
	renderText(w, v, digits, true)
}

// gridOf returns a table's or a matrix's header and rows. A matrix has no
// header (names nil); a list that is not a matrix is not a grid.
func gridOf(v val) (names []string, rows func(i int) []val, ok bool) {
	switch v := v.(type) {
	case *tableVal:
		return v.names, func(i int) []val {
			if i >= v.rows() {
				return nil
			}
			return v.cellsOf(i)
		}, true
	case []val:
		// Only a rectangular list of rows of scalars; a ragged list or a
		// deeper nesting stays JSON text.
		if s := shape(v); len(s) != 2 || s[1] == 0 {
			return nil, nil, false
		}
		return nil, func(i int) []val {
			if i >= len(v) {
				return nil
			}
			return v[i].([]val)
		}, true
	}
	return nil, nil, false
}

// cellText is a cell as plain text: a string bare, an empty cell as "".
func cellText(v val, digits int) string {
	if s, ok := v.(string); ok {
		return s
	}
	if isEmpty(v) {
		return ""
	}
	w := &capWriter{limit: MaxOutputBytes}
	renderText(w, v, digits, true)
	return w.b.String()
}

var mdEscaper = strings.NewReplacer("|", `\|`, "\r\n", " ", "\n", " ", "\r", " ")

// renderMarkdown writes a pipe table. A matrix's header numbers its
// columns from 1, since Markdown requires a header row.
func renderMarkdown(w *capWriter, names []string, rows func(int) []val, digits int) {
	first := rows(0)
	if names == nil {
		names = make([]string, len(first))
		for j := range names {
			names[j] = strconv.Itoa(j + 1)
		}
	}
	line := func(cells []string) {
		w.write("| " + strings.Join(cells, " | ") + " |\n")
	}
	hdr := make([]string, len(names))
	for j, n := range names {
		hdr[j] = mdEscaper.Replace(n)
	}
	line(hdr)
	sep := make([]string, len(names))
	for j := range sep {
		sep[j] = "---"
	}
	line(sep)
	for i := 0; !w.over; i++ {
		r := rows(i)
		if r == nil {
			break
		}
		cells := make([]string, len(r))
		for j, c := range r {
			cells[j] = mdEscaper.Replace(cellText(c, digits))
		}
		line(cells)
	}
}

// renderCSV writes RFC 4180 CSV: a header row for a table, none for a
// matrix.
func renderCSV(w *capWriter, names []string, rows func(int) []val, digits int) {
	var b bytes.Buffer
	cw := csv.NewWriter(&b)
	flush := func() {
		cw.Flush()
		w.write(b.String())
		b.Reset()
	}
	if names != nil {
		_ = cw.Write(names) // a bytes.Buffer never fails
		flush()
	}
	for i := 0; !w.over; i++ {
		r := rows(i)
		if r == nil {
			break
		}
		cells := make([]string, len(r))
		for j, c := range r {
			cells[j] = cellText(c, digits)
		}
		_ = cw.Write(cells)
		flush()
	}
}

// renderTableJSON writes a table as JSON text, spaced like a list:
// {"columns": ["a", "b"], "rows": [[1, "x"], [2, null]]}.
func renderTableJSON(w *capWriter, t *tableVal, digits int) {
	w.write(`{"columns": [`)
	for j, n := range t.names {
		if j > 0 {
			w.write(", ")
		}
		q, _ := json.Marshal(n)
		w.write(string(q))
	}
	w.write(`], "rows": [`)
	for i := 0; i < t.rows() && !w.over; i++ {
		if i > 0 {
			w.write(", ")
		}
		w.write("[")
		for j := range t.cols {
			if j > 0 {
				w.write(", ")
			}
			renderText(w, t.cols[j][i], digits, false)
		}
		w.write("]")
	}
	w.write("]}")
}

// renderRowJSON writes a row as a JSON object in column order.
func renderRowJSON(w *capWriter, r rowVal, digits int) {
	w.write("{")
	for j, n := range r.t.names {
		if j > 0 {
			w.write(", ")
		}
		q, _ := json.Marshal(n)
		w.write(string(q) + ": ")
		renderText(w, r.cell(j), digits, false)
	}
	w.write("}")
}

// tableResult is a table in Response.Result.
type tableResult struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
}

func tableJSON(t *tableVal, digits int) tableResult {
	out := tableResult{Columns: t.names, Rows: make([][]any, t.rows())}
	for i := range out.Rows {
		row := make([]any, len(t.cols))
		for j := range t.cols {
			row[j] = toJSON(t.cols[j][i], digits)
		}
		out.Rows[i] = row
	}
	return out
}

// orderedObject is a JSON object that keeps its keys in column order
// (a Go map would sort them).
type orderedObject struct {
	keys []string
	vals []any
}

func (o orderedObject) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		vb, err := json.Marshal(o.vals[i])
		if err != nil {
			return nil, err
		}
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func rowJSON(r rowVal, digits int) orderedObject {
	o := orderedObject{keys: r.t.names, vals: make([]any, len(r.t.names))}
	for j := range r.t.names {
		o.vals[j] = toJSON(r.cell(j), digits)
	}
	return o
}
