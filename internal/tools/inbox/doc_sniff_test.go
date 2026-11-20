package inbox_test

import (
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/files/extract"
	"github.com/denn-gubsky/loomcycle/internal/tools/inbox"
)

// ole is the head of an OLE2 compound file, the container of a Word 97–2003
// document and of spreadsheets, decks and mail messages alike.
var ole = []byte("\xD0\xCF\x11\xE0\xA1\xB1\x1A\xE1" + strings.Repeat("\x00", 504))

// An OLE2 file is typed a Word document by a .doc or .dot name or a
// declared Word type (RFC GX §4.1), and read by the DOC extractor; the
// signature alone types nothing, as a spreadsheet, a deck and a mail
// message share it.
func TestSniff_DOCIsTypedByNameOrDeclaredType(t *testing.T) {
	for _, tc := range []struct {
		declared, name, want string
	}{
		{"", "report.doc", "application/msword"},
		{"application/octet-stream", "REPORT.DOC", "application/msword"},
		{"", "letter.dot", "application/msword"},
		{"application/msword", "file", "application/msword"},
		{"", "book.xls", "application/vnd.ms-excel"},
		{"", "deck.ppt", "application/vnd.ms-powerpoint"},
		{"application/vnd.ms-outlook", "mail.msg", "application/vnd.ms-outlook"},
	} {
		mt := inbox.Sniff(tc.declared, ole, tc.name)
		if mt != tc.want {
			t.Errorf("declared %q, name %q: %q, want %q", tc.declared, tc.name, mt, tc.want)
		}
		if _, ok := extract.Default().For(mt, tc.name); ok != (mt == "application/msword") {
			t.Errorf("declared %q, name %q: typed %q, an extractor %t", tc.declared, tc.name, mt, ok)
		}
	}
	for _, name := range []string{"file", "file.bin", "mail.msg", ""} {
		if mt := inbox.Sniff("", ole, name); mt == "application/msword" {
			t.Errorf("an OLE2 file named %q was typed a Word document by its signature alone", name)
		}
	}
	// A Graphviz .dot is text, not a Word template.
	if mt := inbox.Sniff("", []byte("digraph g { a -> b }\n"), "graph.dot"); mt == "application/msword" {
		t.Errorf("a Graphviz file was typed %q", mt)
	}
	// RTF behind a .doc name and a Word type stays RTF (RFC GV §4.9).
	if mt := inbox.Sniff("application/msword", []byte(`{\rtf1 x}`), "a.doc"); mt != "application/rtf" {
		t.Errorf("RTF named .doc: %q", mt)
	}
	if e, ok := extract.Default().For("application/msword", "a.doc"); !ok || e != (extract.DOC{}) {
		t.Errorf("a Word document's extractor: %T %t", e, ok)
	}
}
