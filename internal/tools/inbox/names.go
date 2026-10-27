package inbox

import (
	"bytes"
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
		ext := path.Ext(out)
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
	if strings.HasPrefix(mt, "image/") {
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
	".doc":  "application/msword",
	".xls":  "application/vnd.ms-excel",
	".ppt":  "application/vnd.ms-powerpoint",
	".csv":  "text/csv",
	".tsv":  "text/tab-separated-values",
	".md":   "text/markdown",
	".json": "application/json",
	".vtt":  "text/vtt",
	".srt":  "application/x-subrip",
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
	if mt := normalizeType(declared); mt != "" && !generic(mt) {
		return mt
	}
	sniffed := normalizeType(http.DetectContentType(head))
	if byExt, ok := extTypes[strings.ToLower(path.Ext(name))]; ok && (generic(sniffed) || sniffed == "application/zip" || sniffed == "application/x-ole-storage") {
		return byExt
	}
	if sniffed == "application/zip" && bytes.Contains(head, []byte("word/")) {
		return extTypes[".docx"]
	}
	return sniffed
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
