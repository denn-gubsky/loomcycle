package inbox_test

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox/inboxtest"
)

// An FB2 book is XML whose root element is FictionBook (RFC GV §4.1): a
// sniffer calls it text/xml or text, so its root is looked for past a byte
// order mark, the prolog and comments, whatever the name says.
func TestSniff_FB2IsTypedByItsRootElement(t *testing.T) {
	const fb2 = "application/x-fictionbook+xml"
	root := `<FictionBook xmlns="http://www.gribuser.ru/xml/fictionbook/2.0"><body/></FictionBook>`
	utf16 := []byte{0xFF, 0xFE}
	for _, c := range `<?xml version="1.0" encoding="UTF-16"?>` + root {
		utf16 = append(utf16, byte(c), 0)
	}
	for name, head := range map[string][]byte{
		"prolog":       []byte(`<?xml version="1.0" encoding="windows-1251"?>` + "\n" + root),
		"no prolog":    []byte(root),
		"bom":          []byte("\xEF\xBB\xBF<?xml version=\"1.0\"?>" + root),
		"utf-16":       utf16,
		"long comment": []byte(`<?xml version="1.0"?><!--` + strings.Repeat("c", 2000) + `--><?xml-stylesheet href="x"?>` + root),
		"prefixed":     []byte(`<fb:FictionBook xmlns:fb="x"/>`),
	} {
		for _, declared := range []string{"", "application/octet-stream", "text/xml", "application/xml"} {
			if mt := inbox.Sniff(declared, head, "book"); mt != fb2 {
				t.Errorf("%s, declared %q: %q", name, declared, mt)
			}
		}
	}
	for name, head := range map[string][]byte{
		"other xml":  []byte(`<?xml version="1.0"?><html><body>FictionBook</body></html>`),
		"cut prolog": []byte(`<?xml version="1.0"`),
		"html":       []byte(`<FictionBooks>`),
	} {
		if mt := inbox.Sniff("", head, "book"); mt == fb2 {
			t.Errorf("%s was typed as FB2", name)
		}
	}
	// A declared type that is not XML or generic is kept.
	if mt := inbox.Sniff("text/plain; charset=utf-8", []byte(root), "b"); mt != fb2 {
		t.Errorf("text/plain FB2: %q", mt)
	}
	if mt := inbox.Sniff("application/pdf", []byte(root), "b"); mt != "application/pdf" {
		t.Errorf("a declared PDF was retyped: %q", mt)
	}
	// By extension when the bytes do not say.
	if mt := inbox.Sniff("", []byte("hello"), "Book.FB2"); mt != fb2 {
		t.Errorf("Sniff(Book.FB2) = %q", mt)
	}
}

func zipOfOne(t *testing.T, name string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte(`<FictionBook/>`))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// A zipped FB2 book is typed by the name of its entry or by its double
// extension, kept with the documents, and keeps ".fb2.zip" when it is named
// or numbered (path.Ext would see only ".zip").
func TestInbox_ZippedFB2IsTypedAndKeepsItsDoubleExtension(t *testing.T) {
	const fb2zip = "application/x-zip-compressed-fb2"
	r := inboxtest.New(t, "root-1", inbox.Service{})
	z := zipOfOne(t, "Кобзар.fb2")
	for _, name := range []string{"", "kobzar.zip", "kobzar.fb2.zip"} {
		res := inbox.Capture(r.Ctx, inbox.Item{Name: name, Fallback: "book", MediaType: "application/zip", Origin: "o"}, z)
		if res.Record.MediaType != fb2zip || !strings.HasPrefix(res.Record.Path, "documents/") {
			t.Errorf("%q: %q at %q", name, res.Record.MediaType, res.Record.Path)
		}
		if name != "kobzar.zip" && !strings.HasSuffix(res.Record.Path, ".fb2.zip") {
			t.Errorf("%q: named %q", name, res.Record.Path)
		}
	}
	// Twice under one name: the number goes before the double extension.
	res := inbox.Capture(r.Ctx, inbox.Item{Name: "kobzar.fb2.zip", Origin: "o"}, z)
	if !strings.HasSuffix(res.Record.Path, "kobzar-2.fb2.zip") {
		t.Errorf("third copy named %q", res.Record.Path)
	}
	if mt := inbox.Sniff("", zipOfOne(t, "notes.txt"), "a.fb2.zip"); mt != fb2zip {
		t.Errorf("by its double extension: %q", mt)
	}
	if mt := inbox.Sniff("", zipOfOne(t, "notes.txt"), "a.zip"); mt == fb2zip {
		t.Error("a zip of a text file was typed as FB2")
	}
	// A plain FB2 book goes to the documents too.
	plain := inbox.Capture(r.Ctx, inbox.Item{Name: "book", Origin: "o"}, []byte(`<?xml version="1.0"?><FictionBook/>`))
	if plain.Record.MediaType != "application/x-fictionbook+xml" || !strings.HasSuffix(plain.Record.Path, ".fb2") || !strings.HasPrefix(plain.Record.Path, "documents/") {
		t.Errorf("plain FB2: %q at %q", plain.Record.MediaType, plain.Record.Path)
	}
}
