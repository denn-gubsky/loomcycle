package extract

import (
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
	"golang.org/x/text/encoding/charmap"
)

// Turning a PDF font's codes into text. A string shown on a page is a
// sequence of codes in the font's own encoding, not text: a CID font (Word and
// LibreOffice embed Cyrillic as Type0 fonts with the Identity-H encoding) shows
// glyph numbers, and only its ToUnicode CMap says which characters they are.
// So the ToUnicode CMap comes first for every font; a simple font without one
// falls back to its named encoding and Differences; a CID font without one
// has no text to give and is counted as such.

// pdfMaxCMapBytes bounds a ToUnicode CMap read for one font.
const pdfMaxCMapBytes = 4 << 20

type pdfFont struct {
	// twoByte: a composite (Type0) font, whose codes are read by the
	// codespace ranges of its CMap (two bytes for Identity-H).
	twoByte bool
	toUni   *pdfCMap
	// simple is the code-to-rune table of a simple font without ToUnicode.
	simple *[256]rune
	// utf16 is a composite font whose CMap is a Unicode one (UniGB-UCS2-H
	// and the like): its codes are the text in UTF-16BE.
	utf16 bool
	// unmapped is a composite font with nothing to say what its codes mean.
	unmapped bool

	widths    map[int]float64 // per code, in thousandths of the size
	dw        float64
	firstChar int
	simpleW   []float64
}

// decode splits a shown string into codes and returns its text and the
// advance width of each code, in thousandths of the font size.
func (f *pdfFont) decode(raw string, each func(r []rune, code int, width float64, single bool)) {
	if f.twoByte {
		for len(raw) > 0 {
			n := 2
			if f.toUni != nil {
				n = f.toUni.codeLen(raw)
			}
			if n > len(raw) {
				n = len(raw)
			}
			code := beInt(raw[:n])
			var rs []rune
			switch {
			case f.toUni != nil:
				rs = f.toUni.lookup(raw[:n])
			case f.utf16:
				rs = utf16Runes(raw[:n])
			}
			w, ok := f.widths[code]
			if !ok {
				w = f.dw
			}
			each(rs, code, w, false)
			raw = raw[n:]
		}
		return
	}
	for i := 0; i < len(raw); i++ {
		code := int(raw[i])
		var rs []rune
		if f.toUni != nil {
			rs = f.toUni.lookup(raw[i : i+1])
		}
		if rs == nil && f.simple != nil {
			if r := f.simple[code]; r != 0 {
				rs = []rune{r}
			}
		}
		w := 0.0
		if j := code - f.firstChar; j >= 0 && j < len(f.simpleW) {
			w = f.simpleW[j]
		}
		if w == 0 {
			w = f.dw
		}
		each(rs, code, w, true)
	}
}

func beInt(s string) int {
	n := 0
	for i := 0; i < len(s) && i < 4; i++ {
		n = n<<8 | int(s[i])
	}
	return n
}

func utf16Runes(s string) []rune {
	if len(s)%2 != 0 {
		return nil
	}
	u := make([]uint16, len(s)/2)
	for i := range u {
		u[i] = uint16(s[2*i])<<8 | uint16(s[2*i+1])
	}
	return utf16.Decode(u)
}

// loadFont reads a font dictionary.
func loadFont(xr *model.XRefTable, o types.Object) *pdfFont {
	d, err := xr.DereferenceDict(o)
	if err != nil || d == nil {
		return &pdfFont{simple: latinTable(), dw: 500}
	}
	f := &pdfFont{dw: 500}
	if cm := loadCMap(xr, d["ToUnicode"]); cm != nil {
		f.toUni = cm
	}
	if pdfName(xr, d["Subtype"]) == "Type0" {
		f.twoByte = true
		f.dw = 1000
		enc := pdfName(xr, d["Encoding"])
		if f.toUni == nil {
			switch {
			case strings.Contains(enc, "UCS2") || strings.Contains(enc, "UTF16"):
				f.utf16 = true
			default:
				f.unmapped = true
			}
		}
		if arr, err := xr.DereferenceArray(d["DescendantFonts"]); err == nil && len(arr) > 0 {
			if cd, err := xr.DereferenceDict(arr[0]); err == nil && cd != nil {
				if v, ok := pdfNumber(xr, cd["DW"]); ok {
					f.dw = v
				}
				f.widths = cidWidths(xr, cd["W"])
			}
		}
		return f
	}
	f.simple = simpleEncoding(xr, d["Encoding"])
	if v, ok := pdfNumber(xr, d["FirstChar"]); ok {
		f.firstChar = int(v)
	}
	if arr, err := xr.DereferenceArray(d["Widths"]); err == nil {
		for i, w := range arr {
			if i >= 256 {
				break
			}
			v, _ := pdfNumber(xr, w)
			f.simpleW = append(f.simpleW, v)
		}
	}
	if fd, err := xr.DereferenceDict(d["FontDescriptor"]); err == nil && fd != nil {
		if v, ok := pdfNumber(xr, fd["MissingWidth"]); ok && v > 0 {
			f.dw = v
		}
	}
	return f
}

