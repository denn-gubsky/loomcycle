package extract

import "strings"

// Neutralize makes Markdown written by someone else safe to import as
// text: outside code fences, every live "<" that would open a tag or a
// comment is escaped (raw HTML, and the loom metadata and edge comments
// import_md acts on), and, when embeds is true, every live image opener "!["
// is escaped, so no image of any kind survives. Without that an importer
// resolving a volume reference would copy one of the reader's own files into
// the document. The rule is per opener, not per embed: an embed's alt text
// may span lines, and a check that looked for a whole embed on one line
// would pass one the importer still reads. Fenced code is left exactly as
// written.
func Neutralize(md string, embeds bool) string {
	lines := strings.Split(md, "\n")
	var fenceChar byte
	fenceRun := 0
	for i, l := range lines {
		if c, run := fenceDelim(l); run > 0 {
			switch {
			case fenceRun == 0:
				fenceChar, fenceRun = c, run
			case c == fenceChar && run >= fenceRun:
				fenceRun = 0
			}
			continue
		}
		if fenceRun > 0 {
			continue
		}
		if embeds {
			l = defuseImageOpeners(l)
		}
		lines[i] = escapeLiveTags(l)
	}
	return strings.Join(lines, "\n")
}

// fenceDelim reads a line as a code fence delimiter (up to three leading
// spaces, then three or more of "`" or "~"), as import_md does.
func fenceDelim(line string) (byte, int) {
	i := 0
	for i < len(line) && i < 3 && line[i] == ' ' {
		i++
	}
	if i >= len(line) || (line[i] != '`' && line[i] != '~') {
		return 0, 0
	}
	c := line[i]
	run := 0
	for i+run < len(line) && line[i+run] == c {
		run++
	}
	if run < 3 {
		return 0, 0
	}
	return c, run
}

// defuseImageOpeners escapes the "[" of each live "![".
func defuseImageOpeners(l string) string {
	if !strings.Contains(l, "![") {
		return l
	}
	var b strings.Builder
	for i := 0; i < len(l); i++ {
		if l[i] == '[' && i > 0 && l[i-1] == '!' && unescaped(l, i-1) && unescaped(l, i) {
			b.WriteByte('\\')
		}
		b.WriteByte(l[i])
	}
	return b.String()
}

// escapeLiveTags puts a backslash before each "<" that starts a tag or a
// comment and is not already escaped.
func escapeLiveTags(l string) string {
	if !strings.Contains(l, "<") {
		return l
	}
	var b strings.Builder
	for i := 0; i < len(l); i++ {
		if l[i] == '<' && i+1 < len(l) && tagStart(l[i+1]) && unescaped(l, i) {
			b.WriteByte('\\')
		}
		b.WriteByte(l[i])
	}
	return b.String()
}

// unescaped reports whether the byte at i follows an even number of
// backslashes.
func unescaped(s string, i int) bool {
	n := 0
	for j := i - 1; j >= 0 && s[j] == '\\'; j-- {
		n++
	}
	return n%2 == 0
}
