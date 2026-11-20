package inbox

import (
	"bytes"
	"encoding/binary"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"unicode"
)

// SanitizeName keeps only a name's last segment, in [A-Za-z0-9._-]: every
// other run of characters becomes one "-", leading dots and dashes go (no
// hidden files, nothing that reads as a flag), and the result is at most 120
// bytes with its extension kept. "" when nothing is left.
func SanitizeName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.IndexAny(name, "?#"); i >= 0 {
		name = name[:i] // a URI's query or fragment is not part of its name
	}
	if dec, err := url.PathUnescape(name); err == nil {
		// A URI's %-escapes are the name's own characters; decoded before
		// the last segment is taken, so an escaped "/" separates too.
		name = strings.ReplaceAll(dec, "\\", "/")
	}
	name = path.Base(name)
	if name == "." || name == "/" {
		return ""
	}
	var b strings.Builder
	dash := false
	for _, r := range name {
		if lat, ok := cyrillic[r]; ok {
			b.WriteString(lat)
			dash = false
			continue
		}
		ok := r == '.' || r == '_' || r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok {
			if !dash {
				b.WriteByte('-')
			}
			dash = true
			continue
		}
		b.WriteRune(r)
		dash = false
	}
	out := strings.TrimLeft(b.String(), ".-")
	out = strings.TrimRight(out, "-")
	// A name that was all punctuation keeps its extension: "№.pdf" is a PDF.
	if ext := path.Ext(name); out == strings.TrimLeft(ext, ".") && ext != "" && !strings.HasPrefix(name, ".") {
		out = "file" + ext
	}
	if len(out) > 120 {
		ext := fileExt(out)
		if len(ext) > 16 {
			ext = ""
		}
		out = strings.TrimRight(out[:120-len(ext)], ".-") + ext
	}
	return out
}

// sanitizeDir sanitises each segment of a relative folder path.
func sanitizeDir(dir string) string {
	var segs []string
	for _, s := range strings.Split(strings.ReplaceAll(dir, "\\", "/"), "/") {
		if s = SanitizeName(s); s != "" {
			segs = append(segs, s)
		}
	}
	if len(segs) == 0 {
		return DirDocuments
	}
	return path.Join(segs...)
}

// DirFor is the folder a file of media type mt goes to.
func DirFor(mt string) string {
	// A DjVu book is image/vnd.djvu, and a document (RFC GV §4.1).
	if strings.HasPrefix(mt, "image/") && mt != djvuType {
		return DirImages
	}
	return DirDocuments
}

// The types a file's extension names when its bytes alone do not tell them
// apart: an Office file sniffs as a zip, and http.DetectContentType knows
// nothing of Office at all.
var extTypes = map[string]string{
	".pdf":  "application/pdf",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".odt":  "application/vnd.oasis.opendocument.text",
	".ott":  "application/vnd.oasis.opendocument.text-template",
	".epub": "application/epub+zip",
	".doc":  "application/msword",
	".xls":  "application/vnd.ms-excel",
	".ppt":  "application/vnd.ms-powerpoint",
	".csv":  "text/csv",
	".tsv":  "text/tab-separated-values",
	".md":   "text/markdown",
	".json": "application/json",
	".vtt":  "text/vtt",
	".srt":  "application/x-subrip",
	".djvu": djvuType,
	".djv":  djvuType,

	// FB2 books; ".fb2.zip" is a double extension, read whole by fileExt.
	".fb2":     fb2Type,
	".fb2.zip": fb2ZipType,

	".rtf": rtfType,

	// Palm database books (RFC GV §4.4); the bytes say which a .prc is.
	".mobi": mobiType,
	".azw":  mobiType,
	".azw3": mobiType,
	".prc":  mobiType,
	".pdb":  palmDocType,

	// Compiled HTML Help (RFC GX §4.1).
	".chm": chmType,

	// Recordings whose bytes a sniffer does not know (FLAC, a bare AAC)
	// or knows only as a generic container.
	".m4a":  "audio/mp4",
	".flac": "audio/flac",
	".opus": "audio/ogg",
	".oga":  "audio/ogg",
	".mp3":  "audio/mpeg",
	".wav":  "audio/wav",
}