// cidWidths reads a CID font's W array: "c [w1 w2 …]" and "c1 c2 w".
func cidWidths(xr *model.XRefTable, o types.Object) map[int]float64 {
	arr, err := xr.DereferenceArray(o)
	if err != nil || len(arr) == 0 {
		return nil
	}
	const maxEntries = 1 << 16
	out := map[int]float64{}
	for i := 0; i < len(arr) && len(out) < maxEntries; {
		c1, ok := pdfNumber(xr, arr[i])
		if !ok || i+1 >= len(arr) {
			break
		}
		if ws, err := xr.DereferenceArray(arr[i+1]); err == nil && ws != nil {
			for k, w := range ws {
				if v, ok := pdfNumber(xr, w); ok && len(out) < maxEntries {
					out[int(c1)+k] = v
				}
			}
			i += 2
			continue
		}
		if i+2 >= len(arr) {
			break
		}
		c2, ok2 := pdfNumber(xr, arr[i+1])
		w, ok3 := pdfNumber(xr, arr[i+2])
		if !ok2 || !ok3 {
			break
		}
		for c := int(c1); c <= int(c2) && len(out) < maxEntries; c++ {
			out[c] = w
		}
		i += 3
	}
	return out
}

func pdfName(xr *model.XRefTable, o types.Object) string {
	o, err := xr.Dereference(o)
	if err != nil {
		return ""
	}
	if n, ok := o.(types.Name); ok {
		return n.Value()
	}
	return ""
}

func pdfNumber(xr *model.XRefTable, o types.Object) (float64, bool) {
	o, err := xr.Dereference(o)
	if err != nil {
		return 0, false
	}
	switch v := o.(type) {
	case types.Integer:
		return float64(v), true
	case types.Float:
		return float64(v), true
	}
	return 0, false
}

// ---- ToUnicode CMaps ----

type pdfCodeRange struct{ lo, hi string }

type pdfBFRange struct {
	lo, hi string
	dst    string   // the first destination, UTF-16BE
	arr    []string // or one per code
}

type pdfCMap struct {
	space  []pdfCodeRange
	chars  map[string][]rune
	ranges []pdfBFRange
}

// codeLen is the length of the code at the start of s: the codespace range
// it falls in, else two bytes (a CMap without ranges is read as Identity-H).
func (m *pdfCMap) codeLen(s string) int {
	for n := 1; n <= 4 && n <= len(s); n++ {
		for _, r := range m.space {
			if len(r.lo) == n && r.lo <= s[:n] && s[:n] <= r.hi {
				return n
			}
		}
	}
	return 2
}

func (m *pdfCMap) lookup(code string) []rune {
	if r, ok := m.chars[code]; ok {
		return r
	}
	for _, br := range m.ranges {
		if len(br.lo) != len(code) || code < br.lo || code > br.hi {
			continue
		}
		off := beInt(code) - beInt(br.lo)
		if br.arr != nil {
			if off < len(br.arr) {
				return utf16Runes(br.arr[off])
			}
			return nil
		}
		// The offset is added to the last UTF-16 unit of the destination.
		d := []byte(br.dst)
		if len(d) < 2 {
			return nil
		}
		last := int(d[len(d)-2])<<8 | int(d[len(d)-1])
		last += off
		d[len(d)-2], d[len(d)-1] = byte(last>>8), byte(last)
		return utf16Runes(string(d))
	}
	return nil
}

