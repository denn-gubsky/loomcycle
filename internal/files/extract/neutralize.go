package extract

import (
	"regexp"
	"strings"
)

// volumeEmbedRe finds an inline image whose target is a volume reference,
// bare or in angle brackets.
var volumeEmbedRe = regexp.MustCompile(`(?i)!\[[^\]]*\]\(\s*<?\s*volume:`)

// Neutralize makes Markdown written by someone else safe to import as
// text: outside code fences, every live "<" that would open a tag or a
// comment is escaped (raw HTML, and the loom metadata and edge comments
// import_md acts on), and, when embeds is true, an image whose target is a
// volume reference loses its "!" and stays a link. Without that an importer
// reading the reference would copy one of the reader's own files into the
// document. Fenced code is left exactly as written.
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
			l = unembedVolumeImages(l)
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

// unembedVolumeImages drops the "!" of each live volume image embed.
func unembedVolumeImages(l string) string {
	locs := volumeEmbedRe.FindAllStringIndex(l, -1)
	if locs == nil {
		return l
	}
	var b strings.Builder
	last := 0
	for _, loc := range locs {
		if !unescaped(l, loc[0]) {
			continue
		}
		b.WriteString(l[last:loc[0]])
		last = loc[0] + 1 // skip the "!"
	}
	b.WriteString(l[last:])
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
