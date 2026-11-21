package extract

import (
	"bytes"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	xunicode "golang.org/x/text/encoding/unicode"
)

// Old books come as text and HTML in an 8-bit code page and name none: a
// Russian text in windows-1251, KOI8-R or DOS CP866, an English one in
// Latin-1. Go's HTML parser reads bytes as UTF-8 whatever a page says, and
// a worker cannot send text that is not UTF-8 at all (its wire is
// protobuf), so such a file is decoded before it is read.

// cyrillicCandidates are the Cyrillic code pages unmarked text is tried
// in, the commonest first, which wins a tie.
var cyrillicCandidates = []*charmap.Charmap{charmap.Windows1251, charmap.KOI8R, charmap.CodePage866}

// cyrillicCodePage reports which single-byte Cyrillic code page unmarked
// text is in, or nil when it does not look Cyrillic. Each candidate is
// scored by the runs of four or more lower-case Cyrillic letters its
// decoding gives: Russian text is mostly lower case, and read in the wrong
// one of these code pages its letters come out as upper case, box drawing
// or punctuation instead. Western text in windows-1252 has its accented
// letters one or two at a time between ASCII ones, so it makes no runs.
// A run counts only when it holds three or more different bytes: a DOS
// box-drawing line (CP437 0xC4 eighty times) decodes as one Cyrillic
// letter repeated in KOI8-R, and is no word. minRuns runs make the text
// Cyrillic, and only when they are not outnumbered four to one by words of
// ASCII letters: English text with a few such lines is not Russian.
func cyrillicCodePage(b []byte, minRuns int) *charmap.Charmap {
	var best *charmap.Charmap
	bestRuns := 0
	for _, cm := range cyrillicCandidates {
		runs, n := 0, 0
		var seen [256]bool
		distinct := 0
		end := func() {
			if n >= 4 && distinct >= 3 {
				runs++
			}
			n, distinct = 0, 0
			seen = [256]bool{}
		}
		for _, c := range b {
			if c >= 0x80 && cyrillicLower(cm.DecodeByte(c)) {
				n++
				if !seen[c] {
					seen[c] = true
					distinct++
				}
				continue
			}
			end()
		}
		end()
		if runs > bestRuns {
			best, bestRuns = cm, runs
		}
	}
	if bestRuns < minRuns || bestRuns*4 < asciiWords(b) {
		return nil
	}
	return best
}

// asciiWords counts runs of four or more ASCII letters outside markup
// tags, so an HTML page's or contents file's element and attribute names
// are not taken for English words.
func asciiWords(b []byte) int {
	words, n := 0, 0
	inTag := false
	for _, c := range b {
		switch {
		case c == '<':
			inTag = true
		case c == '>':
			inTag = false
		}
		if !inTag && (c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			n++
			continue
		}
		if n >= 4 {
			words++
		}
		n = 0
	}
	if n >= 4 {
		words++
	}
	return words
}

// dosBoxDrawing reports whether most of a text's 8-bit bytes are CP437's
// line and box drawing (0xB0-0xDF), as an English DOS text's frames are.
func dosBoxDrawing(b []byte) bool {
	high, box := 0, 0
	for _, c := range b {
		if c >= 0x80 {
			high++
			if c >= 0xB0 && c <= 0xDF {
				box++
			}
		}
	}
	return high > 0 && box*5 >= high*4
}

func cyrillicLower(r rune) bool {
	return unicode.Is(unicode.Cyrillic, r) && unicode.IsLower(r)
}

// utf16BOM decodes text that opens with a UTF-16 byte order mark.
func utf16BOM(b []byte) ([]byte, string, bool) {
	var e encoding.Encoding
	var name string
	switch {
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}):
		e, name = xunicode.UTF16(xunicode.LittleEndian, xunicode.ExpectBOM), "UTF-16LE"
	case bytes.HasPrefix(b, []byte{0xFE, 0xFF}):
		e, name = xunicode.UTF16(xunicode.BigEndian, xunicode.ExpectBOM), "UTF-16BE"
	default:
		return nil, "", false
	}
	out, err := e.NewDecoder().Bytes(b)
	if err != nil {
		return nil, "", false
	}
	return out, name, true
}

