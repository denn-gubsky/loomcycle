package filenames

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// ClientNameMaxBytes caps a client-sent name (SanitizeClientName).
const ClientNameMaxBytes = 120

// SanitizeClientName is the name a file a person sent is kept under (RFC HE
// F1): their own name, in their own script, unlike SanitizeName's ASCII
// form for names a tool or a URL produced. "Паспорт.docx" stays
// "Паспорт.docx".
//
// The name is NFC-normalised. It is refused when it names a path or could
// hide what it says: a / or \, "." or "..", a leading dot (a hidden file),
// or any control or format character (Cc, Cf — which takes in the bidi
// overrides U+202A–U+202E and isolates U+2066–U+2069 that make "exe.pdf"
// read as "fdp.exe"). Within it, letters, marks, digits, spaces and . _ -
// ( ) are kept and every other character becomes "-". It is then cut to
// ClientNameMaxBytes on a rune boundary, keeping its extension.
func SanitizeClientName(name string) (string, error) {
	if !utf8.ValidString(name) {
		return "", errors.New("the name is not valid UTF-8")
	}
	name = strings.TrimSpace(norm.NFC.String(name))
	switch {
	case name == "":
		return "", errors.New("the name is empty")
	case strings.ContainsAny(name, `/\`):
		return "", errors.New("the name may not contain / or \\")
	case name == "." || name == "..":
		return "", errors.New("the name may not be . or ..")
	case strings.HasPrefix(name, "."):
		return "", errors.New("the name may not start with a dot")
	}
	var b strings.Builder
	dash := false
	for _, r := range name {
		switch {
		case unicode.In(r, unicode.Cc, unicode.Cf):
			return "", errors.New("the name may not contain control or format characters")
		case unicode.IsLetter(r) || unicode.IsMark(r) || unicode.IsDigit(r) || strings.ContainsRune(" ._-()", r):
			b.WriteRune(r)
			dash = false
		default:
			if !dash {
				b.WriteByte('-')
			}
			dash = true
		}
	}
	out := b.String()
	if len(out) > ClientNameMaxBytes {
		ext := FileExt(out)
		if len(ext) > 16 {
			ext = ""
		}
		stem := out[:ClientNameMaxBytes-len(ext)]
		for !utf8.ValidString(stem) {
			stem = stem[:len(stem)-1]
		}
		out = strings.TrimRight(stem, " .-") + ext
	}
	if strings.Trim(out, " .-") == "" {
		return "", errors.New("the name has no letters or digits")
	}
	return out, nil
}
