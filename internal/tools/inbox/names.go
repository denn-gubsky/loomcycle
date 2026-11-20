package inbox

import (
	"path"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/files/filenames"
)

// SanitizeName is filenames.SanitizeName: the inbox's own naming rule, which
// the workers apply too.
func SanitizeName(name string) string { return filenames.SanitizeName(name) }

// Sniff is filenames.Sniff: the type the inbox records for a file.
func Sniff(declared string, head []byte, name string) string {
	return filenames.Sniff(declared, head, name)
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
	if strings.HasPrefix(mt, "image/") && mt != filenames.DjVuType {
		return DirImages
	}
	return DirDocuments
}
