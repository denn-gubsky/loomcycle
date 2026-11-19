package inbox_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/files/extract"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox/inboxtest"
)

// A compiled help file is typed by its ITSF header (RFC GX §4.1), whatever
// generic type the producer declared and whatever the name says; its
// extension types it when the bytes do not.
func TestSniff_CHMIsTypedByItsHeader(t *testing.T) {
	head, err := os.ReadFile(filepath.Join("..", "..", "files", "extract", "testdata", "chm", "book.chm"))
	if err != nil {
		t.Fatal(err)
	}
	for _, declared := range []string{"", "application/octet-stream", "text/plain", "application/x-chm", "application/vnd.ms-htmlhelp"} {
		for _, name := range []string{"help.chm", "help.bin", "help"} {
			if mt := inbox.Sniff(declared, head[:512], name); mt != "application/vnd.ms-htmlhelp" {
				t.Errorf("declared %q, named %q: %q", declared, name, mt)
			}
		}
	}
	v2 := append([]byte("ITSF\x02\x00\x00\x00"), head[8:512]...)
	if mt := inbox.Sniff("", v2, "file"); mt == "application/vnd.ms-htmlhelp" {
		t.Error("an ITSF version 2 header was typed as the CHM this reader reads")
	}
	if mt := inbox.Sniff("", []byte("plain words"), "Manual.CHM"); mt != "application/vnd.ms-htmlhelp" {
		t.Errorf("by the .chm extension: %q", mt)
	}
	if mt := inbox.Sniff("text/html", head[:512], "page.html"); mt != "text/html" {
		t.Errorf("a specific declared type was overridden: %q", mt)
	}
}

// Kept with no name, a help file goes to documents/ as .chm, and the CHM
// reader reads it.
func TestCHM_UnnamedHelpFileIsKeptAsCHM(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "files", "extract", "testdata", "chm", "book.chm"))
	if err != nil {
		t.Fatal(err)
	}
	r := inboxtest.New(t, "root-1", inbox.Service{})
	res := inbox.Capture(r.Ctx, inbox.Item{Fallback: "help", Origin: "o"}, data)
	if res.Record.MediaType != "application/vnd.ms-htmlhelp" || !strings.HasPrefix(res.Record.Path, "documents/") || !strings.HasSuffix(res.Record.Path, ".chm") {
		t.Fatalf("kept as %q at %q", res.Record.MediaType, res.Record.Path)
	}
	if e, ok := extract.Default().For(res.Record.MediaType, ""); !ok || e != (extract.CHM{}) {
		t.Fatalf("extractor %T, %t", e, ok)
	}
}