// loadCMap reads a ToUnicode stream: its codespace ranges and its bfchar and
// bfrange mappings. Nothing else in a CMap matters for text.
func loadCMap(xr *model.XRefTable, o types.Object) *pdfCMap {
	if o == nil {
		return nil
	}
	sd, _, err := xr.DereferenceStreamDict(o)
	if err != nil || sd == nil {
		return nil
	}
	if err := sd.DecodeWithLimit(pdfMaxCMapBytes); err != nil {
		return nil
	}
	m := &pdfCMap{chars: map[string][]rune{}}
	const maxMappings = 1 << 17
	var mode string
	var vals []pdfVal
	l := &pdfLexer{b: sd.Content}
	for len(m.chars)+len(m.ranges) < maxMappings {
		v, ok := l.next(0)
		if !ok {
			break
		}
		if v.kind != tokOp {
			if mode != "" {
				vals = append(vals, v)
			}
			continue
		}
		switch v.str {
		case "begincodespacerange", "beginbfchar", "beginbfrange":
			mode, vals = v.str, vals[:0]
		case "endcodespacerange":
			for i := 0; i+1 < len(vals); i += 2 {
				if vals[i].kind == tokStr && vals[i+1].kind == tokStr && len(vals[i].str) == len(vals[i+1].str) && len(vals[i].str) > 0 && len(vals[i].str) <= 4 {
					m.space = append(m.space, pdfCodeRange{vals[i].str, vals[i+1].str})
				}
			}
			mode = ""
		case "endbfchar":
			for i := 0; i+1 < len(vals); i += 2 {
				if vals[i].kind == tokStr {
					switch d := vals[i+1]; d.kind {
					case tokStr:
						m.chars[vals[i].str] = utf16Runes(d.str)
					case tokName:
						if r := glyphRune(d.str); r != 0 {
							m.chars[vals[i].str] = []rune{r}
						}
					}
				}
			}
			mode = ""
		case "endbfrange":
			for i := 0; i+2 < len(vals); i += 3 {
				lo, hi, d := vals[i], vals[i+1], vals[i+2]
				if lo.kind != tokStr || hi.kind != tokStr || len(lo.str) != len(hi.str) || lo.str > hi.str {
					continue
				}
				br := pdfBFRange{lo: lo.str, hi: hi.str}
				switch d.kind {
				case tokStr:
					br.dst = d.str
				case tokArr:
					for _, a := range d.arr {
						br.arr = append(br.arr, a.str)
					}
				default:
					continue
				}
				m.ranges = append(m.ranges, br)
			}
			mode = ""
		}
	}
	if len(m.chars) == 0 && len(m.ranges) == 0 {
		return nil
	}
	return m
}

// ---- simple fonts ----

var (
	winAnsiTable  = byteTable(charmap.Windows1252)
	macRomanTable = byteTable(charmap.Macintosh)
	latin1Table   = byteTable(charmap.ISO8859_1)
)

func byteTable(cm *charmap.Charmap) *[256]rune {
	var t [256]rune
	for i := range t {
		if r := cm.DecodeByte(byte(i)); r != '�' {
			t[i] = r
		}
	}
	return &t
}

// latinTable is the table of a font with no encoding of its own: the
// standard and PDFDoc encodings agree with Latin-1 on letters and digits.
func latinTable() *[256]rune {
	t := *latin1Table
	return &t
}

func simpleEncoding(xr *model.XRefTable, o types.Object) *[256]rune {
	base := func(name string) *[256]rune {
		var t [256]rune
		switch name {
		case "WinAnsiEncoding":
			t = *winAnsiTable
		case "MacRomanEncoding":
			t = *macRomanTable
		default:
			t = *latin1Table
		}
		return &t
	}
	o, err := xr.Dereference(o)
	if err != nil {
		return base("")
	}
	switch v := o.(type) {
	case types.Name:
		return base(v.Value())
	case types.Dict:
		t := base(pdfName(xr, v["BaseEncoding"]))
		diffs, err := xr.DereferenceArray(v["Differences"])
		if err != nil {
			return t
		}
		code := -1
		for _, d := range diffs {
			if n, ok := pdfNumber(xr, d); ok {
				code = int(n)
				continue
			}
			name := pdfName(xr, d)
			if name == "" || code < 0 || code > 255 {
				continue
			}
			if r := glyphRune(name); r != 0 {
				t[code] = r
			} else {
				t[code] = 0
			}
			code++
		}
		return t
	}
	return base("")
}

