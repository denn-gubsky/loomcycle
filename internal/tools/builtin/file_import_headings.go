package builtin

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// importNoRecordNote is what an import says when it reads an inbox file whose
// bytes match no capture record: the file changed after it was captured, so
// nothing about where it came from can be recorded with the runtime's
// authority.
const importNoRecordNote = "file changed since capture: file-level provenance not recorded"

// ImportHeading is one entry of File op=import's headings: structure the
// runtime adds to the file it imports, so a caller can turn a PDF export whose
// structure lives only in its numbering into a tree without changing the file
// — and so without losing the file's capture record, which holds only while
// the bytes still hash to it. An entry can also drop a heading the file
// already has, such as a PDF extract's "## Page N": adding can only make
// boundaries, so without it every page break stays a section boundary.
//
// The caller sends line numbers and titles, never document text: the body is
// the file's.
type ImportHeading struct {
	// Line is 1-based, over the file as read.
	Line int `json:"line"`
	// Level is the heading level, 2–6. Level 1 is the document's root heading.
	// A drop takes none.
	Level int `json:"level"`
	// As is heading (the line becomes the heading), marker (a heading with
	// Title is inserted before the line, which stays body text) or drop (the
	// line, already a heading, is removed).
	As string `json:"as"`
	// Lines is how many lines a heading spans when its title wraps (default
	// 1). They are joined with one space.
	Lines int `json:"lines"`
	// Title is a marker's heading text: one line.
	Title string `json:"title"`
}

const (
	importHeadingAsHeading = "heading"
	importHeadingAsMarker  = "marker"
	importHeadingAsDrop    = "drop"
	importHeadingsMax      = 10000
	importHeadingTitleMax  = 300
	// importHeadingSpanMax bounds a wrapped title: a heading is a line or a
	// few, never a paragraph.
	importHeadingSpanMax = 10
)

// applyImportHeadings returns text with hs applied, or an error naming the
// first entry it refuses; nothing is applied unless every entry is valid.
//
// The result differs from text only by:
//   - a heading's line prefixed with "#"×level and a space;
//   - a heading's spanned lines joined into it with one space (a carriage
//     return ending a joined line is dropped with its line break);
//   - a marker's line "#"×level, a space and its title, inserted before its
//     line;
//   - a dropped line, which is a heading line (see isATXHeadingLine), removed
//     with the line break that ends it (for a last line with none, the one
//     before it).
//
// so removing the marker lines and the prefixes, splitting each joined span
// at the recorded points, and putting each dropped line back at its number
// gives text back.
func applyImportHeadings(text string, hs []ImportHeading) (string, error) {
	if len(hs) > importHeadingsMax {
		return "", fmt.Errorf("%d entries, at most %d", len(hs), importHeadingsMax)
	}
	lines := strings.Split(text, "\n")
	n := len(lines)
	if strings.HasSuffix(text, "\n") {
		n-- // the empty string after the final line break is not a line
	}
	at := make(map[int]ImportHeading, len(hs))
	prevEnd := 0
	for i, h := range hs {
		end, err := checkImportHeading(h, lines, n)
		if err != nil {
			return "", fmt.Errorf("entry %d (line %d): %w", i+1, h.Line, err)
		}
		if h.Line <= prevEnd {
			return "", fmt.Errorf("entry %d (line %d): not after the previous entry, which ends at line %d; give entries in increasing line order, without overlapping a heading's lines", i+1, h.Line, prevEnd)
		}
		at[h.Line] = h
		prevEnd = end
	}
	var b strings.Builder
	b.Grow(len(text) + len(hs)*16)
	// The result is the kept lines joined by line breaks, so a dropped line
	// takes exactly one break with it, wherever it is.
	wrote := false
	for i := 0; i < len(lines); i++ {
		h, ok := at[i+1]
		if ok && h.As == importHeadingAsDrop {
			continue
		}
		if wrote {
			b.WriteByte('\n')
		}
		wrote = true
		if !ok {
			b.WriteString(lines[i])
			continue
		}
		b.WriteString(strings.Repeat("#", h.Level))
		b.WriteByte(' ')
		if h.As == importHeadingAsMarker {
			b.WriteString(strings.TrimSpace(h.Title))
			b.WriteByte('\n')
			b.WriteString(lines[i])
			continue
		}
		span := max(h.Lines, 1)
		for j := range span {
			if j > 0 {
				b.WriteByte(' ')
			}
			l := lines[i+j]
			if j < span-1 {
				l = strings.TrimSuffix(l, "\r")
			}
			b.WriteString(l)
		}
		i += span - 1
	}
	return b.String(), nil
}