// The extension a file of each type is given when its name has none. A
// fixed table, not mime.ExtensionsByType: that reads the host's mime files,
// so the same capture would be named differently on two machines.
var typeExts = map[string]string{
	"application/pdf": ".pdf",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   ".docx",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         ".xlsx",
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": ".pptx",
	"application/vnd.oasis.opendocument.text":                                   ".odt",
	"application/vnd.oasis.opendocument.text-template":                          ".ott",
	"application/epub+zip":          ".epub",
	fb2Type:                         ".fb2",
	fb2ZipType:                      ".fb2.zip",
	rtfType:                         ".rtf",
	mobiType:                        ".mobi",
	palmDocType:                     ".pdb",
	chmType:                         ".chm",
	"application/msword":            ".doc",
	"application/vnd.ms-excel":      ".xls",
	"application/vnd.ms-powerpoint": ".ppt",
	"application/zip":               ".zip",
	"application/json":              ".json",
	"text/plain":                    ".txt",
	"text/csv":                      ".csv",
	"text/html":                     ".html",
	"text/markdown":                 ".md",
	"text/vtt":                      ".vtt",
	"image/png":                     ".png",
	"image/jpeg":                    ".jpg",
	"image/gif":                     ".gif",
	"image/webp":                    ".webp",
	"image/svg+xml":                 ".svg",
	"image/bmp":                     ".bmp",
	"image/tiff":                    ".tiff",
	djvuType:                        ".djvu",
	"audio/mpeg":                    ".mp3",
	"audio/wav":                     ".wav",
	"audio/wave":                    ".wav",
	"audio/ogg":                     ".ogg",
	"audio/flac":                    ".flac",
	"audio/mp4":                     ".m4a",
	"video/mp4":                     ".mp4",
	"video/webm":                    ".webm",
	"application/octet-stream":      ".bin",
}

// settleMediaType picks the file's type: the producer's declaration when it
// is specific, else what the bytes sniff as, refined by the name's extension
// when the sniff is generic (an Office file is a zip to a sniffer).
func settleMediaType(declared string, head []byte, name string) string {
	mt := normalizeType(declared)
	if mt == "text/rtf" || mt == "application/x-rtf" {
		mt = rtfType
	}
	// RTF often hides behind a .doc name and a Word type: its header wins
	// over both.
	if (generic(mt) || mt == rtfType || mt == "application/msword") && rtfHeader(head) {
		return rtfType
	}
	// A MOBI book or a PalmDOC text says which it is at offset 60, where a
	// sniffer does not look; its producer seldom declares more than a
	// generic type or one of Amazon's.
	if kind := palmKind(head); kind != "" && (generic(mt) || amazonEbook[mt] || mt == mobiType || mt == palmDocType) {
		return kind
	}
	// A compiled help file is bytes to a sniffer; its ITSF header says
	// what it is.
	if mt == "application/x-chm" {
		mt = chmType
	}
	if (generic(mt) || mt == chmType) && chmHeader(head) {
		return chmType
	}
	// A Word template (.dot) shares its extension with Graphviz's text
	// files: only an OLE2 compound file named so is Word's (RFC GX §4.1).
	if generic(mt) && strings.ToLower(fileExt(name)) == ".dot" && bytes.HasPrefix(head, oleSignature) {
		return "application/msword"
	}
	// An FB2 book is XML to a sniffer and often to its producer too.
	if (generic(mt) || mt == "text/xml" || mt == "application/xml") && fictionBook(head) {
		return fb2Type
	}
	// A zip names no book type; one whose entry is an FB2 book is one.
	if (mt == "application/zip" || mt == "application/x-zip-compressed") && zippedFB2(head) {
		return fb2ZipType
	}
	if mt != "" && !generic(mt) {
		if mt == "image/x-djvu" || mt == "image/x.djvu" {
			return djvuType
		}
		return mt
	}
	if isDjVu(head) {
		return djvuType
	}
	sniffed := normalizeType(http.DetectContentType(head))
	if sniffed == "application/zip" {
		// An OpenDocument file or an EPUB names its own type: it wins over
		// the extension, which a mail attachment may have lost or got wrong.
		if mt := zipMimetype(head); mt != "" {
			return mt
		}
		if zippedFB2(head) {
			return fb2ZipType
		}
	}
	if byExt, ok := extTypes[strings.ToLower(fileExt(name))]; ok && (generic(sniffed) || sniffed == "application/zip" || sniffed == "application/x-ole-storage") {
		return byExt
	}
	if sniffed == "application/zip" && bytes.Contains(head, []byte("word/")) {
		return extTypes[".docx"]
	}
	return sniffed
}

// oleSignature begins an OLE2 compound file: a Word 97–2003 document, and
// also a spreadsheet, a deck or a mail message, so the bytes alone never
// type a file Word's.
var oleSignature = []byte("\xD0\xCF\x11\xE0\xA1\xB1\x1A\xE1")

// djvuType is a DjVu document's media type; image/x-djvu is its older name.
const djvuType = "image/vnd.djvu"

