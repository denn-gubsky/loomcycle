package inbox_test

import (
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/files/extract"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox/inboxtest"
)

// palmHead is the start of a Palm database of the given type and creator:
// a 32-byte name, then the header up to offset 60.
func palmHead(kind string) []byte {
	b := make([]byte, 128)
	copy(b, "The_Book")
	copy(b[60:], kind)
	return b
}

// A MOBI book and a PalmDOC text are typed by the type and creator at
// offset 60 (RFC GV §4.1), whatever generic or Amazon type the producer
// declared and whatever the name says; a .prc that is PalmDOC is PalmDOC.
func TestSniff_PalmDatabaseBooksAreTypedByTheirHeader(t *testing.T) {
	for kind, want := range map[string]string{"BOOKMOBI": "application/x-mobipocket-ebook", "TEXtREAd": "application/x-palm-database"} {
		for _, declared := range []string{"", "application/octet-stream", "text/plain", "application/vnd.amazon.ebook", "application/x-mobi8-ebook", "application/x-mobipocket-ebook", "application/x-palm-database"} {
			for _, name := range []string{"file", "book.prc", "book.azw3", "book.pdb"} {
				if mt := inbox.Sniff(declared, palmHead(kind), name); mt != want {
					t.Errorf("%s, declared %q, named %q: %q, want %q", kind, declared, name, mt, want)
				}
			}
		}
	}
	// By the extension, when the bytes do not say.
	for name, want := range map[string]string{
		"a.mobi": "application/x-mobipocket-ebook", "a.azw": "application/x-mobipocket-ebook",
		"a.AZW3": "application/x-mobipocket-ebook", "a.prc": "application/x-mobipocket-ebook",
		"a.pdb": "application/x-palm-database",
	} {
		if mt := inbox.Sniff("", []byte{0, 1, 2, 3}, name); mt != want {
			t.Errorf("%s: %q, want %q", name, mt, want)
		}
	}
	// Another Palm database, a short file, and a specific declared type.
	if mt := inbox.Sniff("", palmHead("DataPlkr"), "file"); mt == "application/x-mobipocket-ebook" || mt == "application/x-palm-database" {
		t.Errorf("a Plucker database: %q", mt)
	}
	if mt := inbox.Sniff("", palmHead("BOOKMOBI")[:67], "file"); mt == "application/x-mobipocket-ebook" {
		t.Errorf("a 67-byte file: %q", mt)
	}
	if mt := inbox.Sniff("application/pdf", palmHead("BOOKMOBI"), "file"); mt != "application/pdf" {
		t.Errorf("a declared PDF: %q", mt)
	}
}

// A MOBI book kept with no name goes to documents/ as a .mobi, and the MOBI
// reader reads it; a PalmDOC text as a .pdb, read by the PalmDOC reader.
func TestMOBI_UnnamedBookIsKeptAndRead(t *testing.T) {
	r := inboxtest.New(t, "root-1", inbox.Service{})
	for kind, want := range map[string]struct {
		ext string
		e   extract.Extractor
	}{"BOOKMOBI": {".mobi", extract.MOBI{}}, "TEXtREAd": {".pdb", extract.PalmDOC{}}} {
		res := inbox.Capture(r.Ctx, inbox.Item{Fallback: "book", Origin: "o"}, palmHead(kind))
		if !strings.HasPrefix(res.Record.Path, "documents/") || !strings.HasSuffix(res.Record.Path, want.ext) {
			t.Errorf("%s kept as %q", kind, res.Record.Path)
		}
		if e, ok := extract.Default().For(res.Record.MediaType, res.Record.Path); !ok || e != want.e {
			t.Errorf("%s: extractor %T, %t", kind, e, ok)
		}
	}
}
