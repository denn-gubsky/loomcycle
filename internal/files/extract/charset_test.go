package extract

import (
	"strings"
	"testing"

	"golang.org/x/text/encoding/charmap"
	xunicode "golang.org/x/text/encoding/unicode"
)

const ruText = "Глава первая. Мальчик шёл по дороге и думал о том, что завтра снова будет школа, а потом каникулы.\nВторая строка книги."

// Old books on the library's disks are plain text in an 8-bit code page
// that names none (a field sample found 5 of 50 refused as "not UTF-8").
// Each is read, and the result says which code page was used.
func TestText_LegacyEncodingsAreDecoded(t *testing.T) {
	utf16 := func(le bool, s string) []byte {
		e := xunicode.UTF16(xunicode.BigEndian, xunicode.UseBOM)
		if le {
			e = xunicode.UTF16(xunicode.LittleEndian, xunicode.UseBOM)
		}
		b, err := e.NewEncoder().Bytes([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, tc := range []struct {
		name, want, note string
		data             []byte
	}{
		{"windows-1251", "Мальчик шёл по дороге", "Windows 1251", []byte(cp(t, charmap.Windows1251, ruText))},
		{"koi8-r", "Мальчик шёл по дороге", "KOI8-R", []byte(cp(t, charmap.KOI8R, ruText))},
		{"cp866", "Мальчик шёл по дороге", "866", []byte(cp(t, charmap.CodePage866, ruText))},
		{"windows-1252", "Café déjà vu, naïve façade.", "Windows 1252", []byte(cp(t, charmap.Windows1252, "Café déjà vu, naïve façade.\nNatural bug sprays for plants."))},
		{"utf-16le", "Мальчик шёл по дороге", "UTF-16LE", utf16(true, ruText)},
		{"utf-16be", "Мальчик шёл по дороге", "UTF-16BE", utf16(false, ruText)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, rep := run(t, Text{}, tc.data, Options{Name: "book.txt"})
			if !strings.Contains(s.md.String(), tc.want) {
				t.Fatalf("got:\n%s", s.md.String())
			}
			if !strings.Contains(strings.Join(rep.Notes, "\n"), tc.note) {
				t.Errorf("notes %q do not name %s", rep.Notes, tc.note)
			}
		})
	}
}

// Bytes typed as text that hold NULs are not a book in a code page.
func TestText_BinaryIsStillRefused(t *testing.T) {
	_, err := extractBytes(Text{}, "x.txt", []byte("PK\x03\x04\x00\x00\xff\xfe binary"))
	if !IsRefused(err) || !strings.Contains(err.Error(), "not text") {
		t.Fatalf("err = %v, want refused as not text", err)
	}
}

// An HTML page's <meta> charset was ignored: Go's parser reads UTF-8, so a
// windows-1251 page came out as invalid UTF-8, garbled in process and
// fatal to a conversion worker.
func TestHTML_MetaCharsetIsHonoured(t *testing.T) {
	for _, tc := range []struct {
		label string
		cm    *charmap.Charmap
	}{{"windows-1251", charmap.Windows1251}, {"koi8-r", charmap.KOI8R}} {
		page := `<html><head><meta http-equiv="Content-Type" content="text/html; charset=` + tc.label + `"><title>` +
			cp(t, tc.cm, "Развивающие игры") + `</title></head><body><p>` + cp(t, tc.cm, ruText) + `</p></body></html>`
		s, _ := run(t, HTML{}, []byte(page), Options{Name: "p.html"})
		md := s.md.String()
		if !strings.Contains(md, "# Развивающие игры") || !strings.Contains(md, "Мальчик шёл по дороге") {
			t.Fatalf("%s: got:\n%s", tc.label, md)
		}
	}
}

// A page that names no encoding and is not UTF-8 is read by detection,
// as a plain text is, and says so.
func TestHTML_UnmarkedLegacyPageIsDecoded(t *testing.T) {
	page := `<html><head><title>` + cp(t, charmap.Windows1251, "Развивающие игры") + `</title></head><body><pre>` +
		cp(t, charmap.Windows1251, ruText) + `</pre></body></html>`
	s, rep := run(t, HTML{}, []byte(page), Options{Name: "nikitindev.txt"})
	if md := s.md.String(); !strings.Contains(md, "Мальчик шёл по дороге") {
		t.Fatalf("got:\n%s", md)
	}
	if !strings.Contains(strings.Join(rep.Notes, "\n"), "Windows 1251") {
		t.Errorf("notes %q do not name the code page", rep.Notes)
	}
}

func TestHTML_UnreadEncodingIsRefusedByName(t *testing.T) {
	_, err := extractBytes(HTML{}, "j.html", []byte(`<html><head><meta charset="shift_jis"></head><body><p>x</p></body></html>`))
	if !IsRefused(err) || !strings.Contains(err.Error(), "shift_jis") {
		t.Fatalf("err = %v, want a refusal naming shift_jis", err)
	}
}