// checkImportHeading checks one entry against the file's n lines and returns
// the last line it occupies.
func checkImportHeading(h ImportHeading, lines []string, n int) (int, error) {
	if h.As == importHeadingAsDrop {
		return checkImportHeadingDrop(h, lines, n)
	}
	if h.Level < 2 || h.Level > 6 {
		return 0, fmt.Errorf("level %d is not 2 to 6 (1 is the document's own title)", h.Level)
	}
	if h.Line < 1 || h.Line > n {
		return 0, fmt.Errorf("the file has lines 1 to %d", n)
	}
	switch h.As {
	case importHeadingAsHeading:
		if h.Title != "" {
			return 0, errors.New("a heading is the line itself and takes no title; use as marker to insert a title")
		}
		span := h.Lines
		if span == 0 {
			span = 1
		}
		if span < 1 || span > importHeadingSpanMax {
			return 0, fmt.Errorf("lines %d is not 1 to %d", h.Lines, importHeadingSpanMax)
		}
		end := h.Line + span - 1
		if end > n {
			return 0, fmt.Errorf("spans to line %d, past the file's last line %d", end, n)
		}
		for l := h.Line; l <= end; l++ {
			if strings.TrimSpace(lines[l-1]) == "" {
				return 0, fmt.Errorf("line %d is blank, and a heading needs text", l)
			}
		}
		return end, nil
	case importHeadingAsMarker:
		if h.Lines > 1 {
			return 0, errors.New("a marker is inserted before one line and takes no lines")
		}
		if err := checkImportHeadingTitle(h.Title); err != nil {
			return 0, err
		}
		return h.Line, nil
	}
	return 0, fmt.Errorf("as %q is not heading, marker or drop", h.As)
}

// checkImportHeadingDrop checks a drop entry: it names one line that is
// already a heading, and nothing else. Only a heading may be dropped so that
// the body stays the file's own text, less lines that were structure.
func checkImportHeadingDrop(h ImportHeading, lines []string, n int) (int, error) {
	switch {
	case h.Level != 0:
		return 0, errors.New("a drop removes a heading and takes no level")
	case h.Title != "":
		return 0, errors.New("a drop removes a heading and takes no title")
	case h.Lines != 0:
		return 0, errors.New("a drop removes one line and takes no lines")
	case h.Line < 1 || h.Line > n:
		return 0, fmt.Errorf("the file has lines 1 to %d", n)
	case !isATXHeadingLine(lines[h.Line-1]):
		return 0, fmt.Errorf("line %d is not a heading line; drop removes an existing heading, such as a PDF's \"## Page 3\"", h.Line)
	}
	return h.Line, nil
}

// isATXHeadingLine reports whether l is a Markdown ATX heading line, as
// CommonMark has it: up to 3 spaces of indentation, 1 to 6 "#", then a space,
// a tab or the end of the line (a carriage return ending a CRLF line counts
// as the end).
func isATXHeadingLine(l string) bool {
	l = strings.TrimSuffix(l, "\r")
	indent := len(l) - len(strings.TrimLeft(l, " "))
	if indent > 3 {
		return false
	}
	l = l[indent:]
	hashes := len(l) - len(strings.TrimLeft(l, "#"))
	if hashes < 1 || hashes > 6 {
		return false
	}
	return hashes == len(l) || l[hashes] == ' ' || l[hashes] == '\t'
}

// checkImportHeadingTitle refuses a marker title that is not one short line of
// text: it becomes a heading line of the document, so a line break or a
// control character would change the text around it.
func checkImportHeadingTitle(t string) error {
	if strings.TrimSpace(t) == "" {
		return errors.New("a marker needs a title")
	}
	if c := utf8.RuneCountInString(t); c > importHeadingTitleMax {
		return fmt.Errorf("the title has %d characters, at most %d", c, importHeadingTitleMax)
	}
	for _, r := range t {
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return errors.New("the title must be one line, without control characters")
		}
	}
	return nil
}