// isDjVu reports whether head begins a DjVu file: "AT&TFORM", a length, then
// DJVU (one page) or DJVM (bundled pages) at offset 12. http.DetectContentType
// knows none of it.
func isDjVu(head []byte) bool {
	if len(head) < 16 || string(head[:8]) != "AT&TFORM" {
		return false
	}
	kind := string(head[12:16])
	return kind == "DJVU" || kind == "DJVM"
}

// zipMimetype reads the type an OpenDocument package or an EPUB (OCF)
// declares. Both formats store an uncompressed "mimetype" entry first in
// the zip, so its value sits in the first local file header: "" for any
// other zip, and for a declared type that is neither.
func zipMimetype(head []byte) string {
	const hdr = 30
	if len(head) < hdr || !bytes.HasPrefix(head, []byte("PK\x03\x04")) {
		return ""
	}
	le16 := func(i int) int { return int(head[i]) | int(head[i+1])<<8 }
	flags, method, size := le16(6), le16(8), le16(18)|le16(20)<<16
	nameLen, extraLen := le16(26), le16(28)
	if method != 0 || nameLen != len("mimetype") || len(head) < hdr+nameLen || string(head[hdr:hdr+nameLen]) != "mimetype" {
		return ""
	}
	start := hdr + nameLen + extraLen
	if start >= len(head) {
		return ""
	}
	if size == 0 && flags&0x8 != 0 {
		// A writer that streams puts the sizes in a descriptor after the
		// data: the stored value ends where the next record ("PK") starts.
		if i := bytes.Index(head[start:], []byte("PK")); i > 0 {
			size = i
		}
	}
	if size <= 0 || size > 100 || len(head) < start+size {
		return ""
	}
	mt := normalizeType(string(head[start : start+size]))
	if mt != "application/epub+zip" && !strings.HasPrefix(mt, "application/vnd.oasis.opendocument.") {
		return ""
	}
	return mt
}

// The FB2 types (RFC GV §4.1): a book, and a zip of one book.
const (
	fb2Type    = "application/x-fictionbook+xml"
	fb2ZipType = "application/x-zip-compressed-fb2"
)

// rtfType is an RTF document's type (RFC GV §4.9); text/rtf folds into it.
const rtfType = "application/rtf"

// rtfHeader reports whether head begins an RTF document: "{\rtf", after a
// UTF-8 byte order mark if there is one.
func rtfHeader(head []byte) bool {
	return bytes.HasPrefix(bytes.TrimPrefix(head, []byte("\xEF\xBB\xBF")), []byte(`{\rtf`))
}

// The Palm database types (RFC GV §4.1): a MOBI, AZW, AZW3 or PRC book,
// and a PalmDOC e-text.
const (
	mobiType    = "application/x-mobipocket-ebook"
	palmDocType = "application/x-palm-database"
)

// amazonEbook are the types Amazon's tools declare for a Kindle book; the
// bytes say whether it is one this reader reads (KFX and Topaz are not).
var amazonEbook = map[string]bool{
	"application/vnd.amazon.ebook":       true,
	"application/vnd.amazon.mobi8-ebook": true,
	"application/x-mobi8-ebook":          true,
}

// palmKind is the type of the Palm database head begins, by the type and
// creator at offset 60: BOOKMOBI is a MOBI book, TEXtREAd a PalmDOC text;
// "" for anything else.
func palmKind(head []byte) string {
	if len(head) < 68 {
		return ""
	}
	switch string(head[60:68]) {
	case "BOOKMOBI":
		return mobiType
	case "TEXtREAd":
		return palmDocType
	}
	return ""
}

// chmType is a compiled HTML Help file's type (RFC GX §4.1).
const chmType = "application/vnd.ms-htmlhelp"

// chmHeader reports whether head begins a compiled help file the CHM
// reader reads: "ITSF" and version 3.
func chmHeader(head []byte) bool {
	return len(head) >= 8 && string(head[:4]) == "ITSF" && binary.LittleEndian.Uint32(head[4:]) == 3
}

// fileExt is a name's extension: path.Ext, except that a zipped FB2 book's
// double extension is kept whole (path.Ext of "a.fb2.zip" is ".zip"), so
// the name keeps it when it is typed, trimmed or numbered.
func fileExt(name string) string {
	const fb2Zip = ".fb2.zip"
	if len(name) > len(fb2Zip) && strings.EqualFold(name[len(name)-len(fb2Zip):], fb2Zip) {
		return name[len(name)-len(fb2Zip):]
	}
	return path.Ext(name)
}

