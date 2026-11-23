---
name: Calc/data
kind: section
description: "Calc data by reference — from reads named tables from a volume file, a Memory key or a Document chunk, to writes the result to one; the reference forms, the file formats read and written, which of your tools each needs, and the limits."
---
A table of thousands of rows should not pass through you twice. Name where
it is stored in `from`, compute on it, and write a large result with `to`
instead of reading it back. Only inside a run: called any other way, `from`
and `to` are refused (`bad_request`).

## Reading: `from`

`from` maps a name to a reference. Each source is bound to its name before
the expression runs, as a `vars` entry would be:

```
{"expr": "sum(t.amount)", "from": {"t": "volume:sales.csv"}}
```

A name may be given in `vars` or in `from`, not both. At most 16 sources per
call.

## Writing: `to`

`to` is one reference. The result is written there instead of being
returned; the call returns where it went, the format, `type`, `shape`,
`exact` and `bytes`, and a `text` line saying so — never the content. Read it
back later with `from` if you need it, or show a person a short summary
(`head(t, 5)`, a total) in a separate call.

Writing replaces what is there: the whole file, the Memory value, or the
chunk's whole body.

## References and the tool each needs

| Reference | What it names | Reading needs | Writing needs |
|---|---|---|---|
| `volume:<path>` | a file on your default volume | `Read` | `Write` |
| `volume:<name>:<path>` | a file on the volume `<name>` | `Read` | `Write` |
| `memory:<scope>/<key>` | a Memory value; scope `agent`, `user` or `tenant` | `Memory` | `Memory` |
| `document:<scope>/<chunk_id>` | a Document chunk's body | `Document` | `Document` |

Calc reaches nothing you could not reach yourself: you must have the tool in
the table, the scope must be one you may use, a path stays inside the volume,
a read-only volume is not written, and any hook on that tool is asked first.
A refusal names the reference and what was missing; it carries no data. A
workspace reference is refused: copy the file onto a volume first.

## Formats read

- **CSV** (`.csv`) and **TSV** (`.tsv`, `.tab`): the first line names the
  columns; a quoted field may hold the separator. A blank field is an empty
  cell.
- **JSON** (`.json`): `{"columns": [...], "rows": [[...]]}` or an array of
  objects (a table), an array (a list), an array of arrays (a matrix), or a
  single value.
- **Markdown** (`.md`, `.markdown`, and every `document:` chunk): the first
  pipe table in the text — a header row, a `---` row, then the rows. `\|`
  in a cell is a literal `|`. A short row is padded with empty cells; a long
  one is refused (`shape`).
- With no known extension (a Memory key, `volume:data`), the first bytes
  decide: `[` or `{` is JSON, `|` is Markdown, a tab in the first line and no
  comma is TSV, anything else CSV.

A field that reads as a number (`19.99`, `-1e3`, `1_000`) is that exact
number; anything else stays a string. Text is never run as an expression:
a cell holding `2^10` or `=sum(a)` is that string. Files must be UTF-8.

## Formats written

`format` chooses (`json`, `markdown` or `csv`). Without it, the extension of
the target does (`.csv`, `.json`, `.md`), then a `document:` chunk is
Markdown and anything else JSON. A `format` the name contradicts (`csv` to
`report.json`) is refused, and so is a `.tsv` target.

- A **table** or a **matrix** is written in any of the three. A table's CSV
  and Markdown have a header row; a matrix's CSV has none.
- Any other result — a number, a list, a string — only as JSON.
- Numbers are written as `text` shows them: exact ones in full, floats to
  `digits`.

## Limits

- **16 MB** read over every source together, and 16 MB written. A file's
  size is checked before it is read; a larger one is refused, never cut.
- **1,000,000 cells** over every source together (`list_too_long`), refused
  while reading. A table you read may be that large; a table your expression
  builds from it (`select`, `where`, `sort` …) is held to 100,000 cells
  again, so reduce first: `sum(t.amount)`, `groupby`, or `where` to the rows
  you need.
- Reading costs a step per cell, apart from the expression's own 1,000,000
  steps, and waits for a slot of the shared pool (`Calc/limits`).
- Memory values and Document bodies have their own size limits; a large
  table goes to a volume file.

## Errors

- `bad_data` — the source is not a table or value in its format: invalid
  JSON or CSV quoting, no Markdown table, two columns of one name, or not
  UTF-8. Fix the file, or name a file of the format you meant.
- `bad_request` — `from` or `to` outside a run, a malformed reference, more
  than 16 sources, a name in both `vars` and `from`, or a `format` the
  target's name contradicts.
- `type` — a list or a number written as CSV or Markdown.
- `too_large`, `list_too_long` — past a limit above.

## Examples

These run with a file `sales.csv` on your default volume holding
`region,amount` and the rows `EU,120.10`, `US,300`, `EU,79.90`.

A total from a file:

```json
{"expr": "sum(t.amount)", "from": {"t": "volume:sales.csv"}}
```

Result: 500

Totals by region, written to a CSV file instead of returned:

```json
{"expr": "groupby(t, \"region\", \"total\", g -> sum(g.amount))", "from": {"t": "volume:sales.csv"}, "to": "volume:by-region.csv"}
```

Result: wrote a table of 2 rows and 2 columns, 27 bytes of csv, to volume:by-region.csv

The same as a Markdown table for a report:

```json
{"expr": "sort(t, \"amount\", \"desc\")", "from": {"t": "volume:sales.csv"}, "to": "volume:sales-sorted.md"}
```

Result: wrote a table of 3 rows and 2 columns, 76 bytes of markdown, to volume:sales-sorted.md
