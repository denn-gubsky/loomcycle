package extract

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// BookMetadata is what a book says about itself (RFC GV D10): an EPUB's OPF,
// later FB2's title-info, MOBI's EXTH and DjVu's annotations. It is data for
// a catalogue record, never text to import, so it travels beside the
// Markdown rather than in it. Every value comes from the file and is
// untrusted: Clean bounds each one and strips its control characters, and
// nothing reads a value as a path, a command argument or markup.
type BookMetadata struct {
	Title        string           `json:"title,omitempty"`
	Authors      []string         `json:"authors,omitempty"`
	Translators  []string         `json:"translators,omitempty"`
	Language     string           `json:"language,omitempty"`
	Date         string           `json:"date,omitempty"` // as the book writes it
	Year         int              `json:"year,omitempty"` // when Date holds one
	Publisher    string           `json:"publisher,omitempty"`
	Identifiers  []BookIdentifier `json:"identifiers,omitempty"`
	Series       string           `json:"series,omitempty"`
	SeriesIndex  string           `json:"series_index,omitempty"`
	Subjects     []string         `json:"subjects,omitempty"`
	Description  string           `json:"description,omitempty"`
	SourceFormat string           `json:"source_format,omitempty"`
}

// BookIdentifier is one identifier of a book: scheme is "isbn", "uuid",
// "asin" or the book's own word for it, lower case.
type BookIdentifier struct {
	Scheme string `json:"scheme,omitempty"`
	Value  string `json:"value"`
}

// Bounds on metadata. A real book's values are a line or two; the
// description is a blurb.
const (
	MaxMetaString      = 500
	MaxMetaDescription = 2000
	MaxMetaList        = 50
	maxMetaScheme      = 32
)

// metaYearRe finds a year in a date as written: "2019", "2019-05-01",
// "May 2019". Four digits not inside a longer number.
var metaYearRe = regexp.MustCompile(`(?:^|[^0-9])([0-9]{4})(?:[^0-9]|$)`)

// Clean is m with every value bounded and stripped, empty values and
// duplicates dropped, and Year derived from Date when it parses; nil when
// nothing is left. The worker's answer passes through it again on the core,
// since a worker is as untrusted as the file.
func (m *BookMetadata) Clean() *BookMetadata {
	if m == nil {
		return nil
	}
	c := &BookMetadata{
		Title:        metaString(m.Title, MaxMetaString),
		Authors:      metaList(m.Authors),
		Translators:  metaList(m.Translators),
		Language:     metaString(m.Language, maxMetaScheme),
		Date:         metaString(m.Date, MaxMetaString),
		Publisher:    metaString(m.Publisher, MaxMetaString),
		Series:       metaString(m.Series, MaxMetaString),
		SeriesIndex:  metaString(m.SeriesIndex, maxMetaScheme),
		Subjects:     metaList(m.Subjects),
		Description:  metaString(m.Description, MaxMetaDescription),
		SourceFormat: metaString(m.SourceFormat, maxMetaScheme),
	}
	if m.Year >= 1 && m.Year <= 9999 {
		c.Year = m.Year
	} else if y := metaYearRe.FindStringSubmatch(c.Date); y != nil {
		c.Year, _ = strconv.Atoi(y[1])
	}
	seen := map[BookIdentifier]bool{}
	for _, id := range m.Identifiers {
		if len(c.Identifiers) == MaxMetaList {
			break
		}
		v := BookIdentifier{Scheme: strings.ToLower(metaString(id.Scheme, maxMetaScheme)), Value: metaString(id.Value, MaxMetaString)}
		if v.Value == "" || seen[v] {
			continue
		}
		seen[v] = true
		c.Identifiers = append(c.Identifiers, v)
	}
	if c.empty() {
		return nil
	}
	return c
}

func (m *BookMetadata) empty() bool {
	return m.Title == "" && len(m.Authors) == 0 && len(m.Translators) == 0 && m.Language == "" && m.Date == "" &&
		m.Year == 0 && m.Publisher == "" && len(m.Identifiers) == 0 && m.Series == "" && m.SeriesIndex == "" &&
		len(m.Subjects) == 0 && m.Description == ""
}

// metaString is one value on one line: control and format characters
// dropped (a bidi override could make a title read as something else),
// whitespace folded, cut to n characters.
func metaString(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r' || r == ' ' || r == ' ':
			return ' '
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == unicode.ReplacementChar:
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if t, cut := cutRunesPlain(s, n); cut {
		return strings.TrimSpace(t)
	}
	return s
}

// metaList is a list of values, each cleaned, empty ones and repeats
// dropped, at most MaxMetaList kept.
func metaList(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range in {
		if len(out) == MaxMetaList {
			break
		}
		if s = metaString(s, MaxMetaString); s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
