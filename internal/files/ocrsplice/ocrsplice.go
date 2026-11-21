// Package ocrsplice writes OCR transcripts into an extraction's Markdown in
// place of the pages they read (RFC FG). It is stdlib only so the core and a
// workspace worker splice the same way: the core splices Markdown it holds,
// and a workspace's worker splices Markdown that stays in the workspace (RFC
// GW D34), sent only the transcripts.
package ocrsplice

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Splice writes each page of text in place of that page's own Markdown (cut
// at starts, the byte offsets the extractor reported), keeps every other page
// as it was, and puts header (a provenance comment, written as given) before
// the first page. md is returned unchanged when there is no transcript or no
// page offset.
func Splice(md string, starts map[int]int, text map[int]string, header string) string {
	if len(text) == 0 || len(starts) == 0 {
		return md
	}
	pages := make([]int, 0, len(starts))
	for n := range starts {
		pages = append(pages, n)
	}
	sort.Ints(pages)
	at := func(n int) int { return min(max(starts[n], 0), len(md)) }
	var b strings.Builder
	b.WriteString(md[:at(pages[0])])
	b.WriteString("\n\n" + header)
	for i, n := range pages {
		end := len(md)
		if i+1 < len(pages) {
			end = at(pages[i+1])
		}
		start := min(at(n), end)
		if t, ok := text[n]; ok {
			if kept := headingsBefore(md[start:end], n); kept != "" {
				b.WriteString("\n\n" + kept)
			}
			fmt.Fprintf(&b, "\n\n## Page %d\n\n%s", n, DemoteHeadings(t))
			continue
		}
		b.WriteString(md[start:end])
	}
	return b.String()
}

// ImageStarts is where the one page of an image's Markdown begins: after its
// title, the first blank line. nil when there is none.
func ImageStarts(md string) map[int]int {
	if i := strings.Index(md, "\n\n"); i >= 0 {
		return map[int]int{1: i}
	}
	return nil
}

// headingsBefore are the heading lines a page's Markdown opens with before
// its own "## Page N": a DjVu's outline entries that point to the page (RFC
// GV P3), which an OCR transcript replacing the page must not take with
// it. A PDF page opens with its own heading, so it keeps nothing. The page
// heading is the last of the opening run, since an outline entry may carry
// the same words.
func headingsBefore(span string, n int) string {
	own := fmt.Sprintf("## Page %d", n)
	var run []string
	last := -1
	for _, l := range strings.Split(span, "\n") {
		if l == "" {
			continue
		}
		if !strings.HasPrefix(l, "#") {
			break
		}
		if l == own {
			last = len(run)
		}
		run = append(run, l)
	}
	if last < 1 {
		return ""
	}
	return strings.Join(run[:last], "\n\n")
}

var headingRe = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)

// DemoteHeadings puts a transcript's headings two levels down, under its
// "## Page N", so a page's "# Title" does not become a section of the
// document beside the pages. Fenced code is left as it is.
func DemoteHeadings(md string) string {
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
		if m := headingRe.FindStringSubmatch(l); m != nil {
			lines[i] = strings.Repeat("#", min(len(m[1])+2, 6)) + " " + m[2]
		}
	}
	return strings.Join(lines, "\n")
}

// fenceDelim reads a line as a CommonMark code fence delimiter (up to three
// leading spaces, then three or more ` or ~), returning the character and
// the run's length, or a run of 0 when the line is not one.
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
