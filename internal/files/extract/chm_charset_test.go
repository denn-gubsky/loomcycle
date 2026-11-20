package extract

import (
	"strings"
	"testing"

	"golang.org/x/text/encoding/charmap"
)

// cp encodes s in a single-byte code page, as an old help file stores it.
func cp(t *testing.T, enc *charmap.Charmap, s string) string {
	t.Helper()
	b, err := enc.NewEncoder().Bytes([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A Russian help file compiled on English Windows says its language is
// English (LCID 0x409), and its pages name no encoding. Read as the
// language's windows-1252 they came out as Latin-1 mojibake (a library
// file, radioshpionazh.chm). Its pages and its contents names are
// Cyrillic, so they are read as such, and the file's language is not
// reported as English.
func TestCHM_UnmarkedCyrillicUnderAWesternLanguageIsReadAsCyrillic(t *testing.T) {
	for _, tc := range []struct {
		name string
		enc  *charmap.Charmap
	}{{"windows-1251", charmap.Windows1251}, {"koi8-r", charmap.KOI8R}} {
		t.Run(tc.name, func(t *testing.T) {
			page := "<html><body><p>" + cp(t, tc.enc, "Глава первая. Основные элементы приемо-передатчиков.") + "</p></body></html>"
			toc := `<html><body><ul><li><object type="text/sitemap"><param name="Name" value="` +
				cp(t, tc.enc, "Шпионские штучки") + `"><param name="Local" value="1.htm"></object></ul></body></html>`
			parts := []chmPart{
				{name: "/#SYSTEM", data: chmSystemFile("toc.hhc", "1.htm", "", 0x409, "r")},
				{name: "/toc.hhc", data: []byte(toc), compressed: true},
				{name: "/1.htm", data: []byte(page), compressed: true},
			}
			s, rep := run(t, CHM{}, chmSpec{parts: parts}.build(), Options{Name: "r.chm"})
			md := s.md.String()
			if !strings.Contains(md, "## Шпионские штучки") || !strings.Contains(md, "Глава первая. Основные элементы") {
				t.Fatalf("got:\n%s", md)
			}
			if rep.Metadata != nil && rep.Metadata.Language == "en" {
				t.Errorf("language = en for a file whose pages are Cyrillic")
			}
			if !strings.Contains(strings.Join(rep.Notes, "\n"), "Cyrillic") {
				t.Errorf("notes %q do not say the pages were read as Cyrillic", rep.Notes)
			}
		})
	}
}

// Text in a Western language keeps the file's windows-1252: accented
// letters are not runs of Cyrillic.
func TestCHM_UnmarkedWesternPageKeepsItsCodePage(t *testing.T) {
	text := "Le café était déjà prêt à côté. Über Größe, Maße und Füße äußerst schön."
	page := "<html><body><p>" + cp(t, charmap.Windows1252, text) + "</p></body></html>"
	s, rep := run(t, CHM{}, testCHM(map[string]string{"a.htm": page}, []string{"a.htm"}), Options{Name: "w.chm"})
	if !strings.Contains(s.md.String(), text) {
		t.Fatalf("got:\n%s", s.md.String())
	}
	if rep.Metadata == nil || rep.Metadata.Language != "en" {
		t.Errorf("metadata %+v, want language en kept", rep.Metadata)
	}
}

// A frameset page's <noframes> fallback is raw text to the HTML parser, so
// it was written as escaped markup (\‹body> …). It is a message for old
// browsers, and the frames' pages are topics of their own: it is left out.
func TestCHM_NoframesFallbackIsNotWrittenAsMarkup(t *testing.T) {
	pages := map[string]string{
		"index.htm": `<html><frameset cols="20%,*"><frame src="1.htm"><noframes><body><p>Your browser does not support frames</p></body></noframes></frameset></html>`,
		"1.htm":     `<html><body><p>Chapter one</p></body></html>`,
	}
	s, _ := run(t, CHM{}, testCHM(pages, []string{"index.htm", "1.htm"}), Options{Name: "f.chm"})
	md := s.md.String()
	if strings.Contains(md, "body>") || strings.Contains(md, "does not support frames") || !strings.Contains(md, "Chapter one") {
		t.Fatalf("got:\n%s", md)
	}
}

// The same holds for a plain HTML file.
func TestHTML_NoframesFallbackIsNotWrittenAsMarkup(t *testing.T) {
	page := `<html><head><title>F</title></head><frameset><frame src="a.htm"><noframes><body><p>Old browser text</p></body></noframes></frameset></html>`
	s, _ := run(t, HTML{}, []byte(page), Options{Name: "f.html"})
	if md := s.md.String(); strings.Contains(md, "body>") || strings.Contains(md, "<p>") {
		t.Fatalf("got:\n%s", md)
	}
}
