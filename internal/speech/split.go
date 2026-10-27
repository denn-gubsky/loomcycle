package speech

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Piece is one engine request's text.
type Piece struct {
	Text string
	// ParagraphEnd marks the last piece of a paragraph, so the joiner puts a
	// longer pause after it. The text's last piece always has it set; there
	// is nothing after it to pause before.
	ParagraphEnd bool
}

// Split cuts text into pieces of at most maxChars characters (runes, not
// bytes) for one engine request each. Paragraphs are separated by a blank
// line (the shape Normalize returns); a piece never spans two. Inside a
// paragraph whitespace is folded to single spaces and pieces are filled with
// whole sentences — a sentence ends at '.', '!', '?' or '…' (a run of them,
// then any closing quotes or brackets) followed by whitespace, so "3.14" and
// "e.g.x" are not ends.
//
// A sentence longer than maxChars is cut at the last whitespace before the
// limit, so no word is cut. Only a single word longer than maxChars is cut
// inside: hard, at maxChars, moved back so a combining mark (a stress mark)
// stays with its letter. maxChars below 1 is taken as 1.
func Split(text string, maxChars int) []Piece {
	maxChars = max(maxChars, 1)
	p := packer{max: maxChars}
	for _, para := range paragraphs(text) {
		for _, s := range sentences(para) {
			p.addSentence(s)
		}
		p.flush(true)
	}
	return p.out
}

// paragraphs splits text at blank lines and folds each paragraph's
// whitespace to single spaces, dropping empty ones.
func paragraphs(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	var out []string
	var cur []string
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			if len(cur) > 0 {
				out = append(out, strings.Join(cur, " "))
				cur = nil
			}
			continue
		}
		cur = append(cur, strings.Fields(line)...)
	}
	if len(cur) > 0 {
		out = append(out, strings.Join(cur, " "))
	}
	return out
}

// sentences cuts one whitespace-folded paragraph into sentences.
func sentences(para string) []string {
	var out []string
	start := 0
	for i := 0; i < len(para); {
		r, size := utf8.DecodeRuneInString(para[i:])
		i += size
		if !isSentenceEnd(r) {
			continue
		}
		for i < len(para) {
			r, size := utf8.DecodeRuneInString(para[i:])
			if !isSentenceEnd(r) && !strings.ContainsRune("\"'”’»)]", r) {
				break
			}
			i += size
		}
		if i == len(para) || para[i] == ' ' {
			if s := strings.TrimSpace(para[start:i]); s != "" {
				out = append(out, s)
			}
			start = i
		}
	}
	if s := strings.TrimSpace(para[start:]); s != "" {
		out = append(out, s)
	}
	return out
}

func isSentenceEnd(r rune) bool {
	return r == '.' || r == '!' || r == '?' || r == '…'
}

// packer fills pieces greedily, flushing at sentence boundaries first.
type packer struct {
	max    int
	cur    strings.Builder
	curLen int // runes in cur
	out    []Piece
}

func (p *packer) flush(paragraphEnd bool) {
	if p.curLen == 0 {
		if paragraphEnd && len(p.out) > 0 {
			p.out[len(p.out)-1].ParagraphEnd = true
		}
		return
	}
	p.out = append(p.out, Piece{Text: p.cur.String(), ParagraphEnd: paragraphEnd})
	p.cur.Reset()
	p.curLen = 0
}

// add appends s (n runes) to the open piece if it fits.
func (p *packer) add(s string, n int) bool {
	if p.curLen == 0 {
		if n > p.max {
			return false
		}
		p.cur.WriteString(s)
		p.curLen = n
		return true
	}
	if p.curLen+1+n > p.max {
		return false
	}
	p.cur.WriteByte(' ')
	p.cur.WriteString(s)
	p.curLen += 1 + n
	return true
}

func (p *packer) addSentence(s string) {
	n := utf8.RuneCountInString(s)
	if p.add(s, n) {
		return
	}
	p.flush(false)
	if p.add(s, n) {
		return
	}
	// Longer than a piece: fill word by word.
	for _, w := range strings.Split(s, " ") {
		wn := utf8.RuneCountInString(w)
		if p.add(w, wn) {
			continue
		}
		p.flush(false)
		if p.add(w, wn) {
			continue
		}
		for _, part := range hardSplit(w, p.max) {
			p.flush(false)
			p.add(part, utf8.RuneCountInString(part))
		}
	}
}

// hardSplit cuts a word into parts of at most max runes. A cut that would
// start a part with a combining mark moves back so the mark stays with the
// letter before it; only a run of marks as long as max is cut at max.
func hardSplit(w string, max int) []string {
	runes := []rune(w)
	var out []string
	for len(runes) > 0 {
		cut := min(max, len(runes))
		if cut < len(runes) {
			c := cut
			for c > 0 && unicode.Is(unicode.Mn, runes[c]) {
				c--
			}
			if c > 0 {
				cut = c
			}
		}
		out = append(out, string(runes[:cut]))
		runes = runes[cut:]
	}
	return out
}