// fictionBook reports whether head is the start of an FB2 book: XML whose
// root element is FictionBook, found in the first 4 KiB after a byte order
// mark, the XML declaration, comments and processing instructions.
func fictionBook(head []byte) bool {
	if len(head) > 4096 {
		head = head[:4096]
	}
	switch {
	case bytes.HasPrefix(head, []byte("\xEF\xBB\xBF")):
		head = head[3:]
	case len(head) >= 2 && (head[0] == 0xFF && head[1] == 0xFE || head[0] == 0xFE && head[1] == 0xFF):
		// UTF-16: the markup up to the root is ASCII, one byte in two.
		lo := 2
		if head[0] == 0xFE {
			lo = 3
		}
		var b []byte
		for i := lo; i < len(head); i += 2 {
			b = append(b, head[i])
		}
		head = b
	}
	for {
		head = bytes.TrimLeft(head, " \t\r\n")
		var end []byte
		switch {
		case bytes.HasPrefix(head, []byte("<?")):
			end = []byte("?>")
		case bytes.HasPrefix(head, []byte("<!--")):
			end = []byte("-->")
		}
		if end == nil {
			break
		}
		i := bytes.Index(head[2:], end)
		if i < 0 {
			return false
		}
		head = head[2+i+len(end):]
	}
	if !bytes.HasPrefix(head, []byte("<")) {
		return false
	}
	name := head[1:]
	if i := bytes.IndexAny(name, " \t\r\n/>"); i >= 0 {
		name = name[:i]
	} else {
		return false
	}
	if i := bytes.LastIndexByte(name, ':'); i >= 0 {
		name = name[i+1:]
	}
	return string(name) == "FictionBook"
}

// zippedFB2 reports whether a zip's first entry, as its first local file
// header names it, is an .fb2 book. Whether it is the only one is the
// extractor's to check: an archive of many books is refused there, with a
// reason, rather than typed as a plain zip here.
func zippedFB2(head []byte) bool {
	const hdr = 30
	if len(head) < hdr || !bytes.HasPrefix(head, []byte("PK\x03\x04")) {
		return false
	}
	nameLen := int(head[26]) | int(head[27])<<8
	if nameLen == 0 || len(head) < hdr+nameLen {
		return false
	}
	return strings.HasSuffix(strings.ToLower(string(head[hdr:hdr+nameLen])), ".fb2")
}

// Sniff is settleMediaType for a producer that has the head of the bytes but
// no file yet (a branch deciding whether a response is binary).
func Sniff(declared string, head []byte, name string) string {
	return settleMediaType(declared, head, name)
}

func generic(mt string) bool {
	return mt == "" || mt == "application/octet-stream" || mt == "binary/octet-stream" || mt == "text/plain"
}

func normalizeType(t string) string {
	mt, _, err := mime.ParseMediaType(strings.TrimSpace(t))
	if err != nil || !strings.Contains(mt, "/") {
		return ""
	}
	return strings.ToLower(mt)
}

// withExtension gives name the extension of mt when it has none, and names
// a file that has no name at all.
func withExtension(name, mt string) string {
	if name == "" {
		name = "file"
	}
	if path.Ext(name) != "" {
		return name
	}
	if ext, ok := typeExts[mt]; ok {
		return name + ext
	}
	return name + ".bin"
}

// cyrillic transliterates the Ukrainian and Russian alphabets (the national
// romanisation, without its position rules), so "Звіт.pdf" is kept as
// "Zvit.pdf" rather than a name of dashes. The documents that motivated the
// inbox are Ukrainian; a name the operator can still read is worth the table.
var cyrillic = func() map[rune]string {
	upper := map[rune]string{
		'А': "A", 'Б': "B", 'В': "V", 'Г': "H", 'Ґ': "G", 'Д': "D", 'Е': "E", 'Є': "Ye",
		'Ж': "Zh", 'З': "Z", 'И': "Y", 'І': "I", 'Ї': "Yi", 'Й': "Y", 'К': "K", 'Л': "L",
		'М': "M", 'Н': "N", 'О': "O", 'П': "P", 'Р': "R", 'С': "S", 'Т': "T", 'У': "U",
		'Ф': "F", 'Х': "Kh", 'Ц': "Ts", 'Ч': "Ch", 'Ш': "Sh", 'Щ': "Shch", 'Ь': "", 'Ю': "Yu",
		'Я': "Ya", 'Ё': "Yo", 'Ы': "Y", 'Э': "E", 'Ъ': "",
	}
	m := map[rune]string{'\'': "", '’': "", 'ʼ': ""}
	for r, s := range upper {
		m[r] = s
		m[unicode.ToLower(r)] = strings.ToLower(s)
	}
	return m
}()