// glyphRune is the character a glyph name stands for: uniXXXX and uXXXX[XX]
// names, single letters and digits, the Cyrillic afii names, and the common
// punctuation names. An unknown name is 0.
func glyphRune(name string) rune {
	if i := strings.IndexByte(name, '.'); i > 0 {
		name = name[:i] // a.sc, uni0410.alt
	}
	switch {
	case len(name) == 7 && strings.HasPrefix(name, "uni"):
		if v, err := strconv.ParseUint(name[3:], 16, 32); err == nil {
			return rune(v)
		}
	case len(name) >= 5 && len(name) <= 7 && name[0] == 'u':
		if v, err := strconv.ParseUint(name[1:], 16, 32); err == nil && v <= 0x10FFFF {
			return rune(v)
		}
	case len(name) == 1 && (name[0] >= 'a' && name[0] <= 'z' || name[0] >= 'A' && name[0] <= 'Z'):
		return rune(name[0])
	case len(name) == 9 && strings.HasPrefix(name, "afii10"):
		if n, err := strconv.Atoi(name[4:]); err == nil {
			if r, ok := afiiCyrillic(n); ok {
				return r
			}
		}
	}
	return glyphNames[name]
}

// afiiCyrillic maps the Adobe afii names of the Cyrillic letters:
// afii10017–10049 are А–Я with Ё at 10023, afii10065–10097 а–я with ё at
// 10071, and the Ukrainian and other letters sit outside those runs.
func afiiCyrillic(n int) (rune, bool) {
	run := func(n, first int, a, yo rune) rune {
		switch i := n - first; {
		case i < 6:
			return a + rune(i)
		case i == 6:
			return yo
		default:
			return a + rune(i-1)
		}
	}
	switch {
	case n >= 10017 && n <= 10049:
		return run(n, 10017, 'А', 'Ё'), true
	case n >= 10065 && n <= 10097:
		return run(n, 10065, 'а', 'ё'), true
	}
	r, ok := afiiOther[n]
	return r, ok
}

var afiiOther = map[int]rune{
	10050: 'Ґ', 10051: 'Ђ', 10052: 'Ѓ', 10053: 'Є', 10054: 'Ѕ', 10055: 'І', 10056: 'Ї',
	10057: 'Ј', 10058: 'Љ', 10059: 'Њ', 10060: 'Ћ', 10061: 'Ќ', 10062: 'Ў', 10145: 'Џ',
	10098: 'ґ', 10099: 'ђ', 10100: 'ѓ', 10101: 'є', 10102: 'ѕ', 10103: 'і', 10104: 'ї',
	10105: 'ј', 10106: 'љ', 10107: 'њ', 10108: 'ћ', 10109: 'ќ', 10110: 'ў', 10193: 'џ',
}

var glyphNames = map[string]rune{
	"space": ' ', "exclam": '!', "quotedbl": '"', "numbersign": '#', "dollar": '$',
	"percent": '%', "ampersand": '&', "quotesingle": '\'', "quoteright": '’', "quoteleft": '‘',
	"parenleft": '(', "parenright": ')', "asterisk": '*', "plus": '+', "comma": ',',
	"hyphen": '-', "period": '.', "slash": '/', "colon": ':', "semicolon": ';',
	"less": '<', "equal": '=', "greater": '>', "question": '?', "at": '@',
	"bracketleft": '[', "backslash": '\\', "bracketright": ']', "underscore": '_',
	"braceleft": '{', "bar": '|', "braceright": '}', "asciitilde": '~',
	"zero": '0', "one": '1', "two": '2', "three": '3', "four": '4',
	"five": '5', "six": '6', "seven": '7', "eight": '8', "nine": '9',
	"endash": '–', "emdash": '—', "quotedblleft": '“', "quotedblright": '”',
	"quotedblbase": '„', "guillemotleft": '«', "guillemotright": '»', "bullet": '•',
	"ellipsis": '…', "numero": '№', "afii61352": '№', "degree": '°', "section": '§',
	"copyright": '©', "registered": '®', "trademark": '™', "euro": '€', "Euro": '€',
	"nbspace": ' ', "minus": '−', "fi": 'ﬁ', "fl": 'ﬂ',
}