// decodeUnmarked decodes 8-bit text that names no encoding and is not
// UTF-8: Cyrillic when it reads as Cyrillic; DOS code page 437 when its
// 8-bit bytes are mostly box drawing; else windows-1252, the default the
// web and Windows gave such text. It returns the code page used.
func decodeUnmarked(b []byte) ([]byte, string) {
	enc := cyrillicCodePage(b, 3)
	switch {
	case enc != nil:
	case dosBoxDrawing(b):
		enc = charmap.CodePage437
	default:
		enc = charmap.Windows1252
	}
	out, err := enc.NewDecoder().Bytes(b)
	if err != nil {
		return bytes.ToValidUTF8(b, []byte("�")), "UTF-8 with its invalid bytes replaced"
	}
	return out, enc.String()
}

// binaryText reports whether bytes typed as text are not text: they hold
// a NUL, or control characters other than the ones text uses (tab, line
// and form feeds, carriage return, DOS's end-of-file mark) in more than
// one byte in a hundred.
func binaryText(b []byte) bool {
	ctl := 0
	for _, c := range b {
		switch {
		case c == 0:
			return true
		case c < 0x20 && c != '\t' && c != '\n' && c != '\r' && c != '\f' && c != '\v' && c != 0x1A:
			ctl++
		}
	}
	return ctl*100 > len(b)
}

// decodeHTMLPage returns an HTML page as UTF-8: in the encoding its
// <meta> names, else as UTF-8 when it is valid UTF-8 or opens with a
// UTF-16 mark, else by decodeUnmarked. note is what was done, "" for a
// page that was UTF-8 already. A page naming an encoding outside the fixed
// list is refused by name.
func decodeHTMLPage(data []byte) (out []byte, note string, err error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if dec, name, ok := utf16BOM(data); ok {
		return dec, "the page is " + name, nil
	}
	if label := htmlMetaCharset(data); label != "" {
		switch {
		case label == "utf-8" || label == "utf8":
			if utf8.Valid(data) {
				return data, "", nil
			}
			return bytes.ToValidUTF8(data, []byte("�")), "the page says UTF-8 but is not: its invalid bytes were replaced", nil
		case label == "us-ascii":
			label = "windows-1252"
		}
		enc := fb2Encodings[label]
		if enc == nil {
			return nil, "", refusedf("the page is in %s, which is not read", cutLabel(label))
		}
		dec, derr := enc.NewDecoder().Bytes(data)
		if derr != nil {
			return nil, "", refusedf("the page is not valid %s", cutLabel(label))
		}
		return dec, "", nil
	}
	if utf8.Valid(data) {
		return data, "", nil
	}
	dec, name := decodeUnmarked(data)
	return dec, "the page names no encoding and is not UTF-8: it was read as " + name, nil
}

// htmlMetaCharset is the encoding a page's <meta charset> or http-equiv
// Content-Type names in its first 4 KiB, lower-cased; "" when none.
func htmlMetaCharset(data []byte) string {
	z := html.NewTokenizer(bytes.NewReader(data[:min(len(data), 4096)]))
	for {
		switch z.Next() {
		case html.ErrorToken:
			return ""
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			switch string(name) {
			case "body":
				return ""
			case "meta":
			default:
				continue
			}
			var charset, content string
			for hasAttr {
				var k, v []byte
				k, v, hasAttr = z.TagAttr()
				switch strings.ToLower(string(k)) {
				case "charset":
					charset = string(v)
				case "content":
					content = string(v)
				}
			}
			if charset == "" {
				if i := strings.Index(strings.ToLower(content), "charset="); i >= 0 {
					charset = content[i+len("charset="):]
					if j := strings.IndexAny(charset, "; "); j >= 0 {
						charset = charset[:j]
					}
				}
			}
			if c := strings.ToLower(strings.Trim(strings.TrimSpace(charset), `"'`)); c != "" {
				return c
			}
		}
	}
}
