package extract

import (
	"regexp"
	"strings"
)

// The text a document holds is written into Markdown as text: nothing in it
// may start a heading, a fence, a list, a quote or a loom metadata comment, or
// embed an image. A document line reading "# Рішення" stays a paragraph, and
// one reading "![x](volume:...)" is not a reference for an importer to follow.

var orderedStart = regexp.MustCompile(`^(\d{1,9})([.)])(\s|$)`)

// escapeLine escapes one line of document text: its leading whitespace goes
// (four spaces would make a code block) and a character that would open a
// block construct is backslash-escaped.
func escapeLine(s string) string {
	s = strings.TrimLeft(s, " \t\u00a0")
	s = escapeInline(s)
	if s == "" {
		return s
	}
	if m := orderedStart.FindStringSubmatchIndex(s); m != nil {
		return s[:m[4]] + `\` + s[m[4]:]
	}
	switch s[0] {
	case '#', '>', '`', '~', '|', '=', '-', '*', '+', '_', '<':
		return `\` + s
	}
	return s
}

// escapeInline escapes what would act inside a line: an image opener, and a
// "<" that would start an HTML tag or comment. Every backslash of the text is
// doubled first: one left alone would escape the backslash added here and
// leave the "<" or "[" after it live ("\<img>" is a backslash and a tag).
func escapeInline(s string) string {
	if !strings.ContainsAny(s, "!<\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\':
			b.WriteString(`\\`)
			continue
		case c == '!' && i+1 < len(s) && s[i+1] == '[':
			b.WriteString(`!\`)
			continue
		case c == '<' && i+1 < len(s) && tagStart(s[i+1]):
			b.WriteString(`\<`)
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func tagStart(c byte) bool {
	return c == '/' || c == '!' || c == '?' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// paragraph renders document text as one Markdown paragraph: each line
// escaped, blank lines dropped (a blank line would end the paragraph and let
// the next line start a block of its own).
func paragraph(text string) string {
	var lines []string
	for _, l := range strings.Split(normalizeNewlines(text), "\n") {
		if l = strings.TrimRight(escapeLine(l), " \t"); l != "" {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, "\n")
}

// headingText is text for a heading line: one line, escaped inline.
func headingText(s string) string {
	return strings.TrimSpace(escapeInline(oneLine(s)))
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(normalizeNewlines(s)), " ")
}

func normalizeNewlines(s string) string {
	return strings.NewReplacer("\r\n", "\n", "\r", "\n", "\v", "\n", "\f", "\n", "\u2028", "\n", "\u2029", "\n").Replace(s)
}

// cell renders text for one GFM table cell: one line, escaped inline (its
// backslashes doubled), then its pipes escaped (a "|" would split the cell).
// The pipes go last so the backslash added before each is not doubled.
func cell(s string) string {
	return strings.ReplaceAll(escapeInline(oneLine(s)), "|", `\|`)
}

// table renders rows as a GFM table, the first row as its header. Cells must
// already be rendered with cell (or hold an image embed).
func table(rows [][]string) string {
	width := 0
	for _, r := range rows {
		width = max(width, len(r))
	}
	if width == 0 {
		return ""
	}
	var b strings.Builder
	line := func(r []string) {
		b.WriteString("|")
		for i := range width {
			c := ""
			if i < len(r) {
				c = r[i]
			}
			if c == "" {
				c = " "
			}
			b.WriteString(" " + c + " |")
		}
		b.WriteString("\n")
	}
	line(rows[0])
	b.WriteString("|")
	for range width {
		b.WriteString(" --- |")
	}
	b.WriteString("\n")
	for _, r := range rows[1:] {
		line(r)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// cleanAlt is alt text on one line.
func cleanAlt(s string) string {
	return strings.TrimSpace(oneLine(s))
}

// escapeAlt makes alt text safe inside "![...]": no brackets, no tags.
func escapeAlt(s string) string {
	s = strings.NewReplacer("[", "(", "]", ")", `\`, "/").Replace(s)
	return escapeInline(s)
}

// fenced wraps literal text in a code fence longer than any backtick run in
// it, so nothing inside can close it.
func fenced(text string) string {
	run, longest := 0, 0
	for i := 0; i < len(text); i++ {
		if text[i] == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	f := strings.Repeat("`", max(3, longest+1))
	return f + "\n" + strings.TrimRight(normalizeNewlines(text), "\n") + "\n" + f
}

// heading renders a heading of level (clamped to 1..6).
func heading(level int, text string) string {
	level = min(max(level, 1), 6)
	t := headingText(text)
	if t == "" {
		t = "Untitled"
	}
	return strings.Repeat("#", level) + " " + t
}
