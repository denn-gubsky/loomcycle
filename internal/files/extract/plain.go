package extract

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// maxPlainBytes caps a text, Markdown or CSV file an extractor reads whole.
const maxPlainBytes = 64 << 20

// CSV reads comma- or tab-separated values into one table, the first record
// as its header.
type CSV struct{}

func (CSV) Match(mediaType, name string) bool {
	return matchType(mediaType, name, []string{"text/csv", "text/tab-separated-values", "application/csv"}, []string{".csv", ".tsv"})
}

func (CSV) Extract(ctx context.Context, r io.ReaderAt, size int64, sink Sink, opts Options) (Report, error) {
	rep := Report{Format: "csv"}
	data, err := readAllBounded(r, size)
	if err != nil {
		return rep, err
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	cr := csv.NewReader(bytes.NewReader(data))
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	cr.ReuseRecord = true
	if strings.HasSuffix(strings.ToLower(opts.Name), ".tsv") || tabSeparated(data) {
		cr.Comma = '\t'
		rep.Format = "tsv"
	}
	maxRows := opts.maxRows()
	var rows [][]string
	total := 0
	for {
		if total%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return rep, err
			}
		}
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return rep, fmt.Errorf("extract: malformed CSV: %w", err)
		}
		total++
		if total > maxRows {
			continue
		}
		row := make([]string, 0, min(len(rec), maxSheetColumns))
		for i, f := range rec {
			if i >= maxSheetColumns {
				break
			}
			row = append(row, cell(f))
		}
		rows = append(rows, row)
	}
	w := newWriter(ctx, sink, opts)
	out := heading(1, title(opts.Name, "Table"))
	if len(rows) == 0 {
		out += "\n\n_(no rows)_"
	} else {
		rep.Tables = 1
		out += "\n\n" + table(rows)
		if total > len(rows) {
			out += fmt.Sprintf("\n\n[+%d rows omitted]", total-len(rows))
			rep.note("%d of %d rows shown", len(rows), total)
		}
	}
	if err := w.text(out); err != nil {
		rep.finish(w)
		return rep, err
	}
	rep.finish(w)
	return rep, nil
}

// tabSeparated reports whether the first line holds tabs and no commas.
func tabSeparated(data []byte) bool {
	line, _, _ := bytes.Cut(data, []byte("\n"))
	return bytes.IndexByte(line, '\t') >= 0 && bytes.IndexByte(line, ',') < 0
}

// Text reads plain text and Markdown. Plain text is escaped line by line
// under a "#" heading, so nothing in it reads as Markdown structure; a
// Markdown file keeps its structure, with a "#" heading added when it does
// not open with one, and is Neutralized: no raw HTML, no loom metadata, no
// image embed of a volume reference.
type Text struct{}

func (Text) Match(mediaType, name string) bool {
	return matchType(mediaType, name, []string{"text/plain", "text/markdown", "text/x-markdown"}, []string{".txt", ".md", ".markdown", ".text"})
}

func (Text) Extract(ctx context.Context, r io.ReaderAt, size int64, sink Sink, opts Options) (Report, error) {
	rep := Report{Format: "text"}
	data, err := readAllBounded(r, size)
	if err != nil {
		return rep, err
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(data) {
		return rep, refusedf("the file is not UTF-8 text")
	}
	text := normalizeNewlines(string(data))
	w := newWriter(ctx, sink, opts)
	var out string
	lower := strings.ToLower(opts.Name)
	if strings.HasSuffix(lower, ".md") || strings.HasSuffix(lower, ".markdown") {
		rep.Format = "markdown"
		out = Neutralize(strings.TrimLeft(text, "\n"), true)
		if !strings.HasPrefix(out, "# ") {
			out = heading(1, title(opts.Name, "Document")) + "\n\n" + out
		}
	} else {
		var b strings.Builder
		b.WriteString(heading(1, title(opts.Name, "Text")))
		sc := bufio.NewScanner(strings.NewReader(text))
		sc.Buffer(make([]byte, 64<<10), maxPlainBytes)
		blank := true
		for sc.Scan() {
			l := strings.TrimRight(escapeLine(sc.Text()), " \t")
			if l == "" {
				blank = true
				continue
			}
			if blank {
				b.WriteString("\n\n")
			} else {
				b.WriteString("\n")
			}
			b.WriteString(l)
			blank = false
		}
		out = b.String()
	}
	if err := w.text(out); err != nil {
		rep.finish(w)
		return rep, err
	}
	rep.finish(w)
	return rep, nil
}

// ImageFile "extracts" an image: a one-section document that embeds it, so
// importing it yields one image chunk.
type ImageFile struct{}

func (ImageFile) Match(mediaType, name string) bool {
	return matchType(mediaType, name, []string{"image/png", "image/jpeg", "image/gif", "image/webp", "image/bmp"},
		[]string{".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp"})
}

func (ImageFile) Extract(ctx context.Context, r io.ReaderAt, size int64, sink Sink, opts Options) (Report, error) {
	rep := Report{Format: "image"}
	data, err := readAllBounded(r, size)
	if err != nil {
		return rep, err
	}
	w := newWriter(ctx, sink, opts)
	name := title(opts.Name, "Image")
	md, err := w.image(data, name)
	if err != nil {
		return rep, err
	}
	if err := w.text(heading(1, name) + "\n\n" + md); err != nil {
		rep.finish(w)
		return rep, err
	}
	rep.finish(w)
	return rep, nil
}

func readAllBounded(r io.ReaderAt, size int64) ([]byte, error) {
	if size > maxPlainBytes {
		return nil, refusedf("the file is over %d bytes", maxPlainBytes)
	}
	return io.ReadAll(io.NewSectionReader(r, 0, size))
}
